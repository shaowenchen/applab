package api

import (
	"context"
	"log/slog"

	"github.com/shaowenchen/applab/internal/model"
)

// ReconcileBuilds brings builds left in a non-terminal state by a restart up to
// date with the cluster.
//
// AppLab has no controller watching Jobs: a build's outcome is recorded when
// something asks. If the process restarts while a build is running, nothing is
// left to notice that it finished, and the build would read as "running"
// forever — so a caller polling it would wait indefinitely for a Job that ended
// minutes ago.
//
// It runs at startup rather than on a timer because that is when the problem is
// created. A build that starts and finishes while the process is up is handled
// by the endpoint that reads it; only a restart can orphan one.
func (s *Server) ReconcileBuilds(ctx context.Context) {
	if s.build == nil {
		return
	}

	builds, err := s.store.ListUnfinishedBuilds(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "could not list unfinished builds to reconcile", "error", err)
		return
	}
	if len(builds) == 0 {
		return
	}

	slog.Info("reconciling builds left unfinished by a restart", "count", len(builds))

	for _, b := range builds {
		// A build with no Job name never got as far as recording one, so there is
		// nothing in the cluster this can look up. It is failed rather than
		// retried: retrying on every restart would loop, and the caller can ask
		// again.
		//
		// The window that leaves is between the record being written and the Job's
		// name being recorded against it — two writes, so a crash in between
		// leaves a running Job whose name nothing knows. The Job's own TTL
		// collects it, and the build reads as failed rather than as running
		// forever, which is the safer of the two ways to be wrong.
		if b.JobName == "" {
			s.setBuildStatus(ctx, b.AppID, b.ID, model.BuildStatusFailed,
				"applab restarted before the build job was started")
			s.setAppStatus(ctx, b.AppID, model.AppStatusBuildFailed, "applab restarted during the build")
			continue
		}

		app, err := s.loadAppByID(ctx, b.AppID)
		if err != nil {
			slog.WarnContext(ctx, "could not reconcile a build: its app is gone",
				"build", b.ID, "app", b.AppID, "error", err)
			continue
		}

		status, reason, err := s.buildStatus(ctx, app.Namespace, b.JobName)
		if err != nil {
			slog.WarnContext(ctx, "could not read a build's job state while reconciling",
				"build", b.ID, "job", b.JobName, "error", err)
			continue
		}

		switch {
		case status.Terminal():
			s.setBuildStatus(ctx, b.AppID, b.ID, status, reason)
			appStatus := model.AppStatusDeploying
			if status == model.BuildStatusFailed {
				appStatus = model.AppStatusBuildFailed
			}
			s.setAppStatus(ctx, b.AppID, appStatus, reason)
			slog.InfoContext(ctx, "reconciled a build", "build", b.ID, "status", status)

		case status == "":
			// The Job is gone entirely — its TTL elapsed while AppLab was down.
			// The outcome is unknowable, and claiming success would let a caller
			// deploy an image that may never have been pushed.
			s.setBuildStatus(ctx, b.AppID, b.ID, model.BuildStatusFailed,
				"the build job finished while applab was restarting and its outcome could not be determined")
			s.setAppStatus(ctx, b.AppID, model.AppStatusBuildFailed,
				"the build outcome could not be determined after a restart")

		default:
			// Still running: record the state so the app's status is accurate, and
			// leave it to the build endpoint to finish reporting.
			if err := s.store.SetBuildStatus(ctx, b.AppID, b.ID, status, reason); err != nil {
				slog.WarnContext(ctx, "could not record build status", "build", b.ID, "error", err)
			}
		}
	}
}
