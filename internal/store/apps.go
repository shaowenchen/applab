package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/shaowenchen/applab/internal/model"
)

// ErrNotFound is returned when a lookup by primary key finds nothing.
//
// It is a sentinel so handlers can map it to a 404 without matching on a
// driver error string, which would be brittle across driver versions.
var ErrNotFound = errors.New("not found")

// ErrExists is returned when creating a record whose primary key is taken.
var ErrExists = errors.New("already exists")

// CreateApp inserts a new app.
//
// It returns ErrExists rather than upserting: a create that silently replaced
// an existing app would destroy that app's recorded history, and the caller's
// intent — "make me a new app" — is not served by overwriting one.
func (s *Store) CreateApp(ctx context.Context, app *model.App) error {
	now := time.Now().UTC()
	if app.CreatedAt.IsZero() {
		app.CreatedAt = now
	}
	app.UpdatedAt = now
	if app.Status == "" {
		app.Status = model.AppStatusCreated
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO apps (id, name, port, replicas, dockerfile, domain, commit_sha, image,
		                  status, status_reason, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		app.ID, app.Name, app.Port, app.Replicas, app.Dockerfile, app.Domain,
		app.CommitSHA, app.Image, app.Status, app.StatusReason,
		app.CreatedAt.Unix(), app.UpdatedAt.Unix(),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("app %q: %w", app.ID, ErrExists)
		}
		return fmt.Errorf("insert app %s: %w", app.ID, err)
	}
	return nil
}

// GetApp loads one app by id.
func (s *Store) GetApp(ctx context.Context, id string) (*model.App, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, port, replicas, dockerfile, domain, commit_sha, image,
		       status, status_reason, created_at, updated_at
		FROM apps WHERE id = ?`, id)

	app, err := scanApp(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("app %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get app %s: %w", id, err)
	}
	return app, nil
}

// ListApps returns every app, newest first.
//
// Deleted apps are included: their rows remain so an id is not quietly reused,
// and a caller that wants only live apps filters on Status. Excluding them here
// would make "why did my app vanish" unanswerable from the API.
func (s *Store) ListApps(ctx context.Context) ([]*model.App, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, port, replicas, dockerfile, domain, commit_sha, image,
		       status, status_reason, created_at, updated_at
		FROM apps ORDER BY created_at DESC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	defer rows.Close()

	var out []*model.App
	for rows.Next() {
		app, err := scanApp(rows)
		if err != nil {
			return nil, fmt.Errorf("scan app: %w", err)
		}
		out = append(out, app)
	}
	return out, rows.Err()
}

// UpdateApp writes the mutable fields of an app back.
//
// It is a full write of those fields rather than a patch: every caller has the
// app in hand and is setting it to a known state, so a column-by-column update
// would add surface without changing any outcome.
//
// created_at is deliberately not written: it is a fact about when the app came
// into existence, and no update changes it.
func (s *Store) UpdateApp(ctx context.Context, app *model.App) error {
	app.UpdatedAt = time.Now().UTC()

	res, err := s.db.ExecContext(ctx, `
		UPDATE apps SET
			name = ?, port = ?, replicas = ?, dockerfile = ?, domain = ?,
			commit_sha = ?, image = ?, status = ?, status_reason = ?, updated_at = ?
		WHERE id = ?`,
		app.Name, app.Port, app.Replicas, app.Dockerfile, app.Domain,
		app.CommitSHA, app.Image, app.Status, app.StatusReason,
		app.UpdatedAt.Unix(), app.ID,
	)
	if err != nil {
		return fmt.Errorf("update app %s: %w", app.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update app %s: %w", app.ID, err)
	}
	if n == 0 {
		return fmt.Errorf("app %q: %w", app.ID, ErrNotFound)
	}
	return nil
}

// SetAppStatus records the outcome of the most recent operation.
//
// It is separate from UpdateApp so that a pipeline stage can report progress
// without reading and rewriting the whole row — two concurrent writers each
// doing a full update would otherwise clobber one another's unrelated fields.
func (s *Store) SetAppStatus(ctx context.Context, id string, status model.AppStatus, reason string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE apps SET status = ?, status_reason = ?, updated_at = ? WHERE id = ?`,
		status, reason, time.Now().UTC().Unix(), id)
	if err != nil {
		return fmt.Errorf("set app %s status: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set app %s status: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("app %q: %w", id, ErrNotFound)
	}
	return nil
}

// SetAppDeployed records the commit and image now deployed.
func (s *Store) SetAppDeployed(ctx context.Context, id, commitSHA, image string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE apps SET commit_sha = ?, image = ?, updated_at = ? WHERE id = ?`,
		commitSHA, image, time.Now().UTC().Unix(), id)
	if err != nil {
		return fmt.Errorf("set app %s deployment: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set app %s deployment: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("app %q: %w", id, ErrNotFound)
	}
	return nil
}

// DeleteApp removes an app's row and every record that belongs to it, in one
// transaction.
//
// Unlike a deploy, which keeps a tombstone row, this is a real removal: the
// caller asked to be rid of the app. Foreign keys are off by default in SQLite
// for compatibility, so the dependent rows are deleted explicitly rather than
// relying on a cascade that may not be enforced.
func (s *Store) DeleteApp(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete app %s: %w", id, err)
	}
	defer tx.Rollback()

	for _, q := range []string{
		`DELETE FROM builds WHERE app_id = ?`,
		`DELETE FROM commits WHERE app_id = ?`,
		`DELETE FROM uploads WHERE app_id = ?`,
		`DELETE FROM apps WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return fmt.Errorf("delete app %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete app %s: %w", id, err)
	}
	return nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows, so one scan helper
// serves single-row and multi-row queries.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanApp(sc rowScanner) (*model.App, error) {
	var (
		app                  model.App
		createdAt, updatedAt int64
	)
	if err := sc.Scan(
		&app.ID, &app.Name, &app.Port, &app.Replicas, &app.Dockerfile, &app.Domain,
		&app.CommitSHA, &app.Image, &app.Status, &app.StatusReason,
		&createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	app.CreatedAt = time.Unix(createdAt, 0).UTC()
	app.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return &app, nil
}
