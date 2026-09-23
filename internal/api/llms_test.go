package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/auth"
	"github.com/shaowenchen/applab/internal/config"
	"github.com/shaowenchen/applab/internal/store"
)

// newTestServer builds a Server backed by a temporary database.
func newTestServer(t *testing.T) (*api.Server, *store.Store) {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.Default()
	cfg.Keys = []string{"test-key"}
	cfg.BaseDomain = "apps.example.com"

	srv := api.New(cfg, st, auth.New(cfg.Keys))
	return srv, st
}

// TestLlmsTxtMatchesCommittedFile is the guard that keeps the agent-facing
// contract honest.
//
// llms.txt is the API as far as an agent is concerned: it acts on what the file
// says. The served copy is generated from the route table, so it is always
// right — but the committed copy in api/llms.txt is what a person reads in the
// repository and what gets reviewed in a diff, and it goes stale silently the
// moment a route changes. This test is what makes that staleness loud.
//
// When it fails, run `make llms` and commit the result.
func TestLlmsTxtMatchesCommittedFile(t *testing.T) {
	srv, _ := newTestServer(t)

	want, err := api.RenderLlmsTxt(srv)
	if err != nil {
		t.Fatalf("render llms.txt: %v", err)
	}

	path := filepath.Join("..", "..", "api", "llms.txt")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (run `make llms`)", path, err)
	}

	if string(got) != want {
		t.Errorf(`api/llms.txt is out of date with the route table.

The served document is generated from routes() in internal/api/router.go, so
this file has to be regenerated whenever a route is added, removed or reworded:

    make llms

--- diff (first difference) ---
%s`, firstDifference(string(got), want))
	}
}

// TestRouteReferenceIsNonEmpty guards the consistency check above against
// passing vacuously: if the generator returned nothing, the comparison would
// still catch a stale file, but a document with no endpoints in it would look
// plausible and tell an agent nothing.
func TestRouteReferenceIsNonEmpty(t *testing.T) {
	srv, _ := newTestServer(t)

	if n := srv.DocumentedRouteCount(); n < 5 {
		t.Errorf("only %d routes are documented; the generated reference is too thin to be useful", n)
	}

	ref := srv.RouteReference()
	for _, want := range []string{
		"GET /api/v1/apps",
		"POST /api/v1/apps",
		"GET /api/v1/apps/{app}",
	} {
		if !strings.Contains(ref, want) {
			t.Errorf("route reference does not mention %q", want)
		}
	}
}

// TestEveryDataRouteRequiresAuth asserts that a route which reads or changes
// data is protected.
//
// The failure this prevents is the expensive one: a new endpoint added without
// Auth set would be reachable by anyone who knows the URL, and nothing else in
// the system would notice.
func TestEveryDataRouteRequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t)

	// The only routes that may be open are the ones a caller needs before it
	// can authenticate, plus liveness.
	openAllowed := map[string]bool{
		"GET /health":         true,
		"GET /api/v1/config":  true,
		"GET /llms.txt":       true,
		"GET /api/v1/version": true,
	}

	for _, pattern := range srv.SortedPatterns() {
		if openAllowed[pattern] {
			continue
		}
		if !srv.PatternRequiresAuth(pattern) {
			t.Errorf("route %q is not protected by a key; set Auth: true unless it is safe to serve openly", pattern)
		}
	}
}

// TestOpenRoutesAreOnlyTheExpectedOnes is the converse: an endpoint that only
// ever needed to be open must not quietly become authenticated, because that
// breaks a probe or a bootstrap that has no key to present.
func TestOpenRoutesAreOnlyTheExpectedOnes(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, pattern := range srv.SortedPatterns() {
		if srv.PatternRequiresAuth(pattern) {
			continue
		}
		switch pattern {
		case "GET /health", "GET /api/v1/config", "GET /llms.txt", "GET /api/v1/version":
		default:
			t.Errorf("route %q is open but is not on the list of routes expected to be open", pattern)
		}
	}
}

// TestAuthRejectsMissingAndWrongKeys exercises the middleware end to end through
// the real handler, since that is where the wiring — and so the mistakes —
// actually live.
func TestAuthRejectsMissingAndWrongKeys(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	cases := []struct {
		name       string
		header     string
		wantStatus int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong key", "Bearer nope", http.StatusUnauthorized},
		{"bare wrong key", "nope", http.StatusUnauthorized},
		{"empty bearer", "Bearer ", http.StatusUnauthorized},
		{"correct key with scheme", "Bearer test-key", http.StatusOK},
		{"correct key bare", "test-key", http.StatusOK},
		{"correct key, lowercase scheme", "bearer test-key", http.StatusOK},
		{"correct key with padding", "Bearer   test-key  ", http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/apps", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("Authorization %q: got status %d, want %d (body: %s)",
					tc.header, rec.Code, tc.wantStatus, strings.TrimSpace(rec.Body.String()))
			}
		})
	}
}

// TestKeyNeverAcceptedInQuery ensures a credential cannot be smuggled through a
// URL, where it would be recorded in access logs, shell history and Referer
// headers.
func TestKeyNeverAcceptedInQuery(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/apps?key=test-key", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a key in the query string was accepted (status %d); it must only be read from the Authorization header", rec.Code)
	}
}

// firstDifference renders the first differing line of two documents, which is
// far easier to act on than a wall of escaped text.
func firstDifference(got, want string) string {
	gotLines := strings.Split(got, "\n")
	wantLines := strings.Split(want, "\n")

	for i := 0; i < len(gotLines) && i < len(wantLines); i++ {
		if gotLines[i] != wantLines[i] {
			return "line " + itoa(i+1) + ":\n  committed: " + gotLines[i] + "\n  generated: " + wantLines[i]
		}
	}
	switch {
	case len(gotLines) < len(wantLines):
		return "committed file ends early at line " + itoa(len(gotLines)+1) + ":\n  generated: " + wantLines[len(gotLines)]
	case len(wantLines) < len(gotLines):
		return "committed file has an extra line " + itoa(len(wantLines)+1) + ":\n  committed: " + gotLines[len(wantLines)]
	}
	return "(documents are equal)"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
