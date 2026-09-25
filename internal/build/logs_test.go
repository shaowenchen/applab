package build

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Reading a build's log needs a cluster that can serve one.
//
// The fake clientset cannot: `GetLogs(...).Stream()` is an HTTP call to a
// subresource, and the fake returns an error rather than a body. So these run
// against a stub server behind a real client, which is what makes the request
// the engine builds — the container it names, whether it asks for the previous
// instance — something a check can see.

// engineWithLogs builds an Engine whose cluster is a stub serving the given pods
// and logs. Logs are keyed "<container>", or "<container>:previous" for the
// previous instance's output.
func engineWithLogs(t *testing.T, pods []corev1.Pod, logs map[string]string) *Engine {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			key := r.URL.Query().Get("container")
			if r.URL.Query().Get("previous") == "true" {
				key += ":previous"
			}
			text, ok := logs[key]
			if !ok {
				// A container with no log — one that never started, or whose
				// previous instance does not exist. What the API answers in that
				// case, so what the engine must survive.
				http.Error(w, "the container has no log", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, text)
			return
		}

		list := corev1.PodList{Items: pods}
		list.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	}))
	t.Cleanup(srv.Close)

	client, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("build a client for the test server: %v", err)
	}
	return New(client, testConfig())
}

// TestLogsReadsTheBuildContainer asserts the container that ran is the one read,
// under a heading naming it.
//
// The heading was once load-bearing in a way it no longer is: a build used to
// have two containers, a fetch that downloaded the source and a builder that
// consumed it, and a fetch failure concatenated with a builder's startup banner
// read as one stream nobody could interpret. Kaniko is one container, so the
// heading is usually a single line — kept because the list comes from the pod,
// and a pod with more than one container must still be readable.
func TestLogsReadsTheBuildContainer(t *testing.T) {
	pod := buildPod("build-job-abc", corev1.PodStatus{})

	engine := engineWithLogs(t, []corev1.Pod{*pod}, map[string]string{
		"build": "#1 building...\n#1 DONE\n",
	})

	out, err := engine.Logs(context.Background(), "applab-shop", "build-job", 100)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}

	for _, want := range []string{"=== build ===", "#1 DONE"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log is missing %q:\n%s", want, out)
		}
	}
}

// TestLogsFallsBackToThePreviousInstance asserts a crash loop is readable. A
// container being restarted has written nothing this time round, and its cause
// is on the instance that died — the same reason an app's pods are read this
// way in internal/observe.
func TestLogsFallsBackToThePreviousInstance(t *testing.T) {
	pod := buildPod("build-job-abc", corev1.PodStatus{})

	engine := engineWithLogs(t, []corev1.Pod{*pod}, map[string]string{
		"build":          "",
		"build:previous": "error: the app's key was rotated while this build was starting\n",
	})

	out, err := engine.Logs(context.Background(), "applab-shop", "build-job", 100)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !strings.Contains(out, "the app's key was rotated") {
		t.Errorf("the previous instance's output is missing:\n%s", out)
	}
	// Said out loud, so nobody reads it as output from the run they just
	// started — the timestamps are a different build's.
	if !strings.Contains(out, "previous instance") {
		t.Errorf("the fallback is not labelled:\n%s", out)
	}
}

// TestLogsKeepsTheReadableContainerWhenAnotherFails asserts one container's log
// being unavailable does not discard another's.
//
// Fetching a build's log is what someone does when the build broke, so a read
// that returns nothing because a *different* container had no log is the worst
// possible moment to give up. The pod is given an extra container here because
// that is what the code has to survive, not because a build declares one.
func TestLogsKeepsTheReadableContainerWhenAnotherFails(t *testing.T) {
	pod := buildPod("build-job-abc", corev1.PodStatus{})
	pod.Spec.InitContainers = []corev1.Container{{Name: "sidecar-of-the-past"}}

	// No "sidecar-of-the-past" key: the stub answers that container with an
	// error, which is what the API does for a container that never started.
	engine := engineWithLogs(t, []corev1.Pod{*pod}, map[string]string{
		"build": "cloning into /kaniko/buildcontext...\n",
	})

	out, err := engine.Logs(context.Background(), "applab-shop", "build-job", 100)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !strings.Contains(out, "cloning into /kaniko/buildcontext") {
		t.Errorf("the container that did have a log was dropped:\n%s", out)
	}
	if !strings.Contains(out, "could not read this container's log") {
		t.Errorf("the missing container is not reported:\n%s", out)
	}
}

// TestLogsReadsTheNewestPod asserts a retried Job's log is the attempt that ran
// last, since the earlier ones are the attempts that failed and a reader is
// looking for where it ended up.
func TestLogsReadsTheNewestPod(t *testing.T) {
	old := buildPod("build-job-old", corev1.PodStatus{})
	old.CreationTimestamp = metav1.NewTime(metav1.Now().Add(-60 * 1e9))
	fresh := buildPod("build-job-new", corev1.PodStatus{})

	engine := engineWithLogs(t, []corev1.Pod{*old, *fresh}, map[string]string{
		"build": "#1 building...\n",
	})

	out, err := engine.Logs(context.Background(), "applab-shop", "build-job", 100)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !strings.Contains(out, "#1 building") {
		t.Errorf("the newest pod's log was not read:\n%s", out)
	}
}
