package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestConsoleMountDoesNotConflict asserts the console at the root and the JSON
// fallback for /api can coexist — a duplicate ServeMux pattern panics at
// registration, which would take the whole server down on start.
func TestConsoleMountCoexistsWithAPI(t *testing.T) {
	srv, _ := newTestServer(t)

	page := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>console</html>"))
	})
	srv.WithConsole(page)

	// Constructing the handler is what would panic.
	h := srv.Handler()

	// An API route still reaches the API.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/api/v1/config = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "api_version") {
		t.Errorf("the API route did not reach the API: %s", rec.Body.String())
	}

	// An unknown API path is a JSON error, not the console page — otherwise a
	// client that mistyped an endpoint would parse HTML.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/nonexistent", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "no route matching") {
		t.Errorf("an unknown API path did not produce a JSON error: %s", rec.Body.String())
	}

	// A deep link reaches the console.
	req = httptest.NewRequest(http.MethodGet, "/apps/shop", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "console") {
		t.Errorf("a deep link did not reach the console: %s", rec.Body.String())
	}
}

// TestHandlerConstructionNeverPanics is a regression test for a startup crash.
//
// The console mounts at the root, and the JSON fallback for /api and /git is
// registered alongside it. Registering a pattern another handler already claimed
// makes ServeMux panic — and because the collision depended on whether git was
// attached, the first version of this passed a test that had no git transport and
// then crashed the real binary on boot.
//
// Construction is the whole test: a duplicate pattern panics here rather than in
// production.
func TestHandlerConstructionNeverPanics(t *testing.T) {
	srv, _ := newTestServer(t)

	console := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>console</html>"))
	})
	git := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("git"))
	})

	// Every combination of the optional root-level handlers.
	for _, withConsole := range []bool{true, false} {
		for _, withGit := range []bool{true, false} {
			name := "no console, no git"
			switch {
			case withConsole && withGit:
				name = "console and git"
			case withConsole:
				name = "console only"
			case withGit:
				name = "git only"
			}

			t.Run(name, func(t *testing.T) {
				s := *srv
				if withConsole {
					s.WithConsole(console)
				}
				if withGit {
					s.WithGit(git)
				}

				// A panic here is the failure being guarded against.
				h := s.Handler()

				// And it must actually serve something, not merely construct.
				req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("/api/v1/config = %d, want 200", rec.Code)
				}
			})
		}
	}
}
