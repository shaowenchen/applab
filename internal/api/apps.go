package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/shaowenchen/applab/internal/deploy"
	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/store"
)

// defaultAppPort is what an app is assumed to listen on when the caller does not
// say. It is a guess, and a wrong one is visible immediately as a Deployment
// whose Service points at nothing — so it is reported back in the app's details
// rather than kept implicit.
//
// 80 rather than 8080 because that is the port a container image's own
// Dockerfile is most likely to EXPOSE: an image built for a platform that serves
// HTTP conventionally listens there, and a default that matches it is one fewer
// thing to correct.
//
// It *is* a low port, and that does constrain what the deployer can write: a bind
// below 1024 is refused for an unprivileged process unless the pod's own network
// namespace lowers ip_unprivileged_port_start, which is why every app pod does.
// A capability would not do it — see the deployer for why — and an app image
// that runs as its own non-root user would otherwise exit on start-up against a
// Deployment that looked healthy.
const defaultAppPort int32 = 80

// appResponse is an app as the API presents it.
//
// It is a separate type from the model on purpose: the wire format is a
// contract that must stay stable, and letting it be the struct AppLab happens to
// use internally would make every refactor a breaking API change.
type appResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	Port     int32 `json:"port"`
	Replicas int32 `json:"replicas"`

	Dockerfile string `json:"dockerfile"`
	Domain     string `json:"domain"`

	// EnvCount reports how many environment variables an app has, without
	// shipping their values. It comes from the app record, so it costs nothing
	// to include on every app in a list.
	//
	// There is no matching secret count, deliberately: secrets live in the
	// cluster, so a list of N apps would cost N Secret reads to produce one. The
	// app's own config endpoint reports them, where a single read is cheap and
	// the names are worth having anyway.
	EnvCount int `json:"env_count"`

	// URL is where an app is served, as one address. It is derived and reported
	// so a caller does not have to reconstruct the deployment's addressing
	// convention.
	//
	// One field rather than a host and a path, because the two are not
	// independently meaningful: with a shared path prefix every app reports the
	// same host and the path is what says which is meant, and with a subdomain
	// each there is no path at all. A caller given the pair has to know which
	// case it is in to use them; given the address it does not.
	//
	// It is reported whether or not the app has been deployed, because it is
	// where the app *is served* — a fact about its settings — rather than whether
	// anything answers there yet. `status` is the field that says that.
	URL string `json:"url,omitempty"`

	// Branch is the app's active branch: what a deploy builds from, and the
	// branch a git clone with no branch named gets. It is always set — an app
	// with no branch recorded reports the default — because "which branch is
	// live" is a question with an answer at all times.
	Branch string `json:"branch"`

	CommitSHA string `json:"commit_sha,omitempty"`
	Image     string `json:"image,omitempty"`

	// AutoDeploy is whether a push to the active branch builds and deploys on
	// its own. Reported as the resolved value rather than the stored one, so a
	// caller never has to know that "unset" means yes — it reads true, which is
	// what the app actually does.
	AutoDeploy bool `json:"auto_deploy"`

	// Resources is what this app has set for itself — not the effective bounds.
	//
	// An empty field here means "the deployment's value", which is a different
	// fact from the number the container actually runs under. That number is on
	// the app's usage endpoint, read from the Deployment, and it is the one a
	// caller should show as the effective limit. These are the settings, and an
	// editor needs them: a form that showed the resolved value would turn a
	// default into an explicit setting the first time anyone pressed save.
	//
	// It is here rather than on a route of its own because it is app state, like
	// port and replicas, and a listing that carries it costs nothing — it is
	// already in the record.
	Resources model.Resources `json:"resources"`

	// Status is the one-word summary shown on the app's own page and by
	// `applab status`: the running state, or "building" while a build is in
	// flight, because there is room for one answer there and "something is
	// happening to this app" is what someone wants from it.
	Status       string `json:"status"`
	StatusReason string `json:"status_reason,omitempty"`

	// RunStatus and BuildStatus are the two halves that summary folds together,
	// reported separately because a listing has room for both and they fail
	// independently: a failed build leaves the previous revision running, and a
	// successful build changes nothing until a deploy applies it. A single
	// column that had to choose one of them was wrong about the other half the
	// time.
	//
	// RunStatus is where the app is running — created, deploying, running or
	// failed — and is the same value Status carries when nothing is building.
	RunStatus string `json:"run_status"`
	// BuildStatus is the *latest* build's outcome: pending, running, succeeded,
	// failed or cancelled. Empty when the app has never been built, which is
	// different from a build that failed — an app whose source has never been
	// built is not an app with a problem.
	BuildStatus string `json:"build_status,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// appRuntime is what the cluster says about one app, gathered so a response can
// be built from one value rather than from a growing argument list.
//
// It is assembled once per listing — see appRuntimes — because each of its parts
// is a read the whole page shares: one Deployment list, one build Job list.
type appRuntime struct {
	// Status is the folded summary; RunStatus is the half of it that is about the
	// Deployment. Both are carried because the two routes present differently: an
	// app's own page shows one, a listing shows the other beside the build.
	Status    model.AppStatus
	RunStatus model.AppStatus

	// BuildStatus is the latest build's outcome, empty when nothing has been
	// built.
	BuildStatus model.BuildStatus

	// Live is the Deployment's own state, for the commit, the image and the
	// rollout message.
	Live deploy.Status
}

// addressFor resolves where an app is served under this deployment's
// conventions. It is the one place the base domain, the base path and the path
// prefix are combined, so a response cannot report one convention while the
// route uses another.
func (s *Server) addressFor(a *model.App) model.Address {
	return a.Address(s.cfg.BaseDomain, s.cfg.BasePath, s.cfg.PathPrefix)
}

// toAppResponse renders an app as the API presents it.
//
// runtime is passed in rather than read from the app, because the app does not
// carry any of it: what is running is a fact about the cluster, and it is read
// once for a whole listing rather than once per row. A caller with no cluster
// passes the zero appRuntime, which reports the app as not deployed — which is
// what a deployment without a cluster can honestly say.
//
// It is a method rather than a function taking the convention as arguments: the
// base domain, the base path and the path prefix are only meaningful together,
// and three positional strings is a pair waiting to be swapped.
func (s *Server) toAppResponse(a *model.App, r *http.Request, runtime appRuntime) appResponse {
	resp := appResponse{
		ID:          a.ID,
		Name:        a.Name,
		Port:        a.Port,
		Replicas:    a.Replicas,
		Dockerfile:  a.Dockerfile,
		Domain:      a.Domain,
		Branch:      a.ActiveBranch(),
		AutoDeploy:  a.AutoDeploys(),
		Resources:   a.Resources,
		Status:      string(runtime.Status),
		RunStatus:   string(runtime.RunStatus),
		BuildStatus: string(runtime.BuildStatus),
		EnvCount:    len(a.Env),
		CreatedAt:   a.CreatedAt,
		UpdatedAt:   a.UpdatedAt,
	}

	// What is deployed, from the cluster. Both are empty for an app with no
	// Deployment, which is what the console reads to decide whether to show a
	// commit at all.
	if runtime.Live.Found {
		resp.CommitSHA = runtime.Live.CommitSHA
		resp.Image = runtime.Live.CurrentImage
		resp.StatusReason = runtime.Live.Message
	}

	addr := s.addressFor(a)
	if !addr.Empty() {
		resp.URL = addr.URL(s.scheme(r))
	}
	return resp
}

// appResponseFor renders one app, reading its live state from the cluster.
//
// It is what every single-app route uses, so that a route cannot report a status
// that disagrees with its neighbours: there is one place that decides, and this
// is the convenience wrapper over it. The list route resolves every app in one
// pass instead, which is the whole point of appRuntimes.
func (s *Server) appResponseFor(ctx context.Context, r *http.Request, app *model.App) appResponse {
	live := s.liveStatusesFor(ctx, app)
	runtimes := s.appRuntimes(ctx, []*model.App{app}, map[string]deploy.Status{app.ID: live})
	return s.toAppResponse(app, r, runtimes[app.ID])
}

// createAppRequest is the body of POST /api/v1/apps.
type createAppRequest struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Port       *int32 `json:"port"`
	Replicas   *int32 `json:"replicas"`
	Dockerfile string `json:"dockerfile"`
	Domain     string `json:"domain"`

	// AutoDeploy is optional and defaults to true, which is what a create
	// without it gets. A pointer so that `"auto_deploy": false` is
	// distinguishable from the field being absent.
	AutoDeploy *bool `json:"auto_deploy"`
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	var req createAppRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}

	req.ID = strings.TrimSpace(req.ID)
	if err := model.ValidateAppID(req.ID); err != nil {
		fail(w, r, BadRequest("%s", err.Error()))
		return
	}

	app := &model.App{
		ID:         req.ID,
		Name:       strings.TrimSpace(req.Name),
		Port:       defaultAppPort,
		Replicas:   1,
		Dockerfile: strings.TrimSpace(req.Dockerfile),
		Domain:     strings.TrimSpace(req.Domain),

		// Set here so everything downstream — the namespace creation just
		// below, and any build or deploy — has it without recomputing.
		Namespace: s.namespaceFor(req.ID),
	}
	if app.Name == "" {
		app.Name = req.ID
	}
	if app.Dockerfile == "" {
		app.Dockerfile = "Dockerfile"
	}
	if req.Port != nil {
		app.Port = *req.Port
	}
	if req.Replicas != nil {
		app.Replicas = *req.Replicas
	}
	app.AutoDeploy = req.AutoDeploy
	if err := validateAppSettings(app); err != nil {
		fail(w, r, err)
		return
	}

	if err := s.store.CreateApp(r.Context(), app); err != nil {
		fail(w, r, fromStoreError(err, fmt.Sprintf("app %q", app.ID)))
		return
	}

	// An app key is minted before the repository, not after it, so the opening
	// commit can carry it.
	//
	// The seeded files are written into the repository as it is created, and one
	// of them — applab.sh — holds the app's key as its default credential. Minting
	// the key afterwards would produce exactly one tree without it, the very first
	// one, so a caller who clones an app they just created would get the one
	// checkout that does not run. Everything else follows from the order.
	//
	// The key is minted with the app for the same reason the repository is: an app
	// without one is a half-created app, and the failure belongs to the caller
	// that caused it rather than surfacing later from a deploy that cannot
	// authenticate.
	//
	// It is deliberately NOT returned on the ordinary response shape. It is
	// returned by the create response, which is a wrapper — see
	// createdAppResponse — because appResponse is shared by list, get and patch,
	// and a field that must be filled in only on one path is one that eventually
	// gets filled in on another.
	if s.appKeys != nil {
		if _, err := s.appKeys.Create(r.Context(), app.ID); err != nil {
			// The app row exists but has no key. Rolling back is the honest
			// outcome, for the same reason as a repository failure: the caller has
			// no way to tell an app with no key from a key they typed wrong.
			if delErr := s.store.DeleteApp(r.Context(), app.ID); delErr != nil {
				slog.ErrorContext(r.Context(), "failed to roll back app after key failure",
					"app", app.ID, "error", delErr)
			}
			fail(w, r, Errorf(http.StatusInternalServerError, "create an API key for app %q", app.ID).Wrap(err))
			return
		}
	}

	// A repository is created eagerly rather than on first upload so that a
	// failure here is reported to the caller that caused it, instead of
	// surfacing later as a confusing error from an unrelated upload.
	if s.initSource != nil {
		if err := s.createRepository(r, app); err != nil {
			// The app row exists but has no repository. Rolling back is the
			// honest outcome: a half-created app would fail every subsequent
			// operation with no way for the caller to fix it.
			if delErr := s.store.DeleteApp(r.Context(), app.ID); delErr != nil {
				slog.ErrorContext(r.Context(), "failed to roll back app after repository failure",
					"app", app.ID, "error", delErr)
			}
			if s.appKeys != nil {
				if keyErr := s.appKeys.Remove(r.Context(), app.ID); keyErr != nil {
					slog.ErrorContext(r.Context(), "failed to remove app key after repository failure",
						"app", app.ID, "error", keyErr)
				}
			}
			fail(w, r, err)
			return
		}
	}

	// The routing is created now, so the address the caller is handed is really
	// wired rather than one that appears at the first deploy. Everything the
	// VirtualService needs — the host, the path, the Service it will point at —
	// is known from the app's own settings.
	//
	// A failure here is logged and not rolled back, unlike the repository and the
	// key above. Those leave the app unusable; this leaves it unreachable until
	// the next deploy, which creates the same object. Destroying an app the
	// caller just created, over routing that a retry fixes, would be the worse
	// answer.
	s.publishApp(r.Context(), app)

	if s.metrics != nil {
		s.metrics.ObserveAppCreated()
	}
	slog.InfoContext(r.Context(), "app created", "app", app.ID)

	// The create response carries the app's key, which no other response does.
	//
	// Creating an app is the one moment the caller is entitled to it: they just
	// asked for the app, the key is minted here, and fetching it afterwards is a
	// second call they have to know to make. Everywhere else the key stays behind
	// GET /apps/{app}/key, where a deliberate request for a credential is what
	// produces one.
	//
	// It is a wrapper around the ordinary response rather than a field on it,
	// which is the point: appResponse is shared by list, get and patch, and a
	// credential on that shape is one refactor away from appearing in a listing
	// of every app in the deployment. A separate type cannot leak that way.
	var appKey string
	if s.appKeys != nil {
		// Best effort, and deliberately so: the key exists — it was created above,
		// and its failure would have rolled the app back — so a read that fails
		// here costs the caller one extra call to GET /apps/{app}/key rather than
		// making a successful create look like a failed one.
		key, err := s.appKeys.Get(r.Context(), app.ID)
		if err != nil {
			slog.ErrorContext(r.Context(), "created an app but could not read back its key",
				"app", app.ID, "error", err)
		}
		appKey = key
	}

	respond(w, http.StatusCreated, createdAppResponse{
		appResponse: s.appResponseFor(r.Context(), r, app),
		AppKey:      appKey,
	})
}

// createdAppResponse is what POST /api/v1/apps returns.
//
// It embeds the app's own response so every field is in the same place it is
// everywhere else, and adds the one thing only a create can answer: the key the
// app was minted with.
type createdAppResponse struct {
	appResponse

	// AppKey is the app's API key. It is the same credential
	// GET /api/v1/apps/{app}/key returns.
	//
	// Omitted when this deployment mints no app keys, which is a real
	// configuration — appKeys is optional — and an empty string would read as a
	// key that authenticates nothing.
	AppKey string `json:"app_key,omitempty"`
}

// publishApp creates or updates an app's routing, reporting a failure without
// failing the caller's operation.
//
// It is best-effort for the same reason on both paths that call it: an app with
// no VirtualService still stores source, still builds, and is published by the
// next deploy — see Deployer.Publish. What it must not do is leave an operator
// unaware, so the failure is logged at error with the app named.
func (s *Server) publishApp(ctx context.Context, app *model.App) {
	if s.deployer == nil || !s.deployer.Ready() {
		// No cluster, or no gateway configured: there is nothing to publish to,
		// and an installation with no base domain has no address at all.
		return
	}
	if _, err := s.deployer.Publish(ctx, app); err != nil {
		slog.ErrorContext(ctx, "could not publish the app's routing; the next deploy will create it",
			"app", app.ID, "error", err)
	}
}

// createRepository delegates to the source store. It is separated so that the
// rollback above stays readable, and so a deployment without source storage
// skips the whole concern.
func (s *Server) createRepository(r *http.Request, app *model.App) *apiError {
	if s.initSource == nil {
		return nil
	}
	// The app's branch, which for a new app is the default. A create does not
	// take a branch parameter: an app is created with one branch and gains others
	// by being pushed to, which is the same way git itself does it.
	if err := s.initSource(r.Context(), app.ID, app.ActiveBranch()); err != nil {
		return Errorf(http.StatusInternalServerError, "create source repository for app %q", app.ID).Wrap(err)
	}
	return nil
}

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "list apps").Wrap(err))
		return
	}

	// An app key sees exactly one app: its own.
	//
	// This route carries no {app} for the middleware to compare against, so the
	// narrowing happens here. It is the one collection an app key can read, and
	// it exists so the console can learn which app a key belongs to before it
	// can ask for that app — without it, an app key would sign in to a page that
	// had no way to find out what to show.
	//
	// The filter is applied to the caller's identity rather than to the query
	// string, so no request parameter can widen it.
	identity := identityFrom(r.Context())

	// Every app is returned. There used to be an `include_deleted` filter and a
	// "deleted" status a delete wrote, so that an id stayed reserved — nothing
	// ever wrote it: DeleteApp removes the record outright, which frees the id
	// for reuse, and the flag selected on a value that could not occur.
	out := make([]appResponse, 0, len(apps))
	live := s.liveStatus(r.Context())
	runtimes := s.appRuntimes(r.Context(), apps, live)
	for _, a := range apps {
		if !identity.Admin() && a.ID != identity.App {
			continue
		}
		out = append(out, s.toAppResponse(a, r, runtimes[a.ID]))
	}
	respond(w, http.StatusOK, out)
}

func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, s.appResponseFor(r.Context(), r, app))
}

// updateAppRequest is the body of PATCH /api/v1/apps/{app}.
//
// Every field is a pointer so that "not mentioned" is distinguishable from
// "explicitly set to the zero value" — without that, a PATCH that only changes
// the name would silently reset the port to whatever the default is.
type updateAppRequest struct {
	Name       *string `json:"name"`
	Port       *int32  `json:"port"`
	Replicas   *int32  `json:"replicas"`
	Dockerfile *string `json:"dockerfile"`
	Domain     *string `json:"domain"`

	// AutoDeploy turns building and deploying on a push on or off for this app.
	//
	// A pointer for the usual reason: an absent field leaves the setting alone,
	// which is how every other field on this request behaves. There is no
	// "unset" state a caller can write — the API reports the resolved value, so
	// what goes in is what comes back.
	AutoDeploy *bool `json:"auto_deploy"`

	// Branch sets the app's active branch.
	//
	// Setting it here does not deploy anything, which is the difference between
	// this and PUT /branch: this records what the app is on, and that route
	// switches to it and rolls it out. Both exist because they are different
	// intentions — correcting which branch an app is on, versus moving it — and
	// conflating them would mean a PATCH that silently redeploys.
	Branch *string `json:"branch"`

	// Resources sets what the container may use. A pointer per field for the
	// usual reason, with one addition worth stating: an empty string *clears*
	// the field, returning it to the deployment's default. That is the only way
	// to unset a value once set, and it is why these are pointers to strings
	// rather than strings — an absent field leaves the setting alone, and an
	// empty one says "back to the default".
	Resources *resourcesRequest `json:"resources"`
}

// resourcesRequest is the resources half of a PATCH.
//
// Grouped in one object rather than as four top-level fields because they are
// always set together — a panel with four inputs submits four values, and a
// caller reading the request body should see them as one setting.
type resourcesRequest struct {
	CPURequest    *string `json:"cpu_request"`
	MemoryRequest *string `json:"memory_request"`
	CPULimit      *string `json:"cpu_limit"`
	MemoryLimit   *string `json:"memory_limit"`
}

// apply copies the fields that were mentioned onto the app's resources.
//
// Each field is trimmed, so an empty string means "clear it" rather than a value
// of whitespace — which Validate would then have to reject, and would report as a
// malformed quantity rather than as the clearing the caller intended.
func (rr *resourcesRequest) apply(into *model.Resources) {
	if rr == nil {
		return
	}
	for _, f := range []struct {
		from *string
		to   *string
	}{
		{rr.CPURequest, &into.CPURequest},
		{rr.MemoryRequest, &into.MemoryRequest},
		{rr.CPULimit, &into.CPULimit},
		{rr.MemoryLimit, &into.MemoryLimit},
	} {
		if f.from != nil {
			*f.to = strings.TrimSpace(*f.from)
		}
	}
}

func (s *Server) handleUpdateApp(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r)
	if err != nil {
		fail(w, r, err)
		return
	}

	var req updateAppRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}

	// Whether this update moves the app's address, which is what decides if its
	// routing has to be rewritten below.
	domainChanged := false

	if req.Name != nil {
		app.Name = strings.TrimSpace(*req.Name)
	}
	if req.Port != nil {
		app.Port = *req.Port
	}
	if req.Replicas != nil {
		app.Replicas = *req.Replicas
	}
	if req.Dockerfile != nil {
		app.Dockerfile = strings.TrimSpace(*req.Dockerfile)
		if app.Dockerfile == "" {
			app.Dockerfile = "Dockerfile"
		}
	}
	if req.Domain != nil {
		next := strings.TrimSpace(*req.Domain)
		domainChanged = next != app.Domain
		app.Domain = next
	}
	if req.AutoDeploy != nil {
		app.AutoDeploy = req.AutoDeploy
	}
	if req.Branch != nil {
		branch := strings.TrimSpace(*req.Branch)
		if branch == "" {
			// Clearing it means the default rather than nothing, which is the
			// same resolution an app that has never had one gets.
			branch = model.DefaultBranch
		}
		if err := model.ValidateBranchName(branch); err != nil {
			fail(w, r, BadRequest("%s", err.Error()))
			return
		}
		app.Branch = branch
	}
	req.Resources.apply(&app.Resources)

	if err := validateAppSettings(app); err != nil {
		fail(w, r, err)
		return
	}
	if err := s.store.UpdateApp(r.Context(), app); err != nil {
		fail(w, r, fromStoreError(err, fmt.Sprintf("app %q", app.ID)))
		return
	}

	// The domain is what the app's address is made of, so changing it moves the
	// app — and its VirtualService would otherwise keep routing the old host
	// until the next deploy. Republished only when it changed, since every other
	// field leaves the address where it was.
	if domainChanged {
		s.publishApp(r.Context(), app)
	}

	respond(w, http.StatusOK, s.appResponseFor(r.Context(), r, app))
}

func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r)
	if err != nil {
		fail(w, r, err)
		return
	}

	keepSource := r.URL.Query().Get("keep_source") == "true"

	// The cluster objects go first. If they cannot be removed, the app row is
	// left alone so the failure is retryable — deleting the record first would
	// orphan objects with nothing left that knows about them.
	//
	// This used to delete the app's namespace. It removes the app's own objects
	// now, because every app shares one namespace: a namespace delete here would
	// take AppLab itself and every other app with it.
	if s.appObjectsDeleter != nil {
		if err := s.appObjectsDeleter(r.Context(), app.ID); err != nil {
			fail(w, r, Errorf(http.StatusInternalServerError, "delete the cluster objects for app %q", app.ID).Wrap(err))
			return
		}
	}

	if !keepSource && s.sourceRemover != nil {
		if err := s.sourceRemover(r.Context(), app.ID); err != nil {
			// The cluster objects are already gone, so the app is not running.
			// Failing here would leave a record of an app that no longer exists;
			// the removal is logged and the record is deleted anyway.
			slog.ErrorContext(r.Context(), "failed to remove source repository; deleting app record anyway",
				"app", app.ID, "error", err)
		}
	}

	// The key is removed here rather than left to the cluster teardown above.
	// That teardown deletes the app's Kubernetes objects and runs before this
	// point and only when a cluster is reachable, so an app deleted while the
	// cluster is down would keep a working credential behind. Deleting the
	// record without deleting the key is the one outcome that leaves a live
	// credential for an app that no longer exists.
	if s.appKeys != nil {
		if err := s.appKeys.Remove(r.Context(), app.ID); err != nil {
			// Logged, not fatal: the app record is going regardless, and a
			// leftover Secret is worth a warning rather than a failed delete
			// that leaves the caller unable to remove the app at all.
			slog.ErrorContext(r.Context(), "failed to remove app key; deleting app record anyway",
				"app", app.ID, "error", err)
		}
	}

	// The secrets go for the same reason the key does, and through the same
	// window: they live in the app's own directory, but the record is deleted
	// below, so anything left under it would be unreachable — live credentials
	// and connection strings for an app that no longer exists.
	if s.appConfig != nil {
		if err := s.appConfig.Delete(r.Context(), app.ID); err != nil {
			slog.ErrorContext(r.Context(), "failed to remove app configuration; deleting app record anyway",
				"app", app.ID, "error", err)
		}
	}

	if err := s.store.DeleteApp(r.Context(), app.ID); err != nil {
		fail(w, r, fromStoreError(err, fmt.Sprintf("app %q", app.ID)))
		return
	}

	slog.InfoContext(r.Context(), "app deleted", "app", app.ID, "kept_source", keepSource)
	respond(w, http.StatusOK, map[string]any{
		"id":          app.ID,
		"deleted":     true,
		"kept_source": keepSource,
	})
}

// loadApp reads the {app} path value and loads it, mapping every failure to the
// right HTTP error. Every app-scoped handler starts here.
func (s *Server) loadApp(r *http.Request) (*model.App, *apiError) {
	id := r.PathValue("app")
	if id == "" {
		return nil, BadRequest("no app id in the request path")
	}
	app, err := s.loadAppByID(r.Context(), id)
	if err != nil {
		return nil, fromStoreError(err, fmt.Sprintf("app %q", id))
	}
	return app, nil
}

// loadAppByID loads an app and fills in the namespace derived from this
// deployment's configuration.
//
// The namespace is derived rather than stored, and it is filled here — at the
// one place an app enters this layer — rather than by each caller. Every
// operation that touches the cluster needs it, and a caller that forgot would
// address the empty namespace, which is a mistake that produces no error: the
// API server treats "" as the default namespace, so objects would be created in
// the wrong place and the app's own namespace would stay empty.
func (s *Server) loadAppByID(ctx context.Context, id string) (*model.App, error) {
	app, err := s.store.GetApp(ctx, id)
	if err != nil {
		return nil, err
	}
	app.Namespace = s.namespaceFor(app.ID)
	return app, nil
}

// requestedBranch returns the branch a handler should act on.
//
// It is the one place "which branch" is decided, because every source operation
// needs an answer and an operation that answered it differently from its
// neighbour would act on one branch and read from another.
//
// The rule is: a branch the request named, or the app's active one. Naming one
// is how a caller works with a branch that is not deployed — browsing its
// history, fetching a commit's tree, or pushing source to it through the tarball
// API rather than over git — and it is a query parameter rather than a path
// segment because it is a choice about the operation, not part of what is being
// addressed.
//
// A named branch is validated here rather than deeper down, so a malformed name
// is a 400 that says what is wrong instead of a 500 from a path join.
func (s *Server) requestedBranch(r *http.Request, app *model.App) (string, *apiError) {
	branch := strings.TrimSpace(r.URL.Query().Get("branch"))
	if branch == "" {
		return app.ActiveBranch(), nil
	}
	if err := model.ValidateBranchName(branch); err != nil {
		return "", BadRequest("%s", err.Error())
	}
	return branch, nil
}

// namespaceFor returns the namespace an app's resources live in.
//
// Every app shares AppLab's own namespace, which is what lets AppLab hold a
// namespaced Role rather than a ClusterRole. Objects are told apart by their
// applab.io/app label, not by a namespace boundary.
func (s *Server) namespaceFor(appID string) string {
	return model.Namespace(s.cfg.Namespace, appID)
}

// validateAppSettings checks the invariants a deploy depends on.
//
// These are rejected at the edge because each one has a failure mode that is
// hard to diagnose from the cluster side: a port outside the valid range
// produces a Service that cannot route, and a replica count of zero produces a
// Deployment that is "ready" while nothing runs.
func validateAppSettings(app *model.App) *apiError {
	// The bounds and their wording live in model, because three surfaces enforce
	// them — this, the console and the CLI — and a person should read the same
	// sentence about a bad port wherever they typed it. The API is the one that
	// decides, so its answer is what the others mirror.
	if err := model.ValidatePort(app.Port); err != nil {
		return BadRequest("%s", err.Error())
	}
	if err := model.ValidateReplicas(app.Replicas); err != nil {
		return BadRequest("%s", err.Error())
	}
	// Refused here rather than in the deployer, where a bad quantity panics:
	// every value the deployer parses comes from AppLab's own checked
	// configuration, and an app's own resources are the one path by which a
	// caller's string would reach it.
	if err := app.Resources.Validate(); err != nil {
		return BadRequest("%s", err.Error())
	}
	if strings.ContainsAny(app.Dockerfile, "\x00") {
		return BadRequest("dockerfile path contains a null byte")
	}
	if strings.HasPrefix(app.Dockerfile, "/") || strings.Contains(app.Dockerfile, "..") {
		// It becomes a path inside the checkout, so a traversal here would let
		// a caller point the build at a file outside the uploaded source.
		return BadRequest("dockerfile must be a relative path inside the source tree, without \"..\"")
	}
	return nil
}

// decodeJSON reads a JSON request body into v.
//
// Unknown fields are rejected rather than ignored: a caller that misspells
// "replicas" would otherwise get a 200 and no change, which looks like AppLab
// silently failing to apply what it was asked.
func decodeJSON(r *http.Request, v any) *apiError {
	defer r.Body.Close()

	dec := json.NewDecoder(io.LimitReader(r.Body, maxJSONBody))
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return BadRequest("request body is empty; expected a JSON object")
		}
		return BadRequest("invalid JSON body: %s", err.Error())
	}
	// A second value in the stream means the body is not one object, which is
	// almost always a client bug worth reporting rather than half-applying.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return BadRequest("request body must contain exactly one JSON object")
	}
	return nil
}

// maxJSONBody bounds a JSON request body. These are all small control-plane
// objects; source archives travel through the upload endpoints instead.
const maxJSONBody = 1 << 20

// scheme reports the URL scheme a client reached this service with, for the
// links handed back to it.
//
// The order is the order of how much each source actually knows:
//
//  1. APPLAB_PUBLIC_URL, when the operator has set it. This is the one source
//     that is authoritative rather than inferred, and it exists because the
//     others are both wrong in the normal deployment: the request arrives from
//     the cluster's own ingress, and BaseURL is the in-cluster Service address.
//  2. The request itself — its TLS state, then X-Forwarded-Proto, which is what
//     an ingress sets when it terminates TLS and forwards plain HTTP.
//  3. Plain http, which is what a request that is neither of the above actually
//     is.
//
// BaseURL is deliberately not consulted, and it used to be. It is the address a
// build pod clones from — "http://applab.ops-system.svc:80" — so taking a scheme
// from it made every app's URL http on an installation served over TLS, which is
// the opposite of what a link into a browser should be. The two addresses answer
// different questions and only one of them is about how a person arrives.
func (s *Server) scheme(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		if i := strings.Index(s.cfg.PublicURL, "://"); i > 0 {
			return s.cfg.PublicURL[:i]
		}
	}
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return "https"
	}
	return "http"
}

// publicURL is the address this deployment is reached at as a whole — the
// console's address, with no app in it and no trailing slash.
//
// It is what a client is handed as the base to build its own requests from, and
// what the seeded applab.sh carries as its default APPLAB_URL. When the operator
// has configured the public address, that is the answer; otherwise it is derived
// from the request, which is right when AppLab is reached directly and an
// assumption behind a proxy that rewrites nothing.
//
// It falls back to the host this deployment would give an app, then to the bare
// request host, so a deployment with no domain configured — one reachable only
// inside the cluster — still produces an address a client can use.
func (s *Server) publicURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return strings.TrimSuffix(strings.TrimSpace(s.cfg.PublicURL), "/")
	}
	scheme := s.scheme(r)

	// With a base domain the installation's address is the domain and its base
	// path, plus the path prefix when the apps share one host — the console and
	// the API live under the prefix too, so it belongs in the address even though
	// nothing here is an app.
	basePath := strings.TrimSuffix(s.cfg.BasePath, "/")
	if s.cfg.BaseDomain != "" {
		if s.cfg.PathPrefix != "" {
			return scheme + "://" + s.cfg.BaseDomain + basePath + s.cfg.PathPrefix
		}
		return scheme + "://" + s.cfg.BaseDomain + basePath
	}
	// No domain: there is no app address to derive anything from, so the request
	// is the only thing that knows where this deployment lives.
	return scheme + "://" + r.Host + basePath
}

// intQuery reads an optional non-negative integer query parameter.
func intQuery(r *http.Request, name string, def int) (int, *apiError) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, BadRequest("query parameter %s must be a non-negative integer", name)
	}
	return n, nil
}

// ensureStoreUnused keeps the store import honest in builds where a handler set
// is trimmed; it is removed once every handler uses the store.
var _ = store.ErrNotFound
