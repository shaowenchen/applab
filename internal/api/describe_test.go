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
// It is not a contract test — the endpoint list in api.endpoints is what says
// which routes exist and what they take. This asserts the things that would make
// the endpoint useless if missing: where this is, what it can do, what the key
// reaches, and what already exists.
func TestDescribeOrientsAnAgent(t *testing.T) {
	srv, _ := newTieredServer(t)
	body := describeFor(t, srv)

	// A one-line answer to "what is this", so an agent can report it without
	// assembling a sentence from six fields.
	summary, _ := body["summary"].(string)
	// The product's own name, spelled the way the product spells it.
	for _, want := range []string{"AppLab", "namespace", "app"} {
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
	if app["url"] == nil {
		t.Errorf("apps[0] carries no url: %v", app)
	}
}

// TestDescribeServesAnonymousButWithholdsTheDeployment pins the decision to make
// this the front door.
//
// It has to answer a caller that holds no key, because a caller cannot
// authenticate to a service it knows nothing about — that is the whole reason
// /api/v1/config is open, and this is the same argument with the endpoint list
// attached. What it must not do is hand an anonymous caller the deployment's
// structure: the apps, the namespace, the registry and the gateway are someone's
// infrastructure, and a request that presented no credential has given no reason
// to disclose them.
func TestDescribeServesAnonymousButWithholdsTheDeployment(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()
	createAppWithKey(t, h, "shop")

	rec := doRequestNoKey(t, h, http.MethodGet, "/api/v1/describe")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/describe = %d without a key, want 200: it is how a caller learns what this service is", rec.Code)
	}
	var body map[string]any
	decodeData(t, rec, &body)

	// The endpoint list is the point, and it is public: it describes the service,
	// not what is deployed on it.
	apiSection := section(t, body, "api")
	endpoints, ok := apiSection["endpoints"].([]any)
	if !ok || len(endpoints) < 5 {
		t.Fatalf("api.endpoints = %v, want the full endpoint list", apiSection["endpoints"])
	}
	// Every entry has to say what credential it needs, because the tiers are not
	// interchangeable and a wrong guess is a 403 an agent cannot explain.
	first, ok := endpoints[0].(map[string]any)
	if !ok {
		t.Fatalf("api.endpoints[0] is %T, want an object", endpoints[0])
	}
	for _, field := range []string{"method", "path", "key", "doc"} {
		if first[field] == nil {
			t.Errorf("api.endpoints[0].%s is absent: %v", field, first)
		}
	}

	// The apps are not, and neither is the wiring.
	if apps, ok := body["apps"].([]any); !ok || len(apps) != 0 {
		t.Errorf("an anonymous caller was given %v; it must see no apps", body["apps"])
	}
	if registry := section(t, body, "build")["registry"]; registry != nil && registry != "" {
		t.Errorf("build.registry = %v; the build registry is not disclosed without a key", registry)
	}
	if gateway := section(t, body, "deploy")["gateway"]; gateway != nil && gateway != "" {
		t.Errorf("deploy.gateway = %v; the gateway is not disclosed without a key", gateway)
	}
	if ns := section(t, body, "deployment")["namespace"]; ns != nil && ns != "" {
		t.Errorf("deployment.namespace = %v; the namespace is not disclosed without a key", ns)
	}

	// And it says so. An agent told "nothing here" when the truth is "you have
	// not shown me that yet" concludes the deployment cannot build.
	if access := section(t, body, "access"); access["tier"] != "none" {
		t.Errorf("access.tier = %v, want none", access["tier"])
	}
	summary, _ := body["summary"].(string)
	if !strings.Contains(summary, "key") {
		t.Errorf("summary %q does not say that a key unlocks more", summary)
	}
	// The summary is a sentence built out of the same fields, so it is the place
	// a redaction gets left behind: clearing deployment.namespace while the
	// prose still reads "in namespace ops-system" is one leak with two surfaces.
	if strings.Contains(summary, "in namespace") {
		t.Errorf("summary %q names the namespace a key would have unlocked", summary)
	}
	// And it must not claim the platform is empty when the truth is that the
	// list was withheld.
	if strings.Contains(summary, "0 apps") {
		t.Errorf("summary %q reports an app count to a caller shown no apps", summary)
	}

	// The calls it cannot make are not offered. An agent handed a command that
	// returns 401 reports a broken deployment.
	httpCalls := section(t, section(t, body, "how_to"), "http")
	if httpCalls["upload"] != nil {
		t.Errorf("how_to.http.upload is offered to a caller with no key: %v", httpCalls["upload"])
	}
	if httpCalls["describe"] == nil {
		t.Error("how_to.http.describe is absent; it is the one call this caller can make")
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
