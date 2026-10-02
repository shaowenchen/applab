// Package openapi renders AppLab's route table as an OpenAPI 3.1 document.
//
// It exists because of the SDKs: openapi-generator produces the Python and
// TypeScript clients from a specification, and a specification has to be a file.
// That is a departure from how the API described itself before — GET
// /api/v1/describe is generated on every request from the running route table,
// precisely so that no copy of it can go stale — and the departure is worth
// naming rather than hiding: api/openapi.yaml IS a copy, and what keeps it
// honest is not care but the drift test in this package's test files, which
// regenerates the document and compares it byte for byte with the committed one.
// If the two ever disagree the build fails and says to run `make gen-openapi`.
//
// # What is generated and what is written by hand
//
// The routes come from the table (api.RouteTable) and the schemas come from the
// Go types in internal/client by reflection, so neither can drift from the thing
// it describes: a field added to client.App appears in the App schema without
// anyone remembering to add it here.
//
// What a table and a type cannot express is written out in registry.go — which
// query parameters a route takes, whether its body is a JSON envelope, a bare
// JSON object or a text stream, and which schema its 2xx response carries. That
// file is the one place a new route must be added to by hand, and a test asserts
// it covers exactly the route table: a route with no registry entry fails the
// build by name.
//
// # Determinism
//
// The document is a pure function of the route table and the registry: no
// version from buildinfo, no timestamp, no map iteration order (yaml.v3 sorts
// map keys, and every map here is either sorted by it or built from ordered
// structs). Two runs on the same commit produce the same bytes, which is what
// makes the drift check meaningful rather than noisy.
package openapi

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Document is an OpenAPI 3.1 document.
//
// The fields are ordered as the specification's own examples are, because yaml.v3
// writes a struct in field order and a reader expects `info` near the top.
type Document struct {
	OpenAPI    string                `yaml:"openapi"`
	Info       Info                  `yaml:"info"`
	Servers    []Server              `yaml:"servers,omitempty"`
	Security   []map[string][]string `yaml:"security,omitempty"`
	Tags       []Tag                 `yaml:"tags,omitempty"`
	Paths      map[string]PathItem   `yaml:"paths"`
	Components Components            `yaml:"components"`
}

// Info is the document's self-description.
type Info struct {
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
	Version     string `yaml:"version"`
}

// Server is a base URL placeholder. It is relative so the generated client is
// pointed at whatever deployment the caller configures, rather than at one baked
// into the document.
type Server struct {
	URL         string `yaml:"url"`
	Description string `yaml:"description,omitempty"`
}

// Tag groups operations in generated documentation.
type Tag struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
}

// PathItem is one URL path and the operations on it.
type PathItem struct {
	Get        *Operation  `yaml:"get,omitempty"`
	Post       *Operation  `yaml:"post,omitempty"`
	Put        *Operation  `yaml:"put,omitempty"`
	Patch      *Operation  `yaml:"patch,omitempty"`
	Delete     *Operation  `yaml:"delete,omitempty"`
	Parameters []Parameter `yaml:"parameters,omitempty"`
}

// Operation is one method on one path.
type Operation struct {
	OperationID string              `yaml:"operationId"`
	Summary     string              `yaml:"summary,omitempty"`
	Description string              `yaml:"description,omitempty"`
	Tags        []string            `yaml:"tags,omitempty"`
	Parameters  []Parameter         `yaml:"parameters,omitempty"`
	RequestBody *RequestBody        `yaml:"requestBody,omitempty"`
	Responses   map[string]Response `yaml:"responses"`
	// Security overrides the document default. An empty (non-nil) list means the
	// route needs no credential, which is how the open routes are expressed.
	Security []map[string][]string `yaml:"security,omitempty"`
	// Tier is AppLab's own credential classification, surfaced so a generated
	// client's documentation can describe the app-key/admin-key boundary that a
	// plain security scheme cannot.
	Tier string `yaml:"x-applab-tier,omitempty"`
}

// Parameter is a path or query parameter.
type Parameter struct {
	Name        string  `yaml:"name"`
	In          string  `yaml:"in"`
	Description string  `yaml:"description,omitempty"`
	Required    bool    `yaml:"required,omitempty"`
	Schema      *Schema `yaml:"schema"`
	Example     any     `yaml:"example,omitempty"`
}

// RequestBody is the metadata for a request body.
type RequestBody struct {
	Description string               `yaml:"description,omitempty"`
	Required    bool                 `yaml:"required,omitempty"`
	Content     map[string]MediaType `yaml:"content"`
}

// Response is the metadata for one status code.
type Response struct {
	Description string               `yaml:"description"`
	Content     map[string]MediaType `yaml:"content,omitempty"`
}

// MediaType is a content type and its schema.
type MediaType struct {
	Schema *Schema `yaml:"schema,omitempty"`
	// Streaming marks a response whose body is read incrementally and has no
	// natural end. It is a vendor extension rather than an OpenAPI feature
	// because the protocol is plain text with a `follow` parameter, not SSE.
	Streaming bool `yaml:"x-streaming,omitempty"`
}

// Components holds the reusable schemas and security schemes.
type Components struct {
	Schemas         map[string]*Schema        `yaml:"schemas"`
	SecuritySchemes map[string]SecurityScheme `yaml:"securitySchemes,omitempty"`
}

// SecurityScheme is an authentication scheme.
type SecurityScheme struct {
	Type        string `yaml:"type"`
	Scheme      string `yaml:"scheme"`
	Description string `yaml:"description,omitempty"`
}

// Schema is a JSON Schema subset — enough for what these types need, and nothing
// speculative. It is a pointer everywhere so an omitted field is absent from the
// output rather than written as a zero.
type Schema struct {
	Ref         string             `yaml:"$ref,omitempty"`
	Type        string             `yaml:"type,omitempty"`
	Format      string             `yaml:"format,omitempty"`
	Description string             `yaml:"description,omitempty"`
	Properties  map[string]*Schema `yaml:"properties,omitempty"`
	Required    []string           `yaml:"required,omitempty"`
	Items       *Schema            `yaml:"items,omitempty"`
	// AdditionalProperties is the value schema of a map. `true`/`false` are not
	// used here: every map in this API has a value type.
	AdditionalProperties *Schema `yaml:"additionalProperties,omitempty"`
	Nullable             bool    `yaml:"nullable,omitempty"`
	Example              any     `yaml:"example,omitempty"`
}

// ref builds a reference to a named component schema.
func ref(name string) *Schema { return &Schema{Ref: "#/components/schemas/" + name} }

// Marshal renders the document as YAML.
//
// It is the only way this package writes a document, so the encoding — two-space
// indent, no line wrapping — is one decision rather than one per caller, and the
// drift test compares against exactly this.
func Marshal(doc *Document) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode openapi document: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close openapi encoder: %w", err)
	}
	return buf.Bytes(), nil
}
