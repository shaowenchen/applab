// Command genopenapi writes AppLab's OpenAPI specification.
//
// It is the only producer of api/openapi.yaml, and it is run two ways: by hand
// to accept a route change (`make gen-openapi`), and by a test that regenerates
// the document and compares it with the committed file, so the file cannot drift
// from the table it describes without the build failing.
//
// It needs no cluster, no store and no key: the route table is a slice of
// literals, so a zero api.Server enumerates everything this deployment serves.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/openapi"
)

func main() {
	out := flag.String("out", "api/openapi.yaml", "path to write the specification to")
	check := flag.Bool("check", false, "do not write; exit 1 if the file differs from the generated document")
	flag.Parse()

	doc, err := Generate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "genopenapi: %v\n", err)
		os.Exit(1)
	}

	if *check {
		existing, err := os.ReadFile(*out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "genopenapi: read %s: %v\nrun: make gen-openapi\n", *out, err)
			os.Exit(1)
		}
		if !bytes.Equal(existing, doc) {
			fmt.Fprintf(os.Stderr, "genopenapi: %s is out of date with the route table\nrun: make gen-openapi\n", *out)
			os.Exit(1)
		}
		return
	}

	if err := os.MkdirAll("api", 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "genopenapi: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, doc, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "genopenapi: write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", *out)
}

// Generate renders the specification from the live route table.
//
// Exported so a test can call it directly rather than shelling out to the binary
// — the drift check is only meaningful if it runs the same code the command does.
func Generate() ([]byte, error) {
	// routes() is a slice of literals, so a zero Server is enough to enumerate
	// the whole table. Nothing here reads a field the server would need wiring
	// for.
	routes := (&api.Server{}).RouteTable()

	out := make([]openapi.Route, 0, len(routes))
	for _, r := range routes {
		out = append(out, openapi.Route{
			Method: r.Method,
			Path:   r.Path,
			Tier:   r.Tier,
			Doc:    r.Doc,
		})
	}

	doc, err := openapi.Build(out)
	if err != nil {
		return nil, err
	}
	return openapi.Marshal(doc)
}
