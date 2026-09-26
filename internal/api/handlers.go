package api

import (
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/shaowenchen/applab/internal/buildinfo"
)

// handleHealth reports liveness.
//
// It deliberately does not check the database or the cluster. A liveness probe
// restarts the process when it fails, and restarting AppLab cannot fix an
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
	// how an app id becomes an address. PathPrefix is set when every app shares
	// one host instead of taking a subdomain, in which case DomainTemplate is
	// not the whole story and a client needs both.
	BaseDomain     string `json:"base_domain"`
	PathPrefix     string `json:"path_prefix,omitempty"`
	DomainTemplate string `json:"domain_template"`

	// Namespace is the one namespace this deployment uses — for itself and for
	// every app it deploys. Reported so a client can say where an app's objects
	// live, and because it is no longer derivable from the app id.
	Namespace string `json:"namespace"`

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
	respond(w, http.StatusOK, s.configResponse(r))
}

// configResponse builds this deployment's self-description.
//
// It is a method rather than inline in the handler because more than one
// endpoint reports it: /api/v1/config is the endpoint for it, and /api/v1/overview
// carries it so a dashboard needs one call rather than two. Building it in one
// place is what keeps those two from describing the same deployment differently.
func (s *Server) configResponse(r *http.Request) configResponse {
	return configResponse{
		APIVersion:      APIVersion,
		Version:         buildinfo.Version,
		APIBaseURL:      s.baseURL(r),
		BaseDomain:      s.cfg.BaseDomain,
		PathPrefix:      s.cfg.PathPrefix,
		DomainTemplate:  s.domainTemplate(),
		Namespace:       s.cfg.Namespace,
		MaxSimpleUpload: s.cfg.MaxSimpleUpload,
		ChunkSize:       s.cfg.ChunkSize,
		MaxChunkBytes:   s.cfg.MaxChunkBytes,
		Capabilities: map[string]bool{
			// Reported so a client can tell before it tries. A deployment with
			// no registry or no cluster is a legitimate way to run AppLab — the
			// API and the source half still work — and a caller that assumed
			// otherwise would get a confusing 500 instead of a clear 501.
			"build":  s.build != nil && s.build.Ready(),
			"deploy": s.deployer != nil && s.deployer.Ready(),
			"source": s.initSource != nil,
			"git":    s.git != nil,
		},
	}
}

// domainTemplate shows how an app id becomes an address, so a client does not
// have to know the deployment's convention to guess a URL.
//
// It is the *host* template, and with a path prefix that is only half the
// address: every app then shares one host and the path says which is meant. The
// two forms are different enough that reporting the wrong one would be worse
// than reporting none, so the prefix changes it rather than being folded into
// it.
//
// The base path is part of it, because an app's route is nested inside it: a
// template of "<domain>/apps/<app>" would describe an address the gateway does
// not serve when the installation is itself under "/applab".
func (s *Server) domainTemplate() string {
	if s.cfg.BaseDomain == "" {
		return "<base domain not configured>"
	}
	if s.cfg.PathPrefix != "" {
		basePath := strings.TrimSuffix(strings.TrimSpace(s.cfg.BasePath), "/")
		return s.cfg.BaseDomain + basePath + s.cfg.PathPrefix + "/<app>"
	}
	return "*." + s.cfg.BaseDomain
}

// baseURL returns the address callers should use for this deployment.
//
// An explicitly configured base wins. Otherwise it is derived from the request,
// which is right behind an ingress and wrong the moment a proxy rewrites the
// Host header — so the configured form is the safer deployment, and this
// fallback exists so a development instance works with no configuration.
//
// The base path is part of the address, not something a caller appends: this is
// what a client is told to build its links from, and a client that has to know
// about a prefix it was never told would build every one of them wrong.
func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		return s.cfg.BaseURL
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host + s.cfg.BasePath
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
