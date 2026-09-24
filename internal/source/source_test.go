package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaowenchen/applab/internal/objectstore"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	s, _ := newTestStoreAndObjects(t)
	return s
}

// newTestStoreAndObjects returns the store and the object store behind it, so a
// test can look at what was actually written — which is where the interesting
// assertions are now that the bucket is the only copy.
func newTestStoreAndObjects(t *testing.T) (*Store, objectstore.Store) {
	t.Helper()

	objs, err := objectstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	s, err := New(Options{Objects: objs, DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, objs
}

// materializeRepo copies an app's repository out of the object store so a test
// can run git against it directly.
//
// It is the test's own copy, taken independently of the package's own download
// path, so an assertion about what is in the repository is about the bucket
// rather than about a copy the package happened to leave lying around.
func materializeRepo(t *testing.T, s *Store, appID string) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), appID+".git")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}

	repo := &workingRepo{
		store:  s.objects,
		prefix: s.repoPrefix(appID),
		dir:    dir,
		before: map[string]fileState{},
	}
	if err := repo.download(context.Background()); err != nil {
		t.Fatalf("download the repository for %s: %v", appID, err)
	}
	return dir
}

// tarEntry is one entry to place in a test archive.
type tarEntry struct {
	name     string
	body     string
	mode     int64
	typeflag byte
	linkname string
}

// buildTar assembles an archive from entries.
func buildTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		typeflag := e.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{
			Name:     e.name,
			Mode:     mode,
			Size:     int64(len(e.body)),
			Typeflag: typeflag,
			Linkname: e.linkname,
			ModTime:  time.Unix(1700000000, 0),
		}
		if typeflag == tar.TypeDir {
			header.Size = 0
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("write header %s: %v", e.name, err)
		}
		if typeflag == tar.TypeReg && len(e.body) > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write body %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

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

// TestIngestCreatesCommitAndIsClonable is the core promise of the source layer:
// an uploaded tarball becomes a real repository that ordinary git can read.
func TestIngestCreatesCommitAndIsClonable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Create(ctx, "shop"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	archive := buildTar(t, []tarEntry{
		{name: "Dockerfile", body: "FROM scratch\n"},
		{name: "main.go", body: "package main\n\nfunc main() {}\n"},
		{name: "README.md", body: "# shop\n"},
	})

	result, err := s.Ingest(ctx, "shop", bytes.NewReader(archive), "initial upload", "", DefaultIngestLimits)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if result.SHA == "" {
		t.Fatal("Ingest produced no commit")
	}
	// The count is the upload plus the files AppLab seeds into every tree — see
	// seed.go. Asserted against len(seeded) rather than a rewritten literal so
	// adding a seeded file does not require editing every count in this file.
	if result.Files != 3+len(seeded) {
		t.Errorf("Files = %d, want %d", result.Files, 3+len(seeded))
	}
	if result.Subject != "initial upload" {
		t.Errorf("Subject = %q, want %q", result.Subject, "initial upload")
	}

	// The real proof: clone it with the git binary and read the files back.
	cloneDir := filepath.Join(t.TempDir(), "clone")
	repoPath := materializeRepo(t, s, "shop")
	runGit(t, "", "clone", repoPath, cloneDir)

	for name, want := range map[string]string{
		"Dockerfile": "FROM scratch\n",
		"main.go":    "package main\n\nfunc main() {}\n",
		"README.md":  "# shop\n",
	} {
		got, err := os.ReadFile(filepath.Join(cloneDir, name))
		if err != nil {
			t.Errorf("clone is missing %s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	// The commit must be on main, since that is what a clone checks out.
	branch := strings.TrimSpace(runGit(t, cloneDir, "rev-parse", "--abbrev-ref", "HEAD"))
	if branch != "main" {
		t.Errorf("cloned branch = %q, want main", branch)
	}
}

// TestIngestStripsSingleWrapperDirectory asserts the common `tar czf - myproject`
// shape lands with its contents at the root, since a Dockerfile one level too
// deep is the difference between a working build and a confusing failure.
func TestIngestStripsSingleWrapperDirectory(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	archive := buildTar(t, []tarEntry{
		{name: "myproject/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "myproject/Dockerfile", body: "FROM scratch\n"},
		{name: "myproject/src/main.go", body: "package main\n"},
	})

	result, err := s.Ingest(ctx, "shop", bytes.NewReader(archive), "", "", DefaultIngestLimits)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if result.Stripped != "myproject" {
		t.Errorf("Stripped = %q, want %q", result.Stripped, "myproject")
	}

	tree := runGit(t, mustRepoPath(t, s, "shop"), "ls-tree", "-r", "--name-only", "refs/heads/main")
	for _, want := range []string{"Dockerfile", "src/main.go"} {
		if !strings.Contains(tree, want) {
			t.Errorf("committed tree does not contain %q at the root; got:\n%s", want, tree)
		}
	}
}

// TestIngestKeepsWrapperWhenItIsNotAlone asserts the strip only fires for a
// genuine wrapper. With a sibling at the top level, removing the directory would
// move files the caller did not ask to move.
func TestIngestKeepsWrapperWhenItIsNotAlone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	archive := buildTar(t, []tarEntry{
		{name: "myproject/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "myproject/main.go", body: "package main\n"},
		{name: "LICENSE", body: "MIT\n"},
	})

	result, err := s.Ingest(ctx, "shop", bytes.NewReader(archive), "", "", DefaultIngestLimits)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if result.Stripped != "" {
		t.Errorf("Stripped = %q, want nothing stripped when there is a sibling at the root", result.Stripped)
	}

	tree := runGit(t, mustRepoPath(t, s, "shop"), "ls-tree", "-r", "--name-only", "refs/heads/main")
	if !strings.Contains(tree, "myproject/main.go") {
		t.Errorf("the wrapper directory was removed; got:\n%s", tree)
	}
	if !strings.Contains(tree, "LICENSE") {
		t.Errorf("LICENSE is missing; got:\n%s", tree)
	}
}

// TestIngestRejectsTraversal is the security test that matters most: an archive
// must not be able to write outside the directory AppLab gave it.
func TestIngestRejectsTraversal(t *testing.T) {
	cases := []struct {
		name  string
		entry string
	}{
		{"parent traversal", "../escape.txt"},
		{"deep traversal", "../../../../tmp/escape.txt"},
		{"absolute path", "/tmp/applab-escape.txt"},
		{"traversal after a directory", "sub/../../escape.txt"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()

			archive := buildTar(t, []tarEntry{
				{name: "ok.txt", body: "fine\n"},
				{name: tc.entry, body: "escaped\n"},
			})

			_, err := s.Ingest(ctx, "shop", bytes.NewReader(archive), "", "", DefaultIngestLimits)
			if err == nil {
				t.Fatalf("an archive containing %q was accepted; it must be refused", tc.entry)
			}
			if !strings.Contains(err.Error(), "escape") && !strings.Contains(err.Error(), "absolute") {
				t.Errorf("the error should name the traversal, got: %v", err)
			}

			// Nothing may have been created outside the data directory. The
			// scratch tree is removed with the failed upload, so the outer
			// directory is what to check.
			if _, statErr := os.Stat(filepath.Join(s.dataDir, "escape.txt")); statErr == nil {
				t.Error("a file escaped into the data directory root")
			}
		})
	}
}

// TestIngestRejectsSymlinkTraversal covers the attack a lexical path check
// cannot catch: a symlink to a directory outside the destination, followed by a
// file written "through" it.
func TestIngestRejectsSymlinkTraversal(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// A directory outside AppLab's storage, standing in for somewhere sensitive.
	outside := t.TempDir()
	canary := filepath.Join(outside, "canary.txt")
	if err := os.WriteFile(canary, []byte("original\n"), 0o600); err != nil {
		t.Fatalf("write canary: %v", err)
	}

	archive := buildTar(t, []tarEntry{
		{name: "ok.txt", body: "fine\n"},
		// A symlink pointing out of the tree.
		{name: "link", typeflag: tar.TypeSymlink, linkname: outside},
		// A write that would land in the outside directory if the link were
		// followed.
		{name: "link/canary.txt", body: "overwritten\n"},
	})

	_, err := s.Ingest(ctx, "shop", bytes.NewReader(archive), "", "", DefaultIngestLimits)
	// Whether this succeeds or fails, the canary outside must be untouched —
	// that is the actual security property, and asserting only on the error
	// would miss a case where the write happened before the failure.
	got, readErr := os.ReadFile(canary)
	if readErr != nil {
		t.Fatalf("read canary: %v", readErr)
	}
	if string(got) != "original\n" {
		t.Errorf("a file outside the destination was overwritten through a symlink; canary is now %q", got)
	}
	_ = err
}

// TestIngestGzipTransparently asserts compression is detected from the bytes,
// not from a filename, since a piped `tar czf -` has no name to inspect.
func TestIngestGzipTransparently(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	archive := gzipBytes(t, buildTar(t, []tarEntry{
		{name: "Dockerfile", body: "FROM scratch\n"},
	}))

	result, err := s.Ingest(ctx, "shop", bytes.NewReader(archive), "gz upload", "", DefaultIngestLimits)
	if err != nil {
		t.Fatalf("Ingest of a gzipped archive: %v", err)
	}
	if result.Files != 1+len(seeded) {
		t.Errorf("Files = %d, want %d", result.Files, 1+len(seeded))
	}
}

// TestIngestSkipsGitDirectory asserts a .git directory in the archive is not
// stored: it is meaningless inside AppLab's own repository and can be large.
func TestIngestSkipsGitDirectory(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	archive := buildTar(t, []tarEntry{
		{name: "main.go", body: "package main\n"},
		{name: ".git/config", body: "[core]\n"},
		{name: ".git/objects/ab/cdef", body: "junk\n"},
	})

	if _, err := s.Ingest(ctx, "shop", bytes.NewReader(archive), "", "", DefaultIngestLimits); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	tree := runGit(t, mustRepoPath(t, s, "shop"), "ls-tree", "-r", "--name-only", "refs/heads/main")
	if strings.Contains(tree, ".git") {
		t.Errorf("the archive's .git directory was committed; got:\n%s", tree)
	}
}

// TestIngestExtendsHistory asserts a second upload builds on the first rather
// than starting a fresh history, which is what makes rollback possible.
func TestIngestExtendsHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	first, err := s.Ingest(ctx, "shop", bytes.NewReader(buildTar(t, []tarEntry{
		{name: "a.txt", body: "one\n"},
	})), "first", "", DefaultIngestLimits)
	if err != nil {
		t.Fatalf("first Ingest: %v", err)
	}

	second, err := s.Ingest(ctx, "shop", bytes.NewReader(buildTar(t, []tarEntry{
		{name: "a.txt", body: "two\n"},
		{name: "b.txt", body: "new\n"},
	})), "second", "", DefaultIngestLimits)
	if err != nil {
		t.Fatalf("second Ingest: %v", err)
	}

	if second.SHA == first.SHA {
		t.Fatal("the second upload produced the same commit as the first")
	}

	// The second commit's parent must be the first, giving linear history.
	parent := strings.TrimSpace(runGit(t, mustRepoPath(t, s, "shop"), "rev-parse", second.SHA+"^"))
	if parent != first.SHA {
		t.Errorf("second commit's parent = %s, want %s", parent, first.SHA)
	}

	commits, err := s.Log(ctx, "shop", 10)
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	// Two uploads on top of the commit a new repository starts with.
	if len(commits) != 2+seedCommits {
		t.Fatalf("Log returned %d commits, want %d", len(commits), 2+seedCommits)
	}
	// Newest first.
	if commits[0].SHA != second.SHA {
		t.Errorf("Log's first entry = %s, want the newest commit %s", commits[0].SHA, second.SHA)
	}
	if commits[0].Subject != "second" {
		t.Errorf("subject = %q, want %q", commits[0].Subject, "second")
	}
}

// TestIngestUnchangedSourceIsANoOp asserts an upload identical to the current
// tip does not add an empty commit — a history entry a rollback could land on
// and gain nothing from.
func TestIngestUnchangedSourceIsANoOp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	entries := []tarEntry{{name: "a.txt", body: "same\n"}}

	first, err := s.Ingest(ctx, "shop", bytes.NewReader(buildTar(t, entries)), "first", "", DefaultIngestLimits)
	if err != nil {
		t.Fatalf("first Ingest: %v", err)
	}
	second, err := s.Ingest(ctx, "shop", bytes.NewReader(buildTar(t, entries)), "again", "", DefaultIngestLimits)
	if err != nil {
		t.Fatalf("second Ingest: %v", err)
	}

	if second.SHA != first.SHA {
		t.Errorf("an unchanged upload created a new commit (%s then %s); it should be a no-op", first.SHA, second.SHA)
	}
}

// TestIngestEnforcesSizeLimit asserts the expansion is bounded, since a tarball
// can claim to be enormous while very little arrives.
func TestIngestEnforcesSizeLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// A body far larger than the budget we impose.
	big := strings.Repeat("x", 10_000)
	archive := buildTar(t, []tarEntry{{name: "big.txt", body: big}})

	_, err := s.Ingest(ctx, "shop", bytes.NewReader(archive), "", "",
		IngestLimits{MaxBytes: 1000, MaxFiles: 10})
	if err == nil {
		t.Fatal("an archive larger than the byte limit was accepted")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("the error should mention the limit, got: %v", err)
	}
}

// TestIngestEnforcesFileCountLimit asserts the entry count is bounded too, so an
// archive of a million empty files cannot exhaust inodes.
func TestIngestEnforcesFileCountLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	entries := make([]tarEntry, 0, 20)
	for i := 0; i < 20; i++ {
		entries = append(entries, tarEntry{name: "f" + itoa(i) + ".txt", body: "x"})
	}

	_, err := s.Ingest(ctx, "shop", bytes.NewReader(buildTar(t, entries)), "", "",
		IngestLimits{MaxBytes: 1 << 20, MaxFiles: 5})
	if err == nil {
		t.Fatal("an archive with more entries than the limit was accepted")
	}
	if !strings.Contains(err.Error(), "entries") {
		t.Errorf("the error should mention the entry limit, got: %v", err)
	}
}

// TestIngestEmptyArchive asserts an archive with nothing in it is reported as
// such rather than creating an empty commit.
func TestIngestEmptyArchive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	result, err := s.Ingest(ctx, "shop", bytes.NewReader(buildTar(t, nil)), "", "", DefaultIngestLimits)
	if err != nil {
		t.Fatalf("Ingest of an empty archive: %v", err)
	}
	if !result.Empty {
		t.Error("an empty archive was not reported as empty")
	}
	if result.SHA != "" {
		t.Errorf("an empty archive produced commit %s", result.SHA)
	}
}

// TestIngestRejectsInvalidAppID asserts the id is validated before it becomes a
// filesystem path.
func TestIngestRejectsInvalidAppID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, id := range []string{"../evil", "Bad", "", "has/slash"} {
		if _, err := s.Ingest(ctx, id, bytes.NewReader(buildTar(t, nil)), "", "", DefaultIngestLimits); err == nil {
			t.Errorf("Ingest accepted the invalid app id %q", id)
		}
	}
}

// TestLogOnEmptyRepository asserts a freshly created app reads as having no
// commits rather than as an error, since that is a normal state.
// TestANewRepositoryHasExactlyTheSeedCommit asserts what a repository holds
// between creation and the first upload.
//
// This used to assert it held *nothing*, which was true when creation made a
// bare repository with no refs. It now holds one commit — the files that tell a
// caller how to work with the app — so the guarantee worth keeping is that it
// holds exactly those and that Log and HeadCommit behave on it.
func TestANewRepositoryHasExactlyTheSeedCommit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Create(ctx, "shop"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	commits, err := s.Log(ctx, "shop", 10)
	if err != nil {
		t.Fatalf("Log on a new repository: %v", err)
	}
	if len(commits) != seedCommits {
		t.Errorf("got %d commits from a new repository, want %d", len(commits), seedCommits)
	}

	// A clone has something to check out, which is the point of the seed commit:
	// a repository whose HEAD names a ref that does not exist clones to nothing
	// and warns about a detached HEAD.
	head, err := s.HeadCommit(ctx, "shop")
	if err != nil {
		t.Fatalf("HeadCommit on a new repository: %v", err)
	}
	if head != commits[0].SHA {
		t.Errorf("HeadCommit = %s, want the tip Log reported, %s", head, commits[0].SHA)
	}
}

// TestCreateIsIdempotent asserts a retried create does not fail, since the
// caller cannot tell whether the first attempt landed.
func TestCreateIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := s.Create(ctx, "shop"); err != nil {
			t.Fatalf("Create attempt %d: %v", i+1, err)
		}
	}
}

// TestIngestExecutableBitIsPreserved asserts a script stays executable, which a
// build may depend on.
func TestIngestExecutableBitIsPreserved(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	archive := buildTar(t, []tarEntry{
		{name: "build.sh", body: "#!/bin/sh\necho hi\n", mode: 0o755},
	})

	if _, err := s.Ingest(ctx, "shop", bytes.NewReader(archive), "", "", DefaultIngestLimits); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	out := runGit(t, mustRepoPath(t, s, "shop"), "ls-tree", "-r", "refs/heads/main")
	if !strings.Contains(out, "100755") {
		t.Errorf("the executable bit was lost; ls-tree shows:\n%s", out)
	}
}

// TestIngestStripsDangerousModes asserts setuid and setgid bits are not
// reproduced from the archive into AppLab's storage.
func TestIngestStripsDangerousModes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	archive := buildTar(t, []tarEntry{
		{name: "sneaky", body: "x\n", mode: 0o4755}, // setuid
	})

	if _, err := s.Ingest(ctx, "shop", bytes.NewReader(archive), "", "", DefaultIngestLimits); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	out := runGit(t, mustRepoPath(t, s, "shop"), "ls-tree", "-r", "refs/heads/main")

	// The mode is the first field of each line, and it is read as one.
	//
	// The original check was a substring search over the whole output, which was
	// wrong in a way that only showed up once trees grew: a blob's SHA is hex and
	// contains "1004" often enough that the search reported a setuid mode on a
	// tree that had none. Reading the field the mode actually lives in is both
	// correct and narrower — any mode that is not one of the three git stores is
	// a failure, rather than only the two this test happened to name.
	sawExecutable := false
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "100644": // regular file
		case "100755": // executable file
			sawExecutable = true
		case "040000": // tree, which -r does not normally list
		default:
			t.Errorf("unexpected mode %s in committed tree: %s", fields[0], line)
		}
	}
	// The seed script has to keep its executable bit, which is the legitimate use
	// of 100755 and the thing the mode stripping must not take away.
	if !sawExecutable {
		t.Errorf("no file at the tip is executable; the seed script must be:\n%s", out)
	}
}

// mustRepoPath is RepoPath with the error turned into a test failure.
func mustRepoPath(t *testing.T, s *Store, appID string) string {
	t.Helper()
	return materializeRepo(t, s, appID)
}

// runGit executes git in dir and returns its combined output, failing the test
// if it does not succeed. It is deliberately independent of the package's own
// runner so a test reads the repository the way any other git client would.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
