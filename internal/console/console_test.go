package console

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestConsoleRendering asserts the console's own JavaScript renders an app's
// address correctly, by running it.
//
// The console shows an app's address in two places, and with a shared path
// prefix the host belongs to the deployment rather than the app — so a version
// that showed the host alone labelled every row identically. That is a display
// bug no Go test can see: the API was right, the page was wrong, and nothing in
// `make check` rendered the page at all.
//
// The script is executed rather than its text matched. A regex for
// "app.hostname + app.path" would pass on code that computes an address and then
// never uses it, which is precisely the mistake worth catching.
//
// Skipped when node is absent, because the console is a static file with no
// build step and requiring a JavaScript runtime to build the server would undo
// that — but never skipped in CI.
//
// A silent skip in CI is the one outcome worth refusing: it would look exactly
// like the check having run, and the bug this test exists to catch would ship
// with a green tick beside it. So a missing node is a failure there and a skip
// only on a developer's machine.
func TestConsoleRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("node is not installed and this is CI: the console rendering "+
				"checks would be skipped silently. Install node in the workflow (%v)", err)
		}
		t.Skip("node is not installed; skipping the console rendering checks")
	}

	script, err := filepath.Abs(filepath.Join("render_test.js"))
	if err != nil {
		t.Fatalf("resolve the test script: %v", err)
	}

	cmd := exec.Command(node, script)
	cmd.Dir = filepath.Dir(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the console does not render correctly (node %s):\n%s", runtime.Version(), out)
	}
	t.Logf("%s", out)
}

// TestThePageCarriesTheDeploymentBasePath covers the tag the console reads its
// API base from.
//
// The page is served for every address that is not a file, so a console opened
// at an app's own address — https://host/applab/apps/shop — cannot work out
// where the deployment is from where it is. It used to try: it built every API
// call from its own location, asking /applab/apps/shop/api/v1/apps. That is not
// a route and not an asset, so the server answered with the page itself, and
// signing in failed with "Could not connect: 200 OK: <!doctype html>".
//
// What must hold is that the page states the deployment's path and not the
// address it was fetched at, and that the root is stated as empty rather than
// omitted — the console has to be able to tell "served at the root" from "not
// served by applab", and an absent tag is the second.
func TestThePageCarriesTheDeploymentBasePath(t *testing.T) {
	for _, tc := range []struct {
		name     string
		basePath string
		want     string
	}{
		{name: "under a path", basePath: "/applab", want: `<meta name="applab-base-path" content="/applab">`},
		{name: "at the root", basePath: "", want: `<meta name="applab-base-path" content="">`},
		// Normalized rather than echoed: the server trims a trailing slash and
		// config normalizes "/" to empty, so a handler that wrote through
		// whatever it was given would put a second answer in the page.
		{name: "with a trailing slash, trimmed", basePath: "/applab/", want: `<meta name="applab-base-path" content="/applab">`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := New(tc.basePath)
			if err != nil {
				t.Fatalf("build the console handler: %v", err)
			}

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			body := rec.Body.String()
			if rec.Code != http.StatusOK {
				t.Fatalf("the console answered %d, want 200", rec.Code)
			}
			if !strings.Contains(body, tc.want) {
				t.Errorf("the page does not carry %s; a console that cannot read the deployment's path builds every API call from its own address, which is a deep link rather than the deployment", tc.want)
			}

			// One tag. A page carrying two would let the console read whichever
			// it found first, which is the ambiguity the tag exists to remove.
			//
			// Counted as the whole tag rather than by name, because the script
			// that reads it names it too — matching on the name alone would
			// count the reader as a second tag and fail on a correct page.
			if n := strings.Count(body, `<meta name="applab-base-path"`); n != 1 {
				t.Errorf("the page carries %d base-path meta tags, want 1", n)
			}

			// And the page is otherwise intact — a deep link serves the same
			// bytes, so the injection must not have replaced the script.
			if !strings.Contains(body, "<script>") {
				t.Error("the served page has no script block; the console is gone")
			}
		})
	}
}

// TestADeepLinkServesThePageWithTheDeploymentPath asserts the failing case
// directly: the address a person reads an app at is not the deployment's, and
// the page served there must still name the deployment.
func TestADeepLinkServesThePageWithTheDeploymentPath(t *testing.T) {
	h, err := New("/applab")
	if err != nil {
		t.Fatalf("build the console handler: %v", err)
	}

	// The route form the console itself was served under when this broke.
	req := httptest.NewRequest(http.MethodGet, "/apps/shop", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("a deep link answered %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<meta name="applab-base-path" content="/applab">`) {
		t.Error("a deep link does not carry the deployment's base path, so a console opened there would ask its own address for the API")
	}
	// Written out by hand here rather than through the file server, so the
	// content type has to be set as well — without it a browser reads the page
	// as plain text and shows the source.
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("a deep link was served as %q, want text/html", ct)
	}
}

// TestAPageWithoutTheAnchorIsStillServed asserts the failure mode chosen when
// the page and this code disagree.
//
// The anchor is a tag in a file this package does not own. If it is renamed,
// the console must still be served — it is the whole of what a deployment shows
// a person, and losing it to a missing tag would be a worse answer than losing
// the fact the tag carried. TestThePageCarriesTheDeploymentBasePath is what
// keeps the two in step.
func TestAPageWithoutTheAnchorIsStillServed(t *testing.T) {
	// A head with no </title> at all, which is the shape a renamed anchor
	// produces: the injection point is simply not there.
	page := []byte("<html><head><meta charset=\"utf-8\"></head><body>hi</body></html>")
	got := withBasePath(page, "/applab")
	if string(got) != string(page) {
		t.Errorf("a page with no anchor was modified:\n got: %s\nwant: %s", got, page)
	}
	if !strings.Contains(string(got), "hi") {
		t.Error("the page lost its body")
	}
}
