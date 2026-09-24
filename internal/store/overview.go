package store

import (
	"context"
	"fmt"

	"github.com/shaowenchen/applab/internal/model"
)

// This file holds the aggregate queries the dashboard needs.
//
// They live in the store rather than being assembled from ListApps and a loop
// over ListBuilds for two reasons. A count is a count: deriving "how many apps
// are failing" by loading every app and tallying in Go makes the answer depend
// on how many apps exist, which is exactly backwards for an overview page. And
// the build query is genuinely cross-app — the existing ListBuilds is per-app,
// so the one thing an overview must show, the recent builds of the whole
// platform, cannot be built from what was already there.

// CountAppsByStatus returns how many apps are in each status.
//
// Deleted apps are excluded. Their rows survive so an id is not quietly reused
// (see ListApps), but counting them would report an app the operator has already
// removed as if it still existed — a platform overview would then never show
// zero, which makes the number useless.
//
// A status with no apps is simply absent from the map rather than present as
// zero, so a caller can tell "none of these" from "this deployment has never
// used that status". Callers that want a fixed set of keys fill the gaps
// themselves, which is what the overview endpoint does.
func (s *Store) CountAppsByStatus(ctx context.Context) (map[model.AppStatus]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM apps WHERE status != ? GROUP BY status`,
		string(model.AppStatusDeleted))
	if err != nil {
		return nil, fmt.Errorf("count apps by status: %w", err)
	}
	defer rows.Close()

	counts := map[model.AppStatus]int{}
	for rows.Next() {
		var (
			status string
			n      int
		)
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("scan app status count: %w", err)
		}
		counts[model.AppStatus(status)] = n
	}
	return counts, rows.Err()
}

// CountBuildsByStatus returns how many builds are in each status, across every
// app.
func (s *Store) CountBuildsByStatus(ctx context.Context) (map[model.BuildStatus]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM builds GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("count builds by status: %w", err)
	}
	defer rows.Close()

	counts := map[model.BuildStatus]int{}
	for rows.Next() {
		var (
			status string
			n      int
		)
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("scan build status count: %w", err)
		}
		counts[model.BuildStatus(status)] = n
	}
	return counts, rows.Err()
}

// ListRecentBuilds returns the most recent builds across every app, newest
// first.
//
// It is the cross-app counterpart to ListBuilds, which takes an app id. The
// ordering matches that one — created_at descending, then id — so a page that
// shows both does not order them differently.
func (s *Store) ListRecentBuilds(ctx context.Context, limit int) ([]*model.Build, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+createBuildColumns+` FROM builds
		 ORDER BY created_at DESC, id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent builds: %w", err)
	}
	defer rows.Close()

	var out []*model.Build
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, fmt.Errorf("scan build: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
