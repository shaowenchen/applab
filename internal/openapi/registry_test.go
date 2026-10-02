package openapi_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/openapi"
)

// realRoutes is the server's own route table, in the shape the generator takes.
//
// The tests read it from internal/api rather than declaring their own, because a
// test with its own route list is a test of that list. This is the table the
// server serves, so a route added to the server is one these tests see.
func realRoutes() []openapi.Route {
	table := (&api.Server{}).RouteTable()
	out := make([]openapi.Route, 0, len(table))
	for _, r := range table {
		out = append(out, openapi.Route{Method: r.Method, Path: r.Path, Tier: r.Tier, Doc: r.Doc})
	}
	return out
}

// TestTheRegistryCoversEveryRoute is the check that keeps the specification
// complete.
//
// The document is built from two sources — the route table and this package's
// registry — and the failure mode of two sources is that a route is added to one
// and not the other. A route with no registry entry is a hole in the generated
// client: the SDK silently has no method for it. This makes that a build failure
// that names the route.
func TestTheRegistryCoversEveryRoute(t *testing.T) {
	routes := realRoutes()
	ops := openapi.Operations()

	routeKeys := map[string]bool{}
	for _, r := range routes {
		routeKeys[r.Method+" "+r.Path] = true
	}

	var missing, extra []string
	for key := range routeKeys {
		if _, ok := ops[key]; !ok {
			missing = append(missing, key)
		}
	}
	for key := range ops {
		if !routeKeys[key] {
			extra = append(extra, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("these routes have no entry in Operations(), so the generated SDK cannot call them:\n  %s\n"+
			"add one — the registry is what says a route's request body, response shape and query parameters",
			strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("Operations() describes routes that do not exist:\n  %s\n"+
			"remove them, or the document promises a method that answers 404", strings.Join(extra, "\n  "))
	}
}

// TestEveryOperationIsFullySpecified checks each entry names what the builder
// needs, so a half-written entry fails here rather than as a nil in the document.
func TestEveryOperationIsFullySpecified(t *testing.T) {
	for key, spec := range openapi.Operations() {
		if spec.ID == "" {
			t.Errorf("%s: no operationId", key)
		}
		if spec.Tag == "" {
			t.Errorf("%s: no tag", key)
		}
		if spec.Status != 200 && spec.Status != 201 && spec.Status != 202 {
			t.Errorf("%s: status %d is not a success code this API uses", key, spec.Status)
		}
		if spec.Kind == openapi.EnvelopeJSON && spec.Response == "" {
			t.Errorf("%s: an envelope response needs a schema name", key)
		}
		// BareJSON carries a schema too; only the text kinds carry none.
		text := spec.Kind == openapi.TextPlain || spec.Kind == openapi.TextPlainStream
		if text && spec.Response != "" {
			t.Errorf("%s: a %s response carries no schema, but Response is set to %q", key, "text", spec.Response)
		}
	}
}

// TestIdentifiersAreUnique keeps operationIds distinct: a generator turns them
// into method names, and two operations sharing one would collapse into one.
func TestIdentifiersAreUnique(t *testing.T) {
	seen := map[string]string{}
	for key, spec := range openapi.Operations() {
		if prev, ok := seen[spec.ID]; ok {
			t.Errorf("operationId %q is used by both %s and %s", spec.ID, prev, key)
		}
		seen[spec.ID] = key
	}
}

// TestEveryReferencedSchemaExists checks every $ref resolves, so a typo in a
// schema name fails here instead of producing a document the generator rejects.
func TestEveryReferencedSchemaExists(t *testing.T) {
	doc, err := openapi.Build(realRoutes())
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	names := map[string]bool{}
	for name := range doc.Components.Schemas {
		names[name] = true
	}

	var walk func(s *openapi.Schema, where string)
	walk = func(s *openapi.Schema, where string) {
		if s == nil {
			return
		}
		if s.Ref != "" {
			const prefix = "#/components/schemas/"
			if !strings.HasPrefix(s.Ref, prefix) {
				t.Errorf("%s: $ref %q is not a component schema reference", where, s.Ref)
			} else if name := strings.TrimPrefix(s.Ref, prefix); !names[name] {
				t.Errorf("%s: $ref %q names no schema", where, s.Ref)
			}
		}
		walk(s.Items, where)
		walk(s.AdditionalProperties, where)
		for _, p := range s.Properties {
			walk(p, where)
		}
	}

	for path, item := range doc.Paths {
		for _, op := range []*openapi.Operation{item.Get, item.Post, item.Put, item.Patch, item.Delete} {
			if op == nil {
				continue
			}
			where := op.OperationID + " (" + path + ")"
			if op.RequestBody != nil {
				for _, m := range op.RequestBody.Content {
					walk(m.Schema, where)
				}
			}
			for _, resp := range op.Responses {
				for _, m := range resp.Content {
					walk(m.Schema, where)
				}
			}
		}
	}
}

// TestOpenRoutesCarryNoSecurity checks the credential tier reaches the document
// the way the server enforces it: a route whose tier is "none" overrides the
// global bearer requirement with an empty list, and every other route inherits it.
func TestOpenRoutesCarryNoSecurity(t *testing.T) {
	doc, err := openapi.Build(realRoutes())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(doc.Security) == 0 {
		t.Fatal("the document has no global security requirement")
	}

	checked := 0
	for path, item := range doc.Paths {
		for _, op := range []*openapi.Operation{item.Get, item.Post, item.Put, item.Patch, item.Delete} {
			// The git path is hand-declared with Basic auth, so it overrides the
			// requirement for its own reason and is checked by its own test.
			if op == nil || op.OperationID == "gitSmartHTTP" {
				continue
			}
			checked++
			open := op.Tier == "none"
			// A nil list means "inherit the global requirement"; a non-nil empty
			// list means "no credential". Only the latter is right for an open
			// route, and only the former for a protected one.
			if open && op.Security == nil {
				t.Errorf("%s (%s): tier none but no security override, so a client would send a key it does not need", op.OperationID, path)
			}
			if !open && op.Security != nil {
				t.Errorf("%s (%s): tier %q but an explicit security list overrides the global requirement", op.OperationID, path, op.Tier)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no operations were checked; the walk is looking at nothing")
	}
}

// TestTheTextRoutesStream is the one property of the hard routes a schema cannot
// express: the log routes may never end, so they are marked streaming and a
// generated client has to read them incrementally.
func TestTheTextRoutesStream(t *testing.T) {
	doc, err := openapi.Build(realRoutes())
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	streaming := map[string]bool{
		"streamAppLogs":   true,
		"streamBuildLogs": true,
		"streamSelfLogs":  true,
	}
	for path, item := range doc.Paths {
		if item.Get == nil {
			continue
		}
		media, ok := item.Get.Responses["200"].Content["text/plain"]
		if !ok {
			continue
		}
		want := streaming[item.Get.OperationID]
		if media.Streaming != want {
			t.Errorf("%s (%s): streaming = %v, want %v", item.Get.OperationID, path, media.Streaming, want)
		}
	}
}
