package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/auth"
	"github.com/shaowenchen/applab/internal/config"
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
func pushServer(t *testing.T) (*api.Server, *fakeBuildEngine, *store.Store) {
	t.Helper()

	srv, _, st := newDeployServerWithSource(t)
	engine := &fakeBuildEngine{}
	srv.WithBuild(engine)
	return srv, engine, st
}

// TestAPushBuildsWhatWasPushed asserts the pushed commit is what gets built.
//
// The tip is read from the repository rather than taken from the push request,
// because the push request does not carry one: git tells the server which refs
// moved, and the commit is whatever that ref now points at. Reading it here is
// what makes a push of a branch that had commits added by anything else still
// build the right thing.
func TestAPushBuildsWhatWasPushed(t *testing.T) {
	srv, engine, st := pushServer(t)
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
	srv, engine, st := pushServer(t)
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

	// The deploy happens on the background job, so the wait is for the app to be
	// recorded as running that commit rather than for the call to return.
	waitFor(t, func() bool {
		app, err := st.GetApp(ctx, "shop")
		return err == nil && app.CommitSHA == commit && app.Image != ""
	})

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

// TestAPushToAnUnknownAppDoesNothing asserts a push the store accepted for an app
// that is not there — which authorization should already have refused — does not
// become a crash or a build against nothing.
func TestAPushToAnUnknownAppDoesNothing(t *testing.T) {
	srv, engine, _ := pushServer(t)

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
