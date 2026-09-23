package k8s

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	testPrefix = "applab-"
	testOwnNS  = "applab-system"
)

func registrySecret(name, ns, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(value)},
	}
}

// The point of CopySecret: a Secret cannot be referenced from another
// namespace, so a registry credential has to be copied into the app's.
func TestCopySecretReachesAppNamespace(t *testing.T) {
	src := registrySecret("regcred", testOwnNS, `{"auths":{}}`)
	client := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testOwnNS}},
		src,
	)
	c := NewWithClientset(client, testPrefix, testOwnNS)

	if err := c.CopySecret(context.Background(), "shop", "regcred"); err != nil {
		t.Fatalf("CopySecret: %v", err)
	}

	got, err := client.CoreV1().Secrets("applab-shop").Get(context.Background(), "regcred", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the copy should exist in the app namespace: %v", err)
	}
	if got.Type != corev1.SecretTypeDockerConfigJson {
		t.Errorf("type = %q, want %q", got.Type, corev1.SecretTypeDockerConfigJson)
	}
	if string(got.Data[corev1.DockerConfigJsonKey]) != `{"auths":{}}` {
		t.Errorf("data did not carry over: %q", got.Data)
	}
	if got.Labels["applab.io/app"] != "shop" {
		t.Errorf("the copy should record which app it belongs to, got labels %v", got.Labels)
	}
}

// A rotated credential has to reach apps that already have the old copy,
// otherwise rotating it appears to work and changes nothing.
func TestCopySecretUpdatesExistingCopy(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testOwnNS}},
		registrySecret("regcred", testOwnNS, "new-value"),
		registrySecret("regcred", "applab-shop", "stale-value"),
	)
	c := NewWithClientset(client, testPrefix, testOwnNS)

	if err := c.CopySecret(context.Background(), "shop", "regcred"); err != nil {
		t.Fatalf("CopySecret: %v", err)
	}

	got, err := client.CoreV1().Secrets("applab-shop").Get(context.Background(), "regcred", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got.Data[corev1.DockerConfigJsonKey]) != "new-value" {
		t.Errorf("the stale copy was not replaced: got %q", got.Data[corev1.DockerConfigJsonKey])
	}
}

// Metadata belongs to the object, not to the credential. Copying a uid or a
// resourceVersion from the original would describe an object that does not
// exist in the target namespace, and the API server rejects the write.
func TestCopySecretDoesNotCopySourceMetadata(t *testing.T) {
	src := registrySecret("regcred", testOwnNS, "v")
	src.UID = "11111111-2222-3333-4444-555555555555"
	src.ResourceVersion = "4321"
	src.OwnerReferences = []metav1.OwnerReference{{Kind: "Deployment", Name: "someone-else", APIVersion: "apps/v1"}}

	client := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testOwnNS}}, src)
	c := NewWithClientset(client, testPrefix, testOwnNS)

	if err := c.CopySecret(context.Background(), "shop", "regcred"); err != nil {
		t.Fatalf("CopySecret: %v", err)
	}

	got, err := client.CoreV1().Secrets("applab-shop").Get(context.Background(), "regcred", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UID == src.UID {
		t.Error("the copy inherited the source's uid")
	}
	if len(got.OwnerReferences) != 0 {
		t.Errorf("the copy inherited owner references pointing at objects in another namespace: %v", got.OwnerReferences)
	}
}

// An empty name means "this installation has no such credential", which is the
// normal case for a cluster-local registry. It must be a no-op, not a lookup of
// a Secret named "".
func TestCopySecretEmptyNameIsNoOp(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testOwnNS}})
	c := NewWithClientset(client, testPrefix, testOwnNS)

	if err := c.CopySecret(context.Background(), "shop", ""); err != nil {
		t.Fatalf("CopySecret with an empty name should do nothing, got %v", err)
	}

	list, err := client.CoreV1().Secrets("applab-shop").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("an empty name created %d secrets", len(list.Items))
	}
}

// The prefix rule is what keeps one applab installation out of another's
// namespaces, and it has to hold for secrets as much as for anything else.
//
// With a non-empty prefix the guard cannot fire — NamespaceFor always produces
// a name carrying it — so the reachable case is the empty one, where the
// namespace would be the bare app id.
func TestCopySecretRefusesNamespaceOutsidePrefix(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testOwnNS}})
	c := NewWithClientset(client, testPrefix, testOwnNS)
	c.namespacePrefix = ""

	err := c.CopySecret(context.Background(), "kube-system", "regcred")
	if err == nil {
		t.Fatal("expected a refusal, got none")
	}
	if !strings.Contains(err.Error(), "prefix") {
		t.Errorf("the refusal should name the prefix rule, got: %v", err)
	}
}

// The prefix test alone is not enough: every string has the empty string as a
// prefix, so an empty prefix would make applab own the whole cluster.
func TestOwnsNamespaceRejectsEmptyPrefix(t *testing.T) {
	c := NewWithClientset(fake.NewSimpleClientset(), "", testOwnNS)

	for _, ns := range []string{"default", "kube-system", "applab-shop", ""} {
		if c.OwnsNamespace(ns) {
			t.Errorf("OwnsNamespace(%q) = true with an empty prefix, which claims the whole cluster", ns)
		}
	}
}

func TestOwnsNamespaceAcceptsOnlyThePrefix(t *testing.T) {
	c := NewWithClientset(fake.NewSimpleClientset(), testPrefix, testOwnNS)

	for _, tc := range []struct {
		namespace string
		want      bool
	}{
		{"applab-shop", true},
		{"applab-", false},     // the prefix alone names no app
		{"applab", false},      // the prefix without its separator
		{"default", false},     // someone else's
		{"my-applab-x", false}, // the prefix must be at the front
	} {
		if got := c.OwnsNamespace(tc.namespace); got != tc.want {
			t.Errorf("OwnsNamespace(%q) = %v, want %v", tc.namespace, got, tc.want)
		}
	}
}

// A named Secret that does not exist is a configuration error the operator has
// to fix, so it must surface rather than being swallowed.
func TestCopySecretMissingSourceIsAnError(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testOwnNS}})
	c := NewWithClientset(client, testPrefix, testOwnNS)

	err := c.CopySecret(context.Background(), "shop", "absent")
	if err == nil {
		t.Fatal("expected an error for a missing source Secret")
	}
	if !strings.Contains(err.Error(), "absent") {
		t.Errorf("the error should name the Secret, got: %v", err)
	}
}
