package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/auth"
	"github.com/shaowenchen/applab/internal/config"
	"github.com/shaowenchen/applab/internal/observe"
	"github.com/shaowenchen/applab/internal/source"
	"github.com/shaowenchen/applab/internal/store"
)

// newObserveServer builds a Server with an observer over a fake cluster.
func newObserveServer(t *testing.T) (*api.Server, *fake.Clientset) {
	t.Helper()

	dataDir := t.TempDir()
	st, err := store.OpenLocal(context.Background(), filepath.Join(dataDir, "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	src, err := source.New(source.Options{Objects: newObjects(t), DataDir: dataDir})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	client := fake.NewSimpleClientset()

	cfg := config.Default()
	cfg.Keys = []string{"test-key"}
	cfg.BaseDomain = "apps.example.com"
	cfg.DataDir = dataDir

	srv := api.New(cfg, st, auth.New(cfg.Keys)).
		WithSource(src).
		WithObserver(observe.New(client)).
		WithMetrics(api.NewMetrics())

	return srv, client
}

// createAppForObserve creates an app so the observability endpoints have
// something to address.
func createAppForObserve(t *testing.T, h http.Handler, appID string) {
	t.Helper()

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": appID}); rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestPodsEndpointReportsState asserts the pod list carries what a caller needs.
func TestPodsEndpointReportsState(t *testing.T) {
	srv, client := newObserveServer(t)
	h := srv.Handler()
	createAppForObserve(t, h, "shop")

	_, err := client.CoreV1().Pods("ops-system").Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-shop-1",
			Namespace: "ops-system",
			Labels:    map[string]string{"applab.io/app": "shop"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "image:abc"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "app",
				Ready: true,
				Image: "image:abc",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod: %v", err)
	}

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/pods", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("pods: %d (%s)", rec.Code, rec.Body.String())
	}

	var result struct {
		Count int `json:"count"`
		Pods  []struct {
			Name  string `json:"name"`
			Ready bool   `json:"ready"`
			Image string `json:"image"`
		} `json:"pods"`
	}
	decodeData(t, rec, &result)

	if result.Count != 1 {
		t.Fatalf("count = %d, want 1", result.Count)
	}
	if result.Pods[0].Name != "app-shop-1" {
		t.Errorf("pod name = %q", result.Pods[0].Name)
	}
	if !result.Pods[0].Ready {
		t.Error("ready = false for a ready pod")
	}
}

// TestPodsEndpointIsEmptyForAnUndeployedApp asserts an app with no pods reports
// zero rather than failing, since that is a normal state.
func TestPodsEndpointIsEmptyForAnUndeployedApp(t *testing.T) {
	srv, _ := newObserveServer(t)
	h := srv.Handler()
	createAppForObserve(t, h, "shop")

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/pods", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("pods: %d (%s)", rec.Code, rec.Body.String())
	}

	var result struct {
		Count int `json:"count"`
	}
	decodeData(t, rec, &result)
	if result.Count != 0 {
		t.Errorf("count = %d, want 0", result.Count)
	}
}

// TestEventsEndpointCountsWarnings asserts the warnings count is reported, since
// "is anything wrong" is the question a caller is actually asking.
func TestEventsEndpointCountsWarnings(t *testing.T) {
	srv, client := newObserveServer(t)
	h := srv.Handler()
	createAppForObserve(t, h, "shop")

	ctx := context.Background()

	// Events are attributed to an app by the objects they concern, so the app
	// needs a pod for these to belong to. Without one they are treated as
	// somebody else's and filtered out — which is the behaviour the shared
	// namespace requires.
	if _, err := client.CoreV1().Pods("ops-system").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-shop-1",
			Namespace: "ops-system",
			Labels:    map[string]string{"applab.io/app": "shop"},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	events := []*corev1.Event{
		{
			ObjectMeta:     metav1.ObjectMeta{Name: "w1", Namespace: "ops-system"},
			Type:           corev1.EventTypeWarning,
			Reason:         "BackOff",
			Message:        "Back-off restarting failed container",
			LastTimestamp:  metav1.Now(),
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "app-shop-1"},
		},
		{
			ObjectMeta:     metav1.ObjectMeta{Name: "n1", Namespace: "ops-system"},
			Type:           corev1.EventTypeNormal,
			Reason:         "Pulled",
			Message:        "Successfully pulled image",
			LastTimestamp:  metav1.Now(),
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "app-shop-1"},
		},
	}
	for _, e := range events {
		if _, err := client.CoreV1().Events("ops-system").Create(ctx, e, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create event: %v", err)
		}
	}

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/events", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("events: %d (%s)", rec.Code, rec.Body.String())
	}

	var result struct {
		Count    int `json:"count"`
		Warnings int `json:"warnings"`
		Events   []struct {
			Type string `json:"type"`
		} `json:"events"`
	}
	decodeData(t, rec, &result)

	if result.Count != 2 {
		t.Fatalf("count = %d, want 2", result.Count)
	}
	if result.Warnings != 1 {
		t.Errorf("warnings = %d, want 1", result.Warnings)
	}
	if len(result.Events) > 0 && result.Events[0].Type != "Warning" {
		t.Errorf("first event type = %q, want the warning first", result.Events[0].Type)
	}
}

// TestDiagnoseExplainsAnUndeployedApp asserts the diagnosis answers without
// needing the cluster when the record already explains it.
func TestDiagnoseExplainsAnUndeployedApp(t *testing.T) {
	srv, _ := newObserveServer(t)
	h := srv.Handler()
	createAppForObserve(t, h, "shop")

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/diagnose", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("diagnose: %d (%s)", rec.Code, rec.Body.String())
	}

	var result struct {
		Problem string `json:"problem"`
		Next    string `json:"next"`
	}
	decodeData(t, rec, &result)

	if !strings.Contains(result.Problem, "nothing has been deployed") {
		t.Errorf("problem = %q, want it to say nothing is deployed", result.Problem)
	}
	// A diagnosis that does not say what to do is only half an answer.
	if result.Next == "" {
		t.Error("no next step was suggested")
	}
}

// TestDiagnoseReportsNoPods asserts a deployed app with no pods is diagnosed,
// with the events that explain it attached.
func TestDiagnoseReportsNoPods(t *testing.T) {
	srv, client := newObserveServer(t)
	h := srv.Handler()
	createAppForObserve(t, h, "shop")

	// Mark the app deployed so the diagnosis moves past the first check.
	createNamespaceForTest(t, client, "ops-system")
	markDeployed(t, srv, "shop")

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/diagnose", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("diagnose: %d (%s)", rec.Code, rec.Body.String())
	}

	var result struct {
		Problem string `json:"problem"`
	}
	decodeData(t, rec, &result)
	if !strings.Contains(result.Problem, "no pods") {
		t.Errorf("problem = %q, want it to say there are no pods", result.Problem)
	}
}

// TestDiagnoseReportsUnreadyPodsWithLogs asserts the useful case: pods exist but
// are not ready, and the answer carries the log that explains why.
func TestDiagnoseReportsUnreadyPodsWithLogs(t *testing.T) {
	srv, client := newObserveServer(t)
	h := srv.Handler()
	createAppForObserve(t, h, "shop")

	ctx := context.Background()
	createNamespaceForTest(t, client, "ops-system")
	markDeployed(t, srv, "shop")

	// A crash-looping pod: not ready, with the reason on the previous instance.
	if _, err := client.CoreV1().Pods("ops-system").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-shop-1",
			Namespace: "ops-system",
			Labels:    map[string]string{"applab.io/app": "shop"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "image:abc"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionFalse,
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "app",
				Ready:        false,
				RestartCount: 5,
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
				},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1},
				},
			}},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/diagnose", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("diagnose: %d (%s)", rec.Code, rec.Body.String())
	}

	var result struct {
		Problem string `json:"problem"`
		Pods    []struct {
			Reason string `json:"reason"`
		} `json:"pods"`
	}
	decodeData(t, rec, &result)

	if !strings.Contains(result.Problem, "not ready") {
		t.Errorf("problem = %q, want it to report unready pods", result.Problem)
	}
	if len(result.Pods) == 0 || result.Pods[0].Reason == "" {
		t.Error("the diagnosis does not carry the reason the pod is failing")
	}
}

// TestDiagnoseReportsHealthy asserts a working app says so rather than leaving
// the caller to infer it from an empty problem field.
func TestDiagnoseReportsHealthy(t *testing.T) {
	srv, client := newObserveServer(t)
	h := srv.Handler()
	createAppForObserve(t, h, "shop")

	ctx := context.Background()
	createNamespaceForTest(t, client, "ops-system")
	markDeployed(t, srv, "shop")

	if _, err := client.CoreV1().Pods("ops-system").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-shop-1",
			Namespace: "ops-system",
			Labels:    map[string]string{"applab.io/app": "shop"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "image:abc"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "app",
				Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/diagnose", nil)
	var result struct {
		Message string `json:"message"`
	}
	decodeData(t, rec, &result)

	if !strings.Contains(result.Message, "ready") {
		t.Errorf("message = %q, want it to confirm the app is ready", result.Message)
	}
}

// TestObservabilityRequiresAKey asserts the new routes are not accidentally open.
func TestObservabilityRequiresAKey(t *testing.T) {
	srv, _ := newObserveServer(t)
	h := srv.Handler()
	createAppForObserve(t, h, "shop")

	for _, path := range []string{
		"/api/v1/apps/shop/pods",
		"/api/v1/apps/shop/logs",
		"/api/v1/apps/shop/events",
		"/api/v1/apps/shop/diagnose",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s returned %d without a key, want 401", path, rec.Code)
		}
	}
}

// TestObservabilityWithoutClusterIs501 asserts a deployment with no cluster says
// so clearly rather than failing obscurely.
func TestObservabilityWithoutClusterIs501(t *testing.T) {
	srv, _ := newTestServer(t) // no observer attached
	h := srv.Handler()
	createAppForObserve(t, h, "shop")

	for _, path := range []string{
		"/api/v1/apps/shop/pods",
		"/api/v1/apps/shop/logs",
		"/api/v1/apps/shop/events",
		"/api/v1/apps/shop/diagnose",
		// The platform's own, which answer the same way: there is no cluster to
		// read, and that is a way to run AppLab rather than a fault.
		"/api/v1/platform/pods",
		"/api/v1/platform/logs",
	} {
		rec := doRequest(t, h, http.MethodGet, path, nil)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s returned %d, want %d", path, rec.Code, http.StatusNotImplemented)
		}
	}
}

// TestMetricsEndpointIsOpenAndWellFormed asserts /metrics is reachable without a
// key and is valid Prometheus text.
//
// Open on purpose — a scraper holds a credential awkwardly and these numbers
// describe the platform rather than any app — so the trade is asserted rather
// than left to drift.
func TestMetricsEndpointIsOpenAndWellFormed(t *testing.T) {
	srv, _ := newObserveServer(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("metrics returned %d without a key, want 200", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("content type = %q, want text/plain", rec.Header().Get("Content-Type"))
	}

	body := rec.Body.String()
	for _, want := range []string{
		"applab_requests_total",
		"applab_builds_started_total",
		"applab_apps",
		"applab_version{",
		"# TYPE",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output is missing %q", want)
		}
	}
}

// TestMetricsCountsRequests asserts the counters actually move, since a metric
// that is registered but never incremented looks the same as a quiet system.
func TestMetricsCountsRequests(t *testing.T) {
	srv, _ := newObserveServer(t)
	h := srv.Handler()
	createAppForObserve(t, h, "shop")

	// A request that succeeds and one that fails.
	doRequest(t, h, http.MethodGet, "/api/v1/apps/shop", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/nonexistent", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "applab_request_errors_total 1") {
		t.Errorf("the error counter did not record a 404:\n%s", firstLines(body, 40))
	}
	if !strings.Contains(body, "applab_apps_created_total 1") {
		t.Errorf("the created-apps counter did not record the app:\n%s", firstLines(body, 40))
	}
}

// TestMetricsCountsAuthRejections asserts refusals are counted, since a steady
// rate is the one signal that distinguishes probing from a misconfigured client.
func TestMetricsCountsAuthRejections(t *testing.T) {
	srv, _ := newObserveServer(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/apps", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "applab_auth_rejections_total 1") {
		t.Errorf("a refused request was not counted:\n%s", firstLines(rec.Body.String(), 30))
	}
}

// TestMetricsExcludesItself asserts scraping does not inflate the request
// counter, which would measure the scraping rather than the traffic.
func TestMetricsExcludesItself(t *testing.T) {
	srv, _ := newObserveServer(t)
	h := srv.Handler()

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "applab_requests_total 0") {
		t.Errorf("scraping /metrics counted itself as traffic:\n%s", firstLines(rec.Body.String(), 30))
	}
}

// TestPlatformPodsEndpointReportsAppLabsOwn is the endpoint's whole reason: it
// is how the console lists the deployment serving it, and it must not list an
// app's pods — the fake cluster holds both, in one namespace, as a real one does.
func TestPlatformPodsEndpointReportsAppLabsOwn(t *testing.T) {
	srv, client := newObserveServer(t)
	h := srv.Handler()
	createAppForObserve(t, h, "shop")

	ctx := context.Background()
	createNamespaceForTest(t, client, "ops-system")

	for _, pod := range []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "applab-6b9f7-abc",
				Namespace: "ops-system",
				Labels:    map[string]string{"app.kubernetes.io/part-of": "applab"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "applab", Image: "applab:abc"}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "app-shop-1",
				Namespace: "ops-system",
				Labels:    map[string]string{"applab.io/app": "shop"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "shop:abc"}}},
		},
	} {
		if _, err := client.CoreV1().Pods("ops-system").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create pod %s: %v", pod.Name, err)
		}
	}

	rec := doRequest(t, h, http.MethodGet, "/api/v1/platform/pods", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("platform pods: %d (%s)", rec.Code, rec.Body.String())
	}

	var result struct {
		Namespace string `json:"namespace"`
		Count     int    `json:"count"`
		Pods      []struct {
			Name string `json:"name"`
		} `json:"pods"`
	}
	decodeData(t, rec, &result)

	if result.Count != 1 {
		t.Fatalf("count = %d, want only the control plane's (%s)", result.Count, rec.Body.String())
	}
	if result.Pods[0].Name != "applab-6b9f7-abc" {
		t.Errorf("pod = %q, want applab's own", result.Pods[0].Name)
	}
	if result.Namespace != "ops-system" {
		t.Errorf("namespace = %q, want the one it looked in", result.Namespace)
	}
}

// TestPlatformLogsEndpointServesText asserts the shape the console reads: a
// text/plain body, so a client that can read an app's log can read this one with
// nothing new to learn.
func TestPlatformLogsEndpointServesText(t *testing.T) {
	srv, client := newObserveServer(t)
	h := srv.Handler()

	ctx := context.Background()
	createNamespaceForTest(t, client, "ops-system")
	if _, err := client.CoreV1().Pods("ops-system").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "applab-6b9f7-abc",
			Namespace: "ops-system",
			Labels:    map[string]string{"app.kubernetes.io/part-of": "applab"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "applab", Image: "applab:abc"}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	// follow=false, so this reads what exists and returns rather than streaming.
	rec := doRequest(t, h, http.MethodGet, "/api/v1/platform/logs?follow=false", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("platform logs: %d (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

// --- helpers ---------------------------------------------------------------

// createNamespaceForTest ensures a namespace exists in the fake cluster.
func createNamespaceForTest(t *testing.T, client *fake.Clientset, name string) {
	t.Helper()

	if _, err := client.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace %s: %v", name, err)
	}
}

// markDeployed gives an app a deployed commit so the diagnosis moves past its
// first check.
func markDeployed(t *testing.T, srv *api.Server, appID string) {
	t.Helper()
	srv.MarkDeployedForTest(appID, strings.Repeat("a", 40), "image:abc")
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
