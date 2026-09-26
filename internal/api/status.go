package api

import (
	"context"
	"log/slog"

	"github.com/shaowenchen/applab/internal/deploy"
	"github.com/shaowenchen/applab/internal/model"
)

// appStatus derives an app's status from what is actually there.
//
// The cluster is the authority on what is running, and the bucket's build
// records supply the one thing the cluster cannot know: that a build is in
// flight. Everything else — whether a Deployment exists, whether it is up,
// whether it cannot progress — is read from the cluster rather than remembered,
// because remembering it is what made a fresh install report every app as
// running with nothing behind it.
//
// buildInFlight is passed in rather than looked up here so a listing pays for it
// once. The routine is pure otherwise, which is what lets the list and the
// detail view be asserted to agree.
func appStatus(live map[string]deploy.Status, appID string, buildInFlight bool) model.AppStatus {
	if buildInFlight {
		// Ahead of the cluster, because a build that is running is a newer fact
		// than the Deployment it will replace: the app is about to change, and
		// reporting the old rollout's outcome would be reporting the past.
		return model.AppStatusBuilding
	}

	status, deployed := live[appID]
	if !deployed || !status.Found {
		// No Deployment. Either it was never deployed or it was stopped, and the
		// two are indistinguishable from outside — which is the right answer for
		// both, since both mean "nothing is running".
		return model.AppStatusCreated
	}

	switch {
	case status.Message != "":
		// A rollout that cannot progress. This is checked before availability
		// because a Deployment can be "available" from a previous revision while
		// its new pods fail to start — reporting the old success would hide the
		// thing worth looking at.
		return model.AppStatusFailed
	case status.Available:
		return model.AppStatusRunning
	default:
		return model.AppStatusDeploying
	}
}

// appStatuses resolves the status of every app in one pass.
//
// It is what the list and the overview call: the caller passes the cluster's
// answer in, which it read once for the whole page — see liveStatus — and this
// adds the one thing the cluster cannot know.
//
// The returned map is keyed by app id and holds an entry for every app given, so
// a caller never has to distinguish "no status" from "not asked about". A nil or
// missing entry in live means no Deployment, which is what a caller with no
// cluster passes for everything.
func (s *Server) appStatuses(ctx context.Context, apps []*model.App, live map[string]deploy.Status) map[string]model.AppStatus {
	out := make(map[string]model.AppStatus, len(apps))

	building := s.buildsInFlight(ctx, apps)

	for _, app := range apps {
		out[app.ID] = appStatus(live, app.ID, building[app.ID])
	}
	return out
}

// liveStatusesFor reads one app's Deployment.
//
// The single-app counterpart to liveStatus, and separate from it rather than
// listing and picking one entry: reading one Deployment by name is cheaper than
// listing every app's, which is the case that matters — a page showing one app
// should not read the whole platform's Deployments.
func (s *Server) liveStatusesFor(ctx context.Context, app *model.App) deploy.Status {
	return s.liveStatus(ctx, app.ID)[app.ID]
}

// liveStatus reads the cluster once and returns what it says, keyed by app id.
//
// A route rendering many apps passes nothing and gets the whole namespace in one
// list; a route rendering one passes its id and gets a single read, which is
// cheaper than listing everything to pick out one entry.
//
// A nil or missing entry means no Deployment. With no cluster — or one that
// cannot be reached — the answer is nil for every app, which is honest rather
// than an error: nothing is known to be running, and the question "what is up"
// has a true answer even when the cluster is down.
func (s *Server) liveStatus(ctx context.Context, appIDs ...string) map[string]deploy.Status {
	if s.deployer == nil || !s.deployer.Ready() {
		return nil
	}

	// Asked for by name, one read is cheaper than a list. Everything else gets
	// the list, which answers for every app at once.
	if len(appIDs) == 1 {
		app := &model.App{ID: appIDs[0], Namespace: s.cfg.Namespace}
		live, err := s.deployer.Status(ctx, app)
		if err != nil {
			slog.WarnContext(ctx, "could not read an app's live status from the cluster",
				"app", appIDs[0], "error", err)
			return nil
		}
		return map[string]deploy.Status{appIDs[0]: live}
	}

	live, err := s.deployer.Statuses(ctx, s.cfg.Namespace)
	if err != nil {
		slog.WarnContext(ctx, "could not read live app statuses from the cluster", "error", err)
		return nil
	}
	return live
}

// buildInFlight reports whether one app has a build that has not finished.
func (s *Server) buildInFlight(ctx context.Context, appID string) bool {
	builds, err := s.store.ListUnfinishedBuildsForApp(ctx, appID)
	if err != nil {
		slog.DebugContext(ctx, "could not list unfinished builds for an app", "app", appID, "error", err)
		return false
	}
	return len(builds) > 0
}

// buildsInFlight reports which apps have a build that has not finished.
//
// This is the one piece of status that is not the cluster's to answer. A build
// Job exists while it runs, but its outcome after it is collected is recorded in
// the bucket — see store.ListUnfinishedBuilds — and "is a build running" is a
// question about AppLab's own queue rather than about the cluster.
func (s *Server) buildsInFlight(ctx context.Context, apps []*model.App) map[string]bool {
	out := map[string]bool{}
	for _, app := range apps {
		out[app.ID] = s.buildInFlight(ctx, app.ID)
	}
	return out
}
