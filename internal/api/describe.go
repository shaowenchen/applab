package api

import (
	"net/http"
	"strconv"

	"github.com/shaowenchen/applab/internal/auth"
	"github.com/shaowenchen/applab/internal/model"
)

// describeResponse is what GET /api/v1/describe returns.
//
// It exists because an agent arriving at a deployment has to answer three
// questions before it can do anything: where is this, what can it do, and what
// is already here. Every one of them is answerable from the existing endpoints —
// config, apps, overview — but only by making three calls and knowing which
// parts of each to read, which is knowledge an agent does not have yet.
//
// So this is deliberately a summary rather than a new source of truth. Every
// field is derived from the same values the other endpoints report, so it cannot
// disagree with them; what it adds is the shape, in one call, with the fields an
// agent needs named rather than buried.
//
// It carries no API reference. The endpoint list and the request shapes are
// llms.txt, which is generated from the route table and is the contract; this is
// *this deployment*, which no static document can describe.
type describeResponse struct {
	// Summary is a sentence an agent can quote back without assembling it.
	Summary string `json:"summary"`

	// API is how to reach this deployment.
	API describeAPI `json:"api"`

	// Deployment is what this installation is wired to: the same
	// self-description GET /api/v1/config returns.
	Deployment configResponse `json:"deployment"`

	// Build and Deploy name the settings that decide whether an app can be
	// built and where it is then published. Both are reported even when the
	// capability is off, so an agent can say *why* it cannot build rather than
	// only that it cannot.
	Build  describeBuild  `json:"build"`
	Deploy describeDeploy `json:"deploy"`

	// Access is what a key may do, which is the first thing an agent holding one
	// needs to know: an app key is not a lesser admin key, it is a different
	// thing with a different reach.
	Access describeAccess `json:"access"`

	// Apps is every app that exists, with where each is served.
	Apps []appResponse `json:"apps"`

	// HowTo is the shortest path to doing each thing, as commands rather than as
	// prose about commands. An agent does not need the behaviour explained; it
	// needs the call.
	HowTo describeHowTo `json:"how_to"`
}

// describeAPI is how this deployment is reached.
type describeAPI struct {
	Version    string `json:"version"`
	APIVersion string `json:"api_version"`

	// BaseURL is the address to prefix every route with, path included. It is
	// derived from the request, so it reflects the address the caller actually
	// reached this service at.
	BaseURL string `json:"base_url"`

	// AuthHeader is the exact header every request but /health, /metrics,
	// /api/v1/config, /api/v1/version and /llms.txt must carry.
	AuthHeader string `json:"auth_header"`
}

// describeBuild is where images go and whether building works at all.
type describeBuild struct {
	Enabled  bool   `json:"enabled"`
	Registry string `json:"registry,omitempty"`

	// Rootless is the security posture of a build. Reported because a build
	// that fails on a node without unprivileged user namespaces fails with a
	// permissions error that does not name the setting.
	Rootless bool `json:"rootless"`
}

// describeDeploy is where apps are published and how they are addressed.
type describeDeploy struct {
	Enabled    bool   `json:"enabled"`
	Gateway    string `json:"gateway,omitempty"`
	BaseDomain string `json:"base_domain,omitempty"`

	// PathPrefix is set when every app shares one host rather than taking a
	// subdomain. When it is set, an app's address is base_domain + path_prefix +
	// "/" + id, and DomainTemplate alone is not the whole answer.
	PathPrefix string `json:"path_prefix,omitempty"`

	// DomainTemplate is how an app id becomes an address, as a format string.
	DomainTemplate string `json:"domain_template,omitempty"`
}

// describeAccess is what the key in hand reaches.
type describeAccess struct {
	// Tier is "admin" or "app", decided by the key that made this request.
	Tier string `json:"tier"`

	// App is the app an app key is scoped to, empty for an admin key.
	App string `json:"app,omitempty"`

	Reach string `json:"reach"`
}

// describeHowTo is the shortest path to each operation.
type describeHowTo struct {
	Push string `json:"push"`
	Logs string `json:"logs"`

	// Clone is how to read an app's source over git, which is also the endpoint
	// that serves it.
	Clone string `json:"clone"`

	// HTTP is the equivalent for a caller with no CLI, naming the route to call.
	HTTP map[string]string `json:"http"`
}

func (s *Server) handleDescribe(w http.ResponseWriter, r *http.Request) {
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "list apps").Wrap(err))
		return
	}

	identity := identityFrom(r.Context())
	scheme := s.scheme(r)
	base := s.baseURL(r)

	out := make([]appResponse, 0, len(apps))
	for _, a := range apps {
		if !identity.Admin() && a.ID != identity.App {
			continue
		}
		if a.Status == model.AppStatusDeleted {
			continue
		}
		out = append(out, toAppResponse(a, s.cfg.BaseDomain, s.cfg.PathPrefix, scheme))
	}

	cfg := s.configResponse(r)

	// One key, one sentence. Assembled here rather than left to the caller
	// because "what is this" is the question the endpoint exists to answer, and
	// an agent that has to compose the answer from six fields is one that might
	// get it wrong.
	summary := "applab " + cfg.Version + " in namespace " + cfg.Namespace
	if cfg.BaseDomain != "" {
		summary += ", serving apps at " + cfg.DomainTemplate
	} else {
		summary += ", serving no apps outside the cluster (no base domain is configured)"
	}
	summary += ", with " + plural(len(out), "app", "apps") + "."

	respond(w, http.StatusOK, describeResponse{
		Summary:    summary,
		Deployment: cfg,
		API: describeAPI{
			Version:    cfg.Version,
			APIVersion: cfg.APIVersion,
			BaseURL:    base,
			AuthHeader: "Authorization: Bearer <key>",
		},
		Build: describeBuild{
			Enabled:  cfg.Capabilities["build"],
			Registry: s.cfg.Build.Registry,
			Rootless: s.cfg.Build.Rootless == nil || *s.cfg.Build.Rootless,
		},
		Deploy: describeDeploy{
			Enabled:        cfg.Capabilities["deploy"],
			Gateway:        s.cfg.Deploy.Gateway,
			BaseDomain:     cfg.BaseDomain,
			PathPrefix:     cfg.PathPrefix,
			DomainTemplate: cfg.DomainTemplate,
		},
		Access: describeAccess{
			Tier:  accessTier(identity),
			App:   identity.App,
			Reach: accessReach(identity),
		},
		Apps: out,
		HowTo: describeHowTo{
			// The commands name the address explicitly rather than relying on
			// APPLAB_URL being set, so they can be copied into a shell as-is.
			Push:  "applab push <app>",
			Logs:  "applab logs <app> -f",
			Clone: "git -c http.extraHeader='" + "Authorization: Bearer <key>" + "' clone " + base + "/git/<app>.git",
			HTTP: map[string]string{
				"create_app":   "POST " + base + "/api/v1/apps  {\"id\":\"<app>\",\"port\":8080}",
				"upload":       "POST " + base + "/api/v1/apps/<app>/source?message=<msg>  (application/gzip)",
				"build":        "POST " + base + "/api/v1/apps/<app>/builds",
				"deploy":       "POST " + base + "/api/v1/apps/<app>/deploy",
				"status":       "GET " + base + "/api/v1/apps/<app>/status",
				"logs":         "GET " + base + "/api/v1/apps/<app>/logs?follow=false&tail=500",
				"config":       "GET " + base + "/api/v1/apps/<app>/config",
				"set_env":      "PUT " + base + "/api/v1/apps/<app>/env  {\"env\":{\"K\":\"V\"}}",
				"set_secret":   "PUT " + base + "/api/v1/apps/<app>/secrets  {\"secrets\":{\"K\":\"V\"}}",
				"app_key":      "GET " + base + "/api/v1/apps/<app>/key",
				"all_apps":     "GET " + base + "/api/v1/apps",
				"overview":     "GET " + base + "/api/v1/overview",
				"api_contract": base + "/llms.txt",
			},
		},
	})
}

// accessTier names the tier a request's identity belongs to.
func accessTier(identity auth.Identity) string {
	if identity.Admin() {
		return "admin"
	}
	return "app"
}

// accessReach says what a key of this tier may do, in the words the rest of the
// documentation uses.
func accessReach(identity auth.Identity) string {
	if identity.Admin() {
		return "Everything: every app, every app's key, and deletion. This is the platform credential."
	}
	return "One app only: push, build, deploy, roll back, read logs and configure it. It cannot delete the app and cannot see any other."
}

// plural renders "1 app" / "2 apps".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
