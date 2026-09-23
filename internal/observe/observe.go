// Package observe reports what is actually running.
//
// This is the half of applab that answers "why is it not working". Every other
// package describes what applab tried to do; these read the cluster directly,
// because a pod's log and a Kubernetes event are the things that say what
// happened rather than what was intended.
//
// Everything here is read-only and scoped to an app's namespace, which is
// derived from the deployment's prefix — so a call cannot reach a workload
// applab did not create.
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Observer reads an app's runtime state.
type Observer struct {
	client kubernetes.Interface

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

// Pods lists an app's pods, newest first.
//
// The pods are found by the app's own label rather than by the Deployment's
// name, so pods from an older ReplicaSet during a rollout are included — a
// caller asking what is running wants all of it, not just the current revision.
func (o *Observer) Pods(ctx context.Context, namespace, appID string, limit int) ([]Pod, error) {
	pods, err := o.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "applab.io/app=" + appID,
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

// toPod reduces a pod to the fields worth reporting.
func toPod(pod *corev1.Pod) Pod {
	out := Pod{
		Name:      pod.Name,
		Phase:     string(pod.Status.Phase),
		Node:      pod.Spec.NodeName,
		StartedAt: pod.CreationTimestamp.Time,
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

// Events returns recent events for an app's namespace.
//
// Events are the answer to questions the pod list cannot answer — why a pod was
// never scheduled, why an image pull failed, why a liveness probe killed a
// container — and they are the first thing a Kubernetes operator reaches for. They
// expire after about an hour, so a caller that wants history has to record them
// itself; this reads what is there now.
func (o *Observer) Events(ctx context.Context, namespace string, limit int) ([]Event, error) {
	events, err := o.client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list events in %s: %w", namespace, err)
	}

	out := make([]Event, 0, len(events.Items))
	for i := range events.Items {
		e := &events.Items[i]

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

	reader := bufio.NewReader(stream)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			if _, writeErr := io.WriteString(w, line); writeErr != nil {
				// The client went away. Not an error worth reporting: it is the
				// normal way a stream ends.
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
			return fmt.Errorf("stream logs for pod %s: %w", podName, err)
		}

		select {
		case <-ctx.Done():
			return nil
		default:
		}
	}
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
		return string(data[:max]) + fmt.Sprintf("\n[applab] log truncated at %d bytes; use ?tail= to read the most recent lines instead\n", max), nil
	}
	return string(data), nil
}
