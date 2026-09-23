// Package console serves the web console.
//
// The console is a client of the public API — every one of its requests is a call
// a person could make with curl — and it holds no privileged position. That is
// deliberate: it means the API is genuinely the product rather than an
// implementation detail behind a UI, and a gap in the API shows up immediately as
// something the console cannot do rather than as a hidden internal endpoint.
//
// The page is embedded in the binary and written in plain HTML and JavaScript
// with no build step. There is nothing here worth a bundler: it is a list, a
// detail view and a log viewer, and adding a toolchain to produce that would mean
// the console's build had to be understood by anyone changing the server.
package console

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var staticFS embed.FS

// Handler serves the console.
type Handler struct {
	files http.Handler
}

// New creates a console handler.
func New() (*Handler, error) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	return &Handler{files: http.FileServer(http.FS(sub))}, nil
}

// ServeHTTP serves the console.
//
// Only GET and HEAD are accepted. The console is entirely static — everything it
// does happens through API calls the browser makes itself — so there is nothing
// for another method to do, and accepting one would invite a form post that
// silently does nothing.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "the console is static; use the API for anything else", http.StatusMethodNotAllowed)
		return
	}

	// The console holds no credentials itself: the key is kept in the browser's
	// storage and sent to the API directly. This header is what stops a browser
	// from treating a stored response as reusable across a key change.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// A single-page client: any path that is not a real file serves the page, so
	// a deep link like /apps/shop does not 404 on a reload.
	if h.isAsset(r.URL.Path) {
		h.files.ServeHTTP(w, r)
		return
	}

	r = r.Clone(r.Context())
	r.URL.Path = "/"

	// A small CSP: everything is inline in one file by design, so script-src
	// cannot be 'self'-only, but nothing may be loaded from anywhere else and
	// the API is reachable only at this origin.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; "+
			"script-src 'unsafe-inline'; "+
			"style-src 'unsafe-inline'; "+
			"connect-src 'self'; "+
			"img-src 'self' data:; "+
			"form-action 'none'; "+
			"base-uri 'none'; "+
			"frame-ancestors 'none'")

	h.files.ServeHTTP(w, r)
}

// isAsset reports whether a path names a real file rather than a route.
func (h *Handler) isAsset(path string) bool {
	if path == "/" || path == "" {
		return true
	}
	for _, suffix := range []string{".css", ".js", ".svg", ".png", ".ico", ".woff2", ".html", ".txt"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}
