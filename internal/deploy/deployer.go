// Package deploy runs an app's image in its own namespace and exposes it.
//
// Everything an app needs is derived from the app record, and every object is
// named through the same helper so the Deployment, Service, Ingress and their
// selectors cannot disagree. A selector that does not match its pods produces an
// app that is running and unreachable, which is a failure mode that looks like
// nothing at all from outside.
package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/shaowenchen/applab/internal/model"
)

// Config describes one deployment's environment for publishing apps.
type Config struct {
	// BaseDomain is the domain apps are exposed under. Empty means no
	// VirtualService is created and an app is reachable only inside the cluster.
	BaseDomain string

	// PathPrefix, when set, puts every app under one path on one shared host
	// instead of giving each its own subdomain. It is what makes a single
	// wildcard-free certificate enough for any number of apps.
	PathPrefix string

	// Gateway is the Istio gateway apps are published through, as
	// "<namespace>/<name>". The gateway is cluster infrastructure that already
	// exists — AppLab attaches to it rather than creating it — so this has to
	// name one that is really there.
	//
	// TLS is not configured here. The certificate belongs to the gateway, which
	// has the listeners and the certificate for the whole domain; an app just
	// has to be routed to.
	Gateway string

	// ImagePullSecret names a Secret holding registry credentials for pulling
	// the built image. It is in the same namespace as the Deployment, which is
	// AppLab's own, so it is referenced directly rather than copied.
	ImagePullSecret string

	// Annotations are added to every VirtualService, for Istio specifics that
	// vary by cluster.
	Annotations map[string]string

	// AppResources are applied to every app container. Empty values fall back to
	// defaults, because an app with no limits can take its node down.
	AppCPURequest    string
	AppMemoryRequest string
	AppCPULimit      string
	AppMemoryLimit   string
}

// virtualServiceGVR addresses Istio's VirtualService for the dynamic client.
//
// Istio's types are not in client-go, and depending on istio.io/api to build one
// struct would add a large module — and a version constraint against whatever
// Istio the cluster runs — for a resource AppLab writes in a dozen lines. The
// dynamic client needs only the group, version and kind.
var virtualServiceGVR = schema.GroupVersionResource{
	Group:    "networking.istio.io",
	Version:  "v1",
	Resource: "virtualservices",
}

const (
	group   = "networking.istio.io"
	version = "v1"
	kind    = "VirtualService"
)

// Deployer creates and updates an app's Kubernetes resources.
type Deployer struct {
	client  kubernetes.Interface
	dynamic dynamic.Interface
	cfg     Config
}

// New creates a Deployer.
func New(client kubernetes.Interface, cfg Config) *Deployer {
	return &Deployer{client: client, cfg: cfg}
}

// NewWithDynamic creates a Deployer that can also write Istio resources.
//
// The dynamic client is passed separately because it is built from the same
// REST config but is a different interface, and because a test that only
// exercises the Deployment and Service should not have to construct one.
func NewWithDynamic(client kubernetes.Interface, dyn dynamic.Interface, cfg Config) *Deployer {
	return &Deployer{client: client, dynamic: dyn, cfg: cfg}
}

// Ready reports whether the deployer can work.
func (d *Deployer) Ready() bool { return d.client != nil }

// Apply creates or updates every resource an app needs, and returns the
// address it is exposed at — empty when the deployment has no base domain, in
// which case no VirtualService was created and the app is reachable only from
// inside the cluster.
//
// The three objects are applied in dependency order and each is
// create-or-update: an app is redeployed whenever anything about it changes, and
// the operation has to be safe to repeat. A failure part-way leaves earlier
// objects in place, which is the correct outcome — they are consistent with each
// other, and the next attempt continues from there.
func (d *Deployer) Apply(ctx context.Context, app *model.App, image string) (model.Address, error) {
	if err := d.applyDeployment(ctx, app, image); err != nil {
		return model.Address{}, err
	}
	if err := d.applyService(ctx, app); err != nil {
		return model.Address{}, err
	}

	addr := app.Address(d.cfg.BaseDomain, d.cfg.PathPrefix)
	if addr.Empty() {
		return model.Address{}, nil
	}
	if err := d.expose(ctx, app, addr); err != nil {
		return model.Address{}, err
	}
	return addr, nil
}

// appLabels are the labels every object of an app carries.
//
// They are used as the Service and Ingress selectors, so they must be identical
// on the pods — which is what the single helper guarantees.
func appLabels(app *model.App) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "applab",
		"app.kubernetes.io/name":       app.ID,
		"applab.io/app":                app.ID,
	}
}

// selectorLabels are the subset used for selection.
//
// It is deliberately narrower than the full label set: a selector must not
// include a label whose value changes, or a rolling update would fail to match
// its own pods. The image tag is carried on the pod template as a separate
// label for that reason.
func selectorLabels(app *model.App) map[string]string {
	return map[string]string{"applab.io/app": app.ID}
}

// ObjectName is the name shared by an app's Deployment, Service and Ingress.
//
// One name for all three is not just tidiness: the Ingress points at the Service
// by name and the Service selects the Deployment's pods, so a single source for
// the name removes the possibility of a typo breaking one of those links.
func ObjectName(appID string) string { return "app-" + appID }

func (d *Deployer) applyDeployment(ctx context.Context, app *model.App, image string) error {
	namespace := app.Namespace
	name := ObjectName(app.ID)

	replicas := app.Replicas
	if replicas < 1 {
		replicas = 1
	}

	labels := appLabels(app)
	selector := selectorLabels(app)

	// The app's configuration — plain variables and secrets alike — is on the
	// app itself. There is nothing to read at deploy time: both live in the
	// app's record, and whoever asked for this deploy loaded it. The deployer
	// used to fetch the secret half from a Secret object, which is what made it
	// the one place in AppLab holding a secret's value.
	secrets := app.Secrets

	// The image tag is recorded as a pod-template label and an annotation, so a
	// rollout's progress can be told apart from a previous one and an operator
	// reading the Deployment can see which commit is running.
	podLabels := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		podLabels[k] = v
	}
	podLabels["applab.io/commit"] = shortSHA(app.CommitSHA)

	annotations := map[string]string{"applab.io/image": image}
	if app.CommitSHA != "" {
		annotations["applab.io/commit"] = app.CommitSHA
	}
	// A fingerprint of the app's configuration, so that changing configuration
	// alone triggers a rollout.
	//
	// The values themselves are in the pod template now — see appEnv — so a
	// change to one already makes the template differ and the Deployment roll.
	// The annotation is kept because it makes that change *visible*: a template
	// diff for a secret is otherwise a wall of identical-looking entries, and the
	// annotation says which deploy changed the configuration.
	//
	// It is a hash, not the values. An annotation carrying a hash of a password
	// reveals nothing; one carrying the password would be a second place to read
	// it from, and the first place is already more than there used to be.
	annotations["applab.io/config-hash"] = configHash(app.Env, secrets)

	desired := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			// RollingUpdate with a surge: a new version comes up before the old
			// one goes away, so a deploy does not take the app down. maxUnavailable
			// 0 is what makes that a guarantee rather than a hope.
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 0},
					MaxSurge:       &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
				},
			},
			// Keep a little history so a rollback via kubectl is possible for an
			// operator who cannot reach applab.
			RevisionHistoryLimit: ptr(int32(5)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: annotations,
				},
				Spec: corev1.PodSpec{
					// No API token. An app is arbitrary code from whoever pushed
					// the source, and it runs in the same namespace as AppLab
					// itself. Kubernetes mounts a service account token into every
					// pod by default, and that token can read every Secret in the
					// namespace — including the API keys that authenticate every
					// other app, and the registry credentials. An app has no
					// legitimate use for the Kubernetes API, so it does not get one.
					//
					// This is the app-side half of the single-namespace trade: the
					// namespace boundary that used to sit between an app and
					// AppLab's own credentials is gone, so the token has to go with
					// it.
					AutomountServiceAccountToken: ptr(false),
					Containers: []corev1.Container{{
						Name:  "app",
						Image: image,
						// Always, not IfNotPresent. An app's image is tagged by
						// commit, so a rebuilt image of the same commit keeps the
						// same tag — and with the default policy a node that
						// already has that tag keeps serving the old layers. The
						// deploy then reports success while the app runs the
						// previous code, which is the hardest kind of failure to
						// notice: there is no error, and the fix looks like it
						// was applied.
						ImagePullPolicy: corev1.PullAlways,
						Ports: []corev1.ContainerPort{{
							Name:          "http",
							ContainerPort: app.Port,
							Protocol:      corev1.ProtocolTCP,
						}},
						// The app's environment: its plain configuration and its
						// secrets, both written out as values.
						//
						// They used to be split — the secrets reached the
						// container through envFrom, with the kubelet doing the
						// substitution so no value appeared here. That is gone
						// with the Secret object it relied on, and the
						// consequence is worth stating where it happens: a
						// secret's value is now in this spec, readable by anyone
						// who can read the Deployment.
						Env: appEnv(app, secrets),
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resourceQty(orDefault(d.cfg.AppCPURequest, "100m")),
								corev1.ResourceMemory: resourceQty(orDefault(d.cfg.AppMemoryRequest, "128Mi")),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resourceQty(orDefault(d.cfg.AppCPULimit, "2")),
								corev1.ResourceMemory: resourceQty(orDefault(d.cfg.AppMemoryLimit, "2Gi")),
							},
						},
						SecurityContext: &corev1.SecurityContext{
							// An uploaded app is untrusted code. Letting it
							// escalate privilege inside its own container would
							// make the container boundary meaningless.
							AllowPrivilegeEscalation: ptr(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						ReadinessProbe: &corev1.Probe{
							// A TCP probe rather than HTTP: AppLab does not know
							// what path an arbitrary app serves, and a GET to /
							// would fail for an app whose root is not 200.
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(app.Port)},
							},
							InitialDelaySeconds: 3,
							PeriodSeconds:       10,
							TimeoutSeconds:      3,
							FailureThreshold:    3,
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(app.Port)},
							},
							// Slow to start and slow to give up: a liveness probe
							// that fires during a normal startup would restart a
							// healthy app forever.
							InitialDelaySeconds: 20,
							PeriodSeconds:       20,
							TimeoutSeconds:      5,
							FailureThreshold:    3,
						},
					}},
					ImagePullSecrets: pullSecrets(d.cfg.ImagePullSecret),
					RestartPolicy:    corev1.RestartPolicyAlways,
				},
			},
		},
	}

	return d.upsertDeployment(ctx, namespace, name, desired)
}

// upsertDeployment creates the Deployment or updates the mutable parts of an
// existing one.
//
// The update is a read-modify-write rather than a blind replace, because the
// selector and the resource version are immutable: sending a fresh object
// wholesale is rejected, and replacing the spec wholesale would discard fields
// the cluster set.
func (d *Deployer) upsertDeployment(ctx context.Context, namespace, name string, desired *appsv1.Deployment) error {
	existing, err := d.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := d.client.AppsV1().Deployments(namespace).Create(ctx, desired, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create deployment %s: %w", name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read deployment %s: %w", name, err)
	}

	// Only the fields AppLab owns are copied across. A blind assignment would
	// also carry over a resource version and a selector, both of which the API
	// server rejects on update.
	existing.Spec.Replicas = desired.Spec.Replicas
	existing.Spec.Template = desired.Spec.Template
	existing.Spec.Strategy = desired.Spec.Strategy
	existing.Labels = merge(existing.Labels, desired.Labels)
	existing.Annotations = merge(existing.Annotations, desired.Annotations)

	if _, err := d.client.AppsV1().Deployments(namespace).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update deployment %s: %w", name, err)
	}
	return nil
}

func (d *Deployer) applyService(ctx context.Context, app *model.App) error {
	namespace := app.Namespace
	name := ObjectName(app.ID)

	// ClusterIP, not NodePort or LoadBalancer: the Ingress is how an app is
	// reached, and exposing every app directly would make the ingress
	// configuration meaningless.
	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    appLabels(app),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selectorLabels(app),
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       80,
				TargetPort: intstr.FromInt32(app.Port),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}

	existing, err := d.client.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := d.client.CoreV1().Services(namespace).Create(ctx, desired, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create service %s: %w", name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read service %s: %w", name, err)
	}

	// A Service's cluster IP is immutable and only the API server knows it, so
	// the ports and selector are updated on the existing object rather than the
	// whole spec being replaced.
	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.Ports = desired.Spec.Ports
	existing.Labels = merge(existing.Labels, desired.Labels)

	if _, err := d.client.CoreV1().Services(namespace).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update service %s: %w", name, err)
	}
	return nil
}

// expose creates or updates the VirtualService that publishes an app through
// the cluster's Istio gateway.
//
// A VirtualService rather than an Ingress because the routing in front of these
// apps is an Istio gateway: an Ingress would be ignored by it, and the app would
// be deployed, healthy, and unreachable.
//
// The gateway is not created here. It is a shared piece of cluster
// infrastructure — one per cluster or per team, with the certificate and the
// listeners configured on it — so AppLab attaches to it by name. That also means
// TLS is not AppLab's business: the certificate is on the gateway, and pointing
// a VirtualService at an HTTPS listener is all that is needed to be served over
// it.
func (d *Deployer) expose(ctx context.Context, app *model.App, addr model.Address) error {
	namespace := app.Namespace
	name := ObjectName(app.ID)

	vs := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": group + "/" + version,
		"kind":       kind,
		"metadata": map[string]any{
			"name":        name,
			"namespace":   namespace,
			"labels":      toStringMap(appLabels(app)),
			"annotations": toStringMap(d.cfg.Annotations),
		},
		"spec": map[string]any{
			"hosts":    []any{addr.Host},
			"gateways": []any{d.cfg.Gateway},
			"http":     d.httpRoutes(app, addr),
		},
	}}

	return d.upsertVirtualService(ctx, namespace, name, vs)
}

// httpRoutes builds the HTTP routes for an app: one route, unless a shared path
// prefix makes the app a sub-path of a host it does not own.
//
// Order matters here — routes within one VirtualService are evaluated in
// sequence — but the two below cannot overlap, so neither can shadow the other.
func (d *Deployer) httpRoutes(app *model.App, addr model.Address) []any {
	route := map[string]any{
		"destination": map[string]any{
			"host": ObjectName(app.ID),
			"port": map[string]any{"number": int64(80)},
		},
	}

	// No prefix: the app has its own host and answers at its root, which is
	// every path. A route with no match matches everything.
	if addr.Path == "" {
		return []any{map[string]any{"route": []any{route}}}
	}

	prefix := addr.RoutePath()

	// The app's own path, reached through the shared host.
	//
	// The rewrite strips the prefix, so the app sees the paths it would see if
	// it were mounted at the root — which is what lets an unmodified app be
	// served under a prefix at all. Istio replaces the matched prefix with the
	// rewrite value, so "/apps/shop/cart" arrives as "/cart".
	serve := map[string]any{
		"match": []any{map[string]any{"uri": map[string]any{"prefix": prefix}}},
		"rewrite": map[string]any{
			// "/" rather than "" because the rewritten path has to remain an
			// absolute path; an empty uri is not a valid rewrite.
			"uri": "/",
		},
		"headers": map[string]any{
			"request": map[string]any{
				// The convention for telling a sub-path-mounted app where it is
				// mounted. An app that honours it can build correct absolute
				// links and redirects; one that ignores it still works, which is
				// why this is a header rather than a requirement.
				"set": map[string]any{"X-Forwarded-Prefix": addr.Path},
			},
		},
		"route": []any{route},
	}

	// Redirect the path without its trailing slash to the one with it, rather
	// than serving it directly.
	//
	// Serving it directly would be the friendly choice and the wrong one: an
	// app's relative links resolve against the request path, so at
	// "/apps/shop" a link to "cart" would resolve to "/apps/cart" — the
	// neighbouring app — while at "/apps/shop/" it resolves correctly. One
	// redirect on the bare path removes a whole class of cross-app confusion
	// that would otherwise surface as an app mysteriously showing someone
	// else's page.
	redirect := map[string]any{
		"match": []any{map[string]any{"uri": map[string]any{"exact": addr.Path}}},
		"redirect": map[string]any{
			"uri":          prefix,
			"redirectCode": int64(301),
		},
	}

	return []any{redirect, serve}
}

func (d *Deployer) upsertVirtualService(ctx context.Context, namespace, name string, desired *unstructured.Unstructured) error {
	existing, err := d.dynamic.Resource(virtualServiceGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := d.dynamic.Resource(virtualServiceGVR).Namespace(namespace).Create(ctx, desired, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create virtualservice %s: %w", name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read virtualservice %s: %w", name, err)
	}

	// Only the fields AppLab owns are replaced, rather than the whole object.
	// Istio's control plane writes status and defaults into the same resource, so
	// a wholesale update would fight it.
	existing.SetLabels(merge(existing.GetLabels(), desired.GetLabels()))

	// Annotations are replaced rather than merged, so one AppLab stopped setting
	// actually disappears. The VirtualService is AppLab's own object — nothing
	// else writes to it — so there is no one else's annotation to preserve.
	existing.SetAnnotations(desired.GetAnnotations())

	// The spec is owned entirely by AppLab, so it is replaced.
	spec, found, err := unstructured.NestedMap(desired.Object, "spec")
	if err != nil || !found {
		return fmt.Errorf("virtualservice %s has no spec to apply", name)
	}
	if err := unstructured.SetNestedMap(existing.Object, spec, "spec"); err != nil {
		return fmt.Errorf("set spec on virtualservice %s: %w", name, err)
	}

	if _, err := d.dynamic.Resource(virtualServiceGVR).Namespace(namespace).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update virtualservice %s: %w", name, err)
	}
	return nil
}

// toStringMap converts a labels map for use in an unstructured object.
//
// The dynamic client round-trips through JSON, so values have to be the shapes
// encoding/json produces; map[string]string happens to work but makes the
// intended object harder to read beside a spec that is all map[string]any.
func toStringMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Status describes what the cluster currently shows for an app.
type Status struct {
	// DesiredReplicas and ReadyReplicas are the Deployment's counts.
	DesiredReplicas int32
	ReadyReplicas   int32

	// Available is whether every desired replica is ready.
	Available bool

	// Message explains a rollout that is not progressing.
	Message string

	// CurrentImage is the image the Deployment currently specifies, which is how
	// a caller can tell whether a deploy actually changed anything.
	CurrentImage string

	// Found is false when the app has no Deployment at all, which means it has
	// never been deployed.
	Found bool
}

// Status reads an app's live state.
func (d *Deployer) Status(ctx context.Context, app *model.App) (Status, error) {
	deployment, err := d.client.AppsV1().Deployments(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Status{Found: false}, nil
	}
	if err != nil {
		return Status{}, fmt.Errorf("read deployment for app %s: %w", app.ID, err)
	}

	status := Status{
		Found:           true,
		DesiredReplicas: derefInt32(deployment.Spec.Replicas),
		ReadyReplicas:   deployment.Status.ReadyReplicas,
	}
	if len(deployment.Spec.Template.Spec.Containers) > 0 {
		status.CurrentImage = deployment.Spec.Template.Spec.Containers[0].Image
	}

	// A Deployment is available when the cluster says so; the counts are
	// reported alongside for a caller that wants to show progress, but the
	// condition is the verdict.
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentAvailable && condition.Status == corev1.ConditionTrue {
			status.Available = true
		}
		// A rollout that cannot progress carries the reason, which is the single
		// most useful thing to surface when an app will not come up.
		if condition.Type == appsv1.DeploymentProgressing && condition.Status == corev1.ConditionFalse {
			status.Message = condition.Reason + ": " + condition.Message
		}
		if condition.Type == appsv1.DeploymentReplicaFailure && condition.Status == corev1.ConditionTrue {
			status.Message = condition.Reason + ": " + condition.Message
		}
	}

	return status, nil
}

// Remove deletes an app's Deployment, Service and VirtualService.
//
// It is used when an app is stopped without being deleted: the source and its
// history are kept, so starting again is a deploy rather than a re-upload.
func (d *Deployer) Remove(ctx context.Context, app *model.App) error {
	namespace := app.Namespace
	name := ObjectName(app.ID)
	policy := metav1.DeletePropagationForeground

	if err := d.client.AppsV1().Deployments(namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete deployment for app %s: %w", app.ID, err)
	}
	if err := d.client.CoreV1().Services(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete service for app %s: %w", app.ID, err)
	}
	// A missing VirtualService is normal when the deployment has no base domain,
	// so a not-found is not a failure. A nil dynamic client means this Deployer
	// was built without Istio support, in which case there is none to delete.
	if d.dynamic != nil {
		if err := d.dynamic.Resource(virtualServiceGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete virtualservice for app %s: %w", app.ID, err)
		}
	}
	return nil
}

// Restart triggers a rollout without changing the image, by stamping the pod
// template so the Deployment sees a change.
func (d *Deployer) Restart(ctx context.Context, app *model.App) error {
	deployment, err := d.client.AppsV1().Deployments(app.Namespace).Get(ctx, ObjectName(app.ID), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("app %q has not been deployed yet", app.ID)
		}
		return fmt.Errorf("read deployment for app %s: %w", app.ID, err)
	}

	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = map[string]string{}
	}
	deployment.Spec.Template.Annotations["applab.io/restarted-at"] = time.Now().UTC().Format(time.RFC3339)

	if _, err := d.client.AppsV1().Deployments(app.Namespace).Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("restart app %s: %w", app.ID, err)
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

// appEnv is the container's environment: the port AppLab derives, then the
// app's own variables.
//
// The order is fixed and the names are sorted, because the value of this
// function ends up verbatim in the pod template. Go randomises map iteration,
// so building the list straight from app.Env would produce a different pod
// template on every deploy of an unchanged app — and since a changed template is
// what triggers a rollout, every deploy would roll the app for no reason. Sorting
// makes the result a function of the configuration alone.
// appEnv builds the container's environment: the port, the app's plain
// configuration, and its secrets.
//
// The two configuration maps are merged rather than kept apart, because by the
// time they reach a container they are the same thing. A name in both is refused
// by the API, so the order here does not decide anything; the sort is what makes
// the result stable, which matters because the pod template is compared to
// decide whether a rollout is needed.
func appEnv(app *model.App, secrets map[string]string) []corev1.EnvVar {
	// The port the app should bind. An app that honours this needs no
	// configuration of its own; one that does not is unaffected.
	env := []corev1.EnvVar{{Name: model.PortEnv, Value: fmt.Sprintf("%d", app.Port)}}

	merged := make(map[string]string, len(app.Env)+len(secrets))
	for name, value := range app.Env {
		// PORT is refused at the API, but an app record could predate that check
		// or have been edited directly. Skipping it here as well means the pod
		// spec can never carry the duplicate declaration at all.
		if name == model.PortEnv {
			continue
		}
		merged[name] = value
	}
	for name, value := range secrets {
		if name == model.PortEnv {
			continue
		}
		merged[name] = value
	}

	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		env = append(env, corev1.EnvVar{Name: name, Value: merged[name]})
	}
	return env
}

// configHash fingerprints an app's whole configuration.
//
// Now that both halves are written into the pod template, a change to either
// already makes the template differ and the Deployment roll. The hash is kept
// because it names *what* changed: a template diff for a configuration change is
// otherwise a wall of lines that look alike, and an operator looking at a
// rollout wants to know whether it was the image or the configuration.
//
// It covers both halves, and the values rather than the names — a hash of the
// names alone would report "unchanged" for a rotated password, which is the one
// case where saying so is actively misleading.
//
// Names and values are both fed in, separated so that no pair of configurations
// can collide by concatenation. The result is only ever compared, never printed
// as anything but a digest.
func configHash(env, secrets map[string]string) string {
	h := sha256.New()
	writeSorted := func(kind string, values map[string]string) {
		names := make([]string, 0, len(values))
		for name := range values {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(h, "%s\x00%s\x00%s\x00", kind, name, values[name])
		}
	}
	writeSorted("env", env)
	writeSorted("secret", secrets)

	// Truncated to 16 hex characters. The annotation is a change detector rather
	// than a security boundary, and a short one stays readable in `kubectl
	// describe`.
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func pullSecrets(name string) []corev1.LocalObjectReference {
	if name == "" {
		return nil
	}
	return []corev1.LocalObjectReference{{Name: name}}
}

// merge overlays desired onto existing, leaving keys only existing has.
func merge(existing, desired map[string]string) map[string]string {
	out := make(map[string]string, len(existing)+len(desired))
	for k, v := range existing {
		out[k] = v
	}
	for k, v := range desired {
		out[k] = v
	}
	return out
}

// resourceQty parses a Kubernetes quantity, panicking on a malformed one.
//
// The values are AppLab's own constants, so a bad one is a programming error
// rather than a caller's mistake, and threading an error no caller could act on
// through the resource builders would only add noise.
func resourceQty(s string) resource.Quantity {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		panic(fmt.Sprintf("invalid resource quantity %q: %v", s, err))
	}
	return q
}

func ptr[T any](v T) *T { return &v }

// orDefault returns v, or def when v is empty.
func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func derefInt32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

func shortSHA(sha string) string {
	if sha == "" {
		return "none"
	}
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
