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

func newTestEngine(t *testing.T) (*Engine, *fake.Clientset) {
	t.Helper()

	client := fake.NewSimpleClientset()
	return New(client, testConfig()), client
}

// TestStartCreatesJobAndSecret asserts a build creates everything it needs, and
// that the pieces agree.
func TestStartCreatesJobAndSecret(t *testing.T) {
	engine, client := newTestEngine(t)
	ctx := context.Background()
	app := testApp()

	// The namespace has to exist before a Job can be created in it; in the real
	// flow applab creates it, so the fake must have it too.
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

	// The token must be in a Secret, never in the Job's own spec — a pod spec is
	// readable by anyone who can describe the Job, and a container argument would
	// put the credential there.
	secret, err := client.CoreV1().Secrets(app.Namespace).Get(ctx, jobName+"-token", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the build token secret was not created: %v", err)
	}
	if secret.StringData["token"] != "tok-123" {
		t.Errorf("secret token = %q, want the token that was passed", secret.StringData["token"])
	}

	// And the Job spec itself must not contain it.
	specText := jobSpecText(t, job)
	if strings.Contains(specText, "tok-123") {
		t.Error("the source token appears in the Job spec; it must only be in the Secret")
	}

	// The fetcher must mount the secret.
	fetcher := findContainer(t, job, "fetch-source")
	if !mountsSecretFor(fetcher, "token") {
		t.Error("the fetch container does not mount the token secret, so it cannot authenticate")
	}

	// The build container must NOT: it runs arbitrary code from the uploaded
	// Dockerfile, so giving it the token would hand it applab's source access.
	builder := findContainer(t, job, "build")
	if mountsSecretFor(builder, "token") {
		t.Error("the build container mounts the source token; a build must not hold a credential it can exfiltrate")
	}
}

// TestJobSecurityContext asserts the security posture, which is the part of a
// generated manifest that is easiest to get wrong and hardest to notice.
func TestJobSecurityContext(t *testing.T) {
	t.Run("the build pod is given no API token", func(t *testing.T) {
		// Kubernetes mounts a service account token into every pod by default,
		// and in applab's namespace that token can read every Secret — including
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
// in applab's own table.
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

// TestStartRemovesSecretWhenJobFails asserts a failed start does not leave a
// credential behind. A Secret with no Job to use it would accumulate one per
// attempt and keep a live token on disk.
func TestStartRemovesSecretWhenJobFails(t *testing.T) {
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
		t.Errorf("%d secrets were left behind after a failed build start; the token must be cleaned up", len(secrets.Items))
	}
}

// TestCancelRemovesBoth asserts cancelling stops the build and removes its
// credential.
func TestCancelRemovesBoth(t *testing.T) {
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
	if _, err := client.CoreV1().Secrets(app.Namespace).Get(ctx, jobName+"-token", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Error("the token secret still exists after cancellation")
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
	job := engine.jobSpec(testApp(), "job", "b1", strings.Repeat("a", 40), "image:tag")

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

	job := engine.jobSpec(testApp(), "job", "b1", strings.Repeat("a", 40), "image:tag")
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

	job := engine.jobSpec(app, "job", "b1", strings.Repeat("a", 40), "image:tag")
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

	job := engine.jobSpec(testApp(), "job", "b1", strings.Repeat("a", 40), "image:tag")

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
