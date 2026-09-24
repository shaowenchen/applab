package appkey_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaowenchen/applab/internal/appkey"
	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/objectstore"
	"github.com/shaowenchen/applab/internal/store"
)

// newStore returns a key store over a temporary object store.
//
// The store underneath is the real one, and the object store under that is the
// same implementation a deployment without a bucket runs — so these exercise the
// layout of `apps/<id>/key.json`, which is now the thing that decides what a
// credential can reach.
func newStore(t *testing.T) (*appkey.Store, *store.Store) {
	t.Helper()

	apps, err := store.OpenLocal(context.Background(), filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return appkey.New(apps), apps
}

// createApp records an app, which is the precondition for holding a key.
//
// It is separate from minting the key because the two are separate in the
// product: the API creates the app record and then asks for a key, so a key is
// never written for an app that does not exist.
func createApp(t *testing.T, apps *store.Store, id string) {
	t.Helper()

	app := &model.App{ID: id, Name: id, Port: 8080, Replicas: 1, CreatedAt: time.Now()}
	if err := apps.CreateApp(context.Background(), app); err != nil {
		t.Fatalf("create app %s: %v", id, err)
	}
}

// TestCreateStoresAReadableKey asserts the key a caller is handed is the one
// that comes back.
func TestCreateStoresAReadableKey(t *testing.T) {
	store, apps := newStore(t)
	ctx := context.Background()
	createApp(t, apps, "shop")

	key, err := store.Create(ctx, "shop")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if key == "" {
		t.Fatal("create returned an empty key")
	}

	got, err := store.Get(ctx, "shop")
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	if got != key {
		t.Errorf("Get returned %q, want the key Create minted (%q)", got, key)
	}
}

// TestCreateIsUniquePerCall asserts two apps never share a key, and that one app
// cannot get a second by calling Create twice.
func TestCreateIsUniquePerCall(t *testing.T) {
	store, apps := newStore(t)
	ctx := context.Background()
	createApp(t, apps, "shop")
	createApp(t, apps, "blog")

	shop, err := store.Create(ctx, "shop")
	if err != nil {
		t.Fatalf("create shop: %v", err)
	}
	blog, err := store.Create(ctx, "blog")
	if err != nil {
		t.Fatalf("create blog: %v", err)
	}
	if shop == blog {
		t.Error("two apps were given the same key")
	}

	// A second create for one app must refuse rather than rotate: it would lock
	// out whoever already holds the key.
	if _, err := store.Create(ctx, "shop"); !errors.Is(err, appkey.ErrExists) {
		t.Errorf("a second Create returned %v, want ErrExists so the caller is not silently rotated", err)
	}

	// And the original still works.
	if got, _ := store.Get(ctx, "shop"); got != shop {
		t.Error("the refused second Create changed the existing key")
	}
}

// TestGetReportsMissingKey distinguishes "no key" from any other failure.
func TestGetReportsMissingKey(t *testing.T) {
	store, _ := newStore(t)

	if _, err := store.Get(context.Background(), "nothing"); !errors.Is(err, appkey.ErrNoKey) {
		t.Errorf("Get for an app with no key returned %v, want ErrNoKey", err)
	}
}

// TestCreateRejectsAnEmptyAppID asserts a missing id fails loudly rather than
// scanning for a record named for nothing.
func TestCreateRejectsAnEmptyAppID(t *testing.T) {
	store, _ := newStore(t)

	if _, err := store.Create(context.Background(), ""); err == nil {
		t.Error("creating a key with no app id should fail")
	}
}

// TestRotateReplacesImmediately is the security property: the old key stops
// working the moment rotation returns.
func TestRotateReplacesImmediately(t *testing.T) {
	store, apps := newStore(t)
	ctx := context.Background()
	createApp(t, apps, "shop")

	original, err := store.Create(ctx, "shop")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	rotated, err := store.Rotate(ctx, "shop")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated == original {
		t.Fatal("Rotate returned the same key")
	}

	if got, _ := store.Get(ctx, "shop"); got != rotated {
		t.Errorf("Get returned %q after rotation, want the new key", got)
	}

	// The decisive check: the old key must no longer resolve to anything.
	if app, ok, err := store.ResolveAppKey(ctx, original); err != nil {
		t.Fatalf("resolve old key: %v", err)
	} else if ok {
		t.Errorf("the old key still resolves to app %q; rotation must invalidate it at once", app)
	}
}

// TestRotateCreatesWhenAbsent covers the recovery path: an app whose key was
// lost can be brought back with the same operation rather than an error telling
// the caller to create it first.
func TestRotateCreatesWhenAbsent(t *testing.T) {
	store, _ := newStore(t)

	key, err := store.Rotate(context.Background(), "fresh")
	if err != nil {
		t.Fatalf("rotate a keyless app: %v", err)
	}
	if key == "" {
		t.Error("rotate returned an empty key for an app that had none")
	}
}

// TestResolveMapsKeyToApp is what the auth layer stands on.
func TestResolveMapsKeyToApp(t *testing.T) {
	store, apps := newStore(t)
	ctx := context.Background()
	createApp(t, apps, "shop")
	createApp(t, apps, "blog")

	shopKey, _ := store.Create(ctx, "shop")
	blogKey, _ := store.Create(ctx, "blog")

	cases := []struct {
		name string
		key  string
		want string
		ok   bool
	}{
		{"shop's key is shop's", shopKey, "shop", true},
		{"blog's key is blog's", blogKey, "blog", true},
		{"an unknown key resolves to nothing", "not-a-key", "", false},
		{"an empty key resolves to nothing", "", "", false},
		{"whitespace is not a key", "   ", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, ok, err := store.ResolveAppKey(ctx, tc.key)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if ok != tc.ok || app != tc.want {
				t.Errorf("Resolve(%q) = (%q, %v), want (%q, %v)", tc.key, app, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestResolveToleratesSurroundingSpace asserts a key pasted with a stray newline
// still works — which is what a shell substitution routinely produces.
func TestResolveToleratesSurroundingSpace(t *testing.T) {
	store, apps := newStore(t)
	ctx := context.Background()
	createApp(t, apps, "shop")

	key, _ := store.Create(ctx, "shop")

	app, ok, err := store.ResolveAppKey(ctx, "  "+key+"\n")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !ok || app != "shop" {
		t.Errorf("a key with surrounding whitespace did not resolve: (%q, %v)", app, ok)
	}
}

// TestResolveIgnoresARecordAtTheWrongDepth is the equivalent of the label check
// the Secret version needed, expressed in the layout that replaced it.
//
// A key is found by listing `apps/<id>/key.json` at exactly that depth. The old
// version keyed off a label, which meant anything that could write a Secret with
// a matching digest label could mint a credential for itself; the object store
// has no labels, and what scopes a key to an app is where its record sits. This
// asserts that a record anywhere else is not a credential — which is the
// property that keeps a value someone else's app wrote out of the app list.
func TestResolveIgnoresARecordAtTheWrongDepth(t *testing.T) {
	store, apps := newStore(t)
	ctx := context.Background()
	createApp(t, apps, "shop")

	key, _ := store.Create(ctx, "shop")

	// Take the real record's digest, so the forgery collides with the genuine
	// lookup rather than with a value this test computed itself.
	body, err := apps.Objects().GetBytes(ctx, objectstore.Key("apps", "shop", "key.json"))
	if err != nil {
		t.Fatalf("read shop's key record: %v", err)
	}
	var genuine struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(body, &genuine); err != nil {
		t.Fatalf("decode shop's key record: %v", err)
	}
	if genuine.Digest == "" {
		t.Fatal("shop's key record carries no digest")
	}

	// A record carrying shop's digest, but nested a level deeper — where the
	// listing does not look, because that is where a build's or a commit's own
	// files live.
	forged, _ := json.Marshal(map[string]string{"key": key, "digest": genuine.Digest})
	if err := apps.Objects().PutBytes(ctx, objectstore.Key("apps", "shop", "builds", "key.json"), forged); err != nil {
		t.Fatalf("write the forged record: %v", err)
	}

	// Removing shop's key leaves only the forged record, which must not resolve.
	if err := store.Remove(ctx, "shop"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if app, ok, err := store.ResolveAppKey(ctx, key); err != nil {
		t.Fatalf("resolve: %v", err)
	} else if ok {
		t.Errorf("a key record outside apps/<id>/key.json resolved to app %q", app)
	}
}

// TestRemoveIsIdempotent asserts removing a key that is not there is not an
// error, so teardown can call it unconditionally.
func TestRemoveIsIdempotent(t *testing.T) {
	store, apps := newStore(t)
	ctx := context.Background()

	if err := store.Remove(ctx, "nothing"); err != nil {
		t.Errorf("removing a non-existent key returned %v, want nil", err)
	}

	createApp(t, apps, "shop")
	key, _ := store.Create(ctx, "shop")
	if err := store.Remove(ctx, "shop"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := store.Get(ctx, "shop"); !errors.Is(err, appkey.ErrNoKey) {
		t.Errorf("the key survived its removal: %v", err)
	}
	if _, ok, _ := store.ResolveAppKey(ctx, key); ok {
		t.Error("a removed key still resolves")
	}
}

// TestDeletingAnAppTakesItsKey asserts the teardown property the Secret version
// got from a label: removing an app leaves no working credential behind.
//
// It is the reason the key is an object under the app's own directory rather
// than a shared index — the app's removal is a prefix deletion, and the key is
// inside that prefix.
func TestDeletingAnAppTakesItsKey(t *testing.T) {
	store, apps := newStore(t)
	ctx := context.Background()
	createApp(t, apps, "shop")
	createApp(t, apps, "blog")

	shopKey, _ := store.Create(ctx, "shop")
	blogKey, _ := store.Create(ctx, "blog")

	if err := apps.DeleteApp(ctx, "shop"); err != nil {
		t.Fatalf("delete app: %v", err)
	}

	if app, ok, err := store.ResolveAppKey(ctx, shopKey); err != nil {
		t.Fatalf("resolve: %v", err)
	} else if ok {
		t.Errorf("deleting an app left a working key, resolving to %q", app)
	}
	// The neighbour is untouched: one app's removal is one prefix.
	if app, ok, _ := store.ResolveAppKey(ctx, blogKey); !ok || app != "blog" {
		t.Errorf("deleting shop disturbed blog's key: (%q, %v)", app, ok)
	}
}
