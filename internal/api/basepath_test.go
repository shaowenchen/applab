package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/auth"
	"github.com/shaowenchen/applab/internal/config"
	"github.com/shaowenchen/applab/internal/store"
)

// newBasePathServer builds a Server served under a path prefix.
//
// Built the way every other test server here is — a real store against a
// temporary database — rather than by reaching into the constructed Server,
// which has no exported way to change its config and should not grow one for a
// test's benefit.
func newBasePathServer(t *testing.T, basePath string) *api.Server {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.Default()
	cfg.Keys = []string{"test-key"}
	cfg.BaseDomain = "apps.example.com"
	cfg.BasePath = basePath

	return api.New(cfg, st, auth.New(cfg.Keys)).WithConsole(testConsole())
}

// testConsole stands in for the embedded console, which the real binary attaches
// and this test server otherwise has not. Without one the root serves the
// JSON 404 the API gives an Unown route, so a test of "the prefix root reaches
// the console" would be asserting on a handler that is not there.
func testConsole() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("console"))
	})
}

// TestBasePathServesEveryRouteUnderThePrefix is the whole point of the setting.
//
// An Ingress routes on a path but cannot strip one, so a deployment served at
// /applab receives /applab/api/v1/... — and if the server did not expect the
// prefix, every one of those would miss every route and the console would answer
// 404 while the Ingress looked correct.
func TestBasePathServesEveryRouteUnderThePrefix(t *testing.T) {
	srv := newBasePathServer(t, "/applab")
	h := srv.Handler()

	for _, path := range []string{
		"/applab/health",
		"/applab/api/v1/config",
		"/applab/api/v1/version",
		"/applab/api/v1/describe",
	} {
		rec := doRequestNoKey(t, h, "GET", path)
		if rec.Code != 200 {
			t.Errorf("GET %s -> %d, want 200 (body: %s)", path, rec.Code, rec.Body.String())
		}
	}
}

// TestBasePathLeavesUnprefixedRequestsAlone asserts a request that does not
// carry the prefix is not silently served.
//
// Serving it would make the deployment answer on two paths, which is not what a
// prefix is for: the prefix exists because something else may own the root, and
// answering there anyway is how a deployment shadows its neighbour.
func TestBasePathLeavesUnprefixedRequestsAlone(t *testing.T) {
	h := newBasePathServer(t, "/applab").Handler()

	rec := doRequestNoKey(t, h, "GET", "/api/v1/config")
	if rec.Code == 200 {
		t.Errorf("GET /api/v1/config -> 200 on a deployment served at /applab; the root belongs to whatever else is there")
	}
}

// TestBasePathStillServesTheProbePaths is the regression test for a pod that
// never became ready.
//
// The prefix rule above was applied to every path, including the two the cluster
// itself uses to reach the pod: a kubelet probe asks for /health on the container
// port and a ServiceMonitor asks for /metrics, neither through the Ingress and
// neither knowing the deployment's published path. Refusing them made every probe
// fail against a server that was healthy and listening — the pod logged
// "listening" and stayed 0/1, with a 404 in the only place nobody looks, because
// logRequests quiets these paths.
//
// They must also not move under the prefix: a probe path that tracked
// ingress.path would make the chart's probe configuration depend on the Ingress
// configuration, and a deployment would break when one changed without the other.
func TestBasePathStillServesTheProbePaths(t *testing.T) {
	h := newBasePathServer(t, "/applab").Handler()

	for _, path := range []string{"/health", "/metrics"} {
		rec := doRequestNoKey(t, h, "GET", path)
		if rec.Code == 404 {
			t.Errorf("GET %s -> 404 on a deployment served at /applab; this is an address the cluster "+
				"reaches the pod on, not one the Ingress publishes, so the prefix does not apply to it", path)
		}
	}

	// And the prefix still refuses a path outside it, so exempting these two did
	// not open the whole root.
	rec := doRequestNoKey(t, h, "GET", "/api/v1/config")
	if rec.Code == 200 {
		t.Errorf("GET /api/v1/config -> 200; the two probe paths are the exemption, not the rule")
	}
}

// TestBasePathRootServesTheConsole asserts the bare prefix works.
//
// "/applab" without a trailing slash is what a person types and what a link to
// the deployment produces. Without the special case it reaches the mux as
// "/applab", matching no route.
func TestBasePathRootServesTheConsole(t *testing.T) {
	h := newBasePathServer(t, "/applab").Handler()

	for _, path := range []string{"/applab", "/applab/"} {
		rec := doRequestNoKey(t, h, "GET", path)
		if rec.Code != 200 {
			t.Errorf("GET %s -> %d, want the console (body: %s)", path, rec.Code, rec.Body.String())
		}
	}
}

// TestBasePathReachesTheAPIBaseURL asserts the address the server reports
// includes the prefix.
//
// api_base_url is what a client is told to build its links from. A client that
// has to know about a prefix it was never told would build every one of them
// wrong — and the failure would look like the API being broken rather than the
// address being incomplete.
func TestBasePathReachesTheAPIBaseURL(t *testing.T) {
	h := newBasePathServer(t, "/applab").Handler()

	rec := doRequestNoKey(t, h, "GET", "/applab/api/v1/config")
	if rec.Code != 200 {
		t.Fatalf("GET /applab/api/v1/config -> %d", rec.Code)
	}

	var body struct {
		Data struct {
			APIBaseURL string `json:"api_base_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.HasSuffix(body.Data.APIBaseURL, "/applab") {
		t.Errorf("api_base_url = %q, want it to end in /applab", body.Data.APIBaseURL)
	}
}

// TestNoBasePathIsTheRoot asserts the default is unchanged.
//
// Every existing deployment reaches applab at its own hostname, and a prefix
// that leaked into that case would break all of them at once.
func TestNoBasePathIsTheRoot(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	for _, path := range []string{"/health", "/api/v1/config", "/api/v1/describe"} {
		if rec := doRequestNoKey(t, h, "GET", path); rec.Code != 200 {
			t.Errorf("GET %s -> %d, want 200 (body: %s)", path, rec.Code, rec.Body.String())
		}
	}
}
