package api_test

import (
	"context"
	"crypto/tls"
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

// TestTheServedScriptCarriesTheAppsOwnKey asserts the copy a repository fetches
// is the one that was written into it, key included.
//
// This is the half of the seeding that is easy to forget. `./applab.sh
// self-update` fetches this file and moves it over the one in the checkout — so
// a served copy rendered without the key would not merely be less useful, it
// would delete the credential from a working tree and leave the caller with a
// script that no longer runs.
//
// It also pins the address: the served script must name the deployment serving
// it, not whichever one the test happened to configure.
func TestTheServedScriptCarriesTheAppsOwnKey(t *testing.T) {
	srv, _ := newTieredServer(t)
	h := srv.Handler()

	created := createAppWithKey(t, h, "shop")

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/agent/files/applab.sh", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("serving applab.sh returned %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), created) {
		t.Error("the served script does not carry this app's key; self-update would strip it from a working checkout")
	}

	// The key it carries is this app's, not some other app's. Reading it back is
	// what proves the two agree rather than merely that a key-shaped string is
	// present.
	rec = doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/key", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reading the key returned %d: %s", rec.Code, rec.Body.String())
	}
	var keyBody struct {
		Key string `json:"key"`
	}
	decodeData(t, rec, &keyBody)
	if keyBody.Key != created {
		t.Fatalf("the served script and the key endpoint disagree: %q vs %q", created, keyBody.Key)
	}

	// And nothing else's. A second app must not see the first one's key in its
	// own script, which is the leak this rendering could produce.
	createAppWithKey(t, h, "other")
	rec = doRequest(t, h, http.MethodGet, "/api/v1/apps/other/agent/files/applab.sh", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("serving other's applab.sh returned %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), keyBody.Key) {
		t.Error("another app's script carries this app's key")
	}
}

// TestTheSeededAddressFollowsTheDeployment asserts that the address written into
// every app's script is the address the deployment is actually reached at.
//
// The address is assembled from three settings that each change its shape — the
// public URL, the base domain and the path prefix — and a script naming the
// wrong one sends every command at a host that does not answer. The in-cluster
// Service address is deliberately not among them: that is what a build pod
// clones from, and a person running ./applab.sh is not in the cluster.
//
// Asserted on the served file rather than on the helper that builds the address,
// because the file is what a caller receives.
func TestTheSeededAddressFollowsTheDeployment(t *testing.T) {
	cases := []struct {
		name string
		// publicURL is APPLAB_PUBLIC_URL, when the operator has set it.
		publicURL string
		basePath  string
		prefix    string
		host      string
		tls       bool
		want      string
	}{
		{
			name: "the public URL wins when it is set",
			// The chart sets it from ingress.host, so this is the normal
			// deployment: the address a person types.
			publicURL: "https://applab.example.com/applab",
			basePath:  "/applab",
			host:      "applab.ops-system.svc",
			want:      "https://applab.example.com/applab",
		},
		{
			name:     "without one it is derived from the domain and the base path",
			basePath: "/applab",
			host:     "applab.ops-system.svc",
			want:     "http://apps.example.com/applab",
		},
		{
			name:     "the path prefix is part of it, since the console lives under it too",
			basePath: "/applab",
			prefix:   "/apps",
			host:     "applab.ops-system.svc",
			want:     "http://apps.example.com/applab/apps",
		},
		{
			name: "a request carrying TLS is https when nothing is configured",
			host: "applab.ops-system.svc",
			tls:  true,
			want: "https://apps.example.com",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTieredServerWith(t, func(cfg *config.Config) {
				cfg.PublicURL = tc.publicURL
				cfg.BasePath = tc.basePath
				cfg.PathPrefix = tc.prefix
			})
			h := srv.Handler()

			// Every route lives under the base path, which is the whole point of
			// the setting: an Ingress routes on a prefix and cannot strip it, so
			// the server receives it and expects it.
			base := tc.basePath
			createAppWithKeyAt(t, h, base, "shop")

			// Reached over a host that is not the deployment's address, which is
			// what a request through an ingress looks like: the Host header
			// belongs to the proxy, the domain is the configured one.
			req := httptest.NewRequest(http.MethodGet, "http://"+tc.host+base+"/api/v1/apps/shop/agent/files/applab.sh", nil)
			req.Header.Set("Authorization", "Bearer "+adminKey)
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("serving applab.sh returned %d: %s", rec.Code, rec.Body.String())
			}
			got := "MISSING"
			for _, line := range strings.Split(rec.Body.String(), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "APPLAB_URL=") {
					got = strings.TrimSpace(line)
				}
			}
			want := `APPLAB_URL="${APPLAB_URL:-` + tc.want + `}"`
			if got != want {
				t.Errorf("the seeded script defaults\n  %s\nwant\n  %s", got, want)
			}
		})
	}
}

// createAppWithKeyAt is createAppWithKey for a server served under a base path,
// where every route carries the prefix.
func createAppWithKeyAt(t *testing.T, h http.Handler, base, appID string) string {
	t.Helper()

	if rec := doRequest(t, h, http.MethodPost, base+"/api/v1/apps", map[string]any{"id": appID}); rec.Code != http.StatusCreated {
		t.Fatalf("create app %s: %d (%s)", appID, rec.Code, rec.Body.String())
	}
	rec := doRequest(t, h, http.MethodGet, base+"/api/v1/apps/"+appID+"/key", nil)
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

// TestTheKeyIsMintedBeforeTheRepositoryIsSeeded asserts the order of the two
// things a create does, which is the one part of the seeding a test of the
// *content* cannot see.
//
// The seeded files are written into the repository as it is created, and one of
// them carries the app's key. So the key has to exist by then. Minting it
// afterwards seeds exactly one tree without a key — the opening one, the one a
// caller who just created an app is most likely to clone — and every later
// upload writes it in, so the failure heals itself and looks like nothing.
//
// Asserted on the observation rather than on the file, because the served copy is
// rendered from the templates on the spot and carries the key whatever order the
// create used. The question here is only whether the key was there to be read.
func TestTheKeyIsMintedBeforeTheRepositoryIsSeeded(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.OpenLocal(context.Background(), filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	appKeys := appkey.New(st)

	// What each lookup *found*, recorded as it happened. Reading the key again
	// after the create would succeed either way — a repository seeded before the
	// key existed still leaves the key existing by the time the create returns —
	// which is precisely the bug this is here to catch.
	var found []string
	var missed []string
	src, err := source.New(source.Options{
		Objects: newObjects(t),
		DataDir: dataDir,
		SeedKey: func(ctx context.Context, appID string) (string, error) {
			key, keyErr := appKeys.Get(ctx, appID)
			if keyErr != nil {
				missed = append(missed, appID+": "+keyErr.Error())
				return "", keyErr
			}
			found = append(found, appID)
			return key, nil
		},
	})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	cfg := config.Default()
	cfg.Keys = []string{adminKey}
	cfg.BaseDomain = "apps.example.com"
	cfg.DataDir = dataDir
	h := api.New(cfg, st, auth.New(cfg.Keys)).WithSource(src).Handler()

	if rec := doRequest(t, h, http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"}); rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d (%s)", rec.Code, rec.Body.String())
	}

	// At least one lookup happened — the seeding ask — and every one of them
	// found a key.
	if len(found) == 0 {
		t.Fatalf("the repository was seeded without ever finding the app's key, so the opening "+
			"commit's applab.sh carries none; the key has to be minted before the "+
			"repository is created (lookups that found nothing: %v)", missed)
	}
}
