// Package appconfig stores an app's secret configuration.
//
// An app has two kinds of configuration and they are kept the same way now,
// distinguished by intent rather than by storage. Environment variables are
// plain — LOG_LEVEL, FEATURE_X — and secrets are passwords, tokens and
// connection strings.
//
// # Where they live
//
// Both live in the app's own record in the object store, and both reach a
// container the same way: the deployer writes them into the Deployment's `env`.
//
// They used to be split. The plain ones were in the app's row; the secret ones
// were in a Kubernetes Secret, referenced with `envFrom` so that the kubelet did
// the substitution and the value never appeared in the Pod spec. That split is
// gone with the Secret object it rested on, and it is worth being plain about
// what replaced it:
//
// **A secret value is now in the Deployment's spec, in the clear.** Anything
// that can read a Deployment in this namespace can read every app's secrets:
// `kubectl get deploy -o yaml`, `kubectl describe`, a dashboard with get on the
// namespace. That is a real loss and it is the cost of not having a Secret
// object; there is no way to put a value into a container without it appearing
// somewhere the container spec can see, and the only places Kubernetes offers
// for it are the two object kinds that have now been given up.
//
// What is *not* lost: the value is still not in an app.json that the API
// returns. The API reports a secret's name and never its value, and that
// behaviour is unchanged — the difference is that the value is now readable by
// anyone with cluster access rather than only by AppLab.
//
// # The race this accepts
//
// The app's record is rewritten whole by several callers — a deploy sets the
// deployed commit, a build sets the status, Set here merges a value — so two of
// them writing at the same instant can lose one of the two writes. That is the
// same last-writer-wins window internal/store names for the record as a whole,
// and it is accepted here for the same reason: the alternative is a conditional
// write that not every S3-compatible service implements, and what it costs in
// practice is a configuration change that did not take.
//
// It is worth being explicit that the value itself is not at risk of being
// blanked by a racing deploy: Set re-reads the record, merges, and writes, so a
// deploy that lands in between is the one whose change is lost — and a rotation
// is an explicit operation a caller can repeat.
package appconfig

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/store"
)

// Store reads and writes an app's secret configuration.
type Store struct {
	apps *store.Store
}

// New returns a configuration store over AppLab's own state.
//
// There is no Ready check and no nil-ness any more: a deployment always has an
// object store, so an installation with no cluster keeps secrets exactly like
// one with. That removes a class of "this deployment cannot do that" answers
// which only ever meant "this deployment has no cluster".
func New(apps *store.Store) *Store {
	return &Store{apps: apps}
}

// maxValueBytes bounds one value.
//
// The limit is Kubernetes', not AppLab's: a Deployment's env entry is carried in
// the object and in every list response that returns it, and a value past this
// size is refused by the API server with a message about the object rather than
// about the field. Refusing it here means the caller is told which value is too
// large.
const maxValueBytes = 64 * 1024

// Reserved is the one name that may not be set.
//
// The deployer sets it itself from the app's port setting, because it is how the
// Service finds the container. Letting a caller also set it would put two
// declarations of the same variable in the pod spec — and the deployer resolves
// that collision by dropping the caller's, so the app would run on a port
// nothing routes to, with no error anywhere to explain it. Refusing it here is
// the only outcome that is not a silent failure.
const Reserved = model.PortEnv

// ValidateName reports whether name may be used as a configuration key.
//
// Both kinds of value end up in the same place — a name in the container's env
// list — so one rule covers both, and that rule is Kubernetes' own rather than
// one invented here. Using the API server's validator means a name this accepts
// is a name the Deployment accepts: a stricter local rule would refuse names
// that work, and a looser one would defer the failure to a deploy with a message
// about the Deployment rather than about the name.
//
// It is deliberately not a POSIX rule. Kubernetes permits a dot and a dash in an
// environment variable name, and a caller may legitimately set `has.dot` for a
// runtime that reads such a name; whether a shell can expand it is the app's
// business, not this check's.
func ValidateName(name string) error {
	if name == Reserved {
		return fmt.Errorf("%s is set by AppLab from the app's port setting and cannot be configured here", Reserved)
	}
	if errs := validation.IsEnvVarName(name); len(errs) > 0 {
		return fmt.Errorf("%q is not a valid environment variable name: %s", name, errs[0])
	}
	return nil
}

// Set writes values and returns the names that are now set.
func (s *Store) Set(ctx context.Context, appID string, values map[string]string) ([]string, error) {
	if appID == "" {
		return nil, fmt.Errorf("app id must not be empty")
	}
	if len(values) == 0 {
		return s.Names(ctx, appID)
	}
	for name, value := range values {
		if err := ValidateName(name); err != nil {
			return nil, err
		}
		if len(value) > maxValueBytes {
			return nil, fmt.Errorf("the value of %s is %d bytes; the limit is %d", name, len(value), maxValueBytes)
		}
	}

	app, err := s.apps.GetApp(ctx, appID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("app %q does not exist", appID)
		}
		return nil, err
	}

	merged := map[string]string{}
	for name, value := range app.Secrets {
		merged[name] = value
	}
	for name, value := range values {
		merged[name] = value
	}
	app.Secrets = merged

	if err := s.apps.UpdateApp(ctx, app); err != nil {
		return nil, fmt.Errorf("store configuration for app %s: %w", appID, err)
	}
	return sortedNames(merged), nil
}

// Remove deletes the named values and returns the names that remain.
func (s *Store) Remove(ctx context.Context, appID string, names []string) ([]string, error) {
	if appID == "" {
		return nil, fmt.Errorf("app id must not be empty")
	}

	app, err := s.apps.GetApp(ctx, appID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("app %q does not exist", appID)
		}
		return nil, err
	}
	if len(app.Secrets) == 0 {
		return nil, nil
	}

	for _, name := range names {
		// An unknown name is reported rather than ignored: a caller that typed a
		// name differently from how it was set should be told, not left believing
		// it removed something.
		if _, ok := app.Secrets[name]; !ok {
			return nil, fmt.Errorf("app %s has no secret named %s", appID, name)
		}
		delete(app.Secrets, name)
	}
	if len(app.Secrets) == 0 {
		app.Secrets = nil
	}

	if err := s.apps.UpdateApp(ctx, app); err != nil {
		return nil, fmt.Errorf("store configuration for app %s: %w", appID, err)
	}
	return sortedNames(app.Secrets), nil
}

// Names returns the names of an app's secrets, sorted.
//
// It is what the API reports: a secret's value is never returned by any route,
// and this is the shape that promise takes — the names are here, and the values
// are read by the deployer, which gets them from the app record it was handed
// rather than through this package.
func (s *Store) Names(ctx context.Context, appID string) ([]string, error) {
	app, err := s.apps.GetApp(ctx, appID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("app %q does not exist", appID)
		}
		return nil, err
	}
	return sortedNames(app.Secrets), nil
}

// Delete removes every secret an app has.
//
// It is called when an app is deleted, and it is kept as an operation rather
// than left to the app's own removal because the caller may be removing the
// configuration of an app it is not deleting — the API has a route for exactly
// that — and because naming the intent is cheaper than a reader working out
// whether the app's removal already covered it.
func (s *Store) Delete(ctx context.Context, appID string) error {
	app, err := s.apps.GetApp(ctx, appID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The app is gone, so its secrets are gone with it. Not an error.
			return nil
		}
		return err
	}
	if len(app.Secrets) == 0 {
		return nil
	}

	app.Secrets = nil
	return s.apps.UpdateApp(ctx, app)
}

// Has reports whether an app has any secrets.
func (s *Store) Has(ctx context.Context, appID string) (bool, error) {
	app, err := s.apps.GetApp(ctx, appID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return len(app.Secrets) > 0, nil
}

// sortedNames returns a map's keys in a stable order.
//
// Sorted rather than in map order, because the result is shown to a person and
// compared by tests: two runs over the same data have to produce the same list.
func sortedNames[V any](values map[string]V) []string {
	if len(values) == 0 {
		return nil
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
