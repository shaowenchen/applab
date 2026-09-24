package k8s

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

const testNS = "ops-system"

func testClient(t *testing.T, objs ...interface{}) (*Client, *fake.Clientset) {
	t.Helper()
	client := fake.NewSimpleClientset()
	for _, obj := range objs {
		switch o := obj.(type) {
		case *appsv1.Deployment:
			if _, err := client.AppsV1().Deployments(o.Namespace).Create(context.Background(), o, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed deployment: %v", err)
			}
		case *corev1.Service:
			if _, err := client.CoreV1().Services(o.Namespace).Create(context.Background(), o, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed service: %v", err)
			}
		case *networkingv1.Ingress:
			if _, err := client.NetworkingV1().Ingresses(o.Namespace).Create(context.Background(), o, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed ingress: %v", err)
			}
		case *batchv1.Job:
			if _, err := client.BatchV1().Jobs(o.Namespace).Create(context.Background(), o, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed job: %v", err)
			}
		case *corev1.Secret:
			if _, err := client.CoreV1().Secrets(o.Namespace).Create(context.Background(), o, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed secret: %v", err)
			}
		default:
			t.Fatalf("unhandled seed type %T", obj)
		}
	}
	return NewWithClientset(client, testNS), client
}

func appLabelsFor(appID string) map[string]string {
	return map[string]string{"applab.io/app": appID, "app.kubernetes.io/managed-by": "applab"}
}

func deploymentFor(appID, ns, name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: appLabelsFor(appID)},
	}
}

// The namespace is the one AppLab runs in, whatever app is named. This is what
// replaced "one namespace per app".
func TestNamespaceIgnoresTheAppID(t *testing.T) {
	c, _ := testClient(t)

	for _, appID := range []string{"shop", "blog", ""} {
		if got := c.Namespace(appID); got != testNS {
			t.Errorf("Namespace(%q) = %q, want %q", appID, got, testNS)
		}
	}
}

func TestOwnsNamespaceAcceptsOnlyItsOwn(t *testing.T) {
	c, _ := testClient(t)

	for _, tc := range []struct {
		namespace string
		want      bool
	}{
		{testNS, true},
		{"", false}, // Kubernetes reads "" as the default namespace
		{"default", false},
		{"kube-system", false},
		{"applab-shop", false}, // the model this replaced
		{"ops-system-2", false},
	} {
		if got := c.OwnsNamespace(tc.namespace); got != tc.want {
			t.Errorf("OwnsNamespace(%q) = %v, want %v", tc.namespace, got, tc.want)
		}
	}
}

// A client with no namespace owns nothing. Every string comparison against ""
// is false, so this falls out of the equality — but it is the failure that would
// point every operation at the default namespace, so it is asserted.
func TestOwnsNamespaceWithNoNamespaceConfigured(t *testing.T) {
	c := NewWithClientset(fake.NewSimpleClientset(), "")

	for _, ns := range []string{"", "default", "ops-system", "kube-system"} {
		if c.OwnsNamespace(ns) {
			t.Errorf("OwnsNamespace(%q) = true with no namespace configured", ns)
		}
	}
}

// The most important test here: deleting an app must remove that app's objects
// and nothing else. Every app shares one namespace, so the label is the only
// thing separating them — and AppLab's own Deployment sits in the same namespace
// with no app label at all.
func TestDeleteAppObjectsRemovesOnlyThatApp(t *testing.T) {
	ctx := context.Background()

	_, client := testClient(t,
		// The app being deleted.
		deploymentFor("shop", testNS, "app-shop"),
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "app-shop", Namespace: testNS, Labels: appLabelsFor("shop")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "applab-build-shop-1-token", Namespace: testNS, Labels: appLabelsFor("shop")}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "applab-build-shop-1", Namespace: testNS, Labels: appLabelsFor("shop")}},

		// Another app, in the same namespace.
		deploymentFor("blog", testNS, "app-blog"),
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "app-blog", Namespace: testNS, Labels: appLabelsFor("blog")}},

		// AppLab itself: same namespace, no app label.
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "applab", Namespace: testNS}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "applab", Namespace: testNS}},
	)

	c := NewWithClientset(client, testNS)
	if err := c.DeleteAppObjects(ctx, "shop"); err != nil {
		t.Fatalf("DeleteAppObjects: %v", err)
	}

	// The app's own objects are gone.
	if _, err := client.AppsV1().Deployments(testNS).Get(ctx, "app-shop", metav1.GetOptions{}); err == nil {
		t.Error("the app's Deployment survived")
	}
	if _, err := client.CoreV1().Services(testNS).Get(ctx, "app-shop", metav1.GetOptions{}); err == nil {
		t.Error("the app's Service survived")
	}
	if _, err := client.BatchV1().Jobs(testNS).Get(ctx, "applab-build-shop-1", metav1.GetOptions{}); err == nil {
		t.Error("the app's build Job survived")
	}
	if _, err := client.CoreV1().Secrets(testNS).Get(ctx, "applab-build-shop-1-token", metav1.GetOptions{}); err == nil {
		t.Error("the app's build token Secret survived")
	}

	// And nothing else was touched.
	if _, err := client.AppsV1().Deployments(testNS).Get(ctx, "app-blog", metav1.GetOptions{}); err != nil {
		t.Errorf("another app's Deployment was deleted: %v", err)
	}
	if _, err := client.CoreV1().Services(testNS).Get(ctx, "app-blog", metav1.GetOptions{}); err != nil {
		t.Errorf("another app's Service was deleted: %v", err)
	}
	if _, err := client.AppsV1().Deployments(testNS).Get(ctx, "applab", metav1.GetOptions{}); err != nil {
		t.Errorf("applab's own Deployment was deleted: %v", err)
	}
	if _, err := client.CoreV1().Services(testNS).Get(ctx, "applab", metav1.GetOptions{}); err != nil {
		t.Errorf("applab's own Service was deleted: %v", err)
	}
}

// An app id that shares a prefix with another app's must not take it down. This
// is the mistake a name-based selector would make and a label-based one does
// not: deleting "shop" must not touch "shop-2".
func TestDeleteAppObjectsIsNotFooledByASharedPrefix(t *testing.T) {
	ctx := context.Background()

	_, client := testClient(t,
		deploymentFor("shop", testNS, "app-shop"),
		deploymentFor("shop-2", testNS, "app-shop-2"),
	)

	c := NewWithClientset(client, testNS)
	if err := c.DeleteAppObjects(ctx, "shop"); err != nil {
		t.Fatalf("DeleteAppObjects: %v", err)
	}

	if _, err := client.AppsV1().Deployments(testNS).Get(ctx, "app-shop", metav1.GetOptions{}); err == nil {
		t.Error("the app's Deployment survived")
	}
	if _, err := client.AppsV1().Deployments(testNS).Get(ctx, "app-shop-2", metav1.GetOptions{}); err != nil {
		t.Errorf("deleting \"shop\" deleted \"shop-2\", which shares its prefix: %v", err)
	}
}

// The label check is the last thing standing between a selector bug and deleted
// data, so it is exercised directly. A client that returned objects it was not
// asked for — a broken selector, a mislabeled object — must be refused rather
// than obeyed.
func TestCheckAppLabelsRefusesAnotherAppsObject(t *testing.T) {
	err := checkAppLabels("deployment", "shop", testNS, []metav1.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app-shop", Labels: appLabelsFor("shop")}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app-blog", Labels: appLabelsFor("blog")}},
	})
	if err == nil {
		t.Fatal("expected a refusal when an object belongs to another app")
	}
	if !strings.Contains(err.Error(), "app-blog") {
		t.Errorf("the refusal should name the object it refused, got: %v", err)
	}
}

func TestCheckAppLabelsAcceptsMatchingObjects(t *testing.T) {
	err := checkAppLabels("deployment", "shop", testNS, []metav1.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app-shop", Labels: appLabelsFor("shop")}},
	})
	if err != nil {
		t.Errorf("objects labeled for the app should be accepted, got: %v", err)
	}
}

// Deleting an app is not an escape hatch for reaching another namespace.
func TestDeleteAppObjectsRefusesAForeignNamespace(t *testing.T) {
	client := fake.NewSimpleClientset()
	c := NewWithClientset(client, "ops-system")
	// A client that would compute another namespace, as the old per-app model
	// did, must not be able to use this to delete there.
	c.namespace = ""

	err := c.DeleteAppObjects(context.Background(), "kube-system")
	if err == nil {
		t.Fatal("expected a refusal for a namespace this installation does not own")
	}
	if !strings.Contains(err.Error(), "not this installation's namespace") {
		t.Errorf("the refusal should say why, got: %v", err)
	}
}

// An app with nothing deployed is deleted cleanly rather than failing on the
// absence of objects that were never created.
func TestDeleteAppObjectsWithNothingDeployed(t *testing.T) {
	_, client := testClient(t)
	c := NewWithClientset(client, testNS)

	if err := c.DeleteAppObjects(context.Background(), "never-deployed"); err != nil {
		t.Errorf("deleting an app with no objects should succeed, got: %v", err)
	}
}

// Ready reads a namespaced resource, so a Role missing a rule — or a namespace
// that does not exist — shows up here rather than at the first deploy.
func TestReadyWithAnUnreachableCluster(t *testing.T) {
	// A clientset whose reactors all fail, standing in for an unreachable API
	// server or a Role that grants nothing.
	client := fake.NewSimpleClientset()
	client.PrependReactor("*", "*", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errForbidden
	})

	c := NewWithClientset(client, testNS)
	if c.Ready(context.Background()) {
		t.Error("Ready reported true for a cluster it cannot read")
	}
}

var errForbidden = &forbiddenError{}

type forbiddenError struct{}

func (e *forbiddenError) Error() string { return "forbidden" }
