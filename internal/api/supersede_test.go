package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/store"
)

// fakeBuildEngine is the build half of the pipeline, recording what was asked of
// it. It exists because the behaviour under test is *which Jobs get deleted*,
// and the real engine would try to delete them from a cluster.
type fakeBuildEngine struct {
	mu        sync.Mutex
	cancelled []string
	cancelErr error
	started   []string
}

func (f *fakeBuildEngine) Ready() bool { return true }

func (f *fakeBuildEngine) Start(ctx context.Context, app *model.App, branch, buildID, commitSHA, appKey string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := "job-" + buildID
	f.started = append(f.started, name)
	return name, nil
}

func (f *fakeBuildEngine) Status(ctx context.Context, namespace, jobName string) (model.BuildStatus, string, error) {
	return "", "", nil
}

func (f *fakeBuildEngine) Logs(ctx context.Context, namespace, jobName string, tailLines int64) (string, error) {
	return "", nil
}

func (f *fakeBuildEngine) ImageFor(appID, commitSHA string) string {
	return "registry.example.com/" + appID + ":" + commitSHA
}

func (f *fakeBuildEngine) Cancel(ctx context.Context, namespace, jobName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.cancelled = append(f.cancelled, jobName)
	return nil
}

func (f *fakeBuildEngine) cancelledJobs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cancelled...)
}

// newSupersedeServer returns a server with a build engine attached, plus the
// engine itself so a test can read what it was asked to stop.
func newSupersedeServer(t *testing.T) (*api.Server, *fakeBuildEngine, *store.Store) {
	t.Helper()

	srv, _, st := newDeployServerWithSource(t)
	engine := &fakeBuildEngine{}
	srv.WithBuild(engine)
	return srv, engine, st
}

// unfinishedBuild writes a build row that is still pending, with a Job name, the
// way startBuild records one.
func unfinishedBuild(t *testing.T, st *store.Store, appID, commit, jobName string) string {
	t.Helper()

	ctx := context.Background()
	id, err := model.NewID()
	if err != nil {
		t.Fatalf("generate build id: %v", err)
	}

	build := &model.Build{
		ID:        id,
		AppID:     appID,
		CommitSHA: commit,
		JobName:   jobName,
		Status:    model.BuildStatusPending,
	}
	if err := st.CreateBuild(ctx, build); err != nil {
		t.Fatalf("create build: %v", err)
	}
	return id
}

// TestAnUploadStopsTheBuildInFlight is the behaviour a push depends on: the
// build already running is building a commit that is no longer the tip, so the
// upload stops it rather than letting it run to completion and push an image
// nobody is waiting for.
func TestAnUploadStopsTheBuildInFlight(t *testing.T) {
	srv, engine, st := newSupersedeServer(t)
	h := srv.Handler()

	sortAppWithCommit(t, srv, h, "shop")
	buildID := unfinishedBuild(t, st, "shop", "aaaa", "job-in-flight")

	uploadSource(t, h, "shop", "second")

	if got := engine.cancelledJobs(); len(got) != 1 || got[0] != "job-in-flight" {
		t.Fatalf("cancelled jobs = %v, want [job-in-flight]", got)
	}

	build, err := st.GetBuild(context.Background(), "shop", buildID)
	if err != nil {
		t.Fatalf("read the build: %v", err)
	}
	if build.Status != model.BuildStatusCancelled {
		t.Errorf("build status = %q, want %q", build.Status, model.BuildStatusCancelled)
	}
	if !strings.Contains(build.Reason, "superseded") {
		t.Errorf("the reason should say why it stopped: %q", build.Reason)
	}
}

// TestAnUploadDoesNotStopAFinishedBuild guards the boundary: a build that has
// already finished is a fact, and relabelling it would erase the record of what
// was actually built.
func TestAnUploadDoesNotStopAFinishedBuild(t *testing.T) {
	srv, engine, st := newSupersedeServer(t)
	h := srv.Handler()

	sortAppWithCommit(t, srv, h, "shop")

	buildID := unfinishedBuild(t, st, "shop", "aaaa", "job-done")
	if err := st.SetBuildStatus(context.Background(), "shop", buildID, model.BuildStatusSucceeded, ""); err != nil {
		t.Fatalf("finish the build: %v", err)
	}

	uploadSource(t, h, "shop", "second")

	if got := engine.cancelledJobs(); len(got) != 0 {
		t.Fatalf("cancelled jobs = %v, want none: the build had already finished", got)
	}
	build, err := st.GetBuild(context.Background(), "shop", buildID)
	if err != nil {
		t.Fatalf("read the build: %v", err)
	}
	if build.Status != model.BuildStatusSucceeded {
		t.Errorf("build status = %q, want it left as %q", build.Status, model.BuildStatusSucceeded)
	}
}

// TestAnUploadDoesNotStopAnotherAppsBuild is the scoping check. Deleting another
// app's Job would stop work that has nothing to do with this upload, and it is
// the failure a query that forgets its WHERE clause produces.
func TestAnUploadDoesNotStopAnotherAppsBuild(t *testing.T) {
	srv, engine, st := newSupersedeServer(t)
	h := srv.Handler()

	sortAppWithCommit(t, srv, h, "shop")
	sortAppWithCommit(t, srv, h, "blog")
	unfinishedBuild(t, st, "blog", "bbbb", "job-blog")

	uploadSource(t, h, "shop", "second")

	if got := engine.cancelledJobs(); len(got) != 0 {
		t.Fatalf("cancelled jobs = %v, want none: that build belongs to another app", got)
	}
}

// TestTheUploadSurvivesAFailedCancel asserts a cancel that fails does not fail
// the upload. The source is stored by the time this runs, and refusing the
// upload would trade a working commit for a tidier cluster.
func TestTheUploadSurvivesAFailedCancel(t *testing.T) {
	srv, engine, st := newSupersedeServer(t)
	h := srv.Handler()

	sortAppWithCommit(t, srv, h, "shop")
	unfinishedBuild(t, st, "shop", "aaaa", "job-stuck")
	engine.cancelErr = errCancelRefused

	rec := uploadSourceRecord(t, h, "shop", "second")
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestABuildWithNoJobIsNotCancelled covers the window startBuild opens by
// writing the record before creating the Job. Deleting the Job name of a build
// that never got one would be a delete of "" against the cluster.
func TestABuildWithNoJobIsNotCancelled(t *testing.T) {
	srv, engine, st := newSupersedeServer(t)
	h := srv.Handler()

	sortAppWithCommit(t, srv, h, "shop")
	unfinishedBuild(t, st, "shop", "aaaa", "")

	uploadSource(t, h, "shop", "second")

	for _, job := range engine.cancelledJobs() {
		if job == "" {
			t.Fatal("a build with no Job name was cancelled; that would delete an empty name from the cluster")
		}
	}
}

// TestAStartedBuildSupersedesWhatWasRunning is the second half of the invariant:
// stopping on upload covers the common path, and stopping when a build starts
// covers every other way one can begin — a second POST, a deploy with
// build:true, a rollback that has to rebuild. Two builds racing to push the same
// image tag is what this prevents.
func TestAStartedBuildSupersedesWhatWasRunning(t *testing.T) {
	srv, engine, st := newSupersedeServer(t)
	h := srv.Handler()

	commit := sortAppWithCommit(t, srv, h, "shop")
	unfinishedBuild(t, st, "shop", "aaaa", "job-older")

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/builds",
		map[string]any{"commit_sha": commit}); rec.Code != http.StatusAccepted {
		t.Fatalf("start build: %d (%s)", rec.Code, rec.Body.String())
	}

	got := engine.cancelledJobs()
	if len(got) != 1 || got[0] != "job-older" {
		t.Fatalf("cancelled jobs = %v, want [job-older]", got)
	}
	// The build that was just started must not be the one that was stopped.
	if len(engine.started) == 0 {
		t.Fatal("no build was started")
	}
	for _, job := range got {
		for _, started := range engine.started {
			if job == started {
				t.Fatalf("the build just started (%s) was cancelled by its own start", job)
			}
		}
	}
}

// --- helpers ---------------------------------------------------------------

var errCancelRefused = errCancel("the cluster refused")

type errCancel string

func (e errCancel) Error() string { return string(e) }

// sortAppWithCommit is setupAppWithCommit with the app created first, so a test
// can call it for two apps.
func sortAppWithCommit(t *testing.T, srv *api.Server, h http.Handler, appID string) string {
	t.Helper()
	return setupAppWithCommit(t, srv, h, appID)
}

// uploadSource uploads a source tree and fails the test if it is refused.
func uploadSource(t *testing.T, h http.Handler, appID, message string) {
	t.Helper()
	rec := uploadSourceRecord(t, h, appID, message)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload source: %d (%s)", rec.Code, rec.Body.String())
	}
}

// uploadSourceRecord performs the upload and returns the recorder, for a test
// that wants to assert on the response rather than require a 200.
func uploadSourceRecord(t *testing.T, h http.Handler, appID, message string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/apps/"+appID+"/source?message="+message,
		strings.NewReader(string(tarFiles(t, map[string]string{"Dockerfile": "FROM scratch\n"}))))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
