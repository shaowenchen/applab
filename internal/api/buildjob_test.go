package api_test

import (
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
// derivable from anything AppLab stores — the build id is not in it, and the
// name is a function of the app and the id that nothing recomputes — so if the
// Job is not created with it, the build is unreachable the moment the response
// that started it has been sent. What a caller then gets is a build with no job
// to read a log from, for a build that is running perfectly well.
//
// That was shipped once, when the record and the Job were two writes that could
// disagree. They are one write now — the Job is the record — but the property is
// still worth asserting, because it is the property and not the mechanism that
// matters.
func TestABuildsJobNameIsRecorded(t *testing.T) {
	srv, engine, _ := newSupersedeServer(t)
	h := srv.Handler()

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

	// The engine was asked to start a Job, and that Job is the one the build must
	// be reachable by.
	started := engine.startedJobs()
	if len(started) != 1 {
		t.Fatalf("the engine started %d jobs, want 1", len(started))
	}

	// Read through the endpoint a caller would use, and assert the Job is named
	// in it — the field is what every later operation is addressed by.
	got := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/builds/"+buildID, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("read the build back: %d (%s)", got.Code, got.Body.String())
	}

	var result struct {
		JobName string `json:"job_name"`
	}
	decodeData(t, got, &result)
	if result.JobName == "" {
		t.Fatalf("the build has no job name, so nothing can reach it after it is started")
	}
	if result.JobName != started[0] {
		t.Errorf("the build reports job %q, but the engine started %q", result.JobName, started[0])
	}

	// And the log is readable through it, which is the surface the bug showed on.
	// follow=false, so the response is the log-so-far rather than a stream that
	// waits for a build the fake engine never actually runs.
	logs := doRequest(t, h, http.MethodGet,
		"/api/v1/apps/shop/builds/"+buildID+"/logs?follow=false", nil)
	if logs.Code != http.StatusOK {
		t.Fatalf("read the log: %d (%s)", logs.Code, logs.Body.String())
	}
	if strings.Contains(logs.Body.String(), "no live build job") {
		t.Errorf("a build that was just started reports no live job:\n%s", logs.Body.String())
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
	if strings.Contains(body, "no job to read a log from") {
		t.Errorf("a build that was just started has no job to read a log from:\n%s", body)
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
