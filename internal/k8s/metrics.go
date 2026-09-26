package k8s

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// podMetricsGVR names the resource metrics API for the dynamic client.
//
// The same reasoning as virtualServiceGVR: `k8s.io/metrics` is a module of its
// own, and AppLab reads two numbers out of two fields of one resource. Taking the
// dependency on would be a second Kubernetes API surface to keep in step with the
// client-go version, in order to avoid reading a map.
//
// The group is metrics.k8s.io, served by metrics-server rather than by the API
// server itself. A cluster without it answers 404 for this resource and nothing
// else breaks — see PodUsage.
var podMetricsGVR = schema.GroupVersionResource{
	Group:    "metrics.k8s.io",
	Version:  "v1",
	Resource: "pods",
}

// Usage is how much CPU and memory one pod is using right now.
//
// The quantities are the strings the metrics API reports — "12m", "48Mi" — rather
// than parsed numbers, for the same reason an app's own bounds are: CPU is a
// ratio and memory is bytes, and one field holding either would have to say
// which. A caller that wants to compare them parses them; the API passes them on.
type Usage struct {
	PodName   string
	CPU       string
	Memory    string
	Timestamp string
}

// UsageReader reads what the running pods are using.
//
// An interface rather than the concrete Client so internal/observe can hold one
// without depending on the rest of this package, and so a deployment that cannot
// read metrics holds a nil rather than a stub that answers zero.
type UsageReader interface {
	PodUsage(ctx context.Context, namespace, selector string) (map[string]Usage, bool, error)
}

// PodUsage returns each pod's current usage, keyed by pod name, and whether the
// cluster answered with metrics at all.
//
// The second return is what keeps "no pods are running" apart from "this cluster
// cannot report usage", which are an identical empty map otherwise. A deployment
// without metrics-server is a legitimate way to run a cluster, so a caller needs
// to be able to say "not available here" rather than "0%" — showing a busy app as
// idle is a worse failure than showing nothing.
//
// A cluster without the metrics API is not an error: it answers 404 for the whole
// resource, which is a missing optional component rather than a fault. Anything
// else — a timeout, a refused connection — is returned, because that is a real
// failure the caller may want to report.
func (c *Client) PodUsage(ctx context.Context, namespace, selector string) (map[string]Usage, bool, error) {
	if c.dynamic == nil {
		return nil, false, nil
	}

	list, err := c.dynamic.Resource(podMetricsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// No metrics API in this cluster. Not a failure; see the doc comment.
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read pod usage in %s: %w", namespace, err)
	}

	out := make(map[string]Usage, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]

		// Read as strings rather than through a numeric accessor: the API reports
		// quantities with suffixes ("12m", "48Mi"), and asking the unstructured
		// layer for a number would fail on every one of them. A field that is
		// absent is left empty and reported as such rather than as zero.
		cpu, _, _ := unstructured.NestedString(item.Object, "usage", "cpu")
		memory, _, _ := unstructured.NestedString(item.Object, "usage", "memory")
		stamp, _, _ := unstructured.NestedString(item.Object, "timestamp")

		out[item.GetName()] = Usage{
			PodName:   item.GetName(),
			CPU:       cpu,
			Memory:    memory,
			Timestamp: stamp,
		}
	}
	return out, true, nil
}
