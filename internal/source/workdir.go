package source

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/objectstore"
	"github.com/shaowenchen/applab/internal/store"
)

// workingRepo is an app's repository, materialised on local disk for as long as
// a git command needs it.
//
// Git cannot run against object storage: init, commit-tree, update-ref, archive
// and http-backend all need a real filesystem, with the rename semantics, the
// locking and the random access an object store does not have. So the repository
// lives in a bucket and is copied out to a scratch directory, operated on, and
// copied back.
//
// # What is copied back
//
// Only what changed. The state of every file is recorded after the download, and
// the upload compares: a file that is new, a file whose size moved, a file whose
// modification time moved, and a file that was deleted. For the common operation
// — one upload, which adds one commit's objects and moves one ref — that is a
// handful of small objects rather than the whole repository.
//
// That is not merely an optimisation. A repository's object store grows without
// bound, and re-uploading all of it on every push would make the cost of a push
// proportional to the project's entire history. What it costs instead is the
// download, which is unavoidable: git has to be able to read every object it
// might walk.
//
// # What it is not
//
// It is not a cache. Nothing is kept between operations, because keeping it would
// mean knowing when another replica had changed the same repository, and there is
// no coordination to know that with. Every operation starts from what the bucket
// holds and leaves the bucket as the only copy.
type workingRepo struct {
	store  objectstore.Store
	prefix string
	dir    string

	// before is each file's state as it was downloaded, keyed by its path
	// relative to the repository root.
	before map[string]fileState

	// uploads counts the objects written back, for the log line that says what
	// an operation cost.
	uploads int
}

// fileState is enough of a file to tell whether git touched it.
//
// Size and modification time rather than a content hash: hashing every object in
// a repository after every operation would read the whole thing, which is the
// cost this is trying to avoid. A file git rewrote to the same bytes with a new
// timestamp is re-uploaded, which is harmless — the object is the same.
type fileState struct {
	size    int64
	modTime int64
}

// openWorkingRepo materialises one branch of an app's repository into a scratch
// directory.
//
// A repository that does not exist yet comes back as an empty directory rather
// than as an error: creating the repository is one of the things this is used
// for, and a caller that has to distinguish "not there yet" from "cannot read"
// everywhere would get it wrong somewhere.
//
// The caller must Close it. Close uploads what changed, so a working repo that
// is opened and not closed loses every write git made.
func (s *Store) openWorkingRepo(ctx context.Context, appID, branch string) (*workingRepo, error) {
	if err := validateAppID(appID); err != nil {
		return nil, err
	}
	if err := model.ValidateBranchName(branch); err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp(s.tmpDir(), "repo-*")
	if err != nil {
		return nil, fmt.Errorf("create a working directory: %w", err)
	}

	repo := &workingRepo{
		store:  s.objects,
		prefix: s.branchPrefix(appID, branch),
		dir:    dir,
		before: map[string]fileState{},
	}

	if err := repo.download(ctx); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return repo, nil
}

// repoPrefix is where one app's repository lives in the bucket.
//
// It is built here rather than by the store package so that what is inside a
// repository is this package's business: the store knows an app has a directory
// for its source, and this package knows what a source directory contains.
//
// The directory is "repo" and holds one bare git repository, not one per branch.
// A repository is git's own storage format, and what is inside it — refs, the
// object database, the ref log — is not a set of files that can be split by
// branch without splitting git itself. See the package comment for what that
// costs and the branch handling in commit.go for how branches are kept apart
// within it.
func (s *Store) branchPrefix(appID, branch string) string {
	return store.BranchPrefix(appID, branch)
}

// branchesPrefix is every branch of one app, for a caller that wants them all.
func (s *Store) branchesPrefix(appID string) string {
	return store.BranchesPrefix(appID)
}

// repoPrefixForRemove is the whole of an app's source, which is what removing an
// app deletes.
func (s *Store) repoPrefixForRemove(appID string) string {
	return store.SourcePrefix(appID)
}

// download copies the repository out of the bucket.
func (w *workingRepo) download(ctx context.Context) error {
	objects, err := w.store.List(ctx, w.prefix)
	if err != nil {
		return fmt.Errorf("list the repository: %w", err)
	}

	for _, object := range objects {
		rel := strings.TrimPrefix(object.Key, w.prefix+"/")
		if rel == "" || rel == object.Key {
			continue
		}

		target := filepath.Join(w.dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(target), err)
		}

		body, err := w.store.GetBytes(ctx, object.Key)
		if err != nil {
			return fmt.Errorf("read %s: %w", object.Key, err)
		}

		// Git writes some of its files with the executable bit and reads it back
		// — hooks are the case that matters, and a repository with a hook that
		// lost its bit would silently stop running it. Everything else in a bare
		// repository is data, so the bit is restored from a marker in the name
		// rather than being stored, which keeps this as one object per file.
		mode := os.FileMode(0o600)
		if strings.HasSuffix(rel, ".exec") {
			mode = 0o700
			rel = strings.TrimSuffix(rel, ".exec")
			target = filepath.Join(w.dir, filepath.FromSlash(rel))
		}

		if err := os.WriteFile(target, body, mode); err != nil {
			return fmt.Errorf("write %s: %w", target, err)
		}
		if err := os.Chmod(target, mode); err != nil {
			return fmt.Errorf("set the mode on %s: %w", target, err)
		}

		info, err := os.Stat(target)
		if err != nil {
			return fmt.Errorf("stat %s: %w", target, err)
		}
		w.before[rel] = fileState{size: info.Size(), modTime: info.ModTime().UnixNano()}
	}
	return nil
}

// upload writes back everything git changed and removes what it deleted.
func (w *workingRepo) upload(ctx context.Context) error {
	now := map[string]fileState{}
	err := filepath.WalkDir(w.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(w.dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		info, err := d.Info()
		if err != nil {
			return err
		}
		now[rel] = fileState{size: info.Size(), modTime: info.ModTime().UnixNano()}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk the working repository: %w", err)
	}

	// What git removed. A ref deleted, or a loose object repacked: the objects
	// that were there and are not any more have to go from the bucket too, or
	// the next download would put them back.
	var deleted []string
	for rel := range w.before {
		if _, ok := now[rel]; !ok {
			deleted = append(deleted, rel)
		}
	}
	// Sorted so a failure part-way leaves a deterministic prefix done, which is
	// what makes a retry idempotent rather than differently incomplete.
	sort.Strings(deleted)

	for _, rel := range deleted {
		if err := w.store.Delete(ctx, w.key(rel)); err != nil {
			return fmt.Errorf("delete %s: %w", rel, err)
		}
	}

	// What git wrote or changed, in a deterministic order for the same reason.
	var changed []string
	for rel, state := range now {
		if was, ok := w.before[rel]; ok && was == state {
			continue
		}
		changed = append(changed, rel)
	}
	sort.Strings(changed)

	for _, rel := range changed {
		path := filepath.Join(w.dir, filepath.FromSlash(rel))

		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat %s: %w", rel, err)
		}

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", rel, err)
		}

		putErr := w.store.Put(ctx, w.key(rel), f, info.Size())
		f.Close()
		if putErr != nil {
			return fmt.Errorf("write %s: %w", rel, putErr)
		}
		w.uploads++
	}
	return nil
}

// key is the bucket key for a path within the repository.
//
// The executable-bit marker is appended here and stripped on the way in, so the
// two are in one place and cannot drift.
func (w *workingRepo) key(rel string) string {
	rel = filepath.ToSlash(rel)

	// Only a file git would run — anything under a hooks directory — has its bit
	// recorded. Marking everything would double the key count for nothing, and a
	// bare repository has a handful of files worth running at most.
	if strings.HasPrefix(rel, "hooks/") || strings.HasPrefix(rel, "custom-hooks/") {
		if info, err := os.Stat(filepath.Join(w.dir, filepath.FromSlash(rel))); err == nil && info.Mode()&0o100 != 0 {
			return objectstore.Key(w.prefix, rel+".exec")
		}
	}
	return objectstore.Key(w.prefix, rel)
}

// Close uploads what changed and removes the working directory.
//
// The upload happens here rather than at each call site because every caller has
// to close, and a caller that forgot to upload would produce a repository that
// looked correct on disk and was empty in the bucket. Close is idempotent: a
// second call does nothing, so a caller that closes twice — or closes in a
// deferred call after an explicit one — is not an error.
func (w *workingRepo) Close(ctx context.Context) error {
	if w.dir == "" {
		return nil
	}
	dir := w.dir
	w.dir = ""

	uploadErr := w.upload(ctx)
	removeErr := os.RemoveAll(dir)

	if uploadErr != nil {
		return uploadErr
	}
	if removeErr != nil {
		return fmt.Errorf("remove the working directory: %w", removeErr)
	}
	return nil
}

// discard removes the working directory without uploading. It is what a caller
// wants when it is abandoning the operation rather than completing it.
func (w *workingRepo) discard() {
	if w.dir == "" {
		return
	}
	dir := w.dir
	w.dir = ""
	_ = os.RemoveAll(dir)
}

// existsInBucket reports whether an app has a repository at all.
//
// It is a listing of one prefix rather than a marker object, because a repository
// that exists and is empty — created but never pushed to — is a real state that a
// marker would have to be kept in step with.
func (s *Store) existsInBucket(ctx context.Context, appID, branch string) (bool, error) {
	objects, err := s.objects.List(ctx, s.branchPrefix(appID, branch))
	if err != nil {
		return false, fmt.Errorf("list the %s repository for %s: %w", branch, appID, err)
	}
	return len(objects) > 0, nil
}

// validateAppID is the check this package makes before an id becomes a path or a
// key segment.
var validateAppID = func(appID string) error {
	if appID == "" {
		return errors.New("empty app id")
	}
	for _, r := range appID {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
		default:
			return fmt.Errorf("app id %q contains %q, which is not allowed", appID, r)
		}
	}
	if strings.Contains(appID, "..") {
		return fmt.Errorf("app id %q contains %q, which is not allowed", appID, "..")
	}
	return nil
}
