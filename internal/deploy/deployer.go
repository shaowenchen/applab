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
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/shaowenchen/applab/internal/model"
)

// Config describes one deployment's ingress environment.
type Config struct {
	// IngressClass is the IngressClass apps are served through. Empty means the
	// cluster's default.
	IngressClass string

	// BaseDomain is the domain apps are exposed under. Empty means no Ingress is
	// created and an app is reachable only inside the cluster.
	BaseDomain string

	// TLSSecret is a wildcard certificate Secret for the base domain. Empty
	// means no TLS is configured, which is the honest default — a redirect to
	// https with no certificate is worse than no TLS at all.
	TLSSecret string

	// ClusterIssuer names a cert-manager ClusterIssuer. When set, an annotation
	// asks cert-manager for a per-app certificate, which is the right choice
	// when no wildcard certificate exists.
	ClusterIssuer string

	// ImagePullSecret names a Secret in each app namespace holding registry
	// credentials for pulling the built image.
	ImagePullSecret string

	// Annotations are added to every Ingress, for ingress-controller specifics
	// that vary by cluster (proxy body size, timeouts, SSL redirect).
	Annotations map[string]string
}

// Deployer creates and updates an app's Kubernetes resources.
type Deployer struct {
	client kubernetes.Interface
	cfg    Config
}

// New creates a Deployer.
func New(client kubernetes.Interface, cfg Config) *Deployer {
	return &Deployer{client: client, cfg: cfg}
}

// Ready reports whether the deployer can work.
func (d *Deployer) Ready() bool { return d.client != nil }

// Apply creates or updates every resource an app needs, and returns the
// hostname it is exposed at (empty when no Ingress was created).
//
// The three objects are applied in dependency order and each is
// create-or-update: an app is redeployed whenever anything about it changes, and
// the operation has to be safe to repeat. A failure part-way leaves earlier
// objects in place, which is the correct outcome — they are consistent with each
// other, and the next attempt continues from there.
func (d *Deployer) Apply(ctx context.Context, app *model.App, image string) (string, error) {
	if err := d.applyDeployment(ctx, app, image); err != nil {
		return "", err
	}
	if err := d.applyService(ctx, app); err != nil {
		return "", err
	}

	host := app.Hostname(d.cfg.BaseDomain)
	if host == "" {
		return "", nil
	}
	if err := d.applyIngress(ctx, app, host); err != nil {
		return "", err
	}
	return host, nil
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
					Containers: []corev1.Container{{
						Name:  "app",
						Image: image,
						Ports: []corev1.ContainerPort{{
							Name:          "http",
							ContainerPort: app.Port,
							Protocol:      corev1.ProtocolTCP,
						}},
						Env: []corev1.EnvVar{
							// The port the app should bind. An app that honours
							// this needs no configuration of its own; one that
							// does not is unaffected.
							{Name: "PORT", Value: fmt.Sprintf("%d", app.Port)},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resourceQty("100m"),
								corev1.ResourceMemory: resourceQty("128Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resourceQty("2"),
								corev1.ResourceMemory: resourceQty("2Gi"),
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
							// A TCP probe rather than HTTP: applab does not know
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

	// Only the fields applab owns are copied across. A blind assignment would
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

func (d *Deployer) applyIngress(ctx context.Context, app *model.App, host string) error {
	namespace := app.Namespace
	name := ObjectName(app.ID)

	pathType := networkingv1.PathTypePrefix

	annotations := map[string]string{}
	for k, v := range d.cfg.Annotations {
		annotations[k] = v
	}
	if d.cfg.ClusterIssuer != "" {
		annotations["cert-manager.io/cluster-issuer"] = d.cfg.ClusterIssuer
	}

	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      appLabels(app),
			Annotations: annotations,
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: name,
									Port: networkingv1.ServiceBackendPort{Number: 80},
								},
							},
						}},
					},
				},
			}},
		},
	}

	if d.cfg.IngressClass != "" {
		ingress.Spec.IngressClassName = ptr(d.cfg.IngressClass)
	}

	// TLS only when there is something to serve it with. An Ingress with a
	// tls block naming a Secret that does not exist makes the ingress controller
	// serve its own self-signed certificate, which trains a user to click through
	// a browser warning — worse than plain HTTP, which is at least honest.
	if secretName := d.tlsSecretName(app); secretName != "" {
		ingress.Spec.TLS = []networkingv1.IngressTLS{{
			Hosts:      []string{host},
			SecretName: secretName,
		}}
	}

	return d.upsertIngress(ctx, namespace, name, ingress)
}

// tlsSecretName picks the Secret an app's certificate lives in, or "" for no TLS.
//
// Two arrangements are supported and the difference matters:
//
//   - A configured TLSSecret is a certificate that already exists and covers
//     every app under the base domain — in practice a wildcard. Every app
//     references the same Secret, because that is what a wildcard is for.
//   - With no such Secret but a cert-manager ClusterIssuer configured, each app
//     gets its own certificate, so the Secret is named per app. Using one shared
//     name here would have every app overwrite the same Secret and cert-manager
//     thrash, issuing a certificate for whichever host it saw last.
func (d *Deployer) tlsSecretName(app *model.App) string {
	if d.cfg.TLSSecret != "" {
		return d.cfg.TLSSecret
	}
	if d.cfg.ClusterIssuer != "" {
		return ObjectName(app.ID) + "-tls"
	}
	// No certificate and no issuer: plain HTTP, which is at least honest about
	// what it is.
	return ""
}

func (d *Deployer) upsertIngress(ctx context.Context, namespace, name string, desired *networkingv1.Ingress) error {
	existing, err := d.client.NetworkingV1().Ingresses(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := d.client.NetworkingV1().Ingresses(namespace).Create(ctx, desired, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create ingress %s: %w", name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read ingress %s: %w", name, err)
	}

	existing.Spec = desired.Spec
	existing.Labels = merge(existing.Labels, desired.Labels)
	// Annotations are replaced rather than merged: a removed annotation has to
	// actually disappear, or a setting applab stopped applying would linger
	// forever.
	existing.Annotations = annotationsFor(desired)

	if _, err := d.client.NetworkingV1().Ingresses(namespace).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update ingress %s: %w", name, err)
	}
	return nil
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

// Remove deletes an app's Deployment, Service and Ingress.
//
// It is used when an app is stopped without being deleted. Deleting the
// namespace would be simpler, but it would also take the app's source and
// history with it.
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
	if err := d.client.NetworkingV1().Ingresses(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		// A missing ingress is normal when the deployment has no base domain.
		return fmt.Errorf("delete ingress for app %s: %w", app.ID, err)
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

func pullSecrets(name string) []corev1.LocalObjectReference {
	if name == "" {
		return nil
	}
	return []corev1.LocalObjectReference{{Name: name}}
}

func annotationsFor(ingress *networkingv1.Ingress) map[string]string {
	out := make(map[string]string, len(ingress.Annotations))
	for k, v := range ingress.Annotations {
		out[k] = v
	}
	return out
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
// The values are applab's own constants, so a bad one is a programming error
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
