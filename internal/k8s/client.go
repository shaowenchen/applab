// Package k8s builds the Kubernetes clients applab uses and owns the naming
// rules for the objects it creates.
//
// One rule governs everything here: applab only ever touches namespaces carrying
// its configured prefix. That is what lets several applab installations share a
// cluster without one deleting the other's applications, and it is enforced at
// the point a namespace name is computed rather than trusted at each call site.
package k8s

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Client wraps the Kubernetes clientset with the few conveniences applab needs.
type Client struct {
	clientset kubernetes.Interface

	// namespacePrefix is prepended to an app id to name that app's namespace.
	namespacePrefix string

	// ownNamespace is where applab itself runs.
	ownNamespace string
}

// Options configure the client.
type Options struct {
	// Kubeconfig is an explicit kubeconfig path. Empty means in-cluster config
	// first, falling back to the ambient kubeconfig.
	Kubeconfig string

	NamespacePrefix string
	OwnNamespace    string
}

// New builds a client.
//
// Resolution order is in-cluster, then the explicit path, then the ambient
// kubeconfig. In-cluster first because that is how applab actually runs; the
// fallbacks exist so it can be developed and tested outside a cluster, which is
// where most of its behaviour is checked.
func New(opts Options) (*Client, error) {
	cfg, err := restConfig(opts.Kubeconfig)
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}

	if opts.NamespacePrefix == "" {
		return nil, fmt.Errorf("namespace prefix must not be empty: applab would enumerate every namespace in the cluster")
	}

	return &Client{
		clientset:       clientset,
		namespacePrefix: opts.NamespacePrefix,
		ownNamespace:    opts.OwnNamespace,
	}, nil
}

// NewWithClientset builds a client over a provided clientset, for tests.
//
// It exists so the handlers can be exercised against a fake clientset with no
// cluster and no kubeconfig, which is what makes most of this package's
// behaviour testable on a laptop.
func NewWithClientset(clientset kubernetes.Interface, namespacePrefix, ownNamespace string) *Client {
	return &Client{
		clientset:       clientset,
		namespacePrefix: namespacePrefix,
		ownNamespace:    ownNamespace,
	}
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

// NamespaceFor returns the namespace an app is deployed into.
//
// Every object applab creates is named through this, so the prefix rule holds
// everywhere by construction.
func (c *Client) NamespaceFor(appID string) string {
	return c.namespacePrefix + appID
}

// OwnsNamespace reports whether a namespace belongs to this installation.
//
// It is the check that keeps applab from touching anything it did not create,
// and it is deliberately strict: a namespace name without the prefix is not
// applab's, whatever it contains.
func (c *Client) OwnsNamespace(namespace string) bool {
	return strings.HasPrefix(namespace, c.namespacePrefix) && len(namespace) > len(c.namespacePrefix)
}

// EnsureNamespace creates an app's namespace if it is missing.
//
// Labels record which app and which installation it belongs to, so an operator
// looking at a namespace in the cluster can tell where it came from without
// consulting applab.
func (c *Client) EnsureNamespace(ctx context.Context, appID string) error {
	namespace := c.NamespaceFor(appID)

	_, err := c.clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "applab",
				"applab.io/app":                appID,
			},
		},
	}, metav1.CreateOptions{})

	if err == nil || apierrors.IsAlreadyExists(err) {
		return nil
	}
	return fmt.Errorf("create namespace %s: %w", namespace, err)
}

// DeleteNamespace removes an app's namespace and everything in it.
//
// It refuses a namespace this installation does not own, so a caller that got an
// app id wrong cannot delete someone else's workloads. That refusal is the whole
// reason this method exists rather than callers deleting namespaces directly.
func (c *Client) DeleteNamespace(ctx context.Context, appID string) error {
	namespace := c.NamespaceFor(appID)

	if !c.OwnsNamespace(namespace) {
		return fmt.Errorf("refusing to delete namespace %q: it does not carry this installation's prefix %q", namespace, c.namespacePrefix)
	}

	err := c.clientset.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{})
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	return fmt.Errorf("delete namespace %s: %w", namespace, err)
}

// NamespaceExists reports whether an app's namespace is present.
func (c *Client) NamespaceExists(ctx context.Context, appID string) (bool, error) {
	_, err := c.clientset.CoreV1().Namespaces().Get(ctx, c.NamespaceFor(appID), metav1.GetOptions{})
	if err == nil {
		return true, nil
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("get namespace %s: %w", c.NamespaceFor(appID), err)
}

// Ready reports whether the cluster is reachable.
//
// A lightweight read rather than a version call wherever possible: it exercises
// the same authorization path applab's real operations use, so a permission
// problem surfaces here rather than at the first deploy.
func (c *Client) Ready(ctx context.Context) bool {
	_, err := c.clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 1})
	return err == nil
}
