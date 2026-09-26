package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shaowenchen/applab/internal/build"
	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/observe"
	"github.com/shaowenchen/applab/internal/source"
)

// buildResponse is a build as the API presents it.
type buildResponse struct {
	ID        string `json:"id"`
	AppID     string `json:"app_id"`
	CommitSHA string `json:"commit_sha"`
	Branch    string `json:"branch,omitempty"`
	Status    string `json:"status"`

	Image   string `json:"image,omitempty"`
	JobName string `json:"job_name,omitempty"`
	Reason  string `json:"reason,omitempty"`

	// Pod is the pod the build is running in, when there is one to report.
	//
	// A build's pod is not an instance of the app, so it is not in the app's pod
	// endpoint — it is here, on the build it belongs to. The fields are the ones
	// a pod is already reported with, so a console renders a build's pod with the
	// same code it renders an app's.
	//
	// Absent for a build that has finished and had its Job collected, which is
	// most of them, and for a deployment that cannot observe a cluster.
	Pod *observe.Pod `json:"pod,omitempty"`

	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// toBuildResponse renders a build. pods is the app's build pods keyed by build
// id, and may be nil — which is what a deployment with no cluster passes, and
// what makes the pod field absent rather than empty. image is the image the
// build produced, empty for anything that has not succeeded.
func toBuildResponse(b *build.Result, image string, pods map[string]observe.Pod) buildResponse {
	resp := buildResponse{
		ID:        b.ID,
		AppID:     b.AppID,
		CommitSHA: b.CommitSHA,
		Branch:    b.Branch,
		Status:    string(b.Status),
		Image:     image,
		JobName:   b.JobName,
		Reason:    b.Reason,
		CreatedAt: b.CreatedAt,
	}
	if pod, ok := pods[b.ID]; ok {
		resp.Pod = &pod
	}
	if !b.StartedAt.IsZero() {
		resp.StartedAt = b.StartedAt
	}
	if !b.FinishedAt.IsZero() {
		resp.FinishedAt = b.FinishedAt
	}
	return resp
}

// buildResponses renders a set of builds, reusing one pod map.
func (s *Server) buildResponses(app *model.App, builds []build.Result, pods map[string]observe.Pod) []buildResponse {
	out := make([]buildResponse, 0, len(builds))
	for i := range builds {
		out = append(out, toBuildResponse(&builds[i], s.imageForResult(app.ID, &builds[i]), pods))
	}
	return out
}

// imageForResult names the image a build produced.
//
// It is derived rather than stored, and only for a build that succeeded: the
// image is a function of the app, the commit and the build configuration — which
// is what makes an unchanged commit an unchanged image — and a build that has
// not succeeded has pushed nothing, so naming one would point a caller at a tag
// that is not in the registry.
func (s *Server) imageForResult(appID string, b *build.Result) string {
	if b.Status != model.BuildStatusSucceeded || b.CommitSHA == "" {
		return ""
	}
	return s.imageFor(appID, b.CommitSHA)
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
	if !s.canBuild() {
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
	resolved, err := s.resolveCommit(r.Context(), app.ID, branch, commitSHA)
	if err != nil {
		fail(w, r, NotFound("commit %q in app %q", commitSHA, app.ID))
		return
	}

	buildID, jobName, apiErr := s.startBuild(r.Context(), app, branch, resolved)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	slog.InfoContext(r.Context(), "build started",
		"app", app.ID, "build", buildID, "commit", resolved, "job", jobName)

	// A build that was just started has no pod yet in almost every case — the Job
	// was created a moment ago — so this reads for the answer rather than
	// assuming it, and the field is simply absent when there is nothing yet.
	s.respondBuild(w, r, http.StatusAccepted, app, buildID)
}

// startBuild creates the Job for one build and returns its id and name.
//
// There is no record written first, and that is the change: the Job is the
// record. It used to be written to the object store before the Job so that a Job
// which existed always had something that knew about it — which meant a crash
// between the two writes left a record of a build that never ran, and a build
// history that had to be reconciled against the cluster at every startup. A Job
// that exists is now the only thing that says a build exists.
//
// The branch is what the build clones. A commit can be reachable from more than
// one branch, and each branch is stored as its own repository, so app and commit
// alone do not name one.
func (s *Server) startBuild(ctx context.Context, app *model.App, branch, commitSHA string) (string, string, *apiError) {
	buildID, err := model.NewID()
	if err != nil {
		return "", "", Errorf(http.StatusInternalServerError, "generate build id").Wrap(err)
	}

	// No provisioning step: the namespace already exists, because it is the one
	// AppLab runs in, and the registry credentials a Job pushes with are already
	// there for the same reason.

	// The build clones with the app's own key, read here because the Job is
	// created a few lines below and a key that could not be read is a build that
	// fails after it has already been started.
	//
	// It is the app's key rather than something minted for the build. A build
	// used to be handed a single-use token scoped to one commit, which is what
	// the fetch step wanted when it was one authenticated HTTP GET; the clone is
	// a git clone, which is many authenticated requests, and a credential
	// consumed by the first one cannot work. The reach is therefore the app's
	// repository — every branch and commit of it — and no other app.
	appKey, err := s.appKeys.Get(ctx, app.ID)
	if err != nil {
		return "", "", Errorf(http.StatusInternalServerError, "read the app's key, which the build clones with").Wrap(err)
	}

	jobName, err := s.startBuildJob(ctx, app, branch, buildID, commitSHA, appKey)
	if s.metrics != nil {
		s.metrics.ObserveBuild(err != nil)
	}
	if err != nil {
		return "", "", Errorf(http.StatusInternalServerError, "start the build job").Wrap(err)
	}

	// Any other build of this app is now the older one. Stopping it here as well
	// as on upload is what makes the invariant hold for every way a build can
	// start: two of them running at once would race to push the same image tag,
	// and which one won would be whichever finished last.
	//
	// This build is excluded by id — it is in the unfinished list already.
	s.supersedeOtherBuilds(ctx, app, buildID)

	return buildID, jobName, nil
}

// respondBuild reads a build back from the cluster and writes it.
//
// The build has just been created, so this is a read of the Job that was just
// created rather than of anything AppLab kept. A read that fails is reported as
// a failure: the Job exists and is running, and answering with a build AppLab
// assembled from what it remembers would be exactly the kind of second copy of a
// cluster fact this design removes.
func (s *Server) respondBuild(w http.ResponseWriter, r *http.Request, status int, app *model.App, buildID string) {
	result, err := s.build.Get(r.Context(), app.Namespace, app.ID, buildID)
	if err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "read back the build just started").Wrap(err))
		return
	}
	respond(w, status, toBuildResponse(result, s.imageForResult(app.ID, result), s.buildPods(r.Context(), app)))
}

// supersedeOtherBuilds stops every build of an app except one.
func (s *Server) supersedeOtherBuilds(ctx context.Context, app *model.App, exceptBuildID string) {
	if !s.canBuild() {
		return
	}

	unfinished, err := s.build.Unfinished(ctx, app.Namespace, app.ID)
	if err != nil {
		slog.WarnContext(ctx, "could not list the builds in flight", "app", app.ID, "error", err)
		return
	}

	for i := range unfinished {
		b := &unfinished[i]
		if b.ID == exceptBuildID || b.JobName == "" {
			continue
		}
		if err := s.build.Cancel(ctx, app.Namespace, b.JobName); err != nil {
			slog.WarnContext(ctx, "could not stop a superseded build",
				"app", app.ID, "build", b.ID, "job", b.JobName, "error", err)
			continue
		}
		slog.InfoContext(ctx, "stopped a superseded build",
			"app", app.ID, "build", b.ID, "job", b.JobName)
	}
}

// handleCancelBuild stops a running build.
//
// The same operation the upload path performs, offered explicitly: someone may
// simply want the build to stop — it is pushing a bad commit, or it is wedged —
// without uploading anything.
func (s *Server) handleCancelBuild(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}
	if !s.canBuild() {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment cannot build"))
		return
	}

	buildID := r.PathValue("build")
	result, apiErr := s.loadBuild(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	// The cluster is the only record, so "has it already finished" is answered by
	// reading the Job — which loadBuild has just done, and which reports a build
	// that finished while nothing was watching as finished rather than as still
	// running.
	if result.Status.Terminal() {
		fail(w, r, Conflict("build %s has already finished (%s)", shortSHA(buildID), result.Status))
		return
	}

	if err := s.build.Cancel(r.Context(), app.Namespace, result.JobName); err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "stop build job %s", result.JobName).Wrap(err))
		return
	}

	slog.InfoContext(r.Context(), "build stopped", "app", app.ID, "build", buildID, "job", result.JobName)

	// Read back, so the answer is the state the cluster reports after the delete
	// rather than what this handler believes it did.
	s.respondBuild(w, r, http.StatusOK, app, buildID)
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
// Nothing is marked cancelled, and nothing needs to be: deleting the Job is the
// whole of it. There is no record to relabel, and the build will not reappear in
// a listing, because a listing reads Jobs and this one is gone.
func (s *Server) supersedeBuilds(ctx context.Context, app *model.App) {
	if !s.canBuild() {
		return
	}

	unfinished, err := s.build.Unfinished(ctx, app.Namespace, app.ID)
	if err != nil {
		slog.WarnContext(ctx, "could not list the builds in flight", "app", app.ID, "error", err)
		return
	}

	for i := range unfinished {
		b := &unfinished[i]
		if b.JobName == "" {
			continue
		}

		if err := s.build.Cancel(ctx, app.Namespace, b.JobName); err != nil {
			slog.WarnContext(ctx, "could not stop the build in flight",
				"app", app.ID, "build", b.ID, "job", b.JobName, "error", err)
			continue
		}

		slog.InfoContext(ctx, "stopped the build in flight to make way for an upload",
			"app", app.ID, "build", b.ID, "job", b.JobName)
	}
}

func (s *Server) handleGetBuild(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}
	result, apiErr := s.loadBuild(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	respond(w, http.StatusOK, toBuildResponse(result, s.imageForResult(app.ID, result), s.buildPods(r.Context(), app)))
}

// buildPods reads the pods the app's builds are running in, keyed by build id.
//
// Returning an empty map rather than nil for a deployment with no cluster keeps
// every caller from having to distinguish "cannot observe" from "nothing
// running": both mean no pod to show, and a build row renders the same either
// way.
//
// An unreachable cluster is logged and reported as no pods rather than as a
// failure. This is the one read on the build path that is decoration: the builds
// themselves come from the Jobs, and failing a listing because the pod half
// blinked would hide builds that are perfectly well recorded.
func (s *Server) buildPods(ctx context.Context, app *model.App) map[string]observe.Pod {
	if s.observer == nil || !s.observer.Ready() {
		return nil
	}
	pods, err := s.observer.BuildPods(ctx, app.Namespace, app.ID)
	if err != nil {
		slog.WarnContext(ctx, "could not read build pods; builds are reported without them",
			"app", app.ID, "error", err)
		return nil
	}
	return pods
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

	if !s.canBuild() {
		// No cluster, so no Jobs and no history. An empty list rather than 501:
		// "this app has no builds" is true, and a listing that failed would make
		// a console render an error where a plain empty table belongs.
		respond(w, http.StatusOK, []buildResponse{})
		return
	}

	builds, err := s.build.List(r.Context(), app.Namespace, app.ID, limit)
	if err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "list builds").Wrap(err))
		return
	}

	respond(w, http.StatusOK, s.buildResponses(app, builds, s.buildPods(r.Context(), app)))
}

// handleBuildLogs streams a build's log.
//
// The log is followed while the build runs and the response ends when it
// finishes, so a caller can watch a build with one request rather than polling.
// When the build has already finished the log is returned and the response
// closes immediately — the same call works either way, which is what makes it
// usable from a script that does not know the build's state.
func (s *Server) handleBuildLogs(w http.ResponseWriter, r *http.Request) {
	result, apiErr := s.loadBuild(r)
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

	if err := s.streamBuildLog(r.Context(), w, flusher, result, follow); err != nil {
		// Headers are already sent, so the only honest signal left is a marker in
		// the body. It is written in a form a reader will notice rather than as a
		// bare error code.
		_, _ = fmt.Fprintf(w, "\n[AppLab] log stream ended: %v\n", err)
		flusher.Flush()
	}
}

// streamBuildLog writes a build's log, optionally following it to completion.
func (s *Server) streamBuildLog(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, build *build.Result, follow bool) error {
	app, err := s.loadAppByID(ctx, build.AppID)
	if err != nil {
		return fmt.Errorf("read the app this build belongs to: %w", err)
	}
	if build.JobName == "" {
		return fmt.Errorf("this build has no job to read a log from")
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
	// A build writes progress continuously and there is no streaming API in the
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

// loadBuild reads the {build} path value and loads it from the cluster.
//
// The app id comes from the path, and it is a real check rather than a
// redundancy: the Job is looked up by a selector naming both, so a build id from
// one app cannot be read through another app's path — the Job is simply not
// selected.
func (s *Server) loadBuild(r *http.Request) (*build.Result, *apiError) {
	id := r.PathValue("build")
	if id == "" {
		return nil, BadRequest("no build id in the request path")
	}
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		return nil, apiErr
	}
	if !s.canBuild() {
		return nil, Errorf(http.StatusNotImplemented, "this deployment cannot build")
	}

	result, err := s.build.Get(r.Context(), app.Namespace, app.ID, id)
	if err != nil {
		if errors.Is(err, build.ErrBuildNotFound) {
			return nil, NotFound("build %q", id)
		}
		return nil, Errorf(http.StatusInternalServerError, "read build").Wrap(err)
	}
	return result, nil
}

// shortSHA abbreviates a commit for a human-readable status message.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
