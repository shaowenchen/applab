package observe

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
			Namespace:         "ops-system",
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
	pod := appPod("applab-shop-1", corev1.PodRunning, true, corev1.ContainerStatus{
		Name:         "app",
		Ready:        true,
		RestartCount: 0,
		Image:        "registry.example.com/apps/shop:abc",
		State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	})

	o, _ := newTestObserver(t, pod)

	pods, err := o.Pods(context.Background(), "ops-system", "shop", 10)
	if err != nil {
		t.Fatalf("Pods: %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("got %d pods, want 1", len(pods))
	}

	got := pods[0]
	if got.Name != "applab-shop-1" {
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
	pod := appPod("applab-shop-1", corev1.PodRunning, false, corev1.ContainerStatus{
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

	pods, err := o.Pods(context.Background(), "ops-system", "shop", 10)
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
	pod := appPod("applab-shop-1", corev1.PodPending, false)
	pod.Status.Reason = "Unschedulable"
	pod.Status.Message = "0/3 nodes are available: insufficient memory"

	o, _ := newTestObserver(t, pod)

	pods, err := o.Pods(context.Background(), "ops-system", "shop", 10)
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
	shopPod := appPod("applab-shop-1", corev1.PodRunning, true)
	other := appPod("applab-blog-1", corev1.PodRunning, true)
	other.Labels["applab.io/app"] = "blog"

	o, _ := newTestObserver(t, shopPod, other)

	pods, err := o.Pods(context.Background(), "ops-system", "shop", 10)
	if err != nil {
		t.Fatalf("Pods: %v", err)
	}
	if len(pods) != 1 || pods[0].Name != "applab-shop-1" {
		t.Errorf("got %v, want only the shop pod", pods)
	}
}

// TestPodsNewestFirst asserts the current revision is what a caller reads first,
// which matters during a rollout when both revisions have pods.
func TestPodsNewestFirst(t *testing.T) {
	old := appPod("applab-shop-old", corev1.PodRunning, true)
	old.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))

	fresh := appPod("applab-shop-new", corev1.PodRunning, true)
	fresh.CreationTimestamp = metav1.Now()

	// Listed in the opposite order from the answer, so a missing sort is caught.
	o, _ := newTestObserver(t, old, fresh)

	pods, err := o.Pods(context.Background(), "ops-system", "shop", 10)
	if err != nil {
		t.Fatalf("Pods: %v", err)
	}
	if pods[0].Name != "applab-shop-new" {
		t.Errorf("first pod = %q, want the newest", pods[0].Name)
	}
}

// Events are attributed by object name, so each one needs a real object to
// belong to. This builds the pod the events below are about.
func eventPod(name, appID string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "ops-system",
			Labels:            map[string]string{"applab.io/app": appID},
			CreationTimestamp: metav1.Now(),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// TestEventsWarningsFirst asserts an operator scanning the list sees problems
// before routine notices.
func TestEventsWarningsFirst(t *testing.T) {
	killed := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e1", Namespace: "ops-system", CreationTimestamp: metav1.Now()},
		Type:           corev1.EventTypeWarning,
		Reason:         "BackOff",
		Message:        "Back-off restarting failed container",
		Count:          12,
		LastTimestamp:  metav1.Now(),
		FirstTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "applab-shop-1"},
	}
	pulled := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e2", Namespace: "ops-system", CreationTimestamp: metav1.Now()},
		Type:           corev1.EventTypeNormal,
		Reason:         "Pulled",
		Message:        "Successfully pulled image",
		Count:          1,
		LastTimestamp:  metav1.Now(),
		FirstTimestamp: metav1.Now(),
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "applab-shop-1"},
	}

	o, _ := newTestObserver(t, eventPod("applab-shop-1", "shop"), pulled, killed)

	events, err := o.Events(context.Background(), "ops-system", "shop", 10)
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
	pod := appPod("applab-shop-1", corev1.PodRunning, true)
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
	pod := appPod("applab-shop-1", corev1.PodRunning, true)
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
	pod := appPod("applab-shop-1", corev1.PodRunning, true)
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
			Namespace: "ops-system",
			Labels:    map[string]string{"applab.io/build": "abc"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "build"}}},
	}

	o, _ := newTestObserver(t, buildPod)

	_, err := o.Logs(context.Background(), "ops-system", "shop", LogOptions{Pod: "applab-build-shop-abc"})
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

	_, err := o.Logs(context.Background(), "ops-system", "shop", LogOptions{})
	if err == nil {
		t.Fatal("reading logs for an app with no pods succeeded")
	}
	if !strings.Contains(err.Error(), "no pods") {
		t.Errorf("the error should say there are no pods, got: %v", err)
	}
}

// TestResolvePodPicksTheNewest asserts the default pod is the current one.
func TestResolvePodPicksTheNewest(t *testing.T) {
	old := appPod("applab-shop-old", corev1.PodRunning, true)
	old.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	fresh := appPod("applab-shop-new", corev1.PodRunning, true)
	fresh.CreationTimestamp = metav1.Now()

	o, _ := newTestObserver(t, old, fresh)

	name, container, err := o.resolvePod(context.Background(), "ops-system", "shop", LogOptions{})
	if err != nil {
		t.Fatalf("resolvePod: %v", err)
	}
	if name != "applab-shop-new" {
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
		ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: "ops-system", CreationTimestamp: metav1.Now()},
		Type:       corev1.EventTypeWarning,
		Reason:     "Failed",
		Message:    "Error: ImagePullBackOff",
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod",
			Name: "applab-shop-1",
		},
		LastTimestamp: metav1.Now(),
	}

	o, _ := newTestObserver(t, eventPod("applab-shop-1", "shop"), event)

	events, err := o.Events(context.Background(), "ops-system", "shop", 10)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Object != "Pod/applab-shop-1" {
		t.Errorf("object = %q, want Pod/applab-shop-1", events[0].Object)
	}
}

// TestEventsDoNotLeakBetweenApps is the reason Events takes an app id at all.
//
// Every app shares one namespace, so a naive implementation reports every event
// in it. A caller asking why *their* app is unwell would be shown another app's
// crash loop and go looking in the wrong place — worse than being told nothing.
func TestEventsDoNotLeakBetweenApps(t *testing.T) {
	shopsEvent := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e-shop", Namespace: "ops-system", CreationTimestamp: metav1.Now()},
		Type:           corev1.EventTypeWarning,
		Reason:         "BackOff",
		Message:        "shop is crash looping",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "applab-shop-1"},
		LastTimestamp:  metav1.Now(),
	}
	blogsEvent := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e-blog", Namespace: "ops-system", CreationTimestamp: metav1.Now()},
		Type:           corev1.EventTypeWarning,
		Reason:         "FailedScheduling",
		Message:        "blog has no nodes to run on",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "applab-blog-1"},
		LastTimestamp:  metav1.Now(),
	}

	o, _ := newTestObserver(t,
		eventPod("applab-shop-1", "shop"),
		eventPod("applab-blog-1", "blog"),
		shopsEvent, blogsEvent,
	)

	events, err := o.Events(context.Background(), "ops-system", "shop", 10)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events for shop, want 1 — another app's events leaked in", len(events))
	}
	if !strings.Contains(events[0].Message, "shop") {
		t.Errorf("got %q, which is not shop's event", events[0].Message)
	}
}

// TestEventsForASharedPrefixAreNotConfused covers the mistake a name-prefix
// filter would make. "shop" and "shop-2" produce the prefixes app-shop- and
// app-shop-2-, and one is a prefix of the other: attributing by prefix would
// report shop-2's failures as shop's.
func TestEventsForASharedPrefixAreNotConfused(t *testing.T) {
	second := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e2", Namespace: "ops-system", CreationTimestamp: metav1.Now()},
		Type:           corev1.EventTypeWarning,
		Reason:         "BackOff",
		Message:        "shop-2 is crash looping",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "app-shop-2-abc"},
		LastTimestamp:  metav1.Now(),
	}

	o, _ := newTestObserver(t,
		eventPod("applab-shop-1", "shop"),
		eventPod("app-shop-2-abc", "shop-2"),
		second,
	)

	events, err := o.Events(context.Background(), "ops-system", "shop", 10)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("got %d events for shop, want 0 — shop-2's event was attributed to shop", len(events))
	}
}

// TestEventsIncludeDeploymentAndJobEvents asserts events about the objects
// around a pod are not dropped. A failed scheduling is reported against the
// ReplicaSet as often as against the pod, and a build failure against the Job.
func TestEventsIncludeDeploymentAndJobEvents(t *testing.T) {
	rollout := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e-rollout", Namespace: "ops-system", CreationTimestamp: metav1.Now()},
		Type:           corev1.EventTypeWarning,
		Reason:         "FailedCreate",
		Message:        "cannot create pods",
		InvolvedObject: corev1.ObjectReference{Kind: "ReplicaSet", Name: "app-shop-5f8c"},
		LastTimestamp:  metav1.Now(),
	}

	o, _ := newTestObserver(t,
		eventPod("applab-shop-1", "shop"),
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
			Name: "app-shop-5f8c", Namespace: "ops-system",
			Labels: map[string]string{"applab.io/app": "shop"},
		}},
		rollout,
	)

	events, err := o.Events(context.Background(), "ops-system", "shop", 10)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want the ReplicaSet's event", len(events))
	}
	if events[0].Object != "ReplicaSet/app-shop-5f8c" {
		t.Errorf("object = %q, want ReplicaSet/app-shop-5f8c", events[0].Object)
	}
}

// TestEventsOfAnEmptyNamespaceIsNotAnError asserts an app with no events is a
// normal state, not a failure.
func TestEventsOfAnEmptyNamespaceIsNotAnError(t *testing.T) {
	o, _ := newTestObserver(t)

	events, err := o.Events(context.Background(), "ops-system", "shop", 10)
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

	pods, err := o.Pods(context.Background(), "ops-system", "shop", 3)
	if err != nil {
		t.Fatalf("Pods: %v", err)
	}
	if len(pods) != 3 {
		t.Errorf("got %d pods, want the requested 3", len(pods))
	}
}

// ---------------------------------------------------------------------------
// AppLab's own pods and logs
//
// The control plane runs in the same namespace as every app, so "AppLab's pods"
// is a selection that has to exclude the apps — and "not an app's pod" is the
// boundary that keeps `?pod=` from naming any pod in the namespace and reading
// its log. Both directions are checked below.
// ---------------------------------------------------------------------------

// selfPod builds a pod carrying the chart's own labels.
func selfPod(name string, created time.Time, containers ...string) *corev1.Pod {
	spec := make([]corev1.Container, 0, len(containers))
	for _, c := range containers {
		spec = append(spec, corev1.Container{Name: c})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "ops-system",
			Labels:            map[string]string{"app.kubernetes.io/part-of": "applab"},
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: corev1.PodSpec{Containers: spec},
	}
}

// TestSelfPodsExcludesApps is the whole point of the selector. AppLab and every
// app it manages share a namespace, so a selector that matched too much would
// report an app's pod as part of the control plane — and this is the list someone
// reads to find out whether the control plane itself is up.
func TestSelfPodsExcludesApps(t *testing.T) {
	o, _ := newTestObserver(t,
		selfPod("applab-6b9f7-abc", time.Now(), "applab"),
		appPod("applab-shop-1", corev1.PodRunning, true),
	)

	pods, err := o.SelfPods(context.Background(), "ops-system", 10)
	if err != nil {
		t.Fatalf("SelfPods: %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("got %d pods, want only the control plane's", len(pods))
	}
	if pods[0].Name != "applab-6b9f7-abc" {
		t.Errorf("pod = %q, want applab's own", pods[0].Name)
	}
}

// TestSelfPodsNewestFirst asserts the order a reader scans in: the pod that is
// running now is the one whose log they will want, and during a rollout it is the
// only one that has anything to say.
func TestSelfPodsNewestFirst(t *testing.T) {
	o, _ := newTestObserver(t,
		selfPod("applab-old", time.Now().Add(-time.Hour), "applab"),
		selfPod("applab-new", time.Now(), "applab"),
	)

	pods, err := o.SelfPods(context.Background(), "ops-system", 10)
	if err != nil {
		t.Fatalf("SelfPods: %v", err)
	}
	if len(pods) != 2 || pods[0].Name != "applab-new" {
		got := make([]string, 0, len(pods))
		for _, p := range pods {
			got = append(got, p.Name)
		}
		t.Errorf("got %v, want the newest first", got)
	}
}

// TestSelfLogsRefusesAnAppsPod is the mirror of TestLogsRefusesAPodOfAnotherApp,
// and the reason `?pod=` is not a way to read any log in the namespace.
func TestSelfLogsRefusesAnAppsPod(t *testing.T) {
	o, _ := newTestObserver(t, appPod("applab-shop-1", corev1.PodRunning, true))

	_, err := o.SelfLogs(context.Background(), "ops-system", LogOptions{Pod: "applab-shop-1"})
	if err == nil {
		t.Fatal("the platform log read an app's pod")
	}
	if !strings.Contains(err.Error(), "not part of the AppLab deployment") {
		t.Errorf("the error should say the pod is not AppLab's, got: %v", err)
	}
}

// TestSelfLogsOnADeploymentWithNoPodsIsClear asserts the failure names the likely
// cause. A mid-rollout deployment really does have no pods for a moment, and
// "connection refused" would not say so.
func TestSelfLogsOnADeploymentWithNoPodsIsClear(t *testing.T) {
	o, _ := newTestObserver(t)

	_, err := o.SelfLogs(context.Background(), "ops-system", LogOptions{})
	if err == nil {
		t.Fatal("reading the platform log with no pods succeeded")
	}
	if !strings.Contains(err.Error(), "no pods") {
		t.Errorf("the error should say there are no pods, got: %v", err)
	}
}

// TestSelfLogsPicksTheNewestPod asserts the default target, which is the pod the
// reader means when they ask for "the" log.
func TestSelfLogsPicksTheNewestPod(t *testing.T) {
	o, _ := newTestObserver(t,
		selfPod("applab-old", time.Now().Add(-time.Hour), "applab"),
		selfPod("applab-new", time.Now(), "applab"),
	)

	name, container, err := o.resolveSelfPod(context.Background(), "ops-system", LogOptions{})
	if err != nil {
		t.Fatalf("resolveSelfPod: %v", err)
	}
	if name != "applab-new" {
		t.Errorf("pod = %q, want the newest", name)
	}
	if container != "applab" {
		t.Errorf("container = %q, want applab", container)
	}
}

// TestCopyLinesStopsWhenTheContextEnds asserts a cancelled request ends the copy.
//
// A container that has gone quiet leaves the read blocked, so without the check
// between reads the goroutine would outlive the request by however long the app
// took to log again — which for a healthy app is indefinitely.
func TestCopyLinesStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// A reader that never ends, which is what a followed stream looks like.
	endless := &blockingReader{}

	var out strings.Builder
	done := make(chan error, 1)
	go func() { done <- copyLines(ctx, endless, &out, nil) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("copyLines: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("copyLines did not return after the context was cancelled")
	}
}

// blockingReader hands back one line and then blocks forever.
type blockingReader struct{ sent bool }

func (r *blockingReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "first line\n"), nil
	}
	select {}
}
