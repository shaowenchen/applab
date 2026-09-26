package api

import (
	"context"
	"log/slog"

	"github.com/shaowenchen/applab/internal/build"
	"github.com/shaowenchen/applab/internal/deploy"
	"github.com/shaowenchen/applab/internal/model"
)

// runStatus is where the app is *running*: the cluster's answer and nothing
// else.
//
// It is the half of appStatus that is about the Deployment, split out because
// the two halves answer different questions and a listing now shows both side by
// side. "Is it serving" and "did the last build work" fail independently — a
// failed build leaves the previous revision running, and a successful build
// changes nothing until a deploy applies it — so a single status that folded
// them together had to throw one away whichever it chose.
func runStatus(live map[string]deploy.Status, appID string) model.AppStatus {
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

// appStatus derives an app's status from what is actually there.
//
// Everything it uses is a fact about the cluster: whether a Deployment exists,
// whether it is up, whether it cannot progress, and whether a build is running.
// AppLab records none of it, because recording it is what made a fresh install
// report every app as running with nothing behind it.
//
// buildInFlight is passed in rather than looked up here so a listing pays for it
// once. The routine is pure otherwise, which is what lets the list and the
// detail view be asserted to agree.
func appStatus(live map[string]deploy.Status, appID string, buildInFlight bool) model.AppStatus {
	if buildInFlight {
		// Ahead of the cluster, because a build that is running is a newer fact
		// than the Deployment it will replace: the app is about to change, and
		// reporting the old rollout's outcome would be reporting the past.
		//
		// This one status is the summary shown on an app's own page and by
		// `applab status`, where there is room for one answer and "something is
		// happening to this app" is what someone wants from it. The listing shows
		// the two facts separately instead — see runStatus and buildStatus.
		return model.AppStatusBuilding
	}
	return runStatus(live, appID)
}

// appRuntimes resolves what the cluster says about every app in one pass.
//
// It is what the list, the overview and the describe document call: the caller
// passes the Deployments in, which it read once for the whole page — see
// liveStatus — and this adds the one thing the cluster cannot know without a
// second read, which is what the latest build did.
//
// The returned map is keyed by app id and holds an entry for every app given, so
// a caller never has to distinguish "no status" from "not asked about". A nil or
// missing entry in live means no Deployment, which is what a caller with no
// cluster passes for everything.
//
// The build half costs one listing for the whole page rather than one per app:
// build Jobs carry the app label, so a page of fifty apps is the same read as a
// page of one. That is why each app's *latest* build is what is read here rather
// than its whole history — the history is per app, and would be fifty reads.
func (s *Server) appRuntimes(ctx context.Context, apps []*model.App, live map[string]deploy.Status) map[string]appRuntime {
	out := make(map[string]appRuntime, len(apps))
	latest := s.latestBuilds(ctx)

	for _, app := range apps {
		run := runStatus(live, app.ID)

		var buildStatus model.BuildStatus
		if b, ok := latest[app.ID]; ok {
			buildStatus = b.Status
		}

		// The folded summary, which is what an app's own page shows: a build in
		// flight outranks the cluster's answer, because a build that is running
		// is a newer fact than the Deployment it is about to replace.
		status := run
		if buildStatus == model.BuildStatusPending || buildStatus == model.BuildStatusRunning {
			status = model.AppStatusBuilding
		}

		out[app.ID] = appRuntime{
			Status:      status,
			RunStatus:   run,
			BuildStatus: buildStatus,
			Live:        live[app.ID],
		}
	}
	return out
}

// latestBuilds reads each app's most recent build, keyed by app id.
//
// Empty when this deployment cannot build, which is a real configuration and not
// an error: an installation that deploys images built elsewhere has no build
// Jobs and no build status to report.
func (s *Server) latestBuilds(ctx context.Context) map[string]build.Result {
	if !s.canBuild() {
		return nil
	}
	latest, err := s.build.LatestPerApp(ctx, s.cfg.Namespace)
	if err != nil {
		// Logged rather than fatal: the build column goes blank and every other
		// column still renders. Failing the whole listing because one read
		// blinked would hide the apps themselves.
		slog.DebugContext(ctx, "could not read the latest builds", "error", err)
		return nil
	}
	return latest
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
//
// It reads the build Jobs, which are the only record of a build: a Job that is
// still running is a build that is still running, and there is nothing to
// consult beside it.
func (s *Server) buildInFlight(ctx context.Context, app *model.App) bool {
	if !s.canBuild() {
		return false
	}
	unfinished, err := s.build.Unfinished(ctx, app.Namespace, app.ID)
	if err != nil {
		slog.DebugContext(ctx, "could not list an app's unfinished builds", "app", app.ID, "error", err)
		return false
	}
	return len(unfinished) > 0
}

// buildsInFlight reports which apps have a build that has not finished.
//
// One listing answers for every app rather than one per app: build Jobs carry
// the app label, so a page of apps costs the same single read a page of one app
// does.
func (s *Server) buildsInFlight(ctx context.Context, apps []*model.App) map[string]bool {
	out := make(map[string]bool, len(apps))
	if !s.canBuild() {
		for _, app := range apps {
			out[app.ID] = false
		}
		return out
	}

	inFlight, err := s.build.InFlight(ctx, s.cfg.Namespace)
	if err != nil {
		slog.DebugContext(ctx, "could not list the builds in flight", "error", err)
	}

	for _, app := range apps {
		out[app.ID] = inFlight[app.ID]
	}
	return out
}
