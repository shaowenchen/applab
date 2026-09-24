package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/objectstore"
)

// CreateApp writes a new app.
//
// It refuses an id that already exists rather than replacing what is there: a
// create that silently overwrote an app would take its source, its history and
// its address with it, and the caller would have no way to tell that from a
// successful create of an empty app.
//
// The check and the write are not atomic. Two creates of one id at the same
// instant can both pass the check, and one of them then wins. That window is
// accepted: creating the same app twice concurrently is not a thing that
// happens in practice, and closing it would need a conditional write that not
// every S3-compatible service implements.
func (s *Store) CreateApp(ctx context.Context, app *model.App) error {
	key := appKey(app.ID)

	exists, err := s.objects.Exists(ctx, key)
	if err != nil {
		return fmt.Errorf("store: look for app %s: %w", app.ID, err)
	}
	if exists {
		return fmt.Errorf("store: app %q: %w", app.ID, ErrExists)
	}

	if app.CreatedAt.IsZero() {
		app.CreatedAt = now()
	}
	app.UpdatedAt = app.CreatedAt

	if err := s.putJSON(ctx, key, appRecord{App: *app}); err != nil {
		return err
	}
	return nil
}

// GetApp reads one app.
func (s *Store) GetApp(ctx context.Context, id string) (*model.App, error) {
	var record appRecord
	if err := s.getJSON(ctx, appKey(id), &record); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("store: app %q: %w", id, ErrNotFound)
		}
		return nil, err
	}

	app := record.App
	// Namespace and the address are derived, not stored: they follow from the
	// deployment's configuration, and a deployment that changed its base domain
	// or namespace prefix would otherwise serve apps recorded under the old one
	// until every one of them happened to be redeployed.
	s.derive(&app)
	return &app, nil
}

// ListApps returns every app, newest first.
//
// One listing, because the layout puts each app's record at a fixed depth:
// "apps/" also prefixes every app's commits, builds and repository, and those
// are one level deeper and are skipped by name rather than by being fetched.
func (s *Store) ListApps(ctx context.Context) ([]*model.App, error) {
	objects, err := s.objects.List(ctx, appsPrefix)
	if err != nil {
		return nil, fmt.Errorf("store: list apps: %w", err)
	}

	var apps []*model.App
	for _, object := range objects {
		// Only apps/<id>/app.json is an app. Everything else under the prefix
		// belongs to one.
		rest := strings.TrimPrefix(object.Key, appsPrefix)
		if strings.Count(rest, "/") != 1 || !strings.HasSuffix(rest, "/"+appFile) {
			continue
		}

		var record appRecord
		if err := s.getJSON(ctx, object.Key, &record); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		app := record.App
		s.derive(&app)
		apps = append(apps, &app)
	}

	sortNewestFirst(apps,
		func(a *model.App) string { return a.ID },
		func(a *model.App) time.Time { return a.CreatedAt })
	return apps, nil
}

// UpdateApp replaces an app's record.
//
// It reads first rather than writing the caller's struct straight out, so that a
// caller which loaded an app and changed one field cannot blank the fields it
// did not know about. The read and the write are not atomic, which is the
// last-write-wins property the package comment names.
func (s *Store) UpdateApp(ctx context.Context, app *model.App) error {
	current, err := s.GetApp(ctx, app.ID)
	if err != nil {
		return err
	}

	// The derived fields are not this store's to keep; the caller's copy came
	// from a read that filled them in, and writing them back would freeze a
	// value that is meant to follow the deployment's configuration.
	app.Namespace = ""
	app.CreatedAt = current.CreatedAt
	app.UpdatedAt = now()

	return s.putJSON(ctx, appKey(app.ID), appRecord{App: *app})
}

// SetAppStatus records the outcome of the most recent attempt.
func (s *Store) SetAppStatus(ctx context.Context, id string, status model.AppStatus, reason string) error {
	app, err := s.GetApp(ctx, id)
	if err != nil {
		return err
	}
	app.Status = status
	app.StatusReason = reason
	return s.UpdateApp(ctx, app)
}

// SetAppDeployed records what is now running.
func (s *Store) SetAppDeployed(ctx context.Context, id, commitSHA, image string) error {
	app, err := s.GetApp(ctx, id)
	if err != nil {
		return err
	}
	app.CommitSHA = commitSHA
	app.Image = image
	return s.UpdateApp(ctx, app)
}

// DeleteApp removes the app and everything under it.
//
// The id becomes available again, which is what an operator re-creating an app
// expects. Nothing else in AppLab has to know: the app is gone from every
// listing, so no other app can collide with the name, and the Kubernetes
// objects it owned were removed before this ran — which is what makes a
// same-named app built afterwards start from an empty cluster rather than
// adopting what the last one left.
//
// The one thing that is not undone is an image already pushed to the registry.
// It is addressed by app id and commit, so a new app with the same id and the
// same commit would find the old image and reuse it — which is the same
// behaviour a rebuild of an unchanged commit has, and is therefore not a
// surprise.
func (s *Store) DeleteApp(ctx context.Context, id string) error {
	objects, err := s.objects.List(ctx, appPrefix(id))
	if err != nil {
		return fmt.Errorf("store: list app %s: %w", id, err)
	}
	for _, object := range objects {
		if err := s.objects.Delete(ctx, object.Key); err != nil {
			return fmt.Errorf("store: delete %s: %w", object.Key, err)
		}
	}
	return nil
}

// derive fills in the fields that follow from the deployment rather than from
// anything stored.
//
// It is a no-op here: the resolver is attached by the server, which is what
// knows the namespace prefix and the base domain. The method exists so that
// GetApp and ListApps go through one place, and so that a deployment with no
// resolver — a test, or a console-only install — works unchanged.
func (s *Store) derive(app *model.App) {
	if s.deriveFn != nil {
		s.deriveFn(app)
	}
}

// ErrExists is returned by CreateApp when the id is taken.
var ErrExists = errors.New("already exists")

// KeyRecord is one app's key, with the app it belongs to.
//
// It is exported because the key store lives in another package and needs the
// app id alongside the record. The value is deliberately not the field that is
// read here — see internal/appkey for what a caller does with it.
type KeyRecord struct {
	AppID  string
	Digest string
}

// ListKeyRecords returns every app's key digest.
//
// It reads only the digest field rather than decoding the whole object, because
// its one caller — resolving a presented key — must not have every app's
// credential in memory to find out which one belongs to the caller. The value
// stays on disk until the digest has already matched.
func (s *Store) ListKeyRecords(ctx context.Context) ([]KeyRecord, error) {
	objects, err := s.objects.List(ctx, appsPrefix)
	if err != nil {
		return nil, fmt.Errorf("store: list key records: %w", err)
	}

	var out []KeyRecord
	for _, object := range objects {
		// Only apps/<id>/key.json is a key for an app.
		rest := strings.TrimPrefix(object.Key, appsPrefix)
		if strings.Count(rest, "/") != 1 || !strings.HasSuffix(rest, "/key.json") {
			continue
		}
		appID := strings.TrimSuffix(rest, "/key.json")

		body, err := s.objects.GetBytes(ctx, object.Key)
		if err != nil {
			// A record listed and then gone is a deletion racing this read —
			// the app was removed, and it has no key to match.
			if errors.Is(err, objectstore.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("store: read %s: %w", object.Key, err)
		}

		// A field of the object rather than the whole of it. json.Unmarshal into
		// a struct that names only this field leaves the rest unread, which is
		// the point.
		var record struct {
			Digest string `json:"digest"`
		}
		if err := json.Unmarshal(body, &record); err != nil {
			return nil, fmt.Errorf("store: decode %s: %w", object.Key, err)
		}
		out = append(out, KeyRecord{AppID: appID, Digest: record.Digest})
	}
	return out, nil
}
