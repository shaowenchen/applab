package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/source"
	"github.com/shaowenchen/applab/internal/store"
)

// buildResponse is a build as the API presents it.
type buildResponse struct {
	ID        string `json:"id"`
	AppID     string `json:"app_id"`
	CommitSHA string `json:"commit_sha"`
	Status    string `json:"status"`

	Image   string `json:"image,omitempty"`
	JobName string `json:"job_name,omitempty"`
	Reason  string `json:"reason,omitempty"`

	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

func toBuildResponse(b *model.Build) buildResponse {
	resp := buildResponse{
		ID:        b.ID,
		AppID:     b.AppID,
		CommitSHA: b.CommitSHA,
		Status:    string(b.Status),
		Image:     b.Image,
		JobName:   b.JobName,
		Reason:    b.Reason,
		CreatedAt: b.CreatedAt,
	}
	if !b.StartedAt.IsZero() {
		resp.StartedAt = b.StartedAt
	}
	if !b.FinishedAt.IsZero() {
		resp.FinishedAt = b.FinishedAt
	}
	return resp
}

// handleStartBuild starts a build of a commit.
//
// The commit defaults to the app's current tip, which is what a caller means by
// "build this app": they have just uploaded, and the newest commit is what they
// want built. Passing one explicitly is how a rollback rebuilds or a caller
// builds a specific revision.
func (s *Server) handleStartBuild(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}
	if s.build == nil || !s.build.Ready() {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment cannot build: no registry, builder image or applab URL is configured"))
		return
	}

	var req struct {
		CommitSHA string `json:"commit_sha"`
	}
	// An empty body is allowed and means "build the tip".
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
				fail(w, r, BadRequest("app %q has no source to build; upload source first", app.ID))
				return
			}
			fail(w, r, Errorf(http.StatusInternalServerError, "read the app's current commit").Wrap(err))
			return
		}
		commitSHA = head
	}

	// An abbreviated commit is accepted because that is what a caller was shown,
	// but it is expanded before use so everything downstream deals in full ids.
	resolved, err := s.resolveCommit(r.Context(), app.ID, commitSHA)
	if err != nil {
		fail(w, r, NotFound("commit %q in app %q", commitSHA, app.ID))
		return
	}

	build, apiErr := s.startBuild(r.Context(), app, resolved)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	slog.InfoContext(r.Context(), "build started",
		"app", app.ID, "build", build.ID, "commit", resolved, "job", build.JobName)

	respond(w, http.StatusAccepted, toBuildResponse(build))
}

// startBuild creates the record and the Job for one build.
//
// The record is written before the Job is created, so a Job that exists always
// has something that knows about it. The reverse order would leave a running
// build nothing is tracking if the second call failed.
func (s *Server) startBuild(ctx context.Context, app *model.App, commitSHA string) (*model.Build, *apiError) {
	buildID, err := model.NewID()
	if err != nil {
		return nil, Errorf(http.StatusInternalServerError, "generate build id").Wrap(err)
	}

	// No provisioning step: the namespace already exists, because it is the one
	// AppLab runs in, and the registry credentials a Job pushes with are already
	// there for the same reason.

	build := &model.Build{
		ID:        buildID,
		AppID:     app.ID,
		CommitSHA: commitSHA,
		Status:    model.BuildStatusPending,
	}

	// The token is issued before the record is written so a failure to issue one
	// costs nothing. It grants access to exactly this commit and is consumed by
	// the fetch.
	token, err := s.issueSourceToken(app.ID, commitSHA)
	if err != nil {
		return nil, Errorf(http.StatusInternalServerError, "issue a source token for the build").Wrap(err)
	}

	if err := s.store.CreateBuild(ctx, build); err != nil {
		return nil, Errorf(http.StatusInternalServerError, "record the build").Wrap(err)
	}

	jobName, err := s.startBuildJob(ctx, app, buildID, commitSHA, token)
	if s.metrics != nil {
		s.metrics.ObserveBuild(err != nil)
	}
	if err != nil {
		// The record is kept and marked failed, rather than deleted: the caller
		// asked for a build and needs to be able to see that it did not start and
		// why.
		s.setBuildStatus(ctx, buildID, model.BuildStatusFailed, err.Error())
		s.setAppStatus(ctx, app.ID, model.AppStatusBuildFailed, err.Error())
		return nil, Errorf(http.StatusInternalServerError, "start the build job").Wrap(err)
	}

	build.JobName = jobName
	if err := s.store.SetBuildStatus(ctx, buildID, model.BuildStatusPending, ""); err != nil {
		slog.WarnContext(ctx, "could not record build status", "build", buildID, "error", err)
	}
	s.setAppStatus(ctx, app.ID, model.AppStatusBuilding, "building commit "+shortSHA(commitSHA))

	// Any other build of this app is now the older one. Stopping it here as well
	// as on upload is what makes the invariant hold for every way a build can
	// start: two of them running at once would race to push the same image tag,
	// and which one won would be whichever finished last.
	//
	// This build is excluded by id — it is already in the unfinished list.
	s.supersedeOtherBuilds(ctx, app, buildID)

	return build, nil
}

// supersedeOtherBuilds stops every build of an app except one.
func (s *Server) supersedeOtherBuilds(ctx context.Context, app *model.App, exceptBuildID string) {
	if s.build == nil {
		return
	}

	unfinished, err := s.store.ListUnfinishedBuildsForApp(ctx, app.ID)
	if err != nil {
		slog.WarnContext(ctx, "could not list the builds in flight", "app", app.ID, "error", err)
		return
	}

	for _, build := range unfinished {
		if build.ID == exceptBuildID || build.JobName == "" {
			continue
		}
		if err := s.build.Cancel(ctx, app.Namespace, build.JobName); err != nil {
			slog.WarnContext(ctx, "could not stop a superseded build",
				"app", app.ID, "build", build.ID, "job", build.JobName, "error", err)
			continue
		}
		s.setBuildStatus(ctx, build.ID, model.BuildStatusCancelled,
			"superseded by a newer build of "+app.ID)
		slog.InfoContext(ctx, "stopped a superseded build",
			"app", app.ID, "build", build.ID, "job", build.JobName)
	}
}

// handleCancelBuild stops a running build and marks it cancelled.
//
// The same operation the upload path performs, offered explicitly: someone may
// simply want the build to stop — it is pushing a bad commit, or it is wedged —
// without uploading anything.
func (s *Server) handleCancelBuild(w http.ResponseWriter, r *http.Request) {
	build, apiErr := s.loadBuild(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	if build.Status.Terminal() {
		fail(w, r, Conflict("build %s has already finished (%s)", shortSHA(build.ID), build.Status))
		return
	}

	// The cluster is consulted first, the same way reading a build does: a build
	// whose Job ended while nothing was watching would otherwise be reported as
	// stopped when it had in fact finished on its own.
	s.refreshBuild(r.Context(), build)
	if build.Status.Terminal() {
		fail(w, r, Conflict("build %s has already finished (%s)", shortSHA(build.ID), build.Status))
		return
	}

	// A build with no Job was recorded but never started, so there is nothing in
	// the cluster to delete and the record is the only thing to settle.
	if build.JobName != "" {
		if s.build == nil || !s.build.Ready() {
			fail(w, r, Errorf(http.StatusNotImplemented, "this deployment cannot build"))
			return
		}
		if err := s.build.Cancel(r.Context(), app.Namespace, build.JobName); err != nil {
			fail(w, r, Errorf(http.StatusInternalServerError, "stop build job %s", build.JobName).Wrap(err))
			return
		}
	}

	s.setBuildStatus(r.Context(), build.ID, model.BuildStatusCancelled, "stopped on request")
	build.Status = model.BuildStatusCancelled
	build.Reason = "stopped on request"

	slog.InfoContext(r.Context(), "build stopped", "app", app.ID, "build", build.ID, "job", build.JobName)
	respond(w, http.StatusOK, toBuildResponse(build))
}

// supersedeBuilds stops whatever this app is currently building.
//
// A new upload makes the build already in flight obsolete: it is building a
// commit that is no longer the tip, and left alone it would run to completion,
// hold a build slot and push an image nobody is waiting for — possibly landing
// after the newer build and overwriting what the app should be running. So the
// upload stops it.
//
// Best-effort, and deliberately not fatal to the upload: the source is stored by
// the time this runs, and refusing an upload because a Job could not be deleted
// would trade a working commit for a tidier cluster. Every failure is logged and
// the upload proceeds.
//
// The record is marked cancelled rather than deleted, because it happened: the
// builds table is a history, and a build that was stopped is part of it — which
// is also what keeps someone from reading a vanished build as "the build I asked
// for never started".
//
// Only builds with a Job are touched. One with no JobName is either still being
// created — `startBuild` writes the record before the Job, so there is a real
// window — or was left behind by a restart, and deleting a Job name that was
// never created would be a delete of "" against the cluster. The restart case is
// already handled: `ReconcileBuilds` fails those at startup.
func (s *Server) supersedeBuilds(ctx context.Context, app *model.App) {
	if s.build == nil {
		return
	}

	unfinished, err := s.store.ListUnfinishedBuildsForApp(ctx, app.ID)
	if err != nil {
		slog.WarnContext(ctx, "could not list the builds in flight", "app", app.ID, "error", err)
		return
	}

	for _, build := range unfinished {
		if build.JobName == "" {
			continue
		}

		if err := s.build.Cancel(ctx, app.Namespace, build.JobName); err != nil {
			// The Job is recorded as failed rather than cancelled, because it was
			// not stopped: whatever it is doing, it is still doing it.
			slog.WarnContext(ctx, "could not stop the build in flight",
				"app", app.ID, "build", build.ID, "job", build.JobName, "error", err)
			continue
		}

		s.setBuildStatus(ctx, build.ID, model.BuildStatusCancelled,
			"superseded by a newer upload of "+app.ID)
		slog.InfoContext(ctx, "stopped the build in flight to make way for an upload",
			"app", app.ID, "build", build.ID, "job", build.JobName)

		// The app's own status reverts to what the cluster is actually running.
		// "building" is the status of an app whose build is in flight, and there
		// is no longer one; leaving it would show an app as busy until something
		// else happened to overwrite it.
		if app.Status == model.AppStatusBuilding {
			s.setAppStatus(ctx, app.ID, model.AppStatusDeploying,
				"the build in flight was superseded by a newer upload")
		}
	}
}

func (s *Server) handleGetBuild(w http.ResponseWriter, r *http.Request) {
	build, apiErr := s.loadBuild(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	// The cluster is the source of truth for whether the build is still running,
	// so its state is read before answering — otherwise a caller polling this
	// endpoint would see "pending" forever after a crash-restart of applab.
	s.refreshBuild(r.Context(), build)

	respond(w, http.StatusOK, toBuildResponse(build))
}

func (s *Server) handleListBuilds(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	limit, apiErr := intQuery(r, "limit", 20)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	builds, err := s.store.ListBuilds(r.Context(), app.ID, limit)
	if err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "list builds").Wrap(err))
		return
	}

	out := make([]buildResponse, 0, len(builds))
	for _, b := range builds {
		out = append(out, toBuildResponse(b))
	}
	respond(w, http.StatusOK, out)
}

// handleBuildLogs streams a build's log.
//
// The log is followed while the build runs and the response ends when it
// finishes, so a caller can watch a build with one request rather than polling.
// When the build has already finished the recorded log is returned and the
// response closes immediately — the same call works either way, which is what
// makes it usable from a script that does not know the build's state.
func (s *Server) handleBuildLogs(w http.ResponseWriter, r *http.Request) {
	build, apiErr := s.loadBuild(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, r, Errorf(http.StatusInternalServerError, "log streaming is not supported by this server"))
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no") // tells nginx not to buffer the stream
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	follow := r.URL.Query().Get("follow") != "false"

	if err := s.streamBuildLog(r.Context(), w, flusher, build, follow); err != nil {
		// Headers are already sent, so the only honest signal left is a marker in
		// the body. It is written in a form a reader will notice rather than as a
		// bare error code.
		_, _ = fmt.Fprintf(w, "\n[AppLab] log stream ended: %v\n", err)
		flusher.Flush()
	}
}

// streamBuildLog writes a build's log, optionally following it to completion.
func (s *Server) streamBuildLog(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, build *model.Build, follow bool) error {
	if s.build == nil || build.JobName == "" {
		return s.writeRecordedLog(ctx, w, flusher, build)
	}

	app, err := s.loadAppByID(ctx, build.AppID)
	if err != nil {
		return s.writeRecordedLog(ctx, w, flusher, build)
	}

	// The pod may not exist yet: a Job's pod takes a moment to be admitted, and
	// a caller that asked for logs immediately after starting a build would
	// otherwise get "no pod found" for something that is working fine.
	deadline := time.Now().Add(30 * time.Second)
	for {
		logs, logErr := s.buildLogs(ctx, app.Namespace, build.JobName, 0)
		if logErr == nil && logs != "" {
			_, _ = w.Write([]byte(logs))
			flusher.Flush()
			break
		}
		if time.Now().After(deadline) || !follow {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}

	if !follow {
		return nil
	}

	// Follow the build to completion, re-reading the log each time it grows.
	// BuildKit writes progress continuously and there is no streaming API in the
	// Kubernetes client for a container that may not exist yet, so the log is
	// polled and the delta written. The alternative — one read at the end —
	// would make the endpoint useless for its purpose, which is watching a build
	// that takes minutes.
	seen := 0
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// An overall bound so a build Job that hangs without ever reaching a terminal
	// condition cannot hold the connection open forever.
	overall := time.NewTimer(45 * time.Minute)
	defer overall.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-overall.C:
			_, _ = fmt.Fprintf(w, "\n[AppLab] stopped following after 45m; the build may still be running\n")
			flusher.Flush()
			return nil
		case <-ticker.C:
		}

		if logs, logErr := s.buildLogs(ctx, app.Namespace, build.JobName, 0); logErr == nil {
			if len(logs) > seen {
				_, _ = w.Write([]byte(logs[seen:]))
				seen = len(logs)
				flusher.Flush()
			}
		}

		status, reason, statusErr := s.buildStatus(ctx, app.Namespace, build.JobName)
		if statusErr != nil {
			return statusErr
		}
		if status.Terminal() {
			// One final read so the last output before the pod exited is not lost.
			if logs, logErr := s.buildLogs(ctx, app.Namespace, build.JobName, 0); logErr == nil && len(logs) > seen {
				_, _ = w.Write([]byte(logs[seen:]))
			}
			_, _ = fmt.Fprintf(w, "\n[AppLab] build %s\n", status)
			if reason != "" {
				_, _ = fmt.Fprintf(w, "[AppLab] %s\n", reason)
			}
			flusher.Flush()
			return nil
		}
	}
}

// writeRecordedLog emits what AppLab recorded about a build whose Job is gone.
func (s *Server) writeRecordedLog(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, build *model.Build) error {
	_, _ = fmt.Fprintf(w, "[AppLab] no live build job for this build\n")
	_, _ = fmt.Fprintf(w, "[AppLab] status: %s\n", build.Status)
	if build.Reason != "" {
		_, _ = fmt.Fprintf(w, "[AppLab] reason: %s\n", build.Reason)
	}
	flusher.Flush()
	return nil
}

// refreshBuild brings a build's recorded status up to date with the cluster.
func (s *Server) refreshBuild(ctx context.Context, build *model.Build) {
	if s.build == nil || build.JobName == "" || build.Status.Terminal() {
		return
	}

	app, err := s.loadAppByID(ctx, build.AppID)
	if err != nil {
		return
	}

	status, reason, err := s.buildStatus(ctx, app.Namespace, build.JobName)
	if err != nil {
		slog.DebugContext(ctx, "could not read build status from the cluster",
			"build", build.ID, "error", err)
		return
	}
	// An empty status means the Job is gone; the recorded state is the better
	// answer and is left alone.
	if status == "" || status == build.Status {
		return
	}

	if err := s.store.SetBuildStatus(ctx, build.ID, status, reason); err != nil {
		slog.WarnContext(ctx, "could not record build status", "build", build.ID, "error", err)
		return
	}
	build.Status = status
	build.Reason = reason

	// Mark the image only once the build has pushed it, so nothing refers to an
	// image that does not exist yet.
	if status == model.BuildStatusSucceeded {
		image := s.imageFor(build.AppID, build.CommitSHA)
		if err := s.store.SetBuildImage(ctx, build.ID, image); err != nil {
			slog.WarnContext(ctx, "could not record build image", "build", build.ID, "error", err)
		}
		build.Image = image
		s.setAppStatus(ctx, build.AppID, model.AppStatusDeploying, "built "+shortSHA(build.CommitSHA))
	} else if status == model.BuildStatusFailed {
		s.setAppStatus(ctx, build.AppID, model.AppStatusBuildFailed, reason)
	}
}

// loadBuild reads the {build} path value and loads it.
func (s *Server) loadBuild(r *http.Request) (*model.Build, *apiError) {
	id := r.PathValue("build")
	if id == "" {
		return nil, BadRequest("no build id in the request path")
	}

	build, err := s.store.GetBuild(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, NotFound("build %q", id)
		}
		return nil, Errorf(http.StatusInternalServerError, "read build").Wrap(err)
	}

	// A build is addressed under its app, so the app in the path must match.
	// Without this, a build id from one app could be read through another app's
	// path — which matters because a key holder who guessed an id would otherwise
	// reach a build they were not looking at.
	if appID := r.PathValue("app"); appID != "" && appID != build.AppID {
		return nil, NotFound("build %q", id)
	}
	return build, nil
}

// setAppStatus records an app's status, logging rather than failing on error.
//
// A status write is derived information: failing an operation because AppLab
// could not record how it went would be worse than the stale status.
func (s *Server) setAppStatus(ctx context.Context, appID string, status model.AppStatus, reason string) {
	if err := s.store.SetAppStatus(ctx, appID, status, reason); err != nil {
		slog.WarnContext(ctx, "could not record app status", "app", appID, "status", status, "error", err)
	}
}

func (s *Server) setBuildStatus(ctx context.Context, buildID string, status model.BuildStatus, reason string) {
	if err := s.store.SetBuildStatus(ctx, buildID, status, reason); err != nil {
		slog.WarnContext(ctx, "could not record build status", "build", buildID, "status", status, "error", err)
	}
}

// shortSHA abbreviates a commit for a human-readable status message.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
