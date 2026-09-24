package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shaowenchen/applab/internal/model"
)

// CreateBuild records a new build attempt.
func (s *Store) CreateBuild(ctx context.Context, b *model.Build) error {
	if b.CreatedAt.IsZero() {
		b.CreatedAt = now()
	}
	if b.Status == "" {
		b.Status = model.BuildStatusPending
	}
	if err := s.putJSON(ctx, buildKey(b.AppID, b.ID), b); err != nil {
		return err
	}
	return nil
}

// GetBuild loads one build by id.
//
// A build is addressed under its app in every URL, and this is the only way to
// find it: the layout is per app, so the app id is part of the key and a build
// cannot be looked up by its own id alone. That is why the caller here has to
// name the app — which is also what makes guessing a build id insufficient to
// read someone else's.
func (s *Store) GetBuild(ctx context.Context, appID, id string) (*model.Build, error) {
	var build model.Build
	if err := s.getJSON(ctx, buildKey(appID, id), &build); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("store: build %q: %w", id, ErrNotFound)
		}
		return nil, err
	}
	return &build, nil
}

// ListBuilds returns an app's builds, newest first.
func (s *Store) ListBuilds(ctx context.Context, appID string, limit int) ([]*model.Build, error) {
	if limit <= 0 {
		limit = 50
	}

	var builds []*model.Build
	err := s.listJSON(ctx, buildsPrefix(appID), func(body []byte) error {
		var build model.Build
		if err := decodeInto(body, &build); err != nil {
			return err
		}
		builds = append(builds, &build)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sortNewestFirst(builds,
		func(b *model.Build) string { return b.ID },
		func(b *model.Build) time.Time { return b.CreatedAt })

	if len(builds) > limit {
		builds = builds[:limit]
	}
	return builds, nil
}

// FindSucceededBuild returns the most recent successful build of a commit.
//
// A rollback uses this to redeploy an image that already exists instead of
// rebuilding a byte-identical one. Only succeeded builds with a recorded image
// qualify: anything else has nothing to deploy.
func (s *Store) FindSucceededBuild(ctx context.Context, appID, sha string) (*model.Build, error) {
	builds, err := s.ListBuilds(ctx, appID, 0)
	if err != nil {
		return nil, err
	}

	// Newest first, so the first match is the most recent one.
	for _, build := range builds {
		if build.CommitSHA == sha && build.Status == model.BuildStatusSucceeded && build.Image != "" {
			return build, nil
		}
	}
	return nil, fmt.Errorf("store: no succeeded build of %s in app %q: %w", sha, appID, ErrNotFound)
}

// SetBuildStatus updates a build's status and reason.
//
// The timestamps are set from the transition rather than passed in, so a build
// cannot end up finished before it started: reaching "running" records the
// start once, and reaching a terminal status records the finish.
func (s *Store) SetBuildStatus(ctx context.Context, appID, id string, status model.BuildStatus, reason string) error {
	build, err := s.GetBuild(ctx, appID, id)
	if err != nil {
		return err
	}

	build.Status = status
	build.Reason = reason

	at := now()
	switch {
	case status == model.BuildStatusRunning && build.StartedAt.IsZero():
		build.StartedAt = at
	case status.Terminal() && build.FinishedAt.IsZero():
		build.FinishedAt = at
	}

	return s.putJSON(ctx, buildKey(appID, id), build)
}

// SetBuildImage records the image a build produced, once the build has pushed
// it and reported the digest back.
func (s *Store) SetBuildImage(ctx context.Context, appID, id, image string) error {
	build, err := s.GetBuild(ctx, appID, id)
	if err != nil {
		return err
	}
	build.Image = image
	return s.putJSON(ctx, buildKey(appID, id), build)
}

// ListUnfinishedBuilds returns every build on the platform that was still
// pending or running the last time AppLab looked.
//
// It exists for recovery at startup: if the process restarts while a build is in
// flight, nothing is left to notice the Job finished, so those builds are
// reconciled against the cluster once the server is back.
//
// It reads one listing pair per app rather than one listing overall, because the
// layout is per app: a build's app is part of its key, so a platform-wide query
// for builds is a query per app. That is the cost of the layout, and it is paid
// here — once, at startup — rather than anywhere in the request path.
func (s *Store) ListUnfinishedBuilds(ctx context.Context) ([]*model.Build, error) {
	apps, err := s.ListApps(ctx)
	if err != nil {
		return nil, err
	}

	var out []*model.Build
	for _, app := range apps {
		unfinished, err := s.ListUnfinishedBuildsForApp(ctx, app.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, unfinished...)
	}
	return out, nil
}

// ListUnfinishedBuildsForApp returns one app's builds that are still pending or
// running, oldest first.
//
// It exists so that a new upload can supersede the build already in flight: the
// upload that arrived later is the one whose source someone is waiting to see,
// and a Job left running for the earlier commit would push an image for a
// revision that is no longer the tip.
func (s *Store) ListUnfinishedBuildsForApp(ctx context.Context, appID string) ([]*model.Build, error) {
	builds, err := s.ListBuilds(ctx, appID, 0)
	if err != nil {
		return nil, err
	}

	var out []*model.Build
	for _, build := range builds {
		if !build.Status.Terminal() {
			out = append(out, build)
		}
	}

	// Oldest first, which is the order the caller stops them in.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// buildIDFromKey recovers a build's id from its object key.
func buildIDFromKey(key string) string {
	name := key[strings.LastIndex(key, "/")+1:]
	return strings.TrimSuffix(name, ".json")
}
