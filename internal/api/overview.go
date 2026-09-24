package api

import (
	"net/http"

	"github.com/shaowenchen/applab/internal/model"
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
// a legitimate way to run applab — the API and the source half work — so it is
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
	appCounts, err := s.store.CountAppsByStatus(r.Context())
	if err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "could not count apps").Wrap(err))
		return
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

	// A nil slice encodes as JSON null, which a client would have to special-case
	// before iterating. An empty list is the honest answer for a deployment with
	// no builds yet and needs no branch on the other side.
	recent := make([]buildResponse, 0, len(recentBuilds))
	for _, b := range recentBuilds {
		recent = append(recent, toBuildResponse(b))
	}

	apps := appsSummary{
		Created:     appCounts[model.AppStatusCreated],
		Building:    appCounts[model.AppStatusBuilding],
		Deploying:   appCounts[model.AppStatusDeploying],
		Running:     appCounts[model.AppStatusRunning],
		Failed:      appCounts[model.AppStatusFailed],
		BuildFailed: appCounts[model.AppStatusBuildFailed],
	}
	// Deleted apps are excluded by the query, so the total is the sum of what it
	// returned rather than a second COUNT(*) that could disagree with it.
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

// overviewRecentBuilds is how many recent builds the overview carries.
//
// Enough to fill a dashboard panel without turning the endpoint into a log
// dump: a caller that wants an app's full build history has
// GET /apps/{app}/builds, which is paged and per-app.
const overviewRecentBuilds = 10
