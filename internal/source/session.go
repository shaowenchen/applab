package source

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// withRepo runs fn against an app's repository, materialised on local disk.
//
// This is the whole of the bridging between object storage and git: the
// repository is downloaded into scratch space, fn runs with a real filesystem to
// work against — which is what git needs — and whatever fn changed is uploaded
// again. Nothing is kept between calls.
//
// The download is the cost, and it is not avoidable: git has to be able to read
// every object it might walk, so any operation that resolves a ref has to have
// the objects that ref can reach. What is avoided is re-uploading the whole
// repository, which would make every push cost as much as the project's entire
// history. workingRepo tracks what changed and uploads only that.
//
// fn must not keep the path after it returns. It is removed, and the removal is
// not deferred past the upload: a failure leaves the scratch copy gone rather
// than lying around to be mistaken for state.
func (s *Store) withRepo(ctx context.Context, appID string, fn func(repoPath string) error) error {
	// Serialise per app. Two operations on one repository would each download a
	// copy, each change it, and each upload — and the second upload would be
	// last-write-wins over the whole set of changed files, so one of the two
	// commits would simply be gone. A lock per app is enough because the
	// repository is per app; there is no shared state between them.
	unlock := s.lockRepo(appID)
	defer unlock()

	repoPath, err := s.localRepoPath(appID)
	if err != nil {
		return err
	}

	repo := &workingRepo{
		store:  s.objects,
		prefix: s.repoPrefix(appID),
		dir:    repoPath,
		before: map[string]fileState{},
	}
	// A scratch copy left by an interrupted run is not the repository; the
	// bucket is. Starting from a clean directory is what makes that true.
	if err := os.RemoveAll(repoPath); err != nil {
		return fmt.Errorf("clear the working directory: %w", err)
	}
	if err := os.MkdirAll(repoPath, 0o700); err != nil {
		return fmt.Errorf("create the working directory: %w", err)
	}
	if err := repo.download(ctx); err != nil {
		_ = os.RemoveAll(repoPath)
		return err
	}

	// fn's failure is returned, but the upload still happens: a git command can
	// fail after having written objects — a commit built but a ref not yet moved,
	// or a push that was refused — and leaving those objects out of the bucket
	// would make the next download start from a state git no longer agrees with.
	runErr := fn(repoPath)

	uploadErr := repo.upload(ctx)
	_ = os.RemoveAll(repoPath)

	if runErr != nil {
		return runErr
	}
	return uploadErr
}

// uploadRepo uploads a repository that is already on local disk.
//
// It is for the one case that has no repository to download first: Create builds
// one from nothing. It takes the same lock, because a create racing an upload of
// the same app would otherwise interleave.
func (s *Store) uploadRepo(ctx context.Context, appID, repoPath string) error {
	unlock := s.lockRepo(appID)
	defer unlock()

	repo := &workingRepo{
		store:  s.objects,
		prefix: s.repoPrefix(appID),
		dir:    repoPath,
		before: map[string]fileState{},
	}
	return repo.upload(ctx)
}

// lockRepo serialises operations on one app's repository.
//
// It is a per-app mutex rather than a global one so that two apps uploading at
// once do not wait for each other: the expensive part of an operation is the
// download and the upload, and those are per app.
//
// It is a process-local lock, which is exactly as much as it can be: replicas do
// not share it, so two replicas pushing to one app at the same instant can still
// lose one of the two commits. Making that safe needs a lock object in the
// bucket and a lease protocol object storage does not offer — and the cost of
// getting it wrong is bounded, because a push is followed by a build and the
// build reads what git actually has. It is named here rather than left to be
// discovered.
func (s *Store) lockRepo(appID string) func() {
	s.locksMu.Lock()
	if s.locks == nil {
		s.locks = map[string]*repoLock{}
	}
	lock, ok := s.locks[appID]
	if !ok {
		lock = &repoLock{}
		s.locks[appID] = lock
	}
	s.locksMu.Unlock()

	lock.mu.Lock()
	return lock.mu.Unlock
}

// repoPathFor is the path a caller outside this package is given, for the one
// caller that drives git itself — the HTTP transport.
//
// It is not exposed as a long-lived path: the transport materialises a
// repository for the duration of one request and uploads it afterwards, through
// StartSession below.
func (s *Store) repoPathFor(appID string) (string, error) { return s.localRepoPath(appID) }

// Open materialises an app's repository and returns a function that uploads
// whatever changed.
//
// The git HTTP transport needs a path for the whole of a request rather than a
// callback — it hands the directory to git http-backend and streams the response
// — so this is the one place the path escapes for longer than one call. The
// caller must call finish exactly once, and must not use the path afterwards.
func (s *Store) Open(ctx context.Context, appID string) (repoPath string, finish func() error, err error) {
	unlock := s.lockRepo(appID)

	repoPath, err = s.localRepoPath(appID)
	if err != nil {
		unlock()
		return "", nil, err
	}

	repo := &workingRepo{
		store:  s.objects,
		prefix: s.repoPrefix(appID),
		dir:    repoPath,
		before: map[string]fileState{},
	}
	if err := os.RemoveAll(repoPath); err != nil {
		unlock()
		return "", nil, fmt.Errorf("clear the working directory: %w", err)
	}
	if err := os.MkdirAll(repoPath, 0o700); err != nil {
		unlock()
		return "", nil, fmt.Errorf("create the working directory: %w", err)
	}
	if err := repo.download(ctx); err != nil {
		_ = os.RemoveAll(repoPath)
		unlock()
		return "", nil, err
	}

	finished := false
	return repoPath, func() error {
		if finished {
			return nil
		}
		finished = true
		defer unlock()

		// The context is the request's, which is cancelled by the time a large
		// clone has finished writing its response. Uploading under it would fail
		// for a push that succeeded — git has already written the objects — so
		// what changed is uploaded on a context of its own.
		uploadErr := repo.upload(context.WithoutCancel(ctx))
		_ = os.RemoveAll(repoPath)
		return uploadErr
	}, nil
}

// ensureScratch creates the scratch directories this package uses.
func (s *Store) ensureScratch() error {
	for _, dir := range []string{s.tmpDir(), s.uploadsDir(), filepath.Join(s.tmpDir(), "repos")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}
