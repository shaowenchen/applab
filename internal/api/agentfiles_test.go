package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestTheAgentFilesAreServable asserts the files AppLab writes into an app's
// tree can be fetched back from the running server.
//
// That is what lets a copy in a repository update itself: the seeded files are
// committed on every upload, so a checkout carries whatever version was current
// the last time it was pushed, and AppLab's API changes between releases. Serving
// them from the deployment is the difference between a stale script and one that
// can be refreshed.
func TestTheAgentFilesAreServable(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()
	createAppWithKey(t, h, "shop")

	// The listing names them and gives each a URL.
	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/agent/files", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("the listing returned %d: %s", rec.Code, rec.Body.String())
	}
	var listing struct {
		Files []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"files"`
	}
	decodeData(t, rec, &listing)
	if len(listing.Files) == 0 {
		t.Fatal("the listing is empty; there is nothing for a repository to fetch")
	}

	for _, f := range listing.Files {
		if f.Name == "" || f.URL == "" {
			t.Errorf("an entry is missing its name or url: %+v", f)
			continue
		}
		rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/agent/files/"+f.Name, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s returned %d: %s", f.Name, rec.Code, rec.Body.String())
			continue
		}
		// A file, not an error body or an HTML page: whatever replaces a local
		// copy has to be runnable.
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("%s is served as %q, want text/plain", f.Name, ct)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s came back empty", f.Name)
		}
	}
}

// TestTheAgentFilesNameTheApp asserts the served content is the app's own copy.
//
// The files carry the app's id in the commands they show, so a version rendered
// for the wrong app would hand someone a script that drives someone else's app —
// and would look entirely plausible while doing it.
func TestTheAgentFilesNameTheApp(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()
	createAppWithKey(t, h, "shop")

	for _, name := range []string{"applab.sh", "AGENT.md"} {
		rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/agent/files/"+name, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", name, rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, "{{APP}}") {
			t.Errorf("%s still has its placeholder in it", name)
		}
		if !strings.Contains(body, "shop") {
			t.Errorf("%s does not name the app it is for", name)
		}
	}
}

// TestAnUnknownAgentFileIsNotFound asserts the name is not a path.
//
// It is a path segment from the URL, so the lookup has to be a lookup rather
// than a join — a traversal or an arbitrary filename would otherwise be readable
// through this route.
func TestAnUnknownAgentFileIsNotFound(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()
	createAppWithKey(t, h, "shop")

	for _, name := range []string{"nope.txt", "..", "../../../etc/passwd"} {
		rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/agent/files/"+name, nil)
		if rec.Code == http.StatusOK {
			t.Errorf("%q was served; only the seeded files may be", name)
		}
	}
}
