package objectstore

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Local is an object store backed by a directory.
//
// It exists for development and for the tests. It is not a filesystem
// abstraction that AppLab uses in production — a deployment either points at S3
// or at this, and the difference is the whole point of the package: what runs
// against this directory runs against a bucket, because the interface offers
// nothing a bucket cannot do.
//
// Two things it deliberately does not reproduce. There is no eventual
// consistency, because a directory has none to reproduce. And there is no
// permission model: every key is readable, where a bucket's are not.
type Local struct {
	root string
}

// NewLocal returns a store rooted at a directory, creating it if needed.
func NewLocal(root string) (*Local, error) {
	if root == "" {
		return nil, fmt.Errorf("objectstore: no directory given")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("objectstore: create %s: %w", root, err)
	}
	return &Local{root: root}, nil
}

// String names the backend without naming a credential.
func (l *Local) String() string { return "local:" + l.root }

// path resolves a key to a path under the root, refusing anything that would
// escape it.
//
// Keys are built from app ids, which are caller-supplied, so this is the one
// place where a key becomes a path and the one place a traversal could happen.
// The check is on the resolved path rather than on the key's text: "a/../../b"
// and "a/%2e%2e/b" are the same attempt, and only the resolved form catches
// both.
func (l *Local) path(key string) (string, error) {
	clean := Key(key)
	if clean == "" {
		return "", fmt.Errorf("objectstore: empty key")
	}

	full := filepath.Join(l.root, filepath.FromSlash(clean))

	// filepath.Join has already resolved the ".." segments; what is left is to
	// check that the result is still inside the root.
	rel, err := filepath.Rel(l.root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("objectstore: key %q escapes the store", key)
	}
	return full, nil
}

func (l *Local) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	full, err := l.path(key)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotExist
		}
		return nil, fmt.Errorf("objectstore: read %s: %w", key, err)
	}
	return f, nil
}

func (l *Local) GetBytes(ctx context.Context, key string) ([]byte, error) {
	r, err := l.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func (l *Local) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	full, err := l.path(key)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("objectstore: create the parent of %s: %w", key, err)
	}

	// Written to a temporary and renamed, so a reader never sees a half-written
	// object. Object storage gives this for free — a PUT is atomic — and without
	// it here a concurrent read during a test would see a truncated file and the
	// difference would only ever show up under load.
	tmp, err := os.CreateTemp(filepath.Dir(full), ".tmp-*")
	if err != nil {
		return fmt.Errorf("objectstore: stage %s: %w", key, err)
	}
	tmpName := tmp.Name()

	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("objectstore: write %s: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("objectstore: write %s: %w", key, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("objectstore: write %s: %w", key, err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("objectstore: write %s: %w", key, err)
	}
	return nil
}

func (l *Local) PutBytes(ctx context.Context, key string, body []byte) error {
	return l.Put(ctx, key, strings.NewReader(string(body)), int64(len(body)))
}

func (l *Local) Delete(ctx context.Context, key string) error {
	full, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("objectstore: delete %s: %w", key, err)
	}
	return nil
}

func (l *Local) Exists(ctx context.Context, key string) (bool, error) {
	full, err := l.path(key)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(full); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("objectstore: stat %s: %w", key, err)
	}
	return true, nil
}

func (l *Local) List(ctx context.Context, prefix string) ([]Object, error) {
	base := filepath.Join(l.root, filepath.FromSlash(Key(prefix)))

	// A prefix that is itself a file — listing "apps/shop/app.json" — has no
	// children, and a caller asking for it gets an empty list rather than an
	// error, the same way a bucket would.
	info, err := os.Stat(base)
	if err != nil || !info.IsDir() {
		return nil, nil
	}

	var out []Object
	err = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// The temporary files Put stages are not objects.
		if strings.HasPrefix(d.Name(), ".tmp-") {
			return nil
		}

		entry, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(l.root, path)
		if err != nil {
			return err
		}
		out = append(out, Object{
			Key:  filepath.ToSlash(rel),
			Size: entry.Size(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("objectstore: list %s: %w", prefix, err)
	}

	// WalkDir is already lexical, but sorting states the guarantee rather than
	// inheriting it: S3 promises lexicographic order and callers rely on it.
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
