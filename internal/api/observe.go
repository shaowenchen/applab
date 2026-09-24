package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/shaowenchen/applab/internal/observe"
)

// handleListPods reports an app's pods.
func (s *Server) handleListPods(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}
	if s.observer == nil || !s.observer.Ready() {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment cannot observe: no cluster is configured"))
		return
	}

	limit, apiErr := boundedIntQuery(r, "limit", 100, 1000)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	pods, err := s.listPods(r.Context(), app.Namespace, app.ID, limit)
	if err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "list pods for app %q", app.ID).Wrap(err))
		return
	}

	respond(w, http.StatusOK, map[string]any{
		"app_id": app.ID,
		"pods":   pods,
		"count":  len(pods),
	})
}

// handlePodLogs streams or returns one container's log.
//
// `?follow=true` (the default) keeps the connection open and streams new output,
// which is what a caller watching a crash loop wants. `?follow=false` returns
// what exists and closes, which is what a script wants. The same endpoint serves
// both, so a caller does not have to know which it needs.
func (s *Server) handlePodLogs(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}
	if s.observer == nil || !s.observer.Ready() {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment cannot observe: no cluster is configured"))
		return
	}

	query := r.URL.Query()

	// Capped: a caller asking for a million lines is usually a bug on their side,
	// and serving it would be slow for everyone.
	tail, apiErr := boundedIntQuery(r, "tail", 500, 10000)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	since, apiErr := durationQuery(query.Get("since"))
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	opts := observe.LogOptions{
		Pod:       strings.TrimSpace(query.Get("pod")),
		Container: strings.TrimSpace(query.Get("container")),
		TailLines: int64(tail),
		Previous:  query.Get("previous") == "true",
		Since:     since,
	}

	follow := query.Get("follow") != "false"
	if !follow {
		logs, err := s.podLogs(r.Context(), app.Namespace, app.ID, opts)
		if err != nil {
			fail(w, r, observeError(err, app.ID))
			return
		}
		writeText(w, http.StatusOK, "text/plain", logs)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, r, Errorf(http.StatusInternalServerError, "log streaming is not supported by this server"))
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// Tells a buffering proxy not to hold the stream; without it the client sees
	// nothing until the container stops.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	if err := s.streamPodLogs(r.Context(), app.Namespace, app.ID, opts, w, flusher.Flush); err != nil {
		// The status line is already sent, so the marker in the body is the only
		// honest signal left.
		_, _ = fmt.Fprintf(w, "\n[AppLab] log stream ended: %v\n", err)
		flusher.Flush()
	}
}

// handleListEvents reports recent Kubernetes events for an app.
func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}
	if s.observer == nil || !s.observer.Ready() {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment cannot observe: no cluster is configured"))
		return
	}

	limit, apiErr := boundedIntQuery(r, "limit", 50, 500)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	events, err := s.listEvents(r.Context(), app.Namespace, app.ID, limit)
	if err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "list events for app %q", app.ID).Wrap(err))
		return
	}

	// The count of warnings is reported alongside, because "are there any
	// warnings" is the question a caller is actually asking and it should not
	// have to scan the list to answer it.
	warnings := 0
	for _, e := range events {
		if e.Type == "Warning" {
			warnings++
		}
	}

	respond(w, http.StatusOK, map[string]any{
		"app_id":   app.ID,
		"events":   events,
		"count":    len(events),
		"warnings": warnings,
	})
}

// handleDiagnose reports why an app is not working, in one call.
//
// It exists because answering "why is my app down" otherwise takes four
// requests — status, pods, events, logs — and a caller has to join them itself.
// The order of the checks is deliberate: each step only runs if the previous one
// did not already explain the problem, so the output stays short and the most
// likely cause comes first.
func (s *Server) handleDiagnose(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}
	if s.observer == nil || !s.observer.Ready() {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment cannot observe: no cluster is configured"))
		return
	}

	// The app's own record first, since it needs no cluster call and often
	// already explains things — nothing deployed, or a build that failed.
	diagnosis := map[string]any{
		"app_id": app.ID,
		"status": string(app.Status),
	}
	if app.StatusReason != "" {
		diagnosis["status_reason"] = app.StatusReason
	}
	if app.CommitSHA == "" {
		diagnosis["problem"] = "nothing has been deployed yet"
		diagnosis["next"] = "deploy a commit with POST /api/v1/apps/" + app.ID + "/deploy"
		respond(w, http.StatusOK, diagnosis)
		return
	}

	pods, podErr := s.listPods(r.Context(), app.Namespace, app.ID, 100)
	if podErr != nil {
		diagnosis["problem"] = "the cluster could not be read"
		diagnosis["error"] = podErr.Error()
		respond(w, http.StatusOK, diagnosis)
		return
	}
	if len(pods) == 0 {
		diagnosis["problem"] = "no pods are running for this app"
		diagnosis["next"] = "check that the deploy succeeded, or restart the app"
		diagnosis["events"] = s.eventsBestEffort(r, app.Namespace, app.ID, 10)
		respond(w, http.StatusOK, diagnosis)
		return
	}

	diagnosis["pods"] = pods

	// A pod that is not ready is the problem; its reason is the answer, and the
	// events explain how it got there.
	allReady := true
	for _, p := range pods {
		if !p.Ready {
			allReady = false
			break
		}
	}

	if !allReady {
		diagnosis["problem"] = "some pods are not ready"
		// The events explain *why* — an image pull failure, a failed scheduling,
		// a probe killing the container — which the pod state alone cannot.
		diagnosis["events"] = s.eventsBestEffort(r, app.Namespace, app.ID, 20)

		// A crash loop's reason is in the previous container's log, so that is
		// what gets attached rather than the current one's (which is usually
		// empty, since the container is restarting).
		if logs, err := s.podLogs(r.Context(), app.Namespace, app.ID, observe.LogOptions{
			Previous:  true,
			TailLines: 50,
		}); err == nil && strings.TrimSpace(logs) != "" {
			diagnosis["previous_logs"] = logs
		} else if logs, err := s.podLogs(r.Context(), app.Namespace, app.ID, observe.LogOptions{
			TailLines: 50,
		}); err == nil && strings.TrimSpace(logs) != "" {
			diagnosis["logs"] = logs
		}
		respond(w, http.StatusOK, diagnosis)
		return
	}

	diagnosis["problem"] = ""
	diagnosis["message"] = "all pods are ready"
	respond(w, http.StatusOK, diagnosis)
}

// eventsBestEffort returns events, or nil when they cannot be read.
//
// A diagnosis is still useful without them, and failing the whole call because
// one supplementary read failed would make the endpoint less reliable than the
// four calls it replaces.
func (s *Server) eventsBestEffort(r *http.Request, namespace, appID string, limit int) []observe.Event {
	events, err := s.listEvents(r.Context(), namespace, appID, limit)
	if err != nil {
		return nil
	}
	return events
}

// observeError maps an observe failure onto an HTTP error.
//
// The distinction that matters is whether the caller can fix it: a pod that does
// not exist or is not this app's is their mistake (404), while a cluster read
// failing is ours.
func observeError(err error, appID string) *apiError {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "has no pods"):
		return Conflict("app %q has no pods; it may not be deployed", appID)
	case strings.Contains(msg, "is not part of app"):
		return NotFound("%s", msg)
	case strings.Contains(msg, "has no container"):
		return BadRequest("%s", msg)
	default:
		return Errorf(http.StatusInternalServerError, "read logs for app %q", appID).Wrap(err)
	}
}

// boundedIntQuery reads a query parameter and caps it.
//
// The cap is applied rather than reported: a caller wanting more than the
// maximum is asking for something AppLab will not serve either way, and an error
// would only make them retry with the same value.
func boundedIntQuery(r *http.Request, name string, def, max int) (int, *apiError) {
	value, apiErr := intQuery(r, name, def)
	if apiErr != nil {
		return 0, apiErr
	}
	if value > max {
		return max, nil
	}
	return value, nil
}

// durationQuery parses a Go duration such as "5m" or "1h".
func durationQuery(raw string) (time.Duration, *apiError) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, BadRequest("the since parameter must be a duration such as 5m or 1h")
	}
	return d, nil
}
