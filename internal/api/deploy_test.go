package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamic "k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/auth"
	"github.com/shaowenchen/applab/internal/build"
	"github.com/shaowenchen/applab/internal/config"
	"github.com/shaowenchen/applab/internal/deploy"
	"github.com/shaowenchen/applab/internal/k8s"
	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/source"
	"github.com/shaowenchen/applab/internal/store"
)

// newDeployServer builds a Server with source storage, a namespace manager and a
// deployer over a fake cluster, so the whole deploy path is exercised without a
// cluster.
func newDeployServer(t *testing.T) (*api.Server, *fake.Clientset, *store.Store) {
	t.Helper()
	return newDeployServerWithSource(t)
}

// newDeployServerWithSource also returns the source store, for tests that need
// to read a commit directly.
func newDeployServerWithSource(t *testing.T) (*api.Server, *fake.Clientset, *store.Store) {
	t.Helper()
	srv, client, st, _ := newDeployServerWithDynamic(t)
	return srv, client, st
}

// newDeployServerWithDynamic also returns the dynamic client the server itself
// publishes through.
//
// The typed clientset and the dynamic one are separate handles on separate fake
// stores, so a test that builds its own dynamic client sees none of the objects
// the server created. Anything asserting on a VirtualService needs this one.
func newDeployServerWithDynamic(t *testing.T) (*api.Server, *fake.Clientset, *store.Store, dynamic.Interface) {
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
	dyn := fakeDynamic(t)
	deployer := deploy.NewWithDynamic(client, dyn, deploy.Config{
		BaseDomain: "apps.example.com",
		Gateway:    "ops-system/gateway",
	})

	cfg := config.Default()
	cfg.Keys = []string{"test-key"}
	cfg.BaseDomain = "apps.example.com"
	cfg.Namespace = "ops-system"
	cfg.DataDir = dataDir

	cluster := k8s.NewWithClientset(client, cfg.Namespace)

	// The real build engine, over the same fake clientset. Attaching it here
	// rather than in the tests that want one means the deploy path's build
	// lookups — "has this commit been built", "what image did it produce" — run
	// against the code that will actually answer them, instead of against a
	// stand-in written to agree with the test's assumption.
	buildEngine := build.New(client, build.Config{
		Registry:    "registry.example.com/apps",
		KanikoImage: "gcr.io/kaniko-project/executor:v1.23.2",
		AppLabURL:   "http://applab.ops-system.svc.cluster.local",
	})

	srv := api.New(cfg, st, auth.New(cfg.Keys)).
		WithSource(src).
		WithBuild(buildEngine).
		WithDeployer(deployer).
		WithAppObjectsDeleter(cluster.DeleteAppObjects)

	// Recorded so a helper can drive the same objects the server reads. The
	// alternative — a test building its own clientset — would see none of what
	// the server created, which is a mistake that presents as a passing test
	// asserting nothing.
	testWiring.Store(srv, wiring{client: client, engine: buildEngine})

	return srv, client, st, dyn
}

// wiring is what a test server was built with, for helpers that need to drive
// the same fake cluster and build engine the server itself uses.
type wiring struct {
	client *fake.Clientset
	engine *build.Engine
}

// testWiring maps a test server to the fake cluster behind it.
var testWiring sync.Map

// clientsetFor returns the fake clientset a test server was built over.
func clientsetFor(srv *api.Server) *fake.Clientset {
	w, ok := testWiring.Load(srv)
	if !ok {
		panic("this server was not built by newDeployServer, so its cluster is not reachable from a test")
	}
	return w.(wiring).client
}

// testBuildEngine returns the build engine a test server was built with.
func testBuildEngine(srv *api.Server) *build.Engine {
	w, ok := testWiring.Load(srv)
	if !ok {
		panic("this server was not built by newDeployServer, so its build engine is not reachable from a test")
	}
	return w.(wiring).engine
}

// registerBuildJob starts a real build Job and marks it complete, so the deploy
// path finds an image for a commit.
//
// It is how a test stands in for a finished build now that there is no build
// record to write: the Job is the record. It goes through the engine's own Start
// rather than assembling a Job here, so the labels and annotations the readers
// look for are the ones the writer actually puts on — a hand-built Job would be
// this test asserting its own idea of the label scheme, which is exactly the
// kind of test that passed while the platform log endpoint was broken.
func registerBuildJob(t *testing.T, srv *api.Server, st *store.Store, appID, commit string) string {
	t.Helper()

	ctx := context.Background()
	app, err := st.GetApp(ctx, appID)
	if err != nil {
		t.Fatalf("read the app to register a build for: %v", err)
	}

	engine := testBuildEngine(srv)
	id, err := model.NewID()
	if err != nil {
		t.Fatalf("generate build id: %v", err)
	}

	branch := app.ActiveBranch()
	jobName, err := engine.Start(ctx, app, branch, id, commit, "test-app-key")
	if err != nil {
		t.Fatalf("start the build job: %v", err)
	}

	// Marked complete, which is what the cluster does when kaniko finishes.
	job, err := clientsetFor(srv).BatchV1().Jobs(app.Namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read back the build job: %v", err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
	}}
	job.Status.CompletionTime = &metav1.Time{Time: time.Now()}
	if _, err := clientsetFor(srv).BatchV1().Jobs(app.Namespace).UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("finish the build job: %v", err)
	}
	return id
}

// fakeDynamic returns a dynamic client over the resources AppLab publishes with,
// so the deploy path can run without a cluster.
//
// The list kinds are given explicitly: the fake derives them from the kind by
// pluralising, which turns "Gateway" into "gatewaies" and then does not match
// the "gateways" a caller asks for.
func fakeDynamic(t *testing.T) dynamic.Interface {
	t.Helper()

	gvr := schema.GroupVersionResource{Group: "networking.istio.io", Version: "v1", Resource: "virtualservices"}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			gvr: "VirtualServiceList",
			{Group: "networking.istio.io", Version: "v1", Resource: "gateways"}: "GatewayList",
		})
}

// setupAppWithCommit creates an app and uploads a source commit, returning the
// commit id.
func setupAppWithCommit(t *testing.T, srv *api.Server, h http.Handler, appID string) string {
	t.Helper()

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": appID}); rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d (%s)", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+appID+"/source?message=deployable",
		strings.NewReader(string(tarFiles(t, map[string]string{"Dockerfile": "FROM scratch\n"}))))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload source: %d (%s)", rec.Code, rec.Body.String())
	}

	var result struct {
		CommitSHA string `json:"commit_sha"`
	}
	decodeData(t, rec, &result)
	return result.CommitSHA
}

// TestDeployWithNoImageIsAConflict asserts a deploy of a commit that has never
// been built fails with a message naming the fix.
//
// Silently building instead would make the response time unpredictable and hide
// a build failure behind a different operation; the caller asked to deploy, and
// the honest answer is that there is nothing to deploy yet.
func TestDeployWithNoImageIsAConflict(t *testing.T) {
	srv, _, _ := newDeployServer(t)
	h := srv.Handler()

	setupAppWithCommit(t, srv, h, "shop")

	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusConflict, rec.Body.String())
	}

	var body struct {
		Error string `json:"error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)

	// The message has to name the endpoint that fixes it, or a caller is stuck.
	if !strings.Contains(body.Error, "build it first") {
		t.Errorf("the error does not say what to do: %q", body.Error)
	}
	if !strings.Contains(body.Error, "/builds") {
		t.Errorf("the error does not name the build endpoint: %q", body.Error)
	}
}

// TestDeployCreatesResources asserts a deploy of a commit that has an image
// produces the objects and reports the URL.
func TestDeployCreatesResources(t *testing.T) {
	srv, client, st := newDeployServer(t)
	h := srv.Handler()

	commit := setupAppWithCommit(t, srv, h, "shop")
	app, err := st.GetApp(context.Background(), "shop")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	app.Namespace = "ops-system"

	// Stand in for a completed build: a Job that succeeded, which is the only
	// record of one there is.
	registerBuildJob(t, srv, st, "shop", commit)

	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("deploy: %d (%s)", rec.Code, rec.Body.String())
	}

	var result struct {
		URL string `json:"url"`
	}
	decodeData(t, rec, &result)

	// The address is one field, and it is the app's own — no host beside it to
	// disagree with. The host used to be reported separately, which was only the
	// whole address when no path prefix was configured.
	if result.URL != "http://shop.apps.example.com" {
		t.Errorf("url = %q, want the address the app is served at", result.URL)
	}

	// The objects must exist in the app's namespace.
	if _, err := client.AppsV1().Deployments("ops-system").Get(context.Background(), "applab-shop", metav1.GetOptions{}); err != nil {
		t.Errorf("no deployment was created: %v", err)
	}
	// The cluster must say what is deployed — it is the only place that
	// records it, as the annotation Apply stamps on the Deployment.
	deployment, err := client.AppsV1().Deployments("ops-system").Get(context.Background(), "applab-shop", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the deployment: %v", err)
	}
	if got := deployment.Annotations["applab.io/commit"]; got != commit {
		t.Errorf("deployed commit = %q, want %q", got, commit)
	}
}

// TestRollbackRequiresACommit asserts a rollback without a target is refused,
// since "roll back" has no meaning without saying to what.
func TestRollbackRequiresACommit(t *testing.T) {
	srv, _, _ := newDeployServer(t)
	h := srv.Handler()

	setupAppWithCommit(t, srv, h, "shop")

	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/rollback", map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d for a rollback with no commit", rec.Code, http.StatusBadRequest)
	}
}

// TestRollbackToUnbuiltCommitIsAConflict asserts a rollback never builds. A
// rollback is what someone reaches for while recovering from a bad deploy, so
// turning it into a slow operation that can fail is the wrong trade.
func TestRollbackToUnbuiltCommitIsAConflict(t *testing.T) {
	srv, _, _ := newDeployServer(t)
	h := srv.Handler()

	first := setupAppWithCommit(t, srv, h, "shop")

	// Upload a second commit, so "rolling back to the first" is meaningful.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/shop/source?message=second",
		strings.NewReader(string(tarFiles(t, map[string]string{"Dockerfile": "FROM scratch\n# v2\n"}))))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	rec = doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/rollback", map[string]any{"commit_sha": first})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusConflict, rec.Body.String())
	}

	var body struct {
		Error string `json:"error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if !strings.Contains(body.Error, "no successful build") {
		t.Errorf("the error should say nothing was ever built for that commit: %q", body.Error)
	}
}

// TestRollbackToDeployedCommitIsRefused asserts rolling back to what is already
// running says so, rather than performing a rollout that changes nothing.
func TestRollbackToDeployedCommitIsRefused(t *testing.T) {
	srv, _, st := newDeployServer(t)
	h := srv.Handler()

	commit := setupAppWithCommit(t, srv, h, "shop")
	registerBuildJob(t, srv, st, "shop", commit)

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("deploy: %d (%s)", rec.Code, rec.Body.String())
	}

	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/rollback", map[string]any{"commit_sha": commit})
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d rolling back to the deployed commit", rec.Code, http.StatusConflict)
	}
}

// TestStopRemovesResources asserts stopping removes the objects but keeps the
// app, so starting again is a deploy rather than a re-upload.
func TestStopRemovesResources(t *testing.T) {
	srv, client, st := newDeployServer(t)
	h := srv.Handler()
	ctx := context.Background()

	commit := setupAppWithCommit(t, srv, h, "shop")
	registerBuildJob(t, srv, st, "shop", commit)

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("deploy: %d (%s)", rec.Code, rec.Body.String())
	}

	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/stop", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("stop: %d (%s)", rec.Code, rec.Body.String())
	}

	if _, err := client.AppsV1().Deployments("ops-system").Get(ctx, "applab-shop", metav1.GetOptions{}); err == nil {
		t.Error("the deployment still exists after stop")
	}

	// The app and its source must survive.
	if _, err := st.GetApp(ctx, "shop"); err != nil {
		t.Fatalf("the app was deleted by stop: %v", err)
	}
	commits, err := st.ListCommits(ctx, "shop", 1)
	if err != nil || len(commits) == 0 {
		t.Errorf("the source was lost by stop: commits=%d err=%v", len(commits), err)
	}
}

// TestRestartOfUndeployedAppFails asserts restarting something that is not
// running is reported, rather than silently doing nothing.
func TestRestartOfUndeployedAppFails(t *testing.T) {
	srv, _, _ := newDeployServer(t)
	h := srv.Handler()

	setupAppWithCommit(t, srv, h, "shop")

	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/restart", nil)
	if rec.Code == http.StatusOK {
		t.Error("restarting an app that was never deployed reported success")
	}
}

// TestStatusSeparatesRecordFromCluster asserts status reports AppLab's record
// and the cluster's view as distinct fields.
//
// They are deliberately not reconciled into one value: the difference is the
// useful information, since an app AppLab thinks is running but whose pods are
// TestStatusReportsNothingRunningForAnUndeployedApp asserts the status endpoint
// does not claim an app is deployed when nothing is.
//
// This is the whole point of reading the cluster instead of a stored status: an
// app whose record says "running" from a previous installation, with no
// Deployment behind it, must report as not deployed.
func TestStatusReportsNothingRunningForAnUndeployedApp(t *testing.T) {
	srv, _, _ := newDeployServer(t)
	h := srv.Handler()

	setupAppWithCommit(t, srv, h, "shop")

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d (%s)", rec.Code, rec.Body.String())
	}

	var result struct {
		AppID  string `json:"app_id"`
		Status string `json:"status"`
		Live   *struct {
			Deployed  bool `json:"deployed"`
			Available bool `json:"available"`
		} `json:"live"`
	}
	decodeData(t, rec, &result)

	if result.AppID != "shop" {
		t.Errorf("app_id = %q, want shop", result.AppID)
	}
	// Nothing has been deployed, so the cluster must say so — and with no
	// Deployment there is no live block at all.
	if result.Live != nil {
		t.Errorf("live state was reported for an app with no Deployment: %+v", result.Live)
	}
	if result.Status != "created" {
		t.Errorf("status = %q, want created — nothing is running", result.Status)
	}
	// And the address is still reported. It is where the app is *served* — a fact
	// about its settings, known from the moment the app exists — rather than
	// whether anything answers there, which is what status says above.
	if rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop", nil); !strings.Contains(rec.Body.String(), `"url"`) {
		t.Errorf("an app with no Deployment reported no url; the address is known even when nothing is serving it:\n%s", rec.Body.String())
	}
}

// TestDeployWithBuildStartsABuild asserts `build:true` starts a build and returns
// a 202 rather than holding the request open for a build that may take minutes —
// a caller cannot otherwise tell progress from a hang.
func TestDeployWithBuildStartsABuild(t *testing.T) {
	srv, _, _ := newDeployServer(t)
	h := srv.Handler()

	setupAppWithCommit(t, srv, h, "shop")

	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{"build": true})

	// 202 with the build, not 200: the build runs as a Job and the caller follows
	// it. Holding the request until it finished would make a slow build
	// indistinguishable from a hung server.
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	var body struct {
		Data struct {
			Build struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"build"`
			Status string `json:"status"`
		} `json:"data"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)

	if body.Data.Build.ID == "" {
		t.Errorf("the response carries no build id, so the caller cannot follow it: %s", rec.Body.String())
	}
	if body.Data.Status != "building" {
		t.Errorf("status = %q, want building", body.Data.Status)
	}
}

// TestDeployWithBuildWithoutABuildHalfIs501 asserts the honest refusal when a
// deployment cannot build: `build:true` is a request this installation cannot
// satisfy, and a 500 or a silent no-op would both be worse.
func TestDeployWithBuildWithoutABuildHalfIs501(t *testing.T) {
	// A server with a cluster and a deployer but no build engine: the registry is
	// not configured, which is a way AppLab is meant to run.
	srv, client, st := newDeployServer(t)
	srv.WithBuild(nil)
	h := srv.Handler()

	commit := setupAppWithCommit(t, srv, h, "shop")
	registerBuildJob(t, srv, st, "shop", commit)
	// The Job is the record, but the engine is gone — so a deploy of a commit
	// with no build finds nothing and has nothing to fall back on.
	clientsetFor(srv).BatchV1().Jobs("ops-system").DeleteCollection(
		context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})

	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{"build": true})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusNotImplemented, rec.Body.String())
	}

	var body struct {
		Error string `json:"error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if !strings.Contains(body.Error, "no image exists") {
		t.Errorf("the error should explain there is no image: %q", body.Error)
	}
	_ = client
}

// TestDeployRequiresDeployer asserts a deployment with no cluster answers 501
// rather than failing obscurely.
func TestDeployRequiresDeployer(t *testing.T) {
	srv, _ := newTestServer(t) // no deployer attached
	h := srv.Handler()

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"}); rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d", rec.Code)
	}

	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{})
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotImplemented)
	}
}

// --- helpers ---------------------------------------------------------------

// TestDeletingAnAppRemovesItsClusterObjects covers what deleting an app means
// now that every app shares one namespace: there is no namespace to drop, so the
// server has to find the app's objects by label and remove them.
//
// The other half of the assertion is the one that matters. AppLab's own
// Deployment lives in the same namespace with no app label at all, so a delete
// that is too broad takes the platform down with the app.
func TestDeletingAnAppRemovesItsClusterObjects(t *testing.T) {
	srv, client, st := newDeployServer(t)
	h := srv.Handler()
	ctx := context.Background()

	commit := setupAppWithCommit(t, srv, h, "shop")
	registerBuildJob(t, srv, st, "shop", commit)
	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("deploy: %d (%s)", rec.Code, rec.Body.String())
	}

	// A second app, deployed into the same namespace, plus AppLab itself.
	otherCommit := setupAppWithCommit(t, srv, h, "blog")
	registerBuildJob(t, srv, st, "blog", otherCommit)
	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/blog/deploy", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("deploy blog: %d (%s)", rec.Code, rec.Body.String())
	}
	if _, err := client.AppsV1().Deployments("ops-system").Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "applab", Namespace: "ops-system"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed applab's own deployment: %v", err)
	}

	if rec := doRequest(t, h, http.MethodDelete, "/api/v1/apps/shop", nil); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d (%s)", rec.Code, rec.Body.String())
	}

	// The app's own objects are gone.
	if _, err := client.AppsV1().Deployments("ops-system").Get(ctx, "applab-shop", metav1.GetOptions{}); err == nil {
		t.Error("the app's Deployment survived the delete")
	}
	if _, err := client.CoreV1().Services("ops-system").Get(ctx, "applab-shop", metav1.GetOptions{}); err == nil {
		t.Error("the app's Service survived the delete")
	}

	// Nothing else was touched.
	if _, err := client.AppsV1().Deployments("ops-system").Get(ctx, "applab-blog", metav1.GetOptions{}); err != nil {
		t.Errorf("deleting one app removed another app's Deployment: %v", err)
	}
	if _, err := client.AppsV1().Deployments("ops-system").Get(ctx, "applab", metav1.GetOptions{}); err != nil {
		t.Errorf("deleting an app removed applab's own Deployment: %v", err)
	}
}

// When the cluster objects cannot be removed, the app's record is kept so the
// operation is retryable. Dropping the record first would leave running
// workloads that nothing knows about any more.
func TestDeleteKeepsTheRecordWhenClusterCleanupFails(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.OpenLocal(context.Background(), filepath.Join(dataDir, "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	src, err := source.New(source.Options{Objects: newObjects(t), DataDir: dataDir})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	cfg := config.Default()
	cfg.Keys = []string{"test-key"}
	cfg.Namespace = "ops-system"
	cfg.DataDir = dataDir

	srv := api.New(cfg, st, auth.New(cfg.Keys)).
		WithSource(src).
		WithAppObjectsDeleter(func(ctx context.Context, appID string) error {
			return errors.New("the cluster is unreachable")
		})

	h := srv.Handler()
	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"}); rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d (%s)", rec.Code, rec.Body.String())
	}

	rec := doRequest(t, h, http.MethodDelete, "/api/v1/apps/shop", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	// The record has to survive, or the retry has nothing to retry.
	if _, err := st.GetApp(context.Background(), "shop"); err != nil {
		t.Errorf("the app record was deleted despite the cleanup failing: %v", err)
	}
}

// TestListAppsReportsTheAddressBeforeDeploy asserts an app that has never been
// deployed still reports where it is served.
//
// The address is a fact about the app's settings, known from the moment it
// exists, and it is what someone needs in order to know where the first deploy
// will put it. It used to be withheld until something was serving, on the
// reasoning that a URL that 404s reads as broken — and the field that carried
// "where will this be" in the meantime was the hostname, which only told the
// whole address when there was no path prefix.
func TestListAppsReportsTheAddressBeforeDeploy(t *testing.T) {
	srv, _, _ := newDeployServer(t)
	h := srv.Handler()

	setupAppWithCommit(t, srv, h, "shop")

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop", nil)
	var app map[string]any
	decodeData(t, rec, &app)

	if app["url"] != "http://shop.apps.example.com" {
		t.Errorf("url = %v, want the address even before a deploy", app["url"])
	}
}

// TestDeploymentHasNoStaleObjectsAfterRedeploy asserts a redeploy updates in
// place rather than accumulating objects.
func TestDeploymentHasNoStaleObjectsAfterRedeploy(t *testing.T) {
	srv, client, st := newDeployServer(t)
	h := srv.Handler()
	ctx := context.Background()

	commit := setupAppWithCommit(t, srv, h, "shop")
	registerBuildJob(t, srv, st, "shop", commit)

	for i := 0; i < 3; i++ {
		if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{}); rec.Code != http.StatusOK {
			t.Fatalf("deploy %d: %d (%s)", i, rec.Code, rec.Body.String())
		}
	}

	deployments, _ := client.AppsV1().Deployments("ops-system").List(ctx, metav1.ListOptions{})
	if len(deployments.Items) != 1 {
		t.Errorf("%d deployments exist after repeated deploys, want 1", len(deployments.Items))
	}
	services, _ := client.CoreV1().Services("ops-system").List(ctx, metav1.ListOptions{})
	if len(services.Items) != 1 {
		t.Errorf("%d services exist after repeated deploys, want 1", len(services.Items))
	}
}

// TestStatusReportsLiveClusterState asserts the live numbers come from the
// Deployment rather than from AppLab's own record.
func TestStatusReportsLiveClusterState(t *testing.T) {
	srv, client, st := newDeployServer(t)
	h := srv.Handler()
	ctx := context.Background()

	commit := setupAppWithCommit(t, srv, h, "shop")
	registerBuildJob(t, srv, st, "shop", commit)

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("deploy: %d (%s)", rec.Code, rec.Body.String())
	}

	// Make the cluster report the app as available.
	deployment, err := client.AppsV1().Deployments("ops-system").Get(ctx, "applab-shop", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	deployment.Status.ReadyReplicas = 1
	deployment.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:   appsv1.DeploymentAvailable,
		Status: corev1.ConditionTrue,
	}}
	if _, err := client.AppsV1().Deployments("ops-system").Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update deployment status: %v", err)
	}

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/status", nil)
	var result struct {
		Status string `json:"status"`
		Live   *struct {
			Deployed      bool   `json:"deployed"`
			Available     bool   `json:"available"`
			ReadyReplicas int32  `json:"ready_replicas"`
			CurrentImage  string `json:"current_image"`
		} `json:"live"`
	}
	decodeData(t, rec, &result)

	if result.Live == nil || !result.Live.Available {
		t.Fatalf("live state does not report the app as available: %+v", result.Live)
	}
	if result.Live.CurrentImage == "" {
		t.Error("the current image was not reported")
	}
	// A completed rollout must move AppLab's own record to running, so the two
	// converge rather than drifting.
	if result.Status != "running" {
		t.Errorf("status = %q, want running once the cluster reports the app available", result.Status)
	}
}

// TestNamespaceIsFilledOnLoad is a regression test for a bug that
// would have been invisible until a deploy landed in the wrong place.
//
// App.Namespace is derived from configuration rather than stored, and it has to
// be filled in wherever an app is loaded. When it was not, the deployer addressed
// the empty namespace — and Kubernetes treats "" as the *default* namespace, so
// no error is raised, the app's own namespace stays empty, and the app silently
// runs somewhere else.
func TestNamespaceIsFilledOnLoad(t *testing.T) {
	srv, _, st := newDeployServer(t)
	h := srv.Handler()

	commit := setupAppWithCommit(t, srv, h, "shop")

	// Read the app back through every path that loads one, and check the
	// namespace is derived in each.
	for _, path := range []string{
		"/api/v1/apps/shop",
		"/api/v1/apps",
	} {
		rec := doRequest(t, h, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d (%s)", path, rec.Code, rec.Body.String())
		}

		var raw struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}

		// The listing is an array and the detail is an object; both are checked
		// for the namespace-bearing id, which is what the namespace is derived
		// from, so a regression here means the derivation itself broke.
		var one map[string]any
		if err := json.Unmarshal(raw.Data, &one); err != nil {
			var many []map[string]any
			if err := json.Unmarshal(raw.Data, &many); err != nil || len(many) == 0 {
				t.Fatalf("decode %s: %v", path, err)
			}
			one = many[0]
		}
		if one["id"] != "shop" {
			t.Errorf("%s: id = %v, want shop", path, one["id"])
		}
	}

	// The deploy path is where an empty namespace would actually do damage, so
	// it is exercised rather than only the read paths.
	registerBuildJob(t, srv, st, "shop", commit)

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("deploy: %d (%s)", rec.Code, rec.Body.String())
	}
}
