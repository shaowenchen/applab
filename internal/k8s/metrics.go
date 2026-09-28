package k8s

import (
	"context"
	"fmt"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// podMetricsGVR names the resource metrics API for the dynamic client.
//
// v1beta1 and not v1, which is the version this used to ask for and the reason
// the monitoring panels were empty on every real cluster.
//
// metrics-server registers exactly one version in its storage map, and in every
// released version — v0.6 through v0.8, including the v0.7.2 this repository's
// debugger environment installs — that version is v1beta1:
//
//	apiGroupInfo.VersionedResourcesStorageMap[v1beta1.SchemeGroupVersion.Version] = metricsServerResources
//
// Only master adds a v1 line. A request for a version a group does not serve is
// answered 404, and PodUsage treats NotFound as "this cluster has no metrics
// API" — an optional component that is absent rather than a fault — so the
// whole thing failed silently: no readings, no error, and a console that could
// not tell that apart from an idle deployment. `kubectl top` reads v1beta1 for
// the same reason.
//
// The same reasoning as virtualServiceGVR applies to the dynamic client:
// `k8s.io/metrics` is a module of its own, and AppLab reads two numbers out of
// one resource. Taking the dependency would be a second Kubernetes API surface
// to keep in step with client-go in order to avoid reading a map.
//
// The group is served by metrics-server rather than by the API server itself. A
// cluster without it answers 404 for the whole group and nothing else breaks —
// see PodUsage.
var podMetricsGVR = schema.GroupVersionResource{
	Group:    "metrics.k8s.io",
	Version:  "v1beta1",
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
//
// That conflation is what made the empty panels undiagnosable, and it is kept
// only because the alternative is worse. A 404 here means one of several things
// — no metrics-server, no permission to read it, or a version the group does not
// serve — and none of them is distinguishable from the response alone, so each
// would have to be reported as a guess. What is logged instead is the one fact
// the caller cannot see: that the request was answered 404 at all, which is what
// separates "this cluster has no metrics" from the silent-empty case that made
// the bug invisible. See podMetricsGVR for the version mismatch that did it.
func (c *Client) PodUsage(ctx context.Context, namespace, selector string) (map[string]Usage, bool, error) {
	if c.dynamic == nil {
		return nil, false, nil
	}

	// The version is named explicitly rather than left to the client's
	// negotiation, because there is nothing to negotiate: a group that serves
	// one version answers 404 for any other, and that 404 is swallowed below.
	// See podMetricsGVR.
	list, err := c.dynamic.Resource(podMetricsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// No metrics API this client can read. Not a failure; see the doc
			// comment. Logged rather than silent, because this is the branch that
			// hides a version or permission mistake behind an empty panel.
			slog.DebugContext(ctx, "this cluster answered 404 for the resource metrics API, so no usage is available",
				"group_version", podMetricsGVR.GroupVersion().String(), "namespace", namespace)
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read pod usage in %s: %w", namespace, err)
	}

	out := make(map[string]Usage, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]

		// The timestamp is on the pod, and describes the window every container
		// below was sampled over.
		stamp, _, _ := unstructured.NestedString(item.Object, "timestamp")

		// Usage is per container, and a pod's is their sum.
		//
		// This read used to be `NestedString(item.Object, "usage", "cpu")` —
		// a top-level field that does not exist on a PodMetrics. It does on a
		// *Node*Metrics, which is presumably where the shape was taken from, so
		// the lookup was well-formed and always empty: every pod reported no cpu
		// and no memory, the map was still keyed by pod name, and the console
		// drew nothing while saying the pod was fine. metrics.k8s.io/v1beta1
		// defines PodMetrics as metadata, timestamp, window and containers[],
		// and only ContainerMetrics has a usage.
		//
		// Summed rather than picked out of one container, because a pod is what
		// this reports on: an app's pod may carry a sidecar — Istio's proxy is
		// injected into these namespaces — and reporting the app container alone
		// would understate what the pod costs the node, which is the number
		// anyone reading this is asking about.
		cpu, cpuSeen := sumContainerUsage(item.Object, "cpu")
		memory, memSeen := sumContainerUsage(item.Object, "memory")
		usage := Usage{PodName: item.GetName(), Timestamp: stamp}
		if cpuSeen {
			usage.CPU = cpu
		}
		if memSeen {
			usage.Memory = memory
		}
		out[item.GetName()] = usage
	}
	return out, true, nil
}

// sumContainerUsage adds one resource across a PodMetrics' containers.
//
// The second return says whether any container reported it, which is what keeps
// "nothing reported this yet" apart from a real zero. A pod whose sample has not
// landed has no containers with a usage, and summing nothing would otherwise
// produce "0" — an idle-looking pod that may be starving, which is the failure
// the callers of this package are built to avoid.
//
// Quantities are the metrics API's own strings ("12m", "48Mi"), parsed rather
// than concatenated. One that does not parse is skipped rather than failing the
// read: this is a display path, and the rest of the pod's containers are still
// worth reporting.
func sumContainerUsage(item map[string]any, name string) (string, bool) {
	containers, ok := item["containers"].([]any)
	if !ok {
		return "", false
	}

	total := resource.NewQuantity(0, resource.DecimalSI)
	seen := false
	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// Read as strings rather than through a numeric accessor: the API
		// reports quantities with suffixes, and asking the unstructured layer
		// for a number would fail on every one of them.
		value, _, _ := unstructured.NestedString(container, "usage", name)
		if value == "" {
			continue
		}
		q, err := resource.ParseQuantity(value)
		if err != nil {
			continue
		}
		total.Add(q)
		seen = true
	}
	if !seen {
		return "", false
	}
	return total.String(), true
}
