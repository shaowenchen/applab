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
	"sync"

	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/objectstore"
	"github.com/shaowenchen/applab/internal/store"
)

// downloadConcurrency bounds how many object reads are in flight at once.
//
// It is a bound rather than a target: the point is to stop waiting on one
// round trip at a time, and past a few dozen the bottleneck moves to the
// service's own limits, the connection pool and the network. A number in that
// range gets nearly all of the available speedup without turning a clone into a
// burst a rate limiter will answer with throttling.
const downloadConcurrency = 32

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

	// pack collapses loose objects into a packfile before the upload walk, when
	// there are enough of them to be worth it. See Store.packRepo.
	//
	// Nil means no packing, which is what a caller that is not a source Store —
	// a test — gets. It is a field rather than a Store method on workingRepo so
	// that this file, which is about moving bytes to and from a bucket, does not
	// have to know how to run git.
	pack func(ctx context.Context, repoPath string) error

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
		pack:   s.packRepo,
	}

	if err := repo.download(ctx); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return repo, nil
}

// branchPrefix is where one app's repository for one branch lives in the bucket.
//
// It is built here rather than by the store package so that what is inside a
// repository is this package's business: the store knows an app has a directory
// for its source, and this package knows what a source directory contains.
//
// One bare repository per branch. A repository is git's own storage format, and
// what is inside it — refs, the object database, the ref log — is not a set of
// files that can be split by branch without splitting git itself, so two branches
// of one app cannot share an object database here without sharing a repository.
// The package comment states what that costs.
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
//
// The reads run concurrently, and that is the difference between a clone taking
// seconds and taking minutes. A repository is many small objects — a hundred
// commits of a modest project is thousands — and one read per object over a
// bucket's per-request latency is minutes of waiting for 700 KB of data. The
// work is entirely round-trip bound: nothing here is CPU or bandwidth limited at
// this size, so the only lever that matters is how many requests are in flight.
//
// The concurrency is bounded. An unbounded fan-out over a large repository would
// open thousands of connections to a service that is rate limiting and connection
// limiting on its side, and the result would be throttling rather than speed.
func (w *workingRepo) download(ctx context.Context) error {
	objects, err := w.store.List(ctx, w.prefix)
	if err != nil {
		return fmt.Errorf("list the repository: %w", err)
	}

	// A failure stops the work rather than merely being reported. Returning from
	// the loop below while fetches are still running would leave them blocked on
	// a channel nobody is reading and holding the semaphore, so the collector
	// cancels first and keeps draining until the senders are done.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Each object is written by one goroutine; nothing is shared but the map
	// below, which only the collector touches.
	type fetched struct {
		name  string
		state fileState
		err   error
	}
	results := make(chan fetched)
	sem := make(chan struct{}, downloadConcurrency)

	go func() {
		defer close(results)
		var wg sync.WaitGroup
		for _, object := range objects {
			rel := strings.TrimPrefix(object.Key, w.prefix+"/")
			if rel == "" || rel == object.Key {
				continue
			}

			// Checked before blocking on the semaphore, so a cancelled download
			// stops starting work instead of queueing all of it.
			if ctx.Err() != nil {
				break
			}

			wg.Add(1)
			sem <- struct{}{}
			go func(key, rel string) {
				defer wg.Done()
				defer func() { <-sem }()
				name, state, err := w.fetch(ctx, key, rel)
				results <- fetched{name: name, state: state, err: err}
			}(object.Key, rel)
		}
		wg.Wait()
	}()

	var firstErr error
	for result := range results {
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
				cancel()
			}
			continue
		}
		w.before[result.name] = result.state
	}
	return firstErr
}

// fetch writes one object into the working directory and reports what it wrote,
// under the name git will see it by.
//
// The name it returns is not always the key it was given: the marker that records
// the executable bit is part of the key and not part of the filename, so the two
// differ for a hook. Returning the filename is what keeps the download and the
// upload walk keyed the same way — keying the download by the object key made
// every hook look like a file that had been deleted the moment it was written,
// so each operation removed and re-uploaded all of them.
func (w *workingRepo) fetch(ctx context.Context, key, rel string) (string, fileState, error) {
	target := filepath.Join(w.dir, filepath.FromSlash(rel))

	// Git writes some of its files with the executable bit and reads it back —
	// hooks are the case that matters, and a repository with a hook that lost its
	// bit would silently stop running it. Everything else in a bare repository is
	// data, so the bit is restored from a marker in the name rather than being
	// stored, which keeps this as one object per file.
	mode := os.FileMode(0o600)
	name := rel
	if strings.HasSuffix(name, ".exec") {
		mode = 0o700
		name = strings.TrimSuffix(name, ".exec")
		target = filepath.Join(w.dir, filepath.FromSlash(name))
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", fileState{}, fmt.Errorf("create %s: %w", filepath.Dir(target), err)
	}

	body, err := w.store.GetBytes(ctx, key)
	if err != nil {
		return "", fileState{}, fmt.Errorf("read %s: %w", key, err)
	}
	if err := os.WriteFile(target, body, mode); err != nil {
		return "", fileState{}, fmt.Errorf("write %s: %w", target, err)
	}
	if err := os.Chmod(target, mode); err != nil {
		return "", fileState{}, fmt.Errorf("set the mode on %s: %w", target, err)
	}

	info, err := os.Stat(target)
	if err != nil {
		return "", fileState{}, fmt.Errorf("stat %s: %w", target, err)
	}
	return name, fileState{size: info.Size(), modTime: info.ModTime().UnixNano()}, nil
}

// repackThreshold is how many loose objects a repository may hold before it is
// packed.
//
// Packing is what makes a clone fast, and the threshold is what keeps a push
// cheap, because the two pull in opposite directions:
//
//   - A repository of loose objects costs one read per object to download. A
//     hundred commits of a modest project is thousands of loose objects, which
//     over a bucket's per-request latency is minutes of waiting for a few
//     hundred kilobytes. One packfile is a handful of reads instead.
//   - Packing rewrites the pack, so the pack's bytes change and the upload sends
//     all of it. Doing that on every push would make each push upload the whole
//     repository, where leaving the new objects loose uploads only what the push
//     actually added.
//
// So objects accumulate loose — pushes stay incremental — until there are enough
// of them to be worth collapsing, and then one pack replaces them. It is git's
// own arrangement; the threshold is far below git's default because the cost it
// trades against is a round trip rather than a disk seek.
const repackThreshold = 256

// packRepo collapses loose objects into a packfile, if there are enough to be
// worth it.
//
// It runs after a write and before the upload walk, so the files it rewrites are
// the files the walk records and uploads. That ordering is the whole reason this
// is safe: the reason gc.auto is disabled (see applyDeterministicConfig) is that
// a repack landing *during* the walk moves files out from under it. Run here, it
// has finished before the walk starts.
//
// It also means a clone repacks a repository that is still full of loose objects
// — which is every repository written before this existed. That is deliberate:
// the pack it produces is uploaded on the same request, so one clone pays for
// the whole repository and every clone after it is fast. A read path that writes
// is unusual, and it is the only way an existing repository ever gets packed,
// because nothing else visits it.
func (s *Store) packRepo(ctx context.Context, repoPath string) error {
	loose, err := countLooseObjects(repoPath)
	if err != nil {
		return err
	}
	if loose < repackThreshold {
		return nil
	}

	// -a and -d together: every reachable object goes into one pack, and the
	// loose objects it replaced are removed. Without -a git would add a second
	// pack and keep the first, so the file count would grow with every push
	// rather than staying flat; without -d the loose files would stay behind and
	// still be downloaded.
	if _, err := s.run(ctx, repoPath, "repack", "-a", "-d", "-q"); err != nil {
		return fmt.Errorf("pack the repository: %w", err)
	}
	return nil
}

// countLooseObjects counts the objects git has not packed yet.
//
// Loose objects live two hex digits deep — objects/ab/cdef... — so the count is
// the number of files under those directories. objects/pack and objects/info are
// skipped: the first holds the packs, which are what this is deciding whether to
// make, and the second holds no objects.
func countLooseObjects(repoPath string) (int, error) {
	objectsDir := filepath.Join(repoPath, "objects")
	entries, err := os.ReadDir(objectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			// A repository that has never had an object written to it. Nothing to
			// pack, and nothing wrong.
			return 0, nil
		}
		return 0, fmt.Errorf("read %s: %w", objectsDir, err)
	}

	loose := 0
	for _, entry := range entries {
		// A loose object's directory is exactly two hex digits. Anything else is
		// pack, info, or a file git keeps at the top level.
		if !entry.IsDir() || len(entry.Name()) != 2 {
			continue
		}
		files, err := os.ReadDir(filepath.Join(objectsDir, entry.Name()))
		if err != nil {
			return 0, fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		loose += len(files)
	}
	return loose, nil
}

// upload writes back everything git changed and removes what it deleted.
func (w *workingRepo) upload(ctx context.Context) error {
	// Before the walk, so what the walk sees is what is uploaded.
	if w.pack != nil {
		if err := w.pack(ctx, w.dir); err != nil {
			return err
		}
	}

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
