package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/auth"
	"github.com/shaowenchen/applab/internal/config"
	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/objectstore"
	"github.com/shaowenchen/applab/internal/store"
)

// doRequest issues a request through the real handler with a valid key, and
// returns the recorder. Going through the handler rather than calling methods
// directly is deliberate: routing, authentication and response encoding are
// exactly where the bugs are.
func doRequest(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer test-key")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// doRequestNoKey issues a request with no Authorization header, for asserting
// that a route is protected. It is separate from doRequest rather than a flag on
// it so that "this request deliberately carries no credential" is visible at the
// call site.
func doRequestNoKey(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, bytes.NewReader(nil))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeData unwraps the {"data": ...} envelope.
func decodeData(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()

	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope from %s: %v", rec.Body.String(), err)
	}
	if into == nil {
		return
	}
	if err := json.Unmarshal(envelope.Data, into); err != nil {
		t.Fatalf("decode data from %s: %v", string(envelope.Data), err)
	}
}

// TestCreateAppDefaults asserts the defaults a caller gets when it supplies only
// an id, since that is the shortest path an agent will take.
func TestCreateAppDefaults(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("got status %d, want %d (body: %s)", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var app map[string]any
	decodeData(t, rec, &app)

	if app["id"] != "shop" {
		t.Errorf("id = %v, want shop", app["id"])
	}
	if app["name"] != "shop" {
		t.Errorf("name = %v; it should default to the id so a caller need not supply one", app["name"])
	}
	if app["dockerfile"] != "Dockerfile" {
		t.Errorf("dockerfile = %v, want Dockerfile", app["dockerfile"])
	}
	if app["status"] != string(model.AppStatusCreated) {
		t.Errorf("status = %v, want %v", app["status"], model.AppStatusCreated)
	}
	if app["hostname"] != "shop.apps.example.com" {
		t.Errorf("hostname = %v, want shop.apps.example.com from the configured base domain", app["hostname"])
	}
	// Nothing has been deployed, so there is no URL yet — advertising one that
	// 404s at the ingress would read as a broken deployment.
	if _, present := app["url"]; present {
		t.Errorf("url was reported for an app that has never been deployed: %v", app["url"])
	}
}

// TestCreateAppRejectsBadIDs covers the ids that must not be accepted. Each one
// would break a different downstream system: a namespace, a repository
// directory, or this service's own routes.
func TestCreateAppRejectsBadIDs(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"uppercase", "MyApp"},
		{"underscore", "my_app"},
		{"leading dash", "-myapp"},
		{"trailing dash", "myapp-"},
		{"dot", "my.app"},
		{"slash", "my/app"},
		{"space", "my app"},
		{"too long", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"reserved api", "api"},
		{"reserved health", "health"},
		{"path traversal", "../etc"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServer(t)
			rec := doRequest(t, srv.Handler(), http.MethodPost, "/api/v1/apps", map[string]any{"id": tc.id})

			if rec.Code != http.StatusBadRequest {
				t.Errorf("id %q: got status %d, want %d (body: %s)", tc.id, rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}
}

// TestCreateAppRejectsUnknownFields asserts a misspelled field is an error
// rather than a silent no-op: a caller that asked for replicas and got one
// would otherwise have no way to notice.
func TestCreateAppRejectsUnknownFields(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := doRequest(t, srv.Handler(), http.MethodPost, "/api/v1/apps", map[string]any{
		"id":       "shop",
		"replcias": 3, // typo
	})

	if rec.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d for an unknown field", rec.Code, http.StatusBadRequest)
	}
}

// TestCreateAppIsNotAnUpsert asserts that creating an existing app is refused.
// A create that silently replaced an app would destroy its recorded history.
func TestCreateAppIsNotAnUpsert(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"}); rec.Code != http.StatusCreated {
		t.Fatalf("first create: status %d (%s)", rec.Code, rec.Body.String())
	}
	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop", "name": "different"})
	if rec.Code != http.StatusConflict {
		t.Errorf("second create: got status %d, want %d", rec.Code, http.StatusConflict)
	}
}

// TestAppSettingsValidation covers the settings whose failure mode is hard to
// diagnose from the cluster side.
func TestAppSettingsValidation(t *testing.T) {
	cases := []struct {
		name    string
		body    map[string]any
		wantErr bool
	}{
		{"valid", map[string]any{"id": "shop", "port": 3000, "replicas": 2}, false},
		{"port zero", map[string]any{"id": "shop", "port": 0}, true},
		{"port too high", map[string]any{"id": "shop", "port": 70000}, true},
		{"port negative", map[string]any{"id": "shop", "port": -1}, true},
		{"replicas zero", map[string]any{"id": "shop", "replicas": 0}, true},
		{"replicas negative", map[string]any{"id": "shop", "replicas": -1}, true},
		{"replicas excessive", map[string]any{"id": "shop", "replicas": 100}, true},
		{"dockerfile absolute", map[string]any{"id": "shop", "dockerfile": "/etc/passwd"}, true},
		{"dockerfile traversal", map[string]any{"id": "shop", "dockerfile": "../../etc/passwd"}, true},
		{"dockerfile in subdirectory", map[string]any{"id": "shop", "dockerfile": "build/Dockerfile"}, false},
		{"dockerfile with dots", map[string]any{"id": "shop", "dockerfile": "Dockerfile.prod"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServer(t)
			rec := doRequest(t, srv.Handler(), http.MethodPost, "/api/v1/apps", tc.body)

			if tc.wantErr && rec.Code != http.StatusBadRequest {
				t.Errorf("got status %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if !tc.wantErr && rec.Code != http.StatusCreated {
				t.Errorf("got status %d, want %d (body: %s)", rec.Code, http.StatusCreated, rec.Body.String())
			}
		})
	}
}

// TestListAppsExcludesDeletedByDefault asserts the default listing is what a
// caller expects while keeping the opt-in for the full history.
func TestListAppsExcludesDeletedByDefault(t *testing.T) {
	srv, st := newTestServer(t)
	h := srv.Handler()

	for _, id := range []string{"one", "two"} {
		if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": id}); rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d (%s)", id, rec.Code, rec.Body.String())
		}
	}

	// Mark one deleted directly, standing in for the teardown path.
	app, err := st.GetApp(t.Context(), "two")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	app.Status = model.AppStatusDeleted
	if err := st.UpdateApp(t.Context(), app); err != nil {
		t.Fatalf("update app: %v", err)
	}

	var list []map[string]any
	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps", nil)
	decodeData(t, rec, &list)
	if len(list) != 1 || list[0]["id"] != "one" {
		t.Errorf("default listing = %v, want only the live app", list)
	}

	rec = doRequest(t, h, http.MethodGet, "/api/v1/apps?include_deleted=true", nil)
	decodeData(t, rec, &list)
	if len(list) != 2 {
		t.Errorf("include_deleted listing returned %d apps, want 2", len(list))
	}
}

// TestGetAppNotFound asserts a missing app is a 404, not a 500 or an empty
// success — an agent branches on this.
func TestGetAppNotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := doRequest(t, srv.Handler(), http.MethodGet, "/api/v1/apps/nonexistent", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusNotFound)
	}

	// The error body must be parseable JSON with a message, since that is what
	// the documented error shape promises.
	var body struct {
		Error     string `json:"error"`
		Retryable bool   `json:"retryable"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if body.Error == "" {
		t.Error("error body has no message")
	}
}

// TestPatchIsPartial asserts a PATCH that mentions one field leaves the others
// alone — the whole point of distinguishing "absent" from "zero".
func TestPatchIsPartial(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	body := map[string]any{"id": "shop", "port": 3000, "replicas": 4, "name": "Shop"}
	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", body); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d (%s)", rec.Code, rec.Body.String())
	}

	// Change only the name.
	rec := doRequest(t, h, http.MethodPatch, "/api/v1/apps/shop", map[string]any{"name": "Renamed"})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d (%s)", rec.Code, rec.Body.String())
	}

	var app map[string]any
	decodeData(t, rec, &app)

	if app["name"] != "Renamed" {
		t.Errorf("name = %v, want Renamed", app["name"])
	}
	if app["port"] != float64(3000) {
		t.Errorf("port = %v; a PATCH that did not mention it must not change it", app["port"])
	}
	if app["replicas"] != float64(4) {
		t.Errorf("replicas = %v; a PATCH that did not mention them must not change them", app["replicas"])
	}
}

// TestDeleteApp asserts deletion removes the app and that its id becomes
// available again, which is what an operator re-creating an app expects.
func TestDeleteApp(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"}); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d (%s)", rec.Code, rec.Body.String())
	}

	rec := doRequest(t, h, http.MethodDelete, "/api/v1/apps/shop", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d (%s)", rec.Code, rec.Body.String())
	}

	if rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop", nil); rec.Code != http.StatusNotFound {
		t.Errorf("app is still readable after deletion: status %d", rec.Code)
	}

	// The id must be reusable: a deleted app's row is gone, so nothing should
	// block creating it again.
	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"}); rec.Code != http.StatusCreated {
		t.Errorf("could not recreate a deleted app's id: %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestDeleteMissingApp asserts a missing app is reported as such rather than
// silently succeeding, so a scripted teardown notices a typo.
func TestDeleteMissingApp(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := doRequest(t, srv.Handler(), http.MethodDelete, "/api/v1/apps/nonexistent", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// TestConfigAndHealthNeedNoKey asserts the endpoints a probe and a bootstrap
// client rely on stay reachable without a credential.
func TestConfigAndHealthNeedNoKey(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	for _, path := range []string{"/health", "/api/v1/config", "/api/v1/version", "/api/v1/describe"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("%s returned %d without a key; it must be reachable so a caller can discover the API", path, rec.Code)
		}
	}
}

// TestConfigReportsDeploymentShape asserts the config endpoint tells a client
// what it needs to build correct requests without guessing.
func TestConfigReportsDeploymentShape(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := doRequest(t, srv.Handler(), http.MethodGet, "/api/v1/config", nil)
	var cfg map[string]any
	decodeData(t, rec, &cfg)

	if cfg["base_domain"] != "apps.example.com" {
		t.Errorf("base_domain = %v, want the configured value", cfg["base_domain"])
	}
	// The namespace is reported because it is no longer derivable: every app
	// lives in the one AppLab runs in, so "where are my app's objects" has one
	// answer rather than one per app.
	if cfg["namespace"] == nil || cfg["namespace"] == "" {
		t.Error("namespace is not reported; a client cannot say where an app's objects live")
	}
	if cfg["chunk_size"] == nil || cfg["max_simple_upload"] == nil {
		t.Error("upload limits are not reported; a client would have to discover them by being rejected")
	}

	caps, ok := cfg["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities is missing or not an object: %v", cfg["capabilities"])
	}
	// This test server has no build engine or cluster, and the response must
	// say so rather than implying the pipeline works.
	if caps["build"] != false {
		t.Errorf("build capability = %v, want false for a server with no build engine", caps["build"])
	}
}

// TestUnknownRouteIsJSON asserts a 404 from an unmatched path is in the same
// shape as every other API error, so a client parses one format and not two.
func TestUnknownRouteIsJSON(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := doRequest(t, srv.Handler(), http.MethodGet, "/api/v1/does-not-exist", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusNotFound)
	}

	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Errorf("404 body is not JSON: %v (%s)", err, rec.Body.String())
	}
}

// TestAppResponseReportsBothHalvesOfAPathPrefixAddress asserts the API reports
// the path alongside the host.
//
// With a shared path prefix the host is the deployment's rather than the app's:
// every app reports the same one, and the path is what says which app is meant.
// A client shown only the host would label every app identically, which is
// exactly what the console did until this field existed.
func TestAppResponseReportsBothHalvesOfAPathPrefixAddress(t *testing.T) {
	srv := newTestServerWithPrefix(t, "/apps")
	h := srv.Handler()

	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("got status %d, want %d (body: %s)", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var app map[string]any
	decodeData(t, rec, &app)

	if app["hostname"] != "apps.example.com" {
		t.Errorf("hostname = %v, want the shared host", app["hostname"])
	}
	if app["path"] != "/apps/shop" {
		t.Errorf("path = %v, want /apps/shop; without it the host names the deployment, not the app", app["path"])
	}

	// And the two together are what a client shows, so they have to be the
	// address the app is actually routed on.
	if got := app["hostname"].(string) + app["path"].(string); got != "apps.example.com/apps/shop" {
		t.Errorf("host+path = %q, want apps.example.com/apps/shop", got)
	}
}

// TestAppResponseHasNoPathWithoutAPrefix asserts the field is absent rather than
// empty when there is no prefix, so a client can tell "at this host's root" from
// "the server forgot to say where".
func TestAppResponseHasNoPathWithoutAPrefix(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusCreated)
	}

	var app map[string]any
	decodeData(t, rec, &app)
	if v, present := app["path"]; present {
		t.Errorf("path = %v; with no prefix configured the app is at the host's root and the field should be absent", v)
	}
}

// newTestServerWithPrefix builds a Server that serves every app from one host
// under a shared path prefix.
func newTestServerWithPrefix(t *testing.T, prefix string) *api.Server {
	t.Helper()

	st, err := store.OpenLocal(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	cfg := config.Default()
	cfg.Keys = []string{"test-key"}
	cfg.BaseDomain = "apps.example.com"
	cfg.PathPrefix = prefix

	return api.New(cfg, st, auth.New(cfg.Keys))
}

// newObjects returns an object store in a temporary directory.
//
// Every test that builds a Server needs one, because everything AppLab persists
// — the apps, their history and their source — lives in object storage now. A
// directory is the same implementation a deployment without a bucket uses, so a
// test exercises the real paths rather than a stub.
func newObjects(t *testing.T) objectstore.Store {
	t.Helper()
	objs, err := objectstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return objs
}
