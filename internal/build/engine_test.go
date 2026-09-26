package build

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/shaowenchen/applab/internal/model"
)

func testConfig() Config {
	return Config{
		KanikoImage:     "ghcr.io/osscontainertools/kaniko:v1.26.2",
		Registry:        "registry.example.com/apps",
		AppLabURL:       "https://applab.example.com",
		CacheRepoPrefix: "registry.example.com/cache",
	}
}

func testApp() *model.App {
	return &model.App{
		ID:         "shop",
		Namespace:  "applab-shop",
		Port:       8080,
		Replicas:   1,
		Dockerfile: "Dockerfile",
	}
}

func newTestEngine(t *testing.T, tweak ...func(*Config)) (*Engine, *fake.Clientset) {
	t.Helper()

	cfg := testConfig()
	for _, fn := range tweak {
		fn(&cfg)
	}

	client := fake.NewSimpleClientset()
	return New(client, cfg), client
}

const testCommit = "abc123def456789012345678901234567890abcd"

// TestStartCreatesAJobThatClonesWithTheAppKey asserts the one credential a build
// holds reaches the one container that needs it.
//
// It used to be a single-use token, issued for one commit and consumed by one
// request — because the fetch step was an HTTP GET for a tarball. Kaniko clones
// the repository over git, and a clone is many authenticated requests, so a
// credential that dies on the first one cannot work. What the Job carries now is
// the app's own key: the same one its owner pushes with, reaching that app's
// repository and no other.
func TestStartCreatesAJobThatClonesWithTheAppKey(t *testing.T) {
	engine, client := newTestEngine(t)
	ctx := context.Background()
	app := testApp()
	createNamespace(t, client, app.Namespace)

	jobName, err := engine.Start(ctx, app, "main", "buildid1", testCommit, "app-key-123")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if jobName == "" {
		t.Fatal("Start returned an empty job name")
	}

	job, err := client.BatchV1().Jobs(app.Namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	// No Secret objects at all: the key is an environment variable, so there is
	// nothing beside the Job to leak or to clean up.
	secrets, err := client.CoreV1().Secrets(app.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	if len(secrets.Items) != 0 {
		t.Errorf("the build left %d Secrets behind; the key belongs on the Job", len(secrets.Items))
	}

	builder := findContainer(t, job, "build")
	if got := envValue(builder, "GIT_PASSWORD"); got != "app-key-123" {
		t.Errorf("GIT_PASSWORD = %q, want the app key that was passed", got)
	}
	// Kaniko sends a Basic credential only when there is a username in it, and
	// AppLab reads the password and ignores the username. Something has to be in
	// the username for the header to exist at all.
	if got := envValue(builder, "GIT_USERNAME"); got == "" {
		t.Error("GIT_USERNAME is empty, so kaniko sends no credential and the clone of a private repository fails")
	}
}

// TestJobIsASingleContainer asserts the shape that replacing buildkit with
// kaniko exists to produce.
//
// The old Job was two containers: an init container fetched a tarball because
// buildkit cannot reach a repository itself, and a shared emptyDir was what
// carried the tree from one to the other. Kaniko clones its own context, so both
// the fetch step and the volume it needed are gone — and a regression that
// reintroduced either would be a volume nobody writes and a container nobody
// waits for.
func TestJobIsASingleContainer(t *testing.T) {
	engine, _ := newTestEngine(t)

	job := engine.jobSpec(testApp(), "job", "main", "b1", testCommit, "image:tag", "app-key")

	pod := job.Spec.Template.Spec
	if len(pod.InitContainers) != 0 {
		t.Errorf("the build job has %d init containers; kaniko needs none", len(pod.InitContainers))
	}
	if len(pod.Containers) != 1 {
		t.Fatalf("the build job has %d containers, want one", len(pod.Containers))
	}
	if got := pod.Containers[0].Name; got != "build" {
		t.Errorf("the container is named %q, want build", got)
	}
	for _, v := range pod.Volumes {
		if v.EmptyDir != nil {
			t.Errorf("the build job declares an emptyDir (%s); kaniko's context lives in the container, not in a volume", v.Name)
		}
	}
}

// TestJobSecurityContext asserts the security posture, which is the part of a
// generated manifest that is easiest to get wrong and hardest to notice.
func TestJobSecurityContext(t *testing.T) {
	t.Run("the build pod is given no API token", func(t *testing.T) {
		// Kubernetes mounts a service account token into every pod by default,
		// and in AppLab's namespace that token can read every Secret — including
		// the API keys and the registry credentials sitting beside it. A build
		// runs arbitrary code from the uploaded Dockerfile, so the default is
		// exactly the wrong answer here.
		engine, client := newTestEngine(t)
		ctx := context.Background()
		app := testApp()
		createNamespace(t, client, app.Namespace)

		jobName, err := engine.Start(ctx, app, "main", "b1", testCommit, "app-key")
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		job, err := client.BatchV1().Jobs(app.Namespace).Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get job: %v", err)
		}

		mount := job.Spec.Template.Spec.AutomountServiceAccountToken
		if mount == nil {
			t.Fatal("AutomountServiceAccountToken is unset, so the build pod gets a token by default")
		}
		if *mount {
			t.Error("the build pod is given a Kubernetes API token; a build could read applab's own Secrets")
		}
	})

	t.Run("the build container is not privileged", func(t *testing.T) {
		// Kaniko runs as root — it unpacks the base image into its own root
		// filesystem and runs each Dockerfile step there — but root inside a
		// container is not the same thing as privilege. Nothing here needs
		// privileged mode, host namespaces or a host path, and granting any of
		// them would turn a build from arbitrary code in a container into
		// arbitrary code on the node.
		engine, client := newTestEngine(t)
		ctx := context.Background()
		app := testApp()
		createNamespace(t, client, app.Namespace)

		jobName, err := engine.Start(ctx, app, "main", "b1", testCommit, "app-key")
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		job, err := client.BatchV1().Jobs(app.Namespace).Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get job: %v", err)
		}

		builder := findContainer(t, job, "build")
		sc := builder.SecurityContext
		if sc == nil {
			t.Fatal("the build container has no security context")
		}
		if sc.Privileged != nil && *sc.Privileged {
			t.Error("the build container is privileged; it must not be")
		}
		if sc.RunAsNonRoot != nil && *sc.RunAsNonRoot {
			t.Error("the build container is asked to run as non-root, which kaniko cannot do: it unpacks into its own root filesystem")
		}
		if sc.ReadOnlyRootFilesystem != nil && *sc.ReadOnlyRootFilesystem {
			t.Error("the build container's root filesystem is read-only, so kaniko cannot unpack the image it is building")
		}
	})

	t.Run("the build pod mounts nothing from the node", func(t *testing.T) {
		for _, v := range engineVolumeList(t) {
			if v.HostPath != nil {
				t.Errorf("the build pod mounts %s from the node; a build runs untrusted code and must not reach the host", v.Name)
			}
		}
	})
}

// engineVolumeList is the volumes a default-configured build job declares.
func engineVolumeList(t *testing.T) []corev1.Volume {
	t.Helper()

	engine, _ := newTestEngine(t)
	job := engine.jobSpec(testApp(), "job", "main", "b1", testCommit, "image:tag", "app-key")
	return job.Spec.Template.Spec.Volumes
}

// TestImageTagIsTheCommit asserts the image tag names the source, which is what
// makes a rollback able to reuse an image rather than rebuild it.
func TestImageTagIsTheCommit(t *testing.T) {
	engine, _ := newTestEngine(t)

	image := engine.ImageFor("shop", testCommit)

	if !strings.HasPrefix(image, "registry.example.com/apps/shop:") {
		t.Errorf("image = %q, want it under the configured registry", image)
	}
	if !strings.HasSuffix(image, "abc123def456") {
		t.Errorf("image = %q, want the tag to be the abbreviated commit", image)
	}
}

// TestImageRef covers every shape of registry an app's image has to fit into.
//
// A registry is a host plus a path, and how much path there is decides where the
// app can go. Getting this wrong is not a cosmetic difference: a repository path
// Docker Hub rejects fails the push, and the build reports a failure that says
// nothing about the name being the problem.
func TestImageRef(t *testing.T) {
	cases := []struct {
		name     string
		registry string
		appID    string
		wantRepo string
		wantTag  string
	}{
		// One segment of path: the app becomes the next one and gets a
		// repository of its own. This is the shape every deployment running
		// AppLab uses today, so it must not move.
		{"cluster registry with a prefix", "registry.example.com/apps", "shop", "registry.example.com/apps/shop", ""},
		{"kind registry", "kind-registry:5000", "demo", "kind-registry:5000/demo", ""},
		{"docker hub account", "shaowenchen", "demo", "shaowenchen/demo", ""},
		{"host with no path", "registry.example.com", "shop", "registry.example.com/shop", ""},
		{"localhost", "localhost:5000", "shop", "localhost:5000/shop", ""},

		// Two segments of path: no room left, so the app moves into the tag.
		{"docker hub account and repo", "shaowenchen/applab", "demo", "shaowenchen/applab", "demo-"},
		{"host with a two-segment path", "registry.example.com/team/apps", "shop", "registry.example.com/team/apps", "shop-"},
		{"ghcr", "ghcr.io/owner/repo", "shop", "ghcr.io/owner/repo", "shop-"},

		// A tag on the registry is a prefix on the tag the app already carries,
		// which is how one repository holds several environments without their
		// commits colliding.
		{"docker hub repo with a tag", "shaowenchen/applab:demo", "demo", "shaowenchen/applab", "demo-demo-"},
		{"a deep path with a tag", "registry.example.com/team/apps:staging", "shop", "registry.example.com/team/apps", "staging-shop-"},

		// A colon is a tag only once there has been a slash. Read the other way
		// round, every host:port would become a repository with a tag prefix —
		// "kind-registry:5000" would push to "kind-registry" tagged "5000-demo",
		// which is where this whole split would break a cluster-local registry.
		{"a port is not a tag", "kind-registry:5000", "demo", "kind-registry:5000/demo", ""},
		{"and not with a path either", "registry.example.com:5000/apps", "shop", "registry.example.com:5000/apps/shop", ""},
		{"nor a dotted host and a port", "localhost:5000", "shop", "localhost:5000/shop", ""},

		// A trailing slash is a typo, not a deeper path.
		{"trailing slash", "shaowenchen/applab/", "demo", "shaowenchen/applab", "demo-"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, _ := newTestEngine(t, func(c *Config) { c.Registry = tc.registry })

			image := engine.ImageFor(tc.appID, testCommit)
			want := tc.wantRepo + ":" + tc.wantTag + "abc123def456"
			if image != want {
				t.Errorf("ImageFor(%q, ...) = %q, want %q", tc.appID, image, want)
			}
		})
	}
}

// TestImageRefKeepsAppsApart asserts two apps never resolve to the same image.
//
// This is the property that makes the tag-carrying-the-app variant safe: without
// the app in the tag, every app under a two-segment registry would push to one
// tag and overwrite each other.
func TestImageRefKeepsAppsApart(t *testing.T) {
	for _, registry := range []string{"registry.example.com/apps", "shaowenchen", "shaowenchen/applab", "shaowenchen/applab:demo"} {
		t.Run(registry, func(t *testing.T) {
			engine, _ := newTestEngine(t, func(c *Config) { c.Registry = registry })

			shop := engine.ImageFor("shop", testCommit)
			blog := engine.ImageFor("blog", testCommit)

			if shop == blog {
				t.Errorf("apps shop and blog both resolve to %q under registry %q", shop, registry)
			}
		})
	}
}

// TestCacheRefFollowsTheImage asserts the cache reference is built the same way
// the image is.
//
// A cache reference under a registry too deep to hold it would be rejected by
// the registry, and a rejected cache reference fails the build rather than
// quietly skipping the cache.
func TestCacheRefFollowsTheImage(t *testing.T) {
	cases := []struct {
		registry  string
		cachePath string
		want      string
	}{
		{"registry.example.com/apps", "cache.example.com", "cache.example.com/shop"},
		{"shaowenchen", "cache", "cache/shop"},
		{"shaowenchen/applab", "shaowenchen/cache", "shaowenchen/cache"},
	}

	for _, tc := range cases {
		t.Run(tc.registry, func(t *testing.T) {
			engine, _ := newTestEngine(t, func(c *Config) {
				c.Registry = tc.registry
				c.CacheRepoPrefix = tc.cachePath
			})

			job := engine.jobSpec(testApp(), "job", "main", "b1", testCommit, "image:tag", "app-key")
			args := strings.Join(findContainer(t, job, "build").Args, " ")

			if !strings.Contains(args, "--cache-repo="+tc.want) {
				t.Errorf("the build args do not name the cache repository %q:\n%s", tc.want, args)
			}
		})
	}
}

// TestJobNameLimits asserts the generated name satisfies Kubernetes' constraints,
// since a name that is too long or has bad characters is rejected at creation
// with a message that does not mention which limit was hit.
func TestJobNameLimits(t *testing.T) {
	engine, _ := newTestEngine(t)

	cases := []struct{ appID, buildID string }{
		{"shop", "abc"},
		{strings.Repeat("a", 40), strings.Repeat("b", 32)},
		{"a", "b"},
	}

	for _, tc := range cases {
		name := engine.JobNameFor(tc.appID, tc.buildID)
		if len(name) > 63 {
			t.Errorf("JobNameFor(%q, %q) = %q, which is %d characters; Kubernetes allows 63",
				tc.appID, tc.buildID, name, len(name))
		}
		if !isDNS1123Subdomain(name) {
			t.Errorf("JobNameFor(%q, %q) = %q, which is not a valid Kubernetes name",
				tc.appID, tc.buildID, name)
		}
	}
}

// TestStatusReadsJobConditions asserts the cluster's verdict is translated
// correctly, since that is what every caller's decision depends on.
func TestStatusReadsJobConditions(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name       string
		job        *batchv1.Job
		wantStatus model.BuildStatus
	}{
		{
			name: "complete",
			job: jobWithStatus(batchv1.JobStatus{Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			}}),
			wantStatus: model.BuildStatusSucceeded,
		},
		{
			name: "failed",
			job: jobWithStatus(batchv1.JobStatus{Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
			}}),
			wantStatus: model.BuildStatusFailed,
		},
		{
			name:       "running",
			job:        jobWithStatus(batchv1.JobStatus{Active: 1}),
			wantStatus: model.BuildStatusRunning,
		},
		{
			name:       "not started",
			job:        jobWithStatus(batchv1.JobStatus{}),
			wantStatus: model.BuildStatusPending,
		},
		{
			// A condition that is present but false is not a verdict; the job is
			// still going.
			name: "condition present but false",
			job: jobWithStatus(batchv1.JobStatus{
				Active:     1,
				Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionFalse}},
			}),
			wantStatus: model.BuildStatusRunning,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := New(fake.NewSimpleClientset(tc.job), testConfig())

			status, _, err := engine.Status(ctx, "applab-shop", "build-job")
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if status != tc.wantStatus {
				t.Errorf("status = %q, want %q", status, tc.wantStatus)
			}
		})
	}
}

// TestStatusOfMissingJobIsNotAFailure asserts a Job that has been garbage
// collected is reported as absent rather than failed. Claiming failure would be
// wrong and alarming: the build may have succeeded, and its outcome is recorded
// in AppLab's own table.
func TestStatusOfMissingJobIsNotAFailure(t *testing.T) {
	client := fake.NewSimpleClientset()
	engine := New(client, testConfig())

	status, reason, err := engine.Status(context.Background(), "applab-shop", "nonexistent")
	if err != nil {
		t.Fatalf("Status of a missing job returned an error: %v", err)
	}
	if status != "" {
		t.Errorf("status = %q, want empty to signal that the job is gone", status)
	}
	if reason == "" {
		t.Error("no reason was given for a missing job")
	}
}

// TestStartLeavesNothingBehindWhenJobCreationFails asserts a failed start is a
// no-op.
//
// It used to guard a rollback: the credential was a Secret created before the
// Job and deleted again if the Job was refused. With the key on the Job there is
// nothing to roll back, and the assertion is what keeps that true — a future
// change that reintroduced an object beside the Job would have to clean it up
// here or fail this test.
func TestStartLeavesNothingBehindWhenJobCreationFails(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errForbidden())
	})

	engine := New(client, testConfig())
	ctx := context.Background()
	app := testApp()
	createNamespace(t, client, app.Namespace)

	if _, err := engine.Start(ctx, app, "main", "b1", testCommit, "app-key"); err == nil {
		t.Fatal("Start succeeded despite the job creation failing")
	}

	secrets, _ := client.CoreV1().Secrets(app.Namespace).List(ctx, metav1.ListOptions{})
	if len(secrets.Items) != 0 {
		t.Errorf("%d secrets were left behind after a failed build start", len(secrets.Items))
	}
	jobs, _ := client.BatchV1().Jobs(app.Namespace).List(ctx, metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Errorf("%d jobs exist after a start that was refused", len(jobs.Items))
	}
}

// TestCancelStopsTheBuild asserts cancelling removes the Job, which is the whole
// of stopping a build now: the credential lives on the Job and goes with it.
func TestCancelStopsTheBuild(t *testing.T) {
	engine, client := newTestEngine(t)
	ctx := context.Background()
	app := testApp()
	createNamespace(t, client, app.Namespace)

	jobName, err := engine.Start(ctx, app, "main", "b1", testCommit, "app-key")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := engine.Cancel(ctx, app.Namespace, jobName); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	if _, err := client.BatchV1().Jobs(app.Namespace).Get(ctx, jobName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Error("the job still exists after cancellation")
	}
	secrets, _ := client.CoreV1().Secrets(app.Namespace).List(ctx, metav1.ListOptions{})
	if len(secrets.Items) != 0 {
		t.Errorf("cancelling left %d Secrets behind", len(secrets.Items))
	}
}

// TestReadyRequiresConfiguration asserts a half-configured deployment reports
// itself as unable to build, so a caller gets a clear 501 instead of a Job that
// fails obscurely.
func TestReadyRequiresConfiguration(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"fully configured", testConfig(), true},
		{"no registry", Config{KanikoImage: "k", AppLabURL: "u"}, false},
		{"no kaniko image", Config{Registry: "r", AppLabURL: "u"}, false},
		{"no applab url", Config{Registry: "r", KanikoImage: "k"}, false},
		{"nothing", Config{}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := New(fake.NewSimpleClientset(), tc.cfg)
			if got := engine.Ready(); got != tc.want {
				t.Errorf("Ready() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCacheIsEnabledWhenConfigured asserts the registry-side cache is wired up,
// since a Job has no persistent disk and every rebuild would otherwise start
// from nothing.
func TestCacheIsEnabledWhenConfigured(t *testing.T) {
	engine, _ := newTestEngine(t)
	job := engine.jobSpec(testApp(), "job", "main", "b1", testCommit, "image:tag", "app-key")

	args := strings.Join(findContainer(t, job, "build").Args, " ")

	if !strings.Contains(args, "--cache=true") {
		t.Error("the build does not use a cache, so no rebuild can reuse its layers")
	}
	if !strings.Contains(args, "--cache-repo=registry.example.com/cache/shop") {
		t.Errorf("the cache reference does not name the app:\n%s", args)
	}
}

// TestNoCacheWhenUnconfigured asserts an empty cache prefix produces no cache
// flags, rather than flags with an empty reference that would fail the build.
func TestNoCacheWhenUnconfigured(t *testing.T) {
	cfg := testConfig()
	cfg.CacheRepoPrefix = ""
	engine := New(fake.NewSimpleClientset(), cfg)

	job := engine.jobSpec(testApp(), "job", "main", "b1", testCommit, "image:tag", "app-key")
	args := strings.Join(findContainer(t, job, "build").Args, " ")

	if strings.Contains(args, "--cache") {
		t.Errorf("a cache flag was emitted with no cache prefix configured:\n%s", args)
	}
}

// TestDockerfilePathIsPassed asserts the app's Dockerfile path reaches kaniko,
// since an app may keep it somewhere other than the root.
//
// The path is relative to the checkout, because that is what kaniko resolves it
// against: an absolute path would be looked for on the container's own
// filesystem, where the app's source is not.
func TestDockerfilePathIsPassed(t *testing.T) {
	engine, _ := newTestEngine(t)

	app := testApp()
	app.Dockerfile = "build/Dockerfile.prod"

	job := engine.jobSpec(app, "job", "main", "b1", testCommit, "image:tag", "app-key")
	args := strings.Join(findContainer(t, job, "build").Args, " ")

	if !strings.Contains(args, "--dockerfile build/Dockerfile.prod") {
		t.Errorf("the Dockerfile path was not passed to kaniko:\n%s", args)
	}
}

// TestDeadlineBoundsTheBuild asserts a build cannot run forever.
func TestDeadlineBoundsTheBuild(t *testing.T) {
	cfg := testConfig()
	cfg.ActiveDeadline = 15 * time.Minute
	engine := New(fake.NewSimpleClientset(), cfg)

	job := engine.jobSpec(testApp(), "job", "main", "b1", testCommit, "image:tag", "app-key")

	if job.Spec.ActiveDeadlineSeconds == nil {
		t.Fatal("the build job has no deadline; a hung build would occupy a slot forever")
	}
	if got := *job.Spec.ActiveDeadlineSeconds; got != 900 {
		t.Errorf("deadline = %d seconds, want 900", got)
	}
}

// TestFinishedJobsAreCollected pins how long a finished build's Job is kept.
//
// It is a two-sided assertion, because both directions are failures. Too short
// and a failed build's log is gone before anyone reads why — the build's own
// reason points at the Job, and the Job is what holds the output. Too long and
// every push leaves a Job and a pod behind for a day: the TTL is measured from a
// terminal state, and a successful build's log is worth nothing after a few
// minutes because nothing reads it unless something went wrong.
func TestFinishedJobsAreCollected(t *testing.T) {
	engine := New(fake.NewSimpleClientset(), testConfig())

	job := engine.jobSpec(testApp(), "job", "main", "b1", testCommit, "image:tag", "app-key")

	if job.Spec.TTLSecondsAfterFinished == nil {
		t.Fatal("the finished job is never collected, so every push leaves a Job and a pod behind for good")
	}
	if got, want := *job.Spec.TTLSecondsAfterFinished, int32(1800); got != want {
		t.Errorf("TTLSecondsAfterFinished = %d, want %d (30m)", got, want)
	}
}

// TestContextNamesTheRepositoryTheBranchAndTheCommit asserts what kaniko is
// pointed at, since every part of that string is load-bearing.
//
// The three "#"-separated parts are kaniko's own syntax. The commit is what makes
// a build reproducible: a build is started for a recorded commit, and by the time
// the Job runs the branch may have moved. The full ref is what keeps a branch
// called "v1" from being resolved as a tag of the same name.
func TestContextNamesTheRepositoryTheBranchAndTheCommit(t *testing.T) {
	engine, _ := newTestEngine(t)

	app := testApp()

	job := engine.jobSpec(app, "job", "dev", "b1", testCommit, "image:tag", "app-key")
	args := strings.Join(findContainer(t, job, "build").Args, " ")

	want := "--context git://applab.example.com/git/shop.git#refs/heads/dev#" + testCommit
	if !strings.Contains(args, want) {
		t.Errorf("the build context is not %q:\n%s", want, args)
	}

	// And the app's own branch field is not consulted. A caller can deploy a
	// branch without switching the app to it, so a build that read ActiveBranch
	// here would clone whichever branch is live instead of the one being built —
	// and for a commit that exists only on the branch being built, would fail
	// outright.
	app.Branch = "main"
	job = engine.jobSpec(app, "job", "dev", "b1", testCommit, "image:tag", "app-key")
	args = strings.Join(findContainer(t, job, "build").Args, " ")

	if !strings.Contains(args, want) {
		t.Errorf("the build context followed the app's active branch rather than the branch it was given:\n%s", args)
	}
}

// TestContextFollowsTheAddressAppLabIsReachedAt asserts the clone scheme comes
// from AppLab's own address rather than being assumed.
//
// Kaniko derives the scheme from GIT_PULL_METHOD, which knows only "http" and
// "https" — anything else silently becomes https. An AppLab reached over http
// in-cluster is the cluster-local case the chart already supports, and a build
// that assumed https against it would fail with a TLS error that names neither
// the setting nor the reason.
func TestContextFollowsTheAddressAppLabIsReachedAt(t *testing.T) {
	cases := []struct {
		name       string
		appLabURL  string
		wantHost   string
		wantMethod string
	}{
		{"https", "https://applab.example.com", "applab.example.com", "https"},
		{"http in cluster", "http://applab.ops-system.svc:8080", "applab.ops-system.svc:8080", "http"},
		// base_url is typed by hand in the chart, and a trailing slash is the
		// same address. Left in, it would produce "//git/shop.git", which
		// kaniko's clone reads as a URL with an empty first path segment.
		{"a trailing slash", "https://applab.example.com/", "applab.example.com", "https"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, _ := newTestEngine(t, func(c *Config) { c.AppLabURL = tc.appLabURL })

			job := engine.jobSpec(testApp(), "job", "main", "b1", testCommit, "image:tag", "app-key")
			builder := findContainer(t, job, "build")
			args := strings.Join(builder.Args, " ")

			want := "--context git://" + tc.wantHost + "/git/shop.git#"
			if !strings.Contains(args, want) {
				t.Errorf("the build context is not rooted at %q:\n%s", want, args)
			}
			if got := envValue(builder, "GIT_PULL_METHOD"); got != tc.wantMethod {
				t.Errorf("GIT_PULL_METHOD = %q, want %q: the method and the URL have to agree", got, tc.wantMethod)
			}
		})
	}
}

// TestBuildContainerRunsTheConfiguredImage asserts the image is the one the
// deployment named, and that the credential mount lands where kaniko looks for
// it.
//
// Kaniko reads its registry credentials from DOCKER_CONFIG, which its own image
// sets to /kaniko/.docker. The mount has to be there rather than somewhere that
// merely looks conventional, or the push is unauthenticated and the failure
// reads as a registry that rejected the image.
func TestBuildContainerRunsTheConfiguredImage(t *testing.T) {
	engine, _ := newTestEngine(t, func(c *Config) { c.Secret = "applab-registry" })

	job := engine.jobSpec(testApp(), "job", "main", "b1", testCommit, "image:tag", "app-key")
	builder := findContainer(t, job, "build")

	if builder.Image != "ghcr.io/osscontainertools/kaniko:v1.26.2" {
		t.Errorf("the build runs %q, want the configured kaniko image", builder.Image)
	}
	if !mountsSecretFor(builder, registryVolumeName) {
		t.Fatalf("the build does not mount the registry credential")
	}
	for _, m := range builder.VolumeMounts {
		if m.Name == registryVolumeName && m.MountPath != kanikoDockerConfigDir {
			t.Errorf("the registry credential is mounted at %q, want %q", m.MountPath, kanikoDockerConfigDir)
		}
	}
	// The mount path is the image's own DOCKER_CONFIG, so overriding the
	// variable would have to track it — two things that can drift apart.
	if got := envValue(builder, "DOCKER_CONFIG"); got != "" {
		t.Errorf("DOCKER_CONFIG is overridden to %q; the image's default is the path the mount uses", got)
	}

	// And the volume the mount reads is actually declared. A mount without a
	// volume is a pod that never starts, and the error names neither.
	declared := false
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == registryVolumeName {
			declared = true
			if v.Secret == nil || v.Secret.SecretName != "applab-registry" {
				t.Errorf("the credential volume does not read Secret %q", "applab-registry")
			}
		}
	}
	if !declared {
		t.Error("the credential is mounted but no volume is declared for it")
	}
}

// TestNoCredentialMountWhenTheRegistryNeedsNone asserts a cluster-local registry
// is not handed a volume it has no Secret for.
func TestNoCredentialMountWhenTheRegistryNeedsNone(t *testing.T) {
	engine, _ := newTestEngine(t) // testConfig has no Secret

	job := engine.jobSpec(testApp(), "job", "main", "b1", testCommit, "image:tag", "app-key")

	if vols := job.Spec.Template.Spec.Volumes; len(vols) != 0 {
		t.Errorf("a build with no configured credential declares %d volumes", len(vols))
	}
	if mountsSecretFor(findContainer(t, job, "build"), registryVolumeName) {
		t.Error("a build with no configured credential mounts one anyway")
	}
}

// TestInsecureRegistryCoversBothDirections asserts the opt-in reaches the pull
// as well as the push.
//
// Kaniko has separate flags for each, and a registry that is reachable one way
// and not the other fails in the middle of a build rather than at the start —
// after the clone, which is the slowest part to redo.
func TestInsecureRegistryCoversBothDirections(t *testing.T) {
	engine, _ := newTestEngine(t, func(c *Config) { c.InsecureRegistry = true })

	job := engine.jobSpec(testApp(), "job", "main", "b1", testCommit, "image:tag", "app-key")
	args := strings.Join(findContainer(t, job, "build").Args, " ")

	for _, flag := range []string{"--insecure", "--insecure-pull", "--skip-tls-verify", "--skip-tls-verify-pull"} {
		if !strings.Contains(args, flag) {
			t.Errorf("insecureRegistry is set but %s is not passed:\n%s", flag, args)
		}
	}
}

// TestSecureRegistryPassesNoInsecureFlags is the other half: the default must not
// silently weaken the guarantee that the image that arrived is the image that was
// pushed.
func TestSecureRegistryPassesNoInsecureFlags(t *testing.T) {
	engine, _ := newTestEngine(t)

	job := engine.jobSpec(testApp(), "job", "main", "b1", testCommit, "image:tag", "app-key")
	args := strings.Join(findContainer(t, job, "build").Args, " ")

	if strings.Contains(args, "insecure") || strings.Contains(args, "skip-tls-verify") {
		t.Errorf("the default build passes a flag that disables a verification:\n%s", args)
	}
}

// TestLogFormatIsReadable asserts the output is something a log viewer can read.
//
// Kaniko's default is a redrawing terminal UI: escape codes, cursor movement and
// carriage returns. That is the right output for a person watching a build in a
// terminal and the wrong output for a log AppLab stores and a person reads back
// later.
func TestLogFormatIsReadable(t *testing.T) {
	engine, _ := newTestEngine(t)

	job := engine.jobSpec(testApp(), "job", "main", "b1", testCommit, "image:tag", "app-key")
	args := strings.Join(findContainer(t, job, "build").Args, " ")

	if !strings.Contains(args, "--log-format text") {
		t.Errorf("the build does not ask kaniko for plain text output:\n%s", args)
	}
}

// --- helpers ---------------------------------------------------------------

func createNamespace(t *testing.T, client *fake.Clientset, name string) {
	t.Helper()

	if _, err := client.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace %s: %v", name, err)
	}
}

func jobWithStatus(status batchv1.JobStatus) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "build-job", Namespace: "applab-shop"},
		Status:     status,
	}
}

func findContainer(t *testing.T, job *batchv1.Job, name string) corev1.Container {
	t.Helper()

	for _, c := range job.Spec.Template.Spec.Containers {
		if c.Name == name {
			return c
		}
	}
	for _, c := range job.Spec.Template.Spec.InitContainers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("the job has no container named %q", name)
	return corev1.Container{}
}

func mountsSecretFor(c corev1.Container, volumeName string) bool {
	for _, m := range c.VolumeMounts {
		if m.Name == volumeName {
			return true
		}
	}
	return false
}

// envValue returns the value of a named environment variable on a container, or
// the empty string when the container does not carry it.
func envValue(c corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// isDNS1123Subdomain is the rule Kubernetes applies to a Job name.
func isDNS1123Subdomain(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

func errForbidden() error {
	return apierrors.NewForbidden(
		corev1.Resource("jobs"), "build-job", nil,
	)
}

// ---------------------------------------------------------------------------
// Why a build failed
//
// The Job's own condition is written for `kubectl describe` and names the count
// rather than the cause: "BackoffLimitExceeded: Job has reached the specified
// backoff limit" is what a person pastes into a chat when they have nothing
// else. The cause is on the pod, so the pod is what these check.
// ---------------------------------------------------------------------------

// buildPod builds a pod carrying the Job's label, which is what the failure
// readers select on.
func buildPod(name string, status corev1.PodStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "applab-shop",
			Labels:    map[string]string{"job-name": "build-job"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "build"}},
		},
		Status: status,
	}
}

// TestBackoffLimitFailureNamesWhatThePodDid is the case that prompted this: a
// build that exhausts its retries, where the Job's message is the whole story
// unless the pod is asked.
func TestBackoffLimitFailureNamesWhatThePodDid(t *testing.T) {
	job := jobWithStatus(batchv1.JobStatus{
		Conditions: []batchv1.JobCondition{{
			Type:    batchv1.JobFailed,
			Status:  corev1.ConditionTrue,
			Reason:  "BackoffLimitExceeded",
			Message: "Job has reached the specified backoff limit",
		}},
	})

	pod := buildPod("build-job-abc", corev1.PodStatus{
		Phase: corev1.PodFailed,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "build",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1,
				Reason:   "Error",
			}},
		}},
	})

	engine := New(fake.NewSimpleClientset(job, pod), testConfig())

	status, reason, err := engine.Status(context.Background(), "applab-shop", "build-job")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status != model.BuildStatusFailed {
		t.Fatalf("status = %q, want failed", status)
	}

	// The Job's own words are kept — the pod may be gone by the time this runs,
	// and this string has to survive on its own.
	if !strings.Contains(reason, "BackoffLimitExceeded") {
		t.Errorf("the job's own reason was dropped: %q", reason)
	}
	// And the pod's, which is the part that was missing.
	if !strings.Contains(reason, "build") {
		t.Errorf("the reason does not say which container failed: %q", reason)
	}
	if !strings.Contains(reason, "exited 1") {
		t.Errorf("the reason does not report the exit code: %q", reason)
	}
}

// TestFailureReasonReportsAnEvictedPod asserts the pod-level reason is read:
// an eviction is not a container's failure, and the container statuses are empty
// in exactly that case.
func TestFailureReasonReportsAnEvictedPod(t *testing.T) {
	job := jobWithStatus(batchv1.JobStatus{
		Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit",
		}},
	})
	pod := buildPod("build-job-abc", corev1.PodStatus{
		Phase:   corev1.PodFailed,
		Reason:  "Evicted",
		Message: "The node was low on resource: ephemeral-storage.",
	})

	engine := New(fake.NewSimpleClientset(job, pod), testConfig())

	_, reason, err := engine.Status(context.Background(), "applab-shop", "build-job")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !strings.Contains(reason, "Evicted") {
		t.Errorf("an evicted pod's reason is missing: %q", reason)
	}
	if !strings.Contains(reason, "ephemeral-storage") {
		t.Errorf("the eviction message is missing: %q", reason)
	}
}

// TestFailureReasonPrefersTheInstanceThatFailed asserts a crash loop reports the
// cause rather than the state: the live container is Waiting with
// CrashLoopBackOff, and "Error" with its exit code is on the one that died.
func TestFailureReasonPrefersTheInstanceThatFailed(t *testing.T) {
	job := jobWithStatus(batchv1.JobStatus{
		Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
		}},
	})
	pod := buildPod("build-job-abc", corev1.PodStatus{
		Phase: corev1.PodFailed,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:         "build",
			RestartCount: 3,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CrashLoopBackOff",
			}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 137,
				Reason:   "OOMKilled",
			}},
		}},
	})

	engine := New(fake.NewSimpleClientset(job, pod), testConfig())

	_, reason, err := engine.Status(context.Background(), "applab-shop", "build-job")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !strings.Contains(reason, "OOMKilled") {
		t.Errorf("the cause of the crash loop is missing: %q", reason)
	}
	if strings.Contains(reason, "CrashLoopBackOff") {
		t.Errorf("the reason reports the state rather than the cause: %q", reason)
	}
}

// TestFailureWithoutAPodIsStillReadable asserts the pod read is best-effort. The
// Job's TTL collects the pod, and a failure reported after that must not become
// an error or an empty reason.
func TestFailureWithoutAPodIsStillReadable(t *testing.T) {
	job := jobWithStatus(batchv1.JobStatus{
		Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit",
		}},
	})

	engine := New(fake.NewSimpleClientset(job), testConfig())

	status, reason, err := engine.Status(context.Background(), "applab-shop", "build-job")
	if err != nil {
		t.Fatalf("Status with no pod: %v", err)
	}
	if status != model.BuildStatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
	if !strings.Contains(reason, "BackoffLimitExceeded") {
		t.Errorf("the job's own message did not survive a missing pod: %q", reason)
	}
}

// TestStartRefusesAMissingSecret asserts a build that cannot push is
// refused before a Job is created for it.
//
// A pod that mounts a Secret which is not there never starts, so the Job sits at
// "pending" with an empty log — the container the log would come from never ran
// — and the explanation lives on the pod, where nobody whose build is not working
// is looking. This puts it in the build's own error instead.
func TestStartRefusesAMissingSecret(t *testing.T) {
	engine, client := newTestEngine(t, func(c *Config) { c.Secret = "regcred" })
	ctx := context.Background()
	app := testApp()
	createNamespace(t, client, app.Namespace)

	_, err := engine.Start(ctx, app, "main", "b1", testCommit, "app-key")
	if err == nil {
		t.Fatal("a build started with a registry credential that does not exist")
	}
	// The message has to name the Secret and the namespace, or the reader knows
	// only that something is missing.
	for _, want := range []string{"regcred", app.Namespace} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}

	// And no Job was left behind. One that exists but cannot start is worse than
	// one that was never created: it holds the build's name, so the next attempt
	// is refused as a duplicate.
	jobs, err := client.BatchV1().Jobs(app.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("a Job was created for a build that could not push: %v", jobs.Items[0].Name)
	}
}

// TestStartAcceptsAnExistingSecret is the other half, and the one that keeps
// the check from being a wall: the same configuration with the Secret present
// must build normally.
func TestStartAcceptsAnExistingSecret(t *testing.T) {
	engine, client := newTestEngine(t, func(c *Config) { c.Secret = "regcred" })
	ctx := context.Background()
	app := testApp()
	createNamespace(t, client, app.Namespace)

	if _, err := client.CoreV1().Secrets(app.Namespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "regcred", Namespace: app.Namespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte("{}")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create secret: %v", err)
	}

	if _, err := engine.Start(ctx, app, "main", "b1", testCommit, "app-key"); err != nil {
		t.Fatalf("a build was refused with the credential present: %v", err)
	}
}

// TestStartWithoutASecretDoesNotLookForOne asserts the check is skipped when
// no credential is configured. A cluster-local registry needs none, and a build
// must not be refused for a Secret it was never told to use.
func TestStartWithoutASecretDoesNotLookForOne(t *testing.T) {
	engine, client := newTestEngine(t) // testConfig has no Secret
	ctx := context.Background()
	app := testApp()
	createNamespace(t, client, app.Namespace)

	if _, err := engine.Start(ctx, app, "main", "b1", testCommit, "app-key"); err != nil {
		t.Fatalf("a build with no configured credential was refused: %v", err)
	}
}
