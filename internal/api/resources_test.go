package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/k8s"
	"github.com/shaowenchen/applab/internal/observe"
	"github.com/shaowenchen/applab/internal/store"
)

// App resources are the one place a caller's string reaches a Kubernetes
// quantity, so what is asserted here is mostly about the edges: that a malformed
// value is refused before it can panic the deployer, that an app's own bound
// overrides the deployment's field by field rather than as a block, and that a
// cluster which cannot report usage says so rather than reporting zero.

// TestResourcesReachTheContainer asserts an app's bounds are on the Deployment
// the deployer writes.
//
// It is asserted on the spec rather than on the response, because the response is
// the app's *settings* — a different thing from what the container runs under —
// and only the spec says the second one.
func TestResourcesReachTheContainer(t *testing.T) {
	srv, client, st := newDeployServer(t)
	h := srv.Handler()

	commit := setupAppWithCommit(t, srv, h, "shop")
	deployWithResources(t, srv, h, st, client, "shop", commit, map[string]any{
		"cpu_request":    "250m",
		"memory_request": "256Mi",
		"cpu_limit":      "1500m",
		"memory_limit":   "1Gi",
	})

	// Bound to a variable rather than read inline: ResourceList's accessors take
	// a pointer receiver, so they cannot be called on a temporary.
	res := deploymentResources(t, client, "shop")
	for _, c := range []struct{ want, got, what string }{
		{"250m", res.Requests.Cpu().String(), "cpu request"},
		{"256Mi", res.Requests.Memory().String(), "memory request"},
		{"1500m", res.Limits.Cpu().String(), "cpu limit"},
		{"1Gi", res.Limits.Memory().String(), "memory limit"},
	} {
		if c.got != c.want {
			t.Errorf("%s on the container = %q, want %q", c.what, c.got, c.want)
		}
	}
}

// TestAnAppsBoundOverridesTheDefaultFieldByField is the part that is easy to get
// wrong: the four resolve independently, so an app that raises only its memory
// limit keeps the deployment's CPU bound.
//
// A per-group fallback — "the app set resources, so use the app's" — would leave
// the other three empty, and Kubernetes reads an empty request as no reservation
// at all rather than as the operator's default.
func TestAnAppsBoundOverridesTheDefaultFieldByField(t *testing.T) {
	srv, client, st := newDeployServer(t)
	h := srv.Handler()

	commit := setupAppWithCommit(t, srv, h, "shop")
	deployWithResources(t, srv, h, st, client, "shop", commit, map[string]any{
		"memory_limit": "1Gi",
	})

	res := deploymentResources(t, client, "shop")
	if got := res.Limits.Memory().String(); got != "1Gi" {
		t.Errorf("the app's memory limit = %q, want 1Gi", got)
	}
	// The deployment's defaults, which the test server sets through
	// deploy.appResources. Empty here would mean the app's block replaced the
	// operator's settings rather than overriding one field of them.
	if got := res.Requests.Cpu().String(); got == "" || got == "0" {
		t.Errorf("cpu request = %q, but the app set none; the deployment's default should still apply", got)
	}
	if got := res.Limits.Cpu().String(); got == "" || got == "0" {
		t.Errorf("cpu limit = %q, but the app set none; the deployment's default should still apply", got)
	}
	// And the memory request, which was never set, is still the deployment's —
	// the app setting one field did not disturb the other three.
	if got := res.Requests.Memory().String(); got == "" || got == "0" {
		t.Errorf("memory request = %q, but the app set only a limit; the default should still apply", got)
	}
}

// TestAnEmptyStringClearsABound asserts the way back to the default.
//
// Without it a value, once set, could never be unset — it would have to be edited
// in the bucket by hand, which is not a thing a console user can do.
func TestAnEmptyStringClearsABound(t *testing.T) {
	srv, client, st := newDeployServer(t)
	h := srv.Handler()
	ctx := context.Background()

	commit := setupAppWithCommit(t, srv, h, "shop")

	if rec := doRequest(t, h, http.MethodPatch, "/api/v1/apps/shop", map[string]any{
		"resources": map[string]any{"memory_limit": "1Gi"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("set: %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := doRequest(t, h, http.MethodPatch, "/api/v1/apps/shop", map[string]any{
		"resources": map[string]any{"memory_limit": ""},
	}); rec.Code != http.StatusOK {
		t.Fatalf("clear: %d (%s)", rec.Code, rec.Body.String())
	}

	app, err := st.GetApp(ctx, "shop")
	if err != nil {
		t.Fatalf("read the app: %v", err)
	}
	if app.Resources.MemoryLimit != "" {
		t.Errorf("memory limit = %q after clearing it, want empty", app.Resources.MemoryLimit)
	}

	// And the container runs under the deployment's default again, which is what
	// clearing means.
	registerBuildJob(t, srv, st, "shop", commit)
	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("deploy: %d (%s)", rec.Code, rec.Body.String())
	}
	cleared := deploymentResources(t, client, "shop")
	if got := cleared.Limits.Memory().String(); got == "1Gi" {
		t.Errorf("the container still runs with the cleared limit (memory limit = %q)", got)
	}
}

// TestAMalformedQuantityIsRefused asserts the validation, and it is guarding
// something specific: the deployer parses quantities with a function that panics,
// because every other value it sees comes from AppLab's own checked
// configuration. An app's own resources are the one path a caller's string could
// reach it by, so a bad one has to be refused here or the process dies on the
// next deploy.
func TestAMalformedQuantityIsRefused(t *testing.T) {
	srv, _, _ := newDeployServer(t)
	h := srv.Handler()

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"}); rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d", rec.Code)
	}

	for _, bad := range []string{"lots", "128 Mi", "1gigabytes"} {
		rec := doRequest(t, h, http.MethodPatch, "/api/v1/apps/shop", map[string]any{
			"resources": map[string]any{"memory_limit": bad},
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("memory_limit %q = %d, want 400 (body: %s)", bad, rec.Code, rec.Body.String())
			continue
		}
		// The message has to name the field, or a caller with four of them does
		// not know which to fix.
		if !strings.Contains(rec.Body.String(), "memory_limit") {
			t.Errorf("the error for %q does not name the field: %s", bad, rec.Body.String())
		}
	}

	// And a well-formed one is accepted, so the check is not refusing everything.
	// A bare number is accepted, and it is worth naming why it appears here: the
	// console sends memory as a byte count — that is what its GiB field converts
	// to — so this is the exact spelling the console produces, and a value the
	// API refused would break the form rather than a test.
	for _, good := range []string{"512Mi", "256m", "1", "1Gi", "536870912", "500m"} {
		if rec := doRequest(t, h, http.MethodPatch, "/api/v1/apps/shop", map[string]any{
			"resources": map[string]any{"memory_limit": good},
		}); rec.Code != http.StatusOK {
			t.Errorf("the valid quantity %q was refused: %d (%s)", good, rec.Code, rec.Body.String())
		}
	}
}

// TestUsageReportsUnavailableWithoutAMetricsAPI asserts the honest empty case.
//
// A cluster with no metrics-server answers 404 for the whole resource group.
// That is not a failure — AppLab runs on such a cluster — and the endpoint has to
// say "not known" rather than "0", because a busy app shown as idle is a worse
// answer than no answer at all.
func TestUsageReportsUnavailableWithoutAMetricsAPI(t *testing.T) {
	srv, client, _ := newObserveServer(t)
	h := srv.Handler()
	createAppForObserve(t, h, "shop")

	// A Deployment with bounds, but nothing that serves metrics.k8s.io: the
	// observe test server attaches no usage reader, which is exactly the
	// no-metrics-server case.
	createAppDeployment(t, client, "shop", nil, strings.Repeat("a", 40))
	giveDeploymentBounds(t, client, "shop")

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/resources", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage: %d (%s)", rec.Code, rec.Body.String())
	}

	var usage struct {
		Available bool   `json:"available"`
		CPU       string `json:"cpu"`
		Limited   struct {
			CPU    string `json:"cpu"`
			Memory string `json:"memory"`
		} `json:"limited"`
	}
	decodeData(t, rec, &usage)

	if usage.Available {
		t.Error("usage reports as available on a cluster with no metrics API")
	}
	if usage.CPU != "" {
		t.Errorf("cpu = %q, want empty — an unknown reading is not zero", usage.CPU)
	}
	// The bounds are still reported: they come from the Deployment, which is
	// readable whether or not usage is.
	if usage.Limited.CPU == "" {
		t.Error("the limits are absent; they come from the Deployment and should be reported even without metrics")
	}
}

// TestUsageSumsAcrossPods asserts the figure is the app's rather than one pod's.
//
// What the panel answers is "how much is this app using", and with more than one
// replica that is the sum — reporting a single pod would understate a busy app by
// a factor of its replica count.
func TestUsageSumsAcrossPods(t *testing.T) {
	srv, client, _ := newObserveServer(t)
	h := srv.Handler()
	createAppForObserve(t, h, "shop")
	createAppDeployment(t, client, "shop", nil, strings.Repeat("a", 40))
	giveDeploymentBounds(t, client, "shop")

	// The real Observer over the same fake cluster, with a usage reader standing
	// in for metrics-server. The reader is the stub rather than the Observer,
	// because what is under test is the summing and the bounds lookup — not the
	// wiring of a metrics API that a fake cluster does not have.
	srv.WithObserver(observe.New(client).WithUsage(fakeUsage{pods: map[string]k8s.Usage{
		"applab-shop-1": {PodName: "applab-shop-1", CPU: "100m", Memory: "64Mi"},
		"applab-shop-2": {PodName: "applab-shop-2", CPU: "250m", Memory: "96Mi"},
	}}))

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/resources", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage: %d (%s)", rec.Code, rec.Body.String())
	}

	var usage struct {
		Available bool   `json:"available"`
		CPU       string `json:"cpu"`
		Memory    string `json:"memory"`
	}
	decodeData(t, rec, &usage)

	if !usage.Available {
		t.Fatal("usage reports as unavailable with a metrics API that answers")
	}
	// 100m + 250m = 350m, and 64Mi + 96Mi = 160Mi.
	if usage.CPU != "350m" {
		t.Errorf("cpu = %q, want 350m — the total across the app's pods", usage.CPU)
	}
	if usage.Memory != "160Mi" {
		t.Errorf("memory = %q, want 160Mi — the total across the app's pods", usage.Memory)
	}
}

// giveDeploymentBounds puts requests and limits on an app's container.
//
// createAppDeployment builds a bare container — it exists for the status tests,
// which are about conditions rather than resources — so a test that reads the
// bounds back has to put some there.
func giveDeploymentBounds(t *testing.T, client *fake.Clientset, appID string) {
	t.Helper()

	ctx := context.Background()
	deployment, err := client.AppsV1().Deployments("ops-system").Get(ctx, "applab-"+appID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the deployment for %s: %v", appID, err)
	}
	deployment.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resourceQty(t, "200m"),
			corev1.ResourceMemory: resourceQty(t, "192Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resourceQty(t, "1"),
			corev1.ResourceMemory: resourceQty(t, "512Mi"),
		},
	}
	if _, err := client.AppsV1().Deployments("ops-system").Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("set bounds on the deployment for %s: %v", appID, err)
	}
}

// resourceQty parses a quantity, failing rather than panicking.
func resourceQty(t *testing.T, s string) resource.Quantity {
	t.Helper()
	q, err := resource.ParseQuantity(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return q
}

// fakeUsage is a metrics API that answers with what the test says.
type fakeUsage struct {
	pods map[string]k8s.Usage
}

func (f fakeUsage) PodUsage(ctx context.Context, namespace, selector string) (map[string]k8s.Usage, bool, error) {
	return f.pods, true, nil
}

// deployWithResources sets an app's bounds and deploys it, so the Deployment can
// be read back.
func deployWithResources(t *testing.T, srv *api.Server, h http.Handler, st *store.Store, client *fake.Clientset, appID, commit string, resources map[string]any) {
	t.Helper()

	if rec := doRequest(t, h, http.MethodPatch, "/api/v1/apps/"+appID, map[string]any{
		"resources": resources,
	}); rec.Code != http.StatusOK {
		t.Fatalf("set resources: %d (%s)", rec.Code, rec.Body.String())
	}

	// A build has to exist for the commit, or the deploy is refused: deploying a
	// commit nothing has built is a 409 by design.
	registerBuildJob(t, srv, st, appID, commit)
	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/"+appID+"/deploy", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("deploy: %d (%s)", rec.Code, rec.Body.String())
	}
}

// deploymentResources returns the first container's resource requirements.
//
// Reading the container rather than the app's record is the point: the record
// holds what the app set, and the container holds what it runs under.
func deploymentResources(t *testing.T, client *fake.Clientset, appID string) corev1.ResourceRequirements {
	t.Helper()

	deployment, err := client.AppsV1().Deployments("ops-system").Get(
		context.Background(), "applab-"+appID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the deployment for %s: %v", appID, err)
	}
	if len(deployment.Spec.Template.Spec.Containers) == 0 {
		t.Fatalf("the deployment for %s has no containers", appID)
	}
	return deployment.Spec.Template.Spec.Containers[0].Resources
}
