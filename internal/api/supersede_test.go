package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/build"
	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/store"
)

// fakeBuildEngine is the build half of the pipeline, standing in for the cluster
// with a set of builds it holds itself. It exists because the behaviour under
// test is *which Jobs get deleted* and *what a listing shows*, and the real
// engine would try to do both against a cluster.
//
// It holds build.Result values rather than records of its own, because that is
// what the build half is now: there is no build record anywhere but the cluster,
// so a fake that kept its own would be testing a design the code no longer has.
type fakeBuildEngine struct {
	mu        sync.Mutex
	cancelled []string
	cancelErr error
	started   []string

	// builds is what a listing reads, keyed by build id. A test inserts what it
	// wants read back; startJob inserts what Start was asked for.
	builds map[string]build.Result
}

func (f *fakeBuildEngine) Ready() bool { return true }

func (f *fakeBuildEngine) Start(ctx context.Context, app *model.App, branch, buildID, commitSHA, appKey string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := "job-" + buildID
	f.started = append(f.started, name)
	f.put(build.Result{
		ID: buildID, AppID: app.ID, CommitSHA: commitSHA, Branch: branch,
		JobName: name, Status: model.BuildStatusPending, CreatedAt: time.Now(),
	})
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
	// A cancelled build's Job is gone, which is the whole of what a cancel does
	// now: there is no record to relabel, and the build stops being listed
	// because the thing being listed is gone.
	for id, b := range f.builds {
		if b.JobName == jobName {
			delete(f.builds, id)
		}
	}
	return nil
}

// put records a build. The caller holds the lock.
func (f *fakeBuildEngine) put(b build.Result) {
	if f.builds == nil {
		f.builds = map[string]build.Result{}
	}
	f.builds[b.ID] = b
}

// addBuild inserts a build a test wants read back.
func (f *fakeBuildEngine) addBuild(b build.Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.put(b)
}

func (f *fakeBuildEngine) List(ctx context.Context, namespace, appID string, limit int) ([]build.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.list(func(b build.Result) bool { return b.AppID == appID }, limit), nil
}

func (f *fakeBuildEngine) ListAll(ctx context.Context, namespace string, limit int) ([]build.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.list(func(build.Result) bool { return true }, limit), nil
}

func (f *fakeBuildEngine) Get(ctx context.Context, namespace, appID, buildID string) (*build.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.builds[buildID]
	if !ok || b.AppID != appID {
		return nil, build.ErrBuildNotFound
	}
	return &b, nil
}

func (f *fakeBuildEngine) FindSucceeded(ctx context.Context, namespace, appID, commitSHA string) (*build.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.list(func(b build.Result) bool {
		return b.AppID == appID && b.CommitSHA == commitSHA && b.Status == model.BuildStatusSucceeded
	}, 1) {
		return &b, nil
	}
	return nil, build.ErrBuildNotFound
}

func (f *fakeBuildEngine) Unfinished(ctx context.Context, namespace, appID string) ([]build.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.list(func(b build.Result) bool { return b.AppID == appID && !b.Status.Terminal() }, 0), nil
}

func (f *fakeBuildEngine) InFlight(ctx context.Context, namespace string) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for _, b := range f.builds {
		if !b.Status.Terminal() {
			out[b.AppID] = true
		}
	}
	return out, nil
}

// list selects and orders builds newest first. The caller holds the lock.
func (f *fakeBuildEngine) list(keep func(build.Result) bool, limit int) []build.Result {
	out := make([]build.Result, 0, len(f.builds))
	for _, b := range f.builds {
		if keep(b) {
			out = append(out, b)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
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

// unfinishedBuild adds a running build of an app to the engine, with a Job name.
//
// It is what a build that is still going looks like now: a Job in the cluster,
// not a row in a store. A test that wants one asks the engine to hold one.
func unfinishedBuild(t *testing.T, engine *fakeBuildEngine, appID, commit, jobName string) string {
	t.Helper()

	id, err := model.NewID()
	if err != nil {
		t.Fatalf("generate build id: %v", err)
	}

	engine.addBuild(build.Result{
		ID:        id,
		AppID:     appID,
		CommitSHA: commit,
		JobName:   jobName,
		Status:    model.BuildStatusRunning,
		CreatedAt: time.Now(),
	})
	return id
}

// TestAnUploadStopsTheBuildInFlight is the behaviour a push depends on: the
// build already running is building a commit that is no longer the tip, so the
// upload stops it rather than letting it run to completion and push an image
// nobody is waiting for.
func TestAnUploadStopsTheBuildInFlight(t *testing.T) {
	srv, engine, _ := newSupersedeServer(t)
	h := srv.Handler()

	sortAppWithCommit(t, srv, h, "shop")
	unfinishedBuild(t, engine, "shop", "aaaa", "job-in-flight")

	uploadSource(t, h, "shop", "second")

	if got := engine.cancelledJobs(); len(got) != 1 || got[0] != "job-in-flight" {
		t.Fatalf("cancelled jobs = %v, want [job-in-flight]", got)
	}
}

// TestAnUploadDoesNotStopAFinishedBuild guards the boundary: a build that has
// already finished is a fact, and stopping it would be stopping nothing — the
// Job it ran in is not running anything.
func TestAnUploadDoesNotStopAFinishedBuild(t *testing.T) {
	srv, engine, _ := newSupersedeServer(t)
	h := srv.Handler()

	sortAppWithCommit(t, srv, h, "shop")

	engine.addBuild(build.Result{
		ID: "done", AppID: "shop", CommitSHA: "aaaa", JobName: "job-done",
		Status: model.BuildStatusSucceeded, CreatedAt: time.Now(),
	})

	uploadSource(t, h, "shop", "second")

	if got := engine.cancelledJobs(); len(got) != 0 {
		t.Fatalf("cancelled jobs = %v, want none: the build had already finished", got)
	}
}

// TestAnUploadDoesNotStopAnotherAppsBuild is the scoping check. Deleting another
// app's Job would stop work that has nothing to do with this upload, and it is
// the failure a query that forgets its WHERE clause produces.
func TestAnUploadDoesNotStopAnotherAppsBuild(t *testing.T) {
	srv, engine, _ := newSupersedeServer(t)
	h := srv.Handler()

	sortAppWithCommit(t, srv, h, "shop")
	sortAppWithCommit(t, srv, h, "blog")
	unfinishedBuild(t, engine, "blog", "bbbb", "job-blog")

	uploadSource(t, h, "shop", "second")

	if got := engine.cancelledJobs(); len(got) != 0 {
		t.Fatalf("cancelled jobs = %v, want none: that build belongs to another app", got)
	}
}

// TestTheUploadSurvivesAFailedCancel asserts a cancel that fails does not fail
// the upload. The source is stored by the time this runs, and refusing the
// upload would trade a working commit for a tidier cluster.
func TestTheUploadSurvivesAFailedCancel(t *testing.T) {
	srv, engine, _ := newSupersedeServer(t)
	h := srv.Handler()

	sortAppWithCommit(t, srv, h, "shop")
	unfinishedBuild(t, engine, "shop", "aaaa", "job-stuck")
	engine.cancelErr = errCancelRefused

	rec := uploadSourceRecord(t, h, "shop", "second")
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestABuildWithNoJobIsNotCancelled covers a build whose Job name AppLab cannot
// name. Deleting the Job name of a build that has none would be a delete of ""
// against the cluster.
func TestABuildWithNoJobIsNotCancelled(t *testing.T) {
	srv, engine, _ := newSupersedeServer(t)
	h := srv.Handler()

	sortAppWithCommit(t, srv, h, "shop")
	unfinishedBuild(t, engine, "shop", "aaaa", "")

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
	srv, engine, _ := newSupersedeServer(t)
	h := srv.Handler()

	commit := sortAppWithCommit(t, srv, h, "shop")
	unfinishedBuild(t, engine, "shop", "aaaa", "job-older")

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/builds",
		map[string]any{"commit_sha": commit}); rec.Code != http.StatusAccepted {
		t.Fatalf("start build: %d (%s)", rec.Code, rec.Body.String())
	}

	got := engine.cancelledJobs()
	if len(got) != 1 || got[0] != "job-older" {
		t.Fatalf("cancelled jobs = %v, want [job-older]", got)
	}
	// The build that was just started must not be the one that was stopped.
	if len(engine.startedJobs()) == 0 {
		t.Fatal("no build was started")
	}
	for _, job := range got {
		for _, started := range engine.startedJobs() {
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
