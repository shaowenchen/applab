package appconfig_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaowenchen/applab/internal/appconfig"
	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/store"
)

// newStore returns a configuration store over a temporary object store, plus
// the store underneath so a test can look at where a value ended up.
func newStore(t *testing.T) (*appconfig.Store, *store.Store) {
	t.Helper()

	apps, err := store.OpenLocal(context.Background(), filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return appconfig.New(apps), apps
}

// contentsOf reads an app's secret values out of its record.
//
// The package no longer offers a reader for them — the deployer gets the map
// from the app it was handed — so a test that wants to see a value goes to the
// record directly, which is also what makes these tests assert where a value
// actually lives.
func contentsOf(t *testing.T, apps *store.Store, appID string) (map[string]string, error) {
	t.Helper()
	app, err := apps.GetApp(context.Background(), appID)
	if err != nil {
		return nil, err
	}
	return app.Secrets, nil
}

// createApp records an app, which is what a value belongs to.
func createApp(t *testing.T, apps *store.Store, id string) {
	t.Helper()

	app := &model.App{ID: id, Name: id, Port: 8080, Replicas: 1, CreatedAt: time.Now()}
	if err := apps.CreateApp(context.Background(), app); err != nil {
		t.Fatalf("create app %s: %v", id, err)
	}
}

// TestSecretsAreStoredInTheAppRecord asserts where a secret ends up, because
// that placement is the whole of this package's design now.
//
// It used to be a Kubernetes Secret, referenced from the Deployment with
// envFrom so the kubelet substituted the value and it never appeared in the pod
// spec. With no Secret object the value has to be somewhere the deployer can
// read, and the app's own record is where it went — which means it does reach
// the Deployment's env in the clear. This test is the one that would fail if a
// future change quietly moved it somewhere else, so it asserts the location
// explicitly rather than through the package's own accessor.
func TestSecretsAreStoredInTheAppRecord(t *testing.T) {
	store, apps := newStore(t)
	ctx := context.Background()
	createApp(t, apps, "shop")

	if _, err := store.Set(ctx, "shop", map[string]string{"DATABASE_URL": "postgres://x"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	app, err := apps.GetApp(ctx, "shop")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if got := app.Secrets["DATABASE_URL"]; got != "postgres://x" {
		t.Errorf("the app record holds DATABASE_URL = %q, want %q", got, "postgres://x")
	}
	// The plain half is a different field, and setting a secret must not have
	// written it there: Env is returned by the API in full, Secrets is not.
	if _, leaked := app.Env["DATABASE_URL"]; leaked {
		t.Error("the secret was written into Env, which every route returns in full")
	}
}

// TestSetPreservesUnmentionedKeys is the reason Set merges rather than replaces.
//
// Without it, setting one variable would silently drop the other five, which is
// a configuration change nobody asked for and no error to explain it.
func TestSetPreservesUnmentionedKeys(t *testing.T) {
	store, apps := newStore(t)
	ctx := context.Background()
	createApp(t, apps, "shop")

	if _, err := store.Set(ctx, "shop", map[string]string{"A": "1", "B": "2"}); err != nil {
		t.Fatalf("first set: %v", err)
	}
	names, err := store.Set(ctx, "shop", map[string]string{"C": "3"})
	if err != nil {
		t.Fatalf("second set: %v", err)
	}

	want := []string{"A", "B", "C"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("names after the second set = %v, want %v", names, want)
	}

	contents, err := contentsOf(t, apps, "shop")
	if err != nil {
		t.Fatalf("contents: %v", err)
	}
	if contents["A"] != "1" || contents["B"] != "2" || contents["C"] != "3" {
		t.Errorf("contents = %v, want A=1 B=2 C=3", contents)
	}
}

// TestSetOverwritesAnExistingKey covers the rotation case: setting a name that
// is already there replaces it rather than failing or appending.
func TestSetOverwritesAnExistingKey(t *testing.T) {
	store, apps := newStore(t)
	ctx := context.Background()
	createApp(t, apps, "shop")

	if _, err := store.Set(ctx, "shop", map[string]string{"TOKEN": "old"}); err != nil {
		t.Fatalf("first set: %v", err)
	}
	if _, err := store.Set(ctx, "shop", map[string]string{"TOKEN": "new"}); err != nil {
		t.Fatalf("second set: %v", err)
	}

	contents, err := contentsOf(t, apps, "shop")
	if err != nil {
		t.Fatalf("contents: %v", err)
	}
	if contents["TOKEN"] != "new" {
		t.Errorf("TOKEN = %q, want %q", contents["TOKEN"], "new")
	}
}

// TestRemoveLeavesNoEmptySetBehind keeps the invariant the deployer relies on:
// having no secrets and having an empty set are the same observable state.
//
// An empty map left behind would mean Has and Names disagree with Contents about
// an app that simply has none, for no gain.
func TestRemoveLeavesNoEmptySetBehind(t *testing.T) {
	store, apps := newStore(t)
	ctx := context.Background()
	createApp(t, apps, "shop")

	if _, err := store.Set(ctx, "shop", map[string]string{"TOKEN": "t"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	names, err := store.Remove(ctx, "shop", []string{"TOKEN"})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("names after removing the last value = %v, want none", names)
	}

	has, err := store.Has(ctx, "shop")
	if err != nil {
		t.Fatalf("has: %v", err)
	}
	if has {
		t.Error("Has reported true after the last value was removed")
	}
	record, err := apps.GetApp(ctx, "shop")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if len(record.Secrets) != 0 {
		t.Errorf("the app record still carries %v", record.Secrets)
	}
}

// TestRemoveKeepsTheOtherValues is the other half of that invariant.
func TestRemoveKeepsTheOtherValues(t *testing.T) {
	store, apps := newStore(t)
	ctx := context.Background()
	createApp(t, apps, "shop")

	if _, err := store.Set(ctx, "shop", map[string]string{"A": "1", "B": "2"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	names, err := store.Remove(ctx, "shop", []string{"A"})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}

	if strings.Join(names, ",") != "B" {
		t.Errorf("names = %v, want [B]", names)
	}
}

// TestNoSecretsIsNotAnError asserts the ordinary case for an app that has none.
//
// Reporting that as a failure would make every app without secrets look broken,
// and the API turns this into an empty list.
func TestNoSecretsIsNotAnError(t *testing.T) {
	store, apps := newStore(t)
	ctx := context.Background()
	createApp(t, apps, "shop")

	names, err := store.Names(ctx, "shop")
	if err != nil {
		t.Fatalf("names: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("names = %v, want none", names)
	}

	has, err := store.Has(ctx, "shop")
	if err != nil {
		t.Fatalf("has: %v", err)
	}
	if has {
		t.Error("Has reported true for an app with no secrets")
	}
}

// TestNamesAreSorted asserts the order the console and CLI display.
//
// Go randomises map iteration, so an unordered result would shuffle on every
// read — which reads as a change that did not happen.
func TestNamesAreSorted(t *testing.T) {
	store, apps := newStore(t)
	ctx := context.Background()
	createApp(t, apps, "shop")

	if _, err := store.Set(ctx, "shop", map[string]string{"ZED": "1", "ALPHA": "2", "MID": "3"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	names, err := store.Names(ctx, "shop")
	if err != nil {
		t.Fatalf("names: %v", err)
	}
	if strings.Join(names, ",") != "ALPHA,MID,ZED" {
		t.Errorf("names = %v, want them sorted", names)
	}
}

// TestValidateName covers the ways a name can be refused.
//
// PORT is the one that matters and is not about syntax: the deployer sets it
// from the app's port, so a second declaration would be dropped by the deployer
// and the app would run somewhere nothing routes to, with no error anywhere.
func TestValidateName(t *testing.T) {
	cases := []struct {
		name    string
		wantErr bool
	}{
		{"LOG_LEVEL", false},
		{"DATABASE_URL", false},
		{"_underscore", false},
		{"has-dash", false},
		{"has.dot", false},
		{appconfig.Reserved, true},
		{"", true},
		{"1BAD", true},
		{"has space", true},
		{"has=equals", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := appconfig.ValidateName(tc.name)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateName(%q) = nil, want an error", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateName(%q) = %v, want nil", tc.name, err)
			}
		})
	}
}

// TestSetRefusesReservedName asserts the refusal is enforced in the store, not
// only in the handler.
//
// Both layers check it on purpose: the handler gives the caller a useful message,
// and this makes it impossible for any future caller of the package to write a
// conflicting PORT by going around the API.
func TestSetRefusesReservedName(t *testing.T) {
	store, apps := newStore(t)
	ctx := context.Background()
	createApp(t, apps, "shop")

	_, err := store.Set(ctx, "shop", map[string]string{appconfig.Reserved: "9999"})
	if err == nil {
		t.Fatal("Set accepted the reserved name")
	}
	if !strings.Contains(err.Error(), "port") {
		t.Errorf("the error should say where the value comes from, got: %v", err)
	}
}

// TestSetRefusesAnAppThatDoesNotExist asserts a value is not written for a
// record that is not there.
//
// With secrets in a Secret object this could not happen — the API created a
// Secret for whatever id it was handed. Now the value lives in the app's own
// record, so writing one for an app that does not exist would create a stray
// object under a prefix nothing lists, and nobody would ever see it.
func TestSetRefusesAnAppThatDoesNotExist(t *testing.T) {
	store, _ := newStore(t)

	if _, err := store.Set(context.Background(), "ghost", map[string]string{"A": "1"}); err == nil {
		t.Error("Set accepted a value for an app that does not exist")
	}
}

// TestDeleteRemovesEverySecret covers the app-deletion path.
func TestDeleteRemovesEverySecret(t *testing.T) {
	store, apps := newStore(t)
	ctx := context.Background()
	createApp(t, apps, "shop")

	if _, err := store.Set(ctx, "shop", map[string]string{"A": "1", "B": "2"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := store.Delete(ctx, "shop"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	has, err := store.Has(ctx, "shop")
	if err != nil {
		t.Fatalf("has: %v", err)
	}
	if has {
		t.Error("the secrets survived Delete")
	}

	// Deleting again is not an error: the caller asked for the app's secrets to
	// be gone, and they are.
	if err := store.Delete(ctx, "shop"); err != nil {
		t.Errorf("deleting secrets that are already gone returned %v, want nil", err)
	}
}

// TestOneAppsSecretsAreNotAnothers asserts the lookup is per app.
//
// One record per app is the isolation between them here, so a value set for one
// must not be readable as another's — and must not appear in the other's
// Deployment either, which is what the deployer reads this through.
func TestOneAppsSecretsAreNotAnothers(t *testing.T) {
	store, apps := newStore(t)
	ctx := context.Background()
	createApp(t, apps, "shop")
	createApp(t, apps, "blog")

	if _, err := store.Set(ctx, "shop", map[string]string{"TOKEN": "shop-token"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	contents, err := contentsOf(t, apps, "blog")
	if err != nil {
		t.Fatalf("contents: %v", err)
	}
	if len(contents) != 0 {
		t.Errorf("blog sees %v, want nothing — one record per app is the boundary", contents)
	}
}
