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
		BuilderImage:    "moby/buildkit:v0.19.0",
		FetcherImage:    "alpine:3.21",
		Registry:        "registry.example.com/apps",
		Rootless:        true,
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

// TestStartCreatesJobWithTheSourceToken asserts a build creates the Job it
// needs and hands the token to the one container entitled to it.
//
// The token used to travel in a Secret beside the Job, so that no credential
// appeared in the pod spec. AppLab no longer uses Secret objects, which makes
// the question this test asks a different one: not "is it out of the spec" —
// it is in the spec now, by construction — but "did it reach only the fetcher".
// The fetch container authenticates one GET; the build container runs arbitrary
// code from the uploaded Dockerfile, so a credential there is a credential
// handed to whoever wrote that Dockerfile.
func TestStartCreatesJobWithTheSourceToken(t *testing.T) {
	engine, client := newTestEngine(t)
	ctx := context.Background()
	app := testApp()

	// The namespace has to exist before a Job can be created in it; in the real
	// flow AppLab creates it, so the fake must have it too.
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: app.Namespace},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	const commit = "abc123def456789012345678901234567890abcd"
	jobName, err := engine.Start(ctx, app, "buildid1", commit, "tok-123")
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

	// No Secret objects at all: the token is an environment variable, so there
	// is nothing beside the Job to leak or to clean up.
	secrets, err := client.CoreV1().Secrets(app.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	if len(secrets.Items) != 0 {
		t.Errorf("the build left %d Secrets behind; AppLab keeps the token on the Job", len(secrets.Items))
	}

	// The fetcher is the container that must have it.
	fetcher := findContainer(t, job, "fetch-source")
	if got := envValue(fetcher, "APPLAB_SOURCE_TOKEN"); got != "tok-123" {
		t.Errorf("APPLAB_SOURCE_TOKEN on the fetch container = %q, want the token that was passed", got)
	}

	// The build container must NOT: it runs arbitrary code from the uploaded
	// Dockerfile, so giving it the token would hand it AppLab's source access.
	builder := findContainer(t, job, "build")
	if got := envValue(builder, "APPLAB_SOURCE_TOKEN"); got != "" {
		t.Errorf("the build container carries the source token (%q); a build must not hold a credential it can exfiltrate", got)
	}
	if specText := jobSpecText(t, job); !strings.Contains(specText, "APPLAB_SOURCE_TOKEN") {
		// The fetcher was found by name above, so this only guards the helper
		// staying honest: if jobSpecText stopped rendering env, the assertion
		// above would pass for the wrong reason.
		t.Error("the rendered Job spec does not mention the token variable at all")
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

		jobName, err := engine.Start(ctx, app, "b1", strings.Repeat("a", 40), "tok")
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

	t.Run("rootless is unprivileged", func(t *testing.T) {
		engine, client := newTestEngine(t)
		ctx := context.Background()
		app := testApp()
		createNamespace(t, client, app.Namespace)

		jobName, err := engine.Start(ctx, app, "b1", strings.Repeat("a", 40), "tok")
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
			t.Error("a rootless build must not be privileged; that is the whole point of the mode")
		}
		if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
			t.Error("a rootless build must run as a non-root user")
		}
		// RootlessKit needs to create a user namespace, which the default seccomp
		// profile blocks. Without this the build fails with a permissions error
		// that says nothing about the cause.
		if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined {
			t.Error("a rootless build needs an unconfined seccomp profile to create its user namespace")
		}
	})

	t.Run("privileged is opted into explicitly", func(t *testing.T) {
		cfg := testConfig()
		cfg.Rootless = false
		client := fake.NewSimpleClientset()
		engine := New(client, cfg)

		ctx := context.Background()
		app := testApp()
		createNamespace(t, client, app.Namespace)

		jobName, err := engine.Start(ctx, app, "b1", strings.Repeat("a", 40), "tok")
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		job, _ := client.BatchV1().Jobs(app.Namespace).Get(ctx, jobName, metav1.GetOptions{})

		builder := findContainer(t, job, "build")
		if builder.SecurityContext == nil || builder.SecurityContext.Privileged == nil || !*builder.SecurityContext.Privileged {
			t.Error("the privileged escape hatch did not produced a privileged container")
		}
	})

	t.Run("the fetch container is always unprivileged", func(t *testing.T) {
		// The fetcher downloads and unpacks untrusted input; it never needs
		// privilege, so it must never have any, whichever mode the build is in.
		for _, rootless := range []bool{true, false} {
			cfg := testConfig()
			cfg.Rootless = rootless
			client := fake.NewSimpleClientset()
			engine := New(client, cfg)

			ctx := context.Background()
			app := testApp()
			createNamespace(t, client, app.Namespace)

			jobName, err := engine.Start(ctx, app, "b1", strings.Repeat("a", 40), "tok")
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			job, _ := client.BatchV1().Jobs(app.Namespace).Get(ctx, jobName, metav1.GetOptions{})

			fetcher := findContainer(t, job, "fetch-source")
			sc := fetcher.SecurityContext
			if sc == nil {
				t.Fatal("the fetch container has no security context")
			}
			if sc.Privileged != nil && *sc.Privileged {
				t.Errorf("rootless=%v: the fetch container is privileged; it never needs to be", rootless)
			}
			if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
				t.Errorf("rootless=%v: the fetch container allows privilege escalation", rootless)
			}
		}
	})
}

// TestImageTagIsTheCommit asserts the image tag names the source, which is what
// makes a rollback able to reuse an image rather than rebuild it.
func TestImageTagIsTheCommit(t *testing.T) {
	engine, _ := newTestEngine(t)

	commit := "abc123def456789012345678901234567890abcd"
	image := engine.ImageFor("shop", commit)

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

		// A trailing slash is a typo, not a deeper path.
		{"trailing slash", "shaowenchen/applab/", "demo", "shaowenchen/applab", "demo-"},
	}

	commit := "abc123def456789012345678901234567890abcd"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, _ := newTestEngine(t, func(c *Config) { c.Registry = tc.registry })

			image := engine.ImageFor(tc.appID, commit)
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
	for _, registry := range []string{"registry.example.com/apps", "shaowenchen", "shaowenchen/applab"} {
		t.Run(registry, func(t *testing.T) {
			engine, _ := newTestEngine(t, func(c *Config) { c.Registry = registry })

			commit := "abc123def456789012345678901234567890abcd"
			shop := engine.ImageFor("shop", commit)
			blog := engine.ImageFor("blog", commit)

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
// the registry, and because the export is not best-effort that fails the build
// rather than quietly skipping the cache.
func TestCacheRefFollowsTheImage(t *testing.T) {
	cases := []struct {
		registry  string
		cachePath string
		want      string
	}{
		{"registry.example.com/apps", "cache.example.com", "cache.example.com/shop:buildcache"},
		{"shaowenchen", "cache", "cache/shop:buildcache"},
		{"shaowenchen/applab", "shaowenchen/cache", "shaowenchen/cache:shop-buildcache"},
	}

	for _, tc := range cases {
		t.Run(tc.registry, func(t *testing.T) {
			engine, _ := newTestEngine(t, func(c *Config) {
				c.Registry = tc.registry
				c.CacheRepoPrefix = tc.cachePath
			})

			job := engine.jobSpec(testApp(), "job", "b1", strings.Repeat("a", 40), "image:tag", "tok")
			args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")

			if !strings.Contains(args, tc.want) {
				t.Errorf("the build args do not contain %q:\n%s", tc.want, args)
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
			client := fake.NewSimpleClientset(tc.job)
			engine := New(client, testConfig())

			status, _, err := engine.Status(ctx, "applab-shop", tc.job.Name)
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
// It used to guard a rollback: the token was a Secret created before the Job and
// deleted again if the Job was refused. With the token on the Job there is
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

	if _, err := engine.Start(ctx, app, "b1", strings.Repeat("a", 40), "tok"); err == nil {
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
// of stopping a build now: the token lives on the Job and goes with it.
func TestCancelStopsTheBuild(t *testing.T) {
	engine, client := newTestEngine(t)
	ctx := context.Background()
	app := testApp()
	createNamespace(t, client, app.Namespace)

	jobName, err := engine.Start(ctx, app, "b1", strings.Repeat("a", 40), "tok")
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
		{"no registry", Config{BuilderImage: "b", FetcherImage: "f", AppLabURL: "u"}, false},
		{"no builder image", Config{Registry: "r", FetcherImage: "f", AppLabURL: "u"}, false},
		{"no applab url", Config{Registry: "r", BuilderImage: "b", FetcherImage: "f"}, false},
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

// TestCacheIsImportedAndExported asserts the registry-side cache is wired up
// when configured. Without both flags a Job has no persistent disk, so every
// build would start from nothing — which is the difference between a one-minute
// and a ten-minute rebuild.
func TestCacheIsImportedAndExported(t *testing.T) {
	engine, _ := newTestEngine(t)
	job := engine.jobSpec(testApp(), "job", "b1", strings.Repeat("a", 40), "image:tag", "tok")

	builder := findContainer(t, job, "build")
	args := strings.Join(builder.Args, " ")

	if !strings.Contains(args, "--export-cache") {
		t.Error("the build does not export a cache, so no rebuild can reuse its layers")
	}
	if !strings.Contains(args, "--import-cache") {
		t.Error("the build does not import a cache")
	}
	if !strings.Contains(args, "shop:buildcache") {
		t.Errorf("the cache reference does not name the app:\n%s", args)
	}
}

// TestNoCacheWhenUnconfigured asserts an empty cache prefix produces no cache
// flags, rather than flags with an empty reference that would fail the build.
func TestNoCacheWhenUnconfigured(t *testing.T) {
	cfg := testConfig()
	cfg.CacheRepoPrefix = ""
	engine := New(fake.NewSimpleClientset(), cfg)

	job := engine.jobSpec(testApp(), "job", "b1", strings.Repeat("a", 40), "image:tag", "tok")
	builder := findContainer(t, job, "build")

	args := strings.Join(builder.Args, " ")
	if strings.Contains(args, "buildcache") {
		t.Errorf("a cache flag was emitted with no cache prefix configured:\n%s", args)
	}
}

// TestDockerfilePathIsPassed asserts the app's Dockerfile path reaches buildctl,
// since an app may keep it somewhere other than the root.
func TestDockerfilePathIsPassed(t *testing.T) {
	engine, _ := newTestEngine(t)

	app := testApp()
	app.Dockerfile = "build/Dockerfile.prod"

	job := engine.jobSpec(app, "job", "b1", strings.Repeat("a", 40), "image:tag", "tok")
	builder := findContainer(t, job, "build")

	args := strings.Join(builder.Args, " ")
	if !strings.Contains(args, "filename=build/Dockerfile.prod") {
		t.Errorf("the Dockerfile path was not passed to buildctl:\n%s", args)
	}
}

// TestDeadlineBoundsTheBuild asserts a build cannot run forever.
func TestDeadlineBoundsTheBuild(t *testing.T) {
	cfg := testConfig()
	cfg.ActiveDeadline = 15 * time.Minute
	engine := New(fake.NewSimpleClientset(), cfg)

	job := engine.jobSpec(testApp(), "job", "b1", strings.Repeat("a", 40), "image:tag", "tok")

	if job.Spec.ActiveDeadlineSeconds == nil {
		t.Fatal("the build job has no deadline; a hung build would occupy a slot forever")
	}
	if got := *job.Spec.ActiveDeadlineSeconds; got != 900 {
		t.Errorf("deadline = %d seconds, want 900", got)
	}
	// The Job must be kept long enough to read why it failed.
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished == 0 {
		t.Error("the finished job is deleted immediately, so a failure could not be diagnosed")
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

// jobSpecText renders a Job to text, for asserting that a value does not appear
// anywhere in it.
func jobSpecText(t *testing.T, job *batchv1.Job) string {
	t.Helper()

	var b strings.Builder
	for _, c := range job.Spec.Template.Spec.Containers {
		b.WriteString(c.Name + " " + strings.Join(c.Args, " ") + " " + strings.Join(c.Command, " "))
		for _, e := range c.Env {
			b.WriteString(" " + e.Name + "=" + e.Value)
		}
	}
	for _, c := range job.Spec.Template.Spec.InitContainers {
		b.WriteString(c.Name + " " + strings.Join(c.Args, " ") + " " + strings.Join(c.Command, " "))
		for _, e := range c.Env {
			b.WriteString(" " + e.Name + "=" + e.Value)
		}
	}
	return b.String()
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
			InitContainers: []corev1.Container{{Name: "fetch-source"}},
			Containers:     []corev1.Container{{Name: "build"}},
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
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name: "fetch-source",
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
	if !strings.Contains(reason, "fetch-source") {
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

// ---------------------------------------------------------------------------
// What the containers actually shell out to
//
// The fetcher image is configurable, so nothing at compile time can stop it
// being an image with the wrong tools in it — and the default *was* one. It was
// `alpine:3.21`, whose busybox provides wget and no curl, while the fetch script
// called curl: every build died at exit 127 before it sent a single request, and
// reported only "init fetch-source exited 127".
//
// So the scripts are asserted rather than assumed. These read the rendered Job
// spec, which is the artefact the cluster runs.
// ---------------------------------------------------------------------------

// TestFetchScriptUsesToolsTheFetcherImageHas asserts the fetch container shells
// out only to what the default image provides.
//
// The image is alpine, whose busybox has wget and tar and no curl. Anything
// outside that set does not fail loudly — it fails as exit 127, which is the
// number the shell gives for a command it could not find, and which reads as a
// broken build rather than as a missing binary.
func TestFetchScriptUsesToolsTheFetcherImageHas(t *testing.T) {
	engine, _ := newTestEngine(t)

	fetcher := engine.fetchContainer(testApp(), "build-job", strings.Repeat("a", 40), "workspace", "tok")

	var script strings.Builder
	script.WriteString(strings.Join(fetcher.Command, " "))
	script.WriteString(" ")
	script.WriteString(strings.Join(fetcher.Args, " "))
	// Comments stripped before scanning, because the script's own explanation of
	// this very bug names curl — and a check that reads prose would fail on the
	// comment that documents why the code is right.
	text := stripShellComments(script.String())

	if strings.Contains(text, "curl") {
		t.Error("the fetch script calls curl, which alpine does not have — it exits 127 having sent nothing")
	}
	if !strings.Contains(text, "wget") {
		t.Error("the fetch script does not use wget, the one HTTP client the fetcher image is guaranteed to have")
	}
	if !strings.Contains(text, "tar") {
		t.Error("the fetch script does not unpack the archive")
	}
	// The token goes in a header, never in the URL: a URL is what ends up in the
	// server's access log, and the whole point of the token is that it is the
	// only credential a build holds.
	if !strings.Contains(text, "Authorization: Bearer ${token}") {
		t.Error("the fetch script does not send the token as an Authorization header")
	}
}

// TestFetchScriptFailsLegiblyWhenAToolIsMissing asserts the guard is in place.
//
// Without it a fetcher image missing wget is exit 127 and one line of nothing,
// which is the failure that took this long to diagnose.
func TestFetchScriptFailsLegiblyWhenAToolIsMissing(t *testing.T) {
	engine, _ := newTestEngine(t)

	fetcher := engine.fetchContainer(testApp(), "build-job", strings.Repeat("a", 40), "workspace", "tok")
	script := strings.Join(fetcher.Args, "\n")

	if !strings.Contains(script, "command -v") {
		t.Error("the fetch script does not check that its tools exist; a missing one is an unexplained exit 127")
	}
	for _, tool := range []string{"wget", "tar"} {
		if !strings.Contains(script, tool) {
			t.Errorf("the fetch script does not name %s among the tools it needs", tool)
		}
	}
}

// TestBuildContainerFailsLegiblyWhenAToolIsMissing asserts the same for the
// builder, whose binaries come from an image the operator also chooses: without
// rootlesskit or buildkitd the container dies at exit 127 and its log says
// nothing about which one was absent.
func TestBuildContainerFailsLegiblyWhenAToolIsMissing(t *testing.T) {
	for _, rootless := range []bool{true, false} {
		name := "privileged"
		if rootless {
			name = "rootless"
		}

		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Rootless = rootless
			engine := New(fake.NewSimpleClientset(), cfg)

			container := engine.buildContainer(testApp(), "build-job", "buildid1",
				strings.Repeat("a", 40), "registry.example.com/apps/shop:abc", "workspace")
			script := strings.Join(container.Args, "\n")

			if !strings.Contains(script, "command -v") {
				t.Error("the build script does not check that its tools exist")
			}
			if !strings.Contains(script, "buildctl") {
				t.Error("the build script does not check for buildctl")
			}
			// rootlesskit only starts the daemon in rootless mode, so demanding
			// it of a privileged image would refuse an image that works.
			if rootless && !strings.Contains(script, "rootlesskit") {
				t.Error("the rootless build script does not check for rootlesskit")
			}
			if !rootless && strings.Contains(script, "rootlesskit") {
				t.Error("the privileged build script demands rootlesskit, which it does not use")
			}
		})
	}
}

// stripShellComments removes `#`-to-end-of-line from each line, so a check can
// scan a script's commands without matching the prose around them.
func stripShellComments(script string) string {
	lines := strings.Split(script, "\n")
	for i, line := range lines {
		if at := strings.Index(line, "#"); at >= 0 {
			lines[i] = line[:at]
		}
	}
	return strings.Join(lines, "\n")
}
