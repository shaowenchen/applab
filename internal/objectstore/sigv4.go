package objectstore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// signer adds AWS Signature Version 4 to a request.
//
// Written out rather than pulled in because AppLab has one object store to talk
// to and the AWS SDK is a module the size of the rest of this program. What is
// here is the whole of what a bucket needs: sign a request, with the one
// algorithm S3 accepts.
//
// The reference is the AWS "Signature Version 4 signing process" documentation,
// and every step below names the section it implements, because the failure mode
// of getting one wrong is a bare 403 with no indication of which.
type signer struct {
	accessKey string
	secretKey string

	// region and service are part of the credential scope. S3's service name is
	// always "s3"; the region must match the bucket's, and "us-east-1" is the
	// value that works for a bucket in a region not otherwise specified.
	region  string
	service string

	// now is injectable so the tests can sign a request at the moment the
	// documentation's own example was signed. A signer that can only be tested
	// against the current clock cannot be tested against a published vector.
	now func() time.Time

	// unsignedPayload signs the literal string UNSIGNED-PAYLOAD in place of the
	// body's hash. S3 accepts it, and it is what makes a streaming upload
	// possible: the body is not read twice, so a large archive is not buffered
	// to compute its own signature.
	unsignedPayload bool

	// headers are the headers the signature covers, in the order the canonical
	// request requires — lexicographic by lowercased name.
	//
	// "host" must be present; SigV4 requires it and S3 requires nothing else
	// beyond the two x-amz headers. "content-type" is signed too, so that a
	// signed upload's type cannot be changed in flight.
	//
	// It is a field rather than a package-level variable so the tests can sign
	// AWS's own published vectors, which cover only "host" and "x-amz-date". That
	// is what makes the algorithm below checkable: the S3 header set has no
	// published signature to compare against, and the six steps that produce one
	// are exactly the part that is easy to get subtly wrong.
	headers []string
}

// s3SignedHeaders is the header set AppLab signs S3 requests with.
var s3SignedHeaders = []string{"content-type", "host", "x-amz-content-sha256", "x-amz-date"}

// sign adds the Authorization header to a prepared request.
//
// The request must already carry its final headers — host, content-type, and the
// two x-amz headers the caller sets — because they are what is being signed.
// payloadHash is the SHA-256 of the body in lowercase hex, or the literal
// "UNSIGNED-PAYLOAD".
func (s *signer) sign(req *http.Request, payloadHash string) {
	at := s.now().UTC()

	amzDate := at.Format("20060102T150405Z")
	dateStamp := at.Format("20060102")

	headers := s.headers
	if headers == nil {
		headers = s3SignedHeaders
	}

	// The two x-amz headers are set here rather than by the caller: they are
	// part of the signature, so a caller that forgot one would sign a request
	// without it and the server would reject a request that looks correct.
	//
	// content-type is here for the same reason and was the one that got away. It
	// is in the signed header set, and SigV4 requires every header named in
	// SignedHeaders to be present in the request — so a GET, HEAD or DELETE,
	// which sets no body type, signed a header it did not send. S3 answers that
	// with a bare 400, and the failure is invisible to every test in this
	// repository: the fake S3 server checks that an Authorization header exists
	// and never verifies a signature, so nothing here had ever been checked
	// against a real implementation of the protocol. It showed up the first time
	// an image talked to MinIO.
	//
	// A caller that has a more specific type may set one before signing; this
	// only fills in what is missing, rather than overwriting it.
	for _, name := range headers {
		switch name {
		case "content-type":
			if req.Header.Get("content-type") == "" {
				req.Header.Set("content-type", "application/octet-stream")
			}
		case "x-amz-date":
			req.Header.Set("x-amz-date", amzDate)
		case "x-amz-content-sha256":
			req.Header.Set("x-amz-content-sha256", payloadHash)
		}
	}

	// A host header is not something net/http keeps in Header — it writes it
	// from the URL — but it is part of the signature, so it is computed the same
	// way the transport will write it.
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	canonicalHeaders, headerList := canonicalHeaders(req, host, headers)
	canonicalRequest := strings.Join([]string{req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonicalHeaders,
		headerList,
		payloadHash,
	}, "\n")

	credentialScope := strings.Join([]string{dateStamp, s.region, s.service, "aws4_request"}, "/")

	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		hashHex([]byte(canonicalRequest)),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(s.signingKey(dateStamp), []byte(stringToSign)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.accessKey, credentialScope, headerList, signature))
}

// signingKey derives the key the signature is computed with.
//
// The chain is HMAC, not a hash: each step signs the next, starting from
// "AWS4"+secret. It exists so that a leaked signature for one date and region
// does not yield a key usable against another.
func (s *signer) signingKey(dateStamp string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+s.secretKey), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(s.region))
	kService := hmacSHA256(kRegion, []byte(s.service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

// canonicalHeaders renders the headers the signature covers and the
// semicolon-joined list naming them.
//
// Values are trimmed and their runs of internal whitespace collapsed, which is
// what the canonical form requires; a header with a tab in it that is not
// collapsed signs one way and verifies another.
func canonicalHeaders(req *http.Request, host string, headers []string) (string, string) {
	names := make([]string, len(headers))
	copy(names, headers)
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		value := req.Header.Get(name)
		if name == "host" {
			value = host
		}
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(collapseSpaces(value))
		b.WriteByte('\n')
	}
	return b.String(), strings.Join(names, ";")
}

// canonicalURI renders the path.
//
// S3's rules differ from the general SigV4 ones and this is the pair most often
// got wrong: the path is URI-encoded once, except that "/" is not encoded.
//
// u.Path is what carries the decoded path, and net/http writes it to the wire
// escaped. So this encodes it — with the "/" exception — rather than using
// EscapedPath, which would be right only when Path arrived already escaped.
// Using EscapedPath on a path Go decoded gave back "/apps%2Fshop" for the key
// "apps/shop", and the signature then covered a path the request did not carry.
//
// Callers must therefore leave Path as the decoded form and must not set
// RawPath: a RawPath that disagrees with Path makes net/http write RawPath to
// the wire while this signs Path, which is the same disagreement from the other
// side.
func canonicalURI(u *url.URL) string {
	path := u.Path
	if path == "" {
		return "/"
	}

	// Each segment is encoded separately so the separators survive. The general
	// SigV4 rule is that "/" is not encoded, in a key as much as in a route.
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		segments[i] = awsEncode(segment)
	}
	return strings.Join(segments, "/")
}

// canonicalQuery renders the query string with its parameters sorted by name and
// each name and value encoded.
//
// Sorting is by encoded name, not by raw name, and a repeated name is sorted by
// its value — the two rules that make a presigned URL and a signed one agree.
func canonicalQuery(u *url.URL) string {
	query := u.Query()
	if len(query) == 0 {
		return ""
	}

	type pair struct{ name, value string }
	pairs := make([]pair, 0, len(query))
	for name, values := range query {
		for _, value := range values {
			pairs = append(pairs, pair{awsEncode(name), awsEncode(value)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].name != pairs[j].name {
			return pairs[i].name < pairs[j].name
		}
		return pairs[i].value < pairs[j].value
	})

	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.name + "=" + p.value
	}
	return strings.Join(parts, "&")
}

// awsEncode is the URI encoding SigV4 uses, which is not Go's.
//
// It differs in two characters that matter for a key: Go's url.QueryEscape
// encodes a space as "+", where SigV4 requires "%20", and SigV4 requires "~"
// left as it is where Go leaves it too but escapes other unreserved characters
// inconsistently. Object keys contain app ids, which are ordinary, and also
// commit shas and file names, which are not always.
func awsEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// collapseSpaces trims a header value and reduces internal runs of whitespace to
// a single space, as the canonical form requires.
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}
