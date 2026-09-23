// Package source stores each app's source in a git repository that applab hosts.
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
	"time"

	"github.com/shaowenchen/applab/internal/model"
)

// Store manages the per-app git repositories.
type Store struct {
	// dataDir is the configured data directory. Every path this package builds
	// is derived from it and validated to stay inside it, so an app id can never
	// address a directory outside applab's own storage.
	dataDir string

	// gitBin is the resolved path to the git executable, found once at
	// construction so a missing git is reported at boot rather than as a
	// confusing failure on the first upload.
	gitBin string

	// author is the identity recorded on commits applab creates. It is a
	// deliberate constant rather than the uploading caller's name: the caller
	// may be anyone, and attributing a commit to a name they supplied would let
	// a key holder forge attribution to a person who never made the change.
	authorName  string
	authorEmail string
}

// Options configure a Store.
type Options struct {
	DataDir     string
	AuthorName  string
	AuthorEmail string
}

// New creates a Store, checking that git is available.
func New(opts Options) (*Store, error) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("git is required to store source but was not found in PATH: %w", err)
	}

	if opts.AuthorName == "" {
		opts.AuthorName = "applab"
	}
	if opts.AuthorEmail == "" {
		opts.AuthorEmail = "applab@localhost"
	}

	s := &Store{
		dataDir:     opts.DataDir,
		gitBin:      gitBin,
		authorName:  opts.AuthorName,
		authorEmail: opts.AuthorEmail,
	}

	// Create the layout up front so the first upload does not pay for it, and so
	// a permissions problem surfaces at boot rather than mid-upload.
	for _, dir := range []string{s.reposDir(), s.uploadsDir(), s.tmpDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return s, nil
}

func (s *Store) reposDir() string   { return filepath.Join(s.dataDir, "repos") }
func (s *Store) uploadsDir() string { return filepath.Join(s.dataDir, "uploads") }
func (s *Store) tmpDir() string     { return filepath.Join(s.dataDir, "tmp") }

// RepoPath returns the bare repository directory for an app.
//
// The id is validated first. It has already been checked when the app was
// created, but this is the function that turns it into a filesystem path, so
// this is where a traversal would become real — and defence here costs nothing.
func (s *Store) RepoPath(appID string) (string, error) {
	if err := model.ValidateAppID(appID); err != nil {
		return "", err
	}
	path := filepath.Join(s.reposDir(), appID+".git")

	// Belt and braces: even with a validated id, confirm the result is inside
	// the repositories directory before anyone uses it.
	if !strings.HasPrefix(path, s.reposDir()+string(os.PathSeparator)) {
		return "", fmt.Errorf("app id %q resolves outside the repository directory", appID)
	}
	return path, nil
}

// GitPath returns the resolved git executable, for callers that drive git
// themselves (the HTTP transport).
func (s *Store) GitPath() string { return s.gitBin }

// Exists reports whether an app already has a repository.
func (s *Store) Exists(appID string) (bool, error) {
	path, err := s.RepoPath(appID)
	if err != nil {
		return false, err
	}
	_, statErr := os.Stat(filepath.Join(path, "HEAD"))
	if statErr == nil {
		return true, nil
	}
	if errors.Is(statErr, os.ErrNotExist) {
		return false, nil
	}
	return false, statErr
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
	repoPath, err := s.RepoPath(appID)
	if err != nil {
		return err
	}

	exists, err := s.Exists(appID)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	if err := os.MkdirAll(repoPath, 0o700); err != nil {
		return fmt.Errorf("create repository directory: %w", err)
	}

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

	// Receive-pack is what a push needs, and the repository was just created by
	// the same user that runs the server, so the default hooks are already
	// correct. Nothing further is configured.
	return nil
}

// Remove deletes an app's repository.
func (s *Store) Remove(ctx context.Context, appID string) error {
	repoPath, err := s.RepoPath(appID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(repoPath); err != nil {
		return fmt.Errorf("remove repository for app %s: %w", appID, err)
	}
	return nil
}

// Log returns an app's commits, newest first, read from the repository itself.
//
// This reads git rather than applab's own commit table on purpose: the
// repository is the record, and a database row could have been written for a
// commit that was later removed, or missed for one that was pushed directly
// over git. What git says is what is actually there.
func (s *Store) Log(ctx context.Context, appID string, limit int) ([]CommitInfo, error) {
	repoPath, err := s.RepoPath(appID)
	if err != nil {
		return nil, err
	}
	exists, err := s.Exists(appID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}

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
		return nil, fmt.Errorf("read commits for app %s: %w", appID, err)
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
	repoPath, err := s.RepoPath(appID)
	if err != nil {
		return "", err
	}
	out, err := s.run(ctx, repoPath, "rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve %q in app %s: %w", revision, appID, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// HeadCommit returns the commit at the tip of the default branch.
func (s *Store) HeadCommit(ctx context.Context, appID string) (string, error) {
	repoPath, err := s.RepoPath(appID)
	if err != nil {
		return "", err
	}
	out, err := s.run(ctx, repoPath, "rev-parse", "--verify", "refs/heads/main^{commit}")
	if err != nil {
		if isEmptyRepoError(err) {
			return "", ErrNoCommits
		}
		return "", fmt.Errorf("read head of app %s: %w", appID, err)
	}
	return strings.TrimSpace(string(out)), nil
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
// No argument applab passes currently contains a secret, but git URLs can carry
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
