package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// commitWorkTree turns a directory into a commit on the repository and returns
// the new commit's id, the file count and the total bytes committed.
//
// It uses plumbing — hash-object, update-index, write-tree, commit-tree,
// update-ref — rather than `git add` and `git commit`, for two reasons that both
// matter here:
//
//   - There is no working tree. The repository is bare, and plumbing can build
//     an object graph from arbitrary files without one, so AppLab never has to
//     materialise a second copy of the source.
//   - It is safe under concurrency. Each ingest uses its own index file, and the
//     only shared mutation is the final update-ref, which git performs as a
//     compare-and-swap. Two uploads racing therefore produce two commits and one
//     wins the ref, rather than corrupting each other's tree.
//
// A commit with no parent is created only when the branch is genuinely empty;
// otherwise the new commit is built on the existing tip, so history is linear
// and a rollback has something to roll back to.
func (s *Store) commitWorkTree(
	ctx context.Context,
	appID string,
	branch string,
	repoPath string,
	workTree string,
	scratch string,
	subject string,
	parent string,
) (sha string, files int, totalBytes int64, err error) {
	// The index is per-upload and lives in the scratch directory, so concurrent
	// ingests cannot see each other's staging state.
	indexPath := filepath.Join(scratch, "index")
	env := []string{"GIT_INDEX_FILE=" + indexPath}

	// Resolve the parent. An explicitly requested parent is verified to exist,
	// because committing onto a commit that is not there would silently produce
	// an orphan.
	if parent != "" {
		resolved, err := s.ResolveCommit(ctx, appID, branch, parent)
		if err != nil {
			return "", 0, 0, fmt.Errorf("resolve parent commit: %w", err)
		}
		parent = resolved
	} else {
		// Default to the current tip so an upload extends history.
		if head, err := s.headWithEnv(ctx, repoPath, branch, env); err == nil {
			parent = head
		} else if !errors.Is(err, ErrNoCommits) {
			return "", 0, 0, err
		}
	}

	if err := s.stageDirectory(ctx, repoPath, workTree, env); err != nil {
		return "", 0, 0, err
	}

	treeOut, err := s.runWithEnv(ctx, repoPath, env, "write-tree")
	if err != nil {
		return "", 0, 0, fmt.Errorf("write tree: %w", err)
	}
	tree := strings.TrimSpace(string(treeOut))
	if tree == "" {
		return "", 0, 0, errors.New("git produced no tree for the uploaded source")
	}

	// Count what was committed by asking git, rather than by counting during
	// extraction: this is the tree that actually landed, so the numbers cannot
	// disagree with the record.
	files, totalBytes, err = s.treeStats(ctx, repoPath, tree)
	if err != nil {
		return "", 0, 0, err
	}

	// An upload identical to the current tip has nothing to record. Producing an
	// empty commit for it would add a history entry that a rollback could land
	// on and gain nothing from, so the existing commit is returned instead and
	// the caller can tell the difference from the unchanged file count.
	if parent != "" {
		if parentTreeOut, err := s.run(ctx, repoPath, "rev-parse", parent+"^{tree}"); err == nil {
			if strings.TrimSpace(string(parentTreeOut)) == tree {
				return parent, files, totalBytes, nil
			}
		}
	}

	args := []string{"commit-tree", tree, "-m", subject}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	commitOut, err := s.runWithEnv(ctx, repoPath, env, args...)
	if err != nil {
		return "", 0, 0, fmt.Errorf("create commit: %w", err)
	}
	sha = strings.TrimSpace(string(commitOut))

	// Update the branch. --create-reflog is not needed on a bare repo created by
	// AppLab, but the ref is set unconditionally to this branch so that an upload
	// always lands on the branch a clone will check out.
	if _, err := s.run(ctx, repoPath, "update-ref", branchRef(branch), sha, parent); err != nil {
		// A concurrent upload moved the branch between reading the parent and
		// updating it. The object is written and valid; only the ref lost the
		// race, so retry once on the new tip rather than failing an upload that
		// did nothing wrong.
		if head, headErr := s.HeadCommit(ctx, appID, branch); headErr == nil {
			args := []string{"commit-tree", tree, "-m", subject, "-p", head}
			retryOut, retryErr := s.runWithEnv(ctx, repoPath, env, args...)
			if retryErr != nil {
				return "", 0, 0, fmt.Errorf("create commit: %w", retryErr)
			}
			retrySHA := strings.TrimSpace(string(retryOut))
			if _, retryRefErr := s.run(ctx, repoPath, "update-ref", branchRef(branch), retrySHA, head); retryRefErr != nil {
				return "", 0, 0, fmt.Errorf("update branch after concurrent upload: %w", retryRefErr)
			}
			return retrySHA, files, totalBytes, nil
		}
		return "", 0, 0, fmt.Errorf("update branch: %w", err)
	}

	if _, err := s.run(ctx, repoPath, "symbolic-ref", "HEAD", branchRef(branch)); err != nil {
		return "", 0, 0, fmt.Errorf("point HEAD at %s: %w", branch, err)
	}

	return sha, files, totalBytes, nil
}

// headWithEnv reads the current tip using a specific git environment.
func (s *Store) headWithEnv(ctx context.Context, repoPath, branch string, env []string) (string, error) {
	out, err := s.runWithEnv(ctx, repoPath, env, "rev-parse", "--verify", branchRef(branch)+"^{commit}")
	if err != nil {
		if isEmptyRepoError(err) {
			return "", ErrNoCommits
		}
		return "", fmt.Errorf("read current head: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// stageDirectory adds every file under workTree to the index.
//
// git add with --force is used because a source tree legitimately contains
// entries a default add would skip — a .gitignore that excludes something the
// caller still wants stored, or a build output directory that is committed
// deliberately. The archive is an explicit statement of what the caller wants,
// so AppLab stores what arrived rather than re-deciding on their behalf.
func (s *Store) stageDirectory(ctx context.Context, repoPath, workTree string, env []string) error {
	absWorkTree, err := filepath.Abs(workTree)
	if err != nil {
		return fmt.Errorf("resolve working tree: %w", err)
	}

	cmd := exec.CommandContext(ctx, s.gitBin,
		"--git-dir", repoPath,
		"--work-tree", absWorkTree,
		"add", "--all", "--force", ".")
	cmd.Dir = absWorkTree
	cmd.Env = s.gitEnv(env)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("stage source: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// treeStats counts the files and bytes in a tree.
//
// The byte total is the size of the *content*, not the archive, which is what a
// caller can meaningfully compare against what it uploaded.
func (s *Store) treeStats(ctx context.Context, repoPath, tree string) (int, int64, error) {
	out, err := s.run(ctx, repoPath, "ls-tree", "-r", "-l", "--long", tree)
	if err != nil {
		return 0, 0, fmt.Errorf("inspect committed tree: %w", err)
	}

	var (
		files    int
		totalLen int64
	)
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		// Format: <mode> SP <type> SP <sha> SP* <size> TAB <path>. Only blobs
		// have a size; directories are not listed by -r, and a gitlink (a
		// submodule) reports a dash.
		fields := bytes.Fields(line)
		if len(fields) < 4 || string(fields[1]) != "blob" {
			continue
		}
		files++
		if size, err := parseInt64(fields[3]); err == nil {
			totalLen += size
		}
	}
	return files, totalLen, nil
}

// runWithEnv runs git with extra environment entries.
func (s *Store) runWithEnv(ctx context.Context, repoPath string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, s.gitBin, append([]string{"--git-dir", repoPath}, args...)...)
	cmd.Env = s.gitEnv(env)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(redactArgs(args), " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// gitEnv is the environment every git invocation runs with.
//
// Scrubbing the global and system config is what makes AppLab's behaviour
// independent of the machine it runs on: a host with a global gitignore, a
// commit hook, or a template directory would otherwise change what gets
// committed. The prompt and pager settings matter because a server has no
// terminal, and git asking a question with nowhere to ask it would hang.
func (s *Store) gitEnv(extra []string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + s.dataDir,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GIT_PAGER=cat",
		"LC_ALL=C",
		"TZ=UTC",
		"GIT_AUTHOR_NAME=" + s.authorName,
		"GIT_AUTHOR_EMAIL=" + s.authorEmail,
		"GIT_COMMITTER_NAME=" + s.authorName,
		"GIT_COMMITTER_EMAIL=" + s.authorEmail,
		"GIT_AUTHOR_DATE=" + time.Now().UTC().Format(time.RFC3339),
		"GIT_COMMITTER_DATE=" + time.Now().UTC().Format(time.RFC3339),
	}
	return append(env, extra...)
}

// parseInt64 parses a decimal integer from a byte slice.
func parseInt64(b []byte) (int64, error) {
	var n int64
	if len(b) == 0 {
		return 0, errors.New("empty")
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}
