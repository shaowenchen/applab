// Package store keeps what AppLab knows about its apps in object storage.
//
// It used to be SQLite on a volume. That made AppLab stateful — one replica,
// pinned to one node, with a disk to lose and a claim to resize — and it is the
// half of AppLab that could not be made stateless, because WAL needs POSIX file
// locks and mmap that an object store does not provide. Everything the database
// held now lives in the bucket instead.
//
// # The layout
//
// One directory per app, and within it one object per thing that is written
// independently:
//
//	apps/<id>/app.json            the app: name, port, replicas, env, status,
//	                              the commit and image currently deployed
//	apps/<id>/commits/<sha>.json  one recorded commit
//	apps/<id>/builds/<id>.json    one build attempt
//	apps/<id>/source.git/         the app's repository, see internal/source
//
// The theme is that an object is written by one writer and replaced whole.
// Object storage has no transactions across keys and no compare-and-swap worth
// relying on, so anything that needed a transaction in SQL is arranged here so
// that it does not need one: a build's status is one object rather than a
// column in a row, and two builds never write the same one.
//
// The exception is app.json, which is read-modify-written by several callers —
// a deploy sets the deployed commit, a build sets the status, a configuration
// change sets the environment. Concurrent writers to it are last-write-wins,
// and the loser's field is lost. That is a real property of this design and not
// a bug to be fixed by retrying: the operations that write it are triggered by
// one person or by one pipeline in the normal case, and the alternative — a lock
// object — would need a lease protocol object storage does not have. It is
// named here so the next person does not discover it as a surprise.
//
// # What reads cost
//
// Reading one app is one GET. Listing apps is one list, because the layout puts
// each app's object at a fixed depth and the listing can ignore the deeper keys
// without fetching them. Counting apps by status is a list plus one GET per app,
// which is a real cost that grows with the number of apps — acceptable for a
// dashboard over a platform with tens or hundreds of apps, and the reason the
// overview is a single authenticated call rather than something a client polls.
//
// # What is not here
//
// Secrets. An app's secret configuration lives in a Kubernetes Secret, read by
// the deployer and by nothing else, so the bucket is not a second place for a
// credential to leak from. The split is by sensitivity: everything in an
// app.json is returned by the API, and whatever is in it should be fit to print.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/objectstore"
)

// ErrNotFound is returned by the getters when the thing asked for is not there.
var ErrNotFound = errors.New("store: not found")

// Store is AppLab's state, over an object store.
type Store struct {
	objects objectstore.Store

	// deriveFn fills in the fields that follow from the deployment rather than
	// from anything stored — an app's namespace, which depends on where this
	// AppLab runs. It is attached by the server, which is what knows that, and
	// is nil in a test or a console-only install.
	deriveFn func(*model.App)
}

// Derives attaches the function that fills in an app's derived fields.
//
// It is called for every app this store hands out, so a field set here is
// correct everywhere the app is used and does not depend on each caller
// remembering. A field that a caller has to fill in by hand is one that some
// caller will not, and the namespace is the expensive one to forget: the API
// server reads "" as the default namespace, so an object would be created in the
// wrong place and the app's own namespace would stay empty, with no error
// anywhere.
func (s *Store) Derives(fn func(*model.App)) *Store {
	s.deriveFn = fn
	return s
}

// Open returns a store over an object store.
//
// It does not probe the bucket. A deployment whose credential is wrong finds out
// on the first call rather than at startup — deliberately, because the failure is
// legible either way, and probing would make AppLab refuse to start when a bucket
// is briefly unreachable, which is exactly when a replica restart should be
// allowed to succeed.
func Open(ctx context.Context, objects objectstore.Store) (*Store, error) {
	if objects == nil {
		return nil, fmt.Errorf("store: no object store given")
	}
	return &Store{objects: objects}, nil
}

// OpenLocal returns a store over a directory.
//
// It is for development and for the tests, where there is no bucket. What it
// runs is the same code that runs against S3 — the object store interface offers
// nothing a bucket cannot do — so a test against this exercises the real paths
// rather than a second implementation of them.
func OpenLocal(ctx context.Context, dir string) (*Store, error) {
	objects, err := objectstore.NewLocal(dir)
	if err != nil {
		return nil, err
	}
	return Open(ctx, objects)
}

// Objects returns the object store underneath, for the parts of AppLab that
// keep their own layout in it — the source repositories.
func (s *Store) Objects() objectstore.Store { return s.objects }

// Layout of the keys, in one place so no call site builds one by hand.
//
// Apps are listed by their own prefix at a fixed depth. That is what makes
// ListApps a single listing rather than a walk: the commits and builds of every
// app are under the same prefix but one level deeper, and are skipped without
// being fetched.
const (
	appsPrefix    = "apps/"
	appFile       = "app.json"
	commitsDir    = "commits/"
	buildsDir     = "builds/"
	sourceGitDir  = "source.git"
	uploadsPrefix = "uploads/"
)

// appPrefix is the directory one app's objects live under.
func appPrefix(appID string) string { return objectstore.Key(appsPrefix, appID) }

func appKey(appID string) string {
	return objectstore.Key(appPrefix(appID), appFile)
}

func commitKey(appID, sha string) string {
	return objectstore.Key(appPrefix(appID), commitsDir, sha+".json")
}

func commitsPrefix(appID string) string {
	return objectstore.Key(appPrefix(appID), commitsDir) + "/"
}

func buildKey(appID, id string) string {
	return objectstore.Key(appPrefix(appID), buildsDir, id+".json")
}

func buildsPrefix(appID string) string {
	return objectstore.Key(appPrefix(appID), buildsDir) + "/"
}

// SourcePrefix is where one app's repository lives.
//
// It is exported because internal/source owns what is inside it: the store
// knows only that an app has a directory for its repository, and the source
// package knows what a repository is.
func SourcePrefix(appID string) string {
	return objectstore.Key(appPrefix(appID), sourceGitDir)
}

// uploadKey is where an in-progress chunked upload is described.
//
// Uploads are the one thing here with a lifetime shorter than the app's: an
// upload's parts are staged in the bucket and its descriptor is removed when the
// upload completes, so a failed upload leaves parts behind that nothing refers
// to. They are removed by the same call that would have removed the descriptor,
// and anything left by a crash is stale but harmless.
func uploadKey(id string) string { return objectstore.Key(uploadsPrefix, id) }

// putJSON writes a value as an object.
//
// Every write in this package goes through here or through putJSONIfExists, so
// the encoding is one decision rather than one per call site. It is indented
// rather than compact: an operator debugging a deployment by looking at the
// bucket in a browser is the main reader these objects have.
func (s *Store) putJSON(ctx context.Context, key string, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("store: encode %s: %w", key, err)
	}
	if err := s.objects.PutBytes(ctx, key, body); err != nil {
		return fmt.Errorf("store: write %s: %w", key, err)
	}
	return nil
}

// getJSON reads an object and decodes it, mapping a missing object onto
// ErrNotFound.
func (s *Store) getJSON(ctx context.Context, key string, into any) error {
	body, err := s.objects.GetBytes(ctx, key)
	if err != nil {
		if errors.Is(err, objectstore.ErrNotExist) {
			return ErrNotFound
		}
		return fmt.Errorf("store: read %s: %w", key, err)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("store: decode %s: %w", key, err)
	}
	return nil
}

// listJSON reads every object under a prefix and decodes each.
//
// A malformed object is returned as an error rather than skipped: an object that
// cannot be decoded is either a version of AppLab writing something this build
// does not understand, or a corrupted write, and silently dropping it would make
// an app disappear from a listing with nothing to explain why.
func (s *Store) listJSON(ctx context.Context, prefix string, decode func([]byte) error) error {
	objects, err := s.objects.List(ctx, prefix)
	if err != nil {
		return fmt.Errorf("store: list %s: %w", prefix, err)
	}

	for _, object := range objects {
		body, err := s.objects.GetBytes(ctx, object.Key)
		if err != nil {
			// An object listed and then gone is a deletion racing this read.
			// Skipping it is right: the caller asked for what is there.
			if errors.Is(err, objectstore.ErrNotExist) {
				continue
			}
			return fmt.Errorf("store: read %s: %w", object.Key, err)
		}
		if err := decode(body); err != nil {
			return fmt.Errorf("store: decode %s: %w", object.Key, err)
		}
	}
	return nil
}

// appRecord is what app.json holds: the app and everything about it that
// changes independently of the objects around it.
//
// The model's fields are embedded rather than listed, so a field added to
// model.App is written and read without this struct needing to be edited — the
// failure of the alternative is a field that saves and never loads.
//
// Times are stored as Unix seconds in the embedded struct's own JSON form
// because model.App already has them as time.Time, which marshals to RFC 3339.
// That is unambiguous and readable, which is what an operator reading the bucket
// wants, and it needs no conversion layer between here and the model.
type appRecord struct {
	model.App
}

// now is the clock, as a function so a test can place events in a fixed order.
//
// Listing apps newest-first sorts on the records' timestamps, and two apps
// created in the same second would order arbitrarily — which is fine for a
// listing and wrong for a test that asserts an order.
var now = func() time.Time { return time.Now().UTC() }

// sortNewestFirst orders records by their timestamp, newest first, with the id as
// the tiebreak so the order is total and two runs agree.
func sortNewestFirst[T any](items []T, id func(T) string, at func(T) time.Time) {
	sort.Slice(items, func(i, j int) bool {
		ti, tj := at(items[i]), at(items[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return id(items[i]) > id(items[j])
	})
}

// decodeInto decodes an object's body into a value, as a function rather than a
// method so a lister can call it without repeating the error wrapping.
func decodeInto(body []byte, into any) error {
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}
