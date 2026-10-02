package openapi

import (
	"fmt"
	"reflect"
	"strings"
)

// Route is the part of one route this package needs. It is a plain struct rather
// than api.RouteDescriptor so that internal/openapi does not import internal/api:
// the drift test imports both, and a generator that depended on the server would
// tie the specification to the code that answers it rather than to the table it
// is generated from.
type Route struct {
	Method string
	Path   string
	Tier   string
	Doc    string
}

// builder carries the schemas accumulated while walking the client types.
type builder struct {
	schemas map[string]*Schema
}

// Build assembles the OpenAPI document for a route table.
//
// It is a pure function of its inputs: the same routes and the same registry
// produce the same bytes, which is what makes the drift comparison meaningful.
func Build(routes []Route) (*Document, error) {
	b := &builder{schemas: map[string]*Schema{}}

	// The wire types first: reflection registers a component for each named
	// struct it reaches, so by the time operations reference `App` it exists.
	for _, v := range clientSchemaTypes {
		b.reflectSchema(reflect.TypeOf(v))
	}
	for name, schema := range handWrittenSchemas() {
		b.schemas[name] = schema
	}

	ops := Operations()

	paths := map[string]PathItem{}
	for _, r := range routes {
		key := r.Method + " " + r.Path
		spec, ok := ops[key]
		if !ok {
			// A route with no registry entry. This is not a panic: the registry
			// test reports it by name, and Build returning an error keeps the
			// failure legible rather than producing a document with a hole in it.
			return nil, fmt.Errorf("no registry entry for %s", key)
		}
		op, err := b.operation(r, spec)
		if err != nil {
			return nil, err
		}
		item := paths[r.Path]
		if err := item.set(r.Method, op); err != nil {
			return nil, err
		}
		paths[r.Path] = item
	}

	// The git transport, which is not in the route table — see GitPath.
	paths[GitPath] = PathItem{
		Get: &Operation{
			OperationID: "gitSmartHTTP",
			Summary:     "Clone or push an app's repository over git's smart HTTP protocol.",
			Description: "Not a JSON route: this path answers git's own protocol (`/info/refs`, `/git-upload-pack`, `/git-receive-pack`) with the repository named before the protocol suffix. It is declared here so a client can see that cloning exists and that it authenticates with HTTP Basic — the password is an API key and the username is ignored. A generated client does not wrap git; it is described, not called.",
			Tags:        []string{"git"},
			Parameters: []Parameter{
				{Name: "repo", In: "path", Required: true, Schema: &Schema{Type: "string"},
					Description: "`<app>.git` for the active branch, or `<app>@<branch>.git` for another.",
					Example:     "shop.git"},
			},
			Responses: map[string]Response{
				"200": {Description: "A git protocol response."},
			},
			Security: []map[string][]string{{"basicAuth": {}}},
			Tier:     "app",
		},
	}

	doc := &Document{
		OpenAPI: "3.1.0",
		Info: Info{
			Title:   "AppLab API",
			Version: apiVersionForDoc,
			Description: "AppLab deploys an application to Kubernetes by uploading its source.\n\n" +
				"Every JSON route answers inside a `{\"data\": ...}` envelope, except `/health`; failures answer " +
				"`{\"error\": \"...\", \"retryable\": bool}`. Three routes answer `text/plain` and may stream " +
				"(`x-streaming`), which a client must read incrementally rather than buffer.\n\n" +
				"This document is generated from AppLab's route table. Do not edit it by hand — " +
				"run `make gen-openapi`.",
		},
		Servers: []Server{
			{URL: "{baseUrl}", Description: "The deployment's address, including any base path (for example https://applab.example.com/applab)."},
		},
		Security: []map[string][]string{{"bearerAuth": {}}},
		Tags: []Tag{
			{Name: "meta", Description: "Liveness, configuration and self-description."},
			{Name: "platform", Description: "The AppLab deployment itself, rather than the apps it manages."},
			{Name: "apps", Description: "Create, read, update and delete apps."},
			{Name: "servers", Description: "Other AppLab deployments this one can manage."},
			{Name: "config", Description: "An app's environment, secrets and key."},
			{Name: "source", Description: "Uploading an app's source and managing its branches."},
			{Name: "builds", Description: "Building a commit into an image."},
			{Name: "deploy", Description: "Running an image, and stopping or rolling back what runs."},
			{Name: "observe", Description: "Logs, usage, events and diagnosis."},
			{Name: "git", Description: "Cloning and pushing source over git."},
		},
		Paths: paths,
		Components: Components{
			Schemas: b.schemas,
			SecuritySchemes: map[string]SecurityScheme{
				"bearerAuth": {Type: "http", Scheme: "bearer",
					Description: "An API key as a bearer token. An admin key reaches everything; an app key reaches one app (see x-applab-tier on each operation)."},
				"basicAuth": {Type: "http", Scheme: "basic",
					Description: "The git transport's scheme only: the password is an API key and the username is ignored."},
			},
		},
	}

	return doc, nil
}

// apiVersionForDoc is the API's own major version, which is what the document's
// `info.version` reports.
//
// It is deliberately not the release version: a build changes the binary's
// version every commit, and a document whose version moved every commit would
// make the drift test meaningless. What a client needs to know is which API it is
// written against, and that is v1 until the routes change incompatibly.
const apiVersionForDoc = "v1"

// operation builds one OpenAPI operation from a route and its registry entry.
func (b *builder) operation(r Route, spec OpSpec) (*Operation, error) {
	op := &Operation{
		OperationID: spec.ID,
		Summary:     shortSummary(r.Doc),
		Description: r.Doc,
		Tags:        []string{spec.Tag},
		Parameters:  pathParams(r.Path),
		Tier:        r.Tier,
	}

	op.Parameters = append(op.Parameters, spec.Query...)

	if spec.Request != "" || strings.HasPrefix(spec.RequestType, "application/octet-stream") {
		contentType := spec.RequestType
		if contentType == "" {
			contentType = "application/json"
		}
		var schema *Schema
		if strings.HasPrefix(contentType, "application/octet-stream") {
			schema = &Schema{Type: "string", Format: "binary",
				Description: "The source archive: a tar, optionally gzipped."}
		} else {
			schema = ref(spec.Request)
			if _, ok := b.schemas[spec.Request]; !ok {
				return nil, fmt.Errorf("%s: request schema %q is not defined", spec.ID, spec.Request)
			}
		}
		op.RequestBody = &RequestBody{
			Required: true,
			Content:  map[string]MediaType{contentType: {Schema: schema}},
		}
	}

	status := spec.Status
	if status == 0 {
		status = 200
	}
	responses := map[string]Response{}

	switch spec.Kind {
	case TextPlain, TextPlainStream:
		media := MediaType{Schema: &Schema{Type: "string"}}
		if spec.Kind == TextPlainStream {
			media.Streaming = true
		}
		responses[fmt.Sprint(status)] = Response{
			Description: "A plain-text body.",
			Content:     map[string]MediaType{"text/plain": media},
		}

	case BareJSON:
		schema, ok := b.schemas[spec.Response]
		if !ok {
			return nil, fmt.Errorf("%s: response schema %q is not defined", spec.ID, spec.Response)
		}
		responses[fmt.Sprint(status)] = Response{
			Description: "Success.",
			Content:     map[string]MediaType{"application/json": {Schema: schema}},
		}

	default: // EnvelopeJSON
		if spec.Response != "" {
			schema, ok := b.schemas[spec.Response]
			if !ok {
				return nil, fmt.Errorf("%s: response schema %q is not defined", spec.ID, spec.Response)
			}
			if spec.Array {
				schema = &Schema{Type: "array", Items: schema}
			}
			// The envelope is written per operation rather than as a generic
			// wrapper, because OpenAPI 3.1 generics are not portably understood
			// by the generators, and a client should decode `data` into a
			// concrete type.
			envelope := &Schema{
				Type: "object",
				Properties: map[string]*Schema{
					"data": schema,
				},
				Required: []string{"data"},
			}
			responses[fmt.Sprint(status)] = Response{
				Description: "Success.",
				Content:     map[string]MediaType{"application/json": {Schema: envelope}},
			}
		} else {
			responses[fmt.Sprint(status)] = Response{Description: "Success."}
		}
	}

	// Every JSON route can also answer with the error envelope, and a generated
	// client needs the type to catch it. The text routes deliberately do not get
	// one: their failures are the same envelope, but a client reading a stream
	// has already committed to the body.
	responses["default"] = Response{
		Description: "Failure. `retryable` says whether the same call could succeed on a retry.",
		Content:     map[string]MediaType{"application/json": {Schema: ref("Error")}},
	}
	op.Responses = responses

	// An open route overrides the document's global bearer requirement with an
	// empty list, which is how OpenAPI says "no credential".
	if r.Tier == "none" {
		op.Security = []map[string][]string{}
	}

	return op, nil
}

// set attaches an operation to the right method on a path item.
func (p *PathItem) set(method string, op *Operation) error {
	switch method {
	case "GET":
		p.Get = op
	case "POST":
		p.Post = op
	case "PUT":
		p.Put = op
	case "PATCH":
		p.Patch = op
	case "DELETE":
		p.Delete = op
	default:
		return fmt.Errorf("unsupported method %q", method)
	}
	return nil
}
