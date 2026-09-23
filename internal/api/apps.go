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

	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/store"
)

// defaultAppPort is what an app is assumed to listen on when the caller does not
// say. It is a guess, and a wrong one is visible immediately as a Deployment
// whose Service points at nothing — so it is reported back in the app's details
// rather than kept implicit.
const defaultAppPort int32 = 8080

// appResponse is an app as the API presents it.
//
// It is a separate type from the model on purpose: the wire format is a
// contract that must stay stable, and letting it be the struct applab happens to
// use internally would make every refactor a breaking API change.
type appResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	Port     int32 `json:"port"`
	Replicas int32 `json:"replicas"`

	Dockerfile string `json:"dockerfile"`
	Domain     string `json:"domain"`

	// Hostname and URL are derived, and reported so a caller does not have to
	// reconstruct the deployment's domain convention to know where its app is.
	// URL is empty until something has been deployed.
	Hostname string `json:"hostname,omitempty"`
	URL      string `json:"url,omitempty"`

	CommitSHA string `json:"commit_sha,omitempty"`
	Image     string `json:"image,omitempty"`

	Status       string `json:"status"`
	StatusReason string `json:"status_reason,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toAppResponse(a *model.App, baseDomain, scheme string) appResponse {
	resp := appResponse{
		ID:           a.ID,
		Name:         a.Name,
		Port:         a.Port,
		Replicas:     a.Replicas,
		Dockerfile:   a.Dockerfile,
		Domain:       a.Domain,
		CommitSHA:    a.CommitSHA,
		Image:        a.Image,
		Status:       string(a.Status),
		StatusReason: a.StatusReason,
		CreatedAt:    a.CreatedAt,
		UpdatedAt:    a.UpdatedAt,
	}
	if host := a.Hostname(baseDomain); host != "" {
		resp.Hostname = host
		// Only advertised once the app has actually been deployed: a URL that
		// 404s at the ingress reads as "deployed but broken" when the truth is
		// "not deployed yet".
		if a.Status == model.AppStatusRunning || a.Status == model.AppStatusDeploying {
			resp.URL = scheme + "://" + host
		}
	}
	return resp
}

// createAppRequest is the body of POST /api/v1/apps.
type createAppRequest struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Port       *int32 `json:"port"`
	Replicas   *int32 `json:"replicas"`
	Dockerfile string `json:"dockerfile"`
	Domain     string `json:"domain"`
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
		Status:     model.AppStatusCreated,

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
	if err := validateAppSettings(app); err != nil {
		fail(w, r, err)
		return
	}

	if err := s.store.CreateApp(r.Context(), app); err != nil {
		fail(w, r, fromStoreError(err, fmt.Sprintf("app %q", app.ID)))
		return
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
			fail(w, r, err)
			return
		}
	}

	if s.metrics != nil {
		s.metrics.ObserveAppCreated()
	}
	slog.InfoContext(r.Context(), "app created", "app", app.ID)
	respond(w, http.StatusCreated, toAppResponse(app, s.cfg.BaseDomain, s.scheme(r)))
}

// createRepository delegates to the source store. It is separated so that the
// rollback above stays readable, and so a deployment without source storage
// skips the whole concern.
func (s *Server) createRepository(r *http.Request, app *model.App) *apiError {
	if s.initSource == nil {
		return nil
	}
	if err := s.initSource(r.Context(), app.ID); err != nil {
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

	includeDeleted := r.URL.Query().Get("include_deleted") == "true"
	out := make([]appResponse, 0, len(apps))
	for _, a := range apps {
		if a.Status == model.AppStatusDeleted && !includeDeleted {
			continue
		}
		out = append(out, toAppResponse(a, s.cfg.BaseDomain, s.scheme(r)))
	}
	respond(w, http.StatusOK, out)
}

func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, toAppResponse(app, s.cfg.BaseDomain, s.scheme(r)))
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
		app.Domain = strings.TrimSpace(*req.Domain)
	}

	if err := validateAppSettings(app); err != nil {
		fail(w, r, err)
		return
	}
	if err := s.store.UpdateApp(r.Context(), app); err != nil {
		fail(w, r, fromStoreError(err, fmt.Sprintf("app %q", app.ID)))
		return
	}
	respond(w, http.StatusOK, toAppResponse(app, s.cfg.BaseDomain, s.scheme(r)))
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
	// take applab itself and every other app with it.
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
	if app.Status == model.AppStatusDeleted {
		return nil, NotFound("app %q", id)
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

// namespaceFor returns the namespace an app's resources live in.
//
// Every app shares applab's own namespace, which is what lets applab hold a
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
	if app.Port < 1 || app.Port > 65535 {
		return BadRequest("port %d is not in the valid range 1-65535", app.Port)
	}
	if app.Replicas < 1 {
		// Zero replicas is a legitimate way to park a deployment, but it reads
		// as "running" to anything checking readiness, which makes it a trap
		// rather than a feature. Stopping is what an idle app wants.
		return BadRequest("replicas must be at least 1; use the stop endpoint to scale an app down")
	}
	if app.Replicas > 50 {
		return BadRequest("replicas %d exceeds the maximum of 50", app.Replicas)
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
// "replicas" would otherwise get a 200 and no change, which looks like applab
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
func (s *Server) scheme(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		if i := strings.Index(s.cfg.BaseURL, "://"); i > 0 {
			return s.cfg.BaseURL[:i]
		}
	}
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return "https"
	}
	return "http"
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
