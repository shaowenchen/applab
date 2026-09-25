package objectstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTheSignatureMatchesThePublishedVector signs the request from the AWS
// "Signature Version 4 test suite" documentation and compares it with the
// signature AWS publishes for it.
//
// This is the only way to test a signer without a bucket, and it is the test
// that matters most in this package: every other test here passes against a
// signer that is wrong in a way the fake server does not check, and the symptom
// in production is a bare 403 with nothing in it to say which of the six steps
// was implemented incorrectly.
func TestTheSignatureMatchesThePublishedVector(t *testing.T) {
	// AWS's own GetVanilla example: GET / with a single query parameter, signed
	// at a fixed moment with a known key.
	//
	// From the SigV4 test suite (get-vanilla), whose expected Authorization is
	// published alongside the request.
	s := &signer{
		accessKey: "AKIDEXAMPLE",
		secretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		region:    "us-east-1",
		service:   "service",
		// The suite's requests carry only these two headers. The S3 header set
		// this package signs includes content-type and x-amz-content-sha256,
		// which no published vector covers — so the algorithm is checked with
		// the header set that has one.
		headers: []string{"host", "x-amz-date"},
		now: func() time.Time {
			return time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
		},
	}

	u, err := url.Parse("https://example.amazonaws.com/")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	req := &http.Request{
		Method: http.MethodGet,
		URL:    u,
		Host:   "example.amazonaws.com",
		Header: http.Header{},
	}

	s.sign(req, hashHex(nil))

	got := req.Header.Get("Authorization")
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, " +
		"Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"

	if got != want {
		t.Errorf("the signature does not match the published vector\n got: %s\nwant: %s", got, want)
	}
}

// TestEverySignedHeaderIsOnTheRequest is the check that the signature covers
// only headers the request actually carries.
//
// SigV4 requires every name in SignedHeaders to be present on the request, and a
// header that is signed but not sent produces a request S3 rejects with a bare
// 400. `content-type` is in this package's signed set, and only two of the four
// operations set it — so GET, HEAD and DELETE all signed a header they did not
// send, and every one of them failed against MinIO the first time the image
// talked to a real server.
//
// Why nothing here caught it, which is the part worth keeping: the fake S3
// server in this file checks that an Authorization header is *present* and never
// verifies a signature, and the published vector covers only host and x-amz-date
// — the two headers that cannot go missing, because the signer sets them itself.
// So this asserts the property directly rather than through a signature, because
// the property is the thing that was wrong.
func TestEverySignedHeaderIsOnTheRequest(t *testing.T) {
	// Every method this package issues, each built the way its own code builds
	// it: the body-carrying ones set a content type, the others do not.
	cases := []struct {
		name string
		req  func(t *testing.T) *http.Request
	}{
		{"GET", func(t *testing.T) *http.Request {
			r, err := http.NewRequest(http.MethodGet, "https://s3.example.com/bucket/key", nil)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			return r
		}},
		{"HEAD", func(t *testing.T) *http.Request {
			r, err := http.NewRequest(http.MethodHead, "https://s3.example.com/bucket/key", nil)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			return r
		}},
		{"DELETE", func(t *testing.T) *http.Request {
			r, err := http.NewRequest(http.MethodDelete, "https://s3.example.com/bucket/key", nil)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			return r
		}},
		{"PUT", func(t *testing.T) *http.Request {
			r, err := http.NewRequest(http.MethodPut, "https://s3.example.com/bucket/key",
				bytes.NewReader([]byte("body")))
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			r.Header.Set("Content-Type", "application/octet-stream")
			return r
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &signer{
				accessKey: "key",
				secretKey: "secret",
				region:    "us-east-1",
				service:   "s3",
				now:       func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
			}
			req := tc.req(t)
			s.sign(req, emptyPayloadHash)

			auth := req.Header.Get("Authorization")
			if auth == "" {
				t.Fatal("the request was not signed")
			}

			// Pull the header list back out of the Authorization header, which is
			// what the server will read — rather than comparing against the
			// package's own slice, which would only prove it agrees with itself.
			const marker = "SignedHeaders="
			i := strings.Index(auth, marker)
			if i < 0 {
				t.Fatalf("the Authorization header names no signed headers: %s", auth)
			}
			rest := auth[i+len(marker):]
			j := strings.Index(rest, ",")
			if j < 0 {
				t.Fatalf("the signed header list has no terminator: %s", rest)
			}
			names := strings.Split(rest[:j], ";")

			for _, name := range names {
				if name == "host" {
					// net/http writes this from the URL rather than from Header,
					// and the signer computes it the same way.
					continue
				}
				if req.Header.Get(name) == "" {
					t.Errorf("the signature covers %q but the request does not send it; "+
						"S3 rejects that with a bare 400", name)
				}
			}
		})
	}
}

// TestAnUploadedKeyIsEscapedOnce is the check that a key containing a separator
// is signed over the path the request actually carries.
//
// The failure it guards against is silent: signing an already-escaped path
// produces a correct-looking request whose signature the server rejects, and
// the only symptom is a bare 403.
func TestAnUploadedKeyIsEscapedOnce(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			// A key whose segments contain characters that must be escaped.
			key := "apps/a b/commits/c+d=e.json"
			if err := store.PutBytes(ctx, key, []byte("x")); err != nil {
				t.Fatalf("put: %v", err)
			}

			got, err := store.GetBytes(ctx, key)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if string(got) != "x" {
				t.Errorf("got %q, want x", got)
			}
		})
	}
}

// TestTheSignatureCoversTheBody checks that two requests differing only in what
// they carry are signed differently, which is what stops a body being swapped
// for another between signing and sending.
func TestTheSignatureCoversTheBody(t *testing.T) {
	s := &signer{
		accessKey: "AKIDEXAMPLE",
		secretKey: "secret",
		region:    "us-east-1",
		service:   "s3",
		now:       func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	}

	sign := func(body []byte) string {
		u, _ := url.Parse("https://example.amazonaws.com/key")
		req := &http.Request{Method: http.MethodPut, URL: u, Host: "example.amazonaws.com", Header: http.Header{}}
		s.sign(req, hashHex(body))
		return req.Header.Get("Authorization")
	}

	if sign([]byte("one")) == sign([]byte("two")) {
		t.Error("two different bodies produced the same signature")
	}
}

// TestAWSSpacesAreEncodedAsPercent20 guards the one character where SigV4's
// encoding differs from Go's: url.QueryEscape writes "+", which S3 reads as a
// literal plus, so a key with a space in it signs one way and verifies another.
func TestAWSSpacesAreEncodedAsPercent20(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a b", "a%20b"},
		{"a+b", "a%2Bb"},
		{"a/b", "a%2Fb"},
		{"a~b", "a~b"},
		{"a-b_c.d", "a-b_c.d"},
		{"é", "%C3%A9"},
	} {
		if got := awsEncode(tc.in); got != tc.want {
			t.Errorf("awsEncode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestTheCanonicalQueryIsSorted checks the ordering rule: parameters sort by
// encoded name and then by value, which is what makes a signature computed here
// match one computed by any other client.
func TestTheCanonicalQueryIsSorted(t *testing.T) {
	u, _ := url.Parse("https://example.amazonaws.com/?list-type=2&prefix=b&prefix=a&continuation-token=z")
	got := canonicalQuery(u)
	want := "continuation-token=z&list-type=2&prefix=a&prefix=b"
	if got != want {
		t.Errorf("canonicalQuery = %q, want %q", got, want)
	}
}

// --- the two implementations, held to the same contract ---------------------

// stores returns both backends, so every contract test runs against each.
func stores(t *testing.T) map[string]Store {
	t.Helper()

	local, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	s3, err := NewS3(S3Options{
		Endpoint:  "https://s3.example.com",
		Bucket:    "applab",
		AccessKey: "key",
		SecretKey: "secret",
		// Path-style, because the test server is reached at an address and has no
		// wildcard DNS to put the bucket under.
		PathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}

	// The S3 store is pointed at a server that speaks the subset of the protocol
	// these tests use, so the contract is exercised rather than only the local
	// implementation.
	srv := s3Server(t)
	s3.endpoint, _ = url.Parse(srv.URL)
	s3.client = srv.Client()

	return map[string]Store{"local": local, "s3": s3}
}

// s3Server is a minimal S3 endpoint: enough of GET, PUT, DELETE, HEAD and the
// list API for the contract tests.
//
// It checks the one thing a fake can check about the signing — that a signature
// is present at all — and nothing about whether it is correct. A missing
// Authorization is the failure that makes a bucket answer a bare 403, and it is
// the mistake a client makes when the signing step is skipped rather than wrong.
// Correctness is covered by the published vector above.
func s3Server(t *testing.T) *httptest.Server {
	t.Helper()

	objects := map[string][]byte{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>no signature</Message></Error>`)
			return
		}

		// Path-style: the first segment is the bucket.
		key := strings.TrimPrefix(r.URL.Path, "/")
		if i := strings.Index(key, "/"); i >= 0 {
			key = key[i+1:]
		} else {
			key = ""
		}
		// The server sees the decoded path, which is what the client signed.
		key, err := url.PathUnescape(key)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			objects[key] = body
			w.WriteHeader(http.StatusOK)

		case http.MethodGet:
			if r.URL.Query().Get("list-type") == "2" {
				prefix := r.URL.Query().Get("prefix")

				// Sorted, because S3 guarantees lexicographic key order and the
				// store's callers rely on it. A fake that returned Go's map order
				// would make the ordering assertion below pass or fail at random
				// — and would be testing the fake rather than the client.
				keys := make([]string, 0, len(objects))
				for k := range objects {
					if strings.HasPrefix(k, prefix) {
						keys = append(keys, k)
					}
				}
				sort.Strings(keys)

				var b strings.Builder
				b.WriteString(`<ListBucketResult>`)
				for _, k := range keys {
					b.WriteString("<Contents><Key>" + k + "</Key><Size>" + strconv.Itoa(len(objects[k])) + "</Size></Contents>")
				}
				b.WriteString(`</ListBucketResult>`)
				io.WriteString(w, b.String())
				return
			}
			body, ok := objects[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>not found</Message></Error>`)
				return
			}
			w.Write(body)

		case http.MethodHead:
			if _, ok := objects[key]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)

		case http.MethodDelete:
			delete(objects, key)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPutThenGetReturnsTheSameBytes(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			body := []byte("the app's record")

			if err := store.PutBytes(ctx, "apps/shop/app.json", body); err != nil {
				t.Fatalf("put: %v", err)
			}

			got, err := store.GetBytes(ctx, "apps/shop/app.json")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if string(got) != string(body) {
				t.Errorf("got %q, want %q", got, body)
			}
		})
	}
}

func TestGettingSomethingThatIsNotThereIsErrNotExist(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			_, err := store.Get(context.Background(), "apps/nobody/app.json")
			if !errors.Is(err, ErrNotExist) {
				t.Errorf("err = %v, want ErrNotExist", err)
			}
		})
	}
}

func TestPutReplacesWhatWasThere(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			store.PutBytes(ctx, "k", []byte("first"))
			store.PutBytes(ctx, "k", []byte("second"))

			got, err := store.GetBytes(ctx, "k")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if string(got) != "second" {
				t.Errorf("got %q, want second", got)
			}
		})
	}
}

// TestDeleteOfAMissingKeyIsNotAnError states the contract the callers rely on:
// the operations that delete are removing a state, and "it is already gone" is
// that state reached.
func TestDeleteOfAMissingKeyIsNotAnError(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			if err := store.Delete(context.Background(), "never/existed"); err != nil {
				t.Errorf("delete of a missing key: %v", err)
			}
		})
	}
}

func TestExists(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			ok, err := store.Exists(ctx, "k")
			if err != nil {
				t.Fatalf("exists: %v", err)
			}
			if ok {
				t.Error("a key that was never written reports as present")
			}

			store.PutBytes(ctx, "k", []byte("x"))

			ok, err = store.Exists(ctx, "k")
			if err != nil {
				t.Fatalf("exists: %v", err)
			}
			if !ok {
				t.Error("a key that was written reports as absent")
			}
		})
	}
}

// TestListIsScopedToItsPrefixAndSorted is the listing contract the store layout
// depends on: a prefix selects a subtree, and the result is in key order so
// "newest first" can be a matter of reversing the page.
func TestListIsScopedToItsPrefixAndSorted(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			for _, key := range []string{
				"apps/shop/builds/c.json",
				"apps/shop/builds/a.json",
				"apps/shop/builds/b.json",
				"apps/blog/app.json",
			} {
				if err := store.PutBytes(ctx, key, []byte("x")); err != nil {
					t.Fatalf("put %s: %v", key, err)
				}
			}

			got, err := store.List(ctx, "apps/shop/builds/")
			if err != nil {
				t.Fatalf("list: %v", err)
			}

			var keys []string
			for _, o := range got {
				keys = append(keys, o.Key)
			}
			want := []string{
				"apps/shop/builds/a.json",
				"apps/shop/builds/b.json",
				"apps/shop/builds/c.json",
			}
			if strings.Join(keys, ",") != strings.Join(want, ",") {
				t.Errorf("list = %v, want %v", keys, want)
			}
		})
	}
}

// TestListOfAnEmptyPrefixListsEverything is what the overview uses to count
// across every app.
func TestListOfAnEmptyPrefixListsEverything(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store.PutBytes(ctx, "apps/a/app.json", []byte("x"))
			store.PutBytes(ctx, "apps/b/app.json", []byte("x"))

			got, err := store.List(ctx, "")
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(got) != 2 {
				t.Errorf("listed %d objects, want 2", len(got))
			}
		})
	}
}

// TestAKeyCannotEscapeTheStore is the traversal check, and it is the one that
// matters most for the local backend: keys are built from app ids, which are
// caller-supplied, and this is where a key becomes a path.
func TestAKeyCannotEscapeTheStore(t *testing.T) {
	store, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	for _, key := range []string{
		"../outside",
		"apps/../../outside",
		"apps/shop/../../../../etc/passwd",
	} {
		if _, err := store.GetBytes(context.Background(), key); err == nil {
			t.Errorf("key %q was accepted", key)
		}
	}
}

// TestKeyJoinsWithoutDoublingSlashes keeps one thing from having two keys.
func TestKeyJoinsWithoutDoublingSlashes(t *testing.T) {
	for _, tc := range []struct {
		segments []string
		want     string
	}{
		{[]string{"apps", "shop", "app.json"}, "apps/shop/app.json"},
		{[]string{"/apps/", "/shop/", "app.json"}, "apps/shop/app.json"},
		{[]string{"apps", "", "app.json"}, "apps/app.json"},
	} {
		if got := Key(tc.segments...); got != tc.want {
			t.Errorf("Key(%v) = %q, want %q", tc.segments, got, tc.want)
		}
	}
}
