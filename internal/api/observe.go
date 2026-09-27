package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/shaowenchen/applab/internal/deploy"
	"github.com/shaowenchen/applab/internal/k8s"
	"github.com/shaowenchen/applab/internal/observe"
)

// usageResponse is what both usage routes return.
//
// It embeds the app-level Usage so every field it had is still where it was —
// available, the cpu/memory totals, requested and limited — and adds the
// per-pod readings a picker needs. The totals are what a caller shows with
// nothing selected; Pods is what it shows when one is.
//
// The map is keyed by pod name, which is the same key the pods endpoints
// report, so a caller joins the two by name rather than by position.
//
// The platform route returns this too, with requested and limited absent. That
// is why they are the omitempty halves of a struct rather than required fields:
// the two panels are one piece of console code, and one shape is what keeps them
// that way.
type usageResponse struct {
	observe.Usage

	// Pods is each pod's own CPU and memory. Absent rather than empty on a
	// cluster that cannot report metrics, which is the same distinction
	// Available draws: "no pods" and "no metrics here" are different answers.
	Pods map[string]podUsage `json:"pods,omitempty"`
}

// podUsage is one pod's reading.
//
// A struct rather than the bare k8s.Usage so the wire shape is this package's
// to keep stable: what the metrics API reports is an implementation detail, and
// its PodName field would be the map key repeated inside every value.
type podUsage struct {
	CPU       string `json:"cpu,omitempty"`
	Memory    string `json:"memory,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

// podUsagesFrom converts the observer's readings into the wire shape.
//
// Nil in, nil out: an absent map and an empty one render the same here, and nil
// keeps the omitempty tag meaning what it says — "this cluster reports no
// metrics" rather than "this app has no pods".
func podUsagesFrom(usage map[string]k8s.Usage) map[string]podUsage {
	if len(usage) == 0 {
		return nil
	}
	out := make(map[string]podUsage, len(usage))
	for name, u := range usage {
		out[name] = podUsage{CPU: u.CPU, Memory: u.Memory, Timestamp: u.Timestamp}
	}
	return out
}

// handleAppUsage reports what an app is using and what it may use.
//
// It is deliberately not folded into the app response or the pod list. Usage is
// the only thing here the cluster samples, so it is the only one that changes
// between two reads seconds apart — a page that wants live numbers asks for them
// on their own, and a listing that carried them would fetch a sample per row.
func (s *Server) handleAppUsage(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}
	if s.observer == nil || !s.observer.Ready() {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment cannot observe: no cluster is configured"))
		return
	}

	usage, err := s.observer.AppUsage(r.Context(), app.Namespace, app.ID)
	if err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "read what app %q is using", app.ID).Wrap(err))
		return
	}

	respond(w, http.StatusOK, usageResponse{
		Usage: usage,
		Pods:  podUsagesFrom(usage.PerPod),
	})
}

// handleSelfUsage reports what AppLab's own pods are using.
//
// The platform-side counterpart of handleAppUsage, and it exists for the same
// reason the platform log route does: diagnosing AppLab itself used to mean
// shelling into the cluster. A control plane that is being starved of CPU is a
// thing its own console should be able to show.
//
// Admin only, like the other platform routes. An app key reaches one app, and
// the deployment serving the API is not it.
//
// The response carries no requested or limited, and that is deliberate rather
// than unset: AppLab's own Deployment belongs to the chart, so its requests and
// limits are the release's business. Reporting the app-shaped fields as empty
// would be claiming the control plane is unbounded.
func (s *Server) handleSelfUsage(w http.ResponseWriter, r *http.Request) {
	if s.observer == nil || !s.observer.Ready() {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment cannot observe: no cluster is configured"))
		return
	}

	usage, err := s.observer.PlatformUsage(r.Context(), s.cfg.Namespace)
	if err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "read what applab is using").Wrap(err))
		return
	}

	respond(w, http.StatusOK, usageResponse{
		Usage: usage,
		Pods:  podUsagesFrom(usage.PerPod),
	})
}

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

	// A label selection is applied here rather than passed to Kubernetes, so that
	// what a caller may select is the set of labels the app's own pods actually
	// carry. The alternative — forwarding the string to the API server — would
	// let a caller select on any label in the namespace, and would answer "no
	// pods" for a typo in a way indistinguishable from a genuine absence.
	//
	// The console offers the same selection, so this is the API half of one
	// feature rather than a second way to do it.
	selector := strings.TrimSpace(r.URL.Query().Get("label"))
	if selector != "" {
		parsed, err := labels.Parse(selector)
		if err != nil {
			fail(w, r, Errorf(http.StatusBadRequest, "label selector %q could not be parsed", selector).Wrap(err))
			return
		}
		filtered := make([]observe.Pod, 0, len(pods))
		for _, pod := range pods {
			if parsed.Matches(labels.Set(pod.Labels)) {
				filtered = append(filtered, pod)
			}
		}
		pods = filtered
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

	// What the cluster says comes first, because it is the answer: an app with
	// no Deployment is diagnosed without reading a single pod.
	live := s.liveStatusesFor(r.Context(), app)
	diagnosis := map[string]any{
		"app_id": app.ID,
		"status": string(appStatus(map[string]deploy.Status{app.ID: live}, app.ID, s.buildInFlight(r.Context(), app))),
	}
	if live.Message != "" {
		diagnosis["status_reason"] = live.Message
	}
	if !live.Found {
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
