package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/serverreg"
	"github.com/shaowenchen/applab/internal/store"
)

// newServerWithRegistry builds a test server with a registration store over the
// same fake cluster.
func newServerWithRegistry(t *testing.T) (*api.Server, *fake.Clientset, *store.Store) {
	t.Helper()
	srv, client, st := newDeployServer(t)
	srv.WithServerRegistry(serverreg.New(client, "ops-system"))
	return srv, client, st
}

// fakeRemote stands in for another AppLab deployment.
//
// It answers the handful of routes the proxy calls, and it enforces one thing
// the real one does: /api/v1/overview is admin-only, so an app key is refused
// there. That is what a registration's key is checked against, and a fake that
// accepted anything would make the check untestable.
type fakeRemote struct {
	*httptest.Server

	adminKey string
	keyIsApp bool // when set, the key is refused at /overview as an app key would be

	mu       sync.Mutex
	requests []string
	auths    []string
}

func newFakeRemote(t *testing.T, adminKey string) *fakeRemote {
	t.Helper()
	remote := &fakeRemote{adminKey: adminKey}
	remote.Server = httptest.NewServer(http.HandlerFunc(remote.serve))
	t.Cleanup(remote.Close)
	return remote
}

func (f *fakeRemote) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.mu.Unlock()

	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	authed := token == f.adminKey && !f.keyIsApp

	write := func(status int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status >= 400 {
			_ = json.NewEncoder(w).Encode(map[string]any{"error": fmt.Sprintf("remote says %d", status)})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}

	switch {
	case r.URL.Path == "/api/v1/config":
		// Needs no key, which is what makes it the reachability probe.
		write(http.StatusOK, map[string]any{"api_version": "v1"})

	case r.URL.Path == "/api/v1/overview":
		if !authed {
			write(http.StatusUnauthorized, nil)
			return
		}
		write(http.StatusOK, map[string]any{})

	case r.URL.Path == "/api/v1/apps" && r.Method == http.MethodGet:
		if !authed {
			write(http.StatusUnauthorized, nil)
			return
		}
		write(http.StatusOK, []map[string]any{{"id": "shop", "name": "shop", "url": "https://shop.remote.example.com"}})

	case r.URL.Path == "/api/v1/apps" && r.Method == http.MethodPost:
		if !authed {
			write(http.StatusUnauthorized, nil)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		id, _ := body["id"].(string)
		// The real create hands out the new app's key; the proxy relays it whole.
		write(http.StatusCreated, map[string]any{"id": id, "name": id, "app_key": "app-key-" + id})

	case strings.HasPrefix(r.URL.Path, "/api/v1/apps/") && r.Method == http.MethodGet:
		if !authed {
			write(http.StatusUnauthorized, nil)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/apps/")
		write(http.StatusOK, map[string]any{"id": id, "name": id})

	case strings.HasPrefix(r.URL.Path, "/api/v1/apps/") && r.Method == http.MethodDelete:
		if !authed {
			write(http.StatusUnauthorized, nil)
			return
		}
		write(http.StatusOK, map[string]any{"deleted": true})

	default:
		write(http.StatusNotFound, nil)
	}
}

func (f *fakeRemote) saw(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if r == path {
			return true
		}
	}
	return false
}

func (f *fakeRemote) lastAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.auths) == 0 {
		return ""
	}
	return f.auths[len(f.auths)-1]
}

// register puts a remote into a server's registry through the API, which is the
// only path a caller has.
func register(t *testing.T, srv *api.Server, id, url, key string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, srv.Handler(), http.MethodPost, "/api/v1/servers", map[string]any{
		"id": id, "url": url, "key": key,
	})
}

// A registration is proved before it is stored: a wrong address or a key that is
// not an admin key is a 400 at the moment of registration, not a failure later
// with nothing to say which part was wrong.
func TestRegisteringAServerValidatesBeforeStoring(t *testing.T) {
	t.Run("a reachable remote with an admin key is stored", func(t *testing.T) {
		srv, client, _ := newServerWithRegistry(t)
		remote := newFakeRemote(t, "remote-admin")

		rec := register(t, srv, "lab-2", remote.URL, "remote-admin")
		if rec.Code != http.StatusCreated {
			t.Fatalf("got %d, want 201: %s", rec.Code, rec.Body.String())
		}
		if _, err := client.CoreV1().Secrets("ops-system").Get(context.Background(), "applab-server-lab-2", metav1.GetOptions{}); err != nil {
			t.Fatalf("no registration was stored: %v", err)
		}
	})

	t.Run("a key the remote treats as an app key is refused", func(t *testing.T) {
		srv, client, _ := newServerWithRegistry(t)
		remote := newFakeRemote(t, "remote-admin")
		remote.keyIsApp = true // /overview refuses it, as an app key would be

		rec := register(t, srv, "lab-2", remote.URL, "an-app-key")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body.String())
		}
		if _, err := client.CoreV1().Secrets("ops-system").Get(context.Background(), "applab-server-lab-2", metav1.GetOptions{}); err == nil {
			t.Fatal("a rejected registration was stored anyway")
		}
	})

	t.Run("an unreachable address is refused", func(t *testing.T) {
		srv, _, _ := newServerWithRegistry(t)
		// A port nothing is listening on: the connection is refused, which is
		// what an address typo looks like.
		rec := register(t, srv, "lab-2", "http://127.0.0.1:1", "k")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a malformed or unsafe address is refused before any dial", func(t *testing.T) {
		srv, _, _ := newServerWithRegistry(t)
		for _, address := range []string{
			"ftp://host",                      // not http(s)
			"applab.example.com",              // no scheme
			"https://user:pw@host",            // a credential in the URL
			"http://169.254.169.254/latest",   // link-local / cloud metadata
			"http://metadata.google.internal", // the same, by name
		} {
			rec := register(t, srv, "lab-2", address, "k")
			if rec.Code != http.StatusBadRequest {
				t.Errorf("address %q: got %d, want 400: %s", address, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("local cannot be registered", func(t *testing.T) {
		srv, _, _ := newServerWithRegistry(t)
		rec := register(t, srv, "local", "http://127.0.0.1:1", "k")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})
}

// The built-in local entry is listed first and stands for this deployment, and
// it cannot be removed.
func TestTheLocalEntryIsBuiltIn(t *testing.T) {
	srv, _, _ := newServerWithRegistry(t)
	h := srv.Handler()

	rec := doRequest(t, h, http.MethodGet, "/api/v1/servers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var servers []map[string]any
	decodeData(t, rec, &servers)
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want just the local entry: %+v", len(servers), servers)
	}
	if servers[0]["id"] != "local" || servers[0]["builtin"] != true {
		t.Fatalf("the local entry is not what was listed: %+v", servers[0])
	}

	rec = doRequest(t, h, http.MethodDelete, "/api/v1/servers/local", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("removing local: got %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// A remote that is registered is listed after the local entry, and its key is
// not in the listing.
func TestARegisteredServerIsListedAfterLocal(t *testing.T) {
	srv, _, _ := newServerWithRegistry(t)
	remote := newFakeRemote(t, "remote-admin")
	if rec := register(t, srv, "lab-2", remote.URL, "remote-admin"); rec.Code != http.StatusCreated {
		t.Fatalf("register: %s", rec.Body.String())
	}

	rec := doRequest(t, srv.Handler(), http.MethodGet, "/api/v1/servers", nil)
	var servers []map[string]any
	decodeData(t, rec, &servers)
	if len(servers) != 2 {
		t.Fatalf("got %d servers, want 2: %+v", len(servers), servers)
	}
	if servers[0]["id"] != "local" {
		t.Errorf("the local entry is not first: %+v", servers)
	}
	if servers[1]["id"] != "lab-2" {
		t.Errorf("the registered server is not second: %+v", servers)
	}
	if strings.Contains(rec.Body.String(), "remote-admin") {
		t.Error("the listing returned the stored key")
	}
}

// The proxy relays a remote's answers, and the remote sees the stored key as its
// Bearer token.
func TestProxyingReachesTheRemote(t *testing.T) {
	srv, _, _ := newServerWithRegistry(t)
	remote := newFakeRemote(t, "remote-admin")
	if rec := register(t, srv, "lab-2", remote.URL, "remote-admin"); rec.Code != http.StatusCreated {
		t.Fatalf("register: %s", rec.Body.String())
	}
	h := srv.Handler()

	t.Run("listing apps", func(t *testing.T) {
		rec := doRequest(t, h, http.MethodGet, "/api/v1/servers/lab-2/apps", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
		}
		var apps []map[string]any
		decodeData(t, rec, &apps)
		if len(apps) != 1 || apps[0]["id"] != "shop" {
			t.Fatalf("the remote's list was not relayed: %+v", apps)
		}
		if remote.lastAuth() != "Bearer remote-admin" {
			t.Fatalf("the remote saw auth %q", remote.lastAuth())
		}
	})

	t.Run("creating an app relays the new app's key", func(t *testing.T) {
		rec := doRequest(t, h, http.MethodPost, "/api/v1/servers/lab-2/apps", map[string]any{"id": "shop"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
		}
		var app map[string]any
		decodeData(t, rec, &app)
		if app["app_key"] != "app-key-shop" {
			t.Fatalf("the new app's key was not relayed: %+v", app)
		}
	})

	t.Run("getting and deleting one app", func(t *testing.T) {
		if rec := doRequest(t, h, http.MethodGet, "/api/v1/servers/lab-2/apps/shop", nil); rec.Code != http.StatusOK {
			t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
		}
		if rec := doRequest(t, h, http.MethodDelete, "/api/v1/servers/lab-2/apps/shop", nil); rec.Code != http.StatusOK {
			t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
		}
	})
}

// The remote's failure becomes this deployment's, with the one deliberate
// exception: a remote that refuses our stored key is a 502 about the
// registration, not a 401 about the caller's key.
func TestProxyMapsRemoteStatuses(t *testing.T) {
	cases := []struct {
		remoteStatus int
		wantStatus   int
		wantRetry    bool
	}{
		{http.StatusBadRequest, http.StatusBadRequest, false},
		{http.StatusForbidden, http.StatusForbidden, false},
		{http.StatusNotFound, http.StatusNotFound, false},
		{http.StatusConflict, http.StatusConflict, false},
		{http.StatusUnauthorized, http.StatusBadGateway, false},
		{http.StatusInternalServerError, http.StatusBadGateway, true},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.remoteStatus), func(t *testing.T) {
			srv, _, _ := newServerWithRegistry(t)
			// A remote that answers everything, then refuses the apps route with
			// the status under test.
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/overview" || r.URL.Path == "/api/v1/config" {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"data":{}}`))
					return
				}
				w.WriteHeader(tc.remoteStatus)
				_, _ = w.Write([]byte(`{"error":"the remote's own words"}`))
			}))
			defer remote.Close()

			if rec := register(t, srv, "lab-2", remote.URL, "k"); rec.Code != http.StatusCreated {
				t.Fatalf("register: %s", rec.Body.String())
			}

			rec := doRequest(t, srv.Handler(), http.MethodGet, "/api/v1/servers/lab-2/apps", nil)
			if rec.Code != tc.wantStatus {
				t.Fatalf("remote %d -> got %d, want %d: %s", tc.remoteStatus, rec.Code, tc.wantStatus, rec.Body.String())
			}
			var body struct {
				Error     string `json:"error"`
				Retryable bool   `json:"retryable"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if body.Retryable != tc.wantRetry {
				t.Errorf("retryable = %v, want %v", body.Retryable, tc.wantRetry)
			}
			// A 4xx passes the remote's wording through; a 5xx or a refused key
			// is described by us, so the caller is told the *server* failed.
			if tc.remoteStatus < 500 && tc.remoteStatus != http.StatusUnauthorized {
				if !strings.Contains(body.Error, "the remote's own words") {
					t.Errorf("the remote's message was not relayed: %q", body.Error)
				}
			}
			if !strings.Contains(body.Error, "lab-2") {
				t.Errorf("the message does not name the server: %q", body.Error)
			}
		})
	}
}

// With no cluster, registrations cannot be stored — but the local entry still
// works, because listing and managing this deployment's own apps needs no
// registration.
func TestNoClusterMeansNoRegistrationsButLocalStillWorks(t *testing.T) {
	srv, _ := newTestServer(t) // no registry attached
	h := srv.Handler()

	rec := doRequest(t, h, http.MethodGet, "/api/v1/servers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var servers []map[string]any
	decodeData(t, rec, &servers)
	if len(servers) != 1 || servers[0]["id"] != "local" {
		t.Fatalf("expected just the local entry: %+v", servers)
	}

	rec = register(t, srv, "lab-2", "http://127.0.0.1:1", "k")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("register with no cluster: got %d, want 501: %s", rec.Code, rec.Body.String())
	}

	rec = doRequest(t, h, http.MethodGet, "/api/v1/servers/local/apps", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("local apps with no cluster: %d %s", rec.Code, rec.Body.String())
	}
}

// The local entry answers exactly what the plain apps route does, because it is
// the same handler in process rather than a proxy to ourselves.
func TestLocalServerAnswersLikeThePlainAppsRoute(t *testing.T) {
	srv, _, _ := newServerWithRegistry(t)
	h := srv.Handler()

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"}); rec.Code != http.StatusCreated {
		t.Fatalf("create: %s", rec.Body.String())
	}

	plain := doRequest(t, h, http.MethodGet, "/api/v1/apps", nil)
	viaServer := doRequest(t, h, http.MethodGet, "/api/v1/servers/local/apps", nil)
	if plain.Code != http.StatusOK || viaServer.Code != http.StatusOK {
		t.Fatalf("statuses: plain %d, local %d", plain.Code, viaServer.Code)
	}
	if plain.Body.String() != viaServer.Body.String() {
		t.Fatalf("the local entry and the apps route disagree:\n plain: %s\n local: %s", plain.Body.String(), viaServer.Body.String())
	}

	// And a create through the local entry is a real local app.
	rec := doRequest(t, h, http.MethodPost, "/api/v1/servers/local/apps", map[string]any{"id": "second"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create via local: %d %s", rec.Code, rec.Body.String())
	}
	var app map[string]any
	decodeData(t, rec, &app)
	if app["id"] != "second" {
		t.Fatalf("wrong app created: %+v", app)
	}
}

// The stored key of a remote must never appear in any response from any servers
// route. This is asserted by driving every route rather than by inspection, so a
// route added later without a test of its own is still covered by the shape of
// the list.
func TestTheKeyNeverAppearsInAnyServerResponse(t *testing.T) {
	const secret = "super-secret-admin-key-xyz"

	srv, _, _ := newServerWithRegistry(t)
	remote := newFakeRemote(t, secret)
	if rec := register(t, srv, "lab-2", remote.URL, secret); rec.Code != http.StatusCreated {
		t.Fatalf("register: %s", rec.Body.String())
	}
	h := srv.Handler()

	// Every route that can answer with a server, or that a caller might read a
	// key back from.
	routes := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/servers", nil},
		{http.MethodGet, "/api/v1/servers/lab-2", nil},
		{http.MethodGet, "/api/v1/servers/local", nil},
		{http.MethodGet, "/api/v1/servers/lab-2/apps", nil},
		{http.MethodPost, "/api/v1/servers/lab-2/apps", map[string]any{"id": "shop"}},
		{http.MethodGet, "/api/v1/servers/lab-2/apps/shop", nil},
		{http.MethodDelete, "/api/v1/servers/lab-2/apps/shop", nil},
		{http.MethodGet, "/api/v1/describe", nil},
		{http.MethodGet, "/api/v1/config", nil},
	}
	for _, route := range routes {
		rec := doRequest(t, h, route.method, route.path, route.body)
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("%s %s returned the stored key: %s", route.method, route.path, rec.Body.String())
		}
	}

	// And the type itself has no field a key could arrive in.
	raw, err := json.Marshal(serverreg.Server{ID: "lab-2", URL: "https://x"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "key") {
		t.Errorf("serverreg.Server marshals a key-shaped field: %s", raw)
	}
}

// Every server route is admin-only: an app key reaches one app, and this is not
// it.
func TestServerRoutesRequireAnAdminKey(t *testing.T) {
	srv, _, _ := newServerWithRegistry(t)
	h := srv.Handler()

	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/servers"},
		{http.MethodPost, "/api/v1/servers"},
		{http.MethodGet, "/api/v1/servers/lab-2"},
		{http.MethodDelete, "/api/v1/servers/lab-2"},
		{http.MethodGet, "/api/v1/servers/lab-2/apps"},
		{http.MethodPost, "/api/v1/servers/lab-2/apps"},
		{http.MethodGet, "/api/v1/servers/lab-2/apps/shop"},
		{http.MethodDelete, "/api/v1/servers/lab-2/apps/shop"},
	}
	for _, route := range routes {
		rec := doRequestNoKey(t, h, route.method, route.path)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a key: got %d, want 401", route.method, route.path, rec.Code)
		}
	}
}

// The proxy is exercised against a *real* second AppLab, not a stub of one.
//
// This is the test the handwritten fake cannot be: the fake answers the routes
// the way this package assumes they are shaped, so it agrees with the proxy by
// construction. A real server on the other end proves the relayed request is one
// the API actually accepts — the paths, the method, the envelope — and that an
// app created through the relay really exists on the remote afterwards.
func TestProxyingToARealAppLabDeployment(t *testing.T) {
	// The remote: a second full server over its own bucket and its own fake
	// cluster, served over HTTP the way a deployment is.
	remoteSrv, _, remoteStore := newDeployServer(t)
	remoteHTTP := httptest.NewServer(remoteSrv.Handler())
	defer remoteHTTP.Close()

	// This deployment, with the remote registered against it.
	local, _, localStore := newServerWithRegistry(t)
	// The remote's admin key, which newDeployServer gives every server.
	rec := register(t, local, "lab-2", remoteHTTP.URL, "test-key")
	if rec.Code != http.StatusCreated {
		t.Fatalf("registering the real remote failed: %d %s", rec.Code, rec.Body.String())
	}
	h := local.Handler()

	// Create through the relay: the app has to exist on the remote, not here.
	rec = doRequest(t, h, http.MethodPost, "/api/v1/servers/lab-2/apps", map[string]any{"id": "shop"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create through the relay: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := remoteStore.GetApp(context.Background(), "shop"); err != nil {
		t.Fatalf("the app was not created on the remote: %v", err)
	}
	if _, err := localStore.GetApp(context.Background(), "shop"); err == nil {
		t.Fatal("the app was created here instead of on the remote")
	}

	// Listing through the relay returns the remote's apps.
	rec = doRequest(t, h, http.MethodGet, "/api/v1/servers/lab-2/apps", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list through the relay: %d %s", rec.Code, rec.Body.String())
	}
	var apps []map[string]any
	decodeData(t, rec, &apps)
	if len(apps) != 1 || apps[0]["id"] != "shop" {
		t.Fatalf("the remote's list was not relayed: %+v", apps)
	}

	// Deleting through the relay removes it on the remote.
	rec = doRequest(t, h, http.MethodDelete, "/api/v1/servers/lab-2/apps/shop", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete through the relay: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := remoteStore.GetApp(context.Background(), "shop"); err == nil {
		t.Fatal("the app was not deleted on the remote")
	}
}

func TestRemovingAServerDoesNotReachTheRemote(t *testing.T) {
	srv, _, _ := newServerWithRegistry(t)
	remote := newFakeRemote(t, "remote-admin")
	if rec := register(t, srv, "lab-2", remote.URL, "remote-admin"); rec.Code != http.StatusCreated {
		t.Fatalf("register: %s", rec.Body.String())
	}

	// The remote goes away entirely.
	remote.Close()

	rec := doRequest(t, srv.Handler(), http.MethodDelete, "/api/v1/servers/lab-2", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, srv.Handler(), http.MethodGet, "/api/v1/servers", nil)
	var servers []map[string]any
	decodeData(t, rec, &servers)
	if len(servers) != 1 {
		t.Fatalf("the registration survived: %+v", servers)
	}
}
