package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/shaowenchen/applab/internal/auth"
	"github.com/shaowenchen/applab/internal/config"
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
// It carries the endpoint list, generated from the route table, and this
// deployment's own shape. The list is what a static document used to provide and
// the shape is what no static document can — so this one call is the whole of
// what an agent has to read to start.
type describeResponse struct {
	// Summary is a sentence an agent can quote back without assembling it.
	Summary string `json:"summary"`

	// API is how to reach this deployment, including the endpoint list.
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

	// AuthHeader is the exact header every request that needs a key must carry.
	AuthHeader string `json:"auth_header"`

	// Endpoints is every route this deployment serves, with the credential each
	// requires. Generated from the route table, so it cannot describe a route
	// that does not exist or miss one that does.
	Endpoints []describeEndpoint `json:"endpoints"`
}

// describeEndpoint is one route, as an agent needs it: what to call, and with
// what.
type describeEndpoint struct {
	Method string `json:"method"`
	Path   string `json:"path"`

	// Key is the credential the route requires: "none", "admin", "app" or
	// "token". Reported per route rather than as one note at the top, because
	// the tiers are not interchangeable — an app key on an admin route is a 403
	// the caller cannot explain, and a wrong guess about which routes its key
	// reaches is the most likely way for an agent to waste a call.
	Key string `json:"key"`

	// Doc is what the route does, in one line, from the route table.
	Doc string `json:"doc"`
}

// describeBuild is where images go and whether building works at all.
type describeBuild struct {
	Enabled  bool   `json:"enabled"`
	Registry string `json:"registry,omitempty"`
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
	// The app list, and what the caller is not shown. An anonymous caller gets no
	// apps at all: the ids and addresses of what someone has deployed are the
	// deployment's data, not this service's shape.
	//
	// The line is drawn on whether a credential was presented rather than on the
	// tier, because "authenticated" is exactly the question — an app key proves
	// it is someone's key, an anonymous request proves nothing.
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "list apps").Wrap(err))
		return
	}

	identity := identityFrom(r.Context())
	base := s.baseURL(r)

	out := make([]appResponse, 0, len(apps))
	live := s.liveStatus(r.Context())
	statuses := s.appStatuses(r.Context(), apps, live)
	for _, a := range apps {
		if !identity.Authenticated() {
			break
		}
		if !identity.Admin() && a.ID != identity.App {
			continue
		}
		out = append(out, s.toAppResponse(a, r, statuses[a.ID], live[a.ID]))
	}

	cfg := s.configResponse(r)

	// What a caller with no key is not told. The endpoint list is public — it
	// describes the service — but the deployment's wiring is not: the namespace
	// it runs in, where builds push their images and the gateway apps hang off
	// are the shape of someone's infrastructure, and an anonymous request has
	// given no reason to disclose it.
	//
	// Reported as empty rather than omitted, and said out loud in the summary,
	// so a caller can tell "there is nothing there" from "you have not shown me
	// that yet" — the failure mode of withholding silently is an agent that
	// concludes the deployment cannot build.
	//
	// Done before the summary is composed, not after: the summary is a sentence
	// built out of these same fields, so redacting them afterwards leaves the
	// namespace sitting in the prose while its field reads empty.
	anonymous := !identity.Authenticated()
	withheld := ""
	if anonymous {
		withheld = " Needs a key for: the namespace, the build registry, the gateway, the apps and the calls below."
		cfg.Namespace = ""
		cfg.Capabilities = map[string]bool{}
	}

	// One key, one sentence. Assembled here rather than left to the caller
	// because "what is this" is the question the endpoint exists to answer, and
	// an agent that has to compose the answer from six fields is one that might
	// get it wrong.
	//
	// The app count is stated only to a caller that was shown the list: "with 0
	// apps" is a different claim from "you have not been shown the apps", and an
	// agent reading the first would conclude the platform is empty.
	summary := "AppLab " + cfg.Version
	if !anonymous {
		summary += " in namespace " + cfg.Namespace
	}
	if cfg.BaseDomain != "" {
		summary += ", serving apps at " + cfg.DomainTemplate
	} else {
		summary += ", serving no apps outside the cluster (no base domain is configured)"
	}
	if !anonymous {
		summary += ", with " + plural(len(out), "app", "apps")
	}
	summary += "." + withheld

	respond(w, http.StatusOK, describeResponse{
		Summary:    summary,
		Deployment: cfg,
		API: describeAPI{
			Version:    cfg.Version,
			APIVersion: cfg.APIVersion,
			BaseURL:    base,
			AuthHeader: "Authorization: Bearer <key>",
			Endpoints:  s.RouteReference(),
		},
		Build: describeBuild{
			Enabled:  cfg.Capabilities["build"],
			Registry: registryFor(s.cfg, identity),
		},
		Deploy: describeDeploy{
			Enabled:        cfg.Capabilities["deploy"],
			Gateway:        gatewayFor(s.cfg, identity),
			BaseDomain:     cfg.BaseDomain,
			PathPrefix:     cfg.PathPrefix,
			DomainTemplate: cfg.DomainTemplate,
		},
		Access: describeAccess{
			Tier:  accessTier(identity),
			App:   identity.App,
			Reach: accessReach(identity),
		},
		Apps:  out,
		HowTo: s.describeHowTo(base, identity),
	})
}

// registryFor and gatewayFor report the deployment's wiring to a caller that has
// authenticated, and nothing to one that has not.
func registryFor(cfg config.Config, identity auth.Identity) string {
	if !identity.Authenticated() {
		return ""
	}
	return cfg.Build.Registry
}

func gatewayFor(cfg config.Config, identity auth.Identity) string {
	if !identity.Authenticated() {
		return ""
	}
	return cfg.Deploy.Gateway
}

// describeHowTo is the shortest path to each operation, as commands rather than
// as prose about commands.
//
// The commands name the address explicitly rather than relying on APPLAB_URL
// being set, so they can be copied into a shell as-is.
func (s *Server) describeHowTo(base string, identity auth.Identity) describeHowTo {
	out := describeHowTo{
		Push:  "applab push <app>",
		Logs:  "applab logs <app> -f",
		Clone: "git clone " + gitURLWithPassword(base, "<app>", ""),
		HTTP: map[string]string{
			"create_app": "POST " + base + "/api/v1/apps  {\"id\":\"<app>\",\"port\":80}",
			"upload":     "POST " + base + "/api/v1/apps/<app>/source?message=<msg>  (application/gzip)",
			"build":      "POST " + base + "/api/v1/apps/<app>/builds",
			"deploy":     "POST " + base + "/api/v1/apps/<app>/deploy",
			"status":     "GET " + base + "/api/v1/apps/<app>/status",
			"logs":       "GET " + base + "/api/v1/apps/<app>/logs?follow=false&tail=500",
			"config":     "GET " + base + "/api/v1/apps/<app>/config",
			"set_env":    "PUT " + base + "/api/v1/apps/<app>/env  {\"env\":{\"K\":\"V\"}}",
			"set_secret": "PUT " + base + "/api/v1/apps/<app>/secrets  {\"secrets\":{\"K\":\"V\"}}",
			"app_key":    "GET " + base + "/api/v1/apps/<app>/key",
			"all_apps":   "GET " + base + "/api/v1/apps",
			"overview":   "GET " + base + "/api/v1/overview",
			"describe":   "GET " + base + "/api/v1/describe",
		},
	}

	// A caller with no key cannot run any of these, and offering the commands as
	// though it could is how an agent ends up reporting a 401 as a broken
	// deployment. The one call it can make is the one it is making.
	if !identity.Authenticated() {
		out.Push, out.Logs, out.Clone = "", "", ""
		out.HTTP = map[string]string{
			"describe": "GET " + base + "/api/v1/describe",
			"config":   "GET " + base + "/api/v1/config",
			"version":  "GET " + base + "/api/v1/version",
		}
	}
	return out
}

// accessTier names the tier a request's identity belongs to.
func accessTier(identity auth.Identity) string {
	switch {
	case identity.Anonymous:
		return "none"
	case identity.Admin():
		return "admin"
	default:
		return "app"
	}
}

// accessReach says what a key of this tier may do, in the words the rest of the
// documentation uses.
func accessReach(identity auth.Identity) string {
	switch {
	case identity.Anonymous:
		return "No key presented. The endpoint list above and this deployment's address are readable; nothing else is."
	case identity.Admin():
		return "Everything: every app, every app's key, and deletion. This is the platform credential."
	default:
		return "One app only: push, build, deploy, roll back, read logs and configure it. It cannot delete the app and cannot see any other."
	}
}

// plural renders "1 app" / "2 apps".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// gitURLWithPassword turns a base URL into the form a clone uses, with a
// placeholder where the credential goes.
//
// Built here rather than written as a literal so the scheme is stripped once and
// the result is right for a deployment reached over https and over http alike:
// the address is inserted into a `git clone` line, and that line already names
// the scheme.
//
// The branch is part of the path — "/git/shop@dev.git" — and the default branch
// is the one URL that omits it, so that an app's address stays stable as it
// gains branches. See gitx.Transport for why the branch is joined with "@"
// rather than as a second path segment.
func gitURLWithPassword(base, appID, branch string) string {
	host := base
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	name := appID
	if branch != "" && branch != model.DefaultBranch {
		name += "@" + branch
	}
	return "https://x:<key>@" + host + "/git/" + name + ".git"
}
