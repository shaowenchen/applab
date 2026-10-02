package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestTheCommittedSpecIsCurrent is the guard that makes api/openapi.yaml
// trustworthy.
//
// The specification is a committed copy of the route table, and a committed copy
// of anything goes stale — which is exactly why the API described itself only at
// runtime before the SDKs existed. The copy is worth having (openapi-generator
// needs a file), and this is what keeps it honest: regenerate from the table and
// compare. If they differ, a route changed and the document did not.
//
// It runs wherever `make check` runs and needs no Java, because it only exercises
// the Go generator. The SDK trees that openapi-generator produces from this file
// are checked in CI, where Java exists — see .github/workflows/sdk.yml.
func TestTheCommittedSpecIsCurrent(t *testing.T) {
	generated, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	path := filepath.Join("..", "..", "api", "openapi.yaml")
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v\n(regenerate it with: make gen-openapi)", path, err)
	}

	if bytes.Equal(generated, committed) {
		return
	}

	// Report the first differing line rather than dumping two thousands-line
	// documents, which is what made this kind of failure unreadable the last time
	// this repository stored a generated artifact.
	gen := bytes.Split(generated, []byte("\n"))
	got := bytes.Split(committed, []byte("\n"))
	for i := 0; i < len(gen) || i < len(got); i++ {
		var g, c []byte
		if i < len(gen) {
			g = gen[i]
		}
		if i < len(got) {
			c = got[i]
		}
		if !bytes.Equal(g, c) {
			t.Fatalf("api/openapi.yaml is out of date with the route table.\n"+
				"first difference at line %d:\n  committed: %s\n  generated: %s\n\n"+
				"run: make gen-openapi",
				i+1, c, g)
		}
	}
	t.Fatal("api/openapi.yaml differs from the generated document; run: make gen-openapi")
}
