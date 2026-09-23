package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shaowenchen/applab/internal/model"
)

// isUniqueViolation reports whether err is a primary-key or unique-index
// conflict.
//
// The check is on the message because the pure-Go SQLite driver exposes no
// typed error code for it. It is confined to this one function so that a driver
// change has exactly one place to fix.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// RecordCommit stores a source commit.
//
// Re-recording the same sha is not an error: a retried upload resolves to the
// same commit, and the record it would write is the one already there.
func (s *Store) RecordCommit(ctx context.Context, c *model.Commit) error {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO commits (app_id, sha, message, author, files, bytes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (app_id, sha) DO UPDATE SET
			message = excluded.message,
			files   = excluded.files,
			bytes   = excluded.bytes`,
		c.AppID, c.SHA, c.Message, c.Author, c.Files, c.Bytes, c.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("record commit %s for app %s: %w", c.SHA, c.AppID, err)
	}
	return nil
}

// ListCommits returns an app's commits, newest first.
func (s *Store) ListCommits(ctx context.Context, appID string, limit int) ([]*model.Commit, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT app_id, sha, message, author, files, bytes, created_at
		FROM commits WHERE app_id = ?
		ORDER BY created_at DESC, sha ASC
		LIMIT ?`, appID, limit)
	if err != nil {
		return nil, fmt.Errorf("list commits for app %s: %w", appID, err)
	}
	defer rows.Close()

	var out []*model.Commit
	for rows.Next() {
		var (
			c         model.Commit
			createdAt int64
		)
		if err := rows.Scan(&c.AppID, &c.SHA, &c.Message, &c.Author, &c.Files, &c.Bytes, &createdAt); err != nil {
			return nil, fmt.Errorf("scan commit: %w", err)
		}
		c.CreatedAt = time.Unix(createdAt, 0).UTC()
		out = append(out, &c)
	}
	return out, rows.Err()
}

// GetCommit loads one commit record.
func (s *Store) GetCommit(ctx context.Context, appID, sha string) (*model.Commit, error) {
	var (
		c         model.Commit
		createdAt int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT app_id, sha, message, author, files, bytes, created_at
		FROM commits WHERE app_id = ? AND sha = ?`, appID, sha).
		Scan(&c.AppID, &c.SHA, &c.Message, &c.Author, &c.Files, &c.Bytes, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("commit %s of app %q: %w", sha, appID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get commit %s of app %s: %w", sha, appID, err)
	}
	c.CreatedAt = time.Unix(createdAt, 0).UTC()
	return &c, nil
}

// CreateUpload records a chunked upload session.
func (s *Store) CreateUpload(ctx context.Context, u *model.Upload) error {
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO uploads (id, app_id, total, chunk_size, message, author, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.AppID, u.Total, u.ChunkSize, u.Message, u.Author, u.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("create upload %s: %w", u.ID, err)
	}
	return nil
}

// GetUpload loads an upload session.
func (s *Store) GetUpload(ctx context.Context, id string) (*model.Upload, error) {
	var (
		u         model.Upload
		createdAt int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, app_id, total, chunk_size, message, author, created_at
		FROM uploads WHERE id = ?`, id).
		Scan(&u.ID, &u.AppID, &u.Total, &u.ChunkSize, &u.Message, &u.Author, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("upload %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get upload %s: %w", id, err)
	}
	u.CreatedAt = time.Unix(createdAt, 0).UTC()
	return &u, nil
}

// DeleteUpload removes an upload session once its parts have been assembled.
func (s *Store) DeleteUpload(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM uploads WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete upload %s: %w", id, err)
	}
	return nil
}
