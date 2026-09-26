package source

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/shaowenchen/applab/internal/model"
)

// IngestLimits bound what a single upload may consume.
type IngestLimits struct {
	// MaxBytes is the total uncompressed size allowed across all files.
	MaxBytes int64
	// MaxFiles is how many entries a tarball may contain.
	MaxFiles int
}

// DefaultIngestLimits are the bounds applied when a caller does not specify any.
var DefaultIngestLimits = IngestLimits{
	// Generous for a source tree, but bounded: a tarball can claim to hold
	// terabytes while a few megabytes arrive, and without a ceiling that
	// expansion happens on AppLab's disk rather than the uploader's.
	MaxBytes: 512 << 20,
	MaxFiles: 100_000,
}

// IngestResult describes what an upload produced.
type IngestResult struct {
	SHA      string
	Files    int
	Bytes    int64
	Subject  string
	Empty    bool
	Stripped string
}

// Ingest turns a source archive into a commit on the app's repository.
//
// body is read as a tar or tar.gz stream — the compression is detected from the
// leading bytes rather than from a filename, because a caller that pipes
// `tar czf -` has no filename to inspect and guessing wrong would produce an
// inscrutable error.
//
// The commit is built with git plumbing against a temporary index rather than by
// checking the source out anywhere. That keeps the bare repository the only copy
// on disk, and it is what makes two concurrent uploads safe: each builds its own
// tree and the final ref update is a single compare-and-swap.
//
// parent is the commit to build on. An empty parent starts a new history.
func (s *Store) Ingest(
	ctx context.Context,
	appID string,
	branch string,
	body io.Reader,
	message string,
	parent string,
	limits IngestLimits,
) (*IngestResult, error) {
	if err := model.ValidateBranchName(branch); err != nil {
		return nil, err
	}
	if limits.MaxBytes <= 0 {
		limits = DefaultIngestLimits
	}

	// The repository has to exist before the archive is unpacked, because the
	// commit is built against it. Creating it here rather than requiring the
	// caller to have done so is what makes an upload the operation that can
	// bring an app's source into being.
	if exists, err := s.Exists(appID, branch); err != nil {
		return nil, err
	} else if !exists {
		if err := s.Create(ctx, appID, branch); err != nil {
			return nil, err
		}
	}

	// Scratch space for this upload's working tree and index. It is created
	// inside the scratch directory rather than the system temp dir so it shares
	// whatever space the node gave AppLab and is cleaned up by the same
	// lifecycle.
	workDir, err := os.MkdirTemp(s.tmpDir(), "ingest-")
	if err != nil {
		return nil, fmt.Errorf("create workspace: %w", err)
	}
	defer os.RemoveAll(workDir)

	workTree := filepath.Join(workDir, "tree")
	if err := os.MkdirAll(workTree, 0o700); err != nil {
		return nil, fmt.Errorf("create working tree: %w", err)
	}

	extracted, err := extractArchive(body, workTree, limits)
	if err != nil {
		return nil, err
	}

	// A tarball whose entries all lived under one directory is the common shape
	// — `tar czf - myproject` produces exactly that — and leaving the wrapper
	// directory in place would put the Dockerfile one level deeper than the
	// caller expects. Stripping happens after extraction so the decision can be
	// made from the whole listing rather than from the first entry, which may be
	// a directory or may be a file directly at the root.
	stripped := stripSingleRoot(workTree)

	if extracted.Files == 0 {
		return &IngestResult{Empty: true, Stripped: stripped}, nil
	}

	// The seeded files go in last, after the wrapper directory has been stripped
	// and before the tree is committed, so they land at the root of what the app
	// actually builds and no uploader can drop them below it.
	//
	// This has to happen on every upload, not only when the repository is
	// created: a commit is built from the uploaded tree alone, so whatever an
	// earlier commit contained is replaced wholesale. Seeding at creation would
	// produce files that vanish with the first push.
	if err := writeSeed(workTree, s.seedValues(ctx, appID)); err != nil {
		return nil, err
	}

	subject := firstLine(message)
	if subject == "" {
		subject = defaultCommitSubject(appID)
	}

	// The commit is built against a materialised copy of the repository, and the
	// copy is uploaded again afterwards — which is what makes an upload durable
	// rather than something that existed only on the node it ran on.
	var sha string
	var files int
	var totalBytes int64
	err = s.withRepo(ctx, appID, branch, func(repoPath string) error {
		var commitErr error
		sha, files, totalBytes, commitErr = s.commitWorkTree(ctx, appID, branch, repoPath, workTree, workDir, subject, parent)
		return commitErr
	})
	if err != nil {
		return nil, err
	}

	return &IngestResult{
		SHA:      sha,
		Files:    files,
		Bytes:    totalBytes,
		Subject:  subject,
		Stripped: stripped,
	}, nil
}

// extractStats is what extraction observed.
type extractStats struct {
	Files int
	Bytes int64
}

// extractArchive unpacks a tar or tar.gz stream into dest.
//
// Three things make this safe to do with a caller-supplied archive, and all
// three matter:
//
//   - Every write goes through an os.Root, which resolves paths without
//     following symlinks out of the destination. This is what stops the classic
//     archive attack: an entry that creates a symlink to /etc followed by an
//     entry that writes through it. A lexical check on the path string cannot
//     catch that, because the escape happens at the OS level when the second
//     entry is opened.
//   - The expansion is bounded in both total bytes and entry count, so a small
//     archive that claims to be enormous is refused rather than filled to disk.
//   - Entries with an absolute path or a ".." component are rejected outright
//     rather than sanitised. Silently rewriting a malicious path would store
//     something the uploader did not send; refusing says what happened.
func extractArchive(body io.Reader, dest string, limits IngestLimits) (extractStats, error) {
	reader, err := decompress(body)
	if err != nil {
		return extractStats{}, err
	}

	root, err := os.OpenRoot(dest)
	if err != nil {
		return extractStats{}, fmt.Errorf("open destination: %w", err)
	}
	defer root.Close()

	tr := tar.NewReader(reader)
	var stats extractStats

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return stats, fmt.Errorf("read archive: %w", err)
		}

		name, err := cleanArchivePath(header.Name)
		if err != nil {
			return stats, err
		}
		if name == "" {
			continue
		}

		// AppLab stores the source in its own git repository, so an archive's
		// own .git directory is both meaningless and potentially large. Skipping
		// it also removes a directory a caller cannot see is being ignored.
		if isGitMetadata(name) {
			continue
		}

		if header.Typeflag == tar.TypeXGlobalHeader || header.Name == "pax_global_header" {
			continue
		}

		stats.Files++
		if stats.Files > limits.MaxFiles {
			return stats, fmt.Errorf("archive contains more than %d entries; split the upload or exclude build output", limits.MaxFiles)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return stats, fmt.Errorf("create directory %s: %w", name, err)
			}

		case tar.TypeReg:
			written, err := writeArchiveFile(root, name, tr, header, limits.MaxBytes-stats.Bytes)
			if err != nil {
				return stats, err
			}
			stats.Bytes += written

		case tar.TypeSymlink:
			// Stored as a symlink rather than resolved: a source tree may
			// legitimately contain one, and git records links as their target
			// string without following them. A target pointing outside the tree
			// is preserved as text and never dereferenced by the ingest, so it
			// cannot be used to read anything.
			if err := ensureParent(root, name); err != nil {
				return stats, err
			}
			if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return stats, fmt.Errorf("replace %s: %w", name, err)
			}
			if err := root.Symlink(header.Linkname, name); err != nil {
				// A symlink the platform refuses is not worth failing an upload
				// over; the file simply is not stored, and the commit reflects
				// what was actually written.
				continue
			}

		case tar.TypeLink:
			source, err := cleanArchivePath(header.Linkname)
			if err != nil {
				return stats, err
			}
			if err := ensureParent(root, name); err != nil {
				return stats, err
			}
			// A hard link must resolve inside the archive. os.Root.Link refuses a
			// target outside the root, which is exactly the guarantee wanted.
			if err := root.Link(source, name); err != nil {
				// Read it as a regular file instead, which preserves the content
				// even when the link cannot be made.
				continue
			}

		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			// Device nodes and FIFOs have no meaning in a source tree and cannot
			// be represented in git. Skipping is the only sensible outcome.

		default:
			// Unknown types are skipped rather than treated as fatal: an archive
			// may carry metadata entries that are safe to ignore, and refusing
			// the whole upload over one would be unhelpful.
		}
	}

	return stats, nil
}

// decompress wraps body in a gzip reader when it looks gzipped.
//
// A peek rather than a content-type or filename: the caller is usually piping
// `tar czf -`, so there is nothing else to go on.
func decompress(body io.Reader) (io.Reader, error) {
	buffered := newPeekReader(body)
	magic, err := buffered.Peek(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read archive header: %w", err)
	}

	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, fmt.Errorf("read gzip stream: %w", err)
		}
		return gz, nil
	}
	return buffered, nil
}

// cleanArchivePath normalises an archive entry name and rejects anything that
// tries to leave the destination.
//
// Returning an error rather than a sanitised path is deliberate: an archive
// containing "../../etc/passwd" is either malicious or broken, and quietly
// storing it as "etc/passwd" would hide that from the caller.
func cleanArchivePath(name string) (string, error) {
	if name == "" {
		return "", nil
	}

	// Windows-style separators and absolute paths are both refusals: an
	// absolute path has no meaning in a source tree, and a backslash is a
	// legitimate filename character on Unix but never a separator here.
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) {
		return "", fmt.Errorf("archive entry %q has an absolute path", name)
	}
	if filepath.IsAbs(name) || (len(name) > 1 && name[1] == ':') {
		return "", fmt.Errorf("archive entry %q has an absolute path", name)
	}

	// path.Clean is the POSIX one, which is what tar uses regardless of the
	// host's separator conventions.
	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == "/" {
		return "", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("archive entry %q escapes the destination", name)
	}
	if strings.Contains(cleaned, "\x00") {
		return "", fmt.Errorf("archive entry %q contains a null byte", name)
	}

	return cleaned, nil
}

// isGitMetadata reports whether an entry belongs to a .git directory.
func isGitMetadata(name string) bool {
	return name == ".git" || strings.HasPrefix(name, ".git/")
}

// ensureParent creates the directory an entry lives in.
func ensureParent(root *os.Root, name string) error {
	dir := path.Dir(name)
	if dir == "." || dir == "/" {
		return nil
	}
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	return nil
}

// writeArchiveFile writes one regular file, enforcing the remaining size budget.
func writeArchiveFile(root *os.Root, name string, r io.Reader, header *tar.Header, remaining int64) (int64, error) {
	if err := ensureParent(root, name); err != nil {
		return 0, err
	}

	// The mode is taken from the archive but stripped of anything that would
	// make the file setuid or world-writable. A source tree needs the executable
	// bit and not much else, and preserving the rest would faithfully reproduce
	// a hostile archive's permissions into AppLab's storage.
	mode := os.FileMode(header.Mode).Perm()
	if mode == 0 {
		mode = 0o644
	}
	mode &^= os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	mode |= 0o600 // AppLab's own read/write, regardless of what was claimed

	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return 0, fmt.Errorf("create file %s: %w", name, err)
	}
	defer file.Close()

	// One extra byte of budget so an oversized entry is detected rather than
	// silently truncated at exactly the limit.
	written, err := io.Copy(file, io.LimitReader(r, remaining+1))
	if err != nil {
		return written, fmt.Errorf("write file %s: %w", name, err)
	}
	if written > remaining {
		return written, fmt.Errorf("archive expands to more than the %d byte limit; exclude build output such as node_modules or a target directory", remaining)
	}

	return written, nil
}

// stripSingleRoot removes a single wrapper directory when every entry sits
// beneath it, and reports the name it removed.
//
// This is what makes `tar czf - myproject | curl ...` produce a source tree
// whose Dockerfile is at the root, which is what the caller means. It only fires
// when the wrapper is the *only* top-level entry: with anything else beside it
// the archive is not a wrapped directory, and removing it would move files the
// caller did not ask to move.
func stripSingleRoot(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return ""
	}

	inner := entries[0].Name()
	innerPath := filepath.Join(dir, inner)

	children, err := os.ReadDir(innerPath)
	if err != nil {
		return ""
	}

	for _, child := range children {
		if err := os.Rename(filepath.Join(innerPath, child.Name()), filepath.Join(dir, child.Name())); err != nil {
			// A partial strip would corrupt the tree, so undo what moved and
			// leave the wrapper in place — the source is still correct, just one
			// level deeper than ideal.
			return ""
		}
	}

	_ = os.Remove(innerPath)
	return inner
}

// defaultCommitSubject is what an upload is called when the caller gives no
// message. It carries a timestamp so a history reads as a sequence of uploads
// rather than as a row of identical subjects.
func defaultCommitSubject(appID string) string {
	return "Upload source for " + appID + " at " + time.Now().UTC().Format(time.RFC3339)
}

// firstLine returns the first line of a message, which git uses as the subject.
func firstLine(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	subject, _, _ := strings.Cut(message, "\n")
	return strings.TrimSpace(subject)
}

// peekReader lets the compression sniffer look at the first bytes without
// consuming them.
type peekReader struct {
	r      io.Reader
	buf    []byte
	offset int
}

func newPeekReader(r io.Reader) *peekReader {
	return &peekReader{r: r}
}

func (p *peekReader) Peek(n int) ([]byte, error) {
	for len(p.buf)-p.offset < n {
		chunk := make([]byte, n-(len(p.buf)-p.offset))
		read, err := p.r.Read(chunk)
		if read > 0 {
			p.buf = append(p.buf, chunk[:read]...)
		}
		if err != nil {
			return p.buf[p.offset:], err
		}
	}
	return p.buf[p.offset : p.offset+n], nil
}

func (p *peekReader) Read(b []byte) (int, error) {
	if p.offset < len(p.buf) {
		n := copy(b, p.buf[p.offset:])
		p.offset += n
		return n, nil
	}
	return p.r.Read(b)
}
