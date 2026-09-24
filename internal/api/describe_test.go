package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// describeFor decodes GET /api/v1/describe with the admin key.
func describeFor(t *testing.T, srv interface {
	Handler() http.Handler
}) map[string]any {
	t.Helper()

	rec := doRequest(t, srv.Handler(), http.MethodGet, "/api/v1/describe", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/describe = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var body map[string]any
	decodeData(t, rec, &body)
	return body
}

// TestDescribeOrientsAnAgent asserts the endpoint answers the questions an agent
// that has just been handed an address and a key actually has.
//
// It is not a contract test — llms.txt covers which routes exist and what they
// take. This asserts the things that would make the endpoint useless if missing:
// where this is, what it can do, what the key reaches, and what already exists.
func TestDescribeOrientsAnAgent(t *testing.T) {
	srv, _ := newTieredServer(t)
	body := describeFor(t, srv)

	// A one-line answer to "what is this", so an agent can report it without
	// assembling a sentence from six fields.
	summary, _ := body["summary"].(string)
	for _, want := range []string{"applab", "namespace", "app"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q does not mention %q", summary, want)
		}
	}

	// How to reach it, including the header every other call needs.
	apiSection := section(t, body, "api")
	if apiSection["base_url"] == "" {
		t.Error("api.base_url is empty; a caller with no other context cannot build a single request")
	}
	if !strings.Contains(apiSection["auth_header"].(string), "Bearer") {
		t.Errorf("api.auth_header is %q, want the header form a caller must send", apiSection["auth_header"])
	}

	// What it is wired to. The build and deploy halves are the two that decide
	// what is possible, and each has to be answerable before it is tried.
	build := section(t, body, "build")
	if _, present := build["enabled"]; !present {
		t.Error("build.enabled is absent; whether this deployment can build is the first thing to know")
	}
	deploy := section(t, body, "deploy")
	if deploy["gateway"] == nil {
		t.Error("deploy.gateway is absent; apps are published through it or not at all")
	}

	// What the key in hand reaches. An app key is not a lesser admin key, so
	// which one this is has to be stated rather than assumed.
	access := section(t, body, "access")
	if access["tier"] != "admin" {
		t.Errorf("access.tier = %v, want admin for the admin key", access["tier"])
	}
	if access["reach"] == "" {
		t.Error("access.reach is empty; the difference between the tiers is what an agent must not get wrong")
	}

	// What already exists.
	if _, present := body["apps"]; !present {
		t.Error("apps is absent; an agent has no way to tell an empty platform from an unlisted one")
	}

	// And the shortest path to acting, which is the difference between an
	// endpoint an agent reads and one it can use.
	howTo := section(t, body, "how_to")
	httpCalls := section(t, howTo, "http")
	for _, op := range []string{"create_app", "upload", "build", "deploy", "logs", "app_key"} {
		if httpCalls[op] == nil {
			t.Errorf("how_to.http.%s is absent; an agent would have to guess that call", op)
		}
	}
}

// TestDescribeNamesTheAppsItLists asserts the app list carries what a caller
// needs to use an app, rather than only that it exists.
func TestDescribeNamesTheAppsItLists(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()
	createAppWithKey(t, h, "shop")

	body := describeFor(t, srv)

	apps, ok := body["apps"].([]any)
	if !ok || len(apps) != 1 {
		t.Fatalf("apps = %v, want the one app that was created", body["apps"])
	}
	app, ok := apps[0].(map[string]any)
	if !ok {
		t.Fatalf("apps[0] is %T, want an object", apps[0])
	}
	if app["id"] != "shop" {
		t.Errorf("apps[0].id = %v, want shop", app["id"])
	}
	// The address an app is served at is the thing a caller most often wants
	// next, and it is not derivable from the id alone — a path prefix changes it.
	if app["hostname"] == nil && app["path"] == nil {
		t.Errorf("apps[0] carries neither hostname nor path: %v", app)
	}
}

// TestDescribeIsDeniedWithoutAKey pins the decision to authenticate it.
//
// It lists the apps and names the registry and gateway, which is this
// deployment's structure rather than a client-facing fact — and /api/v1/config
// already serves the part that is safe to serve openly.
func TestDescribeIsDeniedWithoutAKey(t *testing.T) {
	srv, _ := newTieredServer(t)

	rec := doRequestNoKey(t, srv.Handler(), http.MethodGet, "/api/v1/describe")
	if rec.Code == http.StatusOK {
		t.Fatalf("GET /api/v1/describe answered %d without a key; it lists every app", rec.Code)
	}
}

// TestDescribeNarrowsToAnAppKey asserts an app key sees its own app and learns
// that is what it is, rather than being told it is an admin of a platform it
// cannot see.
func TestDescribeNarrowsToAnAppKey(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()
	shopKey := createAppWithKey(t, h, "shop")
	createAppWithKey(t, h, "blog")

	rec := withKey(t, h, http.MethodGet, "/api/v1/describe", shopKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("describe with an app key = %d (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeData(t, rec, &body)

	apps, ok := body["apps"].([]any)
	if !ok {
		t.Fatalf("apps is %T, want a list", body["apps"])
	}
	if len(apps) != 1 {
		t.Fatalf("an app key sees %d apps, want exactly its own: %v", len(apps), apps)
	}
	if apps[0].(map[string]any)["id"] != "shop" {
		t.Errorf("the app key sees %v, want shop", apps[0])
	}

	access := section(t, body, "access")
	if access["tier"] != "app" {
		t.Errorf("access.tier = %v, want app", access["tier"])
	}
	if access["app"] != "shop" {
		t.Errorf("access.app = %v, want the app the key is scoped to", access["app"])
	}
}
