// Command genllms writes api/llms.txt from the server's own route table.
//
// It exists so the committed copy of the contract can never be hand-edited into
// disagreement with the routes the server actually serves. The served document
// is generated per request; this is only about the copy that lives in the
// repository, which is what a reviewer reads in a diff and what
// internal/api.TestLlmsTxtMatchesCommittedFile checks.
//
//	go run ./cmd/genllms          # write api/llms.txt
//	go run ./cmd/genllms -check   # fail if it is out of date, without writing
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/auth"
	"github.com/shaowenchen/applab/internal/config"

	"github.com/shaowenchen/applab/internal/store"
)

func main() {
	check := flag.Bool("check", false, "fail if api/llms.txt is out of date instead of rewriting it")
	flag.Parse()

	if err := run(*check); err != nil {
		fmt.Fprintf(os.Stderr, "genllms: %v\n", err)
		os.Exit(1)
	}
}

func run(check bool) error {
	// The document depends only on the route table, which is fixed at build
	// time — but building a Server needs a store, and the honest way to get one
	// is to open a real (temporary) database rather than to fake it. That keeps
	// this command from being able to generate something the server could not
	// serve.
	tmp, err := os.MkdirTemp("", "applab-genllms-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	st, err := store.Open(context.Background(), filepath.Join(tmp, "gen.db"))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	cfg := config.Default()
	cfg.Keys = []string{"placeholder"}

	doc, err := api.RenderLlmsTxt(api.New(cfg, st, auth.New(cfg.Keys)))
	if err != nil {
		return err
	}

	path := filepath.Join("api", "llms.txt")

	if check {
		current, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if string(current) != doc {
			return fmt.Errorf("%s is out of date; run `go run ./cmd/genllms`", path)
		}
		fmt.Printf("%s is up to date\n", path)
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Printf("wrote %s (%d bytes)\n", path, len(doc))
	return nil
}
