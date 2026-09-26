package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/observe"
)

// overviewResponse is what GET /api/v1/overview returns.
//
// It is the one call that answers "what is the state of this platform" without
// visiting every app in turn. Every field here is something that could be
// assembled by a client from other endpoints only by listing everything and
// tallying in the browser — which is the work this endpoint exists to do once,
// server-side, where the database can count instead of transfer.
//
// It deliberately carries no app list: the Apps view already has GET /api/v1/apps,
// and duplicating it here would mean two answers to the same question.
type overviewResponse struct {
	Apps    appsSummary    `json:"apps"`
	Builds  buildsSummary  `json:"builds"`
	Cluster clusterSummary `json:"cluster"`

	// Deployment is the same self-description GET /api/v1/config returns. It is
	// built from the same values, so the two cannot disagree about what this
	// deployment is.
	Deployment configResponse `json:"deployment"`
}

// appsSummary counts apps by status.
//
// The named fields are the ones a dashboard shows; "total" counts live apps
// (deleted ones are excluded, since a row kept so an id is not reused is not an
// app that exists). Every named status is present even at zero, so a client can
// render a fixed set of tiles without treating a missing key as an error.
type appsSummary struct {
	Total          int `json:"total"`
	Created        int `json:"created"`
	Building       int `json:"building"`
	Deploying      int `json:"deploying"`
	Running        int `json:"running"`
	Failed         int `json:"failed"`
	BuildFailed    int `json:"build_failed"`
	NeedsAttention int `json:"needs_attention"`
}

// buildsSummary counts builds and carries the most recent ones.
type buildsSummary struct {
	Total     int             `json:"total"`
	Pending   int             `json:"pending"`
	Running   int             `json:"running"`
	Succeeded int             `json:"succeeded"`
	Failed    int             `json:"failed"`
	Recent    []buildResponse `json:"recent"`
}

// clusterSummary reports whether the cluster half is usable.
//
// Configured and reachable are separate questions and are reported separately,
// because the answers differ in what they mean. A deployment with no cluster is
// a legitimate way to run AppLab — the API and the source half work — so it is
// not a failure. A deployment with a cluster it cannot reach is a failure, and
// is the one an operator needs to see. Collapsing the two into one boolean
// would make "not configured" look like "broken".
type clusterSummary struct {
	// Configured is whether a cluster client was attached at startup.
	Configured bool `json:"configured"`

	// Reachable is whether the API server answered. False when not configured:
	// an unreachable cluster is not reachable.
	Reachable bool `json:"reachable"`
}

// handleOverview reports the platform at a glance.
//
// It is authenticated like every other data route: the numbers describe this
// deployment's apps, which is exactly what a key protects everywhere else.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	// Counted from the derived status rather than from the store: a status is a
	// fact about the cluster now, so counting what the bucket holds would count
	// something that is not the question. It costs one list of Deployments for
	// the whole page.
	allApps, err := s.store.ListApps(r.Context())
	if err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "could not list apps").Wrap(err))
		return
	}
	appStatuses := s.appStatuses(r.Context(), allApps, s.liveStatus(r.Context()))

	appCounts := map[model.AppStatus]int{}
	for _, status := range appStatuses {
		appCounts[status]++
	}

	buildCounts, err := s.store.CountBuildsByStatus(r.Context())
	if err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "could not count builds").Wrap(err))
		return
	}

	recentBuilds, err := s.store.ListRecentBuilds(r.Context(), overviewRecentBuilds)
	if err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "could not list recent builds").Wrap(err))
		return
	}

	// A build in flight is the one case where the overview would otherwise report
	// a build with nothing to show for what it is doing, so the pods are read
	// once for the whole panel rather than per row.
	pods := s.allBuildPods(r.Context())

	// A nil slice encodes as JSON null, which a client would have to special-case
	// before iterating. An empty list is the honest answer for a deployment with
	// no builds yet and needs no branch on the other side.
	recent := make([]buildResponse, 0, len(recentBuilds))
	for _, b := range recentBuilds {
		recent = append(recent, toBuildResponse(b, pods))
	}

	apps := appsSummary{
		Created:     appCounts[model.AppStatusCreated],
		Building:    appCounts[model.AppStatusBuilding],
		Deploying:   appCounts[model.AppStatusDeploying],
		Running:     appCounts[model.AppStatusRunning],
		Failed:      appCounts[model.AppStatusFailed],
		BuildFailed: appCounts[model.AppStatusBuildFailed],
	}
	// Every app that exists is counted: DeleteApp removes the record outright,
	// so there is no tombstone to exclude.
	for _, n := range appCounts {
		apps.Total += n
	}
	// The single number a person actually acts on: an app that is not running
	// because something went wrong. "building" and "deploying" are deliberately
	// not counted — work in progress is not a problem to look at.
	apps.NeedsAttention = apps.Failed + apps.BuildFailed

	builds := buildsSummary{
		Pending:   buildCounts[model.BuildStatusPending],
		Running:   buildCounts[model.BuildStatusRunning],
		Succeeded: buildCounts[model.BuildStatusSucceeded],
		Failed:    buildCounts[model.BuildStatusFailed],
		Recent:    recent,
	}
	for _, n := range buildCounts {
		builds.Total += n
	}

	cluster := clusterSummary{Configured: s.clusterReady != nil}
	if s.clusterReady != nil {
		cluster.Reachable = s.clusterReady(r.Context())
	}

	respond(w, http.StatusOK, overviewResponse{
		Apps:       apps,
		Builds:     builds,
		Cluster:    cluster,
		Deployment: s.configResponse(r),
	})
}

// allBuildPods reads every app's build pods, for the recent-builds panel.
//
// Same contract as buildPods: no cluster or an unreachable one means no pods,
// because the build records still deserve to be shown.
func (s *Server) allBuildPods(ctx context.Context) map[string]observe.Pod {
	if s.observer == nil || !s.observer.Ready() {
		return nil
	}
	pods, err := s.observer.AllBuildPods(ctx, s.cfg.Namespace)
	if err != nil {
		slog.WarnContext(ctx, "could not read build pods; recent builds are reported without them", "error", err)
		return nil
	}
	return pods
}

// overviewRecentBuilds is how many recent builds the overview carries.
//
// Enough to fill a dashboard panel without turning the endpoint into a log
// dump: a caller that wants an app's full build history has
// GET /apps/{app}/builds, which is paged and per-app.
const overviewRecentBuilds = 10
