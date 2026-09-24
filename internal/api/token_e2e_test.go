package api_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/auth"
	"github.com/shaowenchen/applab/internal/config"
	"github.com/shaowenchen/applab/internal/source"
	"github.com/shaowenchen/applab/internal/sourcetoken"
	"github.com/shaowenchen/applab/internal/store"
)

// TestSourceArchiveWithToken walks the exact path a build Job's init container
// takes: get a token, present it, receive the source.
func TestSourceArchiveWithToken(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.OpenLocal(context.Background(), filepath.Join(dataDir, "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	src, err := source.New(source.Options{Objects: newObjects(t), DataDir: dataDir})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	cfg := config.Default()
	cfg.Keys = []string{"test-key"}
	cfg.DataDir = dataDir

	issuer := sourcetoken.NewIssuer(time.Minute)
	srv := api.New(cfg, st, auth.New(cfg.Keys)).WithSource(src).WithSourceTokens(issuer)
	h := srv.Handler()

	// Create the app, upload source, and learn the commit.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps", strings.NewReader(`{"id":"shop"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/apps/shop/source",
		bytes.NewReader(gzipBytes(t, tarFiles(t, map[string]string{"Dockerfile": "FROM scratch\n"}))))
	req.Header.Set("Authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d (%s)", rec.Code, rec.Body.String())
	}

	var upload struct {
		CommitSHA string `json:"commit_sha"`
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	json.Unmarshal(rec.Body.Bytes(), &env)
	json.Unmarshal(env.Data, &upload)

	// A token issued for this app and commit, as a build start would.
	tok, err := issuer.Issue("shop", upload.CommitSHA)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	req = httptest.NewRequest(http.MethodGet,
		"/api/v1/apps/shop/source/archive/"+upload.CommitSHA, nil)
	req.Header.Set("Authorization", "Bearer "+tok.Value)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("archive fetch: %d (%s)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Error("no Content-Length; a truncated download would not be detectable")
	}
	if rec.Body.Len() == 0 {
		t.Error("the archive is empty")
	}

	// The real proof: unpack it the way the build job's init container does and
	// check the files are the ones that were uploaded.
	gz, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("the archive is not gzip: %v", err)
	}
	tr := tar.NewReader(gz)

	found := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		body, _ := io.ReadAll(tr)
		found[hdr.Name] = string(body)
	}

	if found["Dockerfile"] != "FROM scratch\n" {
		t.Errorf("the archive's Dockerfile = %q, want %q; a build would not find what was uploaded",
			found["Dockerfile"], "FROM scratch\n")
	}

	// A token is scoped to one app and one commit, so using it against either a
	// different app or a different commit must fail — otherwise a build could
	// read source it was never started for.
	for _, tc := range []struct{ name, path string }{
		{"another app", "/api/v1/apps/other/source/archive/" + upload.CommitSHA},
		{"another commit", "/api/v1/apps/shop/source/archive/" + strings.Repeat("b", 40)},
	} {
		fresh, err := issuer.Issue("shop", upload.CommitSHA)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}

		req = httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+fresh.Value)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code == http.StatusOK {
			t.Errorf("a token issued for shop@%s also granted %s", upload.CommitSHA[:8], tc.name)
		}
	}

	// Replays must fail.
	req = httptest.NewRequest(http.MethodGet,
		"/api/v1/apps/shop/source/archive/"+upload.CommitSHA, nil)
	req.Header.Set("Authorization", "Bearer "+tok.Value)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a used token was accepted again (status %d)", rec.Code)
	}
}
