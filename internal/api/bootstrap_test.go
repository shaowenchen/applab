package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestTheBootstrapScriptNeedsNoKey is the point of the route.
//
// The only other place this file is served is app-scoped and authenticated, so
// someone who has no app and no key had no way to get the tool — which is
// exactly the position a new user is in.
func TestTheBootstrapScriptNeedsNoKey(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	rec := doRequestNoKey(t, h, http.MethodGet, "/bootstrap/applab.sh")
	if rec.Code != http.StatusOK {
		t.Fatalf("the bootstrap script answered %d without a key, want 200", rec.Code)
	}
	if !strings.HasPrefix(rec.Body.String(), "#!/bin/sh") {
		t.Errorf("the response is not a shell script: %q", rec.Body.String()[:min(60, rec.Body.Len())])
	}
}

// TestTheBootstrapScriptCarriesNothingToLeak is the security half, and it is the
// reason this route can be unauthenticated at all.
//
// It is the same template as an app's copy, so the risk is mechanical: render it
// with values that were not meant for it — an app's id and key — and an
// unauthenticated route starts handing out working credentials. The assertions
// are on the rendered bytes rather than on the renderer's arguments, because
// what matters is what leaves the server.
func TestTheBootstrapScriptCarriesNothingToLeak(t *testing.T) {
	srv, st := newTestServer(t)
	h := srv.Handler()

	// An app with a key, so there is something real to leak if the rendering is
	// wrong. Without one the test would pass on an empty string.
	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	decodeData(t, rec, &created)
	key, _ := created["app_key"].(string)
	if key == "" {
		t.Fatal("no app_key on the create response, so this test cannot mean anything")
	}
	if app, err := st.GetApp(t.Context(), "shop"); err != nil || app == nil {
		t.Fatalf("read the app back: %v", err)
	}

	rec = doRequestNoKey(t, h, http.MethodGet, "/bootstrap/applab.sh")
	body := rec.Body.String()

	if strings.Contains(body, key) {
		t.Error("the bootstrap script contains an app's key; that route is unauthenticated")
	}
	// The app assignment is the other half: a bootstrap copy must not claim to be
	// about an app, or every command in it would act on one the reader never
	// chose.
	if strings.Contains(body, `APP="shop"`) {
		t.Error("the bootstrap script names an app; it has no app to name")
	}
	if !strings.Contains(body, `APP="${APPLAB_APP:-}"`) {
		t.Errorf("the app assignment is not the empty, overridable form")
	}
	// And it does point at the deployment, or it cannot reach anything.
	if !strings.Contains(body, "APPLAB_URL=") {
		t.Error("the bootstrap script does not carry the deployment's address")
	}
	// No placeholder survived, which would be a half-rendered file.
	if strings.Contains(body, "{{") {
		t.Error("the bootstrap script has an unsubstituted placeholder")
	}
}

// TestTheBootstrapScriptCanCreateAnApp asserts the fetched copy is actually
// usable for what it is for.
//
// The commands are checked as text because the script is shell: a renderer that
// dropped an arm, or a usage line that was never added, leaves a file that looks
// complete and has nothing to run.
func TestTheBootstrapScriptCanCreateAnApp(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	body := doRequestNoKey(t, h, http.MethodGet, "/bootstrap/applab.sh").Body.String()

	for _, want := range []struct{ name, text string }{
		{"the create command", "\n  create)"},
		{"the use command", "\n  use)"},
		{"the key guard", "require_key()"},
		{"the app guard", "require_app()"},
		{"a line about it in the usage", "create <app>"},
	} {
		if !strings.Contains(body, want.text) {
			t.Errorf("the bootstrap script is missing %s (%q)", want.name, want.text)
		}
	}

	// And it must NOT require a key at the top: create is the command someone
	// with no key runs, and a top-level guard makes the script unable to explain
	// itself to the person who needs it.
	if strings.Contains(body, `: "${APPLAB_KEY:?`) {
		t.Error("the bootstrap script requires a key before anything runs, so `create` cannot be reached")
	}
}

// TestTheBootstrapListingNamesTheScript covers the route a caller reaches with
// no idea what the file is called.
func TestTheBootstrapListingNamesTheScript(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	rec := doRequestNoKey(t, h, http.MethodGet, "/bootstrap")
	if rec.Code != http.StatusOK {
		t.Fatalf("the bootstrap listing answered %d without a key, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "applab.sh") {
		t.Errorf("the listing does not name the script: %s", body)
	}
	if !strings.Contains(body, "/bootstrap/applab.sh") {
		t.Errorf("the listing does not say where to fetch it: %s", body)
	}
}
