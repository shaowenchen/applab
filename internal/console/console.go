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
	"bytes"
	"embed"
	"html"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var staticFS embed.FS

// Where the deployment's path is written into the page as it is handed over.
//
// It is written here rather than kept in the page for the same reason the page
// is a single file: the path is a fact about the deployment, and the page's
// bytes are the same in every one. The console needs it and cannot work it out
// — see baseURL in the page's script for what going without it cost.
const (
	basePathMetaPrefix = `<meta name="applab-base-path" content="`
	basePathMetaSuffix = `">`
	basePathAnchor     = "</title>"
)

// Handler serves the console.
type Handler struct {
	files http.Handler
	page  []byte
}

// New creates a console handler for a deployment served under basePath.
//
// An empty basePath means the root, and it is written down as an empty value
// rather than left out: the page has to be able to tell "served at the root"
// from "not served by anything", and an empty attribute is how it does.
func New(basePath string) (*Handler, error) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	page, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return nil, err
	}
	return &Handler{
		files: http.FileServer(http.FS(sub)),
		page:  withBasePath(page, basePath),
	}, nil
}

// withBasePath writes the base path into the page's head.
//
// A page that no longer has the anchor is served unchanged rather than failing
// to serve at all. The console is the whole of what this deployment shows a
// person, and taking it away because a tag was renamed would be a worse answer
// than a page missing the one fact it could not have worked out for itself. The
// test beside this is what keeps the two in step.
func withBasePath(page []byte, basePath string) []byte {
	i := bytes.Index(page, []byte(basePathAnchor))
	if i < 0 {
		return page
	}
	i += len(basePathAnchor)

	base := strings.TrimSuffix(strings.TrimSpace(basePath), "/")
	meta := basePathMetaPrefix + html.EscapeString(base) + basePathMetaSuffix

	out := make([]byte, 0, len(page)+len(meta))
	out = append(out, page[:i]...)
	out = append(out, meta...)
	out = append(out, page[i:]...)
	return out
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

	// The page, for every address that is not a different file. A single-page
	// client: any path that is not a real asset serves the page, so a deep link
	// like /apps/shop does not 404 on a reload.
	//
	// It is written out rather than handed to the file server because it is the
	// one response carrying a fact the deployment alone knows — its base path.
	// Serving it through the file server would mean the bytes differ depending
	// on which of its addresses was used, and a deep link is served from an
	// address that is not the deployment's own.
	if h.isPage(r.URL.Path) {
		h.servePage(w, r)
		return
	}

	h.files.ServeHTTP(w, r)
}

// isPage reports whether a path is answered with the console page rather than
// with a file.
//
// The root is the page: it is the address a browser arrives at the console by.
// Everything else that is not an asset is a route the page renders itself, like
// /apps/shop.
//
// index.html is deliberately not included. The file server already answers it
// with a redirect to the directory it sits in, which is the address the page is
// served at and therefore the one carrying the right base path — redirecting is
// the correct answer, and serving the bytes directly here would skip it.
func (h *Handler) isPage(path string) bool {
	if path == "/" || path == "" {
		return true
	}
	return !h.isAsset(path)
}

// servePage writes the console page, with this deployment's base path in it.
func (h *Handler) servePage(w http.ResponseWriter, r *http.Request) {
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(h.page)
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
