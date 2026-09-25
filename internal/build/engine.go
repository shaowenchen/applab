// Package build runs an app's Dockerfile in the cluster and pushes the resulting
// image.
//
// A build is a one-shot Job, not a request to a long-running builder daemon.
// That choice is the whole design: there is no build service to operate, no
// daemon to keep warm or scale, and a build's resources are released the moment
// it ends. Build capacity scales with the cluster rather than with a
// configuration AppLab has to manage.
//
// The Job has one container:
//
//	build   clones the app's repository at one commit, runs its Dockerfile, and
//	        pushes the resulting image
//
// It is kaniko, which is what makes a single container possible: it clones its
// own build context and unpacks the image it is assembling into its own root,
// so there is no fetched tarball to hand it and no shared volume to hand it
// through. The cost is that it runs as root, which is where buildkit rootless
// was stronger.
//
// The clone uses the app's own key, so a build holds a credential that reaches
// that app's repository — every branch and commit of it. It reaches no other
// app, and it is the same key its owner pushes with.
package build

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/shaowenchen/applab/internal/model"
)

// Config describes one deployment's build environment.
type Config struct {
	// KanikoImage is the image providing the kaniko executor. Empty disables
	// building.
	//
	// Kaniko builds an image from a Dockerfile inside a container and pushes the
	// result, with no daemon and no privileged helpers — it unpacks the base
	// image into its own root and runs each build step in userspace.
	KanikoImage string

	// Registry is the prefix an app's image is pushed under, e.g.
	// "registry.example.com/apps". The image name is "<registry>/<app>".
	Registry string

	// Secret names a Secret holding a .dockerconfigjson for the registry.
	// Empty means the registry needs no credentials, which is normal for a
	// cluster-local registry.
	//
	// It is read from whichever namespace the Job is created in, which is
	// AppLab's own — apps run beside it rather than in namespaces of their own,
	// so there is one Secret for every app rather than one per app. The app's
	// own Deployments reference the same name to pull with; see Deployer.
	Secret string

	// InsecureRegistry allows pushing over plain HTTP and skipping TLS
	// verification.
	//
	// This exists for cluster-local registries that serve HTTP or use a
	// self-signed certificate. It is off by default because it disables the
	// verification that makes a push trustworthy, and it must be enabled
	// deliberately.
	InsecureRegistry bool

	// AppLabURL is the base URL of the AppLab API, which the build clones its
	// source from.
	AppLabURL string

	// CacheRepoPrefix enables registry-side layer caching. When set, the build
	// imports and exports its cache under "<prefix>/<app>:buildcache", so a
	// second build of the same app reuses unchanged layers instead of rebuilding
	// them.
	//
	// The cost is that the cache is a second image in the registry that has to be
	// stored; the benefit is that it survives the Job, which is the whole point —
	// a Job has no persistent disk, so without a registry-side cache every build
	// starts from nothing.
	CacheRepoPrefix string

	// CPU and memory for the build container, and the ephemeral storage for the
	// shared workspace. Builds are bursty and unbounded by nature, so these are
	// required rather than defaulted: an unbounded build can evict its neighbours.
	BuildCPURequest    string
	BuildMemoryRequest string
	BuildCPULimit      string
	BuildMemoryLimit   string
	WorkspaceSizeLimit string

	// ActiveDeadline bounds a build's wall clock.
	ActiveDeadline time.Duration

	// TTLAfterFinished is how long a finished Job's pods are kept, so a failed
	// build's log can still be read. Zero means delete as soon as it finishes,
	// which would make a failure undiagnosable.
	TTLAfterFinished time.Duration
}

// Engine creates and inspects build Jobs.
type Engine struct {
	client kubernetes.Interface
	cfg    Config
}

// New creates an Engine.
func New(client kubernetes.Interface, cfg Config) *Engine {
	if cfg.ActiveDeadline == 0 {
		cfg.ActiveDeadline = 30 * time.Minute
	}
	if cfg.TTLAfterFinished == 0 {
		// Long enough to read a failure's log after the fact, short enough that
		// pods do not accumulate: every build leaves a Job and a pod behind, and
		// a push makes one.
		cfg.TTLAfterFinished = 30 * time.Minute
	}
	if cfg.WorkspaceSizeLimit == "" {
		// Kaniko unpacks the base image and every intermediate layer and filesystem
		// it modifies into its own root filesystem, so this is not the source tree
		// — it is the source tree plus the whole image being built, twice over.
		// Generous, but bounded: without a limit a build that expands something
		// enormous fills the node's disk and takes other workloads down with it.
		cfg.WorkspaceSizeLimit = "10Gi"
	}
	return &Engine{client: client, cfg: cfg}
}

// Ready reports whether the build half is configured well enough to run.
func (e *Engine) Ready() bool {
	return e.cfg.KanikoImage != "" && e.cfg.Registry != "" && e.cfg.AppLabURL != ""
}

// ImageFor returns the image name an app's builds push to.
//
// The tag is the commit, not "latest": a tag that names the exact source is what
// makes a rollback able to reuse an image without rebuilding it, and what makes
// two deploys of the same commit genuinely the same image.
func (e *Engine) ImageFor(appID, commitSHA string) string {
	repo, tagPrefix := imageRef(e.cfg.Registry, appID)
	return fmt.Sprintf("%s:%s%s", repo, tagPrefix, shortSHA(commitSHA))
}

// imageRef works out where an app's image lives under a registry, as a
// repository and a prefix the tag must carry.
//
// A registry is a host plus a path, and the length of that path decides how an
// app is named — because a repository path can only be extended so far before
// the registry rejects it:
//
//	registry                     image for app "demo"
//	registry.example.com/apps    registry.example.com/apps/demo:abc123
//	kind-registry:5000           kind-registry:5000/demo:abc123
//	shaowenchen                  shaowenchen/demo:abc123
//	shaowenchen/applab           shaowenchen/applab:demo-abc123
//
// With nothing or one segment after the host, the app becomes the next segment
// and gets a repository of its own. That is what a cluster-local registry wants,
// and it is what every deployment actually running AppLab uses, so it is
// preserved exactly. With two or more segments the path is already as deep as a
// Docker Hub repository may be, so the app moves into the tag instead — the only
// remaining place to put it.
//
// The app id is used as-is. It is already constrained to lowercase letters,
// digits and dashes by model.ValidateAppID, which is the character set a
// repository and a tag both accept — so no rewriting is needed, and none is done,
// because a rewrite would be a way for two different apps to collide.
func imageRef(registry, appID string) (repo, tagPrefix string) {
	registry = strings.TrimSuffix(strings.TrimSpace(registry), "/")
	if registry == "" {
		return appID, ""
	}
	if pathSegments(registry) <= 1 {
		return registry + "/" + appID, ""
	}
	return registry, appID + "-"
}

// pathSegments counts the path segments after a registry's host.
//
// The host has to be told apart from the path because it is the path that runs
// out of room. A first segment containing a dot or a colon is a host —
// "ghcr.io", "registry.example.com:5000", "localhost" — and everything after it
// is the path. With no host the whole string is a Docker Hub path, whose first
// segment is the account name, so "shaowenchen" is one segment and
// "shaowenchen/applab" is two.
func pathSegments(registry string) int {
	segments := strings.Split(registry, "/")
	if isRegistryHost(segments[0]) {
		return len(segments) - 1
	}
	return len(segments)
}

// isRegistryHost reports whether a registry's first segment names a host rather
// than a Docker Hub account.
func isRegistryHost(segment string) bool {
	return segment == "localhost" || strings.ContainsAny(segment, ".:")
}

// JobNameFor returns the Job name for a build.
//
// The build id is part of it so two builds of one app do not collide, and the
// name is lowercased and truncated to satisfy Kubernetes' 63-character limit on
// a Job name.
func (e *Engine) JobNameFor(appID, buildID string) string {
	name := fmt.Sprintf("applab-build-%s-%s", appID, buildID)
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.ToLower(strings.Trim(name, "-"))
}

// Start creates the build Job for a commit.
//
// appKey is the app's own API key, and it is what the build clones its source
// with. It replaces a single-use token scoped to one commit, which was what the
// fetch step needed when AppLab downloaded a tarball; kaniko clones the
// repository over git instead, and a git clone is many requests, so a credential
// that is consumed by the first one cannot work.
//
// The reach is therefore the app's repository — every branch and every commit of
// it — rather than one commit. That is the app's own key, the same one its owner
// pushes with, and it reaches no other app; see the chart README's "Keys".
//
// branch is what is cloned, and it is passed in rather than read from the app.
// Each branch is stored as its own repository, so the branch decides *which*
// repository the commit is looked for in — reading the app's active branch here
// would clone the wrong one, and for a commit that only exists on the branch
// being built it would fail outright. A caller can deploy a branch without
// switching the app to it, so the app's own field is not the answer.
func (e *Engine) Start(ctx context.Context, app *model.App, branch, buildID, commitSHA, appKey string) (string, error) {
	if !e.Ready() {
		return "", fmt.Errorf("build is not configured: kaniko image, registry and applab URL are all required")
	}

	namespace := app.Namespace
	jobName := e.JobNameFor(app.ID, buildID)
	image := e.ImageFor(app.ID, commitSHA)

	// Checked before the Job exists, so a missing credential is a failed build
	// with a reason rather than a Job that never starts.
	//
	// The failure it prevents is a silent one. A pod that mounts a Secret which
	// is not there does not start — the container runtime refuses — so the
	// build sits at "pending", its log is empty because the container a log
	// would come from never ran, and the explanation is on the pod rather than
	// anywhere a caller of this API would look. Naming it here puts it in the
	// build's own record, which is where someone whose build is not working is
	// already looking.
	if err := e.checkSecret(ctx, namespace); err != nil {
		return "", err
	}

	job := e.jobSpec(app, jobName, branch, buildID, commitSHA, image, appKey)

	if _, err := e.client.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		// Nothing to roll back: the credential is an environment variable on the
		// Job rather than an object beside it, so a start that fails leaves
		// nothing behind.
		if apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("a build job named %s already exists", jobName)
		}
		return "", fmt.Errorf("create build job %s: %w", jobName, err)
	}

	return jobName, nil
}

// checkSecret reports whether the registry credential a build will mount is
// actually there.
//
// Only when one is configured: a cluster-local registry needs no credential, and
// demanding one would refuse a build that would have worked.
func (e *Engine) checkSecret(ctx context.Context, namespace string) error {
	if e.cfg.Secret == "" {
		return nil
	}

	_, err := e.client.CoreV1().Secrets(namespace).Get(ctx, e.cfg.Secret, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("registry credential %q is not in namespace %s: create it before installing applab (kubectl -n %s create secret docker-registry %s --docker-server=... --docker-username=... --docker-password=...), or set build.secret empty if this registry needs no authentication",
			e.cfg.Secret, namespace, namespace, e.cfg.Secret)
	}
	return fmt.Errorf("read registry credential %q in %s: %w", e.cfg.Secret, namespace, err)
}

// jobSpec builds the Job.
//
// It is separated from Start so it can be rendered and inspected in a test
// without a cluster, which is the only way to check the security context and
// volume wiring on a laptop.
func (e *Engine) jobSpec(app *model.App, jobName, branch, buildID, commitSHA, image, appKey string) *batchv1.Job {
	namespace := app.Namespace

	ttl := int32(e.cfg.TTLAfterFinished.Seconds())
	deadline := int64(e.cfg.ActiveDeadline.Seconds())
	backoff := int32(1)

	labels := map[string]string{
		"app.kubernetes.io/managed-by": "applab",
		"applab.io/app":                app.ID,
		"applab.io/build":              buildID,
	}

	// No shared workspace volume, unlike the two-container Job this replaces.
	// Kaniko clones its own context and unpacks the image it is building into its
	// own root filesystem, so there is nothing for a second container to hand it
	// and nothing for an emptyDir to hold. What kaniko does need is scratch space
	// that grows with the image, which is the writable layer the container runtime
	// gives it — bounded by the pod's ephemeral-storage limit below rather than by
	// a volume this code declares.
	volumes := e.registryVolumes()

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			// One retry, not the default six. A build that fails is almost always
			// the source's fault and will fail identically every time; the retry is
			// there for an evicted pod or a registry blip, not for a broken
			// Dockerfile.
			BackoffLimit: &backoff,

			// Bounds a build that hangs — a Dockerfile RUN that waits on a network
			// resource that never answers would otherwise occupy a slot forever.
			ActiveDeadlineSeconds: &deadline,

			// Keeps a finished Job long enough to read its log, which is the only
			// way to find out why a build failed.
			TTLSecondsAfterFinished: &ttl,

			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					// No API token. A build runs arbitrary code from the uploaded
					// Dockerfile, and Kubernetes mounts a service account token into
					// every pod by default, which in this namespace is read access to
					// whatever AppLab's Role grants. Nothing in this Job needs to talk
					// to the API server: kaniko clones its source over git with the
					// app's own key and pushes to a registry.
					AutomountServiceAccountToken: ptr(false),
					RestartPolicy:                corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						e.buildContainer(app, branch, commitSHA, image, appKey),
					},
					Volumes: volumes,
				},
			},
		},
	}
}

// registryVolumes returns the volumes needed to authenticate to the registry.
//
// Only the volume is declared here; the mount that reads it belongs to the
// container that needs it, in buildContainer. The two are named by the constants
// beside this so they cannot drift: a mount whose volume is not declared is a pod
// that never starts, and the error names neither.
//
// No volume at all when no credential is configured, which is normal for a
// cluster-local registry — demanding one would refuse a build that would have
// worked.
func (e *Engine) registryVolumes() []corev1.Volume {
	if e.cfg.Secret == "" {
		return nil
	}

	return []corev1.Volume{{
		Name: registryVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: e.cfg.Secret,
				Items: []corev1.KeyToPath{
					// A docker-registry Secret holds the config under this key,
					// and kaniko reads it as config.json.
					{Key: ".dockerconfigjson", Path: "config.json"},
				},
			},
		},
	}}
}

const (
	// registryVolumeName is the volume holding the registry credential, and the
	// name buildContainer mounts it under.
	registryVolumeName = "docker-config"

	// kanikoDockerConfigDir is where kaniko looks for a Docker config.
	//
	// It is the image's own DOCKER_CONFIG, which the chart's documentation and
	// kaniko's Dockerfile both name: /kaniko/.docker. Overriding it would mean
	// carrying a variable that has to agree with a mount path, and the image's
	// default is the one thing about the path that is not AppLab's to choose.
	//
	// The alternative — the standard /root/.docker, since kaniko runs as root —
	// would put a registry credential in a directory the build's own Dockerfile
	// can read, which is the same exposure the mount already carries and one more
	// place for it to be.
	kanikoDockerConfigDir = "/kaniko/.docker"
)

// buildContainer builds the image with kaniko and pushes it.
//
// One container, where this was two. The previous shape needed an init container
// to fetch a tarball and unpack it into a shared volume, because buildkit has no
// way to reach a repository itself; kaniko clones the git context directly, so
// the fetch step and the volume it fed both have nothing left to do.
//
// The clone is over the app's own git URL with the app's own key — see Start for
// why a single-use credential cannot work against a git clone.
func (e *Engine) buildContainer(app *model.App, branch, commitSHA, image, appKey string) corev1.Container {
	context, pullMethod := e.gitContext(app, branch, commitSHA)

	args := []string{
		"--context", context,
		"--dockerfile", app.Dockerfile,
		"--destination", image,
	}

	// A build is read as text by a person looking for what failed, and kaniko's
	// default output is a redrawing terminal UI: escape codes, cursor movement,
	// and — in the log AppLab keeps — nothing that reads as a line. Text format
	// with timestamps is what a log viewer and a `grep` both want.
	args = append(args,
		"--log-format", "text",
		"--log-timestamp",
		"--verbosity", "info",
	)

	// A cluster-local registry commonly serves plain HTTP or uses a self-signed
	// certificate. Both have to be asked for explicitly, and both weaken the
	// guarantee that the image that arrives is the image that was pushed — which
	// is why this is a deliberate opt-in rather than a default.
	//
	// Pull and push are separate flags in kaniko: the four below cover both
	// directions, because the base images are pulled through the same registry
	// the result is pushed to, and one insecure registry that only half worked
	// would fail in the middle of a build rather than at the start.
	if e.cfg.InsecureRegistry {
		args = append(args, "--insecure", "--insecure-pull", "--skip-tls-verify", "--skip-tls-verify-pull")
	}

	if e.cfg.CacheRepoPrefix != "" {
		// The cache reference is worked out the same way the image is, so that a
		// registry too deep to hold one repository per app does not get a cache
		// reference it would reject — which would fail the build rather than
		// merely skipping the cache.
		//
		// Unlike buildkit's export/import pair, kaniko's cache is a repository of
		// its own, so the app goes into the repository name where the image puts
		// it in the tag.
		cacheRepo, _ := imageRef(e.cfg.CacheRepoPrefix, app.ID)
		args = append(args,
			"--cache=true",
			"--cache-repo="+cacheRepo,
		)
	}

	// The registry credential is mounted rather than passed as an argument, and
	// the mount is only declared when there is one: a credential the registry
	// does not need would otherwise be a volume that has to exist.
	mounts := []corev1.VolumeMount{}
	if e.cfg.Secret != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      registryVolumeName,
			MountPath: kanikoDockerConfigDir,
			ReadOnly:  true,
		})
	}

	container := corev1.Container{
		Name:  "build",
		Image: e.cfg.KanikoImage,
		Args:  args,
		Env: []corev1.EnvVar{
			// The app's own key, as the Basic password git sends.
			//
			// GIT_USERNAME is the app id rather than empty because a Basic
			// credential with no username is not sent at all — AppLab reads the
			// password and ignores the username, but something has to be in it
			// for the header to exist. See internal/auth.
			{Name: "GIT_USERNAME", Value: app.ID},
			{Name: "GIT_PASSWORD", Value: appKey},
			// Which scheme kaniko prepends to the context URL. It has to agree
			// with the address the context was built from, so both come out of
			// the same call below.
			{Name: "GIT_PULL_METHOD", Value: pullMethod},
		},
		VolumeMounts: mounts,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resourcePtr(orDefault(e.cfg.BuildCPURequest, "500m")),
				corev1.ResourceMemory: resourcePtr(orDefault(e.cfg.BuildMemoryRequest, "1Gi")),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:              resourcePtr(orDefault(e.cfg.BuildCPULimit, "4")),
				corev1.ResourceMemory:           resourcePtr(orDefault(e.cfg.BuildMemoryLimit, "8Gi")),
				corev1.ResourceEphemeralStorage: resourcePtr(orDefault(e.cfg.WorkspaceSizeLimit, "10Gi")),
			},
		},
	}

	// Kaniko unpacks the base image and every layer it builds into its own root
	// filesystem, and it runs the Dockerfile's RUN steps as root — there is no
	// unprivileged mode, which is the one thing this change gives up. The
	// container is therefore not given a runAsUser, and the chart's documentation
	// says so where it used to promise rootless builds.
	//
	// The root filesystem is writable because kaniko unpacks into it, and the
	// ephemeral-storage limit above is what bounds it: the writable layer grows
	// with the image being built, and the limit is the ceiling that keeps one
	// build from filling the node's disk.
	container.SecurityContext = &corev1.SecurityContext{
		ReadOnlyRootFilesystem: ptr(false),
	}

	return container
}

// gitContext renders the --context argument and the pull method that goes with
// it: the app's repository, a branch, and exactly one commit.
//
// The three "#"-separated parts are kaniko's own syntax, read by
// pkg/buildcontext/git.go. The first is a URL without a scheme — kaniko prepends
// one from GIT_PULL_METHOD — and the second is what to clone.
//
// The clone names a full ref, "refs/heads/<branch>", rather than a bare branch
// name, and the two are not the same thing to kaniko. A full ref is matched
// literally, so a branch called "v1" is cloned as a branch and never confused
// with a tag of the same name — kaniko resolves a bare name against branches
// first and tags second, which is the ambiguity this avoids. A branch name that
// is also a valid tag is not hypothetical, and a build that silently used the
// tag would build the wrong source at the right commit SHA only by accident.
//
// The third part is the commit, and it is what makes the build reproducible: a
// build is started for a recorded commit, and by the time it runs the branch may
// have moved. kaniko clones the branch and then checks this commit out, so the
// image that is built is the one that was asked for.
//
// The URL carries no scheme because kaniko reads the method from the
// environment. It is derived from AppLab's own address rather than assumed:
// GIT_PULL_METHOD only knows "http" and "https" — anything else silently becomes
// https — and an AppLab reached over http in-cluster is the cluster-local case
// the chart already supports, so the scheme it is reached at is the scheme the
// build must use.
func (e *Engine) gitContext(app *model.App, branch, commitSHA string) (context, pullMethod string) {
	host := e.cfg.AppLabURL

	pullMethod = "https"
	if after, ok := strings.CutPrefix(host, "http://"); ok {
		host, pullMethod = after, "http"
	} else {
		host = strings.TrimPrefix(host, "https://")
	}

	// Trailing slashes are trimmed rather than refused: base_url is configured by
	// hand in the chart, and "https://example.com/" is the same address as
	// "https://example.com". The path is then concatenated, so a leftover slash
	// would produce "//git/...", which kaniko's clone would treat as a URL with an
	// empty first path segment.
	host = strings.TrimSuffix(host, "/")

	// Returned rather than set as an environment variable here, so that the URL and
	// the method it is fetched with cannot disagree: they are two halves of one
	// answer, and a https URL fetched as http fails as a connection error that
	// names neither.
	return "git://" + host + "/git/" + app.ID + ".git" +
		"#refs/heads/" + branch +
		"#" + commitSHA, pullMethod
}

// Status reads a build Job's current state.
func (e *Engine) Status(ctx context.Context, namespace, jobName string) (model.BuildStatus, string, error) {
	job, err := e.client.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The Job is gone — most likely cleaned up by its TTL after finishing.
			// Reporting "failed" would be wrong and alarming; the recorded status
			// in AppLab's own state is the better answer, so this is surfaced as
			// an absence rather than a verdict.
			return "", "the build job no longer exists; it was probably cleaned up after finishing", nil
		}
		return "", "", fmt.Errorf("read build job %s: %w", jobName, err)
	}

	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobComplete:
			return model.BuildStatusSucceeded, condition.Message, nil
		case batchv1.JobFailed:
			return model.BuildStatusFailed, e.failureReason(ctx, namespace, jobName, condition), nil
		}
	}

	if job.Status.Active > 0 {
		return model.BuildStatusRunning, "", nil
	}

	// No condition and nothing active: the Job was created but its pod has not
	// started yet.
	return model.BuildStatusPending, "", nil
}

// failureReason says why a build failed, in as much detail as the cluster can
// still be asked for.
//
// The Job's own message is written for an operator reading `kubectl describe`,
// and for the failure that matters most it says almost nothing: a build whose
// first attempt is evicted or fails to start reads exactly as
//
//	BackoffLimitExceeded: Job has reached the specified backoff limit
//
// which names the count, not the cause. The cause is on the pod — an eviction in
// its status, a failed image pull on its container, a non-zero exit on the
// container that ran — and so is the one line of the log that usually explains
// it outright.
//
// So the pod is consulted, and the result is the Job's message with what was
// found appended. Appending rather than replacing is deliberate: the pod may be
// gone by the time this runs (the Job's TTL collects it), and the Job's message
// is then the whole answer, so it has to stay readable on its own.
func (e *Engine) failureReason(ctx context.Context, namespace, jobName string, condition batchv1.JobCondition) string {
	base := strings.TrimSpace(condition.Reason + ": " + condition.Message)

	detail := e.podFailureDetail(ctx, namespace, jobName)
	if detail == "" {
		return base
	}
	return base + " — " + detail
}

// podFailureDetail describes why the build's pod stopped.
//
// Every read here is best-effort: this runs while a failure is being reported,
// and a cluster that will not answer is not a second failure worth surfacing —
// the caller gets the Job's own message, which is what it would have got before
// this existed.
func (e *Engine) podFailureDetail(ctx context.Context, namespace, jobName string) string {
	pods, err := e.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}

	// The most recent pod, for the same reason Logs reads the most recent one: a
	// Job that exhausted its backoff ran several, and the later attempt is the
	// one nearer the cause. (The first attempt's own reason is repeated on the
	// Job's message when that is what happened, so nothing is lost.)
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].CreationTimestamp.Before(&pods.Items[j].CreationTimestamp)
	})
	pod := &pods.Items[len(pods.Items)-1]

	// A pod that never ran says why on itself — an eviction, a scheduling
	// failure, a node that went away.
	if pod.Status.Reason != "" {
		return pod.Name + ": " + pod.Status.Reason + ": " + pod.Status.Message
	}

	var parts []string
	for _, status := range pod.Status.InitContainerStatuses {
		if line := containerFailure(status); line != "" {
			parts = append(parts, "init "+line)
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if line := containerFailure(status); line != "" {
			parts = append(parts, line)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return pod.Name + " ended: " + strings.Join(parts, "; ")
}

// containerFailure describes how one container ended, preferring the instance
// that failed over the one that is merely waiting.
//
// A container in a crash loop is Waiting with "CrashLoopBackOff", which is the
// state and not the cause; the cause is on the previous instance — "Error", exit
// code 1, "OOMKilled". That ordering is the same one internal/observe uses for
// an app's pods, and for the same reason.
func containerFailure(status corev1.ContainerStatus) string {
	if status.State.Terminated != nil {
		return describeTermination(status.Name, status.State.Terminated)
	}
	if status.LastTerminationState.Terminated != nil {
		return describeTermination(status.Name, status.LastTerminationState.Terminated)
	}
	if status.State.Waiting != nil {
		line := status.Name + " is waiting: " + status.State.Waiting.Reason
		if status.State.Waiting.Message != "" {
			line += ": " + status.State.Waiting.Message
		}
		return line
	}
	return ""
}

// describeTermination renders one container's exit.
func describeTermination(name string, term *corev1.ContainerStateTerminated) string {
	line := name + " exited " + fmt.Sprintf("%d", term.ExitCode)
	if term.Reason != "" {
		line += " (" + term.Reason + ")"
	}
	if term.Message != "" {
		line += ": " + term.Message
	}
	return line
}

// Logs returns the build's log output.
//
// When a build failed, the log is read from the pod that actually ran rather
// than from the container by name, because a Job's pod is what holds it and the
// Job itself outlives the pod only until its TTL.
func (e *Engine) Logs(ctx context.Context, namespace, jobName string, tailLines int64) (string, error) {
	pods, err := e.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil {
		return "", fmt.Errorf("list build pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pod for build job %s; it may not have started yet", jobName)
	}

	// The most recently created pod is the one that ran: a retried Job has more
	// than one, and the earlier attempts are the ones that failed.
	pod := pods.Items[0]
	for _, candidate := range pods.Items[1:] {
		if candidate.CreationTimestamp.After(pod.CreationTimestamp.Time) {
			pod = candidate
		}
	}

	// Every container the pod declares, under headings naming which is which.
	//
	// A build is one container now — kaniko does the cloning and the building —
	// so the headings are usually a single line. They are kept because the list
	// comes from the pod rather than from this code's idea of a build: if the
	// pod ever carries more than one container, the streams are separated rather
	// than run together, and a reader is told which output came from where.
	var out strings.Builder
	for _, container := range buildContainers(&pod) {
		text, err := e.containerLog(ctx, namespace, pod.Name, container, tailLines)
		if err != nil {
			// One container's log being unavailable does not discard the other's,
			// which is the half most likely to be readable.
			fmt.Fprintf(&out, "=== %s ===\n[could not read this container's log: %v]\n", container, err)
			continue
		}
		fmt.Fprintf(&out, "=== %s ===\n%s\n", container, strings.TrimRight(text, "\n"))
	}
	return out.String(), nil
}

// buildContainers names the containers to read, in order, from what the pod
// actually declares.
//
// From the pod rather than from the Job spec: this runs against a pod that
// exists, and a Job whose spec has since been edited would otherwise have this
// ask for a container that is not there.
func buildContainers(pod *corev1.Pod) []string {
	names := make([]string, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	for _, c := range pod.Spec.InitContainers {
		names = append(names, c.Name)
	}
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	return names
}

// containerLog reads one container's output, falling back to the previous
// instance when the current one has not written anything.
//
// The fallback is what makes a crash loop readable. A container that is being
// restarted has an empty current log and its cause in the instance that died —
// the same reason internal/observe reads previous for an app's pods.
func (e *Engine) containerLog(ctx context.Context, namespace, podName, container string, tailLines int64) (string, error) {
	read := func(previous bool) (string, error) {
		options := &corev1.PodLogOptions{Container: container, Previous: previous}
		if tailLines > 0 {
			options.TailLines = &tailLines
		}
		stream, err := e.client.CoreV1().Pods(namespace).GetLogs(podName, options).Stream(ctx)
		if err != nil {
			return "", err
		}
		defer stream.Close()

		var buf strings.Builder
		if _, err := copyToBuilder(&buf, stream); err != nil {
			return buf.String(), err
		}
		return buf.String(), nil
	}

	text, err := read(false)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(text) != "" {
		return text, nil
	}
	// Nothing this time round. If there is a previous instance, its output is
	// what explains the restart; if there is not, the empty current log is the
	// honest answer and this returns it.
	if previous, prevErr := read(true); prevErr == nil && strings.TrimSpace(previous) != "" {
		return "[the current instance has written nothing; this is the previous instance]\n" + previous, nil
	}
	return text, nil
}

// Cancel deletes a build Job, stopping the build.
func (e *Engine) Cancel(ctx context.Context, namespace, jobName string) error {
	// Foreground propagation so the pods are gone before the Job is, which is
	// what makes a subsequent build start cleanly.
	policy := metav1.DeletePropagationForeground

	err := e.client.BatchV1().Jobs(namespace).Delete(ctx, jobName, metav1.DeleteOptions{
		PropagationPolicy: &policy,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete build job %s: %w", jobName, err)
	}

	return nil
}

// shortSHA is the abbreviated commit used as an image tag.
//
// Twelve characters is git's own default abbreviation and is comfortably beyond
// any realistic collision within a single app's history.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// orDefault returns v, or def when v is empty.
func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// ptr returns a pointer to a value, for the many Kubernetes fields that are
// pointers so that "unset" is distinguishable from "zero".
func ptr[T any](v T) *T { return &v }

// resourcePtr parses a Kubernetes quantity, panicking on a malformed one.
//
// A panic is right here: every value it is given comes from AppLab's own
// configuration, checked at boot, so a bad one is a programming error rather
// than a caller's mistake. The alternative — returning an error — would thread a
// failure through the Job spec builder that no caller could act on.
func resourcePtr(s string) resource.Quantity {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		panic(fmt.Sprintf("invalid resource quantity %q: %v", s, err))
	}
	return q
}

// copyToBuilder copies r into a strings.Builder.
//
// io.Copy is not used directly because a strings.Builder is not an io.Writer,
// and the log is small enough that holding it is simpler than streaming it —
// the streaming path is the SSE endpoint, which reads from the pod directly.
func copyToBuilder(b *strings.Builder, r io.Reader) (int64, error) {
	return io.Copy(writerFunc(func(p []byte) (int, error) {
		return b.Write(p)
	}), r)
}

// writerFunc adapts a function to io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
