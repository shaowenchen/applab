package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// setEnv issues a PUT .../env with an arbitrary key.
func setEnv(t *testing.T, h http.Handler, appID, key string, env map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return withKey(t, h, http.MethodPut, "/api/v1/apps/"+appID+"/env", key, map[string]any{"env": env})
}

// setSecrets issues a PUT .../secrets with an arbitrary key.
func setSecrets(t *testing.T, h http.Handler, appID, key string, secrets map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return withKey(t, h, http.MethodPut, "/api/v1/apps/"+appID+"/secrets", key, map[string]any{"secrets": secrets})
}

// described names a key for a test failure message without printing it.
func described(key string) string {
	if key == adminKey {
		return "the admin key"
	}
	return "an app key"
}

// TestEnvironmentVariablesRoundTrip asserts a plain variable survives a write
// and comes back — which is the whole difference between it and a secret.
func TestEnvironmentVariablesRoundTrip(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()
	createAppWithKey(t, h, "shop")

	rec := setEnv(t, h, "shop", adminKey, map[string]string{"LOG_LEVEL": "debug"})
	if rec.Code != http.StatusOK {
		t.Fatalf("set env: %d (%s)", rec.Code, rec.Body.String())
	}

	var got configResponse
	decodeData(t, rec, &got)
	if got.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("env = %v, want LOG_LEVEL=debug", got.Env)
	}

	// And it is readable through a GET, which is the point of the split.
	rec = doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/config", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get config: %d (%s)", rec.Code, rec.Body.String())
	}
	decodeData(t, rec, &got)
	if got.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("env after a fresh read = %v, want LOG_LEVEL=debug", got.Env)
	}
}

// TestSecretValueIsNeverReturnedByAnyRoute is the promise the whole design
// exists to keep, asserted across every route that could conceivably leak one.
//
// It is written as a sweep rather than as one assertion against the config
// route, because the risk is not that this route leaks — it is that some future
// route does. A new endpoint that returned a Secret would have to be added to
// this list to be checked, which is the wrong way round, so instead the sweep
// covers every route the server knows about.
func TestSecretValueIsNeverReturnedByAnyRoute(t *testing.T) {
	const secretValue = "postgres://user:hunter2@db.internal:5432/app"

	srv, _ := newTieredServer(t)
	h := srv.Handler()
	key := createAppWithKey(t, h, "shop")

	if rec := setSecrets(t, h, "shop", adminKey, map[string]string{"DATABASE_URL": secretValue}); rec.Code != http.StatusOK {
		t.Fatalf("set secret: %d (%s)", rec.Code, rec.Body.String())
	}

	// Every route the server exposes, with {app} resolved to the app that holds
	// the secret. Anything that answers must not contain the value.
	for _, pattern := range srv.SortedPatterns() {
		method, path, _ := strings.Cut(pattern, " ")
		if strings.Contains(path, "{name}") {
			path = strings.ReplaceAll(path, "{name}", "DATABASE_URL")
		}
		if strings.Contains(path, "{build}") {
			path = strings.ReplaceAll(path, "{build}", "0102030405060708")
		}
		path = strings.ReplaceAll(path, "{app}", "shop")

		for _, k := range []string{adminKey, key} {
			rec := withKey(t, h, method, path, k, nil)
			body := rec.Body.String()

			if strings.Contains(body, secretValue) || strings.Contains(body, "hunter2") {
				t.Errorf("%s %s (with %s) returned the secret's value:\n%s",
					method, path, described(k), body)
			}
		}
	}
}

// TestSecretNamesAreReturnedWithoutValues is the intended half of that: the
// console and CLI need to know which secrets an app has, and nothing more.
func TestSecretNamesAreReturnedWithoutValues(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()
	createAppWithKey(t, h, "shop")

	rec := setSecrets(t, h, "shop", adminKey, map[string]string{"DATABASE_URL": "postgres://x", "API_TOKEN": "t"})
	if rec.Code != http.StatusOK {
		t.Fatalf("set secrets: %d (%s)", rec.Code, rec.Body.String())
	}

	var got configResponse
	decodeData(t, rec, &got)
	if strings.Join(got.Secrets, ",") != "API_TOKEN,DATABASE_URL" {
		t.Errorf("secrets = %v, want both names, sorted", got.Secrets)
	}

	rec = doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/config", nil)
	decodeData(t, rec, &got)
	if strings.Join(got.Secrets, ",") != "API_TOKEN,DATABASE_URL" {
		t.Errorf("secrets after a fresh read = %v, want both names", got.Secrets)
	}
}

// TestSetEnvLeavesOtherVariablesAlone covers the merge semantics a person
// depends on when adding a second variable by hand.
func TestSetEnvLeavesOtherVariablesAlone(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()
	createAppWithKey(t, h, "shop")

	if rec := setEnv(t, h, "shop", adminKey, map[string]string{"A": "1", "B": "2"}); rec.Code != http.StatusOK {
		t.Fatalf("first set: %d (%s)", rec.Code, rec.Body.String())
	}
	rec := setEnv(t, h, "shop", adminKey, map[string]string{"C": "3"})
	if rec.Code != http.StatusOK {
		t.Fatalf("second set: %d (%s)", rec.Code, rec.Body.String())
	}

	var got configResponse
	decodeData(t, rec, &got)
	if got.Env["A"] != "1" || got.Env["B"] != "2" || got.Env["C"] != "3" {
		t.Errorf("env = %v, want A, B and C all present", got.Env)
	}
}

// TestDeleteEnvRemovesOneVariable asserts removal is its own operation, so that
// "set this to nothing" and "this is not set" stay different things.
func TestDeleteEnvRemovesOneVariable(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()
	createAppWithKey(t, h, "shop")

	if rec := setEnv(t, h, "shop", adminKey, map[string]string{"A": "1", "B": "2"}); rec.Code != http.StatusOK {
		t.Fatalf("set: %d (%s)", rec.Code, rec.Body.String())
	}

	rec := doRequest(t, h, http.MethodDelete, "/api/v1/apps/shop/env/A", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d (%s)", rec.Code, rec.Body.String())
	}

	var got configResponse
	decodeData(t, rec, &got)
	if _, present := got.Env["A"]; present {
		t.Errorf("A is still present: %v", got.Env)
	}
	if got.Env["B"] != "2" {
		t.Errorf("B was lost: %v", got.Env)
	}
}

// TestReservedPortIsRefused asserts the one name that cannot be set is refused
// with an explanation rather than accepted and silently overridden.
//
// PORT comes from the app's port setting, which is also what the Service
// targets. Accepting it would put two declarations in the pod spec; the later
// one wins and the app listens where nothing routes, with no error anywhere.
func TestReservedPortIsRefused(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()
	createAppWithKey(t, h, "shop")

	rec := setEnv(t, h, "shop", adminKey, map[string]string{"PORT": "9999"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("setting PORT returned %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "port") {
		t.Errorf("the error should say where the value comes from, got: %s", rec.Body.String())
	}

	// The same for a secret: it would land in the same environment.
	rec = setSecrets(t, h, "shop", adminKey, map[string]string{"PORT": "9999"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("setting PORT as a secret returned %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
}

// TestInvalidNamesAreRefused asserts a name that is not a legal environment
// variable is rejected at the edge, where it can be explained.
func TestInvalidNamesAreRefused(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()
	createAppWithKey(t, h, "shop")

	for _, name := range []string{"1BAD", "has space", "has=equals"} {
		rec := setEnv(t, h, "shop", adminKey, map[string]string{name: "x"})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("setting %q returned %d, want 400 (%s)", name, rec.Code, rec.Body.String())
		}
	}
}

// TestAppKeyReachesItsOwnConfigAndNotAnothers is the scoping requirement applied
// to the new routes.
//
// Without it, the config endpoints would be the hole in the two-tier model: an
// app key could read or overwrite another app's secrets.
func TestAppKeyReachesItsOwnConfigAndNotAnothers(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()
	shopKey := createAppWithKey(t, h, "shop")
	createAppWithKey(t, h, "blog")

	// Its own: both reading and writing.
	if rec := withKey(t, h, http.MethodGet, "/api/v1/apps/shop/config", shopKey, nil); rec.Code != http.StatusOK {
		t.Errorf("an app key could not read its own config: %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := setEnv(t, h, "shop", shopKey, map[string]string{"LOG_LEVEL": "debug"}); rec.Code != http.StatusOK {
		t.Errorf("an app key could not write its own variables: %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := setSecrets(t, h, "shop", shopKey, map[string]string{"TOKEN": "t"}); rec.Code != http.StatusOK {
		t.Errorf("an app key could not write its own secrets: %d (%s)", rec.Code, rec.Body.String())
	}

	// Another app's: every method, every path. A 404 rather than a 403, so the
	// existence of another app is not confirmed.
	attempts := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/v1/apps/blog/config", nil},
		{http.MethodPut, "/api/v1/apps/blog/env", map[string]any{"env": map[string]string{"X": "1"}}},
		{http.MethodDelete, "/api/v1/apps/blog/env/X", nil},
		{http.MethodPut, "/api/v1/apps/blog/secrets", map[string]any{"secrets": map[string]string{"X": "1"}}},
		{http.MethodDelete, "/api/v1/apps/blog/secrets/X", nil},
	}
	for _, a := range attempts {
		rec := withKey(t, h, a.method, a.path, shopKey, a.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s with another app's key returned %d, want 404 (%s)",
				a.method, a.path, rec.Code, rec.Body.String())
		}
	}
}

// TestConfigOfAMissingAppIs404 asserts the routes do not invent an app.
func TestConfigOfAMissingAppIs404(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()

	for _, path := range []string{"/api/v1/apps/nope/config"} {
		if rec := doRequest(t, h, http.MethodGet, path, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s returned %d, want 404", path, rec.Code)
		}
	}
}

// TestConfigEndpointsRefuseAnUnknownKey asserts nothing here is open.
func TestConfigEndpointsRefuseAnUnknownKey(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()
	createAppWithKey(t, h, "shop")

	attempts := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/v1/apps/shop/config", nil},
		{http.MethodPut, "/api/v1/apps/shop/env", map[string]any{"env": map[string]string{"X": "1"}}},
		{http.MethodPut, "/api/v1/apps/shop/secrets", map[string]any{"secrets": map[string]string{"X": "1"}}},
	}
	for _, a := range attempts {
		rec := withKey(t, h, a.method, a.path, "not-a-key", a.body)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with an unknown key returned %d, want 401 (%s)",
				a.method, a.path, rec.Code, rec.Body.String())
		}
	}
}

// TestEnvDeleteReachesNamesWithDots is why the {name} segment is decoded.
//
// An environment variable name may contain a dot, which a caller will
// percent-encode in a path. Validating the still-encoded segment would reject a
// perfectly legal name for the wrong reason.
func TestEnvDeleteReachesNamesWithDots(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()
	createAppWithKey(t, h, "shop")

	if rec := setEnv(t, h, "shop", adminKey, map[string]string{"has.dot": "1", "keep": "2"}); rec.Code != http.StatusOK {
		t.Fatalf("set: %d (%s)", rec.Code, rec.Body.String())
	}

	rec := doRequest(t, h, http.MethodDelete, "/api/v1/apps/shop/env/has.dot", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d (%s)", rec.Code, rec.Body.String())
	}

	var got configResponse
	decodeData(t, rec, &got)
	if _, present := got.Env["has.dot"]; present {
		t.Errorf("has.dot is still present: %v", got.Env)
	}
	if got.Env["keep"] != "2" {
		t.Errorf("keep was lost: %v", got.Env)
	}
}

// TestEnvCountIsReportedWithoutValues lets a list view show an app is configured
// without shipping every value with it.
func TestEnvCountIsReportedWithoutValues(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()
	createAppWithKey(t, h, "shop")

	if rec := setEnv(t, h, "shop", adminKey, map[string]string{"A": "1", "B": "2"}); rec.Code != http.StatusOK {
		t.Fatalf("set: %d (%s)", rec.Code, rec.Body.String())
	}

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get app: %d (%s)", rec.Code, rec.Body.String())
	}

	var raw struct {
		EnvCount int `json:"env_count"`
	}
	decodeData(t, rec, &raw)
	if raw.EnvCount != 2 {
		t.Errorf("env_count = %d, want 2", raw.EnvCount)
	}

	// And the values themselves are not in the app response.
	if strings.Contains(rec.Body.String(), "\"A\":\"1\"") {
		t.Errorf("the app response carries the variable values: %s", rec.Body.String())
	}
}

// TestDeletingAnAppRemovesItsSecrets covers the keep_source path, where the
// cluster teardown still runs but the record is kept.
//
// A Secret left behind is live credentials for an app that no longer exists.
func TestDeletingAnAppRemovesItsSecrets(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()
	createAppWithKey(t, h, "shop")

	if rec := setSecrets(t, h, "shop", adminKey, map[string]string{"TOKEN": "t"}); rec.Code != http.StatusOK {
		t.Fatalf("set secret: %d (%s)", rec.Code, rec.Body.String())
	}

	rec := doRequest(t, h, http.MethodDelete, "/api/v1/apps/shop?keep_source=true", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete app: %d (%s)", rec.Code, rec.Body.String())
	}

	// The app is gone, so its config route is gone with it.
	if rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/config", nil); rec.Code != http.StatusNotFound {
		t.Errorf("the deleted app's config returned %d, want 404", rec.Code)
	}
}

// configResponse mirrors the API's config payload for decoding in tests.
type configResponse struct {
	AppID   string            `json:"app_id"`
	Env     map[string]string `json:"env"`
	Secrets []string          `json:"secrets"`
}

// TestNoSecretsSerialisesAsAnEmptyList asserts the response says "none" rather
// than "unknown" (null).
//
// A client iterating the field would otherwise have to handle null, and the
// console's card would have to special-case it.
func TestNoSecretsSerialisesAsAnEmptyList(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()
	createAppWithKey(t, h, "shop")

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/config", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get config: %d (%s)", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, `"secrets":null`) {
		t.Errorf("secrets serialised as null: %s", body)
	}
	if !strings.Contains(body, `"secrets":[]`) {
		t.Errorf("secrets did not serialise as an empty list: %s", body)
	}
	if !strings.Contains(body, `"env":{}`) {
		t.Errorf("env did not serialise as an empty object: %s", body)
	}
}
