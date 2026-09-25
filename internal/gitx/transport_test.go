package gitx

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"github.com/shaowenchen/applab/internal/model"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shaowenchen/applab/internal/objectstore"
	"github.com/shaowenchen/applab/internal/source"
)

// newTestTransport builds a Transport over a temporary repository root with one
// app's repository created.
func newTestTransport(t *testing.T, appIDs ...string) (*Transport, *source.Store) {
	t.Helper()

	dataDir := t.TempDir()
	objs, err := objectstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	store, err := source.New(source.Options{Objects: objs, DataDir: dataDir})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	ctx := context.Background()
	for _, id := range appIDs {
		if err := store.Create(ctx, id, model.DefaultBranch); err != nil {
			t.Fatalf("create repository %s: %v", id, err)
		}
	}

	// The sessions hook is what makes a repository available to git for the
	// length of a request, since the repositories live in the object store.
	//
	// The branch resolver is attached the same way main.go attaches it, because
	// the URL these tests use — "/shop.git" — names no branch and the transport
	// cannot know which one that means without being told. Answering with the
	// default is what an app that has never been pushed to a branch does.
	tr, err := New(dataDir, store.GitPath())
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	return tr.
		WithSessions(store).
		WithActiveBranch(func(context.Context, string) string { return model.DefaultBranch }).
		// Attached the way main.go attaches it. Without it a repository served
		// here keeps git's default configuration, including the automatic
		// maintenance that races the upload — which is exactly the flakiness the
		// production wiring exists to prevent, so a test that omitted it would
		// not be testing the configuration that ships.
		WithPrepare(store.Prepare), store
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
	if _, err := store.Ingest(ctx, "shop", model.DefaultBranch, bytes.NewReader(archive), "initial", "", source.DefaultIngestLimits); err != nil {
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
	if _, err := store.Ingest(ctx, "shop", model.DefaultBranch, bytes.NewReader(archive), "initial", "", source.DefaultIngestLimits); err != nil {
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
	head, err := store.HeadCommit(ctx, "shop", model.DefaultBranch)
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	subject := strings.TrimSpace(runGit(t, filepath.Join(t.TempDir()), "--git-dir", mustRepoPath(t, store, "shop"), "log", "-1", "--format=%s", head))
	if subject != "pushed directly" {
		t.Errorf("head commit subject = %q, want %q", subject, "pushed directly")
	}
}

// TestAPushRunsTheAfterPushHook asserts the hook fires once a push has been
// stored, with the app and branch that were pushed — and that it does not fire
// for a fetch.
//
// This is what makes a push build and deploy. The transport knows *when* a push
// has landed and nothing about what should follow it, so the hook is where that
// decision is handed off; a hook that fired on a clone, or before the objects
// were stored, would start a build against a commit that is not there.
func TestAPushRunsTheAfterPushHook(t *testing.T) {
	tr, store := newTestTransport(t, "shop")
	ctx := context.Background()

	archive := buildTar(t, map[string]string{"a.txt": "one\n"})
	if _, err := store.Ingest(ctx, "shop", model.DefaultBranch, bytes.NewReader(archive), "initial", "", source.DefaultIngestLimits); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	type pushEvent struct{ app, branch string }
	events := make(chan pushEvent, 4)
	tr.WithAfterPush(func(_ context.Context, appID, branch string) {
		events <- pushEvent{appID, branch}
	})

	srv := httptest.NewServer(tr)
	defer srv.Close()

	// A clone is not a push, and nothing should follow it.
	runGit(t, "", "clone", srv.URL+"/shop.git", filepath.Join(t.TempDir(), "clone"))
	select {
	case e := <-events:
		t.Fatalf("cloning ran the after-push hook with %+v; a fetch writes nothing", e)
	case <-time.After(200 * time.Millisecond):
	}

	workDir := filepath.Join(t.TempDir(), "work")
	runGit(t, "", "clone", srv.URL+"/shop.git", workDir)
	runGit(t, workDir, "config", "user.email", "test@example.com")
	runGit(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workDir, "b.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "push it")
	runGit(t, workDir, "push", "origin", "main")

	select {
	case e := <-events:
		if e.app != "shop" {
			t.Errorf("the hook ran for app %q, want shop", e.app)
		}
		if e.branch != model.DefaultBranch {
			t.Errorf("the hook ran for branch %q, want %q", e.branch, model.DefaultBranch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pushing did not run the after-push hook")
	}

	// The hook has to run with the objects already stored, or a build it starts
	// would clone a commit that is not there yet. The push above is durable by
	// the time the hook fires, so the tip is the committed subject.
	head, err := store.HeadCommit(ctx, "shop", model.DefaultBranch)
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	subject := strings.TrimSpace(runGit(t, filepath.Join(t.TempDir()), "--git-dir", mustRepoPath(t, store, "shop"), "log", "-1", "--format=%s", head))
	if subject != "push it" {
		t.Errorf("head commit subject = %q, want %q: the hook ran before the push was stored", subject, "push it")
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

		// The branch is a second caller-controlled component in the same path
		// segment, so it gets the same treatment: every shape that would leave a
		// repository, name a flag, or mean two things at once.
		{"branch that is empty", "/shop@.git"},
		{"branch with a traversal", "/shop@..git"},
		{"branch with a nested traversal", "/shop@../.."},
		{"branch with an inner traversal", "/shop@a..b.git"},
		{"branch that is a dash", "/shop@-x.git"},
		{"branch that is a revision operator", "/shop@{1}.git"},
		{"two at signs", "/shop@a@b.git"},
		// Percent-encoded, because a raw space is not a legal request target and
		// httptest refuses to build one. The Go URL parser decodes it before the
		// transport sees it, so what reaches resolvePath is the same string a
		// hand-typed URL would produce.
		{"branch with a space", "/shop@my%20branch.git"},
		{"branch with a slash", "/shop@a//b.git"},
		{"branch ending in lock", "/shop@a.lock.git"},
		{"uppercase branch", "/shop@Dev.git"},
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
	if _, err := store.Ingest(ctx, "shop", model.DefaultBranch, bytes.NewReader(archive), "initial", "", source.DefaultIngestLimits); err != nil {
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

// mustRepoPath materialises an app's repository so a test can assert on what a
// push actually stored.
//
// It opens the default branch, which is the app's active one unless a test says
// otherwise and the branch every push in this file targets.
func mustRepoPath(t *testing.T, s *source.Store, appID string) string {
	t.Helper()

	path, done, err := s.Open(context.Background(), appID, model.DefaultBranch)
	if err != nil {
		t.Fatalf("Open(%q): %v", appID, err)
	}
	// The copy is read-only for these assertions, so nothing it changed needs
	// uploading — but Done still has to run to release the app's lock.
	t.Cleanup(func() { _ = done() })
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

// ---------------------------------------------------------------------------
// The repository must not be maintained while it is being uploaded
// ---------------------------------------------------------------------------

// TestServingPinsTheRepositoryConfig asserts the settings a repository needs
// before git is allowed near it.
//
// The one that matters is gc.auto. `git receive-pack` spawns `git maintenance
// run --auto` in the background after a push and does not wait for it, while
// AppLab walks the working copy to upload it — so a repack that lands mid-walk
// moves the objects out from under it and the upload fails. The push has already
// been answered by then, so the failure is a push the client was told succeeded
// and which was then lost.
//
// The settings are read back with git, from the materialised repository, rather
// than from the code — the setting working is the whole assertion.
//
// Checked inside the hook, because that is where the repository is legitimately
// open: the session holds a per-app lock for the length of the request, so
// opening the same app again from the test would wait for a request that is
// itself waiting on this goroutine.
func TestServingPinsTheRepositoryConfig(t *testing.T) {
	tr, store := newTestTransport(t, "shop")

	// Collected in the hook and asserted in the test goroutine: the hook runs on
	// the server's goroutine, where t.Fatalf would kill the handler rather than
	// the test — which is how the first version of this failed with a bare EOF.
	got := map[string]string{}

	tr = tr.WithPrepare(func(ctx context.Context, repoPath string) error {
		err := store.Prepare(ctx, repoPath)
		for _, setting := range []string{"gc.auto", "maintenance.auto", "core.repositoryformatversion", "core.filemode"} {
			out, _ := exec.Command("git", "--git-dir", repoPath, "config", "--get", setting).Output()
			got[setting] = strings.TrimSpace(string(out))
		}
		return err
	})

	srv := httptest.NewServer(tr)
	defer srv.Close()

	// One request is what materialises a repository and runs the hook.
	resp, err := http.Get(srv.URL + "/shop.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("GET info/refs: %v", err)
	}
	// Read to the end before closing: a body left unread leaves the handler mid-
	// write, and with it the app's lock held — which is what made this test hang
	// rather than fail when it was first written.
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	for setting, want := range map[string]string{
		"gc.auto":                      "0",
		"maintenance.auto":             "false",
		"core.repositoryformatversion": "0",
		"core.filemode":                "false",
	} {
		if got[setting] != want {
			t.Errorf("%s = %q, want %q; git may maintain this repository while it is being uploaded",
				setting, got[setting], want)
		}
	}
}

// TestPushFailureIsReportedToThePusher asserts a push whose upload fails is not
// reported as success.
//
// The upload is what makes a push durable, and it happens after git has
// answered. Before this the failure was logged and dropped: `git push` printed
// "ok", the objects never reached the bucket, and the only trace was a line in
// AppLab's own log. The bug that prompted it — a repack racing the walk — showed
// up as a flaky test and, in a real deployment, as a push that vanished.
//
// The failure is injected through the sessions hook, which is the seam that
// makes this observable at all.
func TestPushFailureIsReportedToThePusher(t *testing.T) {
	tr, store := newTestTransport(t, "shop")
	ctx := context.Background()

	archive := buildTar(t, map[string]string{"a.txt": "one\n"})
	if _, err := store.Ingest(ctx, "shop", model.DefaultBranch, bytes.NewReader(archive), "initial", "", source.DefaultIngestLimits); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// A source store whose upload always fails, wrapping the real one so
	// everything else behaves.
	failing := &failingUploads{Store: store}
	tr = tr.WithSessions(failing)

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

	cmd := exec.Command("git", "push", "origin", "main")
	cmd.Dir = workDir
	cmd.Env = gitTestEnv()
	raw, err := cmd.CombinedOutput()
	out := string(raw)
	if err == nil {
		t.Fatalf("the push reported success while the upload failed; git said:\n%s", out)
	}
	// The pusher has to be told it was the *storage*, not the push: a rejected
	// push and a push that could not be saved are different problems, and the
	// second one is safe to simply repeat.
	if !strings.Contains(out, "could not be stored") {
		t.Errorf("the payload was not explained to the pusher:\n%s", out)
	}
}

// failingUploads wraps a source store so every session's upload fails.
//
// It is the only seam that makes this testable: the real failure needs a
// concurrent repack, which cannot be scheduled from a test.
type failingUploads struct {
	*source.Store
}

func (f *failingUploads) Open(ctx context.Context, appID, branch string) (string, func() error, error) {
	path, done, err := f.Store.Open(ctx, appID, branch)
	if err != nil {
		return path, done, err
	}
	return path, func() error {
		// The real Done still runs, so the working copy is cleaned up and the
		// lock released; only the outcome is replaced.
		_ = done()
		return errors.New("injected: the repository could not be stored")
	}, nil
}
