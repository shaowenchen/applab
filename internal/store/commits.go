package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shaowenchen/applab/internal/model"
)

// RecordCommit writes a commit into AppLab's index.
//
// It is an index, not the record: the commit itself is in the app's git
// repository, and this exists so a listing does not have to run git. Anything
// lost here can be rebuilt from the repository, which is why an upload logs a
// failure here rather than reporting it — the source was stored either way.
//
// It is idempotent: recording the same commit twice replaces the object with the
// same content, which is what makes it safe to call from a path that may already
// have been run.
func (s *Store) RecordCommit(ctx context.Context, c *model.Commit) error {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now()
	}
	return s.putJSON(ctx, commitKey(c.AppID, c.SHA), c)
}

// GetCommit reads one recorded commit.
func (s *Store) GetCommit(ctx context.Context, appID, sha string) (*model.Commit, error) {
	var commit model.Commit
	if err := s.getJSON(ctx, commitKey(appID, sha), &commit); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("store: commit %q in app %q: %w", sha, appID, ErrNotFound)
		}
		return nil, err
	}
	return &commit, nil
}

// ListCommits returns an app's recorded commits, newest first.
func (s *Store) ListCommits(ctx context.Context, appID string, limit int) ([]*model.Commit, error) {
	if limit <= 0 {
		limit = 50
	}

	var commits []*model.Commit
	err := s.listJSON(ctx, commitsPrefix(appID), func(body []byte) error {
		var commit model.Commit
		if err := decodeInto(body, &commit); err != nil {
			return err
		}
		commits = append(commits, &commit)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sortNewestFirst(commits,
		func(c *model.Commit) string { return c.SHA },
		func(c *model.Commit) time.Time { return c.CreatedAt })

	if len(commits) > limit {
		commits = commits[:limit]
	}
	return commits, nil
}

// CreateUpload records an in-progress chunked upload.
func (s *Store) CreateUpload(ctx context.Context, u *model.Upload) error {
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now()
	}
	return s.putJSON(ctx, uploadKey(u.ID), u)
}

// GetUpload reads an upload's descriptor.
func (s *Store) GetUpload(ctx context.Context, id string) (*model.Upload, error) {
	var upload model.Upload
	if err := s.getJSON(ctx, uploadKey(id), &upload); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("store: upload %q: %w", id, ErrNotFound)
		}
		return nil, err
	}
	return &upload, nil
}

// DeleteUpload removes an upload's descriptor.
//
// It claims the upload: two concurrent completions would otherwise both assemble
// and both commit, and the caller would get two commits for one logical upload.
// The parts themselves are not removed here — the parts container is deleted by
// the caller that owns the upload's directory — and an upload abandoned by a
// crash leaves parts that nothing refers to.
func (s *Store) DeleteUpload(ctx context.Context, id string) error {
	return s.objects.Delete(ctx, uploadKey(id))
}
