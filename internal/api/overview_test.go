package api_test

import (
	"context"
	"net/http"
	"testing"

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

// TestOverviewCountsAppsByStatus asserts the counts are real counts.
func TestOverviewCountsAppsByStatus(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()

	// Two running, one failed, one merely created.
	for _, app := range []struct {
		id     string
		status model.AppStatus
	}{
		{"shop", model.AppStatusRunning},
		{"blog", model.AppStatusRunning},
		{"broken", model.AppStatusFailed},
		{"fresh", model.AppStatusCreated},
	} {
		if err := st.CreateApp(ctx, &model.App{
			ID: app.id, Port: 8080, Replicas: 1, Dockerfile: "Dockerfile",
			Status: app.status, Namespace: "ops-system",
		}); err != nil {
			t.Fatalf("create app %s: %v", app.id, err)
		}
	}

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
	// The number a person acts on: one app is in a state that needs looking at.
	if apps["needs_attention"] != float64(1) {
		t.Errorf("apps.needs_attention = %v, want 1", apps["needs_attention"])
	}
}

// TestOverviewExcludesDeletedApps asserts a deleted app is not counted.
//
// Its row survives so the id is not quietly reused, but reporting it would mean
// the count could never return to zero after an app was removed — which makes
// the number useless for exactly the question it exists to answer.
func TestOverviewExcludesDeletedApps(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()

	if err := st.CreateApp(ctx, &model.App{
		ID: "gone", Port: 8080, Replicas: 1, Dockerfile: "Dockerfile",
		Status: model.AppStatusDeleted, Namespace: "ops-system",
	}); err != nil {
		t.Fatalf("create deleted app: %v", err)
	}

	apps := section(t, overviewFor(t, srv), "apps")
	if apps["total"] != float64(0) {
		t.Errorf("apps.total = %v, want 0 — a deleted app is still a row, not an app", apps["total"])
	}
}

// TestOverviewCarriesRecentBuildsAcrossApps asserts the cross-app build list,
// which is the thing no existing endpoint can produce.
func TestOverviewCarriesRecentBuildsAcrossApps(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()

	for _, app := range []string{"shop", "blog"} {
		if err := st.CreateApp(ctx, &model.App{ID: app, Name: app}); err != nil {
			t.Fatalf("create app %s: %v", app, err)
		}
	}

	for _, b := range []struct {
		id, app, status string
	}{
		{"b1", "shop", string(model.BuildStatusSucceeded)},
		{"b2", "blog", string(model.BuildStatusFailed)},
		{"b3", "shop", string(model.BuildStatusSucceeded)},
	} {
		if err := st.CreateBuild(ctx, &model.Build{
			ID: b.id, AppID: b.app, CommitSHA: "0123456789012345678901234567890123456789",
			Status: model.BuildStatus(b.status), JobName: "job-" + b.id,
		}); err != nil {
			t.Fatalf("create build %s: %v", b.id, err)
		}
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

// TestCountAppsByStatusIgnoresDeleted covers the store query directly, since the
// endpoint could pass for the wrong reason if a deleted app happened to be the
// only row.
func TestCountAppsByStatusIgnoresDeleted(t *testing.T) {
	_, st := newTestServer(t)
	ctx := context.Background()

	for _, app := range []struct {
		id     string
		status model.AppStatus
	}{
		{"live", model.AppStatusRunning},
		{"dead", model.AppStatusDeleted},
	} {
		if err := st.CreateApp(ctx, &model.App{
			ID: app.id, Port: 8080, Replicas: 1, Dockerfile: "Dockerfile",
		}); err != nil {
			t.Fatalf("create app %s: %v", app.id, err)
		}
		if err := st.SetAppStatus(ctx, app.id, app.status, ""); err != nil {
			t.Fatalf("set %s status: %v", app.id, err)
		}
	}

	counts, err := st.CountAppsByStatus(ctx)
	if err != nil {
		t.Fatalf("count apps by status: %v", err)
	}
	if counts[model.AppStatusRunning] != 1 {
		t.Errorf("running = %d, want 1", counts[model.AppStatusRunning])
	}
	if _, present := counts[model.AppStatusDeleted]; present {
		t.Errorf("deleted apps were counted: %v", counts)
	}
}

// TestListRecentBuildsOrdersNewestFirst asserts the ordering a dashboard panel
// depends on.
func TestListRecentBuildsOrdersNewestFirst(t *testing.T) {
	_, st := newTestServer(t)
	ctx := context.Background()

	// The app has to exist: a build lives under its app's directory, so there is
	// nowhere to put a build for an app that is not there.
	if err := st.CreateApp(ctx, &model.App{ID: "shop", Name: "Shop"}); err != nil {
		t.Fatalf("create app: %v", err)
	}

	// Created in order, so created_at increases and the newest is last inserted.
	for _, id := range []string{"b1", "b2", "b3"} {
		if err := st.CreateBuild(ctx, &model.Build{
			ID: id, AppID: "shop", CommitSHA: "0123456789012345678901234567890123456789",
			Status: model.BuildStatusSucceeded, JobName: "job-" + id,
		}); err != nil {
			t.Fatalf("create build %s: %v", id, err)
		}
	}

	builds, err := st.ListRecentBuilds(ctx, 2)
	if err != nil {
		t.Fatalf("list recent builds: %v", err)
	}
	if len(builds) != 2 {
		t.Fatalf("got %d builds, want the 2 the limit asked for", len(builds))
	}
	// Newest first, and the limit caps the list rather than slicing it.
	if builds[0].CreatedAt.Before(builds[1].CreatedAt) {
		t.Errorf("builds are not newest-first: %v then %v", builds[0].CreatedAt, builds[1].CreatedAt)
	}
}

// TestListRecentBuildsSpansApps covers the one thing the per-app query cannot do.
func TestListRecentBuildsSpansApps(t *testing.T) {
	_, st := newTestServer(t)
	ctx := context.Background()

	for _, app := range []string{"shop", "blog"} {
		if err := st.CreateApp(ctx, &model.App{ID: app, Name: app}); err != nil {
			t.Fatalf("create app %s: %v", app, err)
		}
		if err := st.CreateBuild(ctx, &model.Build{
			ID: "build-" + app, AppID: app, CommitSHA: "0123456789012345678901234567890123456789",
			Status: model.BuildStatusSucceeded, JobName: "job-" + app,
		}); err != nil {
			t.Fatalf("create build for %s: %v", app, err)
		}
	}

	builds, err := st.ListRecentBuilds(ctx, 10)
	if err != nil {
		t.Fatalf("list recent builds: %v", err)
	}
	if len(builds) != 2 {
		t.Fatalf("got %d builds, want both apps' builds", len(builds))
	}
}

// TestStoreAggregatesOnEmptyDatabase asserts the queries answer rather than
// erroring when nothing has happened yet.
func TestStoreAggregatesOnEmptyDatabase(t *testing.T) {
	_, st := newTestServer(t)
	ctx := context.Background()

	apps, err := st.CountAppsByStatus(ctx)
	if err != nil {
		t.Fatalf("count apps: %v", err)
	}
	if len(apps) != 0 {
		t.Errorf("apps = %v, want an empty map", apps)
	}

	builds, err := st.ListRecentBuilds(ctx, 10)
	if err != nil {
		t.Fatalf("list builds: %v", err)
	}
	if len(builds) != 0 {
		t.Errorf("builds = %v, want none", builds)
	}
}
