package source

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Archive writes a commit's source tree as a tar.gz stream.
//
// It uses `git archive`, which is the right tool for two reasons: it produces a
// reproducible archive from a commit rather than from the working directory, so
// the same commit always yields the same bytes; and it needs no checkout, so a
// build can fetch its source without AppLab materialising a second copy.
//
// sha must be a full commit id. It reaches git as an argument, so it is validated
// first — a revision string from a request could otherwise be a flag.
func (s *Store) Archive(ctx context.Context, appID, sha string, w io.Writer) error {
	if !validFullSHA(sha) {
		return fmt.Errorf("archive requires a full commit id, got %q", sha)
	}

	repoPath, err := s.RepoPath(appID)
	if err != nil {
		return err
	}

	// -- is what stops the validated id from being read as an option; it is
	// belt and braces given the id is already constrained to hex.
	cmd := exec.CommandContext(ctx, s.gitBin,
		"--git-dir", repoPath,
		"archive", "--format=tar.gz", sha, "--")
	cmd.Env = s.gitEnv(nil)
	cmd.Stdout = w

	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("archive commit %s of app %s: %w: %s", sha, appID, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ArchiveSize returns the byte length of a commit's archive without producing
// it, so a caller can set Content-Length and the caller can show progress.
func (s *Store) ArchiveSize(ctx context.Context, appID, sha string) (int64, error) {
	if !validFullSHA(sha) {
		return 0, fmt.Errorf("archive requires a full commit id, got %q", sha)
	}

	// Counting requires producing the archive: gzip output size is not derivable
	// from the tree. It is done to a discarded writer, which is cheap next to the
	// transfer it is preparing for.
	var counter countingWriter
	if err := s.Archive(ctx, appID, sha, &counter); err != nil {
		return 0, err
	}
	return counter.n, nil
}

// countingWriter counts bytes written and discards them.
type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// validFullSHA reports whether s is a 40-character lowercase hex commit id.
//
// It mirrors the model's rule but is duplicated here deliberately: this is the
// boundary where the value becomes a command argument, and depending on a
// validator that might later be relaxed for another purpose would quietly widen
// what reaches git.
func validFullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
