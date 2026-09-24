// Package source stores each app's source in a git repository that AppLab hosts.
//
// The repository is the record. An upload arrives as a tarball because that is
// what any caller can produce — an agent, a container with no git credential, a
// script — but what it leaves behind is a real commit in a real repository, so
// the source can be cloned, diffed, browsed and checked out with ordinary git
// tools. Nothing about the tarball survives the ingest except its contents.
//
// Layout on disk, under the configured data directory:
//
//	repos/<app-id>.git/          bare repository, one per app
//	uploads/<upload-id>/         parts of an in-progress chunked upload
//	tmp/<random>                 scratch space for assembling an upload
//
// Every repository is bare: nothing ever checks out a working tree in place,
// including the ingest itself, which builds its commit with plumbing commands
// against a temporary index. A working tree would be a second copy of the
// source to keep consistent, and concurrent ingests into one would race.
package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shaowenchen/applab/internal/objectstore"
)

// Store manages the per-app git repositories.
//
// The repositories live in object storage and are copied out to a scratch
// directory for as long as an operation needs one — see workingRepo for why git
// cannot run against a bucket, and what that costs. Nothing is kept between
// operations, so a replica holds no state and can be replaced at any moment.
type Store struct {
	// objects is where the repositories live.
	objects objectstore.Store

	// dataDir is scratch space: the local directory a repository is materialised
	// into, and where an upload is unpacked. It needs no volume, because nothing
	// in it survives being useful — a directory emptied on restart loses no
	// repository.
	dataDir string

	// gitBin is the resolved path to the git executable, found once at
	// construction so a missing git is reported at boot rather than as a
	// confusing failure on the first upload.
	gitBin string

	// author is the identity recorded on commits AppLab creates. It is a
	// deliberate constant rather than the uploading caller's name: the caller
	// may be anyone, and attributing a commit to a name they supplied would let
	// a key holder forge attribution to a person who never made the change.
	authorName  string
	authorEmail string

	// locks serialises operations on one app's repository. See lockRepo for why
	// it is per app, and for what it does not protect against.
	locksMu sync.Mutex
	locks   map[string]*repoLock
}

// repoLock is one app's lock. It is a struct rather than a bare mutex so the map
// can hold a pointer and the lock's identity does not move when the map grows.
type repoLock struct{ mu sync.Mutex }

// Options configure a Store.
type Options struct {
	// Objects is where repositories are kept. Required.
	Objects objectstore.Store

	// DataDir is scratch space for materialising a repository and unpacking an
	// upload. Nothing durable is written here.
	DataDir string

	AuthorName  string
	AuthorEmail string

	// GitPath overrides the git executable. Empty means find it in PATH, which
	// is what a deployment wants; a test may set it to pin one.
	GitPath string
}

// New creates a Store, checking that git is available.
func New(opts Options) (*Store, error) {
	if opts.Objects == nil {
		return nil, fmt.Errorf("source: no object store given")
	}
	if opts.DataDir == "" {
		return nil, fmt.Errorf("source: no scratch directory given")
	}

	gitBin := opts.GitPath
	if gitBin == "" {
		var err error
		gitBin, err = exec.LookPath("git")
		if err != nil {
			return nil, fmt.Errorf("git is required to store source but was not found in PATH: %w", err)
		}
	}

	if opts.AuthorName == "" {
		opts.AuthorName = "applab"
	}
	if opts.AuthorEmail == "" {
		opts.AuthorEmail = "applab@localhost"
	}

	s := &Store{
		objects:     opts.Objects,
		dataDir:     opts.DataDir,
		gitBin:      gitBin,
		authorName:  opts.AuthorName,
		authorEmail: opts.AuthorEmail,
	}

	// The scratch layout is created up front so the first upload does not pay for
	// it, and so a permissions problem surfaces at boot rather than mid-upload.
	if err := s.ensureScratch(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) uploadsDir() string { return filepath.Join(s.dataDir, "uploads") }
func (s *Store) tmpDir() string     { return filepath.Join(s.dataDir, "tmp") }

// localRepoPath is where an app's repository is materialised for one operation.
//
// It is not the repository's home — that is object storage — and nothing at this
// path survives the operation that made it. It is inside the scratch directory
// rather than a system temporary directory so it is on the same filesystem as
// the working trees an upload builds, which is what lets a tree be moved into a
// repository rather than copied.
func (s *Store) localRepoPath(appID string) (string, error) {
	if err := validateAppID(appID); err != nil {
		return "", err
	}
	return filepath.Join(s.tmpDir(), "repos", appID+".git"), nil
}

// GitPath returns the resolved git executable, for callers that drive git
// themselves (the HTTP transport).
func (s *Store) GitPath() string { return s.gitBin }

// Exists reports whether an app already has a repository.
func (s *Store) Exists(appID string) (bool, error) {
	return s.existsInBucket(context.Background(), appID)
}

// Create initialises an app's repository.
//
// Initialising an app that already has one is not an error: the operation is
// idempotent, and a retried create should not fail on the second attempt.
//
// The repository is bare and has no working tree, so there is nothing to check
// out and no branch to be "on" — commits are built directly and the default
// branch is set to main so a clone does not warn about a detached HEAD.
func (s *Store) Create(ctx context.Context, appID string) error {
	exists, err := s.Exists(appID)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	repoPath, err := s.localRepoPath(appID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(repoPath, 0o700); err != nil {
		return fmt.Errorf("create repository directory: %w", err)
	}
	// Whatever happens from here, the scratch directory goes: the repository is
	// only real once it has been uploaded.
	defer os.RemoveAll(repoPath)

	if _, err := s.run(ctx, "", "init", "--bare", "--initial-branch=main", repoPath); err != nil {
		// A failed init leaves a directory that Exists() would report as absent
		// (no HEAD), so it is safe to remove and let a retry start clean.
		_ = os.RemoveAll(repoPath)
		return fmt.Errorf("initialise repository for app %s: %w", appID, err)
	}

	// A bare repository created with init has no branch ref at all, so a fresh
	// clone reports "remote HEAD refers to nonexistent ref". Setting HEAD
	// explicitly makes an empty clone behave sensibly, which matters because the
	// very first thing a caller may do is clone an app it just created.
	if _, err := s.run(ctx, repoPath, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		return fmt.Errorf("set default branch for app %s: %w", appID, err)
	}

	// An opening commit carrying the files that tell a caller how to work with
	// this app. It is committed here rather than left to the first upload so that
	// cloning an app that has never been pushed to produces something usable —
	// and the upload path injects the same files again, because a commit is built
	// from the uploaded tree alone and would otherwise replace them.
	if err := s.seedCommit(ctx, appID, repoPath); err != nil {
		return fmt.Errorf("commit the seed files for app %s: %w", appID, err)
	}

	// Receive-pack is what a push needs, and the repository was just created by
	// the same user that runs the server, so the default hooks are already
	// correct. Nothing further is configured.
	//
	// The new repository is uploaded here, before this returns: everything below
	// this line works from the bucket, so a repository that existed only in
	// scratch would be invisible to the upload that follows.
	return s.uploadRepo(ctx, appID, repoPath)
}

// Remove deletes an app's repository.
func (s *Store) Remove(ctx context.Context, appID string) error {
	if err := validateAppID(appID); err != nil {
		return err
	}

	objects, err := s.objects.List(ctx, s.repoPrefix(appID))
	if err != nil {
		return fmt.Errorf("list the repository for app %s: %w", appID, err)
	}
	for _, object := range objects {
		if err := s.objects.Delete(ctx, object.Key); err != nil {
			return fmt.Errorf("remove %s: %w", object.Key, err)
		}
	}

	// And the scratch copy, if one is lying around from an interrupted run.
	if repoPath, err := s.localRepoPath(appID); err == nil {
		_ = os.RemoveAll(repoPath)
	}
	return nil
}

// Log returns an app's commits, newest first, read from the repository itself.
//
// This reads git rather than AppLab's own commit table on purpose: the
// repository is the record, and a database row could have been written for a
// commit that was later removed, or missed for one that was pushed directly
// over git. What git says is what is actually there.
func (s *Store) Log(ctx context.Context, appID string, limit int) ([]CommitInfo, error) {
	var infos []CommitInfo
	err := s.withRepo(ctx, appID, func(repoPath string) error {
		var err error
		infos, err = s.logFrom(ctx, repoPath, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return infos, nil
}

// logFrom reads the history out of a repository that is already on local disk.
func (s *Store) logFrom(ctx context.Context, repoPath string, limit int) ([]CommitInfo, error) {

	if limit <= 0 {
		limit = 50
	}

	// A record separator after each commit's fields, so a commit message with
	// newlines in it cannot be mistaken for the start of the next commit. The
	// format's field separator is a NUL, which cannot appear in a commit header.
	format := "%H%x00%an%x00%aI%x00%s%x00%x1e"
	out, err := s.run(ctx, repoPath, "log", "--max-count="+itoa(limit), "--format="+format, "refs/heads/main", "--")
	if err != nil {
		// An empty repository has no commits and git exits non-zero. That is a
		// normal state for a newly created app, not a failure.
		if isEmptyRepoError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read commits: %w", err)
	}

	return parseLog(out), nil
}

// CommitInfo is one commit as read back from the repository.
type CommitInfo struct {
	SHA       string
	Author    string
	CreatedAt time.Time
	Subject   string
}

// parseLog decodes the format written by Log.
func parseLog(out []byte) []CommitInfo {
	var commits []CommitInfo
	for _, record := range strings.Split(string(out), "\x1e") {
		record = strings.TrimSpace(record)
		if record == "" {
			continue
		}
		fields := strings.Split(record, "\x00")
		if len(fields) < 4 {
			// A malformed record means the format string and this parser have
			// drifted; skipping is better than emitting a nonsense commit, and
			// the count difference will be visible.
			continue
		}
		created, err := time.Parse(time.RFC3339, fields[2])
		if err != nil {
			created = time.Time{}
		}
		commits = append(commits, CommitInfo{
			SHA:       fields[0],
			Author:    fields[1],
			CreatedAt: created.UTC(),
			Subject:   fields[3],
		})
	}
	return commits
}

// ResolveCommit returns the full SHA for a possibly abbreviated revision.
//
// A caller may refer to a commit by any prefix it was shown, so this is what
// turns that into the canonical id the rest of the system uses.
func (s *Store) ResolveCommit(ctx context.Context, appID, revision string) (string, error) {
	var sha string
	err := s.withRepo(ctx, appID, func(repoPath string) error {
		out, err := s.run(ctx, repoPath, "rev-parse", "--verify", revision+"^{commit}")
		if err != nil {
			return err
		}
		sha = strings.TrimSpace(string(out))
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("resolve %q in app %s: %w", revision, appID, err)
	}
	return sha, nil
}

// HeadCommit returns the commit at the tip of the default branch.
func (s *Store) HeadCommit(ctx context.Context, appID string) (string, error) {
	var sha string
	err := s.withRepo(ctx, appID, func(repoPath string) error {
		out, err := s.run(ctx, repoPath, "rev-parse", "--verify", "refs/heads/main^{commit}")
		if err != nil {
			return err
		}
		sha = strings.TrimSpace(string(out))
		return nil
	})
	if err != nil {
		if isEmptyRepoError(err) {
			return "", ErrNoCommits
		}
		return "", fmt.Errorf("read head of app %s: %w", appID, err)
	}
	return sha, nil
}

// ErrNoCommits means the repository exists but has nothing in it yet.
var ErrNoCommits = errors.New("repository has no commits")

// isEmptyRepoError reports whether a git failure is just "nothing here yet".
//
// An empty repository makes several plumbing commands exit non-zero with a
// message rather than a distinct status, so the message is what distinguishes
// "no commits" from a real failure. It is confined to this function so a git
// version that words it differently has one place to fix.
func isEmptyRepoError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "does not have any commits yet") ||
		strings.Contains(msg, "unknown revision or path not in the working tree") ||
		strings.Contains(msg, "bad revision") ||
		strings.Contains(msg, "Needed a single revision")
}

// run executes a git command in dir (or the process's directory when dir is
// empty) and returns its combined output.
//
// The environment is scrubbed of anything that would make git behave
// differently depending on the caller's shell: no global config, no pager, no
// prompts (which would hang a server waiting for input on a terminal it does
// not have), and a fixed identity so commits are attributed the same way
// regardless of who is running the process.
func (s *Store) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, s.gitBin, args...)
	if dir != "" {
		cmd.Dir = dir
	}

	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GIT_PAGER=cat",
		"LC_ALL=C",
		"GIT_AUTHOR_NAME="+s.authorName,
		"GIT_AUTHOR_EMAIL="+s.authorEmail,
		"GIT_COMMITTER_NAME="+s.authorName,
		"GIT_COMMITTER_EMAIL="+s.authorEmail,
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(redactArgs(args), " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// redactArgs keeps error messages from carrying credentials.
//
// No argument AppLab passes currently contains a secret, but git URLs can carry
// one, and an error message is exactly where a token ends up in a log line. The
// scrub is here so that stays true if a future call passes a URL.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.Contains(a, "://") && strings.Contains(a, "@") {
			out[i] = "<redacted url>"
			continue
		}
		out[i] = a
	}
	return out
}

// itoa is a local integer formatter, kept to avoid pulling strconv into the hot
// path of every git invocation for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// Objects returns the object store the repositories live in.
//
// It is exported for tests that need to assert on what a push actually stored:
// the bucket is the only copy now, so an assertion about the repository is an
// assertion about the bucket.
func (s *Store) Objects() objectstore.Store { return s.objects }
