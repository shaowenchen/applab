package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/source"
	"github.com/shaowenchen/applab/internal/store"
)

// handleDeploy deploys a commit.
//
// The commit defaults to the app's current tip, which is what a caller means
// after uploading: build the newest source and run it. An explicit commit is how
// a rollback works — the same endpoint, naming an earlier revision.
//
// If the commit's image already exists from an earlier successful build it is
// reused rather than rebuilt. That is what makes a rollback fast and, more
// importantly, unable to fail for a reason the original build did not.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}
	if s.deployer == nil || !s.deployer.Ready() {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment cannot deploy: no cluster is configured"))
		return
	}

	var req struct {
		CommitSHA string `json:"commit_sha"`
		Build     bool   `json:"build"`
	}
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &req); err != nil {
			fail(w, r, err)
			return
		}
	}

	commitSHA := strings.TrimSpace(req.CommitSHA)
	if commitSHA == "" {
		head, err := s.headCommit(r.Context(), app.ID)
		if err != nil {
			if errors.Is(err, source.ErrNoCommits) {
				fail(w, r, BadRequest("app %q has no source to deploy; upload source first", app.ID))
				return
			}
			fail(w, r, Errorf(http.StatusInternalServerError, "read the app's current commit").Wrap(err))
			return
		}
		commitSHA = head
	}

	resolved, err := s.resolveCommit(r.Context(), app.ID, commitSHA)
	if err != nil {
		fail(w, r, NotFound("commit %q in app %q", commitSHA, app.ID))
		return
	}

	// Deploying means the image needs a build and the resources need creating,
	// in that order. The namespace is not among them: every app shares AppLab's
	// own, which exists by definition.
	//
	// A successful build of this commit, if one exists, supplies the image.
	image := ""
	if existing, err := s.store.FindSucceededBuild(r.Context(), app.ID, resolved); err == nil {
		image = existing.Image
	} else if !errors.Is(err, store.ErrNotFound) {
		fail(w, r, Errorf(http.StatusInternalServerError, "look up an existing build").Wrap(err))
		return
	}

	// No image yet. Either build one now, or tell the caller what to do — a
	// deploy that silently builds would make the response time unpredictable and
	// hide a failure behind a different operation.
	if image == "" {
		if !req.Build {
			fail(w, r, Conflict(
				"no image exists for commit %s; build it first with POST /api/v1/apps/%s/builds, or deploy with {\"build\":true}",
				shortSHA(resolved), app.ID))
			return
		}
		if s.build == nil || !s.build.Ready() {
			fail(w, r, Errorf(http.StatusNotImplemented, "no image exists for commit %s and this deployment cannot build", shortSHA(resolved)))
			return
		}

		build, apiErr := s.startBuild(r.Context(), app, resolved)
		if apiErr != nil {
			fail(w, r, apiErr)
			return
		}
		// The build runs asynchronously; the caller gets its id and follows it.
		// Deploying here would mean holding the request open for a build that may
		// take minutes, and the caller could not tell progress from a hang.
		s.setAppStatus(r.Context(), app.ID, model.AppStatusBuilding, "building commit "+shortSHA(resolved))
		respond(w, http.StatusAccepted, map[string]any{
			"build":  toBuildResponse(build),
			"commit": resolved,
			"status": "building",
			"next":   "follow the build at /api/v1/apps/" + app.ID + "/builds/" + build.ID + "/logs",
		})
		return
	}

	addr, apiErr := s.deployCommit(r.Context(), app, resolved, image)
	if apiErr != nil {
		if s.metrics != nil {
			s.metrics.ObserveDeploy(true)
		}
		fail(w, r, apiErr)
		return
	}
	if s.metrics != nil {
		s.metrics.ObserveDeploy(false)
	}

	slog.InfoContext(r.Context(), "deployed",
		"app", app.ID, "commit", resolved, "image", image, "address", addr.String())

	respond(w, http.StatusOK, map[string]any{
		"app":      toAppResponse(app, s.cfg.BaseDomain, s.cfg.PathPrefix, s.scheme(r)),
		"commit":   resolved,
		"image":    image,
		"host":     addr.Host,
		"path":     addr.Path,
		"url":      addr.URL(s.scheme(r)),
		"deployed": true,
	})
}

// deployCommit applies an app's resources for a known image.
//
// The app record is updated only after the resources are applied. Writing the
// record first would make AppLab claim a deploy that failed, and the record is
// what a caller reads to decide whether anything happened.
func (s *Server) deployCommit(ctx context.Context, app *model.App, commitSHA, image string) (model.Address, *apiError) {
	// The image and commit go into the object the deployer builds, so a rollout
	// carries the revision it came from.
	deployApp := *app
	deployApp.CommitSHA = commitSHA

	addr, err := s.applyDeployment(ctx, &deployApp, image)
	if err != nil {
		s.setAppStatus(ctx, app.ID, model.AppStatusFailed, err.Error())
		return model.Address{}, Errorf(http.StatusInternalServerError, "deploy app %q", app.ID).Wrap(err)
	}

	if err := s.store.SetAppDeployed(ctx, app.ID, commitSHA, image); err != nil {
		slog.WarnContext(ctx, "deployed but could not record it", "app", app.ID, "error", err)
	}
	s.setAppStatus(ctx, app.ID, model.AppStatusDeploying, "rolling out "+shortSHA(commitSHA))

	app.CommitSHA = commitSHA
	app.Image = image

	return addr, nil
}

// handleRollback deploys an earlier commit.
//
// It is a separate endpoint from deploy rather than a flag on it because the
// intent is different and worth naming: a rollback never builds, and a commit it
// names should already have an image. If it does not, the caller is told to
// build — silently building during a rollback would turn a fast, predictable
// operation into a slow one at exactly the moment someone is trying to recover
// from a bad deploy.
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}
	if s.deployer == nil || !s.deployer.Ready() {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment cannot deploy: no cluster is configured"))
		return
	}

	var req struct {
		CommitSHA string `json:"commit_sha"`
	}
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}

	commitSHA := strings.TrimSpace(req.CommitSHA)
	if commitSHA == "" {
		fail(w, r, BadRequest("a commit is required; a rollback names the commit to return to"))
		return
	}

	resolved, err := s.resolveCommit(r.Context(), app.ID, commitSHA)
	if err != nil {
		fail(w, r, NotFound("commit %q in app %q", commitSHA, app.ID))
		return
	}

	if app.CommitSHA == resolved {
		// Rolling back to what is already deployed is a no-op, and saying so is
		// more useful than a rollout that changes nothing.
		fail(w, r, Conflict("commit %s is already deployed to app %q", shortSHA(resolved), app.ID))
		return
	}

	build, err := s.store.FindSucceededBuild(r.Context(), app.ID, resolved)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fail(w, r, Conflict(
				"no image exists for commit %s, so it cannot be rolled back to; build it first with POST /api/v1/apps/%s/builds",
				shortSHA(resolved), app.ID))
			return
		}
		fail(w, r, Errorf(http.StatusInternalServerError, "look up a build of that commit").Wrap(err))
		return
	}

	addr, apiErr := s.deployCommit(r.Context(), app, resolved, build.Image)
	if apiErr != nil {
		if s.metrics != nil {
			s.metrics.ObserveDeploy(true)
		}
		fail(w, r, apiErr)
		return
	}
	if s.metrics != nil {
		s.metrics.ObserveDeploy(false)
	}

	slog.InfoContext(r.Context(), "rolled back",
		"app", app.ID, "commit", resolved, "image", build.Image)

	respond(w, http.StatusOK, map[string]any{
		"app":              toAppResponse(app, s.cfg.BaseDomain, s.cfg.PathPrefix, s.scheme(r)),
		"commit":           resolved,
		"image":            build.Image,
		"host":             addr.Host,
		"path":             addr.Path,
		"url":              addr.URL(s.scheme(r)),
		"rolled_back_from": app.CommitSHA,
	})
}

// handleStop removes an app's running resources without deleting the app.
//
// The source and history stay, so starting again is a deploy rather than a
// re-upload. Deleting the namespace would be simpler and would take both with it.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}
	if s.deployer == nil || !s.deployer.Ready() {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment cannot deploy: no cluster is configured"))
		return
	}

	if err := s.removeDeployment(r.Context(), app); err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "stop app %q", app.ID).Wrap(err))
		return
	}

	s.setAppStatus(r.Context(), app.ID, model.AppStatusCreated, "stopped")
	slog.InfoContext(r.Context(), "app stopped", "app", app.ID)

	respond(w, http.StatusOK, map[string]any{
		"app":     toAppResponse(app, s.cfg.BaseDomain, s.cfg.PathPrefix, s.scheme(r)),
		"stopped": true,
	})
}

// handleRestart triggers a rollout of the running image.
//
// The image does not change, so this is for picking up a ConfigMap, a Secret, or
// recovering an app whose pods are wedged — cases where waiting for a new commit
// would be the wrong answer.
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}
	if s.deployer == nil || !s.deployer.Ready() {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment cannot deploy: no cluster is configured"))
		return
	}

	if err := s.restartDeployment(r.Context(), app); err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "restart app %q", app.ID).Wrap(err))
		return
	}

	s.setAppStatus(r.Context(), app.ID, model.AppStatusDeploying, "restarted")
	respond(w, http.StatusOK, map[string]any{"app": app.ID, "restarted": true})
}

// appStatusResponse is an app's live state as the API presents it.
type appStatusResponse struct {
	AppID  string `json:"app_id"`
	Status string `json:"status"`

	// Deployed is what AppLab recorded; Live is what the cluster shows. They are
	// reported together rather than reconciled, because the difference is the
	// useful information: a running app whose cluster state is unhealthy is a
	// failure AppLab did not cause and cannot see through its own record.
	Deployed *deploymentRecord `json:"deployed,omitempty"`
	Live     *liveState        `json:"live,omitempty"`

	Host string `json:"host,omitempty"`

	// Path is the app's path under the host, set only when the deployment uses
	// a shared path prefix. A caller that has Host but no Path has an app at
	// that host's root.
	Path string `json:"path,omitempty"`
	URL  string `json:"url,omitempty"`
}

type deploymentRecord struct {
	CommitSHA string `json:"commit_sha,omitempty"`
	Image     string `json:"image,omitempty"`
	Status    string `json:"status"`
	Reason    string `json:"status_reason,omitempty"`
}

type liveState struct {
	Deployed        bool   `json:"deployed"`
	Available       bool   `json:"available"`
	ReadyReplicas   int32  `json:"ready_replicas"`
	DesiredReplicas int32  `json:"desired_replicas"`
	CurrentImage    string `json:"current_image,omitempty"`
	Message         string `json:"message,omitempty"`
}

// handleAppStatus reports an app's live state alongside AppLab's record.
func (s *Server) handleAppStatus(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	resp := appStatusResponse{
		AppID:  app.ID,
		Status: string(app.Status),
		Deployed: &deploymentRecord{
			CommitSHA: app.CommitSHA,
			Image:     app.Image,
			Status:    string(app.Status),
			Reason:    app.StatusReason,
		},
	}

	addr := s.addressFor(app)
	if !addr.Empty() {
		resp.Host = addr.Host
		resp.Path = addr.Path
		if app.Status == model.AppStatusRunning || app.Status == model.AppStatusDeploying {
			resp.URL = addr.URL(s.scheme(r))
		}
	}

	if s.deployer != nil && s.deployer.Ready() {
		live, err := s.appLiveStatus(r.Context(), app)
		if err != nil {
			// Not fatal: AppLab's own record is still an answer, and a cluster
			// read failing is exactly when a caller wants to see it.
			slog.DebugContext(r.Context(), "could not read live app status", "app", app.ID, "error", err)
		} else {
			resp.Live = &liveState{
				Deployed:        live.Found,
				Available:       live.Available,
				ReadyReplicas:   live.ReadyReplicas,
				DesiredReplicas: live.DesiredReplicas,
				CurrentImage:    live.CurrentImage,
				Message:         live.Message,
			}

			// Keep AppLab's own status honest: a rollout that has completed is
			// "running", and one that cannot progress is "failed". This is where
			// the record converges on the cluster rather than drifting from it.
			if live.Found {
				switch {
				case live.Available && app.Status == model.AppStatusDeploying:
					s.setAppStatus(r.Context(), app.ID, model.AppStatusRunning, "")
					resp.Status = string(model.AppStatusRunning)
					resp.Deployed.Status = string(model.AppStatusRunning)
				case live.Message != "":
					s.setAppStatus(r.Context(), app.ID, model.AppStatusFailed, live.Message)
					resp.Status = string(model.AppStatusFailed)
					resp.Deployed.Status = string(model.AppStatusFailed)
					resp.Deployed.Reason = live.Message
				}
			}
		}
	}

	respond(w, http.StatusOK, resp)
}
