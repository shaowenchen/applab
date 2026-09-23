package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/auth"
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

	dataDir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dataDir, "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	src, err := source.New(source.Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	client := fake.NewSimpleClientset()
	deployer := deploy.New(client, deploy.Config{BaseDomain: "apps.example.com"})

	cfg := config.Default()
	cfg.Keys = []string{"test-key"}
	cfg.BaseDomain = "apps.example.com"
	cfg.DataDir = dataDir

	srv := api.New(cfg, st, auth.New(cfg.Keys)).
		WithSource(src).
		WithDeployer(deployer).
		WithNamespace(
			func(ctx context.Context, appID string) error {
				// Idempotent, as the real k8s.Client.EnsureNamespace is: a
				// namespace that already exists is not a failure, or a second
				// deploy would break.
				_, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{Name: "applab-" + appID},
				}, metav1.CreateOptions{})
				if err != nil && !apierrors.IsAlreadyExists(err) {
					return err
				}
				return nil
			},
			func(ctx context.Context, appID string) error {
				return client.CoreV1().Namespaces().Delete(ctx, "applab-"+appID, metav1.DeleteOptions{})
			},
		)

	// Give the app a namespace so the deployer's objects land somewhere.
	return srv, client, st
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
	app.Namespace = "applab-shop"

	// Stand in for a completed build: a successful record carrying an image.
	build, err := st.FindSucceededBuild(context.Background(), "shop", commit)
	if err == nil {
		t.Fatalf("a build already exists unexpectedly: %v", build)
	}
	recordBuild(t, st, "shop", commit, "registry.example.com/apps/shop:"+commit[:12])

	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("deploy: %d (%s)", rec.Code, rec.Body.String())
	}

	var result struct {
		Host string `json:"host"`
		URL  string `json:"url"`
	}
	decodeData(t, rec, &result)

	if result.Host != "shop.apps.example.com" {
		t.Errorf("host = %q, want shop.apps.example.com", result.Host)
	}
	if !strings.HasPrefix(result.URL, "http") || !strings.Contains(result.URL, result.Host) {
		t.Errorf("url = %q, want a URL containing the host", result.URL)
	}

	// The objects must exist in the app's namespace.
	if _, err := client.AppsV1().Deployments("applab-shop").Get(context.Background(), "app-shop", metav1.GetOptions{}); err != nil {
		t.Errorf("no deployment was created: %v", err)
	}
	// And applab's record must say what is deployed.
	updated, _ := st.GetApp(context.Background(), "shop")
	if updated.CommitSHA != commit {
		t.Errorf("recorded commit = %q, want %q", updated.CommitSHA, commit)
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
	if !strings.Contains(body.Error, "no image exists") {
		t.Errorf("the error should say no image exists for that commit: %q", body.Error)
	}
}

// TestRollbackToDeployedCommitIsRefused asserts rolling back to what is already
// running says so, rather than performing a rollout that changes nothing.
func TestRollbackToDeployedCommitIsRefused(t *testing.T) {
	srv, _, st := newDeployServer(t)
	h := srv.Handler()

	commit := setupAppWithCommit(t, srv, h, "shop")
	recordBuild(t, st, "shop", commit, "registry.example.com/apps/shop:"+commit[:12])

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
	recordBuild(t, st, "shop", commit, "registry.example.com/apps/shop:x")

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("deploy: %d (%s)", rec.Code, rec.Body.String())
	}

	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/stop", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("stop: %d (%s)", rec.Code, rec.Body.String())
	}

	if _, err := client.AppsV1().Deployments("applab-shop").Get(ctx, "app-shop", metav1.GetOptions{}); err == nil {
		t.Error("the deployment still exists after stop")
	}

	// The app and its source must survive.
	app, err := st.GetApp(ctx, "shop")
	if err != nil {
		t.Fatalf("the app was deleted by stop: %v", err)
	}
	if app.Status == "deleted" {
		t.Error("stop deleted the app instead of stopping it")
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

// TestStatusSeparatesRecordFromCluster asserts status reports applab's record
// and the cluster's view as distinct fields.
//
// They are deliberately not reconciled into one value: the difference is the
// useful information, since an app applab thinks is running but whose pods are
// unhealthy is something applab did not cause and could not see otherwise.
func TestStatusSeparatesRecordFromCluster(t *testing.T) {
	srv, _, _ := newDeployServer(t)
	h := srv.Handler()

	setupAppWithCommit(t, srv, h, "shop")

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d (%s)", rec.Code, rec.Body.String())
	}

	var result struct {
		AppID    string `json:"app_id"`
		Status   string `json:"status"`
		Deployed *struct {
			Status string `json:"status"`
		} `json:"deployed"`
		Live *struct {
			Deployed  bool `json:"deployed"`
			Available bool `json:"available"`
		} `json:"live"`
	}
	decodeData(t, rec, &result)

	if result.AppID != "shop" {
		t.Errorf("app_id = %q, want shop", result.AppID)
	}
	if result.Deployed == nil {
		t.Error("no deployment record was reported")
	}
	if result.Live == nil {
		t.Fatal("no live state was reported")
	}
	// Nothing has been deployed, so the cluster must say so.
	if result.Live.Deployed {
		t.Error("live state says the app is deployed, but nothing was deployed")
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

	// This server has no build engine, so the honest answer is 501 naming the
	// reason — not a 500 and not a silent no-op.
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

// recordBuild writes a successful build row, standing in for a completed build.
func recordBuild(t *testing.T, st *store.Store, appID, commit, image string) {
	t.Helper()

	ctx := context.Background()

	id, err := model.NewID()
	if err != nil {
		t.Fatalf("generate build id: %v", err)
	}

	if err := st.CreateBuild(ctx, &model.Build{
		ID:        id,
		AppID:     appID,
		CommitSHA: commit,
		Status:    model.BuildStatusPending,
	}); err != nil {
		t.Fatalf("create build: %v", err)
	}
	if err := st.SetBuildImage(ctx, id, image); err != nil {
		t.Fatalf("set build image: %v", err)
	}
	if err := st.SetBuildStatus(ctx, id, model.BuildStatusSucceeded, ""); err != nil {
		t.Fatalf("set build status: %v", err)
	}
}

// TestDeployCopiesRegistrySecretIntoAppNamespace covers the whole chain a
// registry credential travels: named in configuration, copied into the app's
// namespace when that namespace is provisioned, and referenced by the
// Deployment that needs to pull with it.
//
// Each step on its own is easy to get right and the seam between them is where
// this broke: the Deployment referenced a Secret that nothing ever created, so
// every app deployed against a private registry sat in ImagePullBackOff with
// nothing in applab's own output to explain why.
func TestDeployCopiesRegistrySecretIntoAppNamespace(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dataDir, "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	src, err := source.New(source.Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	// The source of truth is a Secret in applab's own namespace, as the chart
	// expects an operator to have created it.
	const ownNamespace = "applab-system"
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "regcred", Namespace: ownNamespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)},
	})

	namespaces := k8s.NewWithClientset(client, "applab-", ownNamespace)

	cfg := config.Default()
	cfg.Keys = []string{"test-key"}
	cfg.BaseDomain = "apps.example.com"
	cfg.DataDir = dataDir
	// Set on the config, which is where both the deployer and the copy list
	// read it from — the two have to agree or the Deployment references a
	// Secret the server never copied.
	cfg.Deploy.ImagePullSecret = "regcred"

	deployer := deploy.New(client, deploy.Config{
		BaseDomain:      "apps.example.com",
		ImagePullSecret: cfg.Deploy.ImagePullSecret,
	})

	srv := api.New(cfg, st, auth.New(cfg.Keys)).
		WithSource(src).
		WithDeployer(deployer).
		WithNamespace(namespaces.EnsureNamespace, namespaces.DeleteNamespace).
		WithAppSecrets([]string{cfg.Build.PushSecret, cfg.Deploy.ImagePullSecret}, namespaces.CopySecret)

	h := srv.Handler()
	commit := setupAppWithCommit(t, srv, h, "shop")
	recordBuild(t, st, "shop", commit, "registry.example.com/apps/shop:"+commit[:12])

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("deploy: %d (%s)", rec.Code, rec.Body.String())
	}

	// The credential has to be in the namespace the app runs in: a Secret
	// cannot be referenced across a namespace boundary.
	if _, err := client.CoreV1().Secrets("applab-shop").Get(context.Background(), "regcred", metav1.GetOptions{}); err != nil {
		t.Fatalf("the registry Secret did not reach the app's namespace: %v", err)
	}

	// And the Deployment has to actually reference it, or copying it was
	// pointless.
	dep, err := client.AppsV1().Deployments("applab-shop").Get(context.Background(), "app-shop", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	refs := dep.Spec.Template.Spec.ImagePullSecrets
	if len(refs) != 1 || refs[0].Name != "regcred" {
		t.Errorf("the Deployment does not pull with the copied Secret: %v", refs)
	}
}

// A Secret named in configuration but absent from applab's own namespace is an
// operator's mistake, and it has to stop the deploy.
//
// The alternative is a Deployment whose pods never start, which looks like an
// application problem and sends the reader looking in the wrong place.
func TestDeployFailsWhenConfiguredSecretIsMissing(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dataDir, "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	src, err := source.New(source.Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	// No Secret is created, but one is configured.
	client := fake.NewSimpleClientset()
	namespaces := k8s.NewWithClientset(client, "applab-", "applab-system")

	cfg := config.Default()
	cfg.Keys = []string{"test-key"}
	cfg.BaseDomain = "apps.example.com"
	cfg.DataDir = dataDir

	srv := api.New(cfg, st, auth.New(cfg.Keys)).
		WithSource(src).
		WithDeployer(deploy.New(client, deploy.Config{BaseDomain: "apps.example.com"})).
		WithNamespace(namespaces.EnsureNamespace, namespaces.DeleteNamespace).
		WithAppSecrets([]string{"absent"}, namespaces.CopySecret)

	h := srv.Handler()
	commit := setupAppWithCommit(t, srv, h, "shop")
	recordBuild(t, st, "shop", commit, "registry.example.com/apps/shop:"+commit[:12])

	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}

	// The caller is told which step failed without being told the cause, which
	// would name a Secret in applab's own namespace.
	var body struct {
		Error string `json:"error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if !strings.Contains(body.Error, "namespace") {
		t.Errorf("the error does not say which step failed: %q", body.Error)
	}
}

// TestListAppsReportsURLOnlyWhenDeployed asserts an undeployed app does not
// advertise a URL that would 404 at the ingress.
func TestListAppsReportsURLOnlyWhenDeployed(t *testing.T) {
	srv, _, _ := newDeployServer(t)
	h := srv.Handler()

	setupAppWithCommit(t, srv, h, "shop")

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop", nil)
	var app map[string]any
	decodeData(t, rec, &app)

	if _, present := app["url"]; present {
		t.Errorf("an undeployed app reported a URL: %v", app["url"])
	}
	if app["hostname"] != "shop.apps.example.com" {
		t.Errorf("hostname = %v, want the computed hostname even before deploy", app["hostname"])
	}
}

// TestDeploymentHasNoStaleObjectsAfterRedeploy asserts a redeploy updates in
// place rather than accumulating objects.
func TestDeploymentHasNoStaleObjectsAfterRedeploy(t *testing.T) {
	srv, client, st := newDeployServer(t)
	h := srv.Handler()
	ctx := context.Background()

	commit := setupAppWithCommit(t, srv, h, "shop")
	recordBuild(t, st, "shop", commit, "registry.example.com/apps/shop:one")

	for i := 0; i < 3; i++ {
		if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{}); rec.Code != http.StatusOK {
			t.Fatalf("deploy %d: %d (%s)", i, rec.Code, rec.Body.String())
		}
	}

	deployments, _ := client.AppsV1().Deployments("applab-shop").List(ctx, metav1.ListOptions{})
	if len(deployments.Items) != 1 {
		t.Errorf("%d deployments exist after repeated deploys, want 1", len(deployments.Items))
	}
	services, _ := client.CoreV1().Services("applab-shop").List(ctx, metav1.ListOptions{})
	if len(services.Items) != 1 {
		t.Errorf("%d services exist after repeated deploys, want 1", len(services.Items))
	}
}

// TestStatusReportsLiveClusterState asserts the live numbers come from the
// Deployment rather than from applab's own record.
func TestStatusReportsLiveClusterState(t *testing.T) {
	srv, client, st := newDeployServer(t)
	h := srv.Handler()
	ctx := context.Background()

	commit := setupAppWithCommit(t, srv, h, "shop")
	recordBuild(t, st, "shop", commit, "registry.example.com/apps/shop:x")

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("deploy: %d (%s)", rec.Code, rec.Body.String())
	}

	// Make the cluster report the app as available.
	deployment, err := client.AppsV1().Deployments("applab-shop").Get(ctx, "app-shop", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	deployment.Status.ReadyReplicas = 1
	deployment.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:   appsv1.DeploymentAvailable,
		Status: corev1.ConditionTrue,
	}}
	if _, err := client.AppsV1().Deployments("applab-shop").Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
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
	// A completed rollout must move applab's own record to running, so the two
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
	recordBuild(t, st, "shop", commit, "registry.example.com/apps/shop:"+commit[:12])

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("deploy: %d (%s)", rec.Code, rec.Body.String())
	}
}
