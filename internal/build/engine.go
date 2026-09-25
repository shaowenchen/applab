// Package build runs an app's Dockerfile in the cluster and pushes the resulting
// image.
//
// A build is a one-shot Job, not a request to a long-running builder daemon.
// That choice is the whole design: there is no build service to operate, no
// daemon to keep warm or scale, and a build's resources are released the moment
// it ends. Build capacity scales with the cluster rather than with a
// configuration AppLab has to manage.
//
// The Job has two containers, and the split is what keeps source out of the
// builder:
//
//	init  fetch-source   downloads one commit's tree into an emptyDir
//	main  build          runs buildkitd rootless and builds inside that tree
//
// The fetch step uses a single-use token (see internal/sourcetoken) rather than
// AppLab's own API key, so a build holds no credential that reaches beyond the
// one commit it was started for.
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
	// BuilderImage is the image providing `buildctl` and, when rootless mode is
	// on, running `buildkitd` itself. It must contain both.
	BuilderImage string

	// FetcherImage is the image the init container runs. It needs a shell, curl
	// and tar — nothing more, since it only downloads and unpacks.
	FetcherImage string

	// Registry is the prefix an app's image is pushed under, e.g.
	// "registry.example.com/apps". The image name is "<registry>/<app>".
	Registry string

	// PushSecret names a Secret in each app namespace holding a
	// .dockerconfigjson for the registry. Empty means the registry needs no
	// credentials, which is normal for a cluster-local registry.
	PushSecret string

	// InsecureRegistry allows pushing over plain HTTP and skipping TLS
	// verification.
	//
	// This exists for cluster-local registries that serve HTTP or use a
	// self-signed certificate. It is off by default because it disables the
	// verification that makes a push trustworthy, and it must be enabled
	// deliberately.
	InsecureRegistry bool

	// BuildKitImage is the image whose `buildkitd` runs, when RootlessBuildKit is
	// on. Kept separate from BuilderImage because the daemon and the client are
	// conventionally different images, though they may be the same one.
	BuildKitImage string

	// Rootless runs BuildKit as an unprivileged user.
	//
	// This is the default and the recommended mode, for the reason git's own
	// Kubernetes examples give: a privileged build container is a container
	// breakout away from the node, and a build runs arbitrary code from whoever
	// pushed the source. Rootless needs kernel support for unprivileged user
	// namespaces (see the chart's README for the prerequisites); Privileged is
	// the escape hatch for a cluster that does not have it.
	Rootless bool

	// AppLabURL is the base URL of the AppLab API, which the init container
	// fetches source from.
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
		// pods do not accumulate.
		cfg.TTLAfterFinished = 24 * time.Hour
	}
	if cfg.WorkspaceSizeLimit == "" {
		// A source tree plus BuildKit's intermediate state. Generous, but bounded:
		// without a limit a build that expands something enormous fills the node's
		// disk and takes other workloads down with it.
		cfg.WorkspaceSizeLimit = "10Gi"
	}
	return &Engine{client: client, cfg: cfg}
}

// Ready reports whether the build half is configured well enough to run.
func (e *Engine) Ready() bool {
	return e.cfg.BuilderImage != "" && e.cfg.Registry != "" && e.cfg.AppLabURL != ""
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
// sourceToken grants the init container access to exactly that commit. It is
// passed as an environment variable rather than as a container argument, because
// arguments appear in the process listing of the running container and in the
// Job's own description.
func (e *Engine) Start(ctx context.Context, app *model.App, buildID, commitSHA, sourceToken string) (string, error) {
	if !e.Ready() {
		return "", fmt.Errorf("build is not configured: builder image, registry and applab URL are all required")
	}

	namespace := app.Namespace
	jobName := e.JobNameFor(app.ID, buildID)
	image := e.ImageFor(app.ID, commitSHA)

	job := e.jobSpec(app, jobName, buildID, commitSHA, image, sourceToken)

	if _, err := e.client.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		// Nothing to roll back: the token is an environment variable on the Job
		// rather than an object beside it, so a start that fails leaves nothing
		// behind.
		if apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("a build job named %s already exists", jobName)
		}
		return "", fmt.Errorf("create build job %s: %w", jobName, err)
	}

	return jobName, nil
}

// jobSpec builds the Job.
//
// It is separated from Start so it can be rendered and inspected in a test
// without a cluster, which is the only way to check the security context and
// volume wiring on a laptop.
func (e *Engine) jobSpec(app *model.App, jobName, buildID, commitSHA, image, sourceToken string) *batchv1.Job {
	namespace := app.Namespace
	workspace := "workspace"

	ttl := int32(e.cfg.TTLAfterFinished.Seconds())
	deadline := int64(e.cfg.ActiveDeadline.Seconds())
	backoff := int32(1)

	labels := map[string]string{
		"app.kubernetes.io/managed-by": "applab",
		"applab.io/app":                app.ID,
		"applab.io/build":              buildID,
	}

	volumes := []corev1.Volume{
		{
			Name: workspace,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: ptr(resourcePtr(e.cfg.WorkspaceSizeLimit)),
				},
			},
		},
	}
	registryVolumes, _ := e.registryVolumes()
	volumes = append(volumes, registryVolumes...)

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
					// Dockerfile, and Kubernetes mounts a service account token
					// into every pod by default, which in this namespace is read
					// access to whatever AppLab's Role grants. Nothing in this Job
					// needs to talk to the API server: the init container fetches
					// its source over HTTP with a single-use token, and the
					// builder only pushes to a registry.
					AutomountServiceAccountToken: ptr(false),
					RestartPolicy:                corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						// BuildKit's rootless mode needs a user namespace with a
						// subuid range. fsGroup makes the shared workspace writable
						// by whichever uid the containers run as.
						FSGroup: ptr(int64(1000)),
					},
					InitContainers: []corev1.Container{e.fetchContainer(app, jobName, commitSHA, workspace, sourceToken)},
					Containers:     []corev1.Container{e.buildContainer(app, jobName, buildID, commitSHA, image, workspace)},
					Volumes:        volumes,
				},
			},
		},
	}
}

// registryVolumes returns the volumes needed to authenticate to the registry.
//
// It lives apart from jobSpec so the mount in buildContainer and the declaration
// here cannot drift: a mount without a volume is a pod that never starts, and the
// error names neither.
func (e *Engine) registryVolumes() ([]corev1.Volume, []corev1.VolumeMount) {
	if e.cfg.PushSecret == "" {
		return nil, nil
	}

	volume := corev1.Volume{
		Name: "docker-config",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: e.cfg.PushSecret,
				Items: []corev1.KeyToPath{
					{Key: ".dockerconfigjson", Path: "config.json"},
				},
			},
		},
	}
	// BuildKit reads the standard Docker config location, so the secret's
	// .dockerconfigjson is projected as config.json and DOCKER_CONFIG points at
	// the directory.
	mount := corev1.VolumeMount{
		Name:      "docker-config",
		MountPath: "/home/user/.docker",
		ReadOnly:  true,
	}
	return []corev1.Volume{volume}, []corev1.VolumeMount{mount}
}

// fetchContainer downloads the commit's source into the shared workspace.
//
// It is a POSIX shell over curl and tar, rather than a purpose-built binary,
// because that is all the step is: one authenticated GET, piped into tar. Keeping
// it minimal means the init image is small and its behaviour is inspectable from
// the Job spec.
func (e *Engine) fetchContainer(app *model.App, jobName, commitSHA, workspace, token string) corev1.Container {
	// The token arrives as an environment variable, so it is not in the Job's
	// arguments — which are the thing that ends up in a process listing and in
	// the shell history of anything that copied the command.
	//
	// It used to arrive as a mounted Secret, so that no value appeared in the
	// pod spec at all. AppLab no longer uses Secret objects, and the exposure
	// this leaves is small by construction: the token is single-use, names one
	// commit, and expires in a minute.
	script := `
set -eu

token="${APPLAB_SOURCE_TOKEN}"
url="${APPLAB_URL}/api/v1/apps/${APPLAB_APP}/source/archive/${APPLAB_COMMIT}"

echo "fetching source for ${APPLAB_APP} at ${APPLAB_COMMIT}"

# --fail so an HTTP error is a failure rather than an HTML error page written
# into the tar stream, which would fail later with a confusing message.
curl --fail --silent --show-error --location \
  -H "Authorization: Bearer ${token}" \
  -o /workspace/source.tar.gz \
  "${url}"

# -C /workspace so the archive's own relative paths land in the workspace and
# cannot be interpreted relative to the root.
tar -xzf /workspace/source.tar.gz -C /workspace/source

rm -f /workspace/source.tar.gz

# An archive with no Dockerfile is a build that cannot start. Failing here names
# the actual problem, where letting it through would surface as an obscure
# buildctl error.
if [ ! -f "/workspace/source/${APPLAB_DOCKERFILE}" ]; then
  echo "no Dockerfile at '${APPLAB_DOCKERFILE}' in the uploaded source" >&2
  exit 1
fi

echo "source ready: $(find /workspace/source -type f | wc -l) files"
`

	return corev1.Container{
		Name:    "fetch-source",
		Image:   e.cfg.FetcherImage,
		Command: []string{"/bin/sh", "-c"},
		Args:    []string{script},
		Env: []corev1.EnvVar{
			{Name: "APPLAB_URL", Value: e.cfg.AppLabURL},
			{Name: "APPLAB_APP", Value: app.ID},
			{Name: "APPLAB_COMMIT", Value: commitSHA},
			{Name: "APPLAB_DOCKERFILE", Value: app.Dockerfile},
			{Name: "APPLAB_SOURCE_TOKEN", Value: token},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: workspace, MountPath: "/workspace"},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             ptr(true),
			RunAsUser:                ptr(int64(1000)),
			AllowPrivilegeEscalation: ptr(false),
			ReadOnlyRootFilesystem:   ptr(false),
			Capabilities:             &corev1.Capabilities{},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resourcePtr("100m"),
				corev1.ResourceMemory: resourcePtr("128Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resourcePtr("1"),
				corev1.ResourceMemory: resourcePtr("512Mi"),
			},
		},
	}
}

// buildContainer runs buildkitd rootless and builds the image.
func (e *Engine) buildContainer(app *model.App, jobName, buildID, commitSHA, image, workspace string) corev1.Container {
	buildctlArgs := []string{
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=/workspace/source",
		"--local", "dockerfile=/workspace/source",
		"--opt", "filename=" + app.Dockerfile,
		"--progress", "plain",
	}

	// A cluster-local registry commonly serves plain HTTP or uses a self-signed
	// certificate. Both have to be asked for explicitly, and both weaken the
	// guarantee that the image that arrives is the image that was pushed — which
	// is why this is a deliberate opt-in rather than a default.
	if e.cfg.InsecureRegistry {
		buildctlArgs = append(buildctlArgs,
			"--opt", "registry.insecure=true",
		)
	}

	// The push output goes last so the insecure option, which is per-registry,
	// is already in effect when it is parsed.
	buildctlArgs = append(buildctlArgs, "--output", "type=image,name="+image+",push=true")

	// Registry-side caching. Import is best-effort because the first build of an
	// app has no cache to import, and a missing manifest is not a failure.
	if e.cfg.CacheRepoPrefix != "" {
		// The cache reference is worked out the same way the image is, so that a
		// registry too deep to hold one repository per app does not get a cache
		// reference it would reject — which would fail the build rather than
		// merely skipping the cache.
		cacheRepo, cacheTagPrefix := imageRef(e.cfg.CacheRepoPrefix, app.ID)
		cacheRef := fmt.Sprintf("%s:%sbuildcache", cacheRepo, cacheTagPrefix)
		buildctlArgs = append(buildctlArgs,
			"--export-cache", "type=registry,ref="+cacheRef+",mode=max",
			"--import-cache", "type=registry,ref="+cacheRef,
		)
	}

	// A rootless daemon listens on the rootless socket path; a privileged one on
	// the default. Getting this wrong produces a "cannot connect" that says
	// nothing about the cause, so it is derived from the mode rather than set
	// independently.
	buildkitHost := "unix:///run/buildkit/buildkitd.sock"
	daemon := []string{
		"buildkitd",
		"--addr", "/run/buildkit/buildkitd.sock",
		"--oci-worker-no-process-sandbox",
	}

	script := ""
	if e.cfg.Rootless {
		// RootlessKit is what provides the userns mapping buildkitd needs. It is
		// started in the background and the build runs against the socket it
		// creates.
		script = `
set -eu

mkdir -p /home/user/.local/share/buildkit /run/buildkit

rootlesskit --state-dir=/run/buildkit/rootlesskit --net=host --mtu=65520 \
  --copy-up=/etc --copy-up=/run --propagation=rslave \
  buildkitd --addr /run/buildkit/buildkitd.sock --oci-worker-no-process-sandbox &

for i in $(seq 1 60); do
  if [ -S /run/buildkit/buildkitd.sock ]; then break; fi
  sleep 1
done
if [ ! -S /run/buildkit/buildkitd.sock ]; then
  echo "buildkitd did not start; see the prerequisites for rootless builds" >&2
  exit 1
fi

buildctl --addr unix:///run/buildkit/buildkitd.sock "$@"
`
	} else {
		// Privileged: buildkitd can run directly as root.
		script = `
set -eu

mkdir -p /run/buildkit

buildkitd --addr /run/buildkit/buildkitd.sock &

for i in $(seq 1 60); do
  if [ -S /run/buildkit/buildkitd.sock ]; then break; fi
  sleep 1
done
if [ ! -S /run/buildkit/buildkitd.sock ]; then
  echo "buildkitd did not start" >&2
  exit 1
fi

buildctl --addr unix:///run/buildkit/buildkitd.sock "$@"
`
	}

	container := corev1.Container{
		Name:    "build",
		Image:   e.cfg.BuilderImage,
		Command: []string{"/bin/sh", "-c"},
		Args:    append([]string{script, "build"}, buildctlArgs...),
		Env: []corev1.EnvVar{
			{Name: "BUILDKIT_HOST", Value: buildkitHost},
			// The build must not be able to read the token that fetched the
			// source: the fetcher's job is done, and a build runs arbitrary code
			// from the uploaded Dockerfile.
			{Name: "HOME", Value: "/home/user"},
			{Name: "BUILDKITD_FLAGS", Value: strings.Join(daemon, " ")},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: workspace, MountPath: "/workspace"},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resourcePtr(orDefault(e.cfg.BuildCPURequest, "500m")),
				corev1.ResourceMemory: resourcePtr(orDefault(e.cfg.BuildMemoryRequest, "1Gi")),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resourcePtr(orDefault(e.cfg.BuildCPULimit, "4")),
				corev1.ResourceMemory: resourcePtr(orDefault(e.cfg.BuildMemoryLimit, "8Gi")),
			},
		},
	}

	if e.cfg.Rootless {
		container.SecurityContext = &corev1.SecurityContext{
			RunAsNonRoot: ptr(true),
			RunAsUser:    ptr(int64(1000)),
			RunAsGroup:   ptr(int64(1000)),

			// RootlessKit has to create a user namespace and mount filesystems
			// inside it, which the default seccomp and AppArmor profiles block.
			// Unconfining them is what git's own rootless example does, and it is
			// safe precisely because the container is not privileged and runs as a
			// non-root user.
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
			AppArmorProfile:          &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
			AllowPrivilegeEscalation: ptr(true),
			ProcMount:                ptr(corev1.UnmaskedProcMount),
			ReadOnlyRootFilesystem:   ptr(false),
		}
	} else {
		container.SecurityContext = &corev1.SecurityContext{
			Privileged:               ptr(true),
			AllowPrivilegeEscalation: ptr(true),
			RunAsUser:                ptr(int64(0)),
			ReadOnlyRootFilesystem:   ptr(false),
		}
	}

	// Registry credentials, when the registry needs them. The mount comes from
	// registryVolumes so it cannot disagree with the volume declared in jobSpec.
	if _, mounts := e.registryVolumes(); len(mounts) > 0 {
		container.VolumeMounts = append(container.VolumeMounts, mounts...)
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  "DOCKER_CONFIG",
			Value: "/home/user/.docker",
		})
	}

	return container
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

	// Both containers, in the order they run, under headings naming which is
	// which.
	//
	// A build has two, and the one that explains a failure is usually the init
	// container: it fetches the source, so a revoked token, an unreachable
	// AppLab or a truncated download fails there — and that container's output
	// was simply not being read. An empty main-container result would then look
	// like a build that produced nothing, when the fetch never finished.
	//
	// The headings are load-bearing: two streams concatenated without them read
	// as one, and "failed to fetch source" followed by buildkit's startup banner
	// is a sequence nobody can interpret.
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
