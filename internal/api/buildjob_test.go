package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestABuildsJobNameIsRecorded is the check that a started build can be reached
// afterwards.
//
// The Job's name is what every build operation after the start is addressed by:
// the log, the status, stopping it, and the pods it left behind. It is not
// derivable from the stored record — the record holds the app and the build id,
// and the name is a function of those, but nothing recomputes it — so if it is
// not written down the build is unreachable the moment the response that started
// it has been sent. What a caller then gets is "[AppLab] no live build job for
// this build" and "status: pending", for a build that is running perfectly well.
//
// That was shipped, and the reason the tests did not catch it is worth stating:
// every existing test that needed a Job name set one on the build it constructed
// itself, which asserts that the *readers* work and says nothing about whether
// the writer ever writes. This test builds the state the way the server does —
// by starting a build — and reads it back from the store, so the only thing it
// can be observing is what startBuild recorded.
func TestABuildsJobNameIsRecorded(t *testing.T) {
	srv, engine, st := newSupersedeServer(t)
	h := srv.Handler()
	ctx := context.Background()

	commit := setupAppWithCommit(t, srv, h, "shop")

	// The real route, so what runs is startBuild and not a reconstruction of it.
	// 202: a build is started, not completed. The status is asserted rather than
	// the body, so a change to the response shape does not silently skip the
	// assertions below.
	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/builds",
		map[string]any{"commit_sha": commit})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start a build: %d (%s)", rec.Code, rec.Body.String())
	}

	buildID := buildIDFrom(t, rec)

	// The engine was asked to start a Job, and that Job is the one the record
	// must name.
	started := engine.startedJobs()
	if len(started) != 1 {
		t.Fatalf("the engine started %d jobs, want 1", len(started))
	}

	// Read back from the store rather than from the response: the response is
	// built from the record the handler already holds in memory, so a name that
	// was never persisted would still appear in it. That is exactly the shape of
	// the bug this is guarding against.
	build, err := st.GetBuild(ctx, "shop", buildID)
	if err != nil {
		t.Fatalf("read the build back: %v", err)
	}
	if build.JobName == "" {
		t.Fatalf("the build was stored with no job name, so nothing can reach it after it is started; "+
			"the log endpoint answers %q instead",
			"[AppLab] no live build job for this build")
	}
	if build.JobName != started[0] {
		t.Errorf("the stored job name is %q, but the engine started %q", build.JobName, started[0])
	}
}

// TestAStartedBuildsLogIsReadable is the same property through the surface a
// person actually meets: the log of a build that was just started.
//
// Separate from the check above because it is the endpoint that reported the
// problem, and an assertion about a stored field would keep passing if the
// handler stopped consulting it. This drives the API end to end.
func TestAStartedBuildsLogIsReadable(t *testing.T) {
	srv, _, _ := newSupersedeServer(t)
	h := srv.Handler()

	commit := setupAppWithCommit(t, srv, h, "shop")

	// 202: a build is started, not completed. The status is asserted rather than
	// the body, so a change to the response shape does not silently skip the
	// assertions below.
	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/builds",
		map[string]any{"commit_sha": commit})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start a build: %d (%s)", rec.Code, rec.Body.String())
	}
	buildID := buildIDFrom(t, rec)

	// follow=false, so the response is the log-so-far rather than a stream that
	// waits for a build the fake engine never actually runs.
	logs := doRequest(t, h, http.MethodGet,
		"/api/v1/apps/shop/builds/"+buildID+"/logs?follow=false", nil)
	if logs.Code != http.StatusOK {
		t.Fatalf("read the log: %d (%s)", logs.Code, logs.Body.String())
	}

	body := logs.Body.String()
	if strings.Contains(body, "no live build job") {
		t.Errorf("a build that was just started reports no live job:\n%s", body)
	}
	if strings.Contains(body, "status: pending") && !strings.Contains(body, "[AppLab] build") {
		t.Errorf("the build reads as pending rather than as running:\n%s", body)
	}
}

// buildIDFrom pulls the build id out of a start-build response, failing the test
// rather than returning an empty string that would make every later assertion
// vacuous.
func buildIDFrom(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	var result struct {
		ID string `json:"id"`
	}
	decodeData(t, rec, &result)
	if result.ID == "" {
		t.Fatalf("the start-build response carries no build id: %s", rec.Body.String())
	}
	return result.ID
}

// startedJobs reports the Job names the fake engine was asked to start.
func (f *fakeBuildEngine) startedJobs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...)
}
