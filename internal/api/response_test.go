package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shaowenchen/applab/internal/store"
)

// TestDeliberateMessagesSurviveOnEveryStatus pins the rule that a handler's own
// message reaches the caller.
//
// This was a real bug: every 5xx message was replaced with "internal error" on
// the reasoning that a server error's text might leak. But Message is only ever
// text a handler wrote on purpose, so the replacement turned actionable answers
// — "this deployment cannot build", "retrying could help" — into useless ones.
// The test covers 501 and 503 specifically because those are the statuses whose
// message is the entire point of the response.
func TestDeliberateMessagesSurviveOnEveryStatus(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		message string
	}{
		{"bad request", http.StatusBadRequest, "port must be between 1 and 65535"},
		{"unauthorized", http.StatusUnauthorized, "present an API key"},
		{"not found", http.StatusNotFound, "app \"shop\" not found"},
		{"conflict", http.StatusConflict, "app \"shop\" already exists"},
		{"not implemented", http.StatusNotImplemented, "this deployment cannot build: no registry is configured"},
		{"unavailable", http.StatusServiceUnavailable, "the cluster is unreachable"},
		{"internal with a deliberate message", http.StatusInternalServerError, "the build job could not be started"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/apps", nil)

			fail(rec, req, Errorf(tc.status, "%s", tc.message))

			if rec.Code != tc.status {
				t.Errorf("status = %d, want %d", rec.Code, tc.status)
			}

			var body struct {
				Error     string `json:"error"`
				Retryable bool   `json:"retryable"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not JSON: %v (%s)", err, rec.Body.String())
			}
			if body.Error != tc.message {
				t.Errorf("error = %q, want %q", body.Error, tc.message)
			}
		})
	}
}

// TestInternalCauseIsNeverSent is the other half: the underlying error, which
// routinely names tables, paths or another caller's data, must reach the log and
// never the response.
func TestInternalCauseIsNeverSent(t *testing.T) {
	secret := "pq: relation \"apps\" does not exist at /var/lib/applab/applab.db"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/apps", nil)

	fail(rec, req, Errorf(http.StatusInternalServerError, "list apps").Wrap(errors.New(secret)))

	body := rec.Body.String()
	if strings.Contains(body, secret) {
		t.Errorf("the underlying cause was sent to the caller:\n%s", body)
	}
	if strings.Contains(body, "applab.db") || strings.Contains(body, "relation") {
		t.Errorf("part of the underlying cause leaked:\n%s", body)
	}

	// The message the handler chose is still there, so the caller knows which
	// operation failed.
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if parsed.Error != "list apps" {
		t.Errorf("error = %q, want the handler's own message", parsed.Error)
	}
}

// TestUnexpectedErrorBecomesGeneric asserts that an error nobody classified is
// reported as an internal failure rather than passed through. It is the fallback
// that makes the blanket-hiding rule unnecessary: anything a handler did not
// deliberately describe is already generic.
func TestUnexpectedErrorBecomesGeneric(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/apps", nil)

	fail(rec, req, fmt.Errorf("some raw error with /etc/passwd in it"))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	var body struct {
		Error string `json:"error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)

	if body.Error != "internal error" {
		t.Errorf("error = %q, want the generic message", body.Error)
	}
	if strings.Contains(rec.Body.String(), "/etc/passwd") {
		t.Errorf("a raw error's text was sent to the caller:\n%s", rec.Body.String())
	}
}

// TestRetryableFlagIsReported asserts the flag survives to the response, since
// it is the one thing a client cannot infer from a status code.
func TestRetryableFlagIsReported(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	fail(rec, req, Errorf(http.StatusServiceUnavailable, "the cluster is unreachable").Retryable())

	var body struct {
		Error     string `json:"error"`
		Retryable bool   `json:"retryable"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if !body.Retryable {
		t.Error("retryable = false, want true for an error marked retryable")
	}
}

// TestStoreErrorMapping asserts the sentinels map onto the right statuses, so a
// missing app is a 404 rather than a 500 everywhere it is looked up.
func TestStoreErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"not found", fmt.Errorf("app %q: %w", "shop", errNotFoundSentinel()), http.StatusNotFound},
		{"already exists", fmt.Errorf("app %q: %w", "shop", errExistsSentinel()), http.StatusConflict},
		{"other", errors.New("disk on fire"), http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fromStoreError(tc.err, "app \"shop\"")
			if got.Status != tc.status {
				t.Errorf("status = %d, want %d", got.Status, tc.status)
			}
		})
	}
}

// errNotFoundSentinel and errExistsSentinel wrap the store's sentinels so the
// mapping test does not depend on the store package's error text.
func errNotFoundSentinel() error { return store.ErrNotFound }
func errExistsSentinel() error   { return store.ErrExists }
