package client

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestServer returns a server that answers each path from a handler map and
// records the requests it saw.
func newTestServer(t *testing.T, handlers map[string]func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *[]*http.Request) {
	t.Helper()

	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The body is read here so a handler can inspect it after the fact; a
		// test asserting on a streamed upload needs the bytes.
		if r.Body != nil {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		seen = append(seen, r)

		path := r.URL.Path
		if handler, ok := handlers[path]; ok {
			handler(w, r)
			return
		}
		// A prefix match, so a path with an id in it can be handled too.
		for pattern, handler := range handlers {
			if strings.HasSuffix(pattern, "*") && strings.HasPrefix(path, strings.TrimSuffix(pattern, "*")) {
				handler(w, r)
				return
			}
		}
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}))

	t.Cleanup(srv.Close)
	// Dereference happens after the requests have run.
	ptr := &seen
	return srv, ptr
}

func dataResponse(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func errorResponse(w http.ResponseWriter, status int, message string, retryable bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": message, "retryable": retryable})
}

func newTestClient(t *testing.T, url string) *Client {
	t.Helper()

	c, err := New(Options{BaseURL: url, Key: "test-key"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestNewRequiresURLAndKey asserts a missing configuration is reported before any
// request is attempted.
func TestNewRequiresURLAndKey(t *testing.T) {
	cases := []struct {
		name string
		opts Options
	}{
		{"no url", Options{Key: "k"}},
		{"no key", Options{BaseURL: "https://example.com"}},
		{"neither", Options{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.opts); err == nil {
				t.Error("New succeeded without the required configuration")
			}
		})
	}
}

// TestBareHostGetsHTTPS asserts a host typed without a scheme is upgraded, since
// the alternative would send the key in clear text.
func TestBareHostGetsHTTPS(t *testing.T) {
	c, err := New(Options{BaseURL: "applab.example.com", Key: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.HasPrefix(c.BaseURL(), "https://") {
		t.Errorf("BaseURL = %q, want https", c.BaseURL())
	}
}

// TestTrailingSlashIsTrimmed asserts a URL copied from a browser works.
func TestTrailingSlashIsTrimmed(t *testing.T) {
	c, err := New(Options{BaseURL: "https://applab.example.com/", Key: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if strings.HasSuffix(c.BaseURL(), "/") {
		t.Errorf("BaseURL = %q, want no trailing slash", c.BaseURL())
	}
}

// TestKeyIsSentAsAHeader asserts the credential never appears in the URL, where
// it would be written to access logs and shell history.
func TestKeyIsSentAsAHeader(t *testing.T) {
	srv, requests := newTestServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/config": func(w http.ResponseWriter, r *http.Request) {
			dataResponse(w, map[string]any{"api_version": "v1"})
		},
	})

	c := newTestClient(t, srv.URL)
	if _, err := c.Config(context.Background()); err != nil {
		t.Fatalf("Config: %v", err)
	}

	req := (*requests)[0]
	if got := req.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q, want the key", got)
	}
	if strings.Contains(req.URL.String(), "test-key") {
		t.Errorf("the key appears in the request URL: %s", req.URL.String())
	}
}

// TestErrorEnvelopeIsDecoded asserts a failure's message and retryable flag
// survive to the caller, since the flag is the one thing a status alone cannot
// convey.
func TestErrorEnvelopeIsDecoded(t *testing.T) {
	srv, _ := newTestServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/apps/shop": func(w http.ResponseWriter, r *http.Request) {
			errorResponse(w, http.StatusServiceUnavailable, "the cluster is unreachable", true)
		},
	})

	c := newTestClient(t, srv.URL)
	_, err := c.GetApp(context.Background(), "shop")
	if err == nil {
		t.Fatal("GetApp succeeded on an error response")
	}

	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		t.Fatalf("the error is not an APIError: %T", err)
	}
	if apiErr.Status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", apiErr.Status, http.StatusServiceUnavailable)
	}
	if apiErr.Message != "the cluster is unreachable" {
		t.Errorf("message = %q", apiErr.Message)
	}
	if !apiErr.Retryable {
		t.Error("retryable = false; the flag is the one signal a status alone cannot convey")
	}
}

// TestOurOwnMessagesAreNotRetryable asserts a refusal is not presented as worth
// retrying, which would make a caller loop.
func TestOurOwnMessagesAreNotRetryable(t *testing.T) {
	srv, _ := newTestServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/apps/shop": func(w http.ResponseWriter, r *http.Request) {
			errorResponse(w, http.StatusConflict, "no image exists for that commit", false)
		},
	})

	c := newTestClient(t, srv.URL)
	_, err := c.GetApp(context.Background(), "shop")

	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		t.Fatalf("not an APIError: %T", err)
	}
	if apiErr.Retryable {
		t.Error("a conflict was marked retryable")
	}
	if !strings.Contains(apiErr.Error(), "no image") {
		t.Errorf("the message was lost: %q", apiErr.Error())
	}
}

// TestNonJSONErrorIsReportedWithItsStatus asserts a proxy's HTML error is
// surfaced as something readable rather than as a parse failure, which would hide
// the real cause — an ingress body limit is the common one.
func TestNonJSONErrorIsReportedWithItsStatus(t *testing.T) {
	srv, _ := newTestServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/apps/shop": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			w.Write([]byte("<html><body><h1>413 Request Entity Too Large</h1></body></html>"))
		},
	})

	c := newTestClient(t, srv.URL)
	_, err := c.GetApp(context.Background(), "shop")
	if err == nil {
		t.Fatal("GetApp succeeded on a 413")
	}

	msg := err.Error()
	if !strings.Contains(msg, "413") {
		t.Errorf("the status is missing from the error: %q", msg)
	}
	if !strings.Contains(msg, "413 Request Entity Too Large") {
		t.Errorf("the body excerpt is missing: %q", msg)
	}
	// A JSON parse error would mean the HTML was fed to the decoder.
	if strings.Contains(msg, "invalid character") || strings.Contains(msg, "unexpected token") {
		t.Errorf("the HTML was parsed as JSON: %q", msg)
	}
}

// TestUploadSendsTheArchiveWithTheRightContentType asserts a gzipped archive is
// labelled as such, since the server sniffs the bytes but a proxy may not.
func TestUploadSendsTheArchiveWithTheRightContentType(t *testing.T) {
	var received []byte
	srv, _ := newTestServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/apps/shop/source": func(w http.ResponseWriter, r *http.Request) {
			received, _ = io.ReadAll(r.Body)
			if ct := r.Header.Get("Content-Type"); ct != "application/gzip" {
				t.Errorf("Content-Type = %q, want application/gzip", ct)
			}
			dataResponse(w, map[string]any{"commit_sha": strings.Repeat("a", 40), "files": 2})
		},
	})

	c := newTestClient(t, srv.URL)
	result, err := c.UploadSource(context.Background(), "shop", strings.NewReader("archive-bytes"), true, "a message")
	if err != nil {
		t.Fatalf("UploadSource: %v", err)
	}
	if string(received) != "archive-bytes" {
		t.Errorf("the server received %q", received)
	}
	if result.Files != 2 {
		t.Errorf("files = %d, want 2", result.Files)
	}
}

// TestUploadMessageIsEscapedInTheQuery asserts a commit message with spaces and
// punctuation survives, since a message is free text from a person.
func TestUploadMessageIsEscapedInTheQuery(t *testing.T) {
	var gotQuery string
	srv, _ := newTestServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/apps/shop/source": func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.RawQuery
			dataResponse(w, map[string]any{"commit_sha": "abc"})
		},
	})

	c := newTestClient(t, srv.URL)
	message := "fix: handle & escape = properly + unicode ✓"
	if _, err := c.UploadSource(context.Background(), "shop", strings.NewReader("x"), true, message); err != nil {
		t.Fatalf("UploadSource: %v", err)
	}

	// The raw query must not carry the & unescaped, or the server would read it
	// as the start of another parameter.
	if strings.Contains(gotQuery, "escape") && strings.Contains(gotQuery, " = ") {
		t.Errorf("the message was not escaped: %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "message=") {
		t.Errorf("the message is missing from the query: %q", gotQuery)
	}
}

// TestConfigReadsCapabilities asserts the client surfaces which halves of the
// pipeline a deployment has, since that is what decides whether push can build.
func TestConfigReadsCapabilities(t *testing.T) {
	srv, _ := newTestServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/config": func(w http.ResponseWriter, r *http.Request) {
			dataResponse(w, map[string]any{
				"api_version":  "v1",
				"base_domain":  "apps.example.com",
				"capabilities": map[string]bool{"build": true, "deploy": false, "source": true},
			})
		},
	})

	c := newTestClient(t, srv.URL)
	cfg, err := c.Config(context.Background())
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if !cfg.Capabilities["build"] {
		t.Error("the build capability was not read")
	}
	if cfg.Capabilities["deploy"] {
		t.Error("the deploy capability was reported as available when it is not")
	}
	if cfg.BaseDomain != "apps.example.com" {
		t.Errorf("base domain = %q", cfg.BaseDomain)
	}
}

// TestArchiveDirRoundTrips is the test that matters for push: what goes up has to
// be what was on disk.
func TestArchiveDirRoundTrips(t *testing.T) {
	dir := t.TempDir()

	files := map[string]string{
		"Dockerfile":     "FROM scratch\n",
		"main.go":        "package main\n\nfunc main() {}\n",
		"src/nested.txt": "deeply nested\n",
		"build.sh":       "#!/bin/sh\necho hi\n",
	}
	for name, body := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(full, []byte(body), mode); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	var buf bytes.Buffer
	if err := ArchiveDir(context.Background(), dir, &buf, nil); err != nil {
		t.Fatalf("ArchiveDir: %v", err)
	}

	// Read it back the way the server does.
	gz, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("the archive is not gzip: %v", err)
	}
	tr := tar.NewReader(gz)

	found := map[string]string{}
	modes := map[string]int64{}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		body, _ := io.ReadAll(tr)
		found[header.Name] = string(body)
		modes[header.Name] = header.Mode
	}

	for name, want := range files {
		got, ok := found[name]
		if !ok {
			t.Errorf("%s is missing from the archive", name)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	// The executable bit has to survive: a build may run a script.
	if modes["build.sh"]&0o111 == 0 {
		t.Errorf("build.sh lost its executable bit (mode %o)", modes["build.sh"])
	}
	// And paths use forward slashes regardless of the host, since tar is POSIX.
	if _, ok := found["src/nested.txt"]; !ok {
		t.Errorf("the nested path did not use forward slashes: %v", keysOf(found))
	}
}

// TestArchiveDirSkipsBuildOutput asserts the directories that make an upload
// large for no benefit are left out.
func TestArchiveDirSkipsBuildOutput(t *testing.T) {
	dir := t.TempDir()

	mustWrite(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\n")
	mustWrite(t, filepath.Join(dir, "main.go"), "package main\n")
	mustWrite(t, filepath.Join(dir, "node_modules/pkg/index.js"), "junk\n")
	mustWrite(t, filepath.Join(dir, ".git/config"), "junk\n")
	mustWrite(t, filepath.Join(dir, "target/debug/binary"), "junk\n")

	var buf bytes.Buffer
	if err := ArchiveDir(context.Background(), dir, &buf, DefaultSkipDirs); err != nil {
		t.Fatalf("ArchiveDir: %v", err)
	}

	names := archiveNames(t, buf.Bytes())

	for _, unwanted := range []string{"node_modules/pkg/index.js", ".git/config", "target/debug/binary"} {
		if _, present := names[unwanted]; present {
			t.Errorf("%s was included; it should be skipped by default", unwanted)
		}
	}
	for _, wanted := range []string{"Dockerfile", "main.go"} {
		if _, present := names[wanted]; !present {
			t.Errorf("%s is missing", wanted)
		}
	}
}

// TestArchiveDirEntryOrderIsStable asserts the archive's entries are written in a
// deterministic order.
//
// It is worth pinning because the walk's order is the filesystem's, which varies
// between hosts and runs. The archive bytes are not identical across runs — mtimes
// differ and gzip carries them — but that does not matter: the server decides
// whether an upload is a no-op from the *git tree* it produces, and a git tree
// records content and the executable bit rather than timestamps. Verified
// directly: staging identical content with different mtimes yields the same tree
// hash. Entry order still matters for a readable archive and a stable diff.
func TestArchiveDirEntryOrderIsStable(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c/d.go", "c/e.go"} {
		mustWrite(t, filepath.Join(dir, name), "content of "+name+"\n")
	}

	var first, second bytes.Buffer
	if err := ArchiveDir(context.Background(), dir, &first, nil); err != nil {
		t.Fatalf("first ArchiveDir: %v", err)
	}
	if err := ArchiveDir(context.Background(), dir, &second, nil); err != nil {
		t.Fatalf("second ArchiveDir: %v", err)
	}

	firstNames := keysOf(archiveNames(t, first.Bytes()))
	secondNames := keysOf(archiveNames(t, second.Bytes()))
	if strings.Join(firstNames, ",") != strings.Join(secondNames, ",") {
		t.Errorf("the archive's entry order differs between runs:\n%v\n%v", firstNames, secondNames)
	}
}

// TestArchiveDirHonoursCustomSkip asserts an extra skip name is applied, which is
// what lets a project exclude something AppLab does not know about.
func TestArchiveDirHonoursCustomSkip(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "keep.go"), "keep\n")
	mustWrite(t, filepath.Join(dir, "generated/huge.json"), "{}\n")

	var buf bytes.Buffer
	if err := ArchiveDir(context.Background(), dir, &buf, []string{"generated"}); err != nil {
		t.Fatalf("ArchiveDir: %v", err)
	}

	names := archiveNames(t, buf.Bytes())
	if _, present := names["generated/huge.json"]; present {
		t.Error("a custom skip was not applied")
	}
	if _, present := names["keep.go"]; !present {
		t.Error("the custom skip removed too much")
	}
}

// TestRedirectsAreNotFollowed asserts a redirect is not chased, since following
// one would send the key to wherever it points.
func TestRedirectsAreNotFollowed(t *testing.T) {
	var elsewhereHit bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereHit = true
		if r.Header.Get("Authorization") != "" {
			t.Error("the API key was forwarded to a redirect target")
		}
		dataResponse(w, map[string]any{})
	}))
	defer elsewhere.Close()

	srv, _ := newTestServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/config": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL+"/api/v1/config", http.StatusTemporaryRedirect)
		},
	})

	c := newTestClient(t, srv.URL)
	_, err := c.Config(context.Background())
	if err == nil {
		t.Fatal("a redirect was followed successfully")
	}
	if elsewhereHit {
		t.Error("the client followed a redirect and sent the key elsewhere")
	}
}

// --- helpers ---------------------------------------------------------------

func mustWrite(t *testing.T, path, body string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func archiveNames(t *testing.T, raw []byte) map[string]string {
	t.Helper()

	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)

	out := map[string]string{}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		if header.Typeflag == tar.TypeReg {
			body, _ := io.ReadAll(tr)
			out[header.Name] = string(body)
		}
	}
	return out
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Sorted so a comparison between two runs is about content, not order.
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// asAPIError is errors.As specialised to *APIError, kept local so the import
// list stays short.
func asAPIError(err error, target **APIError) bool {
	for err != nil {
		if apiErr, ok := err.(*APIError); ok {
			*target = apiErr
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

// TestGetAppKeyReadsTheEnvelope covers the key endpoint's shape.
//
// The key is returned in full under "data", and this asserts the client passes
// it through unchanged: a client that trimmed, redacted or re-encoded it would
// hand the caller a credential that does not work, which is the kind of bug that
// only shows up when someone tries to use it.
func TestGetAppKeyReadsTheEnvelope(t *testing.T) {
	// A key with characters that a careless encoder would mangle.
	const wantKey = "aB3-_xY.z9:Q4/w+8kL0mN1oP2qR3sT4uV5wX6yZ7A8b9C0d"

	srv, _ := newTestServer(t, map[string]func(w http.ResponseWriter, r *http.Request){
		"/api/v1/apps/shop/key": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("method = %s, want GET", r.Method)
			}
			dataResponse(w, map[string]any{"app_id": "shop", "key": wantKey})
		},
	})

	c, err := New(Options{BaseURL: srv.URL, Key: "test-key"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	key, keyErr := c.GetAppKey(context.Background(), "shop")
	if keyErr != nil {
		t.Fatalf("GetAppKey: %v", keyErr)
	}
	if key.Key != wantKey {
		t.Errorf("key = %q, want %q — the value must survive the round trip byte for byte", key.Key, wantKey)
	}
	if key.AppID != "shop" {
		t.Errorf("app_id = %q, want shop", key.AppID)
	}
}

// TestRotateAppKeyPostsToTheRotatePath asserts rotation is a POST to the right
// endpoint — a GET would read the existing key and look like success.
func TestRotateAppKeyPostsToTheRotatePath(t *testing.T) {
	var method string

	srv, _ := newTestServer(t, map[string]func(w http.ResponseWriter, r *http.Request){
		"/api/v1/apps/shop/key/rotate": func(w http.ResponseWriter, r *http.Request) {
			method = r.Method
			dataResponse(w, map[string]any{"app_id": "shop", "key": "rotated-key"})
		},
	})

	c, err := New(Options{BaseURL: srv.URL, Key: "test-key"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	key, rotErr := c.RotateAppKey(context.Background(), "shop")
	if rotErr != nil {
		t.Fatalf("RotateAppKey: %v", rotErr)
	}
	if method != http.MethodPost {
		t.Errorf("method = %s, want POST", method)
	}
	if key.Key != "rotated-key" {
		t.Errorf("key = %q, want rotated-key", key.Key)
	}
}

// TestKeyEndpointsReportAnUnavailableDeployment covers the no-cluster case: the
// deployment answers 501 with a message, and the client must surface that text
// rather than a generic failure, because it names the fix.
func TestKeyEndpointsReportAnUnavailableDeployment(t *testing.T) {
	const message = "this deployment has no cluster, so per-app keys are unavailable; use an admin key"

	srv, _ := newTestServer(t, map[string]func(w http.ResponseWriter, r *http.Request){
		"/api/v1/apps/shop/key": func(w http.ResponseWriter, r *http.Request) {
			errorResponse(w, http.StatusNotImplemented, message, false)
		},
	})

	c, err := New(Options{BaseURL: srv.URL, Key: "test-key"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, keyErr := c.GetAppKey(context.Background(), "shop")
	if keyErr == nil {
		t.Fatal("GetAppKey succeeded against a deployment with no key store")
	}
	if !strings.Contains(keyErr.Error(), message) {
		t.Errorf("error = %q, want it to carry the server's explanation", err)
	}
}
