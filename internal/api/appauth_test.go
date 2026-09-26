package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/appkey"
	"github.com/shaowenchen/applab/internal/auth"
	"github.com/shaowenchen/applab/internal/config"
	"github.com/shaowenchen/applab/internal/source"
	"github.com/shaowenchen/applab/internal/store"
)

// adminKey is the key this suite's server is configured with.
const adminKey = "test-key"

// newTieredServer builds a server with both key tiers wired.
//
// The two per-app stores read and write AppLab's own object storage, so there is
// nothing cluster-shaped about them and nothing to attach: the Server builds
// them from the same store it was handed. That is worth stating here because the
// function used to exist to wire a fake clientset in, and what it exercises now
// is the real resolution path — listing the key records, comparing digests,
// deciding scope — against a real store.
func newTieredServer(t *testing.T) (*api.Server, *store.Store) {
	t.Helper()
	return newTieredServerWith(t, nil)
}

// newTieredServerWith is newTieredServer with a last word on the configuration.
//
// It exists for the tests whose subject is the configuration itself — the base
// path, the path prefix, the public address — which all have to be asserted
// through the files the server writes rather than in isolation.
func newTieredServerWith(t *testing.T, tweak func(*config.Config)) (*api.Server, *store.Store) {
	t.Helper()

	srv, st, _ := newTieredServerWithSource(t, tweak)
	return srv, st
}

// newTieredServerWithSource is newTieredServerWith, and also hands back the
// source store.
//
// It exists for the assertions that are about what was *committed* rather than
// what is served: the files an app's tree carries are written when the
// repository is created and again on every upload, and the served copy is
// rendered fresh from the templates either way — so a server that seeded the
// opening commit without the app's key still serves a file that has it.
func newTieredServerWithSource(t *testing.T, tweak func(*config.Config)) (*api.Server, *store.Store, *source.Store) {
	t.Helper()

	dataDir := t.TempDir()
	st, err := store.OpenLocal(context.Background(), filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	// PublicURL and SeedKey are attached exactly as the binary attaches them, so
	// the files this suite fetches are the files a deployment would write. The
	// key lookup is the same store the server mints keys from — a test that
	// wired a different one would pass while the real seeding wrote the wrong
	// credential into every app's script.
	src, err := source.New(source.Options{
		Objects:   newObjects(t),
		DataDir:   dataDir,
		PublicURL: "https://applab.example.com",
		SeedKey:   appkey.New(st).Get,
	})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	cfg := config.Default()
	cfg.Keys = []string{adminKey}
	cfg.BaseDomain = "apps.example.com"
	cfg.DataDir = dataDir
	if tweak != nil {
		tweak(&cfg)
	}

	return api.New(cfg, st, auth.New(cfg.Keys)).WithSource(src), st, src
}

// withKey issues a request carrying an arbitrary key.
func withKey(t *testing.T, h http.Handler, method, path, key string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// createAppWithKey creates an app and returns its freshly minted key.
func createAppWithKey(t *testing.T, h http.Handler, appID string) string {
	t.Helper()

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": appID}); rec.Code != http.StatusCreated {
		t.Fatalf("create app %s: %d (%s)", appID, rec.Code, rec.Body.String())
	}

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/"+appID+"/key", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("read key for %s: %d (%s)", appID, rec.Code, rec.Body.String())
	}
	var out struct {
		Key string `json:"key"`
	}
	decodeData(t, rec, &out)
	if out.Key == "" {
		t.Fatalf("app %s was created without a key", appID)
	}
	return out.Key
}

// TestAppKeyIsMintedWithTheApp asserts every app has a key from birth, so there
// is no half-configured state where an app exists but cannot be managed by its
// own credential.
func TestAppKeyIsMintedWithTheApp(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()

	key := createAppWithKey(t, h, "shop")

	// The key must be usable immediately, and must reach its own app.
	rec := withKey(t, h, http.MethodGet, "/api/v1/apps/shop", key, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("an app key could not read its own app: %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestAppKeyCannotReachAnotherApp is the boundary the whole tier exists for.
//
// The status is asserted as 404 rather than 403 deliberately: a 403 would
// confirm that the named app exists, which turns every scoped route into a way
// to enumerate the platform for anyone holding one app's key.
func TestAppKeyCannotReachAnotherApp(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()

	shopKey := createAppWithKey(t, h, "shop")
	createAppWithKey(t, h, "blog")

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/apps/blog"},
		{http.MethodGet, "/api/v1/apps/blog/status"},
		{http.MethodGet, "/api/v1/apps/blog/builds"},
		{http.MethodGet, "/api/v1/apps/blog/commits"},
		{http.MethodGet, "/api/v1/apps/blog/key"},
		{http.MethodPost, "/api/v1/apps/blog/restart"},
		{http.MethodPost, "/api/v1/apps/blog/stop"},
		{http.MethodDelete, "/api/v1/apps/blog"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := withKey(t, h, tc.method, tc.path, shopKey, nil)
			if rec.Code != http.StatusNotFound {
				t.Errorf("shop's key reached %s %s: got %d, want 404 so the app's existence is not disclosed (body: %s)",
					tc.method, tc.path, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestAppKeyMayManageItsOwnApp asserts the tier is useful, not merely safe: an
// app key must be able to do everything its app needs.
func TestAppKeyMayManageItsOwnApp(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()

	key := createAppWithKey(t, h, "shop")

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		// Reading.
		{http.MethodGet, "/api/v1/apps/shop", http.StatusOK},
		{http.MethodGet, "/api/v1/apps/shop/status", http.StatusOK},
		{http.MethodGet, "/api/v1/apps/shop/builds", http.StatusOK},
		{http.MethodGet, "/api/v1/apps/shop/commits", http.StatusOK},
		{http.MethodGet, "/api/v1/apps/shop/key", http.StatusOK},
		// Changing the app's settings.
		{http.MethodPatch, "/api/v1/apps/shop", http.StatusOK},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := withKey(t, h, tc.method, tc.path, key, map[string]any{})
			if rec.Code != tc.want {
				t.Errorf("an app key got %d for %s %s, want %d (body: %s)",
					rec.Code, tc.method, tc.path, tc.want, rec.Body.String())
			}
		})
	}
}

// TestAppKeyCannotDeleteItsApp asserts the one operation an app key is denied.
//
// Deploying is already the power to change what runs, so withholding delete is
// not about capability — it is that deleting destroys the source and the history
// with it, irreversibly, and that is the operator's decision rather than the
// app owner's.
func TestAppKeyCannotDeleteItsApp(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()

	key := createAppWithKey(t, h, "shop")

	rec := withKey(t, h, http.MethodDelete, "/api/v1/apps/shop", key, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an app key deleting its own app got %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
	// The refusal names the rule and the fix, because the caller is the one
	// person who can act on it.
	if !strings.Contains(rec.Body.String(), "admin key") {
		t.Errorf("the refusal does not say an admin key is needed: %s", rec.Body.String())
	}

	// And the app is still there.
	if rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop", nil); rec.Code != http.StatusOK {
		t.Errorf("the app was deleted despite the refusal: %d", rec.Code)
	}
}

// TestAdminKeyCanStillDelete asserts the denial above is the app tier's alone.
func TestAdminKeyCanStillDelete(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()

	createAppWithKey(t, h, "shop")

	if rec := doRequest(t, h, http.MethodDelete, "/api/v1/apps/shop", nil); rec.Code != http.StatusOK {
		t.Errorf("an admin key could not delete an app: %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestAppKeyIsRefusedTheAdminSurface asserts the routes with no app to scope to
// stay out of reach.
func TestAppKeyIsRefusedTheAdminSurface(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()

	key := createAppWithKey(t, h, "shop")

	for _, tc := range []struct{ method, path string }{
		// The platform overview counts every app.
		{http.MethodGet, "/api/v1/overview"},
		// Creating apps is how the set of apps is changed.
		{http.MethodPost, "/api/v1/apps"},
		// The control plane's own pods and log. It runs in the same namespace
		// as every app, and its log names them, their commits and their
		// failures — so an app key reaching it would read past its own app
		// through a route with no {app} to scope against.
		{http.MethodGet, "/api/v1/platform/pods"},
		{http.MethodGet, "/api/v1/platform/logs"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := withKey(t, h, tc.method, tc.path, key, map[string]any{"id": "sneaky"})
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("an app key got %d for %s %s, want 401 (body: %s)",
					rec.Code, tc.method, tc.path, rec.Body.String())
			}
		})
	}
}

// TestAppKeySeesOnlyItsOwnAppInTheList covers the one collection an app key may
// read — and the reason it exists: the console needs a way to learn which app a
// key belongs to before it can ask for that app.
func TestAppKeySeesOnlyItsOwnAppInTheList(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()

	key := createAppWithKey(t, h, "shop")
	createAppWithKey(t, h, "blog")
	createAppWithKey(t, h, "other")

	rec := withKey(t, h, http.MethodGet, "/api/v1/apps", key, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("an app key could not list apps: %d (%s)", rec.Code, rec.Body.String())
	}

	var apps []map[string]any
	decodeData(t, rec, &apps)

	if len(apps) != 1 {
		t.Fatalf("an app key saw %d apps, want exactly its own: %v", len(apps), apps)
	}
	if apps[0]["id"] != "shop" {
		t.Errorf("an app key saw app %q, want shop", apps[0]["id"])
	}

	// The admin key still sees everything, so the filter is scoped to the tier
	// rather than applied to the route.
	rec = doRequest(t, h, http.MethodGet, "/api/v1/apps", nil)
	var all []map[string]any
	decodeData(t, rec, &all)
	if len(all) != 3 {
		t.Errorf("the admin key saw %d apps, want all 3", len(all))
	}
}

// TestAnUnknownKeyIsRefusedEverything asserts an unrecognised credential is a
// 401 everywhere, indistinguishable from a merely wrong admin key.
func TestAnUnknownKeyIsRefusedEverything(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()

	createAppWithKey(t, h, "shop")

	for _, path := range []string{"/api/v1/apps", "/api/v1/apps/shop", "/api/v1/apps/shop/key", "/api/v1/overview"} {
		t.Run(path, func(t *testing.T) {
			rec := withKey(t, h, http.MethodGet, path, "not-a-real-key", nil)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("an unknown key got %d for %s, want 401", rec.Code, path)
			}
		})
	}
}

// TestRotationInvalidatesTheOldKeyImmediately is the property that makes
// rotation worth doing: a leaked key must stop working the moment it is replaced.
func TestRotationInvalidatesTheOldKeyImmediately(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()

	oldKey := createAppWithKey(t, h, "shop")

	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/key/rotate", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d (%s)", rec.Code, rec.Body.String())
	}
	var rotated struct {
		Key string `json:"key"`
	}
	decodeData(t, rec, &rotated)
	if rotated.Key == "" || rotated.Key == oldKey {
		t.Fatalf("rotation returned %q, want a new key distinct from the old one", rotated.Key)
	}

	// The old key is dead.
	if rec := withKey(t, h, http.MethodGet, "/api/v1/apps/shop", oldKey, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("the old key still works after rotation: %d", rec.Code)
	}
	// The new one is live.
	if rec := withKey(t, h, http.MethodGet, "/api/v1/apps/shop", rotated.Key, nil); rec.Code != http.StatusOK {
		t.Errorf("the new key does not work: %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestAppKeyIsReadableByItsOwnerAndByAnAdmin covers the deployment's choice that
// a key is recoverable rather than shown once.
func TestAppKeyIsReadableByItsOwnerAndByAnAdmin(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()

	key := createAppWithKey(t, h, "shop")

	// By its own key.
	rec := withKey(t, h, http.MethodGet, "/api/v1/apps/shop/key", key, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("an app key could not read itself: %d (%s)", rec.Code, rec.Body.String())
	}
	var own struct {
		AppID string `json:"app_id"`
		Key   string `json:"key"`
	}
	decodeData(t, rec, &own)
	if own.Key != key || own.AppID != "shop" {
		t.Errorf("read back (%q, %q), want (shop, %q)", own.AppID, own.Key, key)
	}

	// By an admin.
	rec = doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/key", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("an admin key could not read an app key: %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestDeletingAnAppRemovesItsKey asserts no credential outlives the app it
// belonged to.
func TestDeletingAnAppRemovesItsKey(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()

	key := createAppWithKey(t, h, "shop")

	if rec := doRequest(t, h, http.MethodDelete, "/api/v1/apps/shop", nil); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d (%s)", rec.Code, rec.Body.String())
	}

	// The key resolves to nothing now, so it is refused rather than treated as
	// a credential for an app that no longer exists.
	if rec := withKey(t, h, http.MethodGet, "/api/v1/apps", key, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("a deleted app's key is still accepted: %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestWithoutAClusterTheAppTierStillExists covers the deployment that runs with
// no cluster at all, which is a documented way to run applab.
//
// The app tier used to be missing in that shape: keys were Secrets, so a
// deployment with no API server had nowhere to keep one and the routes answered
// 501. They live in the object store now, which every deployment has, so the
// only thing a cluster is still needed for is building and deploying — and this
// asserts the boundary sits there rather than at a credential.
func TestWithoutAClusterTheAppTierStillExists(t *testing.T) {
	// newTestServer attaches no cluster-backed capability of any kind.
	srv, _ := newTestServer(t)
	h := srv.Handler()

	// The admin tier works as it always did.
	if rec := doRequest(t, h, http.MethodGet, "/api/v1/apps", nil); rec.Code != http.StatusOK {
		t.Errorf("the admin key stopped working without a cluster: %d", rec.Code)
	}

	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d (%s)", rec.Code, rec.Body.String())
	}

	// And so does the app tier, including the key minted with the app.
	rec = doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/key", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reading a key with no cluster got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Key string `json:"key"`
	}
	decodeData(t, rec, &got)
	if got.Key == "" {
		t.Fatal("the app was created with no key")
	}

	if rec := withKey(t, h, http.MethodGet, "/api/v1/apps/shop", got.Key, nil); rec.Code != http.StatusOK {
		t.Errorf("an app key could not authenticate without a cluster: %d (%s)", rec.Code, rec.Body.String())
	}

	// What a cluster is still required for, and the only thing: a deploy.
	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", nil); rec.Code != http.StatusNotImplemented {
		t.Errorf("deploying with no cluster got %d, want 501 naming the missing capability (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestAppKeyCannotBeUsedAsAnAdminKeyOnTheGitTransport asserts the repository
// boundary, which is where a repository's own secrets live.
//
// The transport is mounted as one prefix covering every repository, so this is
// the one boundary the route table cannot express — it is enforced inside the
// handler, by Server.AuthorizeGitRepo.
func TestAppKeyCannotBeUsedAsAnAdminKeyOnTheGitTransport(t *testing.T) {
	srv, _ := newTieredServer(t)

	// The authorize hook is what main.go attaches, so the decision under test is
	// the one the real deployment makes.
	authorize := srv.AuthorizeGitRepo

	requestWithKey := func(key string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/git/shop.git/info/refs", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		return req
	}

	// A key belonging to shop, obtained the way a caller would.
	h := srv.Handler()
	shopKey := createAppWithKey(t, h, "shop")
	createAppWithKey(t, h, "blog")

	if !authorize(requestWithKey(shopKey), "shop") {
		t.Error("shop's key was refused its own repository")
	}
	if authorize(requestWithKey(shopKey), "blog") {
		t.Error("shop's key reached blog's repository; an app key must not read another app's source")
	}
	if !authorize(requestWithKey(adminKey), "blog") {
		t.Error("the admin key was refused a repository")
	}
	if authorize(requestWithKey("not-a-key"), "shop") {
		t.Error("an unknown key reached a repository")
	}
	if authorize(requestWithKey(shopKey), "") {
		t.Error("a repository with no name was authorized")
	}
}

// TestTheGitTransportChallengesWithBasic is the test whose absence let a broken
// clone ship.
//
// A key in a clone URL is sent as a Basic credential, and git holds it back
// until the server challenges for it — RFC 7617, and libcurl behind git
// implements it strictly. A `Bearer` challenge leaves git with no way to present
// what it already has, so it fails without ever sending a credential, and the
// user sees "Authentication failed" for a key that works everywhere else.
//
// The API routes are unaffected and must stay on the Bearer challenge: nothing
// there is driven by libcurl's credential handling, and a Basic challenge would
// invite a browser's credential prompt over an API endpoint.
func TestTheGitTransportChallengesWithBasic(t *testing.T) {
	srv, _ := newTieredServer(t)

	// Git has to be attached, or the mount is absent and every /git/ request is
	// the console's 404 rather than the transport's 401 — which would make this
	// pass against a deployment that serves no repositories at all. The handler
	// itself is never reached: the middleware refuses first, and that is what is
	// being checked.
	srv.WithGit(http.NotFoundHandler())
	h := srv.Handler()

	// No credential: the refusal itself is what is under test.
	rec := doRequestNoKey(t, h, http.MethodGet, "/git/shop.git/info/refs?service=git-upload-pack")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated git request got %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}
	challenge := rec.Header().Get("WWW-Authenticate")
	if !strings.HasPrefix(strings.ToLower(challenge), "basic") {
		t.Errorf("the git transport challenged with %q, so a clone URL's credential is never sent; git needs a Basic challenge", challenge)
	}

	// And the API keeps the scheme that matches how it is actually called.
	rec = doRequestNoKey(t, h, http.MethodGet, "/api/v1/apps")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated API request got %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(strings.ToLower(got), "bearer") {
		t.Errorf("the API challenged with %q, want Bearer", got)
	}
}

// TestTheConsoleCanDiscoverItsOwnApp covers the reason the app list is readable
// by an app key: a console signing in with one has to learn what to show.
func TestTheConsoleCanDiscoverItsOwnApp(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()

	key := createAppWithKey(t, h, "shop")
	createAppWithKey(t, h, "blog")

	rec := withKey(t, h, http.MethodGet, "/api/v1/apps", key, nil)
	var apps []struct {
		ID string `json:"id"`
	}
	decodeData(t, rec, &apps)

	if len(apps) != 1 || apps[0].ID != "shop" {
		t.Errorf("the console would discover %v, want only shop", apps)
	}
}

// TestKeysAreNotSharedBetweenApps asserts two apps never get the same credential
// — the failure that would silently make the tiers pointless.
func TestKeysAreNotSharedBetweenApps(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()

	seen := map[string]string{}
	for _, id := range []string{"shop", "blog", "other"} {
		key := createAppWithKey(t, h, id)
		if prev, dup := seen[key]; dup {
			t.Fatalf("apps %s and %s were given the same key", prev, id)
		}
		seen[key] = id
		// An app key must not authenticate as the admin tier either.
		if key == adminKey {
			t.Fatal("an app was given the admin key")
		}
	}
}

// TestEveryAppScopedRouteNamesAnApp is the structural invariant the two-tier
// model depends on.
//
// appAuthMiddleware scopes an app key by reading {app} from the path, so a route
// marked appPathScoped whose pattern has no {app} would answer 500 to every app
// key — a failure that appears only for the hardest tier to exercise by hand.
// The flags are therefore checked against the patterns that carry them, rather
// than trusted to have been applied correctly when the route was written.
//
// The three flags mean three different things and each is asserted:
//
//	AppAuth        requires {app}, because an app key is scoped by it
//	AppAdminOnly   implies AppAuth, because it narrows an existing scope
//	AppListScope   forbids {app}, because the handler narrows the result set
func TestEveryAppScopedRouteNamesAnApp(t *testing.T) {
	srv, _ := newTestServer(t)

	checked := 0
	for _, pattern := range srv.SortedPatterns() {
		scoped := srv.PatternAcceptsAppKey(pattern)
		_, path, _ := strings.Cut(pattern, " ")
		hasApp := strings.Contains(path, "{app}")

		if !scoped {
			continue
		}
		checked++

		// An app-key route that is not authenticated would serve the public.
		if !srv.PatternRequiresAuth(pattern) {
			t.Errorf("route %q accepts an app key but is not authenticated at all", pattern)
		}

		// The list-scoped flag is the only one legal without {app}...
		if !hasApp && !srv.PatternIsAppListScoped(pattern) {
			t.Errorf("route %q accepts an app key, has no {app}, and is not marked AppListScope — "+
				"an app key cannot be scoped against it", pattern)
		}
		// ...and it is never legal with one.
		if hasApp && srv.PatternIsAppListScoped(pattern) {
			t.Errorf("route %q is marked AppListScope but names {app}; it should be AppAuth", pattern)
		}
	}

	if checked == 0 {
		t.Fatal("no route accepts an app key, so this test proves nothing")
	}
	t.Logf("checked %d app-key routes", checked)

	// The boundary the whole model rests on: an app key cannot reach the
	// platform-wide routes, which have no app to scope to.
	for _, pattern := range []string{"GET /api/v1/overview", "POST /api/v1/apps"} {
		if srv.PatternAcceptsAppKey(pattern) {
			t.Errorf("%q accepts an app key, but there is no app for it to be scoped to", pattern)
		}
	}
}

// TestAppCollectionRouteIsListScoped asserts the one route without an {app} that
// accepts an app key is the app list, and that an app key really is narrowed
// there.
//
// It is the route the console uses to learn which app a key belongs to, so it
// has to be reachable — but it is also the only route where an app key's scope is
// applied by the handler rather than by the middleware, which is exactly the
// asymmetry that should be asserted rather than remembered.
func TestAppCollectionRouteIsListScoped(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()

	if !srv.PatternAcceptsAppKey("GET /api/v1/apps") {
		t.Fatal("GET /api/v1/apps does not accept an app key; the console cannot discover which app a key is for")
	}

	key := createAppWithKey(t, h, "shop")
	createAppWithKey(t, h, "blog")

	rec := withKey(t, h, http.MethodGet, "/api/v1/apps", key, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("an app key listing apps got %d (%s)", rec.Code, rec.Body.String())
	}

	var apps []struct {
		ID string `json:"id"`
	}
	decodeData(t, rec, &apps)
	if len(apps) != 1 {
		t.Errorf("the collection route returned %d apps to an app key, want 1 — the handler is responsible for narrowing it", len(apps))
	}
}
