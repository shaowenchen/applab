package appconfig_test

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/shaowenchen/applab/internal/appconfig"
	"github.com/shaowenchen/applab/internal/k8s"
)

const namespace = "ops-system"

func newStore(t *testing.T) (*appconfig.Store, kubernetes.Interface) {
	t.Helper()
	client := fake.NewSimpleClientset()
	return appconfig.New(client, namespace), client
}

// TestStoreWritesDataNotStringData guards the divergence that has bitten this
// codebase before.
//
// StringData is write-only sugar: a real API server folds it into Data and never
// returns it, while the fake clientset used by these tests does not fold at all.
// A store written against StringData therefore behaves one way in a test and
// another way against a cluster. Writing Data directly is what makes the object
// identical in both.
func TestStoreWritesDataNotStringData(t *testing.T) {
	store, client := newStore(t)
	ctx := context.Background()

	if _, err := store.Set(ctx, "shop", map[string]string{"DATABASE_URL": "postgres://x"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	secret, err := client.CoreV1().Secrets(namespace).Get(ctx, appconfig.Name("shop"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if len(secret.StringData) != 0 {
		t.Errorf("the Secret carries StringData %v; a real API server folds it into Data and the fake does not, so the two would disagree", secret.StringData)
	}
	if got := string(secret.Data["DATABASE_URL"]); got != "postgres://x" {
		t.Errorf("Data[DATABASE_URL] = %q, want %q", got, "postgres://x")
	}
}

// TestSecretCarriesTheAppLabel is what makes deleting an app delete its secrets.
//
// k8s.Client.DeleteAppObjects lists Secrets by this selector; a Secret without
// the label would survive the app it belongs to, leaving live credentials for an
// app that no longer exists.
func TestSecretCarriesTheAppLabel(t *testing.T) {
	store, client := newStore(t)
	ctx := context.Background()

	if _, err := store.Set(ctx, "shop", map[string]string{"TOKEN": "t"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	secret, err := client.CoreV1().Secrets(namespace).Get(ctx, appconfig.Name("shop"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := secret.Labels[k8s.LabelApp]; got != "shop" {
		t.Errorf("label %s = %q, want %q — the teardown selector would not find this Secret", k8s.LabelApp, got, "shop")
	}
}

// TestSetPreservesUnmentionedKeys is the reason Set merges rather than replaces.
//
// Without it, setting one variable would silently drop the other five, which is
// a configuration change nobody asked for and no error to explain it.
func TestSetPreservesUnmentionedKeys(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

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

	contents, err := store.Contents(ctx, "shop")
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
	store, _ := newStore(t)
	ctx := context.Background()

	if _, err := store.Set(ctx, "shop", map[string]string{"TOKEN": "old"}); err != nil {
		t.Fatalf("first set: %v", err)
	}
	if _, err := store.Set(ctx, "shop", map[string]string{"TOKEN": "new"}); err != nil {
		t.Fatalf("second set: %v", err)
	}

	contents, err := store.Contents(ctx, "shop")
	if err != nil {
		t.Fatalf("contents: %v", err)
	}
	if contents["TOKEN"] != "new" {
		t.Errorf("TOKEN = %q, want %q", contents["TOKEN"], "new")
	}
}

// TestRemoveDeletesTheSecretWhenTheLastValueGoes keeps the invariant the
// deployer relies on: a Secret that exists means the app has secrets.
//
// An empty Secret left behind would make the deployer reference it, which is
// harmless — but it would also mean "has secrets" and "has none" are the same
// observable state, and every future reader would have to check the contents
// rather than the object.
func TestRemoveDeletesTheSecretWhenTheLastValueGoes(t *testing.T) {
	store, client := newStore(t)
	ctx := context.Background()

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

	if _, err := client.CoreV1().Secrets(namespace).Get(ctx, appconfig.Name("shop"), metav1.GetOptions{}); err == nil {
		t.Error("the Secret still exists after its last value was removed")
	}
}

// TestRemoveKeepsTheSecretWhileValuesRemain is the other half of that invariant.
func TestRemoveKeepsTheSecretWhileValuesRemain(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

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
// Reporting a missing Secret as a failure would make every app without secrets
// look broken, and the API turns this into an empty list.
func TestNoSecretsIsNotAnError(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	names, err := store.Names(ctx, "nothing-configured")
	if err != nil {
		t.Fatalf("names: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("names = %v, want none", names)
	}

	has, err := store.Has(ctx, "nothing-configured")
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
	store, _ := newStore(t)
	ctx := context.Background()

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

// TestValidateName covers the two ways a name can be refused.
//
// PORT is the one that matters and is not about syntax: the deployer sets it
// from the app's port, so a second declaration would win over the port the
// Service targets and the app would run somewhere nothing routes to, with no
// error anywhere.
func TestValidateName(t *testing.T) {
	cases := []struct {
		name    string
		wantErr bool
	}{
		{"LOG_LEVEL", false},
		{"DATABASE_URL", false},
		{"has-dash", false},
		{"has.dot", false},
		{"_underscore", false},
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
	store, _ := newStore(t)
	ctx := context.Background()

	_, err := store.Set(ctx, "shop", map[string]string{appconfig.Reserved: "9999"})
	if err == nil {
		t.Fatal("Set accepted the reserved name")
	}
	if !strings.Contains(err.Error(), "port") {
		t.Errorf("the error should say where the value comes from, got: %v", err)
	}
}

// TestDeleteRemovesTheWholeSecret covers the app-deletion path.
func TestDeleteRemovesTheWholeSecret(t *testing.T) {
	store, client := newStore(t)
	ctx := context.Background()

	if _, err := store.Set(ctx, "shop", map[string]string{"A": "1", "B": "2"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := store.Delete(ctx, "shop"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := client.CoreV1().Secrets(namespace).Get(ctx, appconfig.Name("shop"), metav1.GetOptions{}); err == nil {
		t.Error("the Secret still exists after Delete")
	}

	// Deleting again is not an error: the caller asked for the app's secrets to
	// be gone, and they are.
	if err := store.Delete(ctx, "shop"); err != nil {
		t.Errorf("deleting a Secret that is already gone returned %v, want nil", err)
	}
}

// TestStoreForAnotherAppIsNotVisible asserts the lookup is by exact name.
//
// One Secret per app is the isolation between them here, so a Set for one app
// must not be readable as another's.
func TestStoreForAnotherAppIsNotVisible(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	if _, err := store.Set(ctx, "shop", map[string]string{"TOKEN": "shop-token"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	contents, err := store.Contents(ctx, "blog")
	if err != nil {
		t.Fatalf("contents: %v", err)
	}
	if len(contents) != 0 {
		t.Errorf("blog sees %v, want nothing — one Secret per app is the boundary", contents)
	}
}

// TestNamespaceIsWhereTheSecretLives asserts the store writes where it was told.
func TestNamespaceIsWhereTheSecretLives(t *testing.T) {
	store, client := newStore(t)
	ctx := context.Background()

	if _, err := store.Set(ctx, "shop", map[string]string{"A": "1"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	list, err := client.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("found %d Secrets in %s, want 1", len(list.Items), namespace)
	}
	if list.Items[0].Type != corev1.SecretTypeOpaque {
		t.Errorf("Secret type = %q, want %q", list.Items[0].Type, corev1.SecretTypeOpaque)
	}
}
