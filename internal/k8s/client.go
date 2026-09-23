// Package k8s builds the Kubernetes clients applab uses and owns the naming
// rules for the objects it creates.
//
// One rule governs everything here: applab only ever touches its own namespace.
// Every app it deploys lives there too, rather than in a namespace of its own,
// which is what lets applab hold a namespaced Role instead of a ClusterRole — it
// has no permission anywhere else in the cluster.
//
// The consequence is that a namespace no longer separates one app's objects from
// another's. The `applab.io/app` label does, so anything that lists or deletes
// objects must filter by it, and nothing here may assume a namespace contains
// only the app it was asked about.
package k8s

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// LabelApp identifies the app an object belongs to.
//
// It is the only thing distinguishing one app's objects from another's in the
// shared namespace, so every object applab creates carries it and every query
// that could return another app's object filters by it.
const LabelApp = "applab.io/app"

// Client wraps the Kubernetes clientset with the few conveniences applab needs.
type Client struct {
	clientset kubernetes.Interface

	// dynamic reads and writes resources that are not in client-go, which is
	// how Istio's VirtualService is handled without depending on istio.io/api.
	dynamic dynamic.Interface

	// namespace is the one namespace applab uses for everything.
	namespace string
}

// Options configure the client.
type Options struct {
	// Kubeconfig is an explicit kubeconfig path. Empty means in-cluster config
	// first, falling back to the ambient kubeconfig.
	Kubeconfig string

	// Namespace is where applab runs and where it deploys every app.
	Namespace string
}

// New builds a client.
//
// Resolution order is in-cluster, then the explicit path, then the ambient
// kubeconfig. In-cluster first because that is how applab actually runs; the
// fallbacks exist so it can be developed and tested outside a cluster, which is
// where most of its behaviour is checked.
func New(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.Namespace) == "" {
		// Not merely a convenience check: Kubernetes reads "" as the default
		// namespace, so an unset value would quietly deploy every app into
		// `default` instead of failing.
		return nil, fmt.Errorf("namespace must not be empty: an empty namespace is read as \"default\" by the API server, so objects would be created somewhere other than intended")
	}

	cfg, err := restConfig(opts.Kubeconfig)
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}

	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}

	return &Client{
		clientset: clientset,
		dynamic:   dyn,
		namespace: opts.Namespace,
	}, nil
}

// NewWithClientset builds a client over a provided clientset, for tests.
//
// It exists so the handlers can be exercised against a fake clientset with no
// cluster and no kubeconfig, which is what makes most of this package's
// behaviour testable on a laptop.
func NewWithClientset(clientset kubernetes.Interface, namespace string) *Client {
	return &Client{clientset: clientset, namespace: namespace}
}

// NewWithDynamic attaches a dynamic client, for tests that exercise the Istio
// resources as well as the built-in ones.
func (c *Client) NewWithDynamic(dyn dynamic.Interface) *Client {
	c.dynamic = dyn
	return c
}

// restConfig resolves a Kubernetes REST config.
func restConfig(kubeconfig string) (*rest.Config, error) {
	// In-cluster first: applab's normal habitat is a pod, and the ambient
	// kubeconfig on a developer's machine is often stale or points elsewhere.
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}

	path := kubeconfig
	if path == "" {
		// The standard locations, in the order kubectl itself looks.
		if env := os.Getenv("KUBECONFIG"); env != "" {
			path = strings.Split(env, string(os.PathListSeparator))[0]
		} else if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, ".kube", "config")
		}
	}

	if path == "" {
		return nil, fmt.Errorf("no Kubernetes configuration found: not running in a cluster and no kubeconfig could be located")
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig %s: %w", path, err)
	}
	return cfg, nil
}

// Clientset exposes the underlying clientset.
func (c *Client) Clientset() kubernetes.Interface { return c.clientset }

// Dynamic exposes the client for resources outside client-go, such as Istio's.
func (c *Client) Dynamic() dynamic.Interface { return c.dynamic }

// Namespace returns the namespace an app's resources live in — the one applab
// itself runs in, since every app shares it.
//
// The app id does not affect the result. It is a parameter because the callers
// that have an app at hand read better for passing it, and because the app is
// what all of them are actually talking about.
func (c *Client) Namespace(appID string) string {
	return c.namespace
}

// OwnsNamespace reports whether a namespace is the one applab uses.
//
// It is the check that keeps applab from touching anything it did not create,
// and it is deliberately strict: any other namespace is not applab's, whatever
// it contains.
//
// An empty namespace is rejected explicitly. Kubernetes reads "" as the default
// namespace, so treating it as a match would point every operation at
// `default` — the opposite of confining applab to its own space.
func (c *Client) OwnsNamespace(namespace string) bool {
	return namespace != "" && namespace == c.namespace
}

// DeleteAppObjects removes everything applab created for one app.
//
// This is what deleting an app means now that apps share a namespace: there is
// no namespace to drop, so the objects are found by label and removed
// individually.
//
// Every list is checked before anything is deleted. Deletion is irreversible and
// the selector is the only thing standing between one app's objects and
// another's — or between an app's objects and applab's own Deployment, which
// carries no app label but does live in this namespace. A mislabeled or
// over-broad selector deletes things nobody asked to delete, and a second read
// is a cheap price for noticing that first.
func (c *Client) DeleteAppObjects(ctx context.Context, appID string) error {
	namespace := c.Namespace(appID)
	if !c.OwnsNamespace(namespace) {
		return fmt.Errorf("refusing to delete app %q's objects in namespace %q: it is not this installation's namespace %q",
			appID, namespace, c.namespace)
	}

	selector := LabelApp + "=" + appID

	// Everything the app owns is labeled, so a single selector identifies all of
	// it. Listed per kind rather than deleted blind so the assertion below can
	// see what it is about to remove.
	deployments, err := c.clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list deployments for app %s: %w", appID, err)
	}
	services, err := c.clientset.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list services for app %s: %w", appID, err)
	}
	ingresses, err := c.clientset.NetworkingV1().Ingresses(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list ingresses for app %s: %w", appID, err)
	}
	jobs, err := c.clientset.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list jobs for app %s: %w", appID, err)
	}
	secrets, err := c.clientset.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list secrets for app %s: %w", appID, err)
	}

	// The label selector is the safety property, so it is verified rather than
	// assumed: a client that ignored the selector, or an object labeled by
	// something else, would otherwise be deleted without a word.
	for _, group := range []struct {
		kind    string
		objects []metav1.Object
	}{
		{"deployment", deploymentObjects(deployments.Items)},
		{"service", serviceObjects(services.Items)},
		{"ingress", ingressObjects(ingresses.Items)},
		{"job", jobObjects(jobs.Items)},
		{"secret", secretObjects(secrets.Items)},
	} {
		if err := checkAppLabels(group.kind, appID, namespace, group.objects); err != nil {
			return err
		}
	}

	foreground := metav1.DeletePropagationForeground

	// Jobs first, so their pods stop before the app's Deployment is torn down
	// and a build cannot be left running against a namespace entry that no
	// longer describes it.
	for i := range jobs.Items {
		name := jobs.Items[i].Name
		if err := c.clientset.BatchV1().Jobs(namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &foreground}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete job %s for app %s: %w", name, appID, err)
		}
	}
	for i := range deployments.Items {
		name := deployments.Items[i].Name
		if err := c.clientset.AppsV1().Deployments(namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &foreground}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete deployment %s for app %s: %w", name, appID, err)
		}
	}
	for i := range services.Items {
		name := services.Items[i].Name
		if err := c.clientset.CoreV1().Services(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete service %s for app %s: %w", name, appID, err)
		}
	}
	for i := range ingresses.Items {
		name := ingresses.Items[i].Name
		if err := c.clientset.NetworkingV1().Ingresses(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete ingress %s for app %s: %w", name, appID, err)
		}
	}
	// Secrets last. A build token secret is mounted by a Job, so removing it
	// before the Job is gone would leave a pod referencing a secret that no
	// longer exists.
	for i := range secrets.Items {
		name := secrets.Items[i].Name
		if err := c.clientset.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete secret %s for app %s: %w", name, appID, err)
		}
	}

	return nil
}

// Ready reports whether the cluster is reachable and this namespace is usable.
//
// It reads a namespaced resource rather than asking for the version, so it
// exercises the same authorization path applab's real operations use. A
// permission problem — a Role missing a rule, a namespace that does not exist —
// surfaces here rather than at the first deploy.
func (c *Client) Ready(ctx context.Context) bool {
	_, err := c.clientset.AppsV1().Deployments(c.namespace).List(ctx, metav1.ListOptions{Limit: 1})
	return err == nil
}

// checkAppLabels refuses if any object does not carry appID's label.
//
// It takes the objects the API server returned for the app's selector, so it
// should never fire. It exists because the alternative to finding out here is
// finding out from a deleted object that belonged to someone else.
func checkAppLabels(kind, appID, namespace string, objects []metav1.Object) error {
	for _, obj := range objects {
		if got := obj.GetLabels()[LabelApp]; got != appID {
			return fmt.Errorf("refusing to delete app %q: %s %s in namespace %s carries label %s=%q, which belongs to another app",
				appID, kind, obj.GetName(), namespace, LabelApp, got)
		}
	}
	return nil
}

// The list types produced by a typed client do not satisfy a common interface,
// so each is widened to metav1.Object here rather than at the call site.

func deploymentObjects(items []appsv1.Deployment) []metav1.Object {
	out := make([]metav1.Object, 0, len(items))
	for i := range items {
		out = append(out, &items[i])
	}
	return out
}

func serviceObjects(items []corev1.Service) []metav1.Object {
	out := make([]metav1.Object, 0, len(items))
	for i := range items {
		out = append(out, &items[i])
	}
	return out
}

func ingressObjects(items []networkingv1.Ingress) []metav1.Object {
	out := make([]metav1.Object, 0, len(items))
	for i := range items {
		out = append(out, &items[i])
	}
	return out
}

func jobObjects(items []batchv1.Job) []metav1.Object {
	out := make([]metav1.Object, 0, len(items))
	for i := range items {
		out = append(out, &items[i])
	}
	return out
}

func secretObjects(items []corev1.Secret) []metav1.Object {
	out := make([]metav1.Object, 0, len(items))
	for i := range items {
		out = append(out, &items[i])
	}
	return out
}
