package appkey_test

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/shaowenchen/applab/internal/appkey"
	"github.com/shaowenchen/applab/internal/k8s"
)

const namespace = "ops-system"

func newStore(t *testing.T) (*appkey.Store, kubernetes.Interface) {
	t.Helper()
	client := fake.NewSimpleClientset()
	return appkey.New(client, namespace), client
}

// TestStoreWritesDataNotStringData guards the divergence that made the first
// version of this package unverifiable.
//
// StringData is write-only sugar: a real API server folds it into Data and never
// returns it, while the fake clientset used by these tests does not fold at all.
// A store written against StringData therefore behaves one way in a test and
// another way against a cluster — and a read path that consulted StringData
// would work in the test and see an empty key in production. The Secret this
// package writes must carry the same shape in both, which writing Data directly
// is what guarantees.
func TestStoreWritesDataNotStringData(t *testing.T) {
	store, client := newStore(t)
	ctx := context.Background()

	key, err := store.Create(ctx, "shop")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	secret, err := client.CoreV1().Secrets(namespace).Get(ctx, appkey.Name("shop"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if len(secret.StringData) != 0 {
		t.Errorf("the Secret was written with StringData=%v; a real API server folds that into Data and drops it, so the stored shape differs between a cluster and these tests",
			secret.StringData)
	}
	if got := string(secret.Data["key"]); got != key {
		t.Errorf("Data[key] = %q, want the key that was minted (%q)", got, key)
	}
}

// TestCreateStoresAReadableKey asserts the key a caller is handed is the one
// that comes back.
func TestCreateStoresAReadableKey(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

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
	store, _ := newStore(t)
	ctx := context.Background()

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

// TestRotateReplacesImmediately is the security property: the old key stops
// working the moment rotation returns.
func TestRotateReplacesImmediately(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

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
	store, _ := newStore(t)
	ctx := context.Background()

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
	store, _ := newStore(t)
	ctx := context.Background()

	key, _ := store.Create(ctx, "shop")

	app, ok, err := store.ResolveAppKey(ctx, "  "+key+"\n")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !ok || app != "shop" {
		t.Errorf("a key with surrounding whitespace did not resolve: (%q, %v)", app, ok)
	}
}

// TestResolveIgnoresASecretWithoutAnAppLabel asserts a Secret carrying only the
// digest label grants nothing.
//
// The lookup is by digest, so anything that can write a Secret with that label
// would otherwise be able to mint a credential. Without the app label there is
// no app to scope it to, and this package never writes such a Secret — so it is
// refused rather than treated as admin.
func TestResolveIgnoresASecretWithoutAnAppLabel(t *testing.T) {
	store, client := newStore(t)
	ctx := context.Background()

	key, _ := store.Create(ctx, "shop")

	// Take the real digest off shop's own Secret, so the forgery collides with
	// the genuine lookup rather than with a value this test computed itself.
	real, err := client.CoreV1().Secrets(namespace).Get(ctx, appkey.Name("shop"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get shop's secret: %v", err)
	}
	digest := real.Labels["applab.io/key-digest"]
	if digest == "" {
		t.Fatal("shop's Secret carries no digest label")
	}

	// A hand-made Secret whose digest matches shop's key but which names no app.
	forged := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "forged",
			Namespace: namespace,
			Labels:    map[string]string{"applab.io/key-digest": digest},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"key": []byte(key)},
	}
	if _, err := client.CoreV1().Secrets(namespace).Create(ctx, forged, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create forged secret: %v", err)
	}

	// Deleting shop's key leaves only the forged Secret, which must not resolve.
	if err := store.Remove(ctx, "shop"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if app, ok, err := store.ResolveAppKey(ctx, key); err != nil {
		t.Fatalf("resolve: %v", err)
	} else if ok {
		t.Errorf("a Secret with no %s label resolved to app %q; the app label is what scopes the key", k8s.LabelApp, app)
	}
}

// TestRemoveIsIdempotent asserts removing a key that is not there is not an
// error, so teardown can call it unconditionally.
func TestRemoveIsIdempotent(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	if err := store.Remove(ctx, "nothing"); err != nil {
		t.Errorf("removing a non-existent key returned %v, want nil", err)
	}

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

// TestSecretCarriesTheLabelsTheSystemDependsOn asserts the two labels that carry
// weight elsewhere: the app label, which is how deleting an app removes its key,
// and the digest label, which is how a key is found.
func TestSecretCarriesTheLabelsTheSystemDependsOn(t *testing.T) {
	store, client := newStore(t)
	ctx := context.Background()

	if _, err := store.Create(ctx, "shop"); err != nil {
		t.Fatalf("create key: %v", err)
	}

	secret, err := client.CoreV1().Secrets(namespace).Get(ctx, appkey.Name("shop"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}

	// The exact label k8s.Client.DeleteAppObjects selects on, so that deleting
	// an app deletes its key. If this constant ever changes, the key would be
	// orphaned rather than removed.
	if got := secret.Labels[k8s.LabelApp]; got != "shop" {
		t.Errorf("the key Secret carries %s=%q, want shop — deleting an app removes its key by this label",
			k8s.LabelApp, got)
	}
	if secret.Labels["applab.io/key-digest"] == "" {
		t.Error("the key Secret has no digest label, so Resolve cannot find it")
	}
}

// TestDigestLabelFitsAKubernetesLabelValue guards the bug that made the first
// version of this unshippable.
//
// A label value may not exceed 63 bytes and a sha256 hex digest is 64, so
// storing the whole digest makes the Secret impossible to create — against a
// real API server, and only there. The check is the API's own rule rather than a
// length comparison, so it cannot drift from what the server enforces.
func TestDigestLabelFitsAKubernetesLabelValue(t *testing.T) {
	store, client := newStore(t)
	ctx := context.Background()

	// Creating through the store is the real test: a label the server rejects
	// would make this fail.
	if _, err := store.Create(ctx, "shop"); err != nil {
		t.Fatalf("create: %v", err)
	}

	secret, err := client.CoreV1().Secrets(namespace).Get(ctx, appkey.Name("shop"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	digest := secret.Labels["applab.io/key-digest"]
	if len(digest) > 63 {
		t.Errorf("the digest label is %d bytes; Kubernetes allows at most 63, so this Secret cannot be created", len(digest))
	}
	if errs := validation.IsValidLabelValue(digest); len(errs) > 0 {
		t.Errorf("the digest label %q is not a valid label value: %v", digest, errs)
	}
}

// TestCreateRejectsAnEmptyAppID asserts a missing id fails loudly rather than
// writing a Secret named for nothing.
func TestCreateRejectsAnEmptyAppID(t *testing.T) {
	store, _ := newStore(t)

	if _, err := store.Create(context.Background(), ""); err == nil {
		t.Error("creating a key with no app id should fail")
	}
}

// TestNameIsDerivedFromTheAppID asserts the naming rule is one function, so a
// caller and the store cannot disagree about where a key lives.
func TestNameIsDerivedFromTheAppID(t *testing.T) {
	if got, want := appkey.Name("shop"), "applab-key-shop"; got != want {
		t.Errorf("Name(shop) = %q, want %q", got, want)
	}
}

// TestReadyReflectsWhetherAClientIsAttached covers the no-cluster deployment,
// where the API layer must report the capability as unavailable.
func TestReadyReflectsWhetherAClientIsAttached(t *testing.T) {
	if !appkey.New(fake.NewSimpleClientset(), namespace).Ready() {
		t.Error("a store with a client should be ready")
	}
	if appkey.New(nil, namespace).Ready() {
		t.Error("a store with no client is not ready")
	}
}
