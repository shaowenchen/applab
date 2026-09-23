// Package api serves applab's HTTP interface.
//
// The API is the product: the console and the CLI are both just clients of it,
// and the contract an agent reads is llms.txt. Every route is declared in one
// table in router.go so that the served API, the documented API and the
// authorization rules cannot drift apart.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/shaowenchen/applab/internal/store"
)

// Response shapes.
//
// Success is the payload alone under "data" — a caller that got a 2xx has
// nothing else to check, so a status field inside the body would be a second
// source of truth to keep in sync.
//
// Failure carries a human-readable message and an explicit `retryable`. That
// flag is the one thing a machine cannot infer from a status code: 409 and 503
// are both "try again" for some operations and "stop" for others, and an agent
// that guesses wrong either gives up on a transient failure or hammers a
// permanent one. Callers should still branch on the HTTP status, and treat
// `retryable` as an additional signal for the cases where a retry is in fact
// worth attempting.
type successBody struct {
	Data any `json:"data"`
}

type errorBody struct {
	Error     string `json:"error"`
	Retryable bool   `json:"retryable"`
}

// apiError is a failure with a status and whether retrying could help.
type apiError struct {
	Status  int
	Message string

	// CanRetry is whether retrying could plausibly succeed. Named to leave
	// Retryable free as the builder method below, so a call site reads as
	// Errorf(...).Retryable() rather than assigning the field directly.
	CanRetry bool

	// cause is logged but never sent: an internal error's text can name tables,
	// paths or other callers' data, and the client can do nothing with it.
	cause error
}

func (e *apiError) Error() string {
	if e.cause != nil {
		return e.Message + ": " + e.cause.Error()
	}
	return e.Message
}

// Errorf builds an apiError with a formatted message.
func Errorf(status int, format string, args ...any) *apiError {
	return &apiError{Status: status, Message: fmt.Sprintf(format, args...)}
}

// Wrap attaches an underlying cause, which is logged but not returned.
func (e *apiError) Wrap(err error) *apiError {
	e.cause = err
	return e
}

// Retryable marks the error as worth retrying.
func (e *apiError) Retryable() *apiError {
	e.CanRetry = true
	return e
}

// NotFound is the standard missing-resource error.
func NotFound(what string, args ...any) *apiError {
	return Errorf(http.StatusNotFound, what+" not found", args...)
}

// BadRequest is the standard malformed-request error.
func BadRequest(format string, args ...any) *apiError {
	return Errorf(http.StatusBadRequest, format, args...)
}

// Conflict is the standard "already exists" or "in the wrong state" error.
func Conflict(format string, args ...any) *apiError {
	return Errorf(http.StatusConflict, format, args...)
}

// fromStoreError maps a store sentinel onto the right HTTP error, so callers
// do not each re-derive the same mapping and get it subtly different.
func fromStoreError(err error, what string) *apiError {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return NotFound("%s", what).
			Wrap(err)
	case errors.Is(err, store.ErrExists):
		return Conflict("%s already exists", what).Wrap(err)
	default:
		return Errorf(http.StatusInternalServerError, "internal error").
			Wrap(fmt.Errorf("%s: %w", what, err))
	}
}

// writeJSON sends a payload with the given status.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// The status line and headers are already out, so the response cannot be
		// replaced with an error — the best available signal is a truncated body,
		// which the client's own decode will catch.
		return
	}
}

// respond sends a success envelope.
func respond(w http.ResponseWriter, status int, data any) {
	writeJSON(w, status, successBody{Data: data})
}

// fail sends an error envelope, logging the underlying cause.
//
// The invariant that makes this safe: Message is always text a handler wrote
// deliberately, and it is the only thing sent to the caller. The underlying
// cause — which routinely names database tables, file paths or another caller's
// data — lives in cause, reaches the log through Error(), and is never encoded
// into a response.
//
// So a 5xx message is shown rather than replaced. Blanket-hiding it looked
// safer but was a bug: 501 and 503 carry deliberate messages the caller needs
// ("this deployment cannot build", "retrying could help"), and replacing them
// with "internal error" turns an actionable answer into a useless one. An
// unexpected failure is already reported as "internal error" by the fallback
// below, which is where that wording belongs.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		apiErr = Errorf(http.StatusInternalServerError, "internal error").Wrap(err)
	}

	if apiErr.Status >= 500 {
		slog.ErrorContext(r.Context(), "request failed",
			"method", r.Method,
			"path", r.URL.Path,
			"status", apiErr.Status,
			"error", apiErr.Error())
	} else {
		slog.DebugContext(r.Context(), "request rejected",
			"method", r.Method,
			"path", r.URL.Path,
			"status", apiErr.Status,
			"error", apiErr.Error())
	}

	writeJSON(w, apiErr.Status, errorBody{
		Error:     apiErr.Message,
		Retryable: apiErr.CanRetry,
	})
}

// writeText sends a plain-text body, used for llms.txt.
func writeText(w http.ResponseWriter, status int, contentType, body string) {
	w.Header().Set("Content-Type", contentType+"; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
