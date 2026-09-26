// Package observe reports what is actually running.
//
// This is the half of AppLab that answers "why is it not working". Every other
// package describes what AppLab tried to do; these read the cluster directly,
// because a pod's log and a Kubernetes event are the things that say what
// happened rather than what was intended.
//
// Everything here is read-only and scoped to an app's namespace, which is
// derived from the deployment's prefix — so a call cannot reach a workload
// AppLab did not create.
package observe

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/shaowenchen/applab/internal/k8s"
)

// Observer reads an app's runtime state.
type Observer struct {
	client kubernetes.Interface

	// usage reads the resource metrics API, which is a separate group served by
	// metrics-server rather than by the API server. It is the k8s client rather
	// than a function because what it wraps is a resource address, not a
	// dependency the caller can be handed.
	//
	// Nil on a Client built without a dynamic client — see k8s.NewWithClientset —
	// in which case usage is reported as unavailable rather than as zero.
	usage k8s.UsageReader

	// maxLogBytes bounds a single log read. A container that logs in a loop can
	// produce gigabytes, and an endpoint that reads all of it would exhaust
	// memory long before it answered.
	maxLogBytes int64
}

// New creates an Observer.
func New(client kubernetes.Interface) *Observer {
	return &Observer{
		client:      client,
		maxLogBytes: 4 << 20, // 4 MiB
	}
}

// WithUsage attaches the resource metrics reader.
//
// Separate from New, and optional, because it depends on the dynamic client
// rather than on the typed one — so a caller that built a client without one gets
// an Observer that reports usage as unavailable rather than one that cannot be
// constructed. The console's resource panel is the only thing that reads it.
func (o *Observer) WithUsage(r k8s.UsageReader) *Observer {
	o.usage = r
	return o
}

// Ready reports whether the observer can reach the cluster.
func (o *Observer) Ready() bool { return o.client != nil }

// Pod is one pod of an app, reduced to what a caller needs to decide what to do.
type Pod struct {
	Name     string `json:"name"`
	Phase    string `json:"phase"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`

	// Node is reported because a pod stuck on one node is a different problem
	// from a pod that will not start anywhere.
	Node string `json:"node,omitempty"`

	// Image is the image the container actually runs, which is how a caller
	// confirms a deploy took effect.
	Image string `json:"image,omitempty"`

	// Reason and Message explain a pod that is not Running — "CrashLoopBackOff",
	// "ImagePullBackOff". It is the single most useful thing to surface.
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`

	// StartedAt is when the current container instance started, which is how a
	// restart storm is spotted.
	StartedAt time.Time `json:"started_at,omitempty"`

	// Containers reports per-container state, since an app with a sidecar can
	// have one healthy and one not.
	Containers []ContainerState `json:"containers,omitempty"`

	// Labels are the pod's own labels, which is what a caller filters a list of
	// pods by: during a rollout the app's pods carry the commit they were built
	// from, and "show me only the new revision" is a label selection.
	//
	// They are the app's own labels — its id, its commit, and the marker saying
	// AppLab manages them — so there is nothing here a caller allowed to read the
	// app's pods could not already derive.
	Labels map[string]string `json:"labels,omitempty"`
}

// ContainerState is one container's state within a pod.
type ContainerState struct {
	Name     string `json:"name"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`

	// LastTerminatedReason explains why the previous instance stopped, which is
	// what a crash loop actually looks like: the current state is "waiting"
	// while the reason is in the last termination.
	LastTerminatedReason string `json:"last_terminated_reason,omitempty"`
	LastExitCode         int32  `json:"last_exit_code,omitempty"`
}

// Usage is what an app is using and what it is allowed to use.
//
// The two halves come from different places and are deliberately together,
// because neither answers the question on its own: usage with no bound says
// nothing about whether the app is near its limit, and a bound with no usage is
// just the setting, which the app's own record already carries.
type Usage struct {
	// Available is whether the cluster could report usage at all. False on a
	// cluster with no metrics-server, which is a legitimate way to run one — the
	// panel then shows the bounds and says usage is unavailable, rather than
	// showing zeros that look like an idle app.
	Available bool `json:"available"`

	// CPU and Memory are the totals across the app's pods, in the units the
	// metrics API reports. Summed rather than listed: what an app is using is the
	// process's question, and which replica is using more is the pods endpoint's.
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`

	// Requested and Limited are what the running pods may use, read from the
	// container spec — so they are the *effective* values, with the deployment's
	// defaults already resolved onto the app's own where it had none.
	//
	// That is why they are read from the cluster rather than from the app's
	// record: the record holds only what was set for this app, and a panel
	// showing an empty limit for an app that is in fact capped by the deployment's
	// default would be describing a container that does not exist.
	Requested ResourceBounds `json:"requested"`
	Limited   ResourceBounds `json:"limited"`
}

// ResourceBounds is a CPU and memory pair.
type ResourceBounds struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// AppUsage reads what an app's pods are using, and what they are allowed to.
//
// The bounds come from the app's Deployment, which is where the deployer wrote
// them — so what a caller sees is what the scheduler saw, not a second reading of
// the app's settings that could disagree with it.
func (o *Observer) AppUsage(ctx context.Context, namespace, appID string) (Usage, error) {
	var out Usage

	// The bounds first, from the Deployment. An app with none has no container
	// spec to read a limit from, and reports zero values with available false —
	// which is the honest answer for something that is not running.
	deployment, err := o.client.AppsV1().Deployments(namespace).Get(ctx, "applab-"+appID, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return out, nil
		}
		return out, fmt.Errorf("read the deployment for %s: %w", appID, err)
	}
	if len(deployment.Spec.Template.Spec.Containers) > 0 {
		c := deployment.Spec.Template.Spec.Containers[0]
		out.Requested = ResourceBounds{
			CPU:    quantityString(c.Resources.Requests, corev1.ResourceCPU),
			Memory: quantityString(c.Resources.Requests, corev1.ResourceMemory),
		}
		out.Limited = ResourceBounds{
			CPU:    quantityString(c.Resources.Limits, corev1.ResourceCPU),
			Memory: quantityString(c.Resources.Limits, corev1.ResourceMemory),
		}
	}

	if o.usage == nil {
		return out, nil
	}

	// The app's pods, and not a build's — the same selector the pod list uses, for
	// the same reason: a build Job's pod carries the app label too, and counting
	// one would report a build's resource use as the app's.
	pods, usageAvailable, err := o.usage.PodUsage(ctx, namespace, k8s.LabelApp+"="+appID+",!"+k8s.LabelBuild)
	if err != nil {
		return out, err
	}
	if !usageAvailable {
		return out, nil
	}

	out.Available = true
	out.CPU, out.Memory, err = sumUsage(pods)
	if err != nil {
		return out, err
	}
	return out, nil
}

// sumUsage adds up the CPU and memory across pods.
//
// The quantities arrive as strings with suffixes — "12m", "48Mi" — so they are
// parsed rather than concatenated. A pod with no sample yet contributes nothing
// rather than failing the sum: a rollout in progress has some pods reporting and
// some not, and the useful answer there is the total of what is known.
func sumUsage(pods map[string]k8s.Usage) (cpu, memory string, err error) {
	cpuTotal := resource.NewQuantity(0, resource.DecimalSI)
	memTotal := resource.NewQuantity(0, resource.BinarySI)

	for _, usage := range pods {
		if usage.CPU != "" {
			q, parseErr := resource.ParseQuantity(usage.CPU)
			if parseErr != nil {
				return "", "", fmt.Errorf("the metrics API reported cpu %q, which is not a quantity: %w", usage.CPU, parseErr)
			}
			cpuTotal.Add(q)
		}
		if usage.Memory != "" {
			q, parseErr := resource.ParseQuantity(usage.Memory)
			if parseErr != nil {
				return "", "", fmt.Errorf("the metrics API reported memory %q, which is not a quantity: %w", usage.Memory, parseErr)
			}
			memTotal.Add(q)
		}
	}
	return cpuTotal.String(), memTotal.String(), nil
}

// quantityString renders a resource list's entry, or an empty string when it is
// not there.
func quantityString(list corev1.ResourceList, name corev1.ResourceName) string {
	if q, ok := list[name]; ok {
		return q.String()
	}
	return ""
}

// Pods lists an app's pods, newest first.
//
// The pods are found by the app's own label rather than by the Deployment's
// name, so pods from an older ReplicaSet during a rollout are included — a
// caller asking what is running wants all of it, not just the current revision.
func (o *Observer) Pods(ctx context.Context, namespace, appID string, limit int) ([]Pod, error) {
	// The app's own pods, and only those. A build Job's pod carries the app
	// label too — it is how the sweep finds it to delete — so it is excluded by
	// the second label rather than by a name prefix, which is the mistake the
	// package comment warns about: "applab-shop-build-..." and an app called
	// "shop-build" would be the same string.
	//
	// Without this, every build in flight appears in the app's instance list as
	// a pod that is not ready and eventually succeeds, which reads as the app
	// itself being broken.
	//
	// `!build` and not `build!=`: an existence test excludes any pod carrying the
	// label, while `build!=` parses as "the label's value is not the empty
	// string" and therefore matches every build pod, since a build's id is never
	// empty. The two look interchangeable and are not.
	pods, err := o.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: k8s.LabelApp + "=" + appID + ",!" + k8s.LabelBuild,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods in %s: %w", namespace, err)
	}

	out := make([]Pod, 0, len(pods.Items))
	for i := range pods.Items {
		out = append(out, toPod(&pods.Items[i]))
	}

	// Newest first, so the current revision is what a caller reads first and a
	// long list can be truncated without losing it.
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// BuildPods lists one app's build pods, keyed by build id.
//
// A build's pod is not an instance of the app — it is the thing that produced the
// image the instances run — so it belongs with the build it belongs to rather
// than in the app's pod list. This is the read that puts it there.
//
// Keyed by build id because that is what a caller has: the label carries the
// build's id, and a caller rendering a build's row is holding exactly that. A
// build whose Job has been collected by its TTL has no pod and simply has no
// entry, which is the same "nothing to show" a build that never started gets.
//
// Only pods, not Jobs: a Job that is waiting to be scheduled has no pod yet and
// the build record already says "pending", so there is nothing a pod could add.
// Empty result is not an error — most builds are long finished.
func (o *Observer) BuildPods(ctx context.Context, namespace, appID string) (map[string]Pod, error) {
	return o.buildPods(ctx, namespace, k8s.LabelApp+"="+appID)
}

// AllBuildPods lists every app's build pods in a namespace, keyed by build id.
//
// It exists for the overview, which lists recent builds across apps and would
// otherwise need one cluster read per app on the list. Keying by build id alone
// is enough because build ids are random and globally unique — they come from
// NewID, not from a per-app sequence — so two apps' builds cannot collide.
func (o *Observer) AllBuildPods(ctx context.Context, namespace string) (map[string]Pod, error) {
	return o.buildPods(ctx, namespace, k8s.LabelBuild)
}

// buildPods reads build pods matching the app selector, with the build label
// required, and keys them by build id.
func (o *Observer) buildPods(ctx context.Context, namespace, appSelector string) (map[string]Pod, error) {
	pods, err := o.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: appSelector + "," + k8s.LabelBuild,
	})
	if err != nil {
		return nil, fmt.Errorf("list build pods in %s: %w", namespace, err)
	}

	out := make(map[string]Pod, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		buildID := pod.Labels[k8s.LabelBuild]
		if buildID == "" {
			// Unreachable through this selector, which requires the label, but a
			// lookup under an empty key would put one build's pod on every row
			// that has no pod — so it is skipped rather than trusted.
			continue
		}
		out[buildID] = toPod(pod)
	}
	return out, nil
}

// toPod reduces a pod to the fields worth reporting.
func toPod(pod *corev1.Pod) Pod {
	out := Pod{
		Name:      pod.Name,
		Phase:     string(pod.Status.Phase),
		Node:      pod.Spec.NodeName,
		StartedAt: pod.CreationTimestamp.Time,
	}

	// A copy, not the pod's own map: the result outlives the call, and a caller
	// that filtered or annotated it in place would be writing into the object
	// the client returned.
	if len(pod.Labels) > 0 {
		out.Labels = make(map[string]string, len(pod.Labels))
		for k, v := range pod.Labels {
			out.Labels[k] = v
		}
	}

	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			out.Ready = true
		}
	}

	// A pod's containers are checked rather than its phase alone: a pod can be
	// Running while its only container is crash-looping, and the container's
	// reason is the one that explains it.
	for _, status := range pod.Status.ContainerStatuses {
		state := ContainerState{
			Name:     status.Name,
			Ready:    status.Ready,
			Restarts: status.RestartCount,
			State:    describeState(status.State),
		}
		out.Restarts += status.RestartCount

		if status.State.Waiting != nil {
			state.Reason = status.State.Waiting.Reason
		}
		if status.State.Terminated != nil {
			state.Reason = status.State.Terminated.Reason
		}
		// The previous instance's reason is where a crash loop lives: the
		// current state is "waiting" and says nothing, while the last
		// termination says "OOMKilled" or "Error".
		if status.LastTerminationState.Terminated != nil {
			state.LastTerminatedReason = status.LastTerminationState.Terminated.Reason
			state.LastExitCode = status.LastTerminationState.Terminated.ExitCode
		}

		out.Containers = append(out.Containers, state)

		// The first container is the app's own; its image is the one a caller
		// means by "what is running".
		if out.Image == "" {
			out.Image = status.Image
		}
		// Surface the most alarming state as the pod's own, so a caller that
		// reads only the top-level fields still learns what is wrong.
		if state.LastTerminatedReason != "" && out.Reason == "" {
			out.Reason = state.LastTerminatedReason
		}
		if state.Reason != "" && out.Reason == "" {
			out.Reason = state.Reason
		}
	}

	// A pod that has not started has its reason on the pod rather than on a
	// container — a scheduling failure, an unsatisfiable image pull.
	if out.Reason == "" && pod.Status.Reason != "" {
		out.Reason = pod.Status.Reason
		out.Message = pod.Status.Message
	}

	return out
}

// describeState names a container's state, mapping the three-way union onto one
// word.
func describeState(state corev1.ContainerState) string {
	switch {
	case state.Running != nil:
		return "running"
	case state.Waiting != nil:
		return "waiting"
	case state.Terminated != nil:
		return "terminated"
	default:
		return "unknown"
	}
}

// Event is one Kubernetes event concerning an app.
type Event struct {
	Type      string    `json:"type"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
	Object    string    `json:"object"`
	Count     int32     `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// Events returns recent Kubernetes events concerning one app.
//
// Events are the answer to questions the pod list cannot answer — why a pod was
// never scheduled, why an image pull failed, why a liveness probe killed a
// container — and they are the first thing a Kubernetes operator reaches for. They
// expire after about an hour, so a caller that wants history has to record them
// itself; this reads what is there now.
//
// Filtering is by exact object name rather than by name prefix, because every
// app shares one namespace and a prefix is not enough to tell them apart: with
// apps "shop" and "shop-2", the prefix "app-shop-" matches shop-2's pods too,
// and this endpoint would report one app's failures under another's name. The
// names are collected from the objects themselves, so each app's events are
// matched to it by the label it carries rather than by a guess about its name.
func (o *Observer) Events(ctx context.Context, namespace, appID string, limit int) ([]Event, error) {
	names, err := o.appObjectNames(ctx, namespace, appID)
	if err != nil {
		return nil, err
	}

	events, err := o.client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list events in %s: %w", namespace, err)
	}

	out := make([]Event, 0, len(events.Items))
	for i := range events.Items {
		e := &events.Items[i]

		// An event about something in this namespace that this app did not
		// create — another app's pod, AppLab's own Deployment — is not this
		// app's to report.
		if _, mine := names[e.InvolvedObject.Name]; !mine {
			continue
		}

		lastSeen := e.LastTimestamp.Time
		if lastSeen.IsZero() {
			// An event that has not been updated yet carries only its creation
			// time; without this fallback it would sort as if it were ancient.
			lastSeen = e.EventTime.Time
		}
		if lastSeen.IsZero() {
			lastSeen = e.CreationTimestamp.Time
		}

		firstSeen := e.FirstTimestamp.Time
		if firstSeen.IsZero() {
			firstSeen = e.CreationTimestamp.Time
		}

		out = append(out, Event{
			Type:      e.Type,
			Reason:    e.Reason,
			Message:   e.Message,
			Object:    e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
			Count:     e.Count,
			FirstSeen: firstSeen.UTC(),
			LastSeen:  lastSeen.UTC(),
		})
	}

	// Warnings first, then newest first within each group.
	//
	// The alternative — purely by recency — buries the event that explains a
	// failure among routine pull and scheduling notices, which are the majority
	// of events in a healthy namespace and are never what a caller is looking
	// for. Events expire within the hour, so a warning in this list is recent by
	// construction, and an operator asking "what is wrong" wants it at the top
	// regardless of whether a routine notice arrived a second later.
	sort.Slice(out, func(i, j int) bool {
		iWarn := out[i].Type == corev1.EventTypeWarning
		jWarn := out[j].Type == corev1.EventTypeWarning
		if iWarn != jWarn {
			return iWarn
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// appObjectNames returns every object name in the namespace that carries the
// app's label.
//
// It is how an event is attributed to an app. The alternative — matching the
// event's object name against a prefix built from the app id — looks simpler and
// is wrong: apps "shop" and "shop-2" share the prefix "applab-shop-", so a prefix
// match reports one app's pod failures under another app's name. Asking each
// kind for its labeled objects costs a few list calls and cannot confuse two
// apps.
//
// A kind that cannot be listed is skipped rather than failing the whole read:
// the pod list is the part that matters, and a caller asking why an app is
// unwell should still get the events that can be found.
func (o *Observer) appObjectNames(ctx context.Context, namespace, appID string) (map[string]struct{}, error) {
	selector := metav1.ListOptions{LabelSelector: "applab.io/app=" + appID}

	names := map[string]struct{}{}

	pods, err := o.client.CoreV1().Pods(namespace).List(ctx, selector)
	if err != nil {
		return nil, fmt.Errorf("list pods in %s: %w", namespace, err)
	}
	for i := range pods.Items {
		names[pods.Items[i].Name] = struct{}{}
	}

	// A pod's owner is named here too because Kubernetes reports a failed
	// scheduling against the ReplicaSet or the Job as often as against the pod.
	if deployments, err := o.client.AppsV1().Deployments(namespace).List(ctx, selector); err == nil {
		for i := range deployments.Items {
			names[deployments.Items[i].Name] = struct{}{}
		}
	}
	if replicaSets, err := o.client.AppsV1().ReplicaSets(namespace).List(ctx, selector); err == nil {
		for i := range replicaSets.Items {
			names[replicaSets.Items[i].Name] = struct{}{}
		}
	}
	if services, err := o.client.CoreV1().Services(namespace).List(ctx, selector); err == nil {
		for i := range services.Items {
			names[services.Items[i].Name] = struct{}{}
		}
	}
	if ingresses, err := o.client.NetworkingV1().Ingresses(namespace).List(ctx, selector); err == nil {
		for i := range ingresses.Items {
			names[ingresses.Items[i].Name] = struct{}{}
		}
	}
	if jobs, err := o.client.BatchV1().Jobs(namespace).List(ctx, selector); err == nil {
		for i := range jobs.Items {
			names[jobs.Items[i].Name] = struct{}{}
		}
	}

	return names, nil
}

// LogOptions selects a container and how much of its log to read.
type LogOptions struct {
	Pod       string
	Container string

	// TailLines reads only the last N lines. Zero means from the start, bounded
	// by the byte cap.
	TailLines int64

	// Previous reads the log of the container's previous instance, which is
	// where the reason for a crash loop is written.
	Previous bool

	// Since reads only recent output.
	Since time.Duration
}

// Logs returns a container's log.
//
// A pod with several containers requires one to be named, because "the log" is
// ambiguous and picking for the caller would silently give the wrong one.
// The most recently started container is the default when only one app container
// exists — the common case, so a caller need not know the container's name.
func (o *Observer) Logs(ctx context.Context, namespace, appID string, opts LogOptions) (string, error) {
	podName, container, err := o.resolvePod(ctx, namespace, appID, opts)
	if err != nil {
		return "", err
	}

	logOpts := &corev1.PodLogOptions{
		Container: container,
		Previous:  opts.Previous,
	}
	if opts.TailLines > 0 {
		logOpts.TailLines = &opts.TailLines
	}
	if opts.Since > 0 {
		seconds := int64(opts.Since.Seconds())
		logOpts.SinceSeconds = &seconds
	}

	stream, err := o.client.CoreV1().Pods(namespace).GetLogs(podName, logOpts).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("read logs for pod %s: %w", podName, err)
	}
	defer stream.Close()

	return readCapped(stream, o.maxLogBytes)
}

// StreamLogs follows a container's log until the context ends or the container
// stops producing output.
//
// It writes to w as lines arrive rather than returning, so a caller streaming to
// a client does not have to buffer. flush is called after each write, which is
// what makes the output actually reach a client rather than sitting in a buffer.
func (o *Observer) StreamLogs(ctx context.Context, namespace, appID string, opts LogOptions, w io.Writer, flush func()) error {
	podName, container, err := o.resolvePod(ctx, namespace, appID, opts)
	if err != nil {
		return err
	}

	logOpts := &corev1.PodLogOptions{
		Container: container,
		Follow:    true,
		Previous:  opts.Previous,
	}
	if opts.TailLines > 0 {
		logOpts.TailLines = &opts.TailLines
	} else {
		// A follow with no tail replays the whole log first; a small tail keeps
		// the useful recent context without streaming an entire container's
		// output before the live part.
		tail := int64(200)
		logOpts.TailLines = &tail
	}
	if opts.Since > 0 {
		seconds := int64(opts.Since.Seconds())
		logOpts.SinceSeconds = &seconds
	}

	stream, err := o.client.CoreV1().Pods(namespace).GetLogs(podName, logOpts).Stream(ctx)
	if err != nil {
		return fmt.Errorf("follow logs for pod %s: %w", podName, err)
	}
	defer stream.Close()

	if err := copyLines(ctx, stream, w, flush); err != nil {
		return fmt.Errorf("stream logs for pod %s: %w", podName, err)
	}
	return nil
}

// resolvePod picks the pod and container a log request refers to.
//
// When the caller names neither, the most recently started pod is chosen and,
// within it, the container whose name is not a known sidecar — which is the
// app's own. Requiring the caller to name both would be correct but unusable: an
// agent asking "what does this app log" does not know either.
func (o *Observer) resolvePod(ctx context.Context, namespace, appID string, opts LogOptions) (string, string, error) {
	if opts.Pod != "" {
		pod, err := o.client.CoreV1().Pods(namespace).Get(ctx, opts.Pod, metav1.GetOptions{})
		if err != nil {
			return "", "", fmt.Errorf("read pod %s: %w", opts.Pod, err)
		}
		if !belongsToApp(pod, appID) {
			// A pod in the app's namespace that is not this app's — a build job
			// pod, or a neighbour. Refusing is what keeps a log request from
			// reading something it has no business reading.
			return "", "", fmt.Errorf("pod %s is not part of app %q", opts.Pod, appID)
		}
		container, err := pickContainer(pod, opts.Container)
		if err != nil {
			return "", "", err
		}
		return pod.Name, container, nil
	}

	pods, err := o.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "applab.io/app=" + appID,
	})
	if err != nil {
		return "", "", fmt.Errorf("list pods in %s: %w", namespace, err)
	}
	if len(pods.Items) == 0 {
		return "", "", fmt.Errorf("app %q has no pods; it may not be deployed", appID)
	}

	newest := pods.Items[0]
	for _, candidate := range pods.Items[1:] {
		if candidate.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = candidate
		}
	}

	container, err := pickContainer(&newest, opts.Container)
	if err != nil {
		return "", "", err
	}
	return newest.Name, container, nil
}

// belongsToApp reports whether a pod carries the app's label.
func belongsToApp(pod *corev1.Pod, appID string) bool {
	return pod.Labels["applab.io/app"] == appID
}

// ---------------------------------------------------------------------------
// AppLab's own pods
//
// The same reads as above, addressed to the deployment itself rather than to an
// app it manages. They exist because the alternative is `kubectl logs` — which
// is fine for whoever already has cluster access and useless to everyone else,
// including whoever is debugging through the console.
//
// These are found by the chart's own labels rather than by the app label, which
// is what keeps them from ever returning an app's pod: an app carries
// applab.io/app and no app.kubernetes.io/name, and AppLab carries the reverse.
// ---------------------------------------------------------------------------

// selfSelector matches the pods the chart installs: AppLab and its
// ServiceMonitor, but no app.
const selfSelector = "app.kubernetes.io/part-of=applab"

// SelfPods lists AppLab's own pods, newest first.
func (o *Observer) SelfPods(ctx context.Context, namespace string, limit int) ([]Pod, error) {
	pods, err := o.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selfSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("list applab's pods in %s: %w", namespace, err)
	}

	out := make([]Pod, 0, len(pods.Items))
	for i := range pods.Items {
		out = append(out, toPod(&pods.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SelfLogs returns one of AppLab's own containers' logs.
//
// The target resolves like an app's — newest pod, and the caller may name one —
// with one difference: pickContainer prefers a container called "app", which is
// what the deployer names an app's, and AppLab's own is "applab". So the default
// here is the pod's first container rather than a name that will never match.
func (o *Observer) SelfLogs(ctx context.Context, namespace string, opts LogOptions) (string, error) {
	podName, container, err := o.resolveSelfPod(ctx, namespace, opts)
	if err != nil {
		return "", err
	}

	logOpts := &corev1.PodLogOptions{
		Container: container,
		Previous:  opts.Previous,
	}
	if opts.TailLines > 0 {
		logOpts.TailLines = &opts.TailLines
	}
	if opts.Since > 0 {
		seconds := int64(opts.Since.Seconds())
		logOpts.SinceSeconds = &seconds
	}

	stream, err := o.client.CoreV1().Pods(namespace).GetLogs(podName, logOpts).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("read logs for pod %s: %w", podName, err)
	}
	defer stream.Close()

	return readCapped(stream, o.maxLogBytes)
}

// StreamSelfLogs follows one of AppLab's own containers' logs.
func (o *Observer) StreamSelfLogs(ctx context.Context, namespace string, opts LogOptions, w io.Writer, flush func()) error {
	podName, container, err := o.resolveSelfPod(ctx, namespace, opts)
	if err != nil {
		return err
	}

	logOpts := &corev1.PodLogOptions{
		Container: container,
		Follow:    true,
		Previous:  opts.Previous,
	}
	if opts.TailLines > 0 {
		logOpts.TailLines = &opts.TailLines
	} else {
		tail := int64(200)
		logOpts.TailLines = &tail
	}
	if opts.Since > 0 {
		seconds := int64(opts.Since.Seconds())
		logOpts.SinceSeconds = &seconds
	}

	stream, err := o.client.CoreV1().Pods(namespace).GetLogs(podName, logOpts).Stream(ctx)
	if err != nil {
		return fmt.Errorf("follow logs for pod %s: %w", podName, err)
	}
	defer stream.Close()

	if err := copyLines(ctx, stream, w, flush); err != nil {
		return fmt.Errorf("follow logs for pod %s: %w", podName, err)
	}
	return nil
}

// resolveSelfPod picks which of AppLab's pods and containers a request refers to.
func (o *Observer) resolveSelfPod(ctx context.Context, namespace string, opts LogOptions) (string, string, error) {
	if opts.Pod != "" {
		pod, err := o.client.CoreV1().Pods(namespace).Get(ctx, opts.Pod, metav1.GetOptions{})
		if err != nil {
			return "", "", fmt.Errorf("read pod %s: %w", opts.Pod, err)
		}
		if !isAppLabPod(pod) {
			// The same boundary an app's logs have, from the other side: naming
			// a pod here must not reach an app's pod, or this endpoint would be
			// a way to read any log in the namespace by name.
			return "", "", fmt.Errorf("pod %s is not part of the AppLab deployment", opts.Pod)
		}
		container, err := pickContainer(pod, opts.Container)
		if err != nil {
			return "", "", err
		}
		return pod.Name, container, nil
	}

	pods, err := o.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selfSelector,
	})
	if err != nil {
		return "", "", fmt.Errorf("list applab's pods in %s: %w", namespace, err)
	}
	if len(pods.Items) == 0 {
		return "", "", fmt.Errorf("applab has no pods in %s; it may be mid-rollout, or the namespace is wrong", namespace)
	}

	newest := pods.Items[0]
	for _, candidate := range pods.Items[1:] {
		if candidate.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = candidate
		}
	}

	container, err := pickContainer(&newest, opts.Container)
	if err != nil {
		return "", "", err
	}
	return newest.Name, container, nil
}

// isAppLabPod reports whether a pod is part of the deployment rather than an app.
func isAppLabPod(pod *corev1.Pod) bool {
	return pod.Labels["app.kubernetes.io/part-of"] == "applab"
}

// copyLines writes r to w a line at a time, flushing after each, and stops when
// the context ends.
//
// The context check is between reads rather than passed to the reader: a
// container that stops producing output leaves the read blocked, so a stream
// whose request has gone would otherwise hold the goroutine until the pod
// stopped. Checking here is what lets a cancelled request end the copy — the
// read itself is unblocked by closing the stream, which is the caller's defer.
func copyLines(ctx context.Context, r io.Reader, w io.Writer, flush func()) error {
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			if _, writeErr := io.WriteString(w, line); writeErr != nil {
				// The client went away, which is the normal way a stream ends.
				return nil
			}
			if flush != nil {
				flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		default:
		}
	}
}

// pickContainer chooses a container from a pod.
func pickContainer(pod *corev1.Pod, requested string) (string, error) {
	if requested != "" {
		for _, c := range pod.Spec.Containers {
			if c.Name == requested {
				return requested, nil
			}
		}
		// A container name that is not in the pod is a mistake worth naming,
		// rather than falling back to a different container's log.
		names := make([]string, 0, len(pod.Spec.Containers))
		for _, c := range pod.Spec.Containers {
			names = append(names, c.Name)
		}
		return "", fmt.Errorf("pod %s has no container %q; it has: %s", pod.Name, requested, strings.Join(names, ", "))
	}

	if len(pod.Spec.Containers) == 0 {
		return "", fmt.Errorf("pod %s has no containers", pod.Name)
	}
	// The app's container is named "app" by the deployer; prefer it so a pod
	// with sidecars logs the right one by default.
	for _, c := range pod.Spec.Containers {
		if c.Name == "app" {
			return c.Name, nil
		}
	}
	return pod.Spec.Containers[0].Name, nil
}

// readCapped reads up to max bytes, reporting that it truncated rather than
// silently returning a partial log that looks complete.
func readCapped(r io.Reader, max int64) (string, error) {
	limited := io.LimitReader(r, max+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return string(data), fmt.Errorf("read log: %w", err)
	}

	if int64(len(data)) > max {
		return string(data[:max]) + fmt.Sprintf("\n[AppLab] log truncated at %d bytes; use ?tail= to read the most recent lines instead\n", max), nil
	}
	return string(data), nil
}
