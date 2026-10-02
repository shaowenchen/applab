package openapi

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/shaowenchen/applab/internal/client"
)

// Schemas are derived from the Go types in internal/client rather than written
// out by hand, and the reason is the one that decides most of this codebase: a
// second copy of a shape is a copy that goes stale. client.App is what the CLI
// decodes every app response into, so it is the shape the API is known to
// produce; describing it again here would be describing something that only
// looks the same until someone adds a field.
//
// Reflection covers the wire types. The request bodies are the exception — their
// types are unexported in internal/api — so those are declared in registry.go
// next to the operations that use them.

// reflectSchema turns a Go type into a JSON Schema, registering a component for
// every named struct it meets.
//
// The type is walked through pointers, slices and maps so that a field declared
// `*int32`, `[]Commit` or `map[string]PodUsage` describes the value underneath it
// rather than the container.
func (b *builder) reflectSchema(t reflect.Type) *Schema {
	switch t.Kind() {
	case reflect.Pointer:
		// A pointer is an optional or nullable value. nullability is recorded so
		// a generated client type-checks `"replicas": null` the way the API can
		// actually answer it.
		inner := b.reflectSchema(t.Elem())
		clone := *inner
		clone.Nullable = true
		return &clone

	case reflect.Slice:
		return &Schema{Type: "array", Items: b.reflectSchema(t.Elem())}

	case reflect.Map:
		return &Schema{Type: "object", AdditionalProperties: b.reflectSchema(t.Elem())}

	case reflect.String:
		return &Schema{Type: "string"}

	case reflect.Bool:
		return &Schema{Type: "boolean"}

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		schema := &Schema{Type: "integer"}
		if t.Kind() == reflect.Int32 {
			schema.Format = "int32"
		} else if t.Kind() == reflect.Int64 {
			schema.Format = "int64"
		}
		return schema

	case reflect.Float32, reflect.Float64:
		return &Schema{Type: "number"}

	case reflect.Struct:
		// time.Time is JSON-marshalled as an RFC 3339 string, not as an object, so
		// it is the one struct that is not a component.
		if t == reflect.TypeOf(time.Time{}) {
			return &Schema{Type: "string", Format: "date-time"}
		}
		return b.component(t)

	case reflect.Interface:
		return &Schema{}

	default:
		return &Schema{}
	}
}

// component registers a named struct and returns a reference to it.
//
// The name comes from the Go type, so the component and the type cannot drift:
// client.Build is `Build` in the document, always.
func (b *builder) component(t reflect.Type) *Schema {
	name := t.Name()
	if name == "" {
		return &Schema{Type: "object"}
	}

	if _, done := b.schemas[name]; done {
		// Already built, or being built — a recursive type is a reference, not a
		// loop. client.Build holds a *Pod and client.Pod holds nothing back, but
		// the guard costs nothing and makes a future cycle safe.
		return ref(name)
	}

	// Registered before the fields are walked so a self-referential type refers to
	// itself instead of recursing forever.
	b.schemas[name] = &Schema{Type: "object", Properties: map[string]*Schema{}}

	schema := &Schema{Type: "object", Properties: map[string]*Schema{}}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		jsonName, opts, _ := strings.Cut(tag, ",")
		if jsonName == "" {
			jsonName = field.Name
		}
		schema.Properties[jsonName] = b.reflectSchema(field.Type)
		// A field with no `omitempty` is always present in the API's own answer,
		// so it is required. This is the same signal the encoder uses, which is
		// what keeps the two from disagreeing.
		if !strings.Contains(opts, "omitempty") {
			schema.Required = append(schema.Required, jsonName)
		}
	}
	sort.Strings(schema.Required)

	b.schemas[name] = schema
	return ref(name)
}

// handWrittenSchemas are the shapes the reflection above cannot reach: request
// bodies whose types are unexported in internal/api, and a few responses written
// as ad-hoc maps with no type at all. Each is named so registry.go can reference
// it.
//
// The request bodies the CLI itself sends — CreateAppRequest, UpdateAppRequest,
// ResourcesRequest, RegisterServerRequest — are deliberately NOT here: those are
// exported in internal/client and are reflected from the real types, so a field
// added to one appears in the document without an edit here.
func handWrittenSchemas() map[string]*Schema {
	str := func() *Schema { return &Schema{Type: "string"} }
	return map[string]*Schema{
		"SetEnvRequest": {
			Type: "object",
			Properties: map[string]*Schema{
				"env": {Type: "object", AdditionalProperties: &Schema{Type: "string", Nullable: true}},
			},
			Required: []string{"env"},
		},
		"SetSecretsRequest": {
			Type: "object",
			Properties: map[string]*Schema{
				"secrets": {Type: "object", AdditionalProperties: &Schema{Type: "string", Nullable: true}},
			},
			Required: []string{"secrets"},
		},
		"SwitchBranchRequest": {
			Type:       "object",
			Properties: map[string]*Schema{"branch": str()},
			Required:   []string{"branch"},
		},
		"BuildRequest": {
			Type: "object",
			Properties: map[string]*Schema{
				"commit_sha": str(),
				"branch":     str(),
			},
		},
		"DeployRequest": {
			Type: "object",
			Properties: map[string]*Schema{
				"commit_sha": str(),
				"build":      {Type: "boolean"},
			},
		},
		"RollbackRequest": {
			Type:       "object",
			Properties: map[string]*Schema{"commit_sha": str()},
			Required:   []string{"commit_sha"},
		},
		"ChunkedUploadStartRequest": {
			Type: "object",
			Properties: map[string]*Schema{
				"total":      {Type: "integer"},
				"chunk_size": {Type: "integer", Format: "int64"},
				"message":    str(),
			},
			Required: []string{"total", "chunk_size"},
		},
		"ChunkedUploadStartResponse": {
			Type: "object",
			Properties: map[string]*Schema{
				"upload_id":  str(),
				"total":      {Type: "integer"},
				"chunk_size": {Type: "integer", Format: "int64"},
				"parts_url":  str(),
			},
		},
		"UploadPartResponse": {
			Type: "object",
			Properties: map[string]*Schema{
				"upload_id": str(),
				"index":     {Type: "integer"},
				"size":      {Type: "integer", Format: "int64"},
			},
		},
		"AgentFilesResponse": {
			Type: "object",
			Properties: map[string]*Schema{
				"files": {Type: "array", Items: &Schema{Ref: "#/components/schemas/AgentFile"}},
			},
		},
		"AgentFile": {
			Type: "object",
			Properties: map[string]*Schema{
				"name": str(),
				"url":  str(),
			},
		},
		"BootstrapFilesResponse": {
			Type: "object",
			Properties: map[string]*Schema{
				"files": {Type: "array", Items: &Schema{Ref: "#/components/schemas/AgentFile"}},
			},
		},
		"RestartResponse": {
			Type: "object",
			Properties: map[string]*Schema{
				"app":       str(),
				"restarted": {Type: "boolean"},
			},
		},
		"StopResponse": {
			Type: "object",
			Properties: map[string]*Schema{
				"app":     {Ref: "#/components/schemas/App"},
				"stopped": {Type: "boolean"},
			},
		},
		"HealthResponse": {
			Type: "object",
			Properties: map[string]*Schema{
				"status": str(),
				"time":   {Type: "string", Format: "date-time"},
			},
		},
		"DeletedResponse": {
			Type: "object",
			Properties: map[string]*Schema{
				"id":          str(),
				"deleted":     {Type: "boolean"},
				"keep_source": {Type: "boolean"},
			},
		},
		"RemovedResponse": {
			Type: "object",
			Properties: map[string]*Schema{
				"id":      str(),
				"removed": {Type: "boolean"},
			},
		},
		"VersionResponse": {
			Type: "object",
			Properties: map[string]*Schema{
				"version":    str(),
				"commit":     str(),
				"build_time": str(),
				"go_version": str(),
			},
		},
		"Error": errorSchema(),
		"DescribeResponse": {
			// The one large response with no exported type. It is described at
			// the level a generated client needs — the fields an agent reads —
			// and `endpoints` is typed, because that is the part a client most
			// plausibly walks.
			Type: "object",
			Properties: map[string]*Schema{
				"summary":    str(),
				"api":        {Ref: "#/components/schemas/DescribeAPI"},
				"deployment": {Ref: "#/components/schemas/Config"},
				"build":      {Type: "object", Properties: map[string]*Schema{"enabled": {Type: "boolean"}, "registry": str()}},
				"deploy":     {Type: "object", Properties: map[string]*Schema{"enabled": {Type: "boolean"}, "gateway": str(), "base_domain": str(), "path_prefix": str(), "domain_template": str()}},
				"access":     {Type: "object", Properties: map[string]*Schema{"tier": str(), "app": str(), "reach": str()}},
				"apps":       {Type: "array", Items: &Schema{Ref: "#/components/schemas/App"}},
				"how_to":     {Type: "object", Properties: map[string]*Schema{"push": str(), "logs": str(), "clone": str(), "http": {Type: "object", AdditionalProperties: &Schema{Type: "string"}}}},
			},
		},
		"DescribeAPI": {
			Type: "object",
			Properties: map[string]*Schema{
				"version":     str(),
				"api_version": str(),
				"base_url":    str(),
				"auth_header": str(),
				"endpoints":   {Type: "array", Items: &Schema{Ref: "#/components/schemas/Endpoint"}},
			},
		},
		"Endpoint": {
			Type: "object",
			Properties: map[string]*Schema{
				"method": str(),
				"path":   str(),
				"key":    str(),
				"doc":    str(),
			},
		},
	}
}

// errorSchema is the failure envelope every JSON route answers with.
func errorSchema() *Schema {
	return &Schema{
		Type: "object",
		Properties: map[string]*Schema{
			"error":     {Type: "string"},
			"retryable": {Type: "boolean"},
		},
	}
}

// clientSchemaTypes names every exported internal/client type that becomes a
// component, so the document's schemas are a known set a test can check rather
// than whatever reflection happened to reach.
//
// The Request types are here rather than among the hand-written schemas on
// purpose: they are the bodies the CLI actually sends, so reflecting them means
// a field added to one is in the document without an edit here. Only the request
// bodies with no exported type are declared by hand.
var clientSchemaTypes = []any{
	client.Config{},
	client.Overview{},
	client.AppKey{},
	client.Branches{},
	client.AppConfig{},
	client.App{},
	client.AppResources{},
	client.AppUsage{},
	client.PodUsage{},
	client.UploadResult{},
	client.Commit{},
	client.CommitList{},
	client.Build{},
	client.DeployResult{},
	client.Status{},
	client.Pod{},
	client.PodList{},
	client.Event{},
	client.EventList{},
	client.Diagnosis{},
	client.Server{},
	client.CreateAppRequest{},
	client.UpdateAppRequest{},
	client.ResourcesRequest{},
	client.RegisterServerRequest{},
}

// componentName is the schema name a Go value's type maps to, so a caller can
// reference it without repeating the type name.
func componentName(v any) string {
	t := reflect.TypeOf(v)
	if t == nil {
		panic("openapi: componentName of a nil value")
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Name() == "" {
		panic(fmt.Sprintf("openapi: %s has no name and cannot be a component", t))
	}
	return t.Name()
}
