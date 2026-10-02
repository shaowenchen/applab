package openapi

import (
	"sort"
	"strings"
)

// ResponseKind says how a route's 2xx answer is shaped on the wire.
//
// It is the one thing the route table cannot express and reflection cannot
// guess: the same server answers some routes inside a `{"data": ...}` envelope,
// one with a bare object, and three with a plain-text stream. A generated client
// that unwrapped `data` from a log stream would destroy the body it was trying to
// read.
type ResponseKind int

const (
	// EnvelopeJSON is `{"data": <payload>}`, the shape of every ordinary route.
	EnvelopeJSON ResponseKind = iota
	// BareJSON is a JSON object written with no envelope — only /health.
	BareJSON
	// TextPlain is a text body read to its end.
	TextPlain
	// TextPlainStream is a text body that may never end, read incrementally.
	TextPlainStream
)

// OpSpec is what a hand-written registry entry says about one operation.
//
// The route table supplies the method, the path, the documentation and the
// credential tier. This supplies everything a table has no room for: the shape
// of the request body, the shape and kind of the answer, and the query
// parameters.
type OpSpec struct {
	// ID is the operationId openapi-generator turns into a method name. It is
	// hand-written because a name derived from the path reads badly in every
	// target language and is not stable when a path gains a segment.
	ID string

	// Tag groups the operation in generated documentation.
	Tag string

	// Request names the component schema of the request body, or "" for none.
	Request string
	// RequestType is the request content type. Empty means application/json.
	RequestType string

	// Status is the success code: 200, 201 or 202.
	Status int
	// Response names the component schema of the payload, or "" for a body that
	// is text or has no payload.
	Response string
	// Array says the payload is a list of Response rather than a single one.
	Array bool
	// Kind says how the answer is framed.
	Kind ResponseKind

	// Query are the query parameters the route reads.
	Query []Parameter
}

// Operations is every operation, keyed by "METHOD /path" with the path spelled
// as the route table spells it.
//
// This map is the one place a new route has to be added by hand, and a test
// asserts it covers the route table exactly: a route with no entry fails the
// build by name, and an entry with no route does too. That is what keeps the
// specification complete without anyone remembering to check.
func Operations() map[string]OpSpec {
	return map[string]OpSpec{
		// -- Liveness and self-description -------------------------------
		"GET /health": {
			ID: "getHealth", Tag: "meta", Status: 200, Kind: BareJSON, Response: "HealthResponse",
		},
		"GET /metrics": {
			ID: "getMetrics", Tag: "meta", Status: 200, Kind: TextPlain,
		},
		"GET /api/v1/config": {
			ID: "getConfig", Tag: "meta", Status: 200, Response: "Config",
		},
		"GET /bootstrap/applab.sh": {
			ID: "getBootstrapScript", Tag: "meta", Status: 200, Kind: TextPlain,
		},
		"GET /bootstrap": {
			ID: "getBootstrapFiles", Tag: "meta", Status: 200, Response: "BootstrapFilesResponse",
		},
		"GET /api/v1/version": {
			ID: "getVersion", Tag: "meta", Status: 200, Response: "VersionResponse",
		},
		"GET /api/v1/describe": {
			ID: "getDescribe", Tag: "meta", Status: 200, Response: "DescribeResponse",
		},

		// -- The platform's own machinery --------------------------------
		"GET /api/v1/overview": {
			ID: "getOverview", Tag: "platform", Status: 200, Response: "Overview",
		},
		"GET /api/v1/platform/pods": {
			ID: "listSelfPods", Tag: "platform", Status: 200, Response: "PodList",
			Query: []Parameter{limitParam()},
		},
		"GET /api/v1/platform/resources": {
			ID: "getSelfUsage", Tag: "platform", Status: 200, Response: "AppUsage",
		},
		"GET /api/v1/platform/logs": {
			ID: "streamSelfLogs", Tag: "platform", Status: 200, Kind: TextPlainStream,
			Query: logQuery(), // follow defaults to true on this route
		},
		"GET /api/v1/platform/events": {
			ID: "listSelfEvents", Tag: "platform", Status: 200, Response: "EventList",
			Query: []Parameter{limitParam()},
		},

		// -- Apps --------------------------------------------------------
		"GET /api/v1/apps": {
			ID: "listApps", Tag: "apps", Status: 200, Response: "App", Array: true,
		},
		"POST /api/v1/apps": {
			ID: "createApp", Tag: "apps", Request: "CreateAppRequest", Status: 201, Response: "App",
		},
		"GET /api/v1/apps/{app}": {
			ID: "getApp", Tag: "apps", Status: 200, Response: "App",
		},
		"PATCH /api/v1/apps/{app}": {
			ID: "updateApp", Tag: "apps", Request: "UpdateAppRequest", Status: 200, Response: "App",
		},
		"DELETE /api/v1/apps/{app}": {
			ID: "deleteApp", Tag: "apps", Status: 200, Response: "DeletedResponse",
			Query: []Parameter{keepSourceParam()},
		},

		// -- Servers: other AppLab deployments ---------------------------
		"GET /api/v1/servers": {
			ID: "listServers", Tag: "servers", Status: 200, Response: "Server", Array: true,
		},
		"POST /api/v1/servers": {
			ID: "registerServer", Tag: "servers", Request: "RegisterServerRequest", Status: 201, Response: "Server",
		},
		"GET /api/v1/servers/{server}": {
			ID: "getServer", Tag: "servers", Status: 200, Response: "Server",
		},
		"DELETE /api/v1/servers/{server}": {
			ID: "removeServer", Tag: "servers", Status: 200, Response: "RemovedResponse",
		},
		"GET /api/v1/servers/{server}/apps": {
			ID: "listServerApps", Tag: "servers", Status: 200, Response: "App", Array: true,
		},
		"POST /api/v1/servers/{server}/apps": {
			ID: "createServerApp", Tag: "servers", Request: "CreateAppRequest", Status: 201, Response: "App",
		},
		"GET /api/v1/servers/{server}/apps/{app}": {
			ID: "getServerApp", Tag: "servers", Status: 200, Response: "App",
		},
		"DELETE /api/v1/servers/{server}/apps/{app}": {
			ID: "deleteServerApp", Tag: "servers", Status: 200, Response: "DeletedResponse",
			Query: []Parameter{keepSourceParam()},
		},

		// -- Configuration ------------------------------------------------
		"GET /api/v1/apps/{app}/config": {
			ID: "getAppConfig", Tag: "config", Status: 200, Response: "AppConfig",
		},
		"PUT /api/v1/apps/{app}/env": {
			ID: "setAppEnv", Tag: "config", Request: "SetEnvRequest", Status: 200, Response: "AppConfig",
		},
		"DELETE /api/v1/apps/{app}/env/{name}": {
			ID: "deleteAppEnv", Tag: "config", Status: 200, Response: "AppConfig",
		},
		"PUT /api/v1/apps/{app}/secrets": {
			ID: "setAppSecrets", Tag: "config", Request: "SetSecretsRequest", Status: 200, Response: "AppConfig",
		},
		"DELETE /api/v1/apps/{app}/secrets/{name}": {
			ID: "deleteAppSecret", Tag: "config", Status: 200, Response: "AppConfig",
		},
		"GET /api/v1/apps/{app}/key": {
			ID: "getAppKey", Tag: "config", Status: 200, Response: "AppKey",
		},
		"POST /api/v1/apps/{app}/key/rotate": {
			ID: "rotateAppKey", Tag: "config", Status: 200, Response: "AppKey",
		},
		"GET /api/v1/apps/{app}/agent/files": {
			ID: "listAgentFiles", Tag: "config", Status: 200, Response: "AgentFilesResponse",
		},
		"GET /api/v1/apps/{app}/agent/files/{file}": {
			ID: "getAgentFile", Tag: "config", Status: 200, Kind: TextPlain,
		},

		// -- Source -------------------------------------------------------
		"POST /api/v1/apps/{app}/source": {
			ID: "uploadSource", Tag: "source", Status: 200, Response: "UploadResult",
			// The body is the archive itself, raw or gzipped. It is declared as
			// binary so a generated client sends bytes rather than trying to
			// JSON-encode them.
			RequestType: "application/octet-stream",
			Query: []Parameter{
				query("message", "string", "Commit message for the upload."),
				query("parent", "string", "Parent commit to upload on top of (default: the branch tip)."),
				query("branch", "string", "Branch to commit to (default: the app's active branch)."),
				publishParam(),
			},
		},
		"GET /api/v1/apps/{app}/commits": {
			ID: "listCommits", Tag: "source", Status: 200, Response: "CommitList",
			Query: []Parameter{
				limitParam(),
				query("branch", "string", "Read another branch's history."),
			},
		},
		"GET /api/v1/apps/{app}/commits/{sha}": {
			ID: "getCommit", Tag: "source", Status: 200, Response: "Commit",
		},
		"GET /api/v1/apps/{app}/branches": {
			ID: "listBranches", Tag: "source", Status: 200, Response: "Branches",
		},
		"PUT /api/v1/apps/{app}/branch": {
			ID: "switchBranch", Tag: "source", Request: "SwitchBranchRequest", Status: 200, Response: "DeployResult",
		},
		"POST /api/v1/apps/{app}/source/uploads": {
			ID: "startChunkedUpload", Tag: "source", Request: "ChunkedUploadStartRequest",
			Status: 201, Response: "ChunkedUploadStartResponse",
		},
		"PUT /api/v1/apps/{app}/source/uploads/{upload}/parts/{index}": {
			ID: "uploadSourcePart", Tag: "source", Status: 200, Response: "UploadPartResponse",
			RequestType: "application/octet-stream",
		},
		"POST /api/v1/apps/{app}/source/uploads/{upload}/complete": {
			ID: "completeChunkedUpload", Tag: "source", Status: 200, Response: "UploadResult",
			Query: []Parameter{
				query("message", "string", "Commit message, overriding the one given when the upload started."),
				query("branch", "string", "Branch to commit to."),
				publishParam(),
			},
		},

		// -- Builds -------------------------------------------------------
		"POST /api/v1/apps/{app}/builds": {
			ID: "startBuild", Tag: "builds", Request: "BuildRequest", Status: 202, Response: "Build",
			Query: []Parameter{query("branch", "string", "Build another branch without switching to it.")},
		},
		"GET /api/v1/apps/{app}/builds": {
			ID: "listBuilds", Tag: "builds", Status: 200, Response: "Build", Array: true,
			Query: []Parameter{limitParam()},
		},
		"GET /api/v1/apps/{app}/builds/{build}": {
			ID: "getBuild", Tag: "builds", Status: 200, Response: "Build",
		},
		"GET /api/v1/apps/{app}/builds/{build}/logs": {
			ID: "streamBuildLogs", Tag: "builds", Status: 200, Kind: TextPlainStream,
			Query: []Parameter{followParam(false)},
		},
		"DELETE /api/v1/apps/{app}/builds/{build}": {
			ID: "cancelBuild", Tag: "builds", Status: 200, Response: "Build",
		},

		// -- Deploy -------------------------------------------------------
		"POST /api/v1/apps/{app}/deploy": {
			ID: "deployApp", Tag: "deploy", Request: "DeployRequest", Status: 200, Response: "DeployResult",
			Query: []Parameter{query("branch", "string", "Deploy another branch, switching the app to it.")},
		},
		"POST /api/v1/apps/{app}/rollback": {
			ID: "rollbackApp", Tag: "deploy", Request: "RollbackRequest", Status: 200, Response: "DeployResult",
		},
		"GET /api/v1/apps/{app}/status": {
			ID: "getAppStatus", Tag: "deploy", Status: 200, Response: "Status",
		},
		"POST /api/v1/apps/{app}/restart": {
			ID: "restartApp", Tag: "deploy", Status: 200, Response: "RestartResponse",
		},
		"POST /api/v1/apps/{app}/stop": {
			ID: "stopApp", Tag: "deploy", Status: 200, Response: "StopResponse",
		},

		// -- Observing a running app --------------------------------------
		"GET /api/v1/apps/{app}/resources": {
			ID: "getAppUsage", Tag: "observe", Status: 200, Response: "AppUsage",
		},
		"GET /api/v1/apps/{app}/pods": {
			ID: "listAppPods", Tag: "observe", Status: 200, Response: "PodList",
			Query: []Parameter{query("label", "string", "An extra label selector to narrow the pods.")},
		},
		"GET /api/v1/apps/{app}/logs": {
			ID: "streamAppLogs", Tag: "observe", Status: 200, Kind: TextPlainStream,
			Query: logQuery(),
		},
		"GET /api/v1/apps/{app}/events": {
			ID: "listAppEvents", Tag: "observe", Status: 200, Response: "EventList",
			Query: []Parameter{limitParam()},
		},
		"GET /api/v1/apps/{app}/diagnose": {
			ID: "diagnoseApp", Tag: "observe", Status: 200, Response: "Diagnosis",
		},
	}
}

// GitPath is the one path the specification carries that is not in the route
// table.
//
// The git transport is mounted as a whole prefix rather than declared as routes,
// because its paths are git's own protocol (`/info/refs`, `/git-upload-pack`)
// with a repository name in front — a shape a route table of `{app}` templates
// cannot express. It is declared here so the generated clients can see that
// cloning exists and what credential it takes, and it is named separately so the
// registry test knows this one path is expected to have no route behind it.
const GitPath = "/git/{repo}"

// Query parameter constructors, so the common ones read the same everywhere.

func limitParam() Parameter {
	return Parameter{
		Name: "limit", In: "query", Schema: &Schema{Type: "integer"},
		Description: "Maximum number of items to return.",
		Example:     20,
	}
}

func keepSourceParam() Parameter {
	return Parameter{
		Name: "keep_source", In: "query", Schema: &Schema{Type: "boolean"},
		Description: "Retain the git repository when the app is deleted.",
		Example:     false,
	}
}

func publishParam() Parameter {
	return Parameter{
		Name: "publish", In: "query", Schema: &Schema{Type: "boolean"},
		Description: "Build and deploy after committing. Defaults to true; send false to upload without shipping.",
		Example:     true,
	}
}

func followParam(def bool) Parameter {
	return Parameter{
		Name: "follow", In: "query", Schema: &Schema{Type: "boolean"},
		Description: "Keep the connection open and stream lines as they arrive rather than returning what exists now. Read the body incrementally; the response may never end.",
		Example:     def,
	}
}

// logQuery is the parameter set the three log routes share.
func logQuery() []Parameter {
	return []Parameter{
		query("pod", "string", "Read one pod's log by name."),
		query("container", "string", "Read one container's log within the pod."),
		query("tail", "integer", "Number of lines from the end to return."),
		{Name: "previous", In: "query", Schema: &Schema{Type: "boolean"}, Description: "Read the previous container instance, where a crash loop's reason is written."},
		query("since", "string", "Only lines newer than this duration or timestamp."),
		followParam(true),
	}
}

func query(name, typ, desc string) Parameter {
	return Parameter{Name: name, In: "query", Schema: &Schema{Type: typ}, Description: desc}
}

// shortSummary is the first sentence of a route's documentation, for the
// operation's summary line.
//
// The full documentation goes in `description`; this is what a generated client's
// method list shows, and a paragraph there is unreadable. A Doc with no sentence
// break is used whole.
func shortSummary(doc string) string {
	if i := strings.Index(doc, ". "); i >= 0 {
		return doc[:i+1]
	}
	return doc
}

// pathParamExample gives a plausible value per path placeholder, so a generated
// client and its documentation show something a reader recognises rather than
// `string`.
func pathParamExample(name string) any {
	switch name {
	case "app":
		return "shop"
	case "server":
		return "local"
	case "build":
		return "a1b2c3d4e5f6"
	case "sha":
		return "a1b2c3d4e5f6"
	case "upload":
		return "3f2a1c9d"
	case "index":
		return 1
	case "file":
		return "applab.sh"
	case "name":
		return "PORT"
	case "repo":
		return "shop.git"
	default:
		return nil
	}
}

// pathParams turns a path template's placeholders into parameters.
func pathParams(path string) []Parameter {
	var out []Parameter
	for _, seg := range strings.Split(path, "/") {
		if !strings.HasPrefix(seg, "{") || !strings.HasSuffix(seg, "}") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(seg, "{"), "}")
		typ := "string"
		example := pathParamExample(name)
		if _, ok := example.(int); ok {
			typ = "integer"
		}
		out = append(out, Parameter{
			Name: name, In: "path", Required: true,
			Schema:  &Schema{Type: typ},
			Example: example,
		})
	}
	return out
}

// sortedOperationKeys returns the registry's keys in a stable order, so a test
// reporting a diff is readable.
func sortedOperationKeys(ops map[string]OpSpec) []string {
	keys := make([]string, 0, len(ops))
	for k := range ops {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
