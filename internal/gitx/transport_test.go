package gitx

import (
	"archive/tar"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shaowenchen/applab/internal/source"
)

// newTestTransport builds a Transport over a temporary repository root with one
// app's repository created.
func newTestTransport(t *testing.T, appIDs ...string) (*Transport, *source.Store) {
	t.Helper()

	dataDir := t.TempDir()
	store, err := source.New(source.Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	ctx := context.Background()
	for _, id := range appIDs {
		if err := store.Create(ctx, id); err != nil {
			t.Fatalf("create repository %s: %v", id, err)
		}
	}

	tr, err := New(filepath.Join(dataDir, "repos"), store.GitPath())
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	return tr, store
}

// TestCloneOverHTTP is the end-to-end proof that the transport works: a real git
// client, over a real HTTP server, gets the real content.
func TestCloneOverHTTP(t *testing.T) {
	tr, store := newTestTransport(t, "shop")
	ctx := context.Background()

	// Put a commit in the repository through the ingest path.
	archive := buildTar(t, map[string]string{
		"Dockerfile": "FROM scratch\n",
		"main.go":    "package main\n",
	})
	if _, err := store.Ingest(ctx, "shop", bytes.NewReader(archive), "initial", "", source.DefaultIngestLimits); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	srv := httptest.NewServer(tr)
	defer srv.Close()

	cloneDir := filepath.Join(t.TempDir(), "clone")
	runGit(t, "", "clone", srv.URL+"/shop.git", cloneDir)

	got, err := os.ReadFile(filepath.Join(cloneDir, "Dockerfile"))
	if err != nil {
		t.Fatalf("read cloned file: %v", err)
	}
	if string(got) != "FROM scratch\n" {
		t.Errorf("cloned Dockerfile = %q, want %q", got, "FROM scratch\n")
	}

	// The clone must be a functioning repository, not just files.
	log := runGit(t, cloneDir, "log", "--oneline")
	if !strings.Contains(log, "initial") {
		t.Errorf("cloned repository has no commit history:\n%s", log)
	}
}

// TestPushOverHTTP asserts a push works, since REMOTE_USER is what enables
// receive-pack and its absence would silently make the repository read-only.
func TestPushOverHTTP(t *testing.T) {
	tr, store := newTestTransport(t, "shop")
	ctx := context.Background()

	archive := buildTar(t, map[string]string{"a.txt": "one\n"})
	if _, err := store.Ingest(ctx, "shop", bytes.NewReader(archive), "initial", "", source.DefaultIngestLimits); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	srv := httptest.NewServer(tr)
	defer srv.Close()

	workDir := filepath.Join(t.TempDir(), "work")
	runGit(t, "", "clone", srv.URL+"/shop.git", workDir)
	runGit(t, workDir, "config", "user.email", "test@example.com")
	runGit(t, workDir, "config", "user.name", "Test")

	if err := os.WriteFile(filepath.Join(workDir, "b.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "pushed directly")
	runGit(t, workDir, "push", "origin", "main")

	// The push must be visible in the repository AppLab reads.
	head, err := store.HeadCommit(ctx, "shop")
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	subject := strings.TrimSpace(runGit(t, filepath.Join(t.TempDir()), "--git-dir", mustRepoPath(t, store, "shop"), "log", "-1", "--format=%s", head))
	if subject != "pushed directly" {
		t.Errorf("head commit subject = %q, want %q", subject, "pushed directly")
	}
}

// TestPathTraversalIsRefused is the security test for the transport boundary. A
// request that tries to address a repository outside the root must not be served.
func TestPathTraversalIsRefused(t *testing.T) {
	tr, _ := newTestTransport(t, "shop")

	cases := []struct {
		name string
		path string
	}{
		{"parent directory", "/../etc/passwd.git"},
		{"encoded parent", "/shop.git/../../../etc/passwd"},
		{"double slash traversal", "//..//shop.git"},
		{"absolute escape", "/shop.git/../../../../tmp/evil.git"},
		{"nested parent", "/a/../../shop.git"},
		{"no dot git suffix", "/etc/passwd"},
		{"uppercase repo name", "/Shop.git"},
		{"repo name with underscore", "/my_shop.git"},
		{"repo name traversal", "/..git"},
		{"empty", "/"},
		{"bare git dir", "/.git/config"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			tr.ServeHTTP(rec, req)

			if rec.Code == http.StatusOK {
				t.Errorf("path %q was served successfully; it must be refused", tc.path)
			}
		})
	}
}

// TestValidRepoName pins the rule independently of the traversal test, so a
// change to the naming rule is caught here rather than as a security surprise.
func TestValidRepoName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"shop", true},
		{"my-shop", true},
		{"shop2", true},
		{"a", true},
		{"", false},
		{"Shop", false},
		{"my_shop", false},
		{"-shop", false},
		{"shop-", false},
		{"my.shop", false},
		{"shop/evil", false},
		{"shop\x00", false},
		{strings.Repeat("a", 41), false},
		{strings.Repeat("a", 40), true},
	}

	for _, tc := range cases {
		if got := validRepoName(tc.name); got != tc.want {
			t.Errorf("validRepoName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestInfoRefsAdvertisesPush asserts the capabilities a client sees include
// push, which is what makes the repository writable over HTTP.
func TestInfoRefsAdvertisesPush(t *testing.T) {
	tr, store := newTestTransport(t, "shop")
	ctx := context.Background()

	archive := buildTar(t, map[string]string{"a.txt": "one\n"})
	if _, err := store.Ingest(ctx, "shop", bytes.NewReader(archive), "initial", "", source.DefaultIngestLimits); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	srv := httptest.NewServer(tr)
	defer srv.Close()

	// ls-remote exercises the advertisement without cloning anything.
	out := runGit(t, "", "ls-remote", srv.URL+"/shop.git")
	if !strings.Contains(out, "refs/heads/main") {
		t.Errorf("ls-remote did not report the main branch:\n%s", out)
	}
}

// TestMissingRepositoryIsNotFound asserts a request for a repository that does
// not exist fails cleanly rather than with a server error.
func TestMissingRepositoryIsNotFound(t *testing.T) {
	tr, _ := newTestTransport(t, "shop")

	srv := httptest.NewServer(tr)
	defer srv.Close()

	out := runGitExpectFailure(t, "ls-remote", srv.URL+"/nonexistent.git")
	lowered := strings.ToLower(out)
	if !strings.Contains(lowered, "not found") && !strings.Contains(lowered, "404") && !strings.Contains(lowered, "repository") {
		t.Errorf("a request for a missing repository produced an unhelpful error:\n%s", out)
	}
}

// buildTar assembles a tar archive from a name-to-content map. Entries are
// written in sorted order so the archive is reproducible.
func buildTar(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	// Directories first, so a nested file's parent exists in the archive.
	for _, name := range names {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{
			Name:    name,
			Mode:    0o644,
			Size:    int64(len(body)),
			ModTime: time.Unix(1700000000, 0),
		}); err != nil {
			t.Fatalf("write header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("write body %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

func mustRepoPath(t *testing.T, s *source.Store, appID string) string {
	t.Helper()

	path, err := s.RepoPath(appID)
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	return path
}

// runGit runs git and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = gitTestEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// runGitExpectFailure runs git and returns its combined output regardless of
// exit status.
func runGitExpectFailure(t *testing.T, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Env = gitTestEnv()
	out, _ := cmd.CombinedOutput()
	return string(out)
}

func gitTestEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)
}
