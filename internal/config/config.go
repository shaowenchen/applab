// Package config loads AppLab's runtime configuration.
//
// Precedence, lowest to highest: built-in defaults, an optional YAML file named
// by APPLAB_CONFIG, then environment variables. Environment last is deliberate —
// in Kubernetes the environment is what an operator sets per deployment, and it
// should win over a file baked into the image.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/shaowenchen/applab/internal/objectstore"
)

// Config is the whole of AppLab's runtime configuration.
type Config struct {
	// Listen is the address the HTTP server binds, e.g. ":8080".
	Listen string `yaml:"listen"`

	// BaseURL is the address callers reach this service at, used wherever a URL
	// is handed back to a client (clone URLs). Empty means
	// derive it from the request's Host header, which is right on a cluster
	// fronted by an ingress and wrong the moment a proxy rewrites Host — so
	// setting it explicitly is the safer deployment.
	//
	// It includes BasePath when there is one: this is the whole address, not a
	// hostname.
	BaseURL string `yaml:"base_url"`

	// BasePath is the path prefix this service is served under, e.g. "/applab".
	//
	// It exists because of how a Kubernetes Ingress works: an Ingress routes on a
	// path but cannot strip one, so an AppLab served at "/applab" receives
	// requests for "/applab/api/v1/...". Without this the server would match
	// none of them and answer 404 to every request while the Ingress looked
	// correct.
	//
	// Empty means the root, which is the default and what a deployment reached at
	// its own hostname wants.
	BasePath string `yaml:"base_path"`

	// Keys are the API keys this deployment accepts. A key is the whole
	// identity: there is no user store, so a key that authenticates may do
	// anything. Empty is refused at boot rather than served as "open".
	Keys []string `yaml:"keys"`

	// DataDir is scratch space, and only that. It holds a repository while git
	// is running against it, and the parts of an upload that is still arriving.
	// Nothing durable is written here, so it needs no volume and survives no
	// restart: an empty directory on the node is enough.
	//
	// It is not where anything is kept. Every app, repository and key lives in
	// the object store; this is the working directory for the operations that
	// need a real filesystem, of which git is the only one.
	DataDir string `yaml:"data_dir"`

	// Object storage is where everything this service persists now lives: the
	// apps, their commit and build history, and their source repositories.
	ObjectStore ObjectStoreConfig `yaml:"object_store"`

	// LogLevel is one of debug, info, warn, error.
	LogLevel string `yaml:"log_level"`

	// Namespace is the namespace AppLab runs in, and the only one whose
	// resources it creates or touches. Every app is deployed into it too.
	//
	// One namespace rather than one per app is what lets AppLab hold a
	// namespaced Role instead of a ClusterRole: it has no permission anywhere
	// else in the cluster. The cost is that apps are not isolated from each
	// other by a namespace boundary, so objects are told apart by their
	// `applab.io/app` label instead.
	Namespace string `yaml:"namespace"`

	// Kubeconfig is an explicit kubeconfig path. Empty means use in-cluster
	// config, falling back to the ambient kubeconfig (which is what makes
	// `AppLab` runnable outside a cluster during development).
	Kubeconfig string `yaml:"kubeconfig"`

	// BaseDomain is the domain apps are exposed under. With no PathPrefix an
	// app with id "shop" is served at "shop.<BaseDomain>"; with one, every app
	// shares this host and is told apart by path instead. Empty means apps get
	// no hostname and are only reachable inside the cluster — a legitimate way
	// to run AppLab while its ingress is being decided.
	BaseDomain string `yaml:"base_domain"`

	// PathPrefix puts every app under one path on one host, so an app with id
	// "shop" is served at "<BaseDomain>/<PathPrefix>/shop". It is a prefix, not
	// a domain: it changes the route, and the host is shared by every app.
	//
	// This is the alternative to a wildcard DNS entry and a wildcard
	// certificate. Serve one host and one certificate, and let the path say
	// which app is meant. AppLab strips the prefix before the request reaches
	// the app, so an app sees the paths it would see if it were at the root.
	//
	// Must begin with "/" and must not end with one; empty means per-app
	// hostnames, the original behaviour.
	PathPrefix string `yaml:"path_prefix"`

	// MaxSimpleUpload is the largest source archive accepted in one request.
	// Anything larger must use the chunked endpoints, which is why it is
	// advertised: a client that discovers the limit by being rejected wastes a
	// whole upload to learn something the server could have told it.
	MaxSimpleUpload int64 `yaml:"max_simple_upload"`

	// ChunkSize is the part size the chunked upload endpoints advertise.
	ChunkSize int64 `yaml:"chunk_size"`

	// MaxChunkBytes is the ceiling a single part may not exceed, regardless of
	// what a client chose.
	MaxChunkBytes int64 `yaml:"max_chunk_bytes"`

	// Deploy configures how apps are exposed.
	Deploy Deploy `yaml:"deploy"`

	// Build configures the image build pipeline. It is a struct rather than
	// loose fields because the whole group is either configured or absent:
	// AppLab runs without any of it (the API and source halves still work) and
	// reports the build capability as unavailable.
	Build Build `yaml:"build"`
}

// ObjectStoreConfig points AppLab at the bucket it keeps everything in.
//
// A bucket is the only supported backing, in every deployment including a
// developer's: the point of the design is that a replica holds nothing, so a
// node can be replaced without anything being lost and more than one replica can
// serve at once — and a directory fallback quietly undid that in whichever
// deployment had a typo in its configuration.
//
// The credential is passed explicitly rather than read from the environment the
// way an AWS SDK would. AppLab has one credential, it comes from a Secret, and
// making it explicit is what keeps "there is no credential" a legible failure at
// startup instead of at the first write.
type ObjectStoreConfig struct {
	// Endpoint is the service's address: https://s3.us-east-1.amazonaws.com for
	// AWS, or a self-hosted service's own address.
	Endpoint string `yaml:"endpoint"`

	// Bucket is the bucket AppLab keeps everything in. It must already exist:
	// a bucket's name, region and lifecycle policy belong to whoever runs the
	// platform, and a service that created one on its own would create it
	// wherever it happened to be configured to.
	Bucket string `yaml:"bucket"`

	// Region is the bucket's region. Defaults to us-east-1.
	Region string `yaml:"region"`

	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`

	// PathStyle puts the bucket in the request path rather than in the hostname.
	// It is required by most self-hosted services, which have no wildcard DNS to
	// put a bucket under, and by any bucket name containing a dot, whose
	// virtual-hosted certificate would not match.
	PathStyle bool `yaml:"path_style"`

	// Prefix is an optional key prefix everything is written under, so one
	// bucket can hold more than one AppLab deployment.
	Prefix string `yaml:"prefix"`

	// Insecure allows an http endpoint. It is explicit rather than inferred from
	// the scheme, because sending a credential and an app's source in the clear
	// is a decision someone has to make on purpose.
	Insecure bool `yaml:"insecure"`
}

// Configured reports whether object storage has been pointed somewhere.
//
// Both are required, and a Config that fails this one is refused at boot. The
// check lives here rather than at the call sites so that "is this configured"
// has one answer: an endpoint with no bucket and a bucket with no endpoint are
// both nothing.
func (c ObjectStoreConfig) Configured() bool {
	return c.Endpoint != "" && c.Bucket != ""
}

// Deploy configures how apps are exposed in the cluster.
type Deploy struct {
	// Gateway is the Istio gateway apps are published through, as
	// "<namespace>/<name>". AppLab attaches VirtualServices to it; it does not
	// create it, because a gateway is shared cluster infrastructure with the
	// listeners and the certificate for the whole domain already on it.
	//
	// TLS is configured on the gateway, not here. An app is served over HTTPS
	// when the gateway has an HTTPS listener, without any per-app setting.
	Gateway string `yaml:"gateway"`

	// Annotations are added to every app VirtualService, for Istio specifics
	// that vary by cluster.
	Annotations map[string]string `yaml:"annotations"`

	// AppResources are the requests and limits applied to every app AppLab
	// deploys.
	//
	// They are the deployment's defaults rather than each app's own: an uploaded
	// app cannot be trusted to declare sane limits for itself, and one with no
	// limits at all can take its node down. An operator running AppLab for
	// several teams sets these per installation.
	AppCPURequest    string `yaml:"app_cpu_request"`
	AppMemoryRequest string `yaml:"app_memory_request"`
	AppCPULimit      string `yaml:"app_cpu_limit"`
	AppMemoryLimit   string `yaml:"app_memory_limit"`
}

// Build configures how images are built and where they are pushed.
type Build struct {
	// Registry is the prefix an app's image is pushed under, e.g.
	// "registry.example.com/apps". Empty disables building.
	Registry string `yaml:"registry"`

	// BuilderImage provides buildctl and buildkitd. Empty disables building.
	BuilderImage string `yaml:"builder_image"`

	// FetcherImage runs the init container that downloads the source. It needs a
	// shell, wget and tar — wget rather than curl, because the default is alpine
	// and busybox provides the first and not the second. See the fetch script in
	// internal/build for why that is not a free choice.
	FetcherImage string `yaml:"fetcher_image"`

	// Secret names the registry credential: the build Job mounts it to push,
	// and every app's Deployment references it to pull. One registry, one
	// credential, one name.
	//
	// Defaults to "applab", which is what the chart creates. An installation
	// that brings its own Secret sets this; an empty value means the registry
	// needs no authentication, which is normal for a cluster-local one.
	//
	// Not copied anywhere: apps run in AppLab's own namespace, so both readers
	// find it where it already is. A name that does not resolve is refused
	// before the Job or the Deployment is created — see build.Engine.Start and
	// Deployer.Apply.
	Secret string `yaml:"secret"`

	// Rootless runs BuildKit unprivileged. Defaults to true; see the chart
	// README for the kernel prerequisites a cluster must meet.
	Rootless *bool `yaml:"rootless"`

	// InsecureRegistry allows pushing over plain HTTP without TLS verification.
	// Off by default because it removes the guarantee that the image that
	// arrived is the image that was pushed.
	InsecureRegistry bool `yaml:"insecure_registry"`

	// CacheRepoPrefix enables registry-side layer caching under
	// "<prefix>/<app>:buildcache". Empty disables caching.
	CacheRepoPrefix string `yaml:"cache_repo_prefix"`

	// Resource requests and limits for the build container.
	CPURequest    string `yaml:"cpu_request"`
	MemoryRequest string `yaml:"memory_request"`
	CPULimit      string `yaml:"cpu_limit"`
	MemoryLimit   string `yaml:"memory_limit"`

	// WorkspaceSizeLimit bounds the ephemeral volume the source and BuildKit's
	// intermediate state share.
	WorkspaceSizeLimit string `yaml:"workspace_size_limit"`

	// Timeout is how long a single build may run before it is killed.
	Timeout time.Duration `yaml:"timeout"`

	// TTLAfterFinished is how long a finished build's Job is kept, so its log
	// can still be read. Zero would have the Job deleted the moment it ends,
	// which makes every failure undiagnosable — so it is left to the engine's
	// own default rather than being settable to nothing.
	TTLAfterFinished time.Duration `yaml:"ttl_after_finished"`
}

// RootlessBuild reports whether builds should run unprivileged, defaulting to
// yes.
func (b Build) RootlessBuild() bool {
	if b.Rootless == nil {
		return true
	}
	return *b.Rootless
}

// Enabled reports whether the build pipeline is configured.
func (b Build) Enabled() bool {
	return b.Registry != "" && b.BuilderImage != "" && b.FetcherImage != ""
}

// Default returns the configuration used when nothing is set. It is a working
// development configuration except for Keys, which is deliberately empty — see
// Load.
func Default() Config {
	return Config{
		Listen:    ":8080",
		DataDir:   "./data",
		LogLevel:  "info",
		Namespace: "ops-system",

		// 8 MiB matches the chunk size of the upload protocol this API borrows
		// its shape from, and is small enough to sit well inside the default
		// ingress body limit — a larger "simple" limit would mostly produce 413s
		// from the proxy in front, which arrive as HTML and are useless to a
		// client parsing JSON.
		MaxSimpleUpload: 8 << 20,
		ChunkSize:       8 << 20,
		MaxChunkBytes:   32 << 20,

		Deploy: Deploy{
			// The naming the official istio/gateway Helm chart produces: release
			// "istio-ingress" in namespace "istio-ingress". A cluster installed
			// with `istioctl install` names it istio-system/istio-ingressgateway
			// and has to say so.
			//
			// Defaulted rather than required because the gateway is almost
			// always one of those two, and a wrong-but-present default fails
			// loudly at the first deploy whereas an empty one fails at startup
			// with a message about a setting the operator did not know to look
			// for.
			Gateway: "istio-ingress/istio-ingress",

			// Bounded by default. An uploaded app with no limits can take its
			// node down, and the requests are small enough that an app which
			// needs more will be noticed rather than quietly starved.
			AppCPURequest:    "100m",
			AppMemoryRequest: "128Mi",
			AppCPULimit:      "2",
			AppMemoryLimit:   "2Gi",
		},

		Build: Build{
			// Pinned rather than "latest": a moving tag would make a build's
			// behaviour change without anything in AppLab changing, which is
			// exactly the kind of surprise a build system must not have.
			BuilderImage:  "moby/buildkit:v0.19.0",
			FetcherImage:  "alpine:3.21",
			CPURequest:    "500m",
			MemoryRequest: "1Gi",
			CPULimit:      "4",
			MemoryLimit:   "8Gi",

			// The default registry credential, named after the platform because
			// that is the only name it could have that an operator does not have
			// to be told. The chart creates it when one is configured; an
			// installation that brings its own Secret names it here.
			Secret: "applab",

			// A source tree plus BuildKit's intermediate state. Generous, but
			// bounded: an unbounded build can fill the node's disk and take
			// other workloads down with it.
			WorkspaceSizeLimit: "10Gi",

			Timeout: 30 * time.Minute,

			// A day is long enough to investigate a failure and short enough
			// that finished Jobs do not accumulate in the app's namespace.
			TTLAfterFinished: 24 * time.Hour,
		},
	}
}

// Load builds the configuration from defaults, an optional YAML file and the
// environment, then validates it.
//
// The YAML file is named by APPLAB_CONFIG and is optional; a path that was
// given but cannot be read is an error rather than a silent fallback, because
// a typo in a path should not quietly start a server with the wrong limits.
func Load() (Config, error) {
	cfg := Default()

	if path := strings.TrimSpace(os.Getenv("APPLAB_CONFIG")); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config file %s: %w", path, err)
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config file %s: %w", path, err)
		}
	}

	applyEnv(&cfg)

	if err := cfg.finalize(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// applyEnv overlays the environment onto cfg. Only variables that are set are
// applied, so an env var left unset keeps whatever the file or defaults gave.
func applyEnv(cfg *Config) {
	setString(&cfg.Listen, "APPLAB_LISTEN")
	setString(&cfg.BaseURL, "APPLAB_BASE_URL")
	setString(&cfg.BasePath, "APPLAB_BASE_PATH")
	setString(&cfg.DataDir, "APPLAB_DATA_DIR")
	setString(&cfg.ObjectStore.Endpoint, "APPLAB_OBJECT_STORE_ENDPOINT")
	setString(&cfg.ObjectStore.Bucket, "APPLAB_OBJECT_STORE_BUCKET")
	setString(&cfg.ObjectStore.Region, "APPLAB_OBJECT_STORE_REGION")
	setString(&cfg.ObjectStore.AccessKey, "APPLAB_OBJECT_STORE_ACCESS_KEY")
	setString(&cfg.ObjectStore.SecretKey, "APPLAB_OBJECT_STORE_SECRET_KEY")
	setString(&cfg.ObjectStore.Prefix, "APPLAB_OBJECT_STORE_PREFIX")
	setBool(&cfg.ObjectStore.PathStyle, "APPLAB_OBJECT_STORE_PATH_STYLE")
	setBool(&cfg.ObjectStore.Insecure, "APPLAB_OBJECT_STORE_INSECURE")
	setString(&cfg.LogLevel, "APPLAB_LOG_LEVEL")
	setString(&cfg.Namespace, "APPLAB_NAMESPACE")
	setString(&cfg.Kubeconfig, "APPLAB_KUBECONFIG")
	setString(&cfg.BaseDomain, "APPLAB_BASE_DOMAIN")
	setString(&cfg.PathPrefix, "APPLAB_PATH_PREFIX")
	setInt64(&cfg.MaxSimpleUpload, "APPLAB_MAX_SIMPLE_UPLOAD")
	setInt64(&cfg.ChunkSize, "APPLAB_CHUNK_SIZE")
	setInt64(&cfg.MaxChunkBytes, "APPLAB_MAX_CHUNK_BYTES")

	setString(&cfg.Build.Registry, "APPLAB_BUILD_REGISTRY")
	setString(&cfg.Build.BuilderImage, "APPLAB_BUILD_BUILDER_IMAGE")
	setString(&cfg.Build.FetcherImage, "APPLAB_BUILD_FETCHER_IMAGE")
	setString(&cfg.Build.Secret, "APPLAB_BUILD_SECRET")
	setString(&cfg.Build.CacheRepoPrefix, "APPLAB_BUILD_CACHE_REPO_PREFIX")
	setString(&cfg.Build.CPURequest, "APPLAB_BUILD_CPU_REQUEST")
	setString(&cfg.Build.MemoryRequest, "APPLAB_BUILD_MEMORY_REQUEST")
	setString(&cfg.Build.CPULimit, "APPLAB_BUILD_CPU_LIMIT")
	setString(&cfg.Build.MemoryLimit, "APPLAB_BUILD_MEMORY_LIMIT")
	setString(&cfg.Build.WorkspaceSizeLimit, "APPLAB_BUILD_WORKSPACE_SIZE_LIMIT")
	setBool(&cfg.Build.InsecureRegistry, "APPLAB_BUILD_INSECURE_REGISTRY")
	setBoolPtr(&cfg.Build.Rootless, "APPLAB_BUILD_ROOTLESS")
	setDuration(&cfg.Build.Timeout, "APPLAB_BUILD_TIMEOUT")
	setDuration(&cfg.Build.TTLAfterFinished, "APPLAB_BUILD_TTL_AFTER_FINISHED")

	setString(&cfg.Deploy.Gateway, "APPLAB_DEPLOY_GATEWAY")
	setString(&cfg.Deploy.AppCPURequest, "APPLAB_DEPLOY_APP_CPU_REQUEST")
	setString(&cfg.Deploy.AppMemoryRequest, "APPLAB_DEPLOY_APP_MEMORY_REQUEST")
	setString(&cfg.Deploy.AppCPULimit, "APPLAB_DEPLOY_APP_CPU_LIMIT")
	setString(&cfg.Deploy.AppMemoryLimit, "APPLAB_DEPLOY_APP_MEMORY_LIMIT")

	// Singular APPLAB_KEY is accepted alongside the plural form: a deployment
	// with one key (the common case) reads better as a single variable, and the
	// two are merged rather than one silently winning.
	if v, ok := os.LookupEnv("APPLAB_KEY"); ok && strings.TrimSpace(v) != "" {
		cfg.Keys = append(cfg.Keys, splitList(v)...)
	}
	if v, ok := os.LookupEnv("APPLAB_KEYS"); ok {
		cfg.Keys = append(cfg.Keys, splitList(v)...)
	}
}

// splitList parses a comma-separated list, dropping empty entries so a trailing
// comma does not become an empty-string key.
func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func setString(dst *string, env string) {
	if v, ok := os.LookupEnv(env); ok && strings.TrimSpace(v) != "" {
		*dst = strings.TrimSpace(v)
	}
}

// setBool applies a boolean environment variable, accepting the forms an
// operator would reasonably write.
func setBool(dst *bool, env string) {
	v, ok := os.LookupEnv(env)
	if !ok || strings.TrimSpace(v) == "" {
		return
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		*dst = true
	case "0", "false", "no", "off":
		*dst = false
	default:
		slog.Warn("ignoring invalid boolean environment variable; keeping the default",
			"name", env, "value", v)
	}
}

// setBoolPtr applies a boolean to a pointer field, so an unset variable leaves
// the field nil and a default applies downstream. That distinction matters for
// rootless builds: nil means "use the default", not "false".
func setBoolPtr(dst **bool, env string) {
	v, ok := os.LookupEnv(env)
	if !ok || strings.TrimSpace(v) == "" {
		return
	}
	var b bool
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		b = true
	case "0", "false", "no", "off":
		b = false
	default:
		slog.Warn("ignoring invalid boolean environment variable; keeping the default",
			"name", env, "value", v)
		return
	}
	*dst = &b
}

// setDuration applies a Go duration string such as "30m" or "90s".
func setDuration(dst *time.Duration, env string) {
	v, ok := os.LookupEnv(env)
	if !ok || strings.TrimSpace(v) == "" {
		return
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil || d <= 0 {
		slog.Warn("ignoring invalid duration environment variable; keeping the default",
			"name", env, "value", v)
		return
	}
	*dst = d
}

// setInt64 applies a numeric environment variable, rejecting a value that is not
// a positive number rather than silently accepting it. A typo in a size limit
// should be reported at boot, not discovered later as a mysterious 413.
//
// An unusable value is warned about and skipped, leaving the default in place.
// That is recoverable — the deployment still starts with working limits — and a
// warning in the log is where an operator will look after seeing odd behaviour.
func setInt64(dst *int64, env string) {
	v, ok := os.LookupEnv(env)
	if !ok || strings.TrimSpace(v) == "" {
		return
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n <= 0 {
		slog.Warn("ignoring invalid numeric environment variable; keeping the default",
			"name", env, "value", v)
		return
	}
	*dst = n
}

// finalize normalises and validates the configuration in place.
func (c *Config) finalize() error {
	c.Keys = dedupe(c.Keys)

	// A deployment with no key has nothing to authenticate against. Serving the
	// API openly in that case would be a far larger mistake than refusing to
	// start, so it is a boot error rather than a warning.
	if len(c.Keys) == 0 {
		return fmt.Errorf("no API keys configured: set APPLAB_KEY or APPLAB_KEYS to a non-empty comma-separated list")
	}

	if c.Namespace == "" {
		return fmt.Errorf("namespace must not be empty: AppLab would address the default namespace by accident, which the API server accepts silently")
	}

	// Everything AppLab persists lives in a bucket, so a deployment without one
	// has nowhere to put an app. Refused at boot rather than at the first write,
	// and refused here rather than in OpenObjectStore so that every caller of
	// Load gets the same answer — including one that never opens a store.
	if !c.ObjectStore.Configured() {
		return fmt.Errorf("no object storage configured: set object_store.endpoint and object_store.bucket (APPLAB_OBJECT_STORE_ENDPOINT and APPLAB_OBJECT_STORE_BUCKET). AppLab keeps every app, its source and its history in a bucket; there is no directory fallback")
	}

	if c.DataDir == "" {
		return fmt.Errorf("data_dir must not be empty")
	}

	// Resolve to absolute paths before anything else uses them: the directory is
	// also the root the git handlers validate against, and a relative root would
	// change meaning with the process's working directory.
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return fmt.Errorf("resolve data_dir %s: %w", c.DataDir, err)
	}
	c.DataDir = abs

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level %q is not one of debug, info, warn, error", c.LogLevel)
	}

	// A part larger than a simple upload makes the chunked path pointless: a
	// client would be told to split its upload and then be allowed to send it
	// whole anyway.
	if c.ChunkSize > c.MaxSimpleUpload {
		return fmt.Errorf("chunk_size (%d) must not exceed max_simple_upload (%d)", c.ChunkSize, c.MaxSimpleUpload)
	}
	if c.MaxChunkBytes < c.ChunkSize {
		return fmt.Errorf("max_chunk_bytes (%d) must be at least chunk_size (%d)", c.MaxChunkBytes, c.ChunkSize)
	}
	if c.MaxSimpleUpload <= 0 || c.ChunkSize <= 0 || c.MaxChunkBytes <= 0 {
		return fmt.Errorf("upload limits must be positive")
	}

	// A trailing dot or scheme in the base domain would produce hostnames that
	// do not resolve, which is a confusing way to find out about a typo.
	if d := strings.TrimSpace(c.BaseDomain); d != "" {
		if strings.Contains(d, "/") || strings.Contains(d, ":") {
			return fmt.Errorf("base_domain %q must be a bare domain, without a scheme, port or path", d)
		}
		c.BaseDomain = strings.Trim(d, ".")
	}

	// The path prefix is normalized rather than rejected for a stray slash:
	// "/apps/" and "apps" are both what someone would plausibly write meaning
	// the same thing, and refusing them teaches nothing. What is refused is a
	// value that cannot be a path at all.
	if p := strings.TrimSpace(c.PathPrefix); p != "" {
		if strings.ContainsAny(p, "?#") {
			return fmt.Errorf("path_prefix %q must be a path, without a query or fragment", p)
		}
		p = "/" + strings.Trim(p, "/")
		if strings.Contains(p, "//") {
			return fmt.Errorf("path_prefix %q has an empty segment", c.PathPrefix)
		}
		// Without a host there is nothing to hang the path on: the prefix is
		// the *only* thing distinguishing one app from another, so a deployment
		// with a prefix and no domain would have every app unreachable and no
		// hostname to report.
		if c.BaseDomain == "" {
			return fmt.Errorf("path_prefix is %q but base_domain is empty: the prefix distinguishes apps on a shared host, so there has to be a host. Set base_domain, or leave path_prefix empty", c.PathPrefix)
		}
		c.PathPrefix = p
	}

	// The base path is normalized the same way, and for the same reason:
	// "/applab/" and "AppLab" are both what someone would write meaning the same
	// thing. What is refused is a value that cannot be a path.
	//
	// "/" normalizes to empty rather than being kept, because empty is what the
	// root already is — writing "/" is a plausible way to *say* the root, and
	// storing it as a prefix would make every route "//api/v1/...".
	if p := strings.TrimSpace(c.BasePath); p != "" {
		if strings.ContainsAny(p, "?#") {
			return fmt.Errorf("base_path %q must be a path, without a query or fragment", c.BasePath)
		}
		p = "/" + strings.Trim(p, "/")
		if p == "/" {
			p = ""
		}
		if strings.Contains(p, "//") {
			return fmt.Errorf("base_path %q has an empty segment", c.BasePath)
		}
		c.BasePath = p
	}

	// A base domain with no gateway produces a VirtualService with an empty
	// gateway list, which Istio reads as mesh-internal only: the app deploys,
	// reports healthy, and is unreachable from outside the cluster. That is a
	// confusing way to find out about a missing setting, so it is refused here.
	if strings.TrimSpace(c.BaseDomain) != "" && strings.TrimSpace(c.Deploy.Gateway) == "" {
		return fmt.Errorf("base_domain is %q but deploy.gateway is empty: apps would be given hostnames with no gateway to serve them, so they would be unreachable from outside the cluster. Set deploy.gateway to \"<namespace>/<name>\", or leave base_domain empty to serve apps inside the cluster only",
			c.BaseDomain)
	}
	if g := strings.TrimSpace(c.Deploy.Gateway); g != "" && !strings.Contains(g, "/") {
		return fmt.Errorf("deploy.gateway %q must be \"<namespace>/<name>\", the form Istio resolves a gateway by", g)
	}

	return nil
}

// EnsureDataDir creates the data directory if it is missing. Kept out of Load
// so that loading configuration has no side effects — a caller that only wants
// to inspect the config should not create directories.
//
// A directory holding every app's source is not something other users on the
// host have any business reading, so one this creates is created 0700. It does
// not *chmod* one that already exists, and that distinction is the whole fix for
// a container that would not start.
//
// The chart mounts the data volume with fsGroup 1000 and runs the process as
// uid 1000 with every capability dropped, so the mount point is owned by root:
// the process can write inside it and cannot change its mode, needing to be the
// owner or to hold CAP_FOWNER. EnsureDataDir used to chmod unconditionally, so
// the process died at boot with "chmod /data: operation not permitted" against a
// volume it could otherwise use perfectly well.
//
// An existing directory's mode belongs to whoever mounted it. It is not a
// relaxation to leave it alone: chmod only ever succeeded where the process
// already owned the directory, which is exactly the case where MkdirAll's own
// permission argument — applied below — had already decided it.
func (c *Config) EnsureDataDir() error {
	if info, err := os.Stat(c.DataDir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("data_dir %s exists and is not a directory", c.DataDir)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat data_dir %s: %w", c.DataDir, err)
	}

	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data_dir %s: %w", c.DataDir, err)
	}
	return nil
}

// dedupe removes duplicate keys while keeping the first occurrence's position,
// so the reported order matches what the operator wrote.
func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// OpenObjectStore opens the object store this deployment is configured with.
//
// There is no directory fallback. There used to be one, so that `go run
// ./cmd/applab` worked with nothing configured — and it was a trap: a deployment
// that failed to pass its object store settings did not fail, it wrote to a
// directory on the pod's own disk and lost everything on the next restart, with
// nothing in the log to say so. A missing bucket is now a boot error naming the
// setting, which is the only outcome that cannot be mistaken for working.
//
// Tests and the local development loop point at a directory deliberately,
// through the objectstore package's own Local backend, rather than by leaving
// this unconfigured.
func (c *Config) OpenObjectStore() (objectstore.Store, error) {
	if !c.ObjectStore.Configured() {
		return nil, fmt.Errorf("no object storage configured: set object_store.endpoint and object_store.bucket (APPLAB_OBJECT_STORE_ENDPOINT and APPLAB_OBJECT_STORE_BUCKET); AppLab keeps every app, repository and key in a bucket and has nowhere else to put them")
	}

	scheme := "https"
	if c.ObjectStore.Insecure {
		scheme = "http"
	}
	endpoint := c.ObjectStore.Endpoint
	if !strings.Contains(endpoint, "://") {
		endpoint = scheme + "://" + endpoint
	}

	store, err := objectstore.NewS3(objectstore.S3Options{
		Endpoint:  endpoint,
		Bucket:    c.ObjectStore.Bucket,
		Region:    c.ObjectStore.Region,
		AccessKey: c.ObjectStore.AccessKey,
		SecretKey: c.ObjectStore.SecretKey,
		PathStyle: c.ObjectStore.PathStyle,
	})
	if err != nil {
		return nil, err
	}
	if c.ObjectStore.Prefix == "" {
		return store, nil
	}
	return objectstore.Prefixed(store, c.ObjectStore.Prefix), nil
}
