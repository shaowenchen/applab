package applabext

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The helpers in this file are the one part of the generated SDKs that is not
// exercised by building the specification, and Go is the one language whose
// toolchain is on a developer's machine here — so this is also the regression
// suite for the protocol they implement: a chunked upload, a followed log, a
// deterministic archive, and a strict build-id lookup.

func TestPackageDirectoryIsDeterministicAndSkipsNoise(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.go", "package main\n")
	writeFile(t, dir, "sub/a.txt", "hello\n")
	writeFile(t, dir, ".git/HEAD", "ref: refs/heads/main\n")
	writeFile(t, dir, "node_modules/dep/index.js", "x\n")

	first, err := PackageDirectory(dir)
	if err != nil {
		t.Fatalf("package: %v", err)
	}
	defer os.Remove(first)
	second, err := PackageDirectory(dir)
	if err != nil {
		t.Fatalf("package again: %v", err)
	}
	defer os.Remove(second)

	a, _ := os.ReadFile(first)
	b, _ := os.ReadFile(second)
	if string(a) != string(b) {
		t.Fatal("two runs over one tree produced different archives, so an unchanged re-push would not be a no-op commit")
	}

	names := archiveNames(t, first)
	want := []string{"main.go", "sub/a.txt"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("archive holds %v, want %v", names, want)
	}
}

func TestPushDirectoryUploadsInParts(t *testing.T) {
	var mu sync.Mutex
	var parts []int
	var completeQuery string
	var startBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/config":
			writeEnvelope(w, map[string]any{"chunk_size": 16, "max_simple_upload": 64, "max_chunk_bytes": 64})
		case strings.HasSuffix(r.URL.Path, "/source/uploads"):
			_ = json.NewDecoder(r.Body).Decode(&startBody)
			w.WriteHeader(http.StatusCreated)
			writeEnvelope(w, map[string]any{"upload_id": "up1"})
		case strings.Contains(r.URL.Path, "/parts/"):
			index, _ := strconv.Atoi(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
			parts = append(parts, index)
			writeEnvelope(w, map[string]any{"index": index})
		case strings.HasSuffix(r.URL.Path, "/complete"):
			completeQuery = r.URL.RawQuery
			writeEnvelope(w, map[string]any{"commit_sha": "deadbeef", "files": 2})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	writeFile(t, dir, "main.go", strings.Repeat("a", 40))

	ext, err := NewExt(server.URL, "k")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	result, err := ext.PushDirectory(context.Background(), "shop", dir, PushOptions{Message: "hi"})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if result.Commit != "deadbeef" {
		t.Fatalf("commit = %q", result.Commit)
	}
	// The declared total has to equal the parts actually sent, and the indices
	// have to be 1..N dense: the server rejects a gap, so a helper that
	// mis-numbered would upload nothing usable. The count is not asserted as a
	// literal — it depends on the archive's size, which tar and gzip decide.
	declared, _ := startBody["total"].(float64)
	if int(declared) != len(parts) {
		t.Fatalf("declared %v parts but sent %d", startBody["total"], len(parts))
	}
	sort.Ints(parts)
	for i, index := range parts {
		if index != i+1 {
			t.Fatalf("part indices %v are not 1..N", parts)
		}
	}
	if len(parts) < 2 {
		t.Fatalf("only %d part sent; the tree should not fit in one chunk", len(parts))
	}
	if !strings.Contains(completeQuery, "message=hi") {
		t.Fatalf("the commit message did not reach complete: %q", completeQuery)
	}
}

func TestSimpleUploadFallsBackToChunkedOn413(t *testing.T) {
	var sawChunked bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/config":
			writeEnvelope(w, map[string]any{"chunk_size": 8})
		case strings.HasSuffix(r.URL.Path, "/source"):
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":"too large; use the chunked endpoints"}`))
		default:
			sawChunked = true
			if strings.HasSuffix(r.URL.Path, "/source/uploads") {
				w.WriteHeader(http.StatusCreated)
				writeEnvelope(w, map[string]any{"upload_id": "up1"})
				return
			}
			if strings.HasSuffix(r.URL.Path, "/complete") {
				writeEnvelope(w, map[string]any{"commit_sha": "c"})
				return
			}
			writeEnvelope(w, map[string]any{})
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	writeFile(t, dir, "main.go", "package main\n")
	archive, err := PackageDirectory(dir)
	if err != nil {
		t.Fatalf("package: %v", err)
	}
	defer os.Remove(archive)

	ext, _ := NewExt(server.URL, "k")
	result, err := ext.UploadSimple(context.Background(), "shop", archive, PushOptions{})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !sawChunked {
		t.Fatal("a 413 did not fall back to the chunked path")
	}
	if result.Commit != "c" {
		t.Fatalf("commit = %q", result.Commit)
	}
}

func TestStreamLogsYieldsLinesAsTheyArrive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/logs") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("follow") != "true" {
			t.Errorf("follow did not reach the server: %q", r.URL.RawQuery)
		}
		flusher := w.(http.Flusher)
		for _, line := range []string{"one\n", "two\n", "three"} {
			_, _ = io.WriteString(w, line)
			flusher.Flush()
		}
	}))
	defer server.Close()

	ext, _ := NewExt(server.URL, "k")
	var lines []string
	err := ext.StreamLogs(context.Background(), "shop", LogOptions{Follow: true}, func(line string) error {
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	// The final line has no newline and must still be delivered.
	if strings.Join(lines, "") != "one\ntwo\nthree" {
		t.Fatalf("lines = %q", lines)
	}
}

func TestStreamLogsStopsOnYieldError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "a\nb\nc\n")
	}))
	defer server.Close()

	ext, _ := NewExt(server.URL, "k")
	stop := fmt.Errorf("caller is done")
	count := 0
	err := ext.StreamLogs(context.Background(), "shop", LogOptions{}, func(string) error {
		count++
		if count == 2 {
			return stop
		}
		return nil
	})
	if err != stop {
		t.Fatalf("err = %v, want the caller's own error", err)
	}
	if count != 2 {
		t.Fatalf("read %d lines past the stop, want 2", count)
	}
}

func TestResolveBuildIDIsStrict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, []map[string]any{{"id": "abc123"}, {"id": "abd999"}})
	}))
	defer server.Close()

	ext, _ := NewExt(server.URL, "k")

	got, err := ext.ResolveBuildID(context.Background(), "shop", "abc1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "abc123" {
		t.Fatalf("resolved %q", got)
	}

	// Two builds share the prefix; guessing is how the wrong build gets cancelled.
	if _, err := ext.ResolveBuildID(context.Background(), "shop", "ab"); err == nil {
		t.Fatal("an ambiguous prefix was resolved instead of refused")
	}
	if _, err := ext.ResolveBuildID(context.Background(), "shop", "zzz"); err == nil {
		t.Fatal("an unknown prefix was resolved instead of refused")
	}
}

func TestTheKeyIsRefusedWhenEmptyAndNeverPutInAURL(t *testing.T) {
	if _, err := NewExt("https://applab.example.com", ""); err == nil {
		t.Fatal("an empty key was accepted")
	}

	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if strings.Contains(r.URL.RawQuery, "k-secret") || strings.Contains(r.URL.Path, "k-secret") {
			t.Error("the key reached the URL, where access logs would record it")
		}
		writeEnvelope(w, map[string]any{"chunk_size": 8})
	}))
	defer server.Close()

	ext, _ := NewExt(server.URL, "k-secret")
	if _, err := ext.Config(context.Background()); err != nil {
		t.Fatalf("config: %v", err)
	}
	if auth != "Bearer k-secret" {
		t.Fatalf("Authorization = %q", auth)
	}
}

// helpers

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func archiveNames(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	var names []string
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		names = append(names, header.Name)
	}
	sort.Strings(names)
	return names
}

func writeEnvelope(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}
