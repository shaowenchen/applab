package store

import (
	"context"
	"time"

	"github.com/shaowenchen/applab/internal/model"
)

// CountBuildsByStatus counts builds by their status, across every app.
//
// Every build is read, so the cost grows with the platform's build history —
// which is why this is the overview's number and not something a request path
// asks for. A deployment that outgrows it would keep the counts in the app
// record instead; that is a change of layout, not of interface.
func (s *Store) CountBuildsByStatus(ctx context.Context) (map[model.BuildStatus]int, error) {
	apps, err := s.ListApps(ctx)
	if err != nil {
		return nil, err
	}

	counts := map[model.BuildStatus]int{}
	for _, app := range apps {
		builds, err := s.ListBuilds(ctx, app.ID, 0)
		if err != nil {
			return nil, err
		}
		for _, build := range builds {
			counts[build.Status]++
		}
	}
	return counts, nil
}

// ListRecentBuilds returns the most recent builds across every app, newest
// first.
//
// It is the cross-app counterpart to ListBuilds, which takes an app id. Reading
// every app's builds to take the newest few is more work than a query would be,
// and it is bounded by the history rather than by the apps: the build objects
// are small, and this is what the overview's panel and the console's landing
// view both need.
func (s *Store) ListRecentBuilds(ctx context.Context, limit int) ([]*model.Build, error) {
	if limit <= 0 {
		limit = 20
	}

	apps, err := s.ListApps(ctx)
	if err != nil {
		return nil, err
	}

	var all []*model.Build
	for _, app := range apps {
		// Only the newest few per app can matter to a platform-wide "recent"
		// list, so an app with thousands of builds does not have all of them
		// read.
		builds, err := s.ListBuilds(ctx, app.ID, limit)
		if err != nil {
			return nil, err
		}
		all = append(all, builds...)
	}

	sortNewestFirst(all,
		func(b *model.Build) string { return b.ID },
		func(b *model.Build) time.Time { return b.CreatedAt })

	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}
