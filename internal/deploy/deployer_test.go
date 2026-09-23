package deploy

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/shaowenchen/applab/internal/model"
)

func testConfig() Config {
	return Config{
		BaseDomain: "apps.example.com",
	}
}

func testApp() *model.App {
	return &model.App{
		ID:         "shop",
		Namespace:  "applab-shop",
		Port:       8080,
		Replicas:   2,
		Dockerfile: "Dockerfile",
		CommitSHA:  "abc123def456789012345678901234567890abcd",
	}
}

func newTestDeployer(t *testing.T, cfg Config) (*Deployer, *fake.Clientset) {
	t.Helper()

	client := fake.NewSimpleClientset()
	return New(client, cfg), client
}

// TestApplyCreatesEverything asserts a deploy produces the whole set of objects
// and that they link up.
func TestApplyCreatesEverything(t *testing.T) {
	d, client := newTestDeployer(t, testConfig())
	ctx := context.Background()
	app := testApp()
	image := "registry.example.com/apps/shop:abc123def456"

	host, err := d.Apply(ctx, app, image)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if host != "shop.apps.example.com" {
		t.Errorf("host = %q, want shop.apps.example.com", host)
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

	ingress, err := client.NetworkingV1().Ingresses(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the ingress was not created: %v", err)
	}
	if got := ingress.Spec.Rules[0].Host; got != host {
		t.Errorf("ingress host = %q, want %q", got, host)
	}
	if got := ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name; got != ObjectName(app.ID) {
		t.Errorf("the ingress points at service %q, want %q", got, ObjectName(app.ID))
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

	// The Ingress must point at the Service's port, or it routes to nothing.
	ingress, err := client.NetworkingV1().Ingresses(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}
	backendPort := ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Number

	var found bool
	for _, p := range service.Spec.Ports {
		if p.Port == backendPort {
			found = true
		}
	}
	if !found {
		t.Errorf("the ingress targets port %d, which the service does not expose (%v)", backendPort, service.Spec.Ports)
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
	ingresses, _ := client.NetworkingV1().Ingresses(app.Namespace).List(ctx, metav1.ListOptions{})
	if len(ingresses.Items) != 1 {
		t.Errorf("%d ingresses exist after repeated applies, want 1", len(ingresses.Items))
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

// TestNoIngressWithoutDomain asserts an app gets no Ingress when no base domain
// is configured. Creating one with an empty host would be a broken object, and
// leaving the app cluster-internal is a coherent choice.
func TestNoIngressWithoutDomain(t *testing.T) {
	d, client := newTestDeployer(t, Config{})
	ctx := context.Background()
	app := testApp()

	host, err := d.Apply(ctx, app, "image:tag")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if host != "" {
		t.Errorf("host = %q, want empty with no base domain", host)
	}

	ingresses, _ := client.NetworkingV1().Ingresses(app.Namespace).List(ctx, metav1.ListOptions{})
	if len(ingresses.Items) != 0 {
		t.Errorf("%d ingresses were created with no base domain configured", len(ingresses.Items))
	}
}

// TestAppDomainOverride asserts an app-level domain wins over the base domain.
func TestAppDomainOverride(t *testing.T) {
	d, client := newTestDeployer(t, testConfig())
	ctx := context.Background()

	app := testApp()
	app.Domain = "shop.acme.com"

	host, err := d.Apply(ctx, app, "image:tag")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if host != "shop.acme.com" {
		t.Errorf("host = %q, want the app's own domain", host)
	}

	ingress, _ := client.NetworkingV1().Ingresses(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{})
	if got := ingress.Spec.Rules[0].Host; got != "shop.acme.com" {
		t.Errorf("ingress host = %q, want shop.acme.com", got)
	}
}

// TestTLSSecretSharedForWildcard asserts every app references the same Secret
// when one covers them all, rather than each getting its own.
func TestTLSSecretSharedForWildcard(t *testing.T) {
	cfg := testConfig()
	cfg.TLSSecret = "wildcard-apps-example-com"

	d, client := newTestDeployer(t, cfg)
	ctx := context.Background()

	for _, id := range []string{"shop", "blog"} {
		app := testApp()
		app.ID = id
		app.Namespace = "applab-" + id
		if _, err := d.Apply(ctx, app, "image:tag"); err != nil {
			t.Fatalf("Apply %s: %v", id, err)
		}
	}

	for _, id := range []string{"shop", "blog"} {
		ingress, err := client.NetworkingV1().Ingresses("applab-"+id).Get(ctx, ObjectName(id), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get ingress for %s: %v", id, err)
		}
		if len(ingress.Spec.TLS) != 1 {
			t.Fatalf("app %s has %d TLS entries, want 1", id, len(ingress.Spec.TLS))
		}
		if got := ingress.Spec.TLS[0].SecretName; got != "wildcard-apps-example-com" {
			t.Errorf("app %s references secret %q, want the shared wildcard", id, got)
		}
	}
}

// TestPerAppCertificateWithCertManager asserts each app gets its own Secret when
// certificates are issued per app.
//
// A shared name here would be a real bug: cert-manager would see every app
// writing to one Secret and issue a certificate for whichever host it last saw,
// so the others would serve a certificate for the wrong name.
func TestPerAppCertificateWithCertManager(t *testing.T) {
	cfg := testConfig()
	cfg.ClusterIssuer = "letsencrypt"

	d, client := newTestDeployer(t, cfg)
	ctx := context.Background()

	secrets := map[string]string{}
	for _, id := range []string{"shop", "blog"} {
		app := testApp()
		app.ID = id
		app.Namespace = "applab-" + id
		if _, err := d.Apply(ctx, app, "image:tag"); err != nil {
			t.Fatalf("Apply %s: %v", id, err)
		}

		ingress, err := client.NetworkingV1().Ingresses("applab-"+id).Get(ctx, ObjectName(id), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get ingress for %s: %v", id, err)
		}
		if len(ingress.Spec.TLS) != 1 {
			t.Fatalf("app %s has no TLS block", id)
		}
		secrets[id] = ingress.Spec.TLS[0].SecretName

		if got := ingress.Annotations["cert-manager.io/cluster-issuer"]; got != "letsencrypt" {
			t.Errorf("app %s: cluster-issuer annotation = %q, want letsencrypt", id, got)
		}
	}

	if secrets["shop"] == secrets["blog"] {
		t.Errorf("both apps use the TLS secret %q; per-app certificates must not share a Secret",
			secrets["shop"])
	}
}

// TestNoTLSWithoutConfiguration asserts no tls block is emitted with nothing to
// serve it.
//
// An Ingress naming a Secret that does not exist makes the ingress controller
// serve its own self-signed certificate, which teaches a user to click through a
// browser warning — worse than plain HTTP, which is at least honest.
func TestNoTLSWithoutConfiguration(t *testing.T) {
	d, client := newTestDeployer(t, testConfig())
	ctx := context.Background()
	app := testApp()

	if _, err := d.Apply(ctx, app, "image:tag"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	ingress, _ := client.NetworkingV1().Ingresses(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{})
	if len(ingress.Spec.TLS) != 0 {
		t.Errorf("a tls block was emitted with no certificate configured: %v", ingress.Spec.TLS)
	}
	if _, ok := ingress.Annotations["cert-manager.io/cluster-issuer"]; ok {
		t.Error("a cert-manager annotation was emitted with no issuer configured")
	}
}

// TestIngressAnnotationsAreReplaced asserts a removed annotation actually
// disappears. Merging would leave a setting applab stopped applying in place
// forever.
func TestIngressAnnotationsAreReplaced(t *testing.T) {
	cfg := testConfig()
	cfg.Annotations = map[string]string{"nginx.ingress.kubernetes.io/proxy-body-size": "10m"}

	d, client := newTestDeployer(t, cfg)
	ctx := context.Background()
	app := testApp()

	if _, err := d.Apply(ctx, app, "image:tag"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Simulate an annotation set by something else, then change applab's config.
	ingress, _ := client.NetworkingV1().Ingresses(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{})
	ingress.Annotations["someone-else"] = "keep me"
	if _, err := client.NetworkingV1().Ingresses(app.Namespace).Update(ctx, ingress, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update ingress: %v", err)
	}

	cfg.Annotations = map[string]string{"nginx.ingress.kubernetes.io/proxy-body-size": "50m"}
	d = New(client, cfg)
	if _, err := d.Apply(ctx, app, "image:tag"); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	ingress, _ = client.NetworkingV1().Ingresses(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{})
	if got := ingress.Annotations["nginx.ingress.kubernetes.io/proxy-body-size"]; got != "50m" {
		t.Errorf("annotation = %q, want the updated value 50m", got)
	}
	if _, ok := ingress.Annotations["someone-else"]; ok {
		t.Error("an annotation applab does not manage survived; annotations must be replaced, not merged")
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
		ObjectMeta: metav1.ObjectMeta{Name: ObjectName("shop"), Namespace: "applab-shop"},
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
