package observe

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestObserver(t *testing.T, objects ...runtime.Object) (*Observer, *fake.Clientset) {
	t.Helper()

	client := fake.NewSimpleClientset(objects...)
	return New(client), client
}

// appPod builds a pod carrying the app's label.
func appPod(name string, phase corev1.PodPhase, ready bool, containers ...corev1.ContainerStatus) *corev1.Pod {
	condition := corev1.ConditionFalse
	if ready {
		condition = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "applab-shop",
			Labels:            map[string]string{"applab.io/app": "shop"},
			CreationTimestamp: metav1.Now(),
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "app", Image: "registry.example.com/apps/shop:abc"}},
		},
		Status: corev1.PodStatus{
			Phase:             phase,
			ContainerStatuses: containers,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: condition,
			}},
		},
	}
}

// TestPodsReportsStateAndReason asserts a pod is reduced to what a caller needs
// to decide what to do.
func TestPodsReportsStateAndReason(t *testing.T) {
	pod := appPod("app-shop-1", corev1.PodRunning, true, corev1.ContainerStatus{
		Name:         "app",
		Ready:        true,
		RestartCount: 0,
		Image:        "registry.example.com/apps/shop:abc",
		State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	})

	o, _ := newTestObserver(t, pod)

	pods, err := o.Pods(context.Background(), "applab-shop", "shop", 10)
	if err != nil {
		t.Fatalf("Pods: %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("got %d pods, want 1", len(pods))
	}

	got := pods[0]
	if got.Name != "app-shop-1" {
		t.Errorf("name = %q", got.Name)
	}
	if !got.Ready {
		t.Error("ready = false for a ready pod")
	}
	if got.Phase != "Running" {
		t.Errorf("phase = %q, want Running", got.Phase)
	}
	if got.Image == "" {
		t.Error("the running image was not reported")
	}
	if got.Node != "node-1" {
		t.Errorf("node = %q", got.Node)
	}
}

// TestCrashLoopReasonComesFromThePreviousContainer is the field that matters
// most: in a crash loop the current container is merely "waiting", and the reason
// is written on the previous instance's termination.
//
// Reading only the current state would report a crash-looping pod as waiting with
// no explanation, which is exactly the case a caller is asking about.
func TestCrashLoopReasonComesFromThePreviousContainer(t *testing.T) {
	pod := appPod("app-shop-1", corev1.PodRunning, false, corev1.ContainerStatus{
		Name:         "app",
		Ready:        false,
		RestartCount: 7,
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s"},
		},
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				Reason:   "Error",
				ExitCode: 1,
			},
		},
	})

	o, _ := newTestObserver(t, pod)

	pods, err := o.Pods(context.Background(), "applab-shop", "shop", 10)
	if err != nil {
		t.Fatalf("Pods: %v", err)
	}

	got := pods[0]
	if got.Restarts != 7 {
		t.Errorf("restarts = %d, want 7", got.Restarts)
	}
	if got.Reason == "" {
		t.Fatal("no reason was reported for a crash-looping pod")
	}
	if !strings.Contains(got.Reason, "CrashLoop") && !strings.Contains(got.Reason, "Error") {
		t.Errorf("reason = %q, want the crash loop's cause", got.Reason)
	}

	// The per-container detail must carry the exit code, which is how "it
	// exited non-zero" is distinguished from "it was killed".
	if len(got.Containers) != 1 {
		t.Fatalf("got %d containers, want 1", len(got.Containers))
	}
	if got.Containers[0].LastExitCode != 1 {
		t.Errorf("last exit code = %d, want 1", got.Containers[0].LastExitCode)
	}
}

// TestPodLevelReasonIsReported asserts a pod that never started carries its
// reason, since an unschedulable pod has no container state to explain it.
func TestPodLevelReasonIsReported(t *testing.T) {
	pod := appPod("app-shop-1", corev1.PodPending, false)
	pod.Status.Reason = "Unschedulable"
	pod.Status.Message = "0/3 nodes are available: insufficient memory"

	o, _ := newTestObserver(t, pod)

	pods, err := o.Pods(context.Background(), "applab-shop", "shop", 10)
	if err != nil {
		t.Fatalf("Pods: %v", err)
	}
	if pods[0].Reason != "Unschedulable" {
		t.Errorf("reason = %q, want Unschedulable", pods[0].Reason)
	}
	if pods[0].Message == "" {
		t.Error("the message explaining why was not reported")
	}
}

// TestPodsAreScopedToTheApp asserts another app's pods are never returned, which
// is what keeps an observability call from reaching a neighbour's workload.
func TestPodsAreScopedToTheApp(t *testing.T) {
	shopPod := appPod("app-shop-1", corev1.PodRunning, true)
	other := appPod("app-blog-1", corev1.PodRunning, true)
	other.Labels["applab.io/app"] = "blog"

	o, _ := newTestObserver(t, shopPod, other)

	pods, err := o.Pods(context.Background(), "applab-shop", "shop", 10)
	if err != nil {
		t.Fatalf("Pods: %v", err)
	}
	if len(pods) != 1 || pods[0].Name != "app-shop-1" {
		t.Errorf("got %v, want only the shop pod", pods)
	}
}

// TestPodsNewestFirst asserts the current revision is what a caller reads first,
// which matters during a rollout when both revisions have pods.
func TestPodsNewestFirst(t *testing.T) {
	old := appPod("app-shop-old", corev1.PodRunning, true)
	old.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))

	fresh := appPod("app-shop-new", corev1.PodRunning, true)
	fresh.CreationTimestamp = metav1.Now()

	// Listed in the opposite order from the answer, so a missing sort is caught.
	o, _ := newTestObserver(t, old, fresh)

	pods, err := o.Pods(context.Background(), "applab-shop", "shop", 10)
	if err != nil {
		t.Fatalf("Pods: %v", err)
	}
	if pods[0].Name != "app-shop-new" {
		t.Errorf("first pod = %q, want the newest", pods[0].Name)
	}
}

// TestEventsWarningsFirst asserts an operator scanning the list sees problems
// before routine notices.
func TestEventsWarningsFirst(t *testing.T) {
	killed := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e1", Namespace: "applab-shop", CreationTimestamp: metav1.Now()},
		Type:           corev1.EventTypeWarning,
		Reason:         "BackOff",
		Message:        "Back-off restarting failed container",
		Count:          12,
		LastTimestamp:  metav1.Now(),
		FirstTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
	}
	pulled := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e2", Namespace: "applab-shop", CreationTimestamp: metav1.Now()},
		Type:           corev1.EventTypeNormal,
		Reason:         "Pulled",
		Message:        "Successfully pulled image",
		Count:          1,
		LastTimestamp:  metav1.Now(),
		FirstTimestamp: metav1.Now(),
	}

	o, _ := newTestObserver(t, pulled, killed)

	events, err := o.Events(context.Background(), "applab-shop", 10)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].Type != corev1.EventTypeWarning {
		t.Errorf("first event type = %q, want the warning first", events[0].Type)
	}
	if events[0].Count != 12 {
		t.Errorf("count = %d, want 12 — a repeated event is one event with a count", events[0].Count)
	}
}

// TestLogsRequiresExplicitContainerWhenAmbiguous asserts a multi-container pod
// resolves to the app's container rather than an arbitrary one.
func TestLogsRequiresExplicitContainerWhenAmbiguous(t *testing.T) {
	pod := appPod("app-shop-1", corev1.PodRunning, true)
	pod.Spec.Containers = []corev1.Container{
		{Name: "istio-proxy"},
		{Name: "app"},
	}

	// The container resolution is what is under test, so it is exercised
	// directly: the fake clientset cannot stream logs, and the resolvePod path
	// that calls this is covered separately.
	name, err := pickContainer(pod, "")
	if err != nil {
		t.Fatalf("pickContainer: %v", err)
	}
	if name != "app" {
		t.Errorf("container = %q, want the app's own container rather than a sidecar", name)
	}
}

// TestPickContainerHonoursAnExplicitName asserts a named container is used.
func TestPickContainerHonoursAnExplicitName(t *testing.T) {
	pod := appPod("app-shop-1", corev1.PodRunning, true)
	pod.Spec.Containers = []corev1.Container{{Name: "app"}, {Name: "sidecar"}}

	name, err := pickContainer(pod, "sidecar")
	if err != nil {
		t.Fatalf("pickContainer: %v", err)
	}
	if name != "sidecar" {
		t.Errorf("container = %q, want sidecar", name)
	}
}

// TestPickContainerRejectsAnUnknownName asserts a typo is reported rather than
// silently falling back to a different container's log, which would answer a
// question that was not asked.
func TestPickContainerRejectsAnUnknownName(t *testing.T) {
	pod := appPod("app-shop-1", corev1.PodRunning, true)
	pod.Spec.Containers = []corev1.Container{{Name: "app"}}

	if _, err := pickContainer(pod, "nope"); err == nil {
		t.Error("an unknown container name was accepted")
	} else if !strings.Contains(err.Error(), "app") {
		t.Errorf("the error should list the available containers, got: %v", err)
	}
}

// TestLogsRefusesAPodOfAnotherApp asserts a log request cannot read a pod that is
// not the app's, even when it is in the app's namespace — a build job's pod, for
// instance.
func TestLogsRefusesAPodOfAnotherApp(t *testing.T) {
	buildPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "applab-build-shop-abc",
			Namespace: "applab-shop",
			Labels:    map[string]string{"applab.io/build": "abc"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "build"}}},
	}

	o, _ := newTestObserver(t, buildPod)

	_, err := o.Logs(context.Background(), "applab-shop", "shop", LogOptions{Pod: "applab-build-shop-abc"})
	if err == nil {
		t.Fatal("a log request read a pod belonging to another app")
	}
	if !strings.Contains(err.Error(), "not part of app") {
		t.Errorf("the error should say the pod is not the app's, got: %v", err)
	}
}

// TestLogsOnAnAppWithNoPodsIsClear asserts the failure names the likely cause
// rather than surfacing a bare Kubernetes error.
func TestLogsOnAnAppWithNoPodsIsClear(t *testing.T) {
	o, _ := newTestObserver(t)

	_, err := o.Logs(context.Background(), "applab-shop", "shop", LogOptions{})
	if err == nil {
		t.Fatal("reading logs for an app with no pods succeeded")
	}
	if !strings.Contains(err.Error(), "no pods") {
		t.Errorf("the error should say there are no pods, got: %v", err)
	}
}

// TestResolvePodPicksTheNewest asserts the default pod is the current one.
func TestResolvePodPicksTheNewest(t *testing.T) {
	old := appPod("app-shop-old", corev1.PodRunning, true)
	old.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	fresh := appPod("app-shop-new", corev1.PodRunning, true)
	fresh.CreationTimestamp = metav1.Now()

	o, _ := newTestObserver(t, old, fresh)

	name, container, err := o.resolvePod(context.Background(), "applab-shop", "shop", LogOptions{})
	if err != nil {
		t.Fatalf("resolvePod: %v", err)
	}
	if name != "app-shop-new" {
		t.Errorf("pod = %q, want the newest", name)
	}
	if container != "app" {
		t.Errorf("container = %q, want app", container)
	}
}

// TestReadCappedTruncatesVisibly asserts an oversized log is cut off with a
// marker. Returning a truncated log silently would look like the whole log, and a
// caller would draw conclusions from a missing tail.
func TestReadCappedTruncatesVisibly(t *testing.T) {
	long := strings.Repeat("x", 1000)

	got, err := readCapped(strings.NewReader(long), 100)
	if err != nil {
		t.Fatalf("readCapped: %v", err)
	}
	if !strings.Contains(got, "truncated") {
		t.Error("a truncated log was returned with no indication that it was cut off")
	}
	if !strings.Contains(got, "tail=") {
		t.Error("the truncation marker does not say how to read the recent lines instead")
	}

	// A short log must come through whole and unmarked.
	short, err := readCapped(strings.NewReader("hello"), 100)
	if err != nil {
		t.Fatalf("readCapped: %v", err)
	}
	if short != "hello" {
		t.Errorf("short log = %q, want it unchanged", short)
	}
}

// TestReady asserts the observer reports unusability rather than panicking.
func TestReady(t *testing.T) {
	if !New(fake.NewSimpleClientset()).Ready() {
		t.Error("Ready() = false with a client")
	}
	if New(nil).Ready() {
		t.Error("Ready() = true with no client")
	}
}

// TestEventsFromAPodWithAContainerError asserts an event referencing a container
// is still reported with its object, since that is how a caller knows which pod
// it concerns.
func TestEventsCarryTheirObject(t *testing.T) {
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: "applab-shop", CreationTimestamp: metav1.Now()},
		Type:       corev1.EventTypeWarning,
		Reason:     "Failed",
		Message:    "Error: ImagePullBackOff",
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod",
			Name: "app-shop-1",
		},
		LastTimestamp: metav1.Now(),
	}

	o, _ := newTestObserver(t, event)

	events, err := o.Events(context.Background(), "applab-shop", 10)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Object != "Pod/app-shop-1" {
		t.Errorf("object = %q, want Pod/app-shop-1", events[0].Object)
	}
}

// TestEventsOfAnEmptyNamespaceIsNotAnError asserts an app with no events is a
// normal state, not a failure.
func TestEventsOfAnEmptyNamespaceIsNotAnError(t *testing.T) {
	o, _ := newTestObserver(t)

	events, err := o.Events(context.Background(), "applab-shop", 10)
	if err != nil {
		t.Fatalf("Events on an empty namespace: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("got %d events from an empty namespace", len(events))
	}
}

// TestPodsLimitIsApplied asserts a request for a bounded number returns that many.
func TestPodsLimitIsApplied(t *testing.T) {
	objects := make([]runtime.Object, 0, 10)
	for i := 0; i < 10; i++ {
		objects = append(objects, appPod("app-shop-"+string(rune('a'+i)), corev1.PodRunning, true))
	}

	o, _ := newTestObserver(t, objects...)

	pods, err := o.Pods(context.Background(), "applab-shop", "shop", 3)
	if err != nil {
		t.Fatalf("Pods: %v", err)
	}
	if len(pods) != 3 {
		t.Errorf("got %d pods, want the requested 3", len(pods))
	}
}
