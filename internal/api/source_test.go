package api_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/auth"
	"github.com/shaowenchen/applab/internal/config"
	"github.com/shaowenchen/applab/internal/source"
	"github.com/shaowenchen/applab/internal/store"
)

// newSourceServer builds a Server with source storage attached, so the upload
// endpoints exist. It mirrors what cmd/applab wires up.
func newSourceServer(t *testing.T) (*api.Server, *store.Store, *source.Store) {
	t.Helper()

	dataDir := t.TempDir()

	st, err := store.Open(context.Background(), filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	src, err := source.New(source.Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	cfg := config.Default()
	cfg.Keys = []string{"test-key"}
	cfg.BaseDomain = "apps.example.com"
	cfg.DataDir = dataDir

	return api.New(cfg, st, auth.New(cfg.Keys)).WithSource(src), st, src
}

// tarFiles builds a tar archive from a name-to-content map.
func tarFiles(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(body)),
		}); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar body %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

// gzipBytes compresses an archive.
func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// TestUploadSourceRawBody covers the main path an agent takes: a tarball piped
// straight into the request body.
func TestUploadSourceRawBody(t *testing.T) {
	srv, _, _ := newSourceServer(t)
	h := srv.Handler()

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"}); rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d (%s)", rec.Code, rec.Body.String())
	}

	archive := gzipBytes(t, tarFiles(t, map[string]string{
		"Dockerfile": "FROM scratch\n",
		"main.go":    "package main\n",
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/shop/source?message=hello", bytes.NewReader(archive))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d (%s)", rec.Code, rec.Body.String())
	}

	var result struct {
		CommitSHA string `json:"commit_sha"`
		Message   string `json:"message"`
		Files     int    `json:"files"`
	}
	decodeData(t, rec, &result)

	if result.CommitSHA == "" {
		t.Error("no commit was produced")
	}
	if result.Message != "hello" {
		t.Errorf("message = %q, want %q", result.Message, "hello")
	}
	// The upload's two files, plus the ones applab seeds into every tree.
	if result.Files != 2+source.SeededFileCount() {
		t.Errorf("files = %d, want %d", result.Files, 2+source.SeededFileCount())
	}
}

// TestUploadSourceMultipart covers the curl -F form, which is the other natural
// way a human or script sends an archive.
func TestUploadSourceMultipart(t *testing.T) {
	srv, _, _ := newSourceServer(t)
	h := srv.Handler()

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"}); rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d", rec.Code)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	partHeader := textproto.MIMEHeader{}
	partHeader.Set("Content-Disposition", `form-data; name="file"; filename="source.tar.gz"`)
	partHeader.Set("Content-Type", "application/gzip")
	part, err := writer.CreatePart(partHeader)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(gzipBytes(t, tarFiles(t, map[string]string{"a.txt": "one\n"}))); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/shop/source", &body)
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("multipart upload: %d (%s)", rec.Code, rec.Body.String())
	}

	var result struct {
		Files int `json:"files"`
	}
	decodeData(t, rec, &result)
	if want := 1 + source.SeededFileCount(); result.Files != want {
		t.Errorf("files = %d, want %d", result.Files, want)
	}
}

// TestUploadSourceRejectsTraversal asserts the API surfaces a hostile archive as
// a 400 rather than a 500, since the caller can fix a bad archive and cannot fix
// an internal error.
func TestUploadSourceRejectsTraversal(t *testing.T) {
	srv, _, _ := newSourceServer(t)
	h := srv.Handler()

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"}); rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d", rec.Code)
	}

	archive := tarFiles(t, map[string]string{
		"ok.txt":    "fine\n",
		"../escape": "nope\n",
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/shop/source", bytes.NewReader(archive))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d for an archive that escapes its destination", rec.Code, http.StatusBadRequest)
	}

	var body struct {
		Error     string `json:"error"`
		Retryable bool   `json:"retryable"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	// Retrying the same hostile archive cannot help, and saying otherwise would
	// make a client loop.
	if body.Retryable {
		t.Error("a rejected archive is marked retryable; retrying it cannot succeed")
	}
}

// TestUploadSourceRequiresKey asserts a new route is not accidentally open.
func TestUploadSourceRequiresKey(t *testing.T) {
	srv, _, _ := newSourceServer(t)

	// Create the app with a key first, then attempt the upload without one.
	if rec := doRequest(t, srv.Handler(), http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"}); rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d", rec.Code)
	}

	paths := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/apps/shop/source"},
		{http.MethodGet, "/api/v1/apps/shop/commits"},
		{http.MethodPost, "/api/v1/apps/shop/source/uploads"},
		{http.MethodPut, "/api/v1/apps/shop/source/uploads/abc/parts/1"},
		{http.MethodPost, "/api/v1/apps/shop/source/uploads/abc/complete"},
	}

	for _, p := range paths {
		req := httptest.NewRequest(p.method, p.path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s returned %d without a key, want 401", p.method, p.path, rec.Code)
		}
	}
}

// TestCommitHistory asserts a second upload extends the history and the tip is
// reported.
func TestCommitHistory(t *testing.T) {
	srv, _, _ := newSourceServer(t)
	h := srv.Handler()

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"}); rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d", rec.Code)
	}

	upload := func(content, message string) string {
		archive := tarFiles(t, map[string]string{"a.txt": content})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/shop/source?message="+message, bytes.NewReader(archive))
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("upload %q: %d (%s)", message, rec.Code, rec.Body.String())
		}
		var result struct {
			CommitSHA string `json:"commit_sha"`
		}
		decodeData(t, rec, &result)
		return result.CommitSHA
	}

	first := upload("one\n", "first")
	second := upload("two\n", "second")

	if first == second {
		t.Fatal("the second upload did not create a new commit")
	}

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/commits", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list commits: %d", rec.Code)
	}

	var listing struct {
		Commits []struct {
			SHA     string `json:"sha"`
			Message string `json:"message"`
			Files   int    `json:"files"`
		} `json:"commits"`
		Head string `json:"head"`
	}
	decodeData(t, rec, &listing)

	// Two uploads on top of the commit a new repository starts with.
	if want := 2 + source.SeededCommitCount(); len(listing.Commits) != want {
		t.Fatalf("got %d commits, want %d", len(listing.Commits), want)
	}
	if listing.Commits[0].SHA != second {
		t.Errorf("newest commit = %s, want %s", listing.Commits[0].SHA, second)
	}
	if listing.Head != second {
		t.Errorf("head = %s, want %s", listing.Head, second)
	}
	if listing.Commits[0].Message != "second" {
		t.Errorf("message = %q, want %q", listing.Commits[0].Message, "second")
	}
}

// TestChunkedUpload asserts the whole multi-part path: begin, send parts,
// complete, and the assembled archive commits correctly.
func TestChunkedUpload(t *testing.T) {
	srv, _, _ := newSourceServer(t)
	h := srv.Handler()

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"}); rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d", rec.Code)
	}

	archive := gzipBytes(t, tarFiles(t, map[string]string{
		"big.txt": strings.Repeat("content\n", 500),
		"b.txt":   "second file\n",
	}))

	// Split into three parts of roughly equal size.
	partSize := (len(archive) + 2) / 3
	var parts [][]byte
	for i := 0; i < len(archive); i += partSize {
		end := i + partSize
		if end > len(archive) {
			end = len(archive)
		}
		parts = append(parts, archive[i:end])
	}

	// Begin.
	beginBody, _ := json.Marshal(map[string]any{
		"total":      len(parts),
		"chunk_size": partSize,
		"message":    "chunked",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/shop/source/uploads", bytes.NewReader(beginBody))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("begin upload: %d (%s)", rec.Code, rec.Body.String())
	}
	var begin struct {
		UploadID string `json:"upload_id"`
	}
	decodeData(t, rec, &begin)
	if begin.UploadID == "" {
		t.Fatal("begin returned no upload_id")
	}

	// Send the parts out of order, which the API promises to accept.
	for _, i := range []int{len(parts), 1} {
		path := "/api/v1/apps/shop/source/uploads/" + begin.UploadID + "/parts/" + itoa(i)
		req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(parts[i-1]))
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("part %d: %d (%s)", i, rec.Code, rec.Body.String())
		}
	}
	if len(parts) > 2 {
		path := "/api/v1/apps/shop/source/uploads/" + begin.UploadID + "/parts/2"
		req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(parts[1]))
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("part 2: %d", rec.Code)
		}
	}

	// Complete.
	req = httptest.NewRequest(http.MethodPost,
		"/api/v1/apps/shop/source/uploads/"+begin.UploadID+"/complete", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("complete: %d (%s)", rec.Code, rec.Body.String())
	}
	var result struct {
		CommitSHA string `json:"commit_sha"`
		Files     int    `json:"files"`
	}
	decodeData(t, rec, &result)

	if result.CommitSHA == "" {
		t.Error("the chunked upload produced no commit")
	}
	if want := 2 + source.SeededFileCount(); result.Files != want {
		t.Errorf("files = %d, want %d; the parts were not reassembled correctly", result.Files, want)
	}
}

// TestChunkedUploadMissingPartIsRejected asserts an incomplete upload fails
// rather than committing a truncated archive, which would look like a
// successful upload of broken source.
func TestChunkedUploadMissingPartIsRejected(t *testing.T) {
	srv, _, _ := newSourceServer(t)
	h := srv.Handler()

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"}); rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d", rec.Code)
	}

	beginBody, _ := json.Marshal(map[string]any{"total": 3, "chunk_size": 100})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/shop/source/uploads", bytes.NewReader(beginBody))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var begin struct {
		UploadID string `json:"upload_id"`
	}
	decodeData(t, rec, &begin)

	// Send only the first part.
	req = httptest.NewRequest(http.MethodPut,
		"/api/v1/apps/shop/source/uploads/"+begin.UploadID+"/parts/1", bytes.NewReader([]byte("partial")))
	req.Header.Set("Authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("part 1: %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost,
		"/api/v1/apps/shop/source/uploads/"+begin.UploadID+"/complete", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("completing with missing parts returned %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// TestChunkedUploadPartIndexBounds asserts a part index outside the declared
// range is refused, so a client's arithmetic error is reported rather than
// silently stored.
func TestChunkedUploadPartIndexBounds(t *testing.T) {
	srv, _, _ := newSourceServer(t)
	h := srv.Handler()

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"}); rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d", rec.Code)
	}

	beginBody, _ := json.Marshal(map[string]any{"total": 2, "chunk_size": 100})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/shop/source/uploads", bytes.NewReader(beginBody))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var begin struct {
		UploadID string `json:"upload_id"`
	}
	decodeData(t, rec, &begin)

	for _, index := range []string{"0", "3", "999"} {
		req := httptest.NewRequest(http.MethodPut,
			"/api/v1/apps/shop/source/uploads/"+begin.UploadID+"/parts/"+index, bytes.NewReader([]byte("x")))
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("part index %s returned %d, want %d", index, rec.Code, http.StatusBadRequest)
		}
	}
}

// TestUploadToMissingAppIsNotFound asserts the app is resolved before any work
// happens, so a typo does not produce a confusing storage error.
func TestUploadToMissingAppIsNotFound(t *testing.T) {
	srv, _, _ := newSourceServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/nonexistent/source",
		bytes.NewReader(tarFiles(t, map[string]string{"a.txt": "x"})))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// TestUploadAcrossAppsIsIsolated asserts an upload id from one app cannot be
// used to plant source into another. The id is a bearer token for those parts,
// so it must be checked against the app it was issued for.
func TestUploadAcrossAppsIsIsolated(t *testing.T) {
	srv, _, _ := newSourceServer(t)
	h := srv.Handler()

	for _, id := range []string{"one", "two"} {
		if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": id}); rec.Code != http.StatusCreated {
			t.Fatalf("create app %s: %d", id, rec.Code)
		}
	}

	// Begin an upload for app "one".
	beginBody, _ := json.Marshal(map[string]any{"total": 1, "chunk_size": 100})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/one/source/uploads", bytes.NewReader(beginBody))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var begin struct {
		UploadID string `json:"upload_id"`
	}
	decodeData(t, rec, &begin)

	// Try to use it against app "two".
	req = httptest.NewRequest(http.MethodPut,
		"/api/v1/apps/two/source/uploads/"+begin.UploadID+"/parts/1", bytes.NewReader([]byte("x")))
	req.Header.Set("Authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("an upload id issued for one app was accepted for another (status %d)", rec.Code)
	}
}

// TestGetCommitByAbbreviatedSHA asserts a commit may be addressed by the short
// form a caller was shown.
func TestGetCommitByAbbreviatedSHA(t *testing.T) {
	srv, _, _ := newSourceServer(t)
	h := srv.Handler()

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"}); rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/shop/source?message=short",
		bytes.NewReader(tarFiles(t, map[string]string{"a.txt": "x"})))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var upload struct {
		CommitSHA string `json:"commit_sha"`
	}
	decodeData(t, rec, &upload)

	rec = doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/commits/"+upload.CommitSHA[:8], nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get commit by abbreviated sha: %d (%s)", rec.Code, rec.Body.String())
	}

	var commit struct {
		SHA     string `json:"sha"`
		Message string `json:"message"`
	}
	decodeData(t, rec, &commit)

	if commit.SHA != upload.CommitSHA {
		t.Errorf("resolved sha = %s, want %s", commit.SHA, upload.CommitSHA)
	}
	if commit.Message != "short" {
		t.Errorf("message = %q, want %q", commit.Message, "short")
	}
}
