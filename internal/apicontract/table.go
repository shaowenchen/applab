package apicontract

// Table is every route this API serves and how each surface covers it.
//
// It is hand-written, and that is the point: the value of the test is that a
// person had to decide, for each route and each surface, whether the route is
// reachable and — when it is not — why. A table generated from the same data the
// check runs on would decide nothing.
//
// The needles are the substrings that actually appear in each surface. Where a
// surface builds a path by concatenation (the console's JavaScript) the needle is
// the static part of that path, which is why some read as a bare suffix like
// "/diagnose": that is exactly what the console's source contains.
//
// When a route is added to the API, this table will not have it and the test will
// name it. Adding a row means answering the four questions; the exemption
// comments below are the answers already given, and the reasoning behind them is
// the thing to read before writing a new one.
var Table = []Entry{
	// -- Liveness, self-description, bootstrap --------------------------
	// These are the routes a machine calls — a kubelet probe, a Prometheus
	// scraper, an agent fetching the tool before it has a key. They are not
	// operations a person performs, which is why no interactive surface offers
	// them. The bootstrap script is fetched by curl, which is why the seeded
	// script's own self-update reaches it while the others do not.
	{
		Method: "GET", Path: "/health",
		Console: exempt("a probe endpoint; the console is served by the deployment it would check"),
		CLI:     exempt("a probe endpoint; `applab` runs on the same side of the network"),
		Script:  exempt("a probe endpoint; the script reaches the deployment through the API it is about"),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/metrics",
		Console: exempt("Prometheus scrapes this; it is not a console operation"),
		CLI:     exempt("Prometheus scrapes this; a terminal has no use for the exposition format"),
		Script:  exempt("Prometheus scrapes this; the script is not a scraper"),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/config",
		Console: exempt("the console learns capabilities from the overview, which carries the same self-description"),
		CLI:     coveredByMethod(),
		Script:  covered("$path"),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/bootstrap/applab.sh",
		Console: exempt("the console is signed-in users; the bootstrap script is for a caller with no key, which the console never has"),
		CLI:     exempt("the CLI already is a client; fetching its own installer is not a thing it does"),
		Script:  covered("/bootstrap/applab.sh"),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/bootstrap",
		Console: exempt("see /bootstrap/applab.sh: a discovery route for a keyless caller"),
		CLI:     exempt("see /bootstrap/applab.sh"),
		Script:  covered("/bootstrap/"),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/version",
		Console: exempt("the console shows no version and has no place for one"),
		CLI:     exempt("`applab version` prints the CLI's own build, which answers the question a terminal is asking"),
		Script:  exempt("the script has no version command; it refreshes itself from the deployment instead"),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/describe",
		Console: exempt("the console is the thing being described; it reads the overview"),
		CLI:     exempt("the CLI reads the routes it implements; a self-description adds nothing a terminal acts on"),
		Script:  exempt("an agent reads describe before it has a key; the script is for someone who already has one"),
		SDK:     coveredByMethod(),
	},

	// -- The platform's own machinery -----------------------------------
	// Admin-scoped. The console and the CLI both reach these; the seeded script
	// holds one app's key and cannot (see the "not covered" note below), which is
	// the one place a surface is deliberately narrower than the API.
	{
		Method: "GET", Path: "/api/v1/overview",
		Console: covered("/api/v1/overview"),
		CLI:     coveredByMethod(),
		Script:  exempt("admin-scoped; the seeded script holds one app's key and cannot read the platform"),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/platform/pods",
		Console: covered("/api/v1/platform/pods"),
		CLI:     coveredByMethod(),
		Script:  exempt("admin-scoped; see /api/v1/overview"),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/platform/resources",
		Console: covered("/api/v1/platform/resources"),
		CLI:     coveredByMethod(),
		Script:  exempt("admin-scoped; see /api/v1/overview"),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/platform/logs",
		Console: covered("/api/v1/platform/logs"),
		CLI:     coveredByMethod(),
		Script:  exempt("admin-scoped; see /api/v1/overview"),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/platform/events",
		Console: covered("/api/v1/platform/events"),
		CLI:     coveredByMethod(),
		Script:  exempt("admin-scoped; see /api/v1/overview"),
		SDK:     coveredByMethod(),
	},

	// -- Apps -----------------------------------------------------------
	{
		Method: "GET", Path: "/api/v1/apps",
		Console: covered("/api/v1/apps"),
		CLI:     coveredByMethod(),
		Script:  covered(`api GET "$(app_path)"`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "POST", Path: "/api/v1/apps",
		Console: covered("/api/v1/apps"),
		CLI:     coveredByMethod(),
		Script:  covered(`api POST "$(app_path)"`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/apps/{app}",
		Console: exempt("the console shows an app from the list it already has; it never re-reads a single one"),
		CLI:     coveredByMethod(),
		Script:  covered(`api GET "/api/v1/apps/$APP"`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "PATCH", Path: "/api/v1/apps/{app}",
		// The console writes an app's replicas, port, resources and auto-deploy
		// through this route. Its method sits on the line after the URL, so the
		// needle is the URL with the options object opening — a request to the
		// app with options is a write, and reads pass none.
		Console: covered(`"/api/v1/apps/" + state.app, {`),
		CLI:     coveredByMethod(),
		Script:  covered(`api PATCH "/api/v1/apps/$APP"`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "DELETE", Path: "/api/v1/apps/{app}",
		Console: exempt("the console's delete goes through this route but its path is assembled at call time, so the static check cannot see it; the seeded script covers the route"),
		CLI:     coveredByMethod(),
		Script:  covered(`api DELETE "$(app_path)/`),
		SDK:     coveredByMethod(),
	},

	// -- Servers: other deployments this one manages ---------------------
	// The remote-server capability. The console and the script both drive it; the
	// CLI does too. A remote's apps are reached through the relay, so "console
	// covers the server app routes" means the Servers view reaches them, with the
	// /api/v1/servers/ prefix the console's own source contains.
	{
		Method: "GET", Path: "/api/v1/servers",
		Console: covered("/api/v1/servers"),
		CLI:     coveredByMethod(),
		Script:  covered(`api GET "/api/v1/servers"`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "POST", Path: "/api/v1/servers",
		Console: covered("/api/v1/servers"),
		CLI:     coveredByMethod(),
		Script:  covered(`api POST "/api/v1/servers"`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/servers/{server}",
		Console: exempt("the console lists servers and acts on them; it never reads one back on its own"),
		CLI:     coveredByMethod(),
		Script:  exempt("the script lists and forgets servers; `servers` prints the list it needs"),
		SDK:     coveredByMethod(),
	},
	{
		Method: "DELETE", Path: "/api/v1/servers/{server}",
		Console: covered("/api/v1/servers/"),
		CLI:     coveredByMethod(),
		Script:  covered(`api DELETE "/api/v1/servers/`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/servers/{server}/apps",
		Console: covered("/api/v1/servers/"),
		CLI:     coveredByMethod(),
		Script:  exempt("the script's list --server reaches this through app_path, whose value the static check cannot follow"),
		SDK:     coveredByMethod(),
	},
	{
		Method: "POST", Path: "/api/v1/servers/{server}/apps",
		Console: covered("/api/v1/servers/"),
		CLI:     coveredByMethod(),
		Script:  exempt("the script's create --server reaches this through app_path; see the note above"),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/servers/{server}/apps/{app}",
		Console: exempt("the console's remote app rows do not open a detail view; the proxy relays the collection, not a single remote app's panels"),
		CLI:     coveredByMethod(),
		Script:  exempt("the script does not read a single remote app back"),
		SDK:     coveredByMethod(),
	},
	{
		Method: "DELETE", Path: "/api/v1/servers/{server}/apps/{app}",
		Console: covered("/api/v1/servers/"),
		CLI:     coveredByMethod(),
		Script:  exempt("the script's delete --server reaches this through app_path; see the note above"),
		SDK:     coveredByMethod(),
	},

	// -- An app's configuration ------------------------------------------
	{
		Method: "GET", Path: "/api/v1/apps/{app}/config",
		Console: covered("/api/v1/apps/"),
		CLI:     coveredByMethod(),
		Script:  covered(`/config"`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "PUT", Path: "/api/v1/apps/{app}/env",
		Console: covered("/api/v1/apps/"),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/env"`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "DELETE", Path: "/api/v1/apps/{app}/env/{name}",
		Console: covered("/env/"),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/env/`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "PUT", Path: "/api/v1/apps/{app}/secrets",
		Console: exempt("secret values are write-only; the console sets environment variables and leaves secrets to the terminal, where they are not left on a screen"),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/secrets"`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "DELETE", Path: "/api/v1/apps/{app}/secrets/{name}",
		Console: exempt("see PUT /api/v1/apps/{app}/secrets"),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/secrets/`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/apps/{app}/key",
		Console: covered("/api/v1/apps/"),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/key"`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "POST", Path: "/api/v1/apps/{app}/key/rotate",
		Console: exempt("rotating a key is an operator action; the console shows a key it does not rotate"),
		CLI:     coveredByMethod(),
		Script:  exempt("the script prints its app's key but does not rotate it; rotation is the admin's action"),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/apps/{app}/agent/files",
		Console: exempt("the seeded files are for a checkout to refresh itself, not for the console to browse"),
		CLI:     exempt("the CLI has the files in the checkout it was run from"),
		Script:  exempt("the script refreshes itself from the single file it needs, below"),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/apps/{app}/agent/files/{file}",
		Console: exempt("see /api/v1/apps/{app}/agent/files"),
		CLI:     exempt("see /api/v1/apps/{app}/agent/files"),
		// The script fetches this directly rather than through the api() helper,
		// because the file is a plain body it writes to disk.
		Script: covered(`/agent/files/$name`),
		SDK:    coveredByMethod(),
	},

	// -- Source -----------------------------------------------------------
	{
		Method: "POST", Path: "/api/v1/apps/{app}/source",
		Console: exempt("a browser cannot tar a directory; uploading source is a terminal operation"),
		CLI:     coveredByMethod(),
		Script:  exempt("the script uploads through the chunked endpoints below, which have no size limit"),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/apps/{app}/commits",
		Console: covered("/commits"),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/commits?limit=`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/apps/{app}/commits/{sha}",
		Console: exempt("the console shows the commit list; a single commit's detail adds nothing it does not already have"),
		CLI:     exempt("the CLI works from the commit list too"),
		Script:  exempt("the script reads commits from the list"),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/apps/{app}/branches",
		Console: covered("/branches"),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/branches`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "PUT", Path: "/api/v1/apps/{app}/branch",
		Console: covered("/branch"),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/branch"`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "POST", Path: "/api/v1/apps/{app}/source/uploads",
		Console: exempt("see POST /api/v1/apps/{app}/source: uploading is a terminal operation"),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/source/uploads"`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "PUT", Path: "/api/v1/apps/{app}/source/uploads/{upload}/parts/{index}",
		Console: exempt("see POST /api/v1/apps/{app}/source"),
		CLI:     coveredByMethod(),
		Script:  covered("/parts/$i"),
		SDK:     coveredByMethod(),
	},
	{
		Method: "POST", Path: "/api/v1/apps/{app}/source/uploads/{upload}/complete",
		Console: exempt("see POST /api/v1/apps/{app}/source"),
		CLI:     coveredByMethod(),
		Script:  covered("/complete"),
		SDK:     coveredByMethod(),
	},

	// -- Builds ------------------------------------------------------------
	{
		Method: "POST", Path: "/api/v1/apps/{app}/builds",
		Console: covered("/builds"),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/builds"`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/apps/{app}/builds",
		Console: covered("/builds"),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/builds?limit=`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/apps/{app}/builds/{build}",
		Console: exempt("the console reads the build list; it does not open a single build on its own"),
		// `applab builds --build <id>` reads one build back, which the client has
		// a method for (GetBuild). The table was wrong about this until the CLI
		// check caught it — a reminder that "the CLI has no command for it" is a
		// claim to verify, not to assume.
		CLI:    coveredByMethod(),
		Script: covered(`/$APP/builds/$sha"`),
		SDK:    coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/apps/{app}/builds/{build}/logs",
		Console: covered(`/builds/" + id + "/logs`),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/builds/$build/logs`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "DELETE", Path: "/api/v1/apps/{app}/builds/{build}",
		Console: covered(`/builds/" + buildID`),
		CLI:     coveredByMethod(),
		Script:  covered(`api DELETE "/api/v1/apps/$APP/builds/`),
		SDK:     coveredByMethod(),
	},

	// -- Deploy ------------------------------------------------------------
	{
		Method: "POST", Path: "/api/v1/apps/{app}/deploy",
		Console: covered("/deploy"),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/deploy"`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "POST", Path: "/api/v1/apps/{app}/rollback",
		Console: covered("/rollback"),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/rollback"`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/apps/{app}/status",
		Console: covered("/status"),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/status"`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "POST", Path: "/api/v1/apps/{app}/restart",
		Console: covered("/restart"),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/restart"`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "POST", Path: "/api/v1/apps/{app}/stop",
		Console: covered("/stop"),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/stop"`),
		SDK:     coveredByMethod(),
	},

	// -- Observing a running app --------------------------------------------
	{
		Method: "GET", Path: "/api/v1/apps/{app}/resources",
		Console: covered("/resources"),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/resources"`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/apps/{app}/pods",
		Console: covered("/pods"),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/pods"`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/apps/{app}/logs",
		Console: covered(`/logs"`),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/logs`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/apps/{app}/events",
		Console: covered("/events?limit="),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/events?limit=`),
		SDK:     coveredByMethod(),
	},
	{
		Method: "GET", Path: "/api/v1/apps/{app}/diagnose",
		Console: exempt("the console prints the diagnose command rather than running it; a diagnosis is long text, and a terminal is where a reader wants it"),
		CLI:     coveredByMethod(),
		Script:  covered(`/$APP/diagnose`),
		SDK:     coveredByMethod(),
	},
}
