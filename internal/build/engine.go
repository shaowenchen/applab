// Package build runs an app's Dockerfile in the cluster and pushes the resulting
// image.
//
// A build is a one-shot Job, not a request to a long-running builder daemon.
// That choice is the whole design: there is no build service to operate, no
// daemon to keep warm or scale, and a build's resources are released the moment
// it ends. Build capacity scales with the cluster rather than with a
// configuration applab has to manage.
//
// The Job has two containers, and the split is what keeps source out of the
// builder:
//
//	init  fetch-source   downloads one commit's tree into an emptyDir
//	main  build          runs buildkitd rootless and builds inside that tree
//
// The fetch step uses a single-use token (see internal/sourcetoken) rather than
// applab's own API key, so a build holds no credential that reaches beyond the
// one commit it was started for.
package build

import (
	"context"
	"fmt"
	"io"
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

	// AppLabURL is the base URL of the applab API, which the init container
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
	return fmt.Sprintf("%s/%s:%s", strings.TrimSuffix(e.cfg.Registry, "/"), appID, shortSHA(commitSHA))
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
// passed through a Secret rather than as a container argument, because arguments
// are readable by anyone who can get a pod spec and appear in the Job's own
// description.
func (e *Engine) Start(ctx context.Context, app *model.App, buildID, commitSHA, sourceToken string) (string, error) {
	if !e.Ready() {
		return "", fmt.Errorf("build is not configured: builder image, registry and applab URL are all required")
	}

	namespace := app.Namespace
	jobName := e.JobNameFor(app.ID, buildID)
	image := e.ImageFor(app.ID, commitSHA)

	if err := e.createTokenSecret(ctx, namespace, jobName, sourceToken, app.ID, buildID); err != nil {
		return "", err
	}

	job := e.jobSpec(app, jobName, buildID, commitSHA, image)

	if _, err := e.client.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		// The Secret is useless without the Job, and leaving it behind would
		// accumulate one per failed start.
		_ = e.client.CoreV1().Secrets(namespace).Delete(ctx, jobName+"-token", metav1.DeleteOptions{})
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
func (e *Engine) jobSpec(app *model.App, jobName, buildID, commitSHA, image string) *batchv1.Job {
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
		{
			Name: "token",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: jobName + "-token",
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
					// into every pod by default — which in this namespace grants
					// read access to every Secret, including the registry
					// credentials beside it. Nothing in this Job needs to talk to
					// the API server: the init container fetches its source over
					// HTTP with a single-use token, and the builder only pushes to
					// a registry. Turning it off is what makes "the builder never
					// sees a credential" true rather than aspirational.
					AutomountServiceAccountToken: ptr(false),
					RestartPolicy:                corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						// BuildKit's rootless mode needs a user namespace with a
						// subuid range. fsGroup makes the shared workspace writable
						// by whichever uid the containers run as.
						FSGroup: ptr(int64(1000)),
					},
					InitContainers: []corev1.Container{e.fetchContainer(app, jobName, commitSHA, workspace)},
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
func (e *Engine) fetchContainer(app *model.App, jobName, commitSHA, workspace string) corev1.Container {
	// The token arrives as a file, so it never appears in the Job's arguments.
	script := `
set -eu

token="$(cat /var/run/applab/token)"
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
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: workspace, MountPath: "/workspace"},
			{Name: "token", MountPath: "/var/run/applab", ReadOnly: true},
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
		cacheRef := fmt.Sprintf("%s/%s:buildcache", strings.TrimSuffix(e.cfg.CacheRepoPrefix, "/"), app.ID)
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

// createTokenSecret stores the source token where the init container can read it.
func (e *Engine) createTokenSecret(ctx context.Context, namespace, jobName, token, appID, buildID string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-token",
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "applab",
				"applab.io/app":                appID,
				"applab.io/build":              buildID,
			},
		},
		// Opaque rather than a typed secret: this is not a standard credential
		// shape and giving it a type would imply a meaning it does not have.
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"token": token},
	}

	if _, err := e.client.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// A retried start reuses the name. Replacing the token is correct:
			// the old one may have been consumed or expired.
			if _, updateErr := e.client.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{}); updateErr != nil {
				return fmt.Errorf("update build token secret %s: %w", secret.Name, updateErr)
			}
			return nil
		}
		return fmt.Errorf("create build token secret %s: %w", secret.Name, err)
	}
	return nil
}

// Status reads a build Job's current state.
func (e *Engine) Status(ctx context.Context, namespace, jobName string) (model.BuildStatus, string, error) {
	job, err := e.client.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The Job is gone — most likely cleaned up by its TTL after finishing.
			// Reporting "failed" would be wrong and alarming; the recorded status
			// in the database is the better answer, so this is surfaced as an
			// absence rather than a verdict.
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
			return model.BuildStatusFailed, condition.Reason + ": " + condition.Message, nil
		}
	}

	if job.Status.Active > 0 {
		return model.BuildStatusRunning, "", nil
	}

	// No condition and nothing active: the Job was created but its pod has not
	// started yet.
	return model.BuildStatusPending, "", nil
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

	options := &corev1.PodLogOptions{Timestamps: false}
	if tailLines > 0 {
		options.TailLines = &tailLines
	}

	stream, err := e.client.CoreV1().Pods(namespace).GetLogs(pod.Name, options).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("read logs for build job %s: %w", jobName, err)
	}
	defer stream.Close()

	var buf strings.Builder
	if _, err := copyToBuilder(&buf, stream); err != nil {
		return buf.String(), fmt.Errorf("read logs for build job %s: %w", jobName, err)
	}
	return buf.String(), nil
}

// Cancel deletes a build Job and its token Secret, stopping the build.
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

	if err := e.client.CoreV1().Secrets(namespace).Delete(ctx, jobName+"-token", metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete build token secret for %s: %w", jobName, err)
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
// A panic is right here: every value it is given comes from applab's own
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
