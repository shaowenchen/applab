package api

import (
	"net/http"
	"runtime"
	"time"

	"github.com/shaowenchen/applab/internal/buildinfo"
)

// handleHealth reports liveness.
//
// It deliberately does not check the database or the cluster. A liveness probe
// restarts the process when it fails, and restarting applab cannot fix an
// unreachable API server or a locked database file — it would only turn a
// degraded control plane into a crash loop, taking down the builds it was
// partway through. Readiness is where dependency checks belong.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

// configResponse is what GET /api/v1/config returns.
//
// It exists so a caller can discover this deployment's shape instead of
// assuming it: the hostname apps are served under, how large an upload may be,
// and whether the build and deploy halves are wired up at all are all
// deployment-specific, and a client that guesses wrong produces confusing
// failures rather than clear ones.
type configResponse struct {
	APIVersion string `json:"api_version"`
	Version    string `json:"version"`

	// APIBaseURL is the base a client should build links from. It may have been
	// derived from the request's Host header, so it reflects the address the
	// caller actually reached this service at.
	APIBaseURL string `json:"api_base_url"`

	// BaseDomain is the domain apps are exposed under, and DomainTemplate shows
	// how an app id becomes a hostname.
	BaseDomain     string `json:"base_domain"`
	DomainTemplate string `json:"domain_template"`

	// NamespacePrefix is prepended to an app id to name that app's namespace.
	NamespacePrefix string `json:"namespace_prefix"`

	// Upload limits, so a client can size its chunks instead of discovering the
	// limit by being rejected.
	MaxSimpleUpload int64 `json:"max_simple_upload"`
	ChunkSize       int64 `json:"chunk_size"`
	MaxChunkBytes   int64 `json:"max_chunk_bytes"`

	// Capabilities reports which halves of the pipeline this deployment has.
	// A deployment can run without a cluster (useful for developing the API
	// itself), and a client should be able to tell before it tries.
	Capabilities map[string]bool `json:"capabilities"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, configResponse{
		APIVersion:      APIVersion,
		Version:         buildinfo.Version,
		APIBaseURL:      s.baseURL(r),
		BaseDomain:      s.cfg.BaseDomain,
		DomainTemplate:  "*." + orPlaceholder(s.cfg.BaseDomain),
		NamespacePrefix: s.cfg.NamespacePrefix,
		MaxSimpleUpload: s.cfg.MaxSimpleUpload,
		ChunkSize:       s.cfg.ChunkSize,
		MaxChunkBytes:   s.cfg.MaxChunkBytes,
		Capabilities: map[string]bool{
			"build":  s.build != nil && s.build.Ready(),
			"deploy": s.deploy != nil && s.deploy.Ready(),
			"source": s.initSource != nil,
		},
	})
}

// orPlaceholder gives a value something honest to show when it is unset, rather
// than an empty string that reads like a bug.
func orPlaceholder(v string) string {
	if v == "" {
		return "<base domain not configured>"
	}
	return v
}

// baseURL returns the address callers should use for this deployment.
//
// An explicitly configured base wins. Otherwise it is derived from the request,
// which is right behind an ingress and wrong the moment a proxy rewrites the
// Host header — so the configured form is the safer deployment, and this
// fallback exists so a development instance works with no configuration.
func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		return s.cfg.BaseURL
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// versionResponse is what GET /api/v1/version returns.
type versionResponse struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, versionResponse{
		Version:   buildinfo.Version,
		Commit:    buildinfo.Commit,
		BuildTime: buildinfo.BuildTime,
		GoVersion: runtime.Version(),
	})
}

// handleLlmsTxt serves the agent-facing contract.
//
// It is rendered per request rather than served as a static file so that the
// endpoint list is always the one the running server actually has: a route
// added without touching a data file cannot end up undocumented here.
func (s *Server) handleLlmsTxt(w http.ResponseWriter, r *http.Request) {
	doc, err := RenderLlmsTxt(s)
	if err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "render llms.txt").Wrap(err))
		return
	}
	writeText(w, http.StatusOK, "text/plain", doc)
}
