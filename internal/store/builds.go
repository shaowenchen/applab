package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/shaowenchen/applab/internal/model"
)

// createBuildColumns is the column list every build query selects, kept in one
// place so a scan helper and its query cannot drift apart.
const createBuildColumns = `id, app_id, commit_sha, image, job_name, status, reason, created_at, started_at, finished_at`

// CreateBuild records a new build attempt.
func (s *Store) CreateBuild(ctx context.Context, b *model.Build) error {
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now().UTC()
	}
	if b.Status == "" {
		b.Status = model.BuildStatusPending
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO builds (id, app_id, commit_sha, image, job_name, status, reason, created_at, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.AppID, b.CommitSHA, b.Image, b.JobName, b.Status, b.Reason,
		b.CreatedAt.Unix(), unixOrZero(b.StartedAt), unixOrZero(b.FinishedAt))
	if err != nil {
		return fmt.Errorf("create build %s: %w", b.ID, err)
	}
	return nil
}

// GetBuild loads one build by id.
func (s *Store) GetBuild(ctx context.Context, id string) (*model.Build, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+createBuildColumns+` FROM builds WHERE id = ?`, id)

	b, err := scanBuild(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("build %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get build %s: %w", id, err)
	}
	return b, nil
}

// ListBuilds returns an app's builds, newest first.
func (s *Store) ListBuilds(ctx context.Context, appID string, limit int) ([]*model.Build, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+createBuildColumns+` FROM builds
		 WHERE app_id = ? ORDER BY created_at DESC, id ASC LIMIT ?`, appID, limit)
	if err != nil {
		return nil, fmt.Errorf("list builds for app %s: %w", appID, err)
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

// FindSucceededBuild returns the most recent successful build of a commit.
//
// A rollback uses this to redeploy an image that already exists instead of
// rebuilding a byte-identical one. Only succeeded builds with a recorded image
// qualify: anything else has nothing to deploy.
func (s *Store) FindSucceededBuild(ctx context.Context, appID, sha string) (*model.Build, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+createBuildColumns+` FROM builds
		 WHERE app_id = ? AND commit_sha = ? AND status = ? AND image != ''
		 ORDER BY created_at DESC LIMIT 1`,
		appID, sha, string(model.BuildStatusSucceeded))

	b, err := scanBuild(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("no successful build of commit %s for app %q: %w", sha, appID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("find build of commit %s for app %s: %w", sha, appID, err)
	}
	return b, nil
}

// SetBuildStatus updates a build's status and reason.
//
// The timestamps are set from the transition rather than passed in, so a build
// cannot end up finished before it started: reaching "running" records the
// start once, and reaching a terminal status records the finish.
func (s *Store) SetBuildStatus(ctx context.Context, id string, status model.BuildStatus, reason string) error {
	now := time.Now().UTC().Unix()

	var q string
	switch {
	case status == model.BuildStatusRunning:
		// COALESCE keeps the first start time if the build was seen as running
		// more than once, which is what a status polled against the cluster
		// will do.
		q = `UPDATE builds SET status = ?, reason = ?, started_at = COALESCE(NULLIF(started_at, 0), ?) WHERE id = ?`
	case status.Terminal():
		q = `UPDATE builds SET status = ?, reason = ?, finished_at = ? WHERE id = ?`
	default:
		q = `UPDATE builds SET status = ?, reason = ? WHERE id = ?`
	}

	var (
		res sql.Result
		err error
	)
	if status == model.BuildStatusRunning || status.Terminal() {
		res, err = s.db.ExecContext(ctx, q, string(status), reason, now, id)
	} else {
		res, err = s.db.ExecContext(ctx, q, string(status), reason, id)
	}
	if err != nil {
		return fmt.Errorf("set build %s status: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set build %s status: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("build %q: %w", id, ErrNotFound)
	}
	return nil
}

// SetBuildImage records the image a build produced, once the build has pushed
// it and reported the digest back.
func (s *Store) SetBuildImage(ctx context.Context, id, image string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE builds SET image = ? WHERE id = ?`, image, id)
	if err != nil {
		return fmt.Errorf("set build %s image: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set build %s image: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("build %q: %w", id, ErrNotFound)
	}
	return nil
}

// ListUnfinishedBuilds returns builds that were still pending or running the
// last time AppLab looked.
//
// It exists for recovery at startup: if the process restarts while a build is
// in flight, nothing is left to notice the Job finished, so those builds are
// reconciled against the cluster once the server is back.
func (s *Store) ListUnfinishedBuilds(ctx context.Context) ([]*model.Build, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+createBuildColumns+` FROM builds
		 WHERE status IN (?, ?) ORDER BY created_at ASC`,
		string(model.BuildStatusPending), string(model.BuildStatusRunning))
	if err != nil {
		return nil, fmt.Errorf("list unfinished builds: %w", err)
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

func scanBuild(sc rowScanner) (*model.Build, error) {
	var (
		b                            model.Build
		createdAt, startedAt, finish int64
	)
	if err := sc.Scan(
		&b.ID, &b.AppID, &b.CommitSHA, &b.Image, &b.JobName, &b.Status, &b.Reason,
		&createdAt, &startedAt, &finish,
	); err != nil {
		return nil, err
	}
	b.CreatedAt = time.Unix(createdAt, 0).UTC()
	b.StartedAt = timeFromUnix(startedAt)
	b.FinishedAt = timeFromUnix(finish)
	return &b, nil
}

// unixOrZero renders a timestamp for storage, mapping the zero time to 0 so a
// NULL is never needed for "has not happened yet".
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// timeFromUnix is the inverse, returning the zero time for 0.
func timeFromUnix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}
