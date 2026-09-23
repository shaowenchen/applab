package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/shaowenchen/applab/internal/auth"
	"github.com/shaowenchen/applab/internal/config"
	"github.com/shaowenchen/applab/internal/llms"
	"github.com/shaowenchen/applab/internal/source"
	"github.com/shaowenchen/applab/internal/store"
)

// APIVersion is the version of this HTTP API, reported by /api/v1/config so a
// client can assert it is talking to an interface it understands. It tracks the
// route surface, not the build version.
const APIVersion = "v1"

// Server holds the dependencies the HTTP layer needs.
//
// The pipeline halves are function fields rather than interfaces because each
// is a single operation at this layer: the API hands an app id across and the
// implementation owns everything else. Keeping them as functions means a
// deployment without source storage leaves them nil and the affected routes are
// simply absent, rather than present and failing at call time.
type Server struct {
	cfg   config.Config
	store *store.Store
	auth  *auth.Authenticator

	// initSource creates an app's source repository. Nil means this deployment
	// has no source storage.
	initSource func(ctx context.Context, appID string) error

	// sourceRemover deletes an app's source repository.
	sourceRemover func(ctx context.Context, appID string) error

	// sourceIngest turns an uploaded archive into a commit.
	sourceIngest func(ctx context.Context, appID string, archive io.Reader, message, parent string) (*source.IngestResult, error)

	// headCommit returns an app's current tip.
	headCommit func(ctx context.Context, appID string) (string, error)

	// sourceLog returns an app's commits in git's own order.
	sourceLog func(ctx context.Context, appID string, limit int) ([]source.CommitInfo, error)

	// resolveCommit expands a possibly-abbreviated commit revision.
	resolveCommit func(ctx context.Context, appID, revision string) (string, error)

	// namespaceDeleter tears down an app's namespace and everything in it.
	namespaceDeleter func(ctx context.Context, appID string) error

	// build and deploy report whether those halves are wired up, for
	// /api/v1/config to advertise.
	build  BuildEngine
	deploy Deployer

	// git is the handler serving repositories over the git smart HTTP protocol.
	// Nil means this deployment does not serve git.
	git http.Handler
}

// BuildEngine is the build half of the pipeline.
type BuildEngine interface {
	// Ready reports whether the engine can start builds right now.
	Ready() bool
}

// Deployer is the deploy half of the pipeline.
type Deployer interface {
	// Ready reports whether the deployer can reach the cluster.
	Ready() bool
}

// New builds a Server.
func New(cfg config.Config, st *store.Store, a *auth.Authenticator) *Server {
	return &Server{cfg: cfg, store: st, auth: a}
}

// WithSource attaches source storage.
//
// The operations are passed in as functions rather than as an interface because
// the API layer only ever hands an app id and some bytes across; the source
// package owns everything else. That keeps this package from having to grow a
// method for every source operation that later appears.
func (s *Server) WithSource(store *source.Store) *Server {
	s.initSource = store.Create
	s.sourceRemover = store.Remove
	s.headCommit = store.HeadCommit
	s.resolveCommit = store.ResolveCommit
	s.sourceLog = store.Log
	s.sourceIngest = func(ctx context.Context, appID string, archive io.Reader, message, parent string) (*source.IngestResult, error) {
		return store.Ingest(ctx, appID, archive, message, parent, source.DefaultIngestLimits)
	}
	return s
}

// WithGit attaches the handler that serves repositories over git's smart HTTP
// protocol, mounted at the path the clone URLs promise.
func (s *Server) WithGit(h http.Handler) *Server { s.git = h; return s }

// WithBuild attaches a build engine.
func (s *Server) WithBuild(b BuildEngine) *Server { s.build = b; return s }

// WithDeploy attaches a deployer.
func (s *Server) WithDeploy(d Deployer) *Server { s.deploy = d; return s }

// WithNamespaceDeleter attaches namespace teardown, used when deleting an app.
func (s *Server) WithNamespaceDeleter(fn func(ctx context.Context, appID string) error) *Server {
	s.namespaceDeleter = fn
	return s
}

// route is one endpoint: how it is matched, whether it is protected, and
// whether and how it is documented.
//
// Declaring all three in one place is what keeps the served API, the secured
// API and the documented API from drifting apart. The alternative — a mux
// configured in one file, prose in another — is how a route ends up reachable
// without authentication or documented but nonexistent.
type route struct {
	// Pattern is the Go 1.22 ServeMux pattern, method included, e.g.
	// "POST /api/v1/apps/{id}/source".
	Pattern string

	// Auth requires a configured API key. Every route that reads or changes
	// data sets it; only liveness and the self-description endpoints do not,
	// because a probe cannot hold a credential and a caller has to be able to
	// discover what this service is before it can authenticate to it.
	Auth bool

	// Doc describes the route in one line for llms.txt. Empty means the route
	// is deliberately undocumented — reserved for destructive maintenance
	// operations, which are not offered to agents that act on what they read.
	Doc string

	Handler http.HandlerFunc
}

// routes returns the full route table.
//
// The order is irrelevant to routing but this is the order the API reference in
// llms.txt is generated in, so it is grouped by resource rather than
// alphabetised.
func (s *Server) routes() []route {
	return []route{
		// -- Liveness and self-description -------------------------------
		{
			Pattern: "GET /health",
			Doc:     "Liveness. No key required — a Kubernetes probe cannot hold one.",
			Handler: s.handleHealth,
		},
		{
			Pattern: "GET /api/v1/config",
			Doc:     "This deployment's limits and capabilities. No key required, so it doubles as the cheapest reachability check.",
			Handler: s.handleConfig,
		},
		{
			Pattern: "GET /api/v1/version",
			Doc:     "Build version and commit.",
			Handler: s.handleVersion,
		},
		{
			// Unauthenticated for the same reason as /api/v1/config: this file
			// is how a caller learns the key is needed and how to present it.
			Pattern: "GET /llms.txt",
			Doc:     "This document.",
			Handler: s.handleLlmsTxt,
		},

		// -- Apps ---------------------------------------------------------
		{
			Pattern: "GET /api/v1/apps",
			Auth:    true,
			Doc:     "List apps. `?include_deleted=true` also returns apps that were deleted but whose id is still reserved.",
			Handler: s.handleListApps,
		},
		{
			Pattern: "POST /api/v1/apps",
			Auth:    true,
			Doc:     "Create an app. Body: `{id, name?, port?, replicas?, dockerfile?, domain?}`.",
			Handler: s.handleCreateApp,
		},
		{
			Pattern: "GET /api/v1/apps/{app}",
			Auth:    true,
			Doc:     "One app.",
			Handler: s.handleGetApp,
		},
		{
			Pattern: "PATCH /api/v1/apps/{app}",
			Auth:    true,
			Doc:     "Change an app's settings: `{name?, port?, replicas?, dockerfile?, domain?}`. Fields omitted are left alone.",
			Handler: s.handleUpdateApp,
		},
		{
			Pattern: "DELETE /api/v1/apps/{app}",
			Auth:    true,
			Doc:     "Delete the app and everything applab recorded for it. `?keep_source=true` retains the git repository.",
			Handler: s.handleDeleteApp,
		},

		// -- Source -------------------------------------------------------
		{
			Pattern: "POST /api/v1/apps/{app}/source",
			Auth:    true,
			Doc:     "Upload source as a tar or tar.gz and commit it. This is the main way to push code. Send the archive as the raw request body (`--data-binary @-`), or as `multipart/form-data` with a `file` field. `?message=` sets the commit message; `?parent=<sha>` commits onto a chosen commit instead of the current tip. A single wrapping directory is stripped, so `tar czf - myproject` lands with its contents at the root.",
			Handler: s.handleUploadSource,
		},
		{
			Pattern: "GET /api/v1/apps/{app}/commits",
			Auth:    true,
			Doc:     "An app's commit history, newest first, with the current tip. `?limit=` (default 50).",
			Handler: s.handleListCommits,
		},
		{
			Pattern: "GET /api/v1/apps/{app}/commits/{sha}",
			Auth:    true,
			Doc:     "One commit. `{sha}` may be an abbreviated id.",
			Handler: s.handleGetCommit,
		},

		// -- Chunked source upload ----------------------------------------
		{
			Pattern: "POST /api/v1/apps/{app}/source/uploads",
			Auth:    true,
			Doc:     "Begin a chunked upload for source too large for one request. Body: `{total, chunk_size, message?}`. Returns an `upload_id`.",
			Handler: s.handleChunkedUploadStart,
		},
		{
			Pattern: "PUT /api/v1/apps/{app}/source/uploads/{upload}/parts/{index}",
			Auth:    true,
			Doc:     "Send one part. Body is the raw bytes. `{index}` is 1-based. Parts may be sent in any order and retried.",
			Handler: s.handleChunkedUploadPart,
		},
		{
			Pattern: "POST /api/v1/apps/{app}/source/uploads/{upload}/complete",
			Auth:    true,
			Doc:     "Assemble every part and commit the result. Fails if any part is missing.",
			Handler: s.handleChunkedUploadComplete,
		},
	}
}

// RouteReference renders the API reference block for llms.txt.
//
// It is generated from the same table that configures the mux, so a route
// cannot be documented without existing or exist without being documented —
// a test asserts the committed llms.txt contains exactly this text.
func (s *Server) RouteReference() string {
	var b strings.Builder
	for _, r := range s.routes() {
		if r.Doc == "" {
			continue
		}
		method, path, _ := strings.Cut(r.Pattern, " ")
		key := "no key"
		if r.Auth {
			key = "key"
		}
		b.WriteString("- `" + method + " " + path + "` — " + r.Doc + " _(" + key + ")_\n")
	}
	return b.String()
}

// DocumentedRouteCount reports how many routes appear in the API reference. It
// exists so a test can assert the reference is non-trivial — an empty generator
// would otherwise make the consistency check pass vacuously.
func (s *Server) DocumentedRouteCount() int {
	n := 0
	for _, r := range s.routes() {
		if r.Doc != "" {
			n++
		}
	}
	return n
}

// PatternRequiresAuth reports whether the route matching pattern is protected.
//
// It exposes the route table's own declaration rather than re-deriving the
// answer, so a test checking that data routes are protected cannot disagree with
// what the mux was actually configured with. An unknown pattern reports true:
// the safe answer for a route nobody can find is "assume it needs a key".
func (s *Server) PatternRequiresAuth(pattern string) bool {
	for _, r := range s.routes() {
		if r.Pattern == pattern {
			return r.Auth
		}
	}
	return true
}

// RenderLlmsTxt produces the exact document the llms.txt endpoint serves.
//
// It exists so the consistency test and the generator command compare against
// the same bytes the server would send, rather than a reimplementation that
// could itself drift.
func RenderLlmsTxt(s *Server) (string, error) {
	doc := llms.Render(s.RouteReference())
	if strings.TrimSpace(doc) == "" {
		return "", fmt.Errorf("generated llms.txt is empty")
	}
	return doc, nil
}

// Handler builds the HTTP handler: the route table, wrapped in authentication
// per route and in request logging and panic recovery around everything.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	for _, r := range s.routes() {
		var h http.Handler = r.Handler
		if r.Auth {
			h = s.auth.Middleware(h)
		}
		mux.Handle(r.Pattern, h)
	}

	// The git endpoints carry binary pack data, not JSON, so they are mounted
	// ahead of the fallback below — and behind the same key check as everything
	// else, since a repository is not public.
	//
	// A single pattern covers every git request for every repository: the
	// transport reads the whole path and works out which repository and which
	// sub-operation (info/refs, git-upload-pack, git-receive-pack) it names.
	// Declaring them individually would mean enumerating a protocol surface that
	// git is free to extend.
	if s.git != nil {
		mux.Handle("/git/", http.StripPrefix("/git", s.auth.Middleware(s.git)))
	}

	// A request matching no route should read as "no such endpoint" rather than
	// falling through to a default page. ServeMux's own 404 is plain text; this
	// keeps every response from the API in the same JSON shape as the rest, so a
	// client parses one error format and not two.
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail(w, r, NotFound("no route matching %s %s", r.Method, r.URL.Path))
	}))

	return recoverPanic(logRequests(mux))
}

// statusRecorder captures the status code so the log line can report it.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	// A handler that writes without calling WriteHeader has implicitly sent a
	// 200; recording that here keeps the log honest about it.
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Flush forwards to the underlying writer when it supports flushing, which
// streaming responses (build and pod logs) need. Without this the wrapper would
// silently break Server-Sent Events: io.Copy would buffer and the client would
// see nothing until the handler returned.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logRequests records one line per request at debug level.
//
// Debug rather than info: a busy control plane is mostly health probes, and at
// info they would crowd out the events actually worth reading. Failures are
// logged separately at the point they occur, at a level that matches.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Probes run every few seconds for the life of the process; logging
		// them at all would be pure noise.
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		slog.DebugContext(r.Context(), "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"remote", r.RemoteAddr)
	})
}

// recoverPanic turns a panicking handler into a 500 rather than a dropped
// connection and a stack trace on stderr.
//
// A panic in one request must not take down a control plane that may be
// mid-build for other apps, so the process stays up and the failure is logged
// with enough context to find it.
func recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is the documented way for a handler to abort
			// a response deliberately; it is not a bug and must propagate.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			slog.ErrorContext(r.Context(), "handler panicked",
				"method", r.Method,
				"path", r.URL.Path,
				"panic", rec)
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error"})
		}()
		next.ServeHTTP(w, r)
	})
}

// SortedPatterns returns every registered pattern, sorted. Used by tests to
// assert the table is well-formed.
func (s *Server) SortedPatterns() []string {
	out := make([]string, 0, len(s.routes()))
	for _, r := range s.routes() {
		out = append(out, r.Pattern)
	}
	sort.Strings(out)
	return out
}
