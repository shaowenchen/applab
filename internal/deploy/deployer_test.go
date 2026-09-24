package deploy

import (
	"context"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/shaowenchen/applab/internal/model"
)

func testConfig() Config {
	return Config{
		BaseDomain: "apps.example.com",
		Gateway:    "ops-system/gateway",
	}
}

func testApp() *model.App {
	return &model.App{
		ID:         "shop",
		Namespace:  "ops-system",
		Port:       8080,
		Replicas:   2,
		Dockerfile: "Dockerfile",
		CommitSHA:  "abc123def456789012345678901234567890abcd",
	}
}

func newTestDeployer(t *testing.T, cfg Config) (*Deployer, *fake.Clientset) {
	t.Helper()

	client := fake.NewSimpleClientset()
	// A bare scheme: with custom list kinds given, the fake registers the list
	// types itself. Pre-registering them here would register them as the wrong
	// Go type and the constructor panics.
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKindsForTest())
	return NewWithDynamic(client, dyn, cfg), client
}

// listKindsForTest maps each resource the tests touch to the Go type the fake
// dynamic client builds for a list of it.
//
// It is given explicitly rather than derived, because the derivation pluralises
// the kind and "Gateway" becomes "gatewaies" — which then does not match the
// "gateways" a caller asks for, and the client panics rather than returning
// nothing.
func listKindsForTest() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		virtualServiceGVR: "VirtualServiceList",
		{Group: "networking.istio.io", Version: "v1", Resource: "gateways"}: "GatewayList",
	}
}

// firstRoute returns a VirtualService's first http route, which is where the
// destination lives.
//
// unstructured's accessors cannot index into a slice by path — "http.[0]" is not
// a path it understands — so the slice is navigated here.
func firstRoute(t *testing.T, vs *unstructured.Unstructured) map[string]any {
	t.Helper()

	http, found, err := unstructured.NestedSlice(vs.Object, "spec", "http")
	if err != nil || !found || len(http) == 0 {
		t.Fatalf("the virtualservice has no http routes (err=%v, found=%v)", err, found)
	}
	entry, ok := http[0].(map[string]any)
	if !ok {
		t.Fatalf("the first http entry is %T, not a map", http[0])
	}
	routes, ok := entry["route"].([]any)
	if !ok || len(routes) == 0 {
		t.Fatalf("the first http entry has no routes: %v", entry)
	}
	route, ok := routes[0].(map[string]any)
	if !ok {
		t.Fatalf("the first route is %T, not a map", routes[0])
	}
	return route
}

// destination returns a VirtualService's routing target.
func destination(t *testing.T, vs *unstructured.Unstructured) map[string]any {
	t.Helper()

	dest, ok := firstRoute(t, vs)["destination"].(map[string]any)
	if !ok {
		t.Fatalf("the first route has no destination")
	}
	return dest
}

// virtualService reads back the VirtualService an app was published through.
func virtualService(t *testing.T, d *Deployer, appID string) *unstructured.Unstructured {
	t.Helper()
	vs, err := d.dynamic.Resource(virtualServiceGVR).Namespace("ops-system").Get(
		context.Background(), ObjectName(appID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get virtualservice for %s: %v", appID, err)
	}
	return vs
}

// TestApplyCreatesEverything asserts a deploy produces the whole set of objects
// and that they link up.
func TestApplyCreatesEverything(t *testing.T) {
	d, client := newTestDeployer(t, testConfig())
	ctx := context.Background()
	app := testApp()
	image := "registry.example.com/apps/shop:abc123def456"

	addr, err := d.Apply(ctx, app, image)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if addr.String() != "shop.apps.example.com" {
		t.Errorf("address = %q, want shop.apps.example.com", addr.String())
	}

	deployment, err := client.AppsV1().Deployments(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the deployment was not created: %v", err)
	}
	if got := deployment.Spec.Template.Spec.Containers[0].Image; got != image {
		t.Errorf("image = %q, want %q", got, image)
	}

	if _, err := client.CoreV1().Services(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{}); err != nil {
		t.Errorf("the service was not created: %v", err)
	}

	vs := virtualService(t, d, app.ID)
	hosts, _, _ := unstructured.NestedStringSlice(vs.Object, "spec", "hosts")
	if len(hosts) != 1 || hosts[0] != addr.Host {
		t.Errorf("virtualservice hosts = %v, want [%s]", hosts, addr.Host)
	}

	gateways, _, _ := unstructured.NestedStringSlice(vs.Object, "spec", "gateways")
	if len(gateways) != 1 || gateways[0] != "ops-system/gateway" {
		t.Errorf("virtualservice gateways = %v, want the configured gateway", gateways)
	}

	if dest := destination(t, vs)["host"]; dest != ObjectName(app.ID) {
		t.Errorf("the virtualservice routes to %q, want %q", dest, ObjectName(app.ID))
	}
}

// TestSelectorsLinkUp is the invariant that a running app is a reachable app.
//
// A Service selector that does not match its Deployment's pods produces an app
// that is up and unreachable — a failure that looks like nothing at all from
// outside, since the pods are healthy and the Service exists. It is checked here
// by actually applying the selector to the pod labels rather than by comparing
// the maps, so the test fails if the two ever diverge in shape.
func TestSelectorsLinkUp(t *testing.T) {
	d, client := newTestDeployer(t, testConfig())
	ctx := context.Background()
	app := testApp()

	if _, err := d.Apply(ctx, app, "registry.example.com/apps/shop:abc"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	deployment, err := client.AppsV1().Deployments(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	service, err := client.CoreV1().Services(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}

	podLabels := labels.Set(deployment.Spec.Template.Labels)

	// The Service must select the pods.
	if !labels.SelectorFromSet(service.Spec.Selector).Matches(podLabels) {
		t.Errorf("the service selector %v does not match the pod labels %v; the app would be unreachable",
			service.Spec.Selector, deployment.Spec.Template.Labels)
	}

	// The Deployment's own selector must too, or a rollout cannot adopt its pods.
	if !labels.SelectorFromSet(deployment.Spec.Selector.MatchLabels).Matches(podLabels) {
		t.Errorf("the deployment selector %v does not match its own pod template labels %v",
			deployment.Spec.Selector.MatchLabels, deployment.Spec.Template.Labels)
	}

	// The VirtualService must route to the Service's port, or it routes to
	// nothing: Istio has no default and an unmatched port is a 503.
	vs := virtualService(t, d, app.ID)
	port, ok := destination(t, vs)["port"].(map[string]any)
	if !ok {
		t.Fatal("the virtualservice does not name a destination port")
	}
	routed, ok := port["number"].(int64)
	if !ok {
		t.Fatalf("the destination port is %T, not a number", port["number"])
	}

	var found bool
	for _, p := range service.Spec.Ports {
		if int64(p.Port) == routed {
			found = true
		}
	}
	if !found {
		t.Errorf("the virtualservice routes to port %d, which the service does not expose (%v)", routed, service.Spec.Ports)
	}
}

// TestServiceTargetsAppPort asserts the Service points at the port the app
// actually listens on, since a mismatch is another way to be running and
// unreachable.
func TestServiceTargetsAppPort(t *testing.T) {
	d, client := newTestDeployer(t, testConfig())
	ctx := context.Background()

	app := testApp()
	app.Port = 3000

	if _, err := d.Apply(ctx, app, "image:tag"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	service, err := client.CoreV1().Services(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	if got := service.Spec.Ports[0].TargetPort.IntValue(); got != 3000 {
		t.Errorf("service target port = %d, want the app's port 3000", got)
	}

	deployment, _ := client.AppsV1().Deployments(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{})
	if got := deployment.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort; got != 3000 {
		t.Errorf("container port = %d, want 3000", got)
	}
}

// TestApplyIsIdempotent asserts a second deploy updates rather than failing.
// An app is redeployed whenever anything about it changes, so repeating the
// operation has to be safe — and the second call is where an immutable-field
// mistake surfaces.
func TestApplyIsIdempotent(t *testing.T) {
	d, client := newTestDeployer(t, testConfig())
	ctx := context.Background()
	app := testApp()

	for i := 0; i < 3; i++ {
		if _, err := d.Apply(ctx, app, "registry.example.com/apps/shop:abc"); err != nil {
			t.Fatalf("Apply attempt %d: %v", i+1, err)
		}
	}

	// Still exactly one of each.
	deployments, _ := client.AppsV1().Deployments(app.Namespace).List(ctx, metav1.ListOptions{})
	if len(deployments.Items) != 1 {
		t.Errorf("%d deployments exist after repeated applies, want 1", len(deployments.Items))
	}
	services, _ := client.CoreV1().Services(app.Namespace).List(ctx, metav1.ListOptions{})
	if len(services.Items) != 1 {
		t.Errorf("%d services exist after repeated applies, want 1", len(services.Items))
	}
	virtualServices, _ := d.dynamic.Resource(virtualServiceGVR).Namespace(app.Namespace).List(ctx, metav1.ListOptions{})
	if len(virtualServices.Items) != 1 {
		t.Errorf("%d virtualservices exist after repeated applies, want 1", len(virtualServices.Items))
	}
}

// TestRedeployChangesTheImage asserts the new image actually reaches the
// Deployment, which is the point of a deploy.
func TestRedeployChangesTheImage(t *testing.T) {
	d, client := newTestDeployer(t, testConfig())
	ctx := context.Background()
	app := testApp()

	if _, err := d.Apply(ctx, app, "registry.example.com/apps/shop:first"); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if _, err := d.Apply(ctx, app, "registry.example.com/apps/shop:second"); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	deployment, _ := client.AppsV1().Deployments(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{})
	if got := deployment.Spec.Template.Spec.Containers[0].Image; got != "registry.example.com/apps/shop:second" {
		t.Errorf("image = %q, want the image from the second deploy", got)
	}
}

// TestNoVirtualServiceWithoutDomain asserts an app gets no VirtualService when
// no base domain is configured. One with an empty host list is a broken object,
// and leaving the app reachable only inside the cluster is a coherent choice.
func TestNoVirtualServiceWithoutDomain(t *testing.T) {
	d, _ := newTestDeployer(t, Config{})
	ctx := context.Background()
	app := testApp()

	addr, err := d.Apply(ctx, app, "image:tag")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !addr.Empty() {
		t.Errorf("address = %q, want empty with no base domain", addr.String())
	}

	list, err := d.dynamic.Resource(virtualServiceGVR).Namespace("ops-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list virtualservices: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("%d virtualservices were created with no base domain configured", len(list.Items))
	}
}

// TestAppDomainOverride asserts an app-level domain wins over the base domain.
func TestAppDomainOverride(t *testing.T) {
	d, _ := newTestDeployer(t, testConfig())
	ctx := context.Background()

	app := testApp()
	app.Domain = "shop.acme.com"

	addr, err := d.Apply(ctx, app, "image:tag")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if addr.Host != "shop.acme.com" {
		t.Errorf("host = %q, want the app's own domain", addr.Host)
	}
	if addr.Path != "" {
		t.Errorf("path = %q; an app-level domain puts the app at that host's root", addr.Path)
	}

	hosts, _, _ := unstructured.NestedStringSlice(virtualService(t, d, app.ID).Object, "spec", "hosts")
	if len(hosts) != 1 || hosts[0] != "shop.acme.com" {
		t.Errorf("virtualservice hosts = %v, want shop.acme.com", hosts)
	}
}

// TestEveryAppAttachesToTheSameGateway asserts the gateway is shared
// infrastructure rather than something applab creates per app.
//
// A gateway carries the listeners and the certificate for the whole domain, so
// one per app would mean one certificate per app and a port bind conflict on the
// ingress gateway it attaches to.
func TestEveryAppAttachesToTheSameGateway(t *testing.T) {
	cfg := testConfig()
	cfg.Gateway = "ops-system/team-gateway"

	d, _ := newTestDeployer(t, cfg)
	ctx := context.Background()

	for _, id := range []string{"shop", "blog"} {
		app := testApp()
		app.ID = id
		if _, err := d.Apply(ctx, app, "image:tag"); err != nil {
			t.Fatalf("Apply %s: %v", id, err)
		}
	}

	for _, id := range []string{"shop", "blog"} {
		gateways, _, _ := unstructured.NestedStringSlice(virtualService(t, d, id).Object, "spec", "gateways")
		if len(gateways) != 1 || gateways[0] != "ops-system/team-gateway" {
			t.Errorf("app %s attaches to %v, want the configured gateway", id, gateways)
		}
	}

	// And applab creates no gateway of its own.
	list, err := d.dynamic.Resource(schema.GroupVersionResource{
		Group: "networking.istio.io", Version: "v1", Resource: "gateways",
	}).Namespace("ops-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list gateways: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("applab created %d gateways; the gateway is the cluster's own", len(list.Items))
	}
}

// TestVirtualServiceAnnotationsAreReplaced asserts a removed annotation actually
// disappears. Merging would leave a setting applab stopped applying in place
// forever.
func TestVirtualServiceAnnotationsAreReplaced(t *testing.T) {
	cfg := testConfig()
	cfg.Annotations = map[string]string{"istio.io/foo": "10m"}

	d, _ := newTestDeployer(t, cfg)
	ctx := context.Background()
	app := testApp()

	if _, err := d.Apply(ctx, app, "image:tag"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := virtualService(t, d, app.ID).GetAnnotations()["istio.io/foo"]; got != "10m" {
		t.Fatalf("annotation = %q, want the configured value", got)
	}

	// Change applab's config and reapply.
	cfg.Annotations = map[string]string{"istio.io/foo": "50m"}
	d = NewWithDynamic(d.client, d.dynamic, cfg)
	if _, err := d.Apply(ctx, app, "image:tag"); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	vs := virtualService(t, d, app.ID)
	if got := vs.GetAnnotations()["istio.io/foo"]; got != "50m" {
		t.Errorf("annotation = %q, want the updated value 50m", got)
	}
}

// TestVirtualServiceAnnotationsCanBeRemoved asserts that dropping an annotation
// from the configuration removes it from the object.
func TestVirtualServiceAnnotationsCanBeRemoved(t *testing.T) {
	cfg := testConfig()
	cfg.Annotations = map[string]string{"istio.io/foo": "1"}

	d, _ := newTestDeployer(t, cfg)
	ctx := context.Background()
	app := testApp()

	if _, err := d.Apply(ctx, app, "image:tag"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	cfg.Annotations = nil
	d = NewWithDynamic(d.client, d.dynamic, cfg)
	if _, err := d.Apply(ctx, app, "image:tag"); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	if _, ok := virtualService(t, d, app.ID).GetAnnotations()["istio.io/foo"]; ok {
		t.Error("an annotation applab no longer sets survived; annotations must be replaced, not merged")
	}
}

// TestAppPodsGetNoAPIToken asserts an app cannot reach the Kubernetes API.
//
// Every pod gets a service account token mounted by default, and in this
// namespace that token can read every Secret — including the API keys that
// authenticate every other app and the registry credentials. An app is arbitrary
// code from whoever pushed the source, and since apps share a namespace with
// applab there is no longer a boundary doing this job.
func TestAppPodsGetNoAPIToken(t *testing.T) {
	d, _ := newTestDeployer(t, testConfig())

	podSpec := buildDeploymentForTest(t, d, testApp()).Spec.Template.Spec
	if podSpec.AutomountServiceAccountToken == nil {
		t.Fatal("AutomountServiceAccountToken is unset, so the app gets a token by default")
	}
	if *podSpec.AutomountServiceAccountToken {
		t.Error("the app's pods are given a Kubernetes API token; they can read applab's own Secrets")
	}
}

// TestPodSecurityHardening asserts an uploaded app cannot escalate privilege.
// An app's code comes from whoever pushed the source, so it is untrusted by
// construction.
func TestPodSecurityHardening(t *testing.T) {
	d, _ := newTestDeployer(t, testConfig())

	deployment := buildDeploymentForTest(t, d, testApp())
	container := deployment.Spec.Template.Spec.Containers[0]

	if container.SecurityContext == nil {
		t.Fatal("the app container has no security context")
	}
	if container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
		t.Error("the app container allows privilege escalation")
	}
	if len(container.SecurityContext.Capabilities.Drop) == 0 {
		t.Error("the app container does not drop capabilities")
	}
	if container.Resources.Limits == nil {
		t.Error("the app container has no resource limits; a runaway app could take its node down")
	}
}

// TestAppResourcesReachTheDeployment asserts the configured per-app resource
// bounds are the ones applied.
//
// This is the setting most likely to be configured and then quietly ignored: it
// is passed through a config field, an environment variable and a render step
// before it has any effect, and a break anywhere along that chain leaves a
// plausible-looking default in place rather than failing.
func TestAppResourcesReachTheDeployment(t *testing.T) {
	d, _ := newTestDeployer(t, Config{
		BaseDomain:       "apps.example.com",
		AppCPURequest:    "250m",
		AppMemoryRequest: "256Mi",
		AppCPULimit:      "3",
		AppMemoryLimit:   "3Gi",
	})

	container := buildDeploymentForTest(t, d, testApp()).Spec.Template.Spec.Containers[0]
	res := container.Resources

	for _, tc := range []struct {
		what string
		got  string
		want string
	}{
		{"cpu request", res.Requests.Cpu().String(), "250m"},
		{"memory request", res.Requests.Memory().String(), "256Mi"},
		{"cpu limit", res.Limits.Cpu().String(), "3"},
		{"memory limit", res.Limits.Memory().String(), "3Gi"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %s, want %s", tc.what, tc.got, tc.want)
		}
	}
}

// An unset value has to fall back to a sane bound rather than to nothing: an
// app with no limits can take its node down with it.
func TestEmptyAppResourcesFallBackToDefaults(t *testing.T) {
	d, _ := newTestDeployer(t, testConfig())

	container := buildDeploymentForTest(t, d, testApp()).Spec.Template.Spec.Containers[0]
	res := container.Resources

	if res.Limits.Cpu().IsZero() || res.Limits.Memory().IsZero() {
		t.Errorf("empty configuration produced no limits: %v", res.Limits)
	}
	if res.Requests.Cpu().IsZero() || res.Requests.Memory().IsZero() {
		t.Errorf("empty configuration produced no requests: %v", res.Requests)
	}
}

// TestRollingUpdateKeepsAvailability asserts a deploy does not take the app down.
func TestRollingUpdateKeepsAvailability(t *testing.T) {
	d, _ := newTestDeployer(t, testConfig())

	deployment := buildDeploymentForTest(t, d, testApp())
	strategy := deployment.Spec.Strategy

	if strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		t.Fatalf("strategy = %q, want RollingUpdate", strategy.Type)
	}
	if strategy.RollingUpdate == nil {
		t.Fatal("no rolling update parameters")
	}
	if got := strategy.RollingUpdate.MaxUnavailable.IntValue(); got != 0 {
		t.Errorf("maxUnavailable = %d, want 0 so a deploy never takes the app below its replica count", got)
	}
	if got := strategy.RollingUpdate.MaxSurge.IntValue(); got < 1 {
		t.Errorf("maxSurge = %d, want at least 1 so a new pod starts before an old one stops", got)
	}
}

// TestReplicasReachTheDeployment asserts the app's replica count is applied, and
// that a nonsensical value does not produce a Deployment that runs nothing.
func TestReplicasReachTheDeployment(t *testing.T) {
	cases := []struct {
		name     string
		replicas int32
		want     int32
	}{
		{"normal", 3, 3},
		{"one", 1, 1},
		{"zero is corrected", 0, 1},
		{"negative is corrected", -5, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDeployer(t, testConfig())
			app := testApp()
			app.Replicas = tc.replicas

			deployment := buildDeploymentForTest(t, d, app)
			if got := *deployment.Spec.Replicas; got != tc.want {
				t.Errorf("replicas = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestStatusReported asserts the cluster's verdict is translated, since that is
// what a caller reads to decide whether the app came up.
func TestStatusReported(t *testing.T) {
	cases := []struct {
		name          string
		deployment    *appsv1.Deployment
		wantFound     bool
		wantAvailable bool
	}{
		{
			name:          "available",
			deployment:    deploymentWithStatus(2, 2, appsv1.DeploymentAvailable, corev1.ConditionTrue, ""),
			wantFound:     true,
			wantAvailable: true,
		},
		{
			name:          "rolling out",
			deployment:    deploymentWithStatus(2, 1, appsv1.DeploymentAvailable, corev1.ConditionFalse, ""),
			wantFound:     true,
			wantAvailable: false,
		},
		{
			name: "cannot progress",
			deployment: deploymentWithStatus(2, 0, appsv1.DeploymentProgressing, corev1.ConditionFalse,
				"ProgressDeadlineExceeded"),
			wantFound:     true,
			wantAvailable: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(tc.deployment)
			d := New(client, testConfig())

			app := testApp()
			app.Namespace = tc.deployment.Namespace

			status, err := d.Status(context.Background(), app)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if status.Found != tc.wantFound {
				t.Errorf("Found = %v, want %v", status.Found, tc.wantFound)
			}
			if status.Available != tc.wantAvailable {
				t.Errorf("Available = %v, want %v", status.Available, tc.wantAvailable)
			}
		})
	}
}

// TestStatusOfUndeployedApp asserts an app that was never deployed reports as
// such rather than as an error, since that is a normal state.
func TestStatusOfUndeployedApp(t *testing.T) {
	d, _ := newTestDeployer(t, testConfig())

	status, err := d.Status(context.Background(), testApp())
	if err != nil {
		t.Fatalf("Status of an undeployed app returned an error: %v", err)
	}
	if status.Found {
		t.Error("Found = true for an app with no deployment")
	}
}

// TestRemoveDeletesEverything asserts stopping an app removes all three objects
// and tolerates ones that are already gone.
func TestRemoveDeletesEverything(t *testing.T) {
	d, client := newTestDeployer(t, testConfig())
	ctx := context.Background()
	app := testApp()

	if _, err := d.Apply(ctx, app, "image:tag"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := d.Remove(ctx, app); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, err := client.AppsV1().Deployments(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Error("the deployment still exists")
	}
	if _, err := client.CoreV1().Services(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Error("the service still exists")
	}
	if _, err := client.NetworkingV1().Ingresses(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Error("the ingress still exists")
	}

	// Removing again must not fail: an app stopped twice is not an error.
	if err := d.Remove(ctx, app); err != nil {
		t.Errorf("Remove on an already-removed app: %v", err)
	}
}

// TestRestartChangesThePodTemplate asserts a restart produces a template change,
// which is what actually triggers a rollout. An annotation on the Deployment
// itself would change nothing.
func TestRestartChangesThePodTemplate(t *testing.T) {
	d, client := newTestDeployer(t, testConfig())
	ctx := context.Background()
	app := testApp()

	if _, err := d.Apply(ctx, app, "image:tag"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	before, _ := client.AppsV1().Deployments(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{})
	beforeStamp := before.Spec.Template.Annotations["applab.io/restarted-at"]

	if err := d.Restart(ctx, app); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	after, _ := client.AppsV1().Deployments(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{})
	afterStamp := after.Spec.Template.Annotations["applab.io/restarted-at"]

	if afterStamp == "" {
		t.Fatal("restart did not stamp the pod template; no rollout would happen")
	}
	if afterStamp == beforeStamp {
		t.Error("restart left the pod template unchanged, so nothing would roll")
	}
}

// TestRestartOfUndeployedAppIsAnError asserts restarting something that is not
// running says so, rather than doing nothing successfully.
func TestRestartOfUndeployedAppIsAnError(t *testing.T) {
	d, _ := newTestDeployer(t, testConfig())

	err := d.Restart(context.Background(), testApp())
	if err == nil {
		t.Fatal("restarting an app that was never deployed succeeded")
	}
	if !strings.Contains(err.Error(), "not been deployed") {
		t.Errorf("the error should say the app is not deployed, got: %v", err)
	}
}

// --- helpers ---------------------------------------------------------------

// buildDeploymentForTest applies an app and returns the resulting Deployment.
func buildDeploymentForTest(t *testing.T, d *Deployer, app *model.App) *appsv1.Deployment {
	t.Helper()

	ctx := context.Background()
	if _, err := d.Apply(ctx, app, "image:tag"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	client := d.client.(*fake.Clientset)
	deployment, err := client.AppsV1().Deployments(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	return deployment
}

func deploymentWithStatus(replicas, ready int32, conditionType appsv1.DeploymentConditionType, status corev1.ConditionStatus, reason string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: ObjectName("shop"), Namespace: "ops-system"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "image:tag"}},
				},
			},
		},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: ready,
			Conditions: []appsv1.DeploymentCondition{{
				Type:   conditionType,
				Status: status,
				Reason: reason,
			}},
		},
	}
}

// pathPrefixConfig is a deployment that serves every app from one host.
func pathPrefixConfig() Config {
	return Config{
		BaseDomain: "www.example.com",
		PathPrefix: "/apps",
		Gateway:    "ops-system/gateway",
	}
}

// httpEntries returns a VirtualService's http routes, in order.
func httpEntries(t *testing.T, vs *unstructured.Unstructured) []map[string]any {
	t.Helper()

	http, found, err := unstructured.NestedSlice(vs.Object, "spec", "http")
	if err != nil || !found {
		t.Fatalf("the virtualservice has no http routes (err=%v, found=%v)", err, found)
	}
	out := make([]map[string]any, 0, len(http))
	for _, raw := range http {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("an http entry is %T, not a map", raw)
		}
		out = append(out, entry)
	}
	return out
}

// matchPrefix returns the uri prefix an http entry matches on.
func matchPrefix(t *testing.T, entry map[string]any) string {
	t.Helper()

	matches, ok := entry["match"].([]any)
	if !ok || len(matches) == 0 {
		t.Fatalf("http entry has no match: %v", entry)
	}
	m, ok := matches[0].(map[string]any)
	if !ok {
		t.Fatalf("match is %T, not a map", matches[0])
	}
	uri, ok := m["uri"].(map[string]any)
	if !ok {
		t.Fatalf("match has no uri: %v", m)
	}
	s, _ := uri["prefix"].(string)
	return s
}

// TestPathPrefixRoutesByPath asserts a shared path prefix puts every app on one
// host and tells them apart by path, stripping the prefix on the way through.
func TestPathPrefixRoutesByPath(t *testing.T) {
	d, _ := newTestDeployer(t, pathPrefixConfig())
	ctx := context.Background()
	app := testApp()

	addr, err := d.Apply(ctx, app, "image:tag")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if addr.Host != "www.example.com" {
		t.Errorf("host = %q, want the shared host", addr.Host)
	}
	if addr.Path != "/apps/shop" {
		t.Errorf("path = %q, want /apps/shop", addr.Path)
	}
	if got := addr.URL("https"); got != "https://www.example.com/apps/shop" {
		t.Errorf("url = %q", got)
	}

	vs := virtualService(t, d, app.ID)
	hosts, _, _ := unstructured.NestedStringSlice(vs.Object, "spec", "hosts")
	if len(hosts) != 1 || hosts[0] != "www.example.com" {
		t.Errorf("hosts = %v, want the one shared host", hosts)
	}

	entries := httpEntries(t, vs)
	if len(entries) != 2 {
		t.Fatalf("got %d http entries, want a redirect and a route", len(entries))
	}

	// The serving route matches a prefix that ends in a slash and rewrites the
	// prefix away, so the app sees the paths it would see at a root.
	serve := entries[1]
	if got := matchPrefix(t, serve); got != "/apps/shop/" {
		t.Errorf("serving prefix = %q, want /apps/shop/", got)
	}
	rewrite, ok := serve["rewrite"].(map[string]any)
	if !ok {
		t.Fatalf("the serving route has no rewrite: %v", serve)
	}
	if rewrite["uri"] != "/" {
		t.Errorf("rewrite uri = %v, want /", rewrite["uri"])
	}

	// The app is told where it is mounted, which is how a sub-path-hosted app
	// builds correct absolute links.
	headers, _ := serve["headers"].(map[string]any)
	request, _ := headers["request"].(map[string]any)
	set, _ := request["set"].(map[string]any)
	if set["X-Forwarded-Prefix"] != "/apps/shop" {
		t.Errorf("X-Forwarded-Prefix = %v, want /apps/shop", set["X-Forwarded-Prefix"])
	}
}

// TestPathPrefixDoesNotMatchANeighbouringApp is the reason the serving prefix
// ends in a slash.
//
// Istio's prefix match is a plain string prefix, not a path-segment match, so
// "/apps/shop" also matches "/apps/shop-2/cart" — and the request would then be
// rewritten to "/-2/cart" and served by whichever app won, which with
// cross-VirtualService ordering undefined is not even deterministic.
func TestPathPrefixDoesNotMatchANeighbouringApp(t *testing.T) {
	for _, neighbour := range []string{
		"/apps/shop-2/cart", // a longer id sharing the prefix
		"/apps/shopfront",   // an id that merely starts with it
	} {
		if strings.HasPrefix(neighbour, "/apps/shop/") {
			t.Errorf("%q would be claimed by shop, but that is a different app", neighbour)
		}
	}

	// And the prefix applab actually builds is the one that survives that.
	addr := model.Address{Host: "www.example.com", Path: "/apps/shop"}
	if got := addr.RoutePath(); got != "/apps/shop/" {
		t.Errorf("RoutePath = %q, want a trailing slash", got)
	}
	if strings.HasPrefix("/apps/shop-2/cart", addr.RoutePath()) {
		t.Error("the route prefix still claims a neighbouring app's path")
	}
}

// TestPathPrefixRedirectsTheBarePath asserts "/apps/shop" without its trailing
// slash is redirected rather than served.
//
// Relative links resolve against the request path, so serving the bare path
// would make a link to "cart" resolve to "/apps/cart" — the neighbouring app.
func TestPathPrefixRedirectsTheBarePath(t *testing.T) {
	d, _ := newTestDeployer(t, pathPrefixConfig())
	ctx := context.Background()
	app := testApp()

	if _, err := d.Apply(ctx, app, "image:tag"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	entries := httpEntries(t, virtualService(t, d, app.ID))
	first := entries[0]

	matches, _ := first["match"].([]any)
	m, _ := matches[0].(map[string]any)
	uri, _ := m["uri"].(map[string]any)
	if uri["exact"] != "/apps/shop" {
		t.Errorf("the first route matches uri %v, want an exact match on /apps/shop", uri)
	}

	redirect, ok := first["redirect"].(map[string]any)
	if !ok {
		t.Fatalf("the first route is not a redirect: %v", first)
	}
	if redirect["uri"] != "/apps/shop/" {
		t.Errorf("redirects to %v, want /apps/shop/", redirect["uri"])
	}
	// A permanent redirect: the correct URL for an app never changes, so a
	// cached 301 costs nothing and saves a round trip on every later request.
	if code := redirect["redirectCode"]; code != int64(301) {
		t.Errorf("redirectCode = %v, want 301", code)
	}
}

// TestNoPathPrefixKeepsPerAppHosts asserts the original behaviour is unchanged
// when no prefix is configured: one host per app, no path, no rewrite.
func TestNoPathPrefixKeepsPerAppHosts(t *testing.T) {
	d, _ := newTestDeployer(t, testConfig())
	ctx := context.Background()
	app := testApp()

	addr, err := d.Apply(ctx, app, "image:tag")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if addr.Host != "shop.apps.example.com" {
		t.Errorf("host = %q", addr.Host)
	}
	if addr.Path != "" {
		t.Errorf("path = %q, want empty with no prefix configured", addr.Path)
	}

	entries := httpEntries(t, virtualService(t, d, app.ID))
	if len(entries) != 1 {
		t.Fatalf("got %d http entries, want one catch-all route", len(entries))
	}
	if _, hasMatch := entries[0]["match"]; hasMatch {
		t.Error("the route has a match; an app with its own host answers every path")
	}
	if _, hasRewrite := entries[0]["rewrite"]; hasRewrite {
		t.Error("the route rewrites the path; there is no prefix to strip")
	}
}

// --- app configuration -----------------------------------------------------

// staticSecrets is a SecretReader that returns a fixed map.
//
// The deployer only ever calls Contents, so a fake this thin is enough — and it
// keeps these tests about what the deployer builds rather than about the Secret
// store, which internal/appconfig tests separately.
type staticSecrets map[string]string

func (s staticSecrets) Contents(_ context.Context, _ string) (map[string]string, error) {
	return s, nil
}

// TestEnvVarsReachTheContainer asserts an app's variables are applied.
func TestEnvVarsReachTheContainer(t *testing.T) {
	cfg := testConfig()
	cfg.Secrets = staticSecrets{}
	d, client := newTestDeployer(t, cfg)
	ctx := context.Background()

	app := testApp()
	app.Env = map[string]string{"LOG_LEVEL": "debug", "FEATURE_X": "on"}

	if _, err := d.Apply(ctx, app, "registry.example.com/apps/shop:abc"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	deployment := deploymentFor(t, client, app)
	env := containerEnv(deployment)

	if env["LOG_LEVEL"] != "debug" || env["FEATURE_X"] != "on" {
		t.Errorf("env = %v, want LOG_LEVEL=debug and FEATURE_X=on", env)
	}
	// PORT is still set from the app, and still wins by being the only
	// declaration of it.
	if env["PORT"] != "8080" {
		t.Errorf("PORT = %q, want 8080", env["PORT"])
	}
}

// TestEnvVarsAreSortedInThePodTemplate is the reason appEnv sorts.
//
// The pod template is what upsertDeployment compares to decide whether anything
// changed, and Go randomises map iteration. An unsorted env list would produce a
// different template on every deploy of an unchanged app — so every deploy would
// roll the app for no reason, and the config hash couldn't tell "changed" from
// "reshuffled".
func TestEnvVarsAreSortedInThePodTemplate(t *testing.T) {
	cfg := testConfig()
	cfg.Secrets = staticSecrets{}
	d, client := newTestDeployer(t, cfg)
	ctx := context.Background()

	app := testApp()
	app.Env = map[string]string{"ZED": "1", "ALPHA": "2", "MID": "3"}

	if _, err := d.Apply(ctx, app, "registry.example.com/apps/shop:abc"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	deployment := deploymentFor(t, client, app)
	var names []string
	for _, e := range deployment.Spec.Template.Spec.Containers[0].Env {
		names = append(names, e.Name)
	}

	want := "PORT,ALPHA,MID,ZED"
	if strings.Join(names, ",") != want {
		t.Errorf("env order = %v, want %s — the pod template must be stable across deploys", names, want)
	}
}

// TestSecretValueNeverReachesTheDeployment is the central claim of this feature.
//
// It is asserted against the object the deployer actually produces, and by
// searching the whole thing rather than the field the value would be expected
// in: a secret that leaked into a label, an annotation or a probe would be just
// as exposed as one in the env list, and checking only the env list would not
// notice.
func TestSecretValueNeverReachesTheDeployment(t *testing.T) {
	const secretValue = "postgres://user:hunter2@db.internal:5432/app"

	cfg := testConfig()
	cfg.Secrets = staticSecrets{"DATABASE_URL": secretValue}
	d, client := newTestDeployer(t, cfg)
	ctx := context.Background()

	app := testApp()
	if _, err := d.Apply(ctx, app, "registry.example.com/apps/shop:abc"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	deployment := deploymentFor(t, client, app)
	rendered := fmt.Sprintf("%+v", deployment)

	if strings.Contains(rendered, secretValue) {
		t.Fatalf("the secret's value appears in the Deployment:\n%s", rendered)
	}
	// Nor any fragment of it, in case a partial rendering is what leaked.
	if strings.Contains(rendered, "hunter2") {
		t.Fatalf("part of the secret's value appears in the Deployment:\n%s", rendered)
	}
}

// TestSecretIsReferencedByEnvFrom asserts the value gets to the container by
// reference rather than by being written down.
func TestSecretIsReferencedByEnvFrom(t *testing.T) {
	cfg := testConfig()
	cfg.Secrets = staticSecrets{"DATABASE_URL": "postgres://x"}
	d, client := newTestDeployer(t, cfg)
	ctx := context.Background()

	app := testApp()
	if _, err := d.Apply(ctx, app, "registry.example.com/apps/shop:abc"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	container := deploymentFor(t, client, app).Spec.Template.Spec.Containers[0]
	if len(container.EnvFrom) != 1 {
		t.Fatalf("got %d envFrom entries, want 1", len(container.EnvFrom))
	}
	ref := container.EnvFrom[0].SecretRef
	if ref == nil || ref.Name != "applab-env-shop" {
		t.Fatalf("envFrom references %v, want applab-env-shop", ref)
	}
	// Required, not optional: an optional reference to a missing Secret would
	// start the app with no configuration and no error.
	if ref.Optional != nil && *ref.Optional {
		t.Error("the secret reference is optional; a missing Secret would then start the app with no configuration and no error")
	}
}

// TestNoSecretMeansNoEnvFrom covers the app that has no secrets.
//
// An envFrom naming a Secret that was never created makes the pod
// unschedulable, which would turn "this app has no secrets" into an app that
// will not start.
func TestNoSecretMeansNoEnvFrom(t *testing.T) {
	cfg := testConfig()
	cfg.Secrets = staticSecrets{}
	d, client := newTestDeployer(t, cfg)
	ctx := context.Background()

	app := testApp()
	if _, err := d.Apply(ctx, app, "registry.example.com/apps/shop:abc"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	container := deploymentFor(t, client, app).Spec.Template.Spec.Containers[0]
	if len(container.EnvFrom) != 0 {
		t.Errorf("got %d envFrom entries for an app with no secrets, want none", len(container.EnvFrom))
	}
}

// TestNoClusterReaderStillDeploys covers a deployment with no cluster-backed
// secret store: environment variables still work, and nothing references a
// Secret that cannot exist.
func TestNoClusterReaderStillDeploys(t *testing.T) {
	cfg := testConfig()
	cfg.Secrets = nil
	d, client := newTestDeployer(t, cfg)
	ctx := context.Background()

	app := testApp()
	app.Env = map[string]string{"LOG_LEVEL": "debug"}

	if _, err := d.Apply(ctx, app, "registry.example.com/apps/shop:abc"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	container := deploymentFor(t, client, app).Spec.Template.Spec.Containers[0]
	if len(container.EnvFrom) != 0 {
		t.Errorf("got %d envFrom entries with no secret reader, want none", len(container.EnvFrom))
	}
	if containerEnv(deploymentFor(t, client, app))["LOG_LEVEL"] != "debug" {
		t.Error("the environment variables did not reach the container")
	}
}

// TestConfigHashChangesWhenASecretValueChanges is the subtle bug this whole
// mechanism exists to prevent.
//
// Kubernetes does not restart pods when a Secret changes, so envFrom alone
// leaves the old pods running the old value. The rollout has to be triggered by
// the pod template changing — and a hash over the secret's *names* would not
// change when a password rotates, which is exactly the case that matters.
func TestConfigHashChangesWhenASecretValueChanges(t *testing.T) {
	ctx := context.Background()
	app := testApp()

	// Same name throughout; only the value differs — the rotation case.
	cfg := testConfig()
	cfg.Secrets = staticSecrets{"TOKEN": "old"}
	d, client := newTestDeployer(t, cfg)

	if _, err := d.Apply(ctx, app, "registry.example.com/apps/shop:abc"); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	before := configHashOf(t, client, app)

	cfg.Secrets = staticSecrets{"TOKEN": "new"}
	d, client = newTestDeployer(t, cfg)
	if _, err := d.Apply(ctx, app, "registry.example.com/apps/shop:abc"); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	after := configHashOf(t, client, app)

	if before == after {
		t.Fatalf("the config hash did not change when a secret's value did (%s); a rotated password would never reach the running pods", before)
	}
}

// TestConfigHashChangesWhenAVariableChanges covers the other half.
func TestConfigHashChangesWhenAVariableChanges(t *testing.T) {
	cfg := testConfig()
	cfg.Secrets = staticSecrets{}
	d, client := newTestDeployer(t, cfg)
	ctx := context.Background()

	app := testApp()
	app.Env = map[string]string{"LOG_LEVEL": "info"}
	if _, err := d.Apply(ctx, app, "registry.example.com/apps/shop:abc"); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	before := configHashOf(t, client, app)

	app.Env = map[string]string{"LOG_LEVEL": "debug"}
	if _, err := d.Apply(ctx, app, "registry.example.com/apps/shop:abc"); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	after := configHashOf(t, client, app)

	if before == after {
		t.Errorf("the config hash did not change when a variable did (%s)", before)
	}
}

// TestConfigHashIsStableAcrossDeploys is the other direction, and matters just
// as much: an app whose configuration did not change must not roll.
//
// A hash that varied per deploy — over a map's iteration order, say — would
// restart every app on every deploy, which is a worse failure than the one the
// hash exists to fix.
func TestConfigHashIsStableAcrossDeploys(t *testing.T) {
	cfg := testConfig()
	cfg.Secrets = staticSecrets{"A": "1", "B": "2", "C": "3"}
	d, client := newTestDeployer(t, cfg)
	ctx := context.Background()

	app := testApp()
	app.Env = map[string]string{"ZED": "1", "ALPHA": "2", "MID": "3"}

	if _, err := d.Apply(ctx, app, "registry.example.com/apps/shop:abc"); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	before := configHashOf(t, client, app)

	// Applied repeatedly: map order differs between runs, so an unstable hash
	// shows up as a difference here even though nothing changed.
	for i := 0; i < 5; i++ {
		if _, err := d.Apply(ctx, app, "registry.example.com/apps/shop:abc"); err != nil {
			t.Fatalf("Apply %d: %v", i, err)
		}
	}
	after := configHashOf(t, client, app)

	if before != after {
		t.Errorf("the config hash changed across identical deploys (%s then %s), so the app would roll on every deploy", before, after)
	}
}

// TestConfigHashCoversBothHalves asserts a name reused between the two kinds of
// value is still distinguishable.
//
// Without the kind prefix in the hash, an env A=1 and a secret A=1 would
// fingerprint identically, so moving a value from one to the other would produce
// no rollout.
func TestConfigHashCoversBothHalves(t *testing.T) {
	cfg := testConfig()
	cfg.Secrets = staticSecrets{}
	d, client := newTestDeployer(t, cfg)
	ctx := context.Background()

	app := testApp()
	app.Env = map[string]string{"VALUE": "x"}
	if _, err := d.Apply(ctx, app, "registry.example.com/apps/shop:abc"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	asEnv := configHashOf(t, client, app)

	app.Env = nil
	cfg.Secrets = staticSecrets{"VALUE": "x"}
	d, client = newTestDeployer(t, cfg)
	if _, err := d.Apply(ctx, app, "registry.example.com/apps/shop:abc"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	asSecret := configHashOf(t, client, app)

	if asEnv == asSecret {
		t.Error("moving a value from a variable to a secret did not change the hash, so it would not roll")
	}
}

// TestReservedPortIsNotDeclaredTwice asserts the deployer cannot produce a pod
// spec with two PORT declarations even if the app record carries one.
//
// The API refuses to set PORT, but a record could predate that check or have
// been edited directly. A duplicate declaration is accepted by Kubernetes and
// the later one wins, so the app would listen where nothing routes and no error
// would say so.
func TestReservedPortIsNotDeclaredTwice(t *testing.T) {
	cfg := testConfig()
	cfg.Secrets = staticSecrets{}
	d, client := newTestDeployer(t, cfg)
	ctx := context.Background()

	app := testApp()
	app.Env = map[string]string{"PORT": "9999"}

	if _, err := d.Apply(ctx, app, "registry.example.com/apps/shop:abc"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	deployment := deploymentFor(t, client, app)
	seen := 0
	for _, e := range deployment.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "PORT" {
			seen++
			if e.Value != "8080" {
				t.Errorf("PORT = %q, want the app's own port 8080", e.Value)
			}
		}
	}
	if seen != 1 {
		t.Errorf("PORT is declared %d times, want exactly 1", seen)
	}
}

// --- helpers ---------------------------------------------------------------

func deploymentFor(t *testing.T, client *fake.Clientset, app *model.App) *appsv1.Deployment {
	t.Helper()
	deployment, err := client.AppsV1().Deployments(app.Namespace).Get(context.Background(), ObjectName(app.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the deployment: %v", err)
	}
	return deployment
}

func containerEnv(deployment *appsv1.Deployment) map[string]string {
	env := map[string]string{}
	for _, e := range deployment.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	return env
}

func configHashOf(t *testing.T, client *fake.Clientset, app *model.App) string {
	t.Helper()
	return deploymentFor(t, client, app).Spec.Template.Annotations["applab.io/config-hash"]
}
