package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/auth"
	"github.com/shaowenchen/applab/internal/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/source"
	"github.com/shaowenchen/applab/internal/store"
)

// A push is what the platform exists for, so what a push starts is asserted
// rather than assumed. Two halves have to be right and only one of them is in
// the cluster: the build Job is created by the transport's caller, and the deploy
// happens in this process once that Job finishes — with nothing else watching it,
// because AppLab has no controller.

// pushServer returns a server with source storage, a build engine and a
// deployer, so a test can push a real commit and observe what follows.
func pushServer(t *testing.T) (*api.Server, *fakeBuildEngine, *store.Store, *fake.Clientset) {
	t.Helper()

	srv, client, st := newDeployServerWithSource(t)
	engine := &fakeBuildEngine{}
	srv.WithBuild(engine)
	return srv, engine, st, client
}

// deployedCommit reports the commit the cluster says is running for an app.
//
// It reads the Deployment's annotation, which is where a deploy records what it
// put there — the app's own record no longer carries one. Empty means no
// Deployment, or one created before that annotation existed.
func deployedCommit(t *testing.T, client *fake.Clientset, appID string) string {
	t.Helper()
	deployment, err := client.AppsV1().Deployments("ops-system").Get(context.Background(), "applab-"+appID, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return deployment.Annotations["applab.io/commit"]
}

// TestAPushBuildsWhatWasPushed asserts the pushed commit is what gets built.
//
// The tip is read from the repository rather than taken from the push request,
// because the push request does not carry one: git tells the server which refs
// moved, and the commit is whatever that ref now points at. Reading it here is
// what makes a push of a branch that had commits added by anything else still
// build the right thing.
func TestAPushBuildsWhatWasPushed(t *testing.T) {
	srv, engine, st, _ := pushServer(t)
	h := srv.Handler()

	sortAppWithCommit(t, srv, h, "shop")

	// What a push does, invoked the way the git transport invokes it.
	srv.StartPushBuild(context.Background(), "shop", "main")
	waitFor(t, func() bool { return len(engine.startedJobs()) == 1 })

	// The build is recorded against the app and points at the pushed tip.
	builds, err := st.ListBuilds(context.Background(), "shop", 0)
	if err != nil {
		t.Fatalf("list builds: %v", err)
	}
	if len(builds) != 1 {
		t.Fatalf("a push created %d builds, want 1", len(builds))
	}
	if builds[0].Status != model.BuildStatusPending {
		t.Errorf("the pushed build is %q, want pending", builds[0].Status)
	}
	if !model.ValidSHA(builds[0].CommitSHA) {
		t.Errorf("the pushed build names commit %q, which is not a commit id", builds[0].CommitSHA)
	}
}

// TestAPushOfAnAlreadyBuiltCommitDeploysWithoutRebuilding is the shortcut that
// makes pushing an unchanged branch cheap.
//
// The image is tagged by commit, so an unchanged commit is an unchanged image and
// a rebuild would push exactly what is already in the registry. The deploy still
// runs: the case this is for is not "already running" but "built and never
// deployed", and a push means "this source is live".
func TestAPushOfAnAlreadyBuiltCommitDeploysWithoutRebuilding(t *testing.T) {
	srv, engine, st, client := pushServer(t)
	h := srv.Handler()
	ctx := context.Background()

	commit := sortAppWithCommit(t, srv, h, "shop")

	// A build of this commit that already succeeded and recorded its image.
	buildID, err := model.NewID()
	if err != nil {
		t.Fatalf("generate build id: %v", err)
	}
	if err := st.CreateBuild(ctx, &model.Build{
		ID: buildID, AppID: "shop", CommitSHA: commit,
		Status: model.BuildStatusSucceeded, Image: "registry.example.com/apps/shop:abc123",
	}); err != nil {
		t.Fatalf("create build: %v", err)
	}

	srv.StartPushBuild(ctx, "shop", "main")

	// The deploy happens on the background job, so the wait is for the cluster to
	// show that commit rather than for the call to return.
	waitFor(t, func() bool { return deployedCommit(t, client, "shop") == commit })

	if got := engine.startedJobs(); len(got) != 0 {
		t.Errorf("a push of a commit that already has an image started %d builds, want none", len(got))
	}
}

// TestAPushWithNothingToBuildOrDeployIsANoOp asserts the guards. Each is a
// deployment that cannot finish the job, and the failure mode without them is a
// push that starts a build whose image nothing runs, or one that waits forever
// for a deploy that will never come.
func TestAPushWithNothingToBuildOrDeployIsANoOp(t *testing.T) {
	t.Run("no build half", func(t *testing.T) {
		// A deployment with no registry: source and the API work, and a push is
		// only a push.
		srv, _, st := newDeployServerWithSource(t)
		h := srv.Handler()
		sortAppWithCommit(t, srv, h, "shop")

		srv.StartPushBuild(context.Background(), "shop", "main")

		if builds, _ := st.ListBuilds(context.Background(), "shop", 0); len(builds) != 0 {
			t.Errorf("a deployment that cannot build recorded %d builds from a push", len(builds))
		}
	})

	t.Run("no deploy half", func(t *testing.T) {
		// A build with nowhere to run: the image would be pushed and nothing
		// would ever use it, so the build is not worth starting either. This is a
		// deployment with no cluster, which is a legitimate way to run AppLab.
		srv, st := newSourceOnlyServer(t)
		engine := &fakeBuildEngine{}
		srv.WithBuild(engine)
		h := srv.Handler()
		sortAppWithCommit(t, srv, h, "shop")

		srv.StartPushBuild(context.Background(), "shop", "main")
		time.Sleep(50 * time.Millisecond)

		if builds, _ := st.ListBuilds(context.Background(), "shop", 0); len(builds) != 0 {
			t.Errorf("a deployment that cannot deploy recorded %d builds from a push", len(builds))
		}
		if got := engine.startedJobs(); len(got) != 0 {
			t.Errorf("a deployment that cannot deploy started %d builds", len(got))
		}
	})
}

// TestAPushToAnAppWithAutoDeployOffDoesNothing asserts the per-app switch.
//
// It stops the build as well as the deploy, and that is the point rather than an
// oversight: whoever turned it off is releasing by hand, and an image pushed on
// every commit is not what they asked for. The push itself still succeeds — the
// source is stored, which is what git was asked to do.
func TestAPushToAnAppWithAutoDeployOffDoesNothing(t *testing.T) {
	srv, engine, st, _ := pushServer(t)
	h := srv.Handler()
	ctx := context.Background()

	sortAppWithCommit(t, srv, h, "shop")

	off := false
	rec := doRequest(t, h, http.MethodPatch, "/api/v1/apps/shop", map[string]any{"auto_deploy": off})
	if rec.Code != http.StatusOK {
		t.Fatalf("turning auto-deploy off: %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"auto_deploy":true`) {
		t.Errorf("the app still reports auto-deploy on:\n%s", rec.Body.String())
	}

	srv.StartPushBuild(ctx, "shop", "main")
	time.Sleep(100 * time.Millisecond)

	if got := engine.startedJobs(); len(got) != 0 {
		t.Errorf("a push to an app with auto-deploy off started %d builds, want none", len(got))
	}
	if builds, _ := st.ListBuilds(ctx, "shop", 0); len(builds) != 0 {
		t.Errorf("a push to an app with auto-deploy off recorded %d builds", len(builds))
	}
}

// TestAutoDeployReadsAsSet asserts the response reports the resolved value, so a
// caller never has to know that "unset" means on.
func TestAutoDeployReadsAsSet(t *testing.T) {
	srv, _, _, _ := pushServer(t)
	h := srv.Handler()

	// Created without the field: on.
	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"auto_deploy":true`) {
		t.Errorf("a new app does not report auto-deploy on:\n%s", rec.Body.String())
	}

	// Set to false: reported as false, and it stays false on a later read.
	if rec := doRequest(t, h, http.MethodPatch, "/api/v1/apps/shop", map[string]any{"auto_deploy": false}); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d (%s)", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, h, http.MethodGet, "/api/v1/apps/shop", nil)
	if !strings.Contains(rec.Body.String(), `"auto_deploy":false`) {
		t.Errorf("the app does not report auto-deploy off after being set:\n%s", rec.Body.String())
	}

	// A patch that does not mention it leaves it alone.
	if rec := doRequest(t, h, http.MethodPatch, "/api/v1/apps/shop", map[string]any{"name": "Shop"}); rec.Code != http.StatusOK {
		t.Fatalf("patch name: %d (%s)", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, h, http.MethodGet, "/api/v1/apps/shop", nil)
	if !strings.Contains(rec.Body.String(), `"auto_deploy":false`) {
		t.Errorf("a patch of another field reset auto-deploy:\n%s", rec.Body.String())
	}
}

// TestCreatingAnAppPublishesItsRouting asserts the address is wired from the
// moment the app exists, not at its first deploy.
//
// The console already renders an undeployed app's address as a link marked "not
// deployed", and the API reports the host and path before anything is running —
// so the routing object has to exist for that address to mean anything. What it
// points at is the Service a deploy will create; until then Istio answers 503,
// which is the deliberate trade of publishing early.
func TestCreatingAnAppPublishesItsRouting(t *testing.T) {
	srv, _, _, dyn := newDeployServerWithDynamic(t)
	h := srv.Handler()

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"}); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d (%s)", rec.Code, rec.Body.String())
	}

	vs, err := dyn.Resource(virtualServiceGVR).Namespace("ops-system").Get(
		context.Background(), "applab-shop", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("an app was created with no routing: %v", err)
	}

	hosts, _, _ := unstructured.NestedStringSlice(vs.Object, "spec", "hosts")
	if len(hosts) != 1 || hosts[0] != "shop.apps.example.com" {
		t.Errorf("the published hosts are %v, want [shop.apps.example.com]", hosts)
	}

	// And nothing is running yet: publishing creates routing, not a workload.
	// The address is still reported, because it is where the app *is served* —
	// the status is the field that says nothing answers there yet.
	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop", nil)
	if !strings.Contains(rec.Body.String(), `"url":"http`) {
		t.Errorf("an app with routing but no workload reports no address:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"created"`) {
		t.Errorf("an app with routing but no workload does not report being undeployed:\n%s", rec.Body.String())
	}
}

// virtualServiceGVR mirrors the resource the deployer publishes through. It is
// spelled here rather than imported because internal/deploy keeps it unexported.
var virtualServiceGVR = schema.GroupVersionResource{
	Group: "networking.istio.io", Version: "v1", Resource: "virtualservices",
}

// TestAPushToAnUnknownAppDoesNothing asserts a push the store accepted for an app
// that is not there — which authorization should already have refused — does not
// become a crash or a build against nothing.
func TestAPushToAnUnknownAppDoesNothing(t *testing.T) {
	srv, engine, _, _ := pushServer(t)

	srv.StartPushBuild(context.Background(), "nosuchapp", "main")

	if got := engine.startedJobs(); len(got) != 0 {
		t.Errorf("a push naming an app that does not exist started %d builds", len(got))
	}
}

// newSourceOnlyServer returns a server with source storage and no deployer,
// which is what a deployment with no cluster looks like.
func newSourceOnlyServer(t *testing.T) (*api.Server, *store.Store) {
	t.Helper()

	dataDir := t.TempDir()
	st, err := store.OpenLocal(context.Background(), dataDir+"/t.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	src, err := source.New(source.Options{Objects: newObjects(t), DataDir: dataDir})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	cfg := config.Default()
	cfg.Keys = []string{"test-key"}
	cfg.DataDir = dataDir

	return api.New(cfg, st, auth.New(cfg.Keys)).WithSource(src), st
}

// waitFor polls until cond holds, so a test can wait on a background job without
// sleeping a fixed amount and hoping.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the condition was not met within 5s")
}
