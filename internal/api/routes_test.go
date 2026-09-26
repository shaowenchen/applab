package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/auth"
	"github.com/shaowenchen/applab/internal/build"
	"github.com/shaowenchen/applab/internal/config"
	"github.com/shaowenchen/applab/internal/store"
)

// newTestServer builds the minimal Server: a bucket, a key, and nothing attached.
//
// Deliberately nothing optional — no cluster, no build half, no console. It is
// what the route and auth tests want, because what they assert is the shape of
// the API rather than what any half does; a test that needs a cluster uses
// newDeployServer instead.
func newTestServer(t *testing.T) (*api.Server, *store.Store) {
	t.Helper()

	st, err := store.OpenLocal(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	cfg := config.Default()
	cfg.Keys = []string{"test-key"}
	cfg.BaseDomain = "apps.example.com"

	srv := api.New(cfg, st, auth.New(cfg.Keys))
	return srv, st
}

// newRealBuildEngine returns the engine AppLab itself uses, over a clientset a
// test can also write to.
//
// The real one rather than a stub, because build history is read from the Jobs —
// the labels, the annotations and the ordering are the engine's own, and a stub
// would be a test agreeing with itself about a scheme it invented.
func newRealBuildEngine(t *testing.T, client *fake.Clientset, namespace string) *build.Engine {
	t.Helper()

	return build.New(client, build.Config{
		Registry:    "registry.example.com/apps",
		KanikoImage: "gcr.io/kaniko-project/executor:v1.23.2",
		AppLabURL:   "http://applab." + namespace + ".svc.cluster.local",
	})
}

// TestRouteReferenceCoversEveryDocumentedRoute is the guard that keeps the
// endpoint list honest.
//
// The list GET /api/v1/describe returns is what an agent acts on, and it is
// generated from the route table — so it is always right about what the server
// serves. What this adds is coverage: a route with a Doc is claiming to be
// documented, and this asserts the list actually contains it, so a route cannot
// end up unreachable-by-description because a render step dropped it.
//
// It replaces the check that compared a committed api/llms.txt against the
// generated document. That document is gone — the list is served rather than
// served-and-also-committed — so there is no copy left to go stale, and the
// thing worth guarding is now that the served list is complete.
func TestRouteReferenceCoversEveryDocumentedRoute(t *testing.T) {
	srv, _ := newTestServer(t)

	ref := srv.RouteReference()
	if len(ref) < 5 {
		t.Fatalf("only %d routes are described; the endpoint list is too thin to be useful", len(ref))
	}

	described := make(map[string]bool, len(ref))
	for _, e := range ref {
		described[e.Method+" "+e.Path] = true
	}

	for _, pattern := range srv.SortedPatterns() {
		method, path, _ := strings.Cut(pattern, " ")
		if !described[method+" "+path] {
			t.Errorf("route %q is served but does not appear in the endpoint list", pattern)
		}
	}
}

// TestRouteReferenceNeverClaimsAnUnknownCredential guards the one field of the
// endpoint list that is not copied from the route table.
//
// The tier is derived from the route's auth flags, and a route that set no flag
// at all would be reported as needing nothing — which is the answer that gets a
// caller a 401 it cannot explain. An empty Doc is the documented way to keep a
// route out of the list, so a route that is in the list has to name a real tier.
func TestRouteReferenceNeverClaimsAnUnknownCredential(t *testing.T) {
	srv, _ := newTestServer(t)

	known := map[string]bool{"none": true, "admin": true, "app": true, "token": true}
	for _, e := range srv.RouteReference() {
		if !known[e.Key] {
			t.Errorf("route %s %s reports credential %q, which is not one of the four tiers",
				e.Method, e.Path, e.Key)
		}
		if strings.TrimSpace(e.Doc) == "" {
			t.Errorf("route %s %s is in the endpoint list with no description", e.Method, e.Path)
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
	// can authenticate, plus liveness and the scrape endpoint.
	openAllowed := map[string]bool{
		"GET /health":          true,
		"GET /api/v1/config":   true,
		"GET /api/v1/describe": true,
		"GET /api/v1/version":  true,
		// Open so a Prometheus scraper can reach it; the deployment restricts it
		// at the network edge instead. See the route's own comment.
		"GET /metrics": true,
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
		case "GET /health", "GET /api/v1/config", "GET /api/v1/describe", "GET /api/v1/version", "GET /metrics":
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

// itoa is a local integer formatter, kept so these tests do not pull strconv in
// for one call. Shared by every test in this package, which is why it lives in
// this file rather than beside its first user.
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
