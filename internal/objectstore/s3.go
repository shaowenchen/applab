package objectstore

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// S3 is an object store reached over the S3 REST API.
//
// It speaks the protocol directly rather than through a vendor SDK. Nothing
// here is vendor-specific: the four operations AppLab uses are the ones every
// S3-compatible service implements, so the same client reaches AWS, MinIO, Ceph,
// Cloudflare R2 and the rest. What changes between them is the endpoint and
// whether path-style addressing is required, and both are options.
type S3 struct {
	bucket string
	client *http.Client
	signer *signer

	// endpoint is the scheme and host to send requests to, with no path. For
	// AWS it is https://s3.<region>.amazonaws.com; for anything self-hosted it is
	// that service's address.
	endpoint *url.URL

	// pathStyle puts the bucket in the path — /bucket/key — rather than in the
	// hostname — bucket.s3.amazonaws.com/key.
	//
	// A bucket name containing a dot cannot use the virtual-hosted form over
	// TLS, because the certificate is issued for a wildcard one level deep. A
	// self-hosted service with no wildcard DNS needs it too. So it is an option
	// rather than a guess: guessing from the bucket name would fix the dotted
	// case and break a service that requires the other.
	pathStyle bool
}

// S3Options is how a deployment points AppLab at a bucket.
type S3Options struct {
	// Endpoint is the service's address, e.g. https://s3.us-east-1.amazonaws.com
	// or http://minio.ops-system:9000. Required.
	Endpoint string

	// Bucket is the bucket AppLab keeps everything in. Required.
	Bucket string

	// Region is the bucket's region. Defaults to us-east-1, which is what a
	// self-hosted service expects and what AWS accepts for a bucket whose region
	// is not otherwise known.
	Region string

	// AccessKey and SecretKey are the credential. Both are required: nothing
	// here reads the instance metadata service or a shared credentials file,
	// because a deployment has one credential and passing it explicitly is what
	// keeps the failure — no credential — legible at startup rather than at the
	// first write.
	AccessKey string
	SecretKey string

	// PathStyle selects path-style addressing. See the field of the same name.
	PathStyle bool

	// HTTPClient is the client to use. Nil means a default with a timeout
	// generous enough for a large archive.
	HTTPClient *http.Client
}

// NewS3 builds a store over an S3-compatible bucket.
func NewS3(opts S3Options) (*S3, error) {
	if opts.Endpoint == "" {
		return nil, fmt.Errorf("objectstore: no endpoint given")
	}
	if opts.Bucket == "" {
		return nil, fmt.Errorf("objectstore: no bucket given")
	}
	if opts.AccessKey == "" || opts.SecretKey == "" {
		return nil, fmt.Errorf("objectstore: no access key given")
	}

	endpoint, err := url.Parse(opts.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("objectstore: endpoint %q: %w", opts.Endpoint, err)
	}
	if endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, fmt.Errorf("objectstore: endpoint %q is not a URL", opts.Endpoint)
	}

	region := opts.Region
	if region == "" {
		region = "us-east-1"
	}

	client := opts.HTTPClient
	if client == nil {
		// No overall timeout: an upload of a large archive over a slow link is
		// legitimately minutes long, and a timeout that fires mid-upload would
		// fail work that was going to succeed. The dial and header timeouts
		// catch the case a timeout is actually for — a service that is not
		// answering at all.
		client = &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          32,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				ExpectContinueTimeout: 5 * time.Second,
			},
		}
	}

	return &S3{
		bucket:    opts.Bucket,
		client:    client,
		endpoint:  endpoint,
		pathStyle: opts.PathStyle,
		signer: &signer{
			accessKey: opts.AccessKey,
			secretKey: opts.SecretKey,
			region:    region,
			service:   "s3",
			now:       time.Now,
		},
	}, nil
}

// String names the backend without naming a credential.
func (s *S3) String() string { return "s3:" + s.bucket + "@" + s.endpoint.Host }

// urlFor builds the request URL for a key.
//
// Path is left in its decoded form and RawPath is left unset, on purpose: the
// signer encodes the path itself, and net/http escapes whatever is in Path when
// it writes the request. A RawPath that disagreed with Path would make the
// transport write one form while the signature covered another — the failure
// that turns a valid key into a bare 403.
func (s *S3) urlFor(key string) *url.URL {
	u := *s.endpoint
	clean := Key(key)

	if s.pathStyle {
		u.Path = "/" + s.bucket
		if clean != "" {
			u.Path += "/" + clean
		}
		return &u
	}

	// A host that is already an IP address cannot take a bucket as a subdomain:
	// "applab.127.0.0.1" does not resolve. Virtual-hosted addressing needs DNS,
	// so a deployment pointed at an address rather than a name gets path-style
	// whatever it asked for — the alternative is a name that cannot be looked up.
	if hostIsIP(u.Hostname()) {
		u.Path = "/" + s.bucket
		if clean != "" {
			u.Path += "/" + clean
		}
		return &u
	}

	u.Host = s.bucket + "." + u.Host
	if clean != "" {
		u.Path = "/" + clean
	}
	return &u
}

// hostIsIP reports whether a host is an address rather than a name.
func hostIsIP(host string) bool {
	return net.ParseIP(host) != nil
}

// do signs a request and sends it, mapping a 404 onto ErrNotExist.
func (s *S3) do(req *http.Request, payloadHash string) (*http.Response, error) {
	s.signer.sign(req, payloadHash)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotExist
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("objectstore: %s %s: %s: %s",
			req.Method, req.URL.Path, resp.Status, s3Error(body, resp.Status))
	}
	return resp, nil
}

// s3Error pulls the message out of an S3 error body.
//
// S3 reports a failure as XML with a Code and a Message. Answering with the
// status alone leaves "403 Forbidden" in a log where the body would have said
// "SignatureDoesNotMatch", which is the difference between a five-minute and an
// hour-long diagnosis.
func s3Error(body []byte, status string) string {
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if err := xml.Unmarshal(body, &e); err != nil || e.Code == "" {
		if len(body) == 0 {
			return status
		}
		return status + " (" + strings.TrimSpace(string(body)) + ")"
	}
	return e.Code + ": " + e.Message
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.urlFor(key).String(), nil)
	if err != nil {
		return nil, fmt.Errorf("objectstore: build the request for %s: %w", key, err)
	}
	resp, err := s.do(req, emptyPayloadHash)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (s *S3) GetBytes(ctx context.Context, key string) ([]byte, error) {
	r, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// Put writes an object.
//
// The body is sent with a length rather than chunked, because S3's plain PUT
// requires a Content-Length and a chunked upload needs the multipart API. A
// caller that cannot say how long its body is buffers it first — see PutBytes —
// rather than the client guessing.
func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if size < 0 {
		return fmt.Errorf("objectstore: writing %s needs a length", key)
	}

	// The signed payload hash for a streaming body is UNSIGNED-PAYLOAD: the
	// alternative is reading the whole body to hash it, which would mean
	// buffering every archive. S3 accepts it when the request is over TLS, and
	// this client refuses to send a credential in the clear anyway.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.urlFor(key).String(), r)
	if err != nil {
		return fmt.Errorf("objectstore: build the request for %s: %w", key, err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := s.do(req, unsignedPayload)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (s *S3) PutBytes(ctx context.Context, key string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.urlFor(key).String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("objectstore: build the request for %s: %w", key, err)
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/octet-stream")

	// A body already in memory is hashed rather than declared unsigned: it costs
	// one pass and makes the write verifiable end to end, which for the small
	// JSON objects that are the app's own record is the case worth being strict
	// about.
	resp, err := s.do(req, hashHex(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.urlFor(key).String(), nil)
	if err != nil {
		return fmt.Errorf("objectstore: build the request for %s: %w", key, err)
	}
	// S3 answers a delete of a missing key with 204, so ErrNotExist is not
	// raised here — which matches the contract: deleting a state that is already
	// gone is that state reached.
	resp, err := s.do(req, emptyPayloadHash)
	if err != nil {
		if err == ErrNotExist {
			return nil
		}
		return err
	}
	resp.Body.Close()
	return nil
}

func (s *S3) Exists(ctx context.Context, key string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.urlFor(key).String(), nil)
	if err != nil {
		return false, fmt.Errorf("objectstore: build the request for %s: %w", key, err)
	}
	resp, err := s.do(req, emptyPayloadHash)
	if err != nil {
		if err == ErrNotExist {
			return false, nil
		}
		return false, err
	}
	resp.Body.Close()
	return true, nil
}

// listPage is one page of a ListObjectsV2 response.
type listPage struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		ETag         string `xml:"ETag"`
		LastModified string `xml:"LastModified"`
	} `xml:"Contents"`
}

// List returns every object under a prefix.
//
// It pages, because a bucket's listing is bounded at 1000 keys per response and
// a deployment with a few hundred builds under one prefix would silently see
// only the first page — a failure that looks like missing data rather than like
// a truncated read.
func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""

	for {
		u := s.urlFor("")
		q := u.Query()
		q.Set("list-type", "2")
		if clean := Key(prefix); clean != "" {
			q.Set("prefix", clean)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		u.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("objectstore: build the list request: %w", err)
		}
		resp, err := s.do(req, emptyPayloadHash)
		if err != nil {
			return nil, err
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("objectstore: read the list response: %w", err)
		}

		var page listPage
		if err := xml.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("objectstore: parse the list response: %w", err)
		}

		for _, c := range page.Contents {
			out = append(out, Object{
				Key:  c.Key,
				Size: c.Size,
				ETag: strings.Trim(c.ETag, `"`),
			})
		}

		if !page.IsTruncated || page.NextContinuationToken == "" {
			return out, nil
		}
		token = page.NextContinuationToken
	}
}

// emptyPayloadHash is the SHA-256 of an empty body, which is what S3 requires
// for a request that carries none.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// unsignedPayload is what S3 accepts in place of a body's hash.
const unsignedPayload = "UNSIGNED-PAYLOAD"
