package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/shaowenchen/applab/internal/deploy"
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

	// Which branch this deploy is of. An app has one branch deployed at a time,
	// so a deploy that named none means its active one; naming one is how a
	// caller deploys a branch without first switching the app to it.
	branch, apiErr := s.requestedBranch(r, app)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	commitSHA := strings.TrimSpace(req.CommitSHA)
	if commitSHA == "" {
		head, err := s.headCommit(r.Context(), app.ID, branch)
		if err != nil {
			if errors.Is(err, source.ErrNoCommits) {
				fail(w, r, BadRequest("app %q has no source on branch %q to deploy; upload source first", app.ID, branch))
				return
			}
			fail(w, r, Errorf(http.StatusInternalServerError, "read the app's current commit").Wrap(err))
			return
		}
		commitSHA = head
	}

	resolved, err := s.resolveCommit(r.Context(), app.ID, branch, commitSHA)
	if err != nil {
		fail(w, r, NotFound("commit %q in app %q on branch %q", commitSHA, app.ID, branch))
		return
	}

	s.deployResolved(w, r, app, branch, resolved, req.Build)
}

// deployResolved deploys a known commit of a known branch, building it first if
// it has no image yet, and writes the response.
//
// It is the whole of the deploy path after the commit has been decided, and it
// takes a writer because two routes reach it: POST /deploy, which resolves a
// commit from the request, and PUT /branch, which resolves the head of the
// branch being switched to. Duplicating this would mean two copies of the
// image lookup, the build-or-refuse decision and the response shape — and the
// two would drift in exactly the case that matters, which is the failure path.
func (s *Server) deployResolved(w http.ResponseWriter, r *http.Request, app *model.App, branch, resolved string, build bool) {
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
		if !build {
			fail(w, r, Conflict(
				"no image exists for commit %s; build it first with POST /api/v1/apps/%s/builds, or deploy with {\"build\":true}",
				shortSHA(resolved), app.ID))
			return
		}
		if s.build == nil || !s.build.Ready() {
			fail(w, r, Errorf(http.StatusNotImplemented, "no image exists for commit %s and this deployment cannot build", shortSHA(resolved)))
			return
		}

		build, apiErr := s.startBuild(r.Context(), app, branch, resolved)
		if apiErr != nil {
			fail(w, r, apiErr)
			return
		}
		// The build runs asynchronously; the caller gets its id and follows it.
		// Deploying here would mean holding the request open for a build that may
		// take minutes, and the caller could not tell progress from a hang.
		//
		// Nothing records that a build started: the build's own record is what
		// says so, and it is what appStatus reads to report the app as building.
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
		"app":      s.appResponseFor(r.Context(), r, app),
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
// Nothing is recorded afterwards, and that is the change: what is deployed is
// what the cluster says is deployed, which Apply has just stamped onto the
// Deployment. Writing it to the bucket as well is what made a fresh install
// report apps as running with nothing behind them.
func (s *Server) deployCommit(ctx context.Context, app *model.App, commitSHA, image string) (model.Address, *apiError) {
	addr, err := s.applyDeployment(ctx, app, image, commitSHA)
	if err != nil {
		return model.Address{}, Errorf(http.StatusInternalServerError, "deploy app %q", app.ID).Wrap(err)
	}
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

	// A rollback resolves within the branch that is live, not across branches:
	// what is being undone is a deploy, and a deploy is built from one branch.
	// Rolling back to a commit that exists only on another branch would leave the
	// app running code its record does not say it is on.
	branch, apiErr := s.requestedBranch(r, app)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	resolved, err := s.resolveCommit(r.Context(), app.ID, branch, commitSHA)
	if err != nil {
		fail(w, r, NotFound("commit %q in app %q on branch %q", commitSHA, app.ID, branch))
		return
	}

	// What is deployed comes from the cluster, which is the only place it is
	// recorded now: the Deployment's annotation, stamped by the deploy that put
	// it there.
	current := s.liveStatusesFor(r.Context(), app).CommitSHA
	if current == resolved {
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
		"app":              s.appResponseFor(r.Context(), r, app),
		"commit":           resolved,
		"image":            build.Image,
		"host":             addr.Host,
		"path":             addr.Path,
		"url":              addr.URL(s.scheme(r)),
		"rolled_back_from": current,
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

	slog.InfoContext(r.Context(), "app stopped", "app", app.ID)

	// The app reports as "created" from here without anything recording it: the
	// Deployment is gone, and a status derived from the cluster says so.
	respond(w, http.StatusOK, map[string]any{
		"app":     s.appResponseFor(r.Context(), r, app),
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

	respond(w, http.StatusOK, map[string]any{"app": app.ID, "restarted": true})
}

// appStatusResponse is an app's state as the API presents it.
//
// There used to be two halves: Deployed, what AppLab had recorded, and Live,
// what the cluster showed. They are the same thing now — nothing is recorded, so
// there is nothing for the cluster to disagree with — and the single view is the
// cluster's. The derived status in Status is that view summarised into one word
// for a listing, and Live is its detail.
type appStatusResponse struct {
	AppID  string `json:"app_id"`
	Status string `json:"status"`

	Live *liveState `json:"live,omitempty"`

	Host string `json:"host,omitempty"`

	// Path is the app's path under the host, set only when the deployment uses
	// a shared path prefix. A caller that has Host but no Path has an app at
	// that host's root.
	Path string `json:"path,omitempty"`
	URL  string `json:"url,omitempty"`
}

type liveState struct {
	Deployed        bool   `json:"deployed"`
	Available       bool   `json:"available"`
	ReadyReplicas   int32  `json:"ready_replicas"`
	DesiredReplicas int32  `json:"desired_replicas"`
	CurrentImage    string `json:"current_image,omitempty"`

	// CommitSHA is the commit the running image was built from, read from the
	// Deployment's annotation. It is the only record of it there is.
	CommitSHA string `json:"commit_sha,omitempty"`

	Message string `json:"message,omitempty"`
}

// handleAppStatus reports what is running for one app.
//
// It is the detail behind the one-word status in a listing, and it comes from
// the cluster — including the commit, which nothing else records now.
func (s *Server) handleAppStatus(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	live := s.liveStatusesFor(r.Context(), app)
	status := appStatus(map[string]deploy.Status{app.ID: live}, app.ID, s.buildInFlight(r.Context(), app.ID))

	resp := appStatusResponse{
		AppID:  app.ID,
		Status: string(status),
	}

	if live.Found {
		resp.Live = &liveState{
			Deployed:        true,
			Available:       live.Available,
			ReadyReplicas:   live.ReadyReplicas,
			DesiredReplicas: live.DesiredReplicas,
			CurrentImage:    live.CurrentImage,
			CommitSHA:       live.CommitSHA,
			Message:         live.Message,
		}
	}

	addr := s.addressFor(app)
	if !addr.Empty() {
		resp.Host = addr.Host
		resp.Path = addr.Path
		if status == model.AppStatusRunning || status == model.AppStatusDeploying {
			resp.URL = addr.URL(s.scheme(r))
		}
	}

	respond(w, http.StatusOK, resp)
}
