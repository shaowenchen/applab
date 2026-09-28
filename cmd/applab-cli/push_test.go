package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/shaowenchen/applab/internal/client"
)

// TestPushFallsBackToChunkedWhenTheArchiveIsTooLarge is the whole point of the
// two paths existing.
//
// A deployment publishes the size above which it wants an upload sent in parts.
// Before the fallback was wired, a tree over that size was simply refused: the
// limit was advertised, so the client knew, and it had nothing to do with the
// knowledge. This drives the real command against a server that refuses the
// single request and accepts the chunked one, and asserts the archive arrived
// intact — because a fallback that sends parts of a stream it had already
// partly handed to the failed request would upload a truncated tree and commit
// it.
func TestPushFallsBackToChunkedWhenTheArchiveIsTooLarge(t *testing.T) {
	var (
		mu            sync.Mutex
		simpleTries   int
		declaredTotal int
		parts         = map[int][]byte{}
		completed     bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer := func(v any) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"data": v})
		}

		switch {
		case r.URL.Path == "/api/v1/config":
			answer(map[string]any{
				"max_simple_upload": 4096,
				"chunk_size":        1000,
				"capabilities":      map[string]bool{"build": false},
			})

		case r.URL.Path == "/api/v1/apps/shop" && r.Method == http.MethodGet:
			answer(map[string]any{"id": "shop", "port": 80, "replicas": 1})

		case r.URL.Path == "/api/v1/apps/shop/source":
			mu.Lock()
			simpleTries++
			mu.Unlock()
			// What the real server sends when the body is over the advertised
			// limit — a 413 whose message names the way through.
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			json.NewEncoder(w).Encode(map[string]any{
				"error": "the archive is larger than this deployment's single-request limit of 4096 bytes; " +
					"upload it in parts through /api/v1/apps/{app}/source/uploads, or raise max_simple_upload",
			})

		case r.URL.Path == "/api/v1/apps/shop/source/uploads":
			var body struct {
				Total     int   `json:"total"`
				ChunkSize int64 `json:"chunk_size"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			declaredTotal = body.Total
			mu.Unlock()
			answer(map[string]any{"upload_id": "up-1"})

		case strings.Contains(r.URL.Path, "/source/uploads/up-1/parts/"):
			index, _ := strconv.Atoi(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			parts[index] = body
			mu.Unlock()
			answer(map[string]any{"index": index})

		case r.URL.Path == "/api/v1/apps/shop/source/uploads/up-1/complete":
			mu.Lock()
			completed = true
			mu.Unlock()
			answer(map[string]any{"commit_sha": strings.Repeat("a", 40), "files": 2, "bytes": 12345})

		default:
			http.Error(w, `{"error":"unexpected path `+r.URL.Path+`"}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// A tree with one file that repeats, so the compressed archive is a known
	// size and the part count is deterministic.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.txt"), []byte(strings.Repeat("applab ", 5000)), 0o644); err != nil {
		t.Fatalf("write app.txt: %v", err)
	}

	c, err := client.New(client.Options{BaseURL: srv.URL, Key: "test-key"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	cfg, err := c.Config(context.Background())
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	opts := &pushOptions{app: "shop", dir: dir, noDeploy: true}
	if err := uploadDirectory(context.Background(), c, opts, cfg); err != nil {
		t.Fatalf("uploadDirectory: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if simpleTries != 1 {
		t.Errorf("the single-request path was tried %d times, want 1", simpleTries)
	}
	if !completed {
		t.Fatal("the chunked upload was never completed")
	}
	if declaredTotal != len(parts) {
		t.Fatalf("declared %d parts but sent %d", declaredTotal, len(parts))
	}

	// The parts, in order, are a gzip stream that unpacks to a tar holding the
	// tree. This is the assertion that catches a truncated retry: reassembling
	// a half-consumed pipe yields bytes that are not a valid gzip stream at all.
	var assembled []byte
	for i := 1; i <= declaredTotal; i++ {
		part, ok := parts[i]
		if !ok {
			t.Fatalf("part %d was never sent", i)
		}
		assembled = append(assembled, part...)
	}

	gz, err := gzip.NewReader(bytes.NewReader(assembled))
	if err != nil {
		t.Fatalf("the assembled archive is not a gzip stream: %v", err)
	}
	unpacked, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read the assembled archive: %v", err)
	}
	if !bytes.Contains(unpacked, []byte("applab ")) {
		t.Error("the assembled archive does not contain the file that was pushed")
	}
	if !bytes.Contains(unpacked, []byte("Dockerfile")) {
		t.Error("the assembled archive does not contain the Dockerfile")
	}
}

// TestPushReportsANonSizeFailureWithoutRetrying asserts the fallback is for one
// specific refusal and not a general retry. A 401 is not going to succeed in
// parts, and re-uploading the tree to find that out costs the transfer twice.
func TestPushReportsANonSizeFailureWithoutRetrying(t *testing.T) {
	var (
		mu         sync.Mutex
		chunkedHit bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer := func(v any) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"data": v})
		}

		switch {
		case r.URL.Path == "/api/v1/config":
			answer(map[string]any{"max_simple_upload": 4096, "chunk_size": 1000, "capabilities": map[string]bool{"build": false}})
		case r.URL.Path == "/api/v1/apps/shop":
			answer(map[string]any{"id": "shop", "port": 80, "replicas": 1})
		case r.URL.Path == "/api/v1/apps/shop/source":
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{"error": "invalid key"})
		case strings.Contains(r.URL.Path, "/source/uploads"):
			mu.Lock()
			chunkedHit = true
			mu.Unlock()
			answer(map[string]any{"upload_id": "up-1"})
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	c, err := client.New(client.Options{BaseURL: srv.URL, Key: "bad-key"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	cfg, err := c.Config(context.Background())
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	opts := &pushOptions{app: "shop", dir: dir, noDeploy: true}
	err = uploadDirectory(context.Background(), c, opts, cfg)
	if err == nil {
		t.Fatal("a 401 push reported success")
	}
	if !strings.Contains(err.Error(), "invalid key") {
		t.Errorf("error = %q, want the server's own message", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if chunkedHit {
		t.Error("a non-size failure was retried through the chunked path")
	}
}
