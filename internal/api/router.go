package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/shaowenchen/applab/internal/appconfig"
	"github.com/shaowenchen/applab/internal/appkey"
	"github.com/shaowenchen/applab/internal/auth"
	"github.com/shaowenchen/applab/internal/config"
	"github.com/shaowenchen/applab/internal/deploy"
	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/observe"
	"github.com/shaowenchen/applab/internal/source"
	"github.com/shaowenchen/applab/internal/sourcetoken"
	"github.com/shaowenchen/applab/internal/store"
)

// APIVersion is the version of this HTTP API, reported by /api/v1/config so a
// client can assert it is talking to an interface it understands. It tracks the
// route surface, not the build version.
const APIVersion = "v1"

// Server holds the dependencies the HTTP layer needs.
//
// The pipeline halves are function fields rather than interfaces because each
// is a single operation at this layer: the API hands an app id across and the
// implementation owns everything else. Keeping them as functions means a
// deployment without source storage leaves them nil and the affected routes are
// simply absent, rather than present and failing at call time.
type Server struct {
	cfg   config.Config
	store *store.Store
	auth  *auth.Authenticator

	// The operations below all name a branch, because each one acts on one
	// branch's repository. AppLab stores a repository per branch, so an operation
	// that did not say which would have to guess — and the guess that reads as
	// natural, "the app's", is the one the caller then cannot control.

	// initSource creates an app's repository for a branch. Nil means this
	// deployment has no source storage.
	initSource func(ctx context.Context, appID, branch string) error

	// sourceRemover deletes an app's source repository, on every branch.
	sourceRemover func(ctx context.Context, appID string) error

	// sourceIngest turns an uploaded archive into a commit on a branch.
	sourceIngest func(ctx context.Context, appID, branch string, archive io.Reader, message, parent string) (*source.IngestResult, error)

	// headCommit returns the tip of a branch.
	headCommit func(ctx context.Context, appID, branch string) (string, error)

	// sourceLog returns a branch's commits in git's own order.
	sourceLog func(ctx context.Context, appID, branch string, limit int) ([]source.CommitInfo, error)

	// resolveCommit expands a possibly-abbreviated commit revision within a
	// branch's repository.
	resolveCommit func(ctx context.Context, appID, branch, revision string) (string, error)

	// sourceArchive writes a commit's source tree as a tar.gz.
	sourceArchive func(ctx context.Context, appID, branch, sha string, w io.Writer) error

	// sourceArchiveSize reports the archive's byte length.
	sourceArchiveSize func(ctx context.Context, appID, branch, sha string) (int64, error)

	// sourceBranches lists the branches an app has a repository for.
	sourceBranches func(ctx context.Context, appID string) ([]string, error)

	// appObjectsDeleter removes everything AppLab created for one app.
	//
	// Deleting an app used to mean deleting its namespace. Every app shares one
	// namespace now, so its objects are found by label and removed instead —
	// which makes this the operation that can most easily destroy something it
	// was not asked to, and the reason it verifies what it is about to delete.
	appObjectsDeleter func(ctx context.Context, appID string) error

	// build implements the build half. Nil means this deployment cannot build.
	build BuildEngine

	// sourceTokens mints the single-use credentials a build Job fetches its
	// source with.
	sourceTokens *sourcetoken.Issuer

	// observer reads an app's runtime state. Nil means this deployment cannot
	// observe.
	observer Observer

	// metrics is the platform's own instrumentation.
	metrics *Metrics

	// clusterReady reports whether the cluster is reachable.
	clusterReady func(ctx context.Context) bool

	// appKeys resolves an app key to its app, and mints and rotates them.
	//
	// It is built from the store this Server already holds rather than attached
	// from outside, because it has no dependency the Server does not: keys live
	// in the object store beside the app they belong to, which every deployment
	// has. It was a cluster-backed attachment while keys were Secrets, and the
	// cluster half of this Server is optional — so its absence used to mean the
	// whole app tier was missing. That is no longer a state that exists.
	appKeys appKeyService

	// appConfig holds an app's secret configuration, on the same terms as
	// appKeys and for the same reason. It is the other half of what used to be
	// a cluster-backed attachment; environment variables were never in it,
	// because they live in the app's own record with everything else.
	appConfig appConfigService

	// deployer is the deploy half of the pipeline. Nil means this deployment
	// cannot deploy.
	deployer Deployer

	// git is the handler serving repositories over the git smart HTTP protocol.
	// Nil means this deployment does not serve git.
	git http.Handler

	// console serves the web console. Nil means this deployment does not serve
	// one.
	console http.Handler
}

// BuildEngine is the build half of the pipeline.
//
// It is an interface rather than a function set because a build is several
// related operations sharing configuration, and because a deployment without a
// cluster leaves it nil — in which case the build routes report "not
// implemented" rather than failing obscurely.
type BuildEngine interface {
	// Ready reports whether the engine can start builds right now.
	Ready() bool

	// Start creates the build Job for a commit and returns its name.
	Start(ctx context.Context, app *model.App, buildID, commitSHA, sourceToken string) (string, error)

	// Status reads a Job's state. An empty status means the Job is gone.
	Status(ctx context.Context, namespace, jobName string) (model.BuildStatus, string, error)

	// Logs returns a build's output.
	Logs(ctx context.Context, namespace, jobName string, tailLines int64) (string, error)

	// ImageFor returns the image a build of a commit pushes to.
	ImageFor(appID, commitSHA string) string

	// Cancel stops a build by deleting its Job.
	//
	// It is what makes a new upload supersede the build already in flight: the
	// Job would otherwise run to completion and push an image for a commit that
	// is no longer the tip, holding a build slot and a registry push that nobody
	// asked for.
	Cancel(ctx context.Context, namespace, jobName string) error
}

// Deployer is the deploy half of the pipeline.
type Deployer interface {
	// Ready reports whether the deployer can reach the cluster.
	Ready() bool

	// Apply creates or updates an app's resources and returns its address.
	Apply(ctx context.Context, app *model.App, image string) (model.Address, error)

	// Status reads an app's live state.
	Status(ctx context.Context, app *model.App) (deploy.Status, error)

	// Remove deletes an app's running resources without deleting the app.
	Remove(ctx context.Context, app *model.App) error

	// Restart triggers a rollout of the running image.
	Restart(ctx context.Context, app *model.App) error
}

// New builds a Server over AppLab's own state.
//
// The per-app key and secret stores are built here rather than attached from
// outside: both live in the object store, which is the same store this Server
// was just handed, so there is nothing for a caller to decide and no deployment
// shape in which one is absent. The attachments that remain are the ones that
// genuinely depend on something optional — a cluster, a git transport, a
// console.
func New(cfg config.Config, st *store.Store, a *auth.Authenticator) *Server {
	return &Server{
		cfg:       cfg,
		store:     st,
		auth:      a,
		appKeys:   appkey.New(st),
		appConfig: appconfig.New(st),
	}
}

// WithSource attaches source storage.
//
// The operations are passed in as functions rather than as an interface because
// the API layer only ever hands an app id and some bytes across; the source
// package owns everything else. That keeps this package from having to grow a
// method for every source operation that later appears.
func (s *Server) WithSource(store *source.Store) *Server {
	s.initSource = store.Create
	s.sourceRemover = store.Remove
	s.headCommit = store.HeadCommit
	s.resolveCommit = store.ResolveCommit
	s.sourceLog = store.Log
	s.sourceArchive = store.Archive
	s.sourceArchiveSize = store.ArchiveSize
	s.sourceBranches = store.Branches
	s.sourceIngest = func(ctx context.Context, appID, branch string, archive io.Reader, message, parent string) (*source.IngestResult, error) {
		return store.Ingest(ctx, appID, branch, archive, message, parent, source.DefaultIngestLimits)
	}
	return s
}

// ActiveBranch is the branch lookup the git transport asks for when a URL named
// no branch — see gitx.Transport.WithActiveBranch.
//
// It reads the app record rather than assuming the default, because "which
// branch is live" is exactly the app state the transport does not hold. An app
// that cannot be read, or does not exist, yields an empty string, which the
// transport answers with the same 404 it gives for anything else it cannot
// serve: this endpoint must not disclose which apps exist.
func (s *Server) ActiveBranch(ctx context.Context, appID string) string {
	app, err := s.loadAppByID(ctx, appID)
	if err != nil {
		return ""
	}
	return app.ActiveBranch()
}

// WithGit attaches the handler that serves repositories over git's smart HTTP
// protocol, mounted at the path the clone URLs promise.
func (s *Server) WithGit(h http.Handler) *Server { s.git = h; return s }

// WithConsole attaches the web console.
//
// The console is a client of the same public API, so it adds no privileges — it
// is mounted at the root and everything under /api and /git takes precedence.
func (s *Server) WithConsole(h http.Handler) *Server { s.console = h; return s }

// WithBuild attaches a build engine.
func (s *Server) WithBuild(engine BuildEngine) *Server { s.build = engine; return s }

// WithSourceTokens attaches the issuer that mints single-use source tokens for
// build Jobs.
func (s *Server) WithSourceTokens(issuer *sourcetoken.Issuer) *Server {
	s.sourceTokens = issuer
	return s
}

// WithAppObjectsDeleter attaches the teardown that removes everything AppLab
// created for an app, used when deleting it.
func (s *Server) WithAppObjectsDeleter(fn func(ctx context.Context, appID string) error) *Server {
	s.appObjectsDeleter = fn
	return s
}

// WithClusterStatus attaches a probe reporting whether the cluster is reachable,
// used to answer whether the deploy half is usable.
func (s *Server) WithClusterStatus(ready func(ctx context.Context) bool) *Server {
	s.clusterReady = ready
	return s
}

// issueSourceToken mints a single-use token granting access to one commit.
//
// It is a method on Server so the build handlers do not each have to know
// whether an issuer is configured, and so the failure mode — no issuer — is one
// clear error where it happens.
func (s *Server) issueSourceToken(appID, branch, commitSHA string) (string, error) {
	if s.sourceTokens == nil {
		return "", fmt.Errorf("no source token issuer is configured; a build job cannot fetch its source")
	}
	token, err := s.sourceTokens.Issue(appID, branch, commitSHA)
	if err != nil {
		return "", err
	}
	return token.Value, nil
}

// startBuildJob creates the build Job.
func (s *Server) startBuildJob(ctx context.Context, app *model.App, buildID, commitSHA, token string) (string, error) {
	if s.build == nil {
		return "", fmt.Errorf("this deployment cannot build")
	}
	return s.build.Start(ctx, app, buildID, commitSHA, token)
}

// buildStatus reads a build Job's state from the cluster.
func (s *Server) buildStatus(ctx context.Context, namespace, jobName string) (model.BuildStatus, string, error) {
	if s.build == nil {
		return "", "", fmt.Errorf("this deployment cannot build")
	}
	return s.build.Status(ctx, namespace, jobName)
}

// buildLogs reads a build Job's log.
func (s *Server) buildLogs(ctx context.Context, namespace, jobName string, tailLines int64) (string, error) {
	if s.build == nil {
		return "", fmt.Errorf("this deployment cannot build")
	}
	return s.build.Logs(ctx, namespace, jobName, tailLines)
}

// imageFor returns the image a commit's build pushes to.
func (s *Server) imageFor(appID, commitSHA string) string {
	if s.build == nil {
		return ""
	}
	return s.build.ImageFor(appID, commitSHA)
}

// applyDeployment applies an app's resources through the deployer.
func (s *Server) applyDeployment(ctx context.Context, app *model.App, image string) (model.Address, error) {
	if s.deployer == nil {
		return model.Address{}, fmt.Errorf("this deployment cannot deploy")
	}
	return s.deployer.Apply(ctx, app, image)
}

// appLiveStatus reads an app's state from the cluster.
func (s *Server) appLiveStatus(ctx context.Context, app *model.App) (deploy.Status, error) {
	if s.deployer == nil {
		return deploy.Status{}, fmt.Errorf("this deployment cannot deploy")
	}
	return s.deployer.Status(ctx, app)
}

// removeDeployment deletes an app's running resources.
func (s *Server) removeDeployment(ctx context.Context, app *model.App) error {
	if s.deployer == nil {
		return fmt.Errorf("this deployment cannot deploy")
	}
	return s.deployer.Remove(ctx, app)
}

// restartDeployment triggers a rollout of an app's running image.
func (s *Server) restartDeployment(ctx context.Context, app *model.App) error {
	if s.deployer == nil {
		return fmt.Errorf("this deployment cannot deploy")
	}
	return s.deployer.Restart(ctx, app)
}

// WithDeployer attaches the deploy half of the pipeline.
func (s *Server) WithDeployer(d Deployer) *Server { s.deployer = d; return s }

// appKeyService is the per-app key store, as this layer needs it.
//
// It is an interface rather than the concrete appkey.Store for the same reason
// the build and deploy halves are — so the layer above can be tested without a
// store behind it — but it is no longer an optional attachment with a Ready
// check. Keys live in the object store now, which every deployment has, so a
// server always holds one.
type appKeyService interface {
	// Create mints a key for an app that has none.
	Create(ctx context.Context, appID string) (string, error)

	// Get returns an app's key.
	Get(ctx context.Context, appID string) (string, error)

	// Rotate replaces an app's key, invalidating the previous one at once.
	Rotate(ctx context.Context, appID string) (string, error)

	// Remove deletes an app's key.
	Remove(ctx context.Context, appID string) error

	// ResolveAppKey reports which app a presented key belongs to. It is the
	// method the auth tier calls, so the interface satisfies auth.AppKeyResolver
	// without either package importing the other.
	ResolveAppKey(ctx context.Context, presented string) (string, bool, error)
}

// appConfigService is the per-app secret store, as this layer needs it.
//
// Note what is missing: there is no method returning a secret's value. The
// deployer reads values, through its own narrower interface, and it is the only
// thing in AppLab that does — which is what makes "no route returns a secret" a
// property of the code rather than a rule someone has to remember.
type appConfigService interface {
	// Set writes the given values, preserving any key not mentioned, and
	// returns the app's full set of names.
	Set(ctx context.Context, appID string, values map[string]string) ([]string, error)

	// Remove deletes the named values and returns the names that remain.
	Remove(ctx context.Context, appID string, names []string) ([]string, error)

	// Names lists the keys present, never their values.
	Names(ctx context.Context, appID string) ([]string, error)

	// Delete removes an app's configuration entirely.
	Delete(ctx context.Context, appID string) error
}

// Observer reads an app's runtime state.
type Observer interface {
	// Ready reports whether the observer can reach the cluster.
	Ready() bool

	// Pods lists an app's pods, newest first.
	Pods(ctx context.Context, namespace, appID string, limit int) ([]observe.Pod, error)

	// Logs returns a container's log.
	Logs(ctx context.Context, namespace, appID string, opts observe.LogOptions) (string, error)

	// StreamLogs follows a container's log, writing as lines arrive.
	StreamLogs(ctx context.Context, namespace, appID string, opts observe.LogOptions, w io.Writer, flush func()) error

	// Events returns recent Kubernetes events concerning one app.
	Events(ctx context.Context, namespace, appID string, limit int) ([]observe.Event, error)

	// SelfPods lists AppLab's own pods.
	SelfPods(ctx context.Context, namespace string, limit int) ([]observe.Pod, error)

	// SelfLogs returns one of AppLab's own containers' logs.
	SelfLogs(ctx context.Context, namespace string, opts observe.LogOptions) (string, error)

	// StreamSelfLogs follows one of AppLab's own containers' logs.
	StreamSelfLogs(ctx context.Context, namespace string, opts observe.LogOptions, w io.Writer, flush func()) error
}

// WithObserver attaches the observability half.
func (s *Server) WithObserver(o Observer) *Server { s.observer = o; return s }

// WithMetrics attaches instrumentation.
//
// Registering the derived gauges here rather than in main keeps them next to the
// storage they read, so a counter cannot be added without its gauge being
// considered.
func (s *Server) WithMetrics(m *Metrics) *Server {
	s.metrics = m

	// These are sampled at scrape time: they are whatever the object store holds,
	// which changes without AppLab doing anything in particular.
	m.RegisterGauge("applab_apps", "Apps known to this installation.", func() float64 {
		apps, err := s.store.ListApps(context.Background())
		if err != nil {
			// A failed read reports zero rather than the last known value: a
			// stale number that looks live is worse than an obvious zero.
			return 0
		}
		n := 0
		for _, a := range apps {
			if a.Status != model.AppStatusDeleted {
				n++
			}
		}
		return float64(n)
	})
	return s
}

// listPods reads an app's pods.
func (s *Server) listPods(ctx context.Context, namespace, appID string, limit int) ([]observe.Pod, error) {
	if s.observer == nil {
		return nil, fmt.Errorf("this deployment cannot observe")
	}
	return s.observer.Pods(ctx, namespace, appID, limit)
}

// podLogs reads a container's log.
func (s *Server) podLogs(ctx context.Context, namespace, appID string, opts observe.LogOptions) (string, error) {
	if s.observer == nil {
		return "", fmt.Errorf("this deployment cannot observe")
	}
	return s.observer.Logs(ctx, namespace, appID, opts)
}

// streamPodLogs follows a container's log.
func (s *Server) streamPodLogs(ctx context.Context, namespace, appID string, opts observe.LogOptions, w io.Writer, flush func()) error {
	if s.observer == nil {
		return fmt.Errorf("this deployment cannot observe")
	}
	return s.observer.StreamLogs(ctx, namespace, appID, opts, w, flush)
}

// listEvents reads recent events concerning one app.
//
// The app is named as well as the namespace because every app shares a
// namespace: without it this would report another app's failures, which is
// worse than reporting nothing.
func (s *Server) listEvents(ctx context.Context, namespace, appID string, limit int) ([]observe.Event, error) {
	if s.observer == nil {
		return nil, fmt.Errorf("this deployment cannot observe")
	}
	return s.observer.Events(ctx, namespace, appID, limit)
}

// listSelfPods reads AppLab's own pods.
func (s *Server) listSelfPods(ctx context.Context, limit int) ([]observe.Pod, error) {
	if s.observer == nil {
		return nil, fmt.Errorf("this deployment cannot observe")
	}
	return s.observer.SelfPods(ctx, s.cfg.Namespace, limit)
}

// selfLogs reads one of AppLab's own containers' logs.
func (s *Server) selfLogs(ctx context.Context, opts observe.LogOptions) (string, error) {
	if s.observer == nil {
		return "", fmt.Errorf("this deployment cannot observe")
	}
	return s.observer.SelfLogs(ctx, s.cfg.Namespace, opts)
}

// streamSelfLogs follows one of AppLab's own containers' logs.
func (s *Server) streamSelfLogs(ctx context.Context, opts observe.LogOptions, w io.Writer, flush func()) error {
	if s.observer == nil {
		return fmt.Errorf("this deployment cannot observe")
	}
	return s.observer.StreamSelfLogs(ctx, s.cfg.Namespace, opts, w, flush)
}

// route is one endpoint: how it is matched, whether it is protected, and
// whether and how it is documented.
//
// Declaring all three in one place is what keeps the served API, the secured
// API and the documented API from drifting apart. The alternative — a mux
// configured in one file, prose in another — is how a route ends up reachable
// without authentication or documented but nonexistent.
type route struct {
	// Pattern is the Go 1.22 ServeMux pattern, method included, e.g.
	// "POST /api/v1/apps/{id}/source".
	Pattern string

	// Auth requires a configured API key. Every route that reads or changes
	// data sets it; only liveness and the self-description endpoints do not,
	// because a probe cannot hold a credential and a caller has to be able to
	// discover what this service is before it can authenticate to it.
	Auth bool

	// AppAuth additionally accepts an app key scoped to the {app} in the path.
	// Routes that set it also set Auth, so the admin tier is always accepted and
	// PatternRequiresAuth keeps its meaning unchanged.
	//
	// It is what lets someone deploy their own app without holding a credential
	// that can delete every app this installation manages. A route that sets it
	// must name {app} in its pattern: there has to be something to scope to, and
	// the middleware treats a missing {app} as a routing mistake rather than
	// letting the request through unscoped. A route serving a *collection*
	// instead sets AppListScope.
	AppAuth bool

	// AppListScope accepts an app key on a route that addresses no single app,
	// leaving the handler to narrow what it returns. It is separate from AppAuth
	// rather than folded into it because the two need opposite things: AppAuth
	// requires an {app} and refuses without one, while this one is only ever set
	// on routes that have none — and a flag whose meaning depended on whether the
	// pattern happened to contain {app} would be a rule nobody could check by
	// reading the table.
	AppListScope bool

	// AppAdminOnly marks a route that an app key may authenticate against but
	// must be refused. Deleting an app is the case: an app key may push, build,
	// deploy and roll back its app — all of which change what runs — but
	// destroying the app and its history is the operator's to do.
	AppAdminOnly bool

	// IdentifyOnly establishes who the caller is without requiring a key: a
	// request with none is passed through as anonymous rather than refused.
	//
	// It is set on the one route that is reachable both ways and means something
	// different each time — GET /api/v1/describe is the front door, so it has to
	// answer a caller that has not authenticated yet, but it can say much more to
	// one that has. The handler is what decides what each caller gets.
	//
	// It is a mode of its own rather than a relaxation of AppListScope because
	// the two differ in exactly the way that matters: AppListScope exists to
	// *require* a credential on a route with no {app} to scope against, and a
	// route that quietly stopped requiring one would hand its collection to
	// anyone. A test asserts the set of routes that are open, so this cannot be
	// added to one by accident.
	IdentifyOnly bool

	// TokenAuth requires a single-use source token instead of an API key.
	//
	// It exists for the one route a build Job calls. A build runs in the app's
	// own namespace and executes code from whoever pushed the source, so giving
	// it an API key — which can delete every app this installation manages —
	// would make every build a route to total control. A source token reaches
	// exactly one commit of one app.
	//
	// A route sets exactly one of Auth and TokenAuth; a test enforces that no
	// route sets neither.
	TokenAuth bool

	// Doc describes the route in one line for the endpoint list GET /api/v1/describe
	// returns. Empty means the route
	// is deliberately undocumented — reserved for destructive maintenance
	// operations, which are not offered to agents that act on what they read.
	Doc string

	Handler http.HandlerFunc
}

// routes returns the full route table.
//
// The order is irrelevant to routing but this is the order the API reference in
// the endpoint list is generated in, so it is grouped by resource rather than
// alphabetised.
func (s *Server) routes() []route {
	return []route{
		// -- Liveness and self-description -------------------------------
		{
			Pattern: "GET /health",
			Doc:     "Liveness. No key required — a Kubernetes probe cannot hold one.",
			Handler: s.handleHealth,
		},
		{
			// Open by design: a Prometheus scraper holds a credential awkwardly,
			// and these numbers describe the platform rather than any app. The
			// deployment is expected to restrict the path at the network edge,
			// which the chart does. Documented as open so the trade is visible.
			Pattern: "GET /metrics",
			Doc:     "Prometheus metrics for this deployment, as text. No key required — a scraper holds one awkwardly and these numbers describe the platform rather than any app. Restrict this path at the network edge where that matters.",
			Handler: s.metricsHandler,
		},
		{
			Pattern: "GET /api/v1/config",
			Doc:     "This deployment's limits and capabilities. No key required, so it doubles as the cheapest reachability check.",
			Handler: s.handleConfig,
		},
		{
			// Unauthenticated, like /api/v1/describe, because it is the other
			// half of the same answer: describe says what this deployment is,
			// this says what came before it. Together they are how a client that
			// holds no key can find out what this service is and what it is
			// compatible with.
			Pattern: "GET /api/v1/version",
			Doc:     "Build version and commit.",
			Handler: s.handleVersion,
		},
		{
			// The one call that answers "what is the state of this platform":
			// counts by status, the most recent builds across every app, whether
			// the cluster is reachable, and this deployment's self-description.
			// A client can assemble the numbers from the endpoints below by
			// listing everything and tallying, which is the work this does once,
			// where the database can count instead of transfer.
			Pattern: "GET /api/v1/overview",
			Auth:    true,
			Doc:     "The platform at a glance: app counts by status, build counts and the most recent builds across every app, whether the cluster is configured and reachable, and this deployment's self-description. Counts exclude deleted apps.",
			Handler: s.handleOverview,
		},
		{
			// Admin key only, like the overview above and for the same reason:
			// the control plane runs in the same namespace as every app but is
			// not any app's, so there is no app for an app key to be scoped
			// against. Plain Auth rather than AppAuth+AppAdminOnly, because that
			// pair *accepts* an app key and then refuses it — the right shape for
			// a route that has an {app} to scope, and the wrong one here.
			Pattern: "GET /api/v1/platform/pods",
			Auth:    true,
			Doc:     "AppLab's own pods, newest first — the deployment that serves this API, not the apps it manages. `?limit=` (default 100). Admin key only: an app key reaches one app and this is not it.",
			Handler: s.handleListSelfPods,
		},
		{
			// The reason this exists: diagnosing AppLab itself used to mean
			// shelling into the cluster for its log. Admin key only, for the same
			// reason as its pods — the control plane's log names other apps, their
			// commits and their failures.
			Pattern: "GET /api/v1/platform/logs",
			Auth:    true,
			Doc:     "AppLab's own log as `text/plain`, following by default; `?follow=false` returns what exists and closes. `?pod=` and `?container=` narrow it (default: the newest AppLab pod). `?previous=true` reads the previous container instance, which is where a crash loop's reason is written. `?tail=` (default 500, max 10000), `?since=` a duration such as `5m`. Admin key only.",
			Handler: s.handleSelfLogs,
		},
		{
			// The orientation call, and the only one an agent needs to be given:
			// it answers where this is, what it can do and what already exists.
			//
			// Unauthenticated, unlike everything below it. It carries no app, no
			// key and no cluster state — an anonymous caller gets the endpoint
			// list and the deployment's shape, and nothing about what anyone has
			// deployed; the app list, the build settings and the registry are all
			// withheld until a key authenticates.
			//
			// That is what makes it the front door: a caller has to be able to
			// find out what this service is before it can authenticate to it,
			// which is the same reason /api/v1/config is open. An app key is
			// still the better answer for a caller that has one — it gets its
			// own app's address in the list rather than an empty one.
			Pattern: "GET /api/v1/describe",
			// Identified, not required: see the flag's own comment. The handler
			// narrows to nothing for an anonymous caller.
			IdentifyOnly: true,
			Doc:          "Start here. Everything needed to work with this deployment in one call: a one-line summary, how to reach it, the full endpoint list with the credential each requires, what is wired to it (registry, gateway, domains), what the presented key may do, every app with its address, and the shortest call for each operation. Needs no key, but returns less without one: an app key sees its app, an admin key sees every app.",
			Handler:      s.handleDescribe,
		},
		// -- Apps ---------------------------------------------------------
		{
			Pattern: "GET /api/v1/apps",
			Auth:    true,
			// Reachable by both tiers, because the console needs a way to learn
			// which app an app key belongs to before it can ask for that app.
			// The handler filters the result set to the caller's own app.
			AppListScope: true,
			Doc:          "List apps. `?include_deleted=true` also returns apps that were deleted but whose id is still reserved.",
			Handler:      s.handleListApps,
		},
		{
			Pattern: "POST /api/v1/apps",
			Auth:    true,
			Doc:     "Create an app. Body: `{id, name?, port?, replicas?, dockerfile?, domain?}`.",
			Handler: s.handleCreateApp,
		},
		{
			Pattern: "GET /api/v1/apps/{app}",
			Auth:    true,
			AppAuth: true,
			Doc:     "One app.",
			Handler: s.handleGetApp,
		},
		{
			Pattern: "PATCH /api/v1/apps/{app}",
			Auth:    true,
			AppAuth: true,
			Doc:     "Change an app's settings: `{name?, port?, replicas?, dockerfile?, domain?}`. Fields omitted are left alone.",
			Handler: s.handleUpdateApp,
		},
		{
			Pattern: "DELETE /api/v1/apps/{app}",
			Auth:    true,
			// Reachable by both tiers so an app key gets a clear 403 naming the
			// rule, rather than a 404 that would read as "this app does not
			// exist" to the one person who knows it does.
			AppAuth:      true,
			AppAdminOnly: true,
			Doc:          "Delete the app and everything applab recorded for it. `?keep_source=true` retains the git repository. Requires an admin key: an app key may manage its app but not destroy it.",
			Handler:      s.handleDeleteApp,
		},

		// -- App configuration --------------------------------------------
		{
			// The asymmetry in the response is deliberate: environment
			// variables are returned with their values, secrets as names only.
			// There is no route anywhere that returns a secret's value.
			Pattern: "GET /api/v1/apps/{app}/config",
			Auth:    true,
			AppAuth: true,
			Doc:     "The app's configuration: environment variables with their values, and the *names* of its secrets. Secret values are never returned by any route.",
			Handler: s.handleGetAppConfig,
		},
		{
			Pattern: "PUT /api/v1/apps/{app}/env",
			Auth:    true,
			AppAuth: true,
			Doc:     "Set environment variables. Body: `{env: {\"NAME\": \"value\"}}`. Names not mentioned are left alone. `PORT` is refused — it is derived from the app's `port`.",
			Handler: s.handleSetAppEnv,
		},
		{
			Pattern: "DELETE /api/v1/apps/{app}/env/{name}",
			Auth:    true,
			AppAuth: true,
			Doc:     "Remove one environment variable.",
			Handler: s.handleDeleteAppEnv,
		},
		{
			Pattern: "PUT /api/v1/apps/{app}/secrets",
			Auth:    true,
			AppAuth: true,
			Doc:     "Set secret values. Body: `{secrets: {\"NAME\": \"value\"}}`. Names not mentioned are left alone. The response lists names only, never values. 501 if this deployment has no cluster, where secrets are kept.",
			Handler: s.handleSetAppSecrets,
		},
		{
			Pattern: "DELETE /api/v1/apps/{app}/secrets/{name}",
			Auth:    true,
			AppAuth: true,
			Doc:     "Remove one secret.",
			Handler: s.handleDeleteAppSecret,
		},

		// -- App keys -----------------------------------------------------
		{
			// An app key may read its own key, and an admin key may read any
			// app's: the middleware scopes by the {app} in the path, so a key
			// for another app gets a 404 rather than this app's credential.
			Pattern: "GET /api/v1/apps/{app}/key",
			Auth:    true,
			AppAuth: true,
			Doc:     "The app's API key, in full. Works with the app's own key or an admin key. This key may be used for the API, the CLI and the console, and reaches only this app.",
			Handler: s.handleGetAppKey,
		},
		{
			Pattern: "POST /api/v1/apps/{app}/key/rotate",
			Auth:    true,
			AppAuth: true,
			Doc:     "Replace the app's API key, invalidating the previous one immediately. Also creates one for an app that has none, so a lost key is recovered here. Returns the new key.",
			Handler: s.handleRotateAppKey,
		},

		// -- The files AppLab keeps in an app's tree ----------------------
		{
			// What the seeded files are, served from the running server so a copy
			// in a repository can update itself — see handleAgentFile.
			Pattern: "GET /api/v1/apps/{app}/agent/files/{file}",
			Auth:    true,
			AppAuth: true,
			Doc:     "One of the files applab keeps in this app's source tree (`applab.sh`, `AGENT.md`), as `text/plain`. This is the current version the deployment would write on the next upload — a copy in a repository can fetch it to bring itself up to date, since applab's API changes between releases. The same content is committed into every app's tree; see `?list` on the app's source for what is there.",
			Handler: s.handleAgentFile,
		},
		{
			Pattern: "GET /api/v1/apps/{app}/agent/files",
			Auth:    true,
			AppAuth: true,
			Doc:     "The names of the files applab keeps in this app's source tree. Each is servable from `GET /api/v1/apps/{app}/agent/files/{file}`.",
			Handler: s.handleAgentFiles,
		},

		// -- Source -------------------------------------------------------
		{
			Pattern: "POST /api/v1/apps/{app}/source",
			Auth:    true,
			AppAuth: true,
			Doc:     "Upload source as a tar or tar.gz and commit it. This is the main way to push code. Send the archive as the raw request body (`--data-binary @-`), or as `multipart/form-data` with a `file` field. `?message=` sets the commit message; `?parent=<sha>` commits onto a chosen commit instead of the current tip; `?branch=` commits to another branch (default: the app's active one, and a branch that does not exist yet is created — see `GET /api/v1/apps/{app}/branches`). A single wrapping directory is stripped, so `tar czf - myproject` lands with its contents at the root.",
			Handler: s.handleUploadSource,
		},
		{
			Pattern: "GET /api/v1/apps/{app}/commits",
			Auth:    true,
			AppAuth: true,
			Doc:     "An app's commit history, newest first, with the current tip. `?limit=` (default 50). `?branch=` reads another branch's history.",
			Handler: s.handleListCommits,
		},
		{
			Pattern: "GET /api/v1/apps/{app}/branches",
			Auth:    true,
			AppAuth: true,
			Doc:     "The branches this app has source for, and which one is active. A branch is created by pushing to it, so this is what the app has been pushed to.",
			Handler: s.handleListBranches,
		},
		{
			// Admin only, unlike the routes that merely read source: this one
			// changes what is running, and an app key's whole purpose is to
			// deploy one app — so an app key *can* reach it. It is listed here
			// for the reader rather than as a restriction; see the AppAuth flag
			// and AuthorizeApp for the actual boundary.
			Pattern: "PUT /api/v1/apps/{app}/branch",
			Auth:    true,
			AppAuth: true,
			Doc:     "Make a branch active and deploy it. Body: `{\"branch\":\"dev\"}`. The branch must already have been pushed to. This replaces what is running: one app runs one branch.",
			Handler: s.handleSwitchBranch,
		},
		{
			Pattern: "GET /api/v1/apps/{app}/commits/{sha}",
			Auth:    true,
			AppAuth: true,
			Doc:     "One commit. `{sha}` may be an abbreviated id.",
			Handler: s.handleGetCommit,
		},

		// -- Chunked source upload ----------------------------------------
		{
			Pattern: "POST /api/v1/apps/{app}/source/uploads",
			Auth:    true,
			AppAuth: true,
			Doc:     "Begin a chunked upload for source too large for one request. Body: `{total, chunk_size, message?}`. Returns an `upload_id`.",
			Handler: s.handleChunkedUploadStart,
		},
		{
			Pattern: "PUT /api/v1/apps/{app}/source/uploads/{upload}/parts/{index}",
			Auth:    true,
			AppAuth: true,
			Doc:     "Send one part. Body is the raw bytes. `{index}` is 1-based. Parts may be sent in any order and retried.",
			Handler: s.handleChunkedUploadPart,
		},
		{
			Pattern: "POST /api/v1/apps/{app}/source/uploads/{upload}/complete",
			Auth:    true,
			AppAuth: true,
			Doc:     "Assemble every part and commit the result. Fails if any part is missing.",
			Handler: s.handleChunkedUploadComplete,
		},

		// -- Builds -------------------------------------------------------
		{
			Pattern: "POST /api/v1/apps/{app}/builds",
			Auth:    true,
			AppAuth: true,
			Doc:     "Build an image from a commit. Body `{commit_sha?}` — omit it to build the current tip. Returns immediately; the build runs as a Job in the cluster. Returns 501 if this deployment cannot build.",
			Handler: s.handleStartBuild,
		},
		{
			Pattern: "GET /api/v1/apps/{app}/builds",
			Auth:    true,
			AppAuth: true,
			Doc:     "An app's builds, newest first. `?limit=` (default 20).",
			Handler: s.handleListBuilds,
		},
		{
			Pattern: "GET /api/v1/apps/{app}/builds/{build}",
			Auth:    true,
			AppAuth: true,
			Doc:     "One build. Its status is read from the cluster, so it reflects the Job rather than what applab last recorded.",
			Handler: s.handleGetBuild,
		},
		{
			Pattern: "GET /api/v1/apps/{app}/builds/{build}/logs",
			Auth:    true,
			AppAuth: true,
			Doc:     "The build's log, as `text/plain`. Follows the build while it runs and ends when it finishes; works unchanged for a build that has already finished. `?follow=false` returns what exists so far and stops.",
			Handler: s.handleBuildLogs,
		},
		{
			Pattern: "DELETE /api/v1/apps/{app}/builds/{build}",
			Auth:    true,
			AppAuth: true,
			Doc:     "Stop a build that has not finished, and mark it `cancelled`. A build that already finished is refused with 409 rather than relabelled. Returns 501 if this deployment cannot build.",
			Handler: s.handleCancelBuild,
		},

		// -- Deploy -------------------------------------------------------
		{
			Pattern: "POST /api/v1/apps/{app}/deploy",
			Auth:    true,
			AppAuth: true,
			Doc:     "Deploy a commit and return the URL it is served at. Body `{commit_sha?, build?}` — omit `commit_sha` to deploy the current tip. If that commit has a successful build its image is reused; if it has none, the call fails with 409 and names the fix unless `build:true` was passed, in which case a build is started and the response is a 202 with the build. Returns 501 if this deployment cannot deploy.",
			Handler: s.handleDeploy,
		},
		{
			Pattern: "POST /api/v1/apps/{app}/rollback",
			Auth:    true,
			AppAuth: true,
			Doc:     "Deploy an earlier commit. Body `{commit_sha}` (required). Reuses that commit's existing image and never builds, so a rollback stays fast and cannot fail for a reason the original build did not.",
			Handler: s.handleRollback,
		},
		{
			Pattern: "GET /api/v1/apps/{app}/status",
			Auth:    true,
			AppAuth: true,
			Doc:     "An app's live state in the cluster alongside what applab recorded. The two are reported separately and deliberately not reconciled: when they disagree, the cluster is right.",
			Handler: s.handleAppStatus,
		},
		{
			Pattern: "POST /api/v1/apps/{app}/restart",
			Auth:    true,
			AppAuth: true,
			Doc:     "Roll the running pods, keeping the same image. For picking up a changed ConfigMap or recovering pods that are wedged.",
			Handler: s.handleRestart,
		},
		{
			Pattern: "POST /api/v1/apps/{app}/stop",
			Auth:    true,
			AppAuth: true,
			Doc:     "Stop the app by removing its Deployment, Service and Ingress. The source and history are kept, so starting again is a deploy rather than a re-upload.",
			Handler: s.handleStop,
		},

		// -- Observability ------------------------------------------------
		{
			Pattern: "GET /api/v1/apps/{app}/pods",
			Auth:    true,
			AppAuth: true,
			Doc:     "The app's pods, newest first, with per-container state. A pod that is not Running carries the reason — `CrashLoopBackOff`, `ImagePullBackOff` — and a crash loop's cause is reported from the *previous* container, since the current one is only restarting. `?limit=` (default 100, max 1000).",
			Handler: s.handleListPods,
		},
		{
			Pattern: "GET /api/v1/apps/{app}/logs",
			Auth:    true,
			AppAuth: true,
			Doc:     "A pod's log as `text/plain`. Follows the pod by default; `?follow=false` returns what exists and closes. `?pod=` and `?container=` narrow it (default: the newest pod and the app container). `?previous=true` reads the previous container instance — where a crash loop's reason is written. `?tail=` (default 500, max 10000), `?since=` a duration such as `5m`.",
			Handler: s.handlePodLogs,
		},
		{
			Pattern: "GET /api/v1/apps/{app}/events",
			Auth:    true,
			AppAuth: true,
			Doc:     "Recent Kubernetes events for the app, warnings first, with a `warnings` count. This is what explains a pod that never started: a failed scheduling, an image pull that was refused, a probe that killed the container. `?limit=` (default 50).",
			Handler: s.handleListEvents,
		},
		{
			Pattern: "GET /api/v1/apps/{app}/diagnose",
			Auth:    true,
			AppAuth: true,
			Doc:     "Why the app is not working, in one call: pods, events and the relevant log, ordered so the most likely cause comes first. Use this before reading the other four endpoints.",
			Handler: s.handleDiagnose,
		},

		// -- Source archive (for build jobs) ------------------------------
		{
			// Documented as an internal endpoint: a build Job's init container
			// calls it with a single-use token, not with the API key, so it is
			// authenticated differently from everything else here. A caller with
			// the API key never needs it — the git endpoint is the way to read a
			// repository.
			Pattern:   "GET /api/v1/apps/{app}/source/archive/{sha}",
			TokenAuth: true,
			Doc:       "Internal. Download a commit's source as a tar.gz. Authenticated with a single-use source token (issued when a build starts) rather than the API key, so a build job holds no credential that reaches beyond its own commit.",
			Handler:   s.handleSourceArchive,
		},
	}
}

// RouteReference describes every documented route, for the endpoint list that
// GET /api/v1/describe returns.
//
// It is generated from the same table that configures the mux, so a route cannot
// be described without existing or exist without being described — a test
// asserts the two are the same set.
//
// It is returned as data rather than rendered as a document, so there is no
// committed copy to go stale and no generator to run: the answer a caller gets
// is produced from the running server's own table, which is the only thing that
// can be right about what it serves.
func (s *Server) RouteReference() []describeEndpoint {
	out := make([]describeEndpoint, 0, len(s.routes()))
	for _, r := range s.routes() {
		if r.Doc == "" {
			continue
		}
		method, path, _ := strings.Cut(r.Pattern, " ")
		out = append(out, describeEndpoint{
			Method: method,
			Path:   path,
			Key:    routeKeyTier(r),
			Doc:    r.Doc,
		})
	}
	return out
}

// routeKeyTier names the credential a route requires, from the flags that
// actually configure its middleware.
//
// It is finer-grained than a boolean because the tiers are not interchangeable
// and a caller that confuses them gets a 403 it cannot explain: an app key is
// not a lesser admin key, it is a key that reaches one app, and a source token
// reaches one commit of one app and cannot be obtained by a caller at all.
func routeKeyTier(r route) string {
	switch {
	case r.TokenAuth:
		// Minted by the server when a build starts and handed to the build Job.
		// No client of this API holds one.
		return "token"
	case r.AppAuth || r.AppListScope:
		// Either tier, with the handler narrowing what an app key reaches.
		return "app"
	case r.Auth:
		return "admin"
	default:
		return "none"
	}
}

// PatternRequiresAuth reports whether the route matching pattern is protected.
//
// It exposes the route table's own declaration rather than re-deriving the
// answer, so a test checking that data routes are protected cannot disagree with
// what the mux was actually configured with. An unknown pattern reports true:
// the safe answer for a route nobody can find is "assume it needs a key".
func (s *Server) PatternRequiresAuth(pattern string) bool {
	for _, r := range s.routes() {
		if r.Pattern == pattern {
			// AppListScope included: it is a distinct middleware, not an absence
			// of one. The route is authenticated by the two-tier check — an app
			// key must still present a key — it simply has no {app} for the
			// middleware to scope against, so the handler narrows instead.
			//
			// This was wrong until GET /api/v1/describe was added: the one
			// AppListScope route that existed also carried Auth, so it answered
			// correctly by accident and a route declared with the scope alone
			// would have been reported as open.
			//
			// IdentifyOnly is deliberately *not* here. That route does not
			// require a key and does not refuse a request without one, so it is
			// genuinely open; claiming otherwise would let a route be reachable
			// anonymously while passing the test that exists to catch exactly
			// that.
			return r.Auth || r.TokenAuth || r.AppListScope || r.AppAuth
		}
	}
	return true
}

// UnauthenticatedPatterns returns every route served without a key, sorted.
//
// It exposes the table's own declaration rather than a list maintained beside
// it, so the test asserting which routes are open cannot disagree with the mux
// the middleware was actually installed on.
func (s *Server) UnauthenticatedPatterns() []string {
	var out []string
	for _, r := range s.routes() {
		if !r.Auth && !r.TokenAuth && !r.AppListScope && !r.AppAuth {
			out = append(out, r.Pattern)
		}
	}
	sort.Strings(out)
	return out
}

// PatternAcceptsAppKey reports whether the route matching pattern admits the
// app-key tier.
//
// It exists for the same reason PatternRequiresAuth does: a test that asserts
// which routes are app-scoped should read the table's own declaration rather
// than a list maintained beside it, where a new route could be added to one and
// forgotten in the other.
func (s *Server) PatternAcceptsAppKey(pattern string) bool {
	for _, r := range s.routes() {
		if r.Pattern == pattern {
			return r.AppAuth || r.AppListScope
		}
	}
	return false
}

// PatternIsAppListScoped reports whether the route matching pattern is a
// collection an app key may read, with the handler narrowing the result.
//
// It is separate from PatternAcceptsAppKey because the two answer different
// questions — "may an app key reach this" and "is there an {app} to scope it
// against" — and a test that conflated them could not tell a route with the
// wrong flag from one with the wrong pattern.
func (s *Server) PatternIsAppListScoped(pattern string) bool {
	for _, r := range s.routes() {
		if r.Pattern == pattern {
			return r.AppListScope
		}
	}
	return false
}

// countAuthRejections records a 401 as an auth rejection.
func (s *Server) countAuthRejections(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == http.StatusUnauthorized && s.metrics != nil {
			s.metrics.ObserveAuthRejection()
		}
	})
}

// metricsHandler serves the instrumentation, or a clear 501 when none is
// attached.
func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	if s.metrics == nil {
		fail(w, r, Errorf(http.StatusNotImplemented, "metrics are not enabled on this deployment"))
		return
	}
	s.metrics.ServeHTTP(w, r)
}

// MarkDeployedForTest records a deployment on an app, for tests that need an app
// past the "nothing deployed yet" state. It is not part of the API surface.
func (s *Server) MarkDeployedForTest(appID, commitSHA, image string) {
	app, err := s.loadAppByID(context.Background(), appID)
	if err != nil {
		return
	}
	_ = s.store.SetAppDeployed(context.Background(), appID, commitSHA, image)
	app.CommitSHA = commitSHA
	app.Image = image
}

// Handler builds the HTTP handler: the route table, wrapped in authentication
// per route and in request logging and panic recovery around everything.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	for _, r := range s.routes() {
		var h http.Handler = r.Handler
		switch {
		case r.AppAuth:
			// Two tiers, so the route is wrapped by the one middleware that
			// knows the difference rather than by the admin check alone.
			h = s.countAuthRejections(s.appAuthMiddleware(h, r.AppAdminOnly))
		case r.IdentifyOnly:
			// Identity established, key never required — and no rejection
			// counting, because a call without a key is the expected case here
			// rather than something worth counting as a refusal.
			h = s.identifyOnlyMiddleware(h)
		case r.AppListScope:
			// Both tiers too, but there is no {app} to compare against — the
			// middleware only establishes the identity and the handler narrows
			// what it returns.
			h = s.countAuthRejections(s.appListAuthMiddleware(h))
		case r.Auth:
			// Wrapped so a refusal is counted: a steady rate of rejections is the
			// one signal that distinguishes probing from a misconfigured client.
			h = s.countAuthRejections(s.auth.Middleware(h))
		case r.TokenAuth:
			h = s.tokenAuthMiddleware(h)
		}
		mux.Handle(r.Pattern, h)
	}

	// The console is mounted at the root, after every API route: a request that
	// names a route reaches that route, and anything else is the console's.
	if s.console != nil {
		mux.Handle("/", s.console)
	}

	// The git endpoints carry binary pack data, not JSON, so they are mounted
	// ahead of the fallback below — and behind a key check, since a repository is
	// not public.
	//
	// A single pattern covers every git request for every repository: the
	// transport reads the whole path and works out which repository and which
	// sub-operation (info/refs, git-upload-pack, git-receive-pack) it names.
	// Declaring them individually would mean enumerating a protocol surface that
	// git is free to extend.
	//
	// Both tiers authenticate here, and *which repository* each may reach is
	// decided inside the transport, by the Authorize hook. It has to be that way
	// round: this mount has no {app} for a middleware to scope against, so the
	// only place the repository name exists is in the handler. The admin
	// middleware alone would refuse every app key outright, which is what made
	// git admin-only before.
	if s.git != nil {
		mux.Handle("/git/", http.StripPrefix("/git", s.appListAuthForGit(s.git)))
	}

	// A request matching no route should read as "no such endpoint" rather than
	// falling through to a default page. ServeMux's own 404 is plain text; this
	// keeps every response from the API in the same JSON shape as the rest, so a
	// client parses one error format and not two.
	//
	// With a console mounted, the root is taken: the console serves its page for
	// an unknown path, because a browser deep link like /apps/shop has to reach
	// the single-page app rather than a 404. An unknown /api path still gets the
	// JSON error, since the API patterns are more specific and win.
	if s.console == nil {
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fail(w, r, NotFound("no route matching %s %s", r.Method, r.URL.Path))
		}))
	} else {
		// With a console at the root, an unknown API path still has to answer in
		// JSON rather than serving the page — a client that mistyped an endpoint
		// should not have to parse HTML to find out.
		//
		// Each prefix is registered only when nothing else already claimed it.
		// Registering "/git/" unconditionally would collide with the git mount
		// above and panic at startup, which is a failure that only appears with
		// git actually enabled — a path a unit test without a transport would
		// never take.
		notFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fail(w, r, NotFound("no route matching %s %s", r.Method, r.URL.Path))
		})

		mux.Handle("/api/", notFound)
		if s.git == nil {
			mux.Handle("/git/", notFound)
		}
	}

	return s.metricsMiddleware(recoverPanic(logRequests(s.withBasePath(mux))))
}

// withBasePath mounts the whole route table under the configured path prefix.
//
// It exists because of how a Kubernetes Ingress works: an Ingress routes on a
// path but cannot strip one, so an AppLab served at "/applab" receives requests
// for "/applab/api/v1/...". Without this, every one of them would miss every
// route and the Ingress would look correct while the console answered 404.
//
// The prefix is stripped before the mux sees the request, so nothing inside —
// the route table, the handlers, the middleware — has to know it is mounted
// anywhere but the root. That is the whole reason it is done here rather than by
// registering every pattern twice.
//
// A request outside the prefix is refused rather than passed through. Passing it
// through would let the mux match it against the root paths, so a deployment
// served at /applab would answer on /api/v1/... as well — two addresses for one
// service, which is what a prefix exists to avoid, and a way for this deployment
// to shadow whatever else owns the root.
func (s *Server) withBasePath(mux *http.ServeMux) http.Handler {
	base := strings.TrimSuffix(strings.TrimSpace(s.cfg.BasePath), "/")
	if base == "" {
		return mux
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// The paths the cluster itself reaches this pod on are not part of the
		// public address, so the prefix does not apply to them.
		//
		// A kubelet probe connects to the container port and asks for /health;
		// it knows nothing about what path an Ingress publishes the deployment
		// under, and it cannot be told without making the chart's probe path
		// track the Ingress path. A Prometheus scrape is the same: the
		// ServiceMonitor names /metrics on the Service, which is inside the pod.
		//
		// They have to be served here rather than under the prefix, because
		// these are addresses the cluster uses to reach the process — the
		// prefix exists for callers arriving from outside through the Ingress,
		// and nothing external should be sent to either path. Refusing them
		// makes every probe fail against a server that is healthy and listening,
		// which reads as a pod that never becomes ready.
		if directPath(path) {
			mux.ServeHTTP(w, r)
			return
		}

		// The prefix itself, with or without a trailing slash, is the console's
		// root: "/applab" is what a person types and what a link to the
		// deployment produces, and it has to serve the same page as "/applab/".
		if path == base || path == base+"/" {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
			mux.ServeHTTP(w, r)
			return
		}

		if !strings.HasPrefix(path, base+"/") {
			// Refused in the API's own shape, so a client parses one error
			// format rather than two. A 404 rather than a 403: from outside,
			// this path is simply not where the deployment is.
			fail(w, r, NotFound("no route matching %s %s", r.Method, path))
			return
		}

		r = r.Clone(r.Context())
		r.URL.Path = strings.TrimPrefix(path, base)
		mux.ServeHTTP(w, r)
	})
}

// directPath reports whether a path is one the cluster reaches the pod on
// rather than one a caller reaches the deployment on.
//
// Kept as a function because the same two paths are recognized by name in
// logRequests, which quiets them so a busy probe does not bury the log. A third
// path that the cluster uses belongs in both places.
func directPath(path string) bool {
	return path == "/health" || path == "/metrics"
}

// statusRecorder captures the status code so the log line can report it.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	// A handler that writes without calling WriteHeader has implicitly sent a
	// 200; recording that here keeps the log honest about it.
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Flush forwards to the underlying writer when it supports flushing, which
// streaming responses (build and pod logs) need. Without this the wrapper would
// silently break Server-Sent Events: io.Copy would buffer and the client would
// see nothing until the handler returned.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logRequests records one line per request at debug level.
//
// Debug rather than info: a busy control plane is mostly health probes, and at
// info they would crowd out the events actually worth reading. Failures are
// logged separately at the point they occur, at a level that matches.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Probes run every few seconds for the life of the process; logging
		// them at all would be pure noise.
		//
		// Matched as a suffix rather than on "/health" exactly, because this
		// middleware sits outside the base path and so sees the path as the
		// client sent it: a deployment served at "/applab" is probed at
		// "/applab/health". Comparing to "/health" would silently start logging
		// every probe, which is the noise this is here to avoid.
		if path := r.URL.Path; path == "/health" || strings.HasSuffix(path, "/health") {
			next.ServeHTTP(w, r)
			return
		}

		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		slog.DebugContext(r.Context(), "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"remote", r.RemoteAddr)
	})
}

// recoverPanic turns a panicking handler into a 500 rather than a dropped
// connection and a stack trace on stderr.
//
// A panic in one request must not take down a control plane that may be
// mid-build for other apps, so the process stays up and the failure is logged
// with enough context to find it.
func recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is the documented way for a handler to abort
			// a response deliberately; it is not a bug and must propagate.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			slog.ErrorContext(r.Context(), "handler panicked",
				"method", r.Method,
				"path", r.URL.Path,
				"panic", rec)
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error"})
		}()
		next.ServeHTTP(w, r)
	})
}

// SortedPatterns returns every registered pattern, sorted. Used by tests to
// assert the table is well-formed.
func (s *Server) SortedPatterns() []string {
	out := make([]string, 0, len(s.routes()))
	for _, r := range s.routes() {
		out = append(out, r.Pattern)
	}
	sort.Strings(out)
	return out
}
