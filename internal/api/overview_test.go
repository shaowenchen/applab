package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/shaowenchen/applab/internal/model"
)

// overviewFor decodes GET /api/v1/overview.
func overviewFor(t *testing.T, srv interface {
	Handler() http.Handler
}) map[string]any {
	t.Helper()

	rec := doRequest(t, srv.Handler(), http.MethodGet, "/api/v1/overview", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/overview = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var body map[string]any
	decodeData(t, rec, &body)
	return body
}

// section pulls a nested object out of the response, failing rather than
// returning nil so a missing key names itself.
func section(t *testing.T, body map[string]any, name string) map[string]any {
	t.Helper()

	raw, ok := body[name]
	if !ok {
		t.Fatalf("the overview has no %q section (body: %v)", name, body)
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%q is %T, want an object", name, raw)
	}
	return obj
}

// TestOverviewReportsEmptyDeployment asserts the endpoint answers sensibly on a
// fresh deployment, which is the state every install starts in.
//
// The zeroes matter as much as the counts: a dashboard rendering "0 running"
// from a missing key and from a key that says 0 look identical in a browser but
// only one of them survives a client that treats absent as an error.
func TestOverviewReportsEmptyDeployment(t *testing.T) {
	srv, _ := newTestServer(t)

	body := overviewFor(t, srv)

	apps := section(t, body, "apps")
	for _, key := range []string{"total", "running", "failed", "needs_attention"} {
		if _, present := apps[key]; !present {
			t.Errorf("apps.%s is absent; every status should be reported even at zero", key)
		}
	}
	if apps["total"] != float64(0) {
		t.Errorf("apps.total = %v, want 0 on a fresh deployment", apps["total"])
	}

	builds := section(t, body, "builds")
	recent, ok := builds["recent"].([]any)
	if !ok {
		t.Fatalf("builds.recent is %T, want an array — a null would force every client to branch before iterating", builds["recent"])
	}
	if len(recent) != 0 {
		t.Errorf("builds.recent has %d entries, want 0", len(recent))
	}
}

// TestOverviewCountsAppsByStatus asserts the counts are real counts, read from
// the cluster.
//
// The statuses are not written into the apps: they are not a field of an app any
// more. What makes an app "running" here is a Deployment the cluster reports as
// available, and what makes it "failed" is one whose rollout cannot progress.
func TestOverviewCountsAppsByStatus(t *testing.T) {
	srv, client, _ := newDeployServer(t)
	h := srv.Handler()

	for _, id := range []string{"shop", "blog", "broken", "fresh"} {
		if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": id}); rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d (%s)", id, rec.Code, rec.Body.String())
		}
	}

	// Two available, one that cannot progress, one with no Deployment at all.
	createDeployment(t, client, "shop", true)
	createDeployment(t, client, "blog", true)
	createDeployment(t, client, "broken", false)

	apps := section(t, overviewFor(t, srv), "apps")

	if apps["total"] != float64(4) {
		t.Errorf("apps.total = %v, want 4", apps["total"])
	}
	if apps["running"] != float64(2) {
		t.Errorf("apps.running = %v, want 2", apps["running"])
	}
	if apps["failed"] != float64(1) {
		t.Errorf("apps.failed = %v, want 1", apps["failed"])
	}
	if apps["created"] != float64(1) {
		t.Errorf("apps.created = %v, want 1 — an app with no Deployment is not running", apps["created"])
	}
	// The number a person acts on: one app is in a state that needs looking at.
	if apps["needs_attention"] != float64(1) {
		t.Errorf("apps.needs_attention = %v, want 1", apps["needs_attention"])
	}
}

// createDeployment gives an app the Deployment a deploy would create, so a test
// can place it in a state the cluster reports.
//
// A ready one is available; an unready one has a rollout that cannot progress,
// which is the condition the API reads as "failed" and the single most useful
// thing to surface when an app will not come up.
func createDeployment(t *testing.T, client *fake.Clientset, appID string, ready bool) {
	t.Helper()

	conditions := []appsv1.DeploymentCondition{
		{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
	}
	if !ready {
		conditions = []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
				Reason:  "ProgressDeadlineExceeded",
				Message: "ReplicaSet has timed out progressing"},
		}
	}

	_, err := client.AppsV1().Deployments("ops-system").Create(context.Background(),
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "applab-" + appID,
				Namespace: "ops-system",
				Labels:    map[string]string{"applab.io/app": appID},
				Annotations: map[string]string{
					"applab.io/commit": strings.Repeat("a", 40),
					"applab.io/image":  "image:abc",
				},
			},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"applab.io/app": appID}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"applab.io/app": appID}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "image:abc"}}},
				},
			},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 1, Conditions: conditions},
		}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create the deployment for %s: %v", appID, err)
	}
}

// TestOverviewCarriesRecentBuildsAcrossApps asserts the cross-app build list,
// which is the thing no existing endpoint can produce.
func TestOverviewCarriesRecentBuildsAcrossApps(t *testing.T) {
	srv, client, st, _ := newDeployServerWithDynamic(t)
	ctx := context.Background()

	for _, app := range []string{"shop", "blog"} {
		if err := st.CreateApp(ctx, &model.App{ID: app, Name: app, Namespace: "ops-system"}); err != nil {
			t.Fatalf("create app %s: %v", app, err)
		}
	}

	// Three builds across two apps, each a Job that has finished.
	for _, b := range []struct {
		id, app string
		success bool
	}{
		{"b1", "shop", true},
		{"b2", "blog", false},
		{"b3", "shop", true},
	} {
		makeFinishedBuildJob(t, client, b.app, b.id, b.success)
	}

	builds := section(t, overviewFor(t, srv), "builds")

	if builds["total"] != float64(3) {
		t.Errorf("builds.total = %v, want 3", builds["total"])
	}
	if builds["succeeded"] != float64(2) {
		t.Errorf("builds.succeeded = %v, want 2", builds["succeeded"])
	}
	if builds["failed"] != float64(1) {
		t.Errorf("builds.failed = %v, want 1", builds["failed"])
	}

	recent, _ := builds["recent"].([]any)
	if len(recent) != 3 {
		t.Fatalf("builds.recent has %d entries, want all 3", len(recent))
	}
	// Builds from more than one app, which is what makes this a platform view
	// rather than a per-app one.
	seen := map[string]bool{}
	for _, entry := range recent {
		row, _ := entry.(map[string]any)
		if app, _ := row["app_id"].(string); app != "" {
			seen[app] = true
		}
	}
	if !seen["shop"] || !seen["blog"] {
		t.Errorf("recent builds come from %v; both apps should appear", seen)
	}
}

// makeFinishedBuildJob creates the Job a finished build leaves behind.
//
// It goes through the engine's own Start, so the labels a listing selects on and
// the annotations it reads are exactly what the engine writes. A Job assembled
// here by hand would assert the reader against this test's idea of the scheme.
func makeFinishedBuildJob(t *testing.T, client *fake.Clientset, appID, buildID string, success bool) {
	t.Helper()

	ctx := context.Background()
	app := &model.App{ID: appID, Namespace: "ops-system", Port: 8080, Dockerfile: "Dockerfile"}

	engine := newRealBuildEngine(t, client, "ops-system")
	commit := strings.Repeat("a", 40)
	jobName, err := engine.Start(ctx, app, "main", buildID, commit, "test-app-key")
	if err != nil {
		t.Fatalf("start build job for %s: %v", buildID, err)
	}

	job, err := client.BatchV1().Jobs("ops-system").Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read back build job %s: %v", jobName, err)
	}
	if success {
		job.Status.Conditions = []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}
		job.Status.CompletionTime = &metav1.Time{Time: time.Now()}
	} else {
		job.Status.Conditions = []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
		}}
	}
	if _, err := client.BatchV1().Jobs("ops-system").UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("finish build job %s: %v", jobName, err)
	}
}

// TestOverviewReportsClusterState asserts the two cluster questions are
// answered separately.
//
// "Not configured" and "configured but unreachable" are different situations —
// a deployment with no cluster is a legitimate way to run AppLab — and
// collapsing them would make a working deployment look broken.
func TestOverviewReportsClusterState(t *testing.T) {
	t.Run("no cluster attached", func(t *testing.T) {
		srv, _ := newTestServer(t)

		cluster := section(t, overviewFor(t, srv), "cluster")
		if cluster["configured"] != false {
			t.Errorf("cluster.configured = %v, want false when no probe is attached", cluster["configured"])
		}
		if cluster["reachable"] != false {
			t.Errorf("cluster.reachable = %v, want false — an absent cluster is not a reachable one", cluster["reachable"])
		}
	})

	t.Run("cluster attached and answering", func(t *testing.T) {
		srv, _ := newTestServer(t)
		srv.WithClusterStatus(func(context.Context) bool { return true })

		cluster := section(t, overviewFor(t, srv), "cluster")
		if cluster["configured"] != true {
			t.Errorf("cluster.configured = %v, want true", cluster["configured"])
		}
		if cluster["reachable"] != true {
			t.Errorf("cluster.reachable = %v, want true", cluster["reachable"])
		}
	})

	t.Run("cluster attached but unreachable", func(t *testing.T) {
		srv, _ := newTestServer(t)
		srv.WithClusterStatus(func(context.Context) bool { return false })

		cluster := section(t, overviewFor(t, srv), "cluster")
		// This is the combination worth telling apart from "not configured":
		// the deployment believes it has a cluster and cannot reach it.
		if cluster["configured"] != true {
			t.Errorf("cluster.configured = %v, want true", cluster["configured"])
		}
		if cluster["reachable"] != false {
			t.Errorf("cluster.reachable = %v, want false", cluster["reachable"])
		}
	})
}

// TestOverviewCarriesDeploymentDescription asserts the overview reports the same
// self-description /api/v1/config does, so a dashboard needs one call and the
// two cannot disagree.
func TestOverviewCarriesDeploymentDescription(t *testing.T) {
	srv, _ := newTestServer(t)

	deployment := section(t, overviewFor(t, srv), "deployment")

	if deployment["base_domain"] != "apps.example.com" {
		t.Errorf("deployment.base_domain = %v, want the configured domain", deployment["base_domain"])
	}
	if deployment["namespace"] != "ops-system" {
		t.Errorf("deployment.namespace = %v, want ops-system", deployment["namespace"])
	}
	if deployment["domain_template"] != "*.apps.example.com" {
		t.Errorf("deployment.domain_template = %v, want the per-app form", deployment["domain_template"])
	}

	// The same values the config endpoint reports, fetched independently: if
	// these ever differ, one of the two is lying to a client.
	rec := doRequest(t, srv.Handler(), http.MethodGet, "/api/v1/config", nil)
	var config map[string]any
	decodeData(t, rec, &config)

	for _, key := range []string{"api_version", "version", "base_domain", "namespace", "domain_template"} {
		if deployment[key] != config[key] {
			t.Errorf("overview deployment.%s = %v but /config says %v; they must not disagree",
				key, deployment[key], config[key])
		}
	}
}

// TestOverviewRequiresAKey asserts the overview is not an unauthenticated view
// of every app's counts.
func TestOverviewRequiresAKey(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := doRequestNoKey(t, srv.Handler(), http.MethodGet, "/api/v1/overview")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/overview without a key = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestOverviewAnswersWithNoBuildHalf asserts a deployment that cannot build
// reports an empty history rather than erroring.
//
// That is a legitimate way to run AppLab — the API and the source half work
// without a cluster — and a dashboard rendering "0 builds" is the right answer,
// not a failure.
func TestOverviewAnswersWithNoBuildHalf(t *testing.T) {
	srv, _ := newTestServer(t)

	builds := section(t, overviewFor(t, srv), "builds")
	if builds["total"] != float64(0) {
		t.Errorf("builds.total = %v, want 0", builds["total"])
	}
	recent, ok := builds["recent"].([]any)
	if !ok {
		t.Fatalf("builds.recent is %T, want an array", builds["recent"])
	}
	if len(recent) != 0 {
		t.Errorf("builds.recent has %d entries, want none", len(recent))
	}
}
