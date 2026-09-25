package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/shaowenchen/applab/internal/observe"
)

// The platform's own logs, as opposed to an app's.
//
// AppLab already serves an app's pods and logs, and had no equivalent for
// itself: diagnosing the control plane meant `kubectl logs`, which is fine for
// whoever already has cluster access and unavailable to everyone else —
// including whoever is looking at the console.
//
// That gap is not theoretical. Every diagnosis of AppLab's own behaviour in this
// repository has gone: run it, read the container log, paste it back. Serving
// that log turns a round trip through a shell into a call the console can make.
//
// Admin only, unlike an app's logs. An app key reaches one app and nothing else,
// and the control plane's log is not that app's — it names other apps, their
// commits and their failures. The boundary is the same one that keeps an app key
// out of the overview.

// handleListSelfPods reports AppLab's own pods.
func (s *Server) handleListSelfPods(w http.ResponseWriter, r *http.Request) {
	if s.observer == nil || !s.observer.Ready() {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment cannot observe: no cluster is configured"))
		return
	}

	limit, apiErr := boundedIntQuery(r, "limit", 100, 1000)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	pods, err := s.listSelfPods(r.Context(), limit)
	if err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "list applab's pods").Wrap(err))
		return
	}

	respond(w, http.StatusOK, map[string]any{
		"namespace": s.cfg.Namespace,
		"pods":      pods,
		"count":     len(pods),
	})
}

// handleSelfLogs streams or returns one of AppLab's own containers' logs.
//
// The same shape as an app's log route — text/plain, following by default — so
// a client that can read one can read the other, and so a reader that discovers
// this endpoint has nothing new to learn.
func (s *Server) handleSelfLogs(w http.ResponseWriter, r *http.Request) {
	if s.observer == nil || !s.observer.Ready() {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment cannot observe: no cluster is configured"))
		return
	}

	query := r.URL.Query()

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

	if query.Get("follow") == "false" {
		logs, err := s.selfLogs(r.Context(), opts)
		if err != nil {
			fail(w, r, Errorf(http.StatusInternalServerError, "read applab's log").Wrap(err))
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
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	if err := s.streamSelfLogs(r.Context(), opts, w, flusher.Flush); err != nil {
		// Headers are already out, so a marker in the body is the only signal
		// left — the same one the app log route uses.
		_, _ = fmt.Fprintf(w, "\n[AppLab] log stream ended: %v\n", err)
		flusher.Flush()
	}
}
