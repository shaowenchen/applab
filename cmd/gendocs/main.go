// Command gendocs renders this repository's documentation into the static site
// published alongside the Helm chart.
//
// It exists so the pages on the site are the markdown in the repository rather
// than a copy kept in step by hand — a second copy of a document is a copy that
// goes stale, and a site that disagrees with the README is worse than no site.
//
//	gendocs -dest ./pages                          # build the site
//	gendocs -dest ./pages -repo <url> -branch b    # explicit targets
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/shaowenchen/applab/internal/docs"
)

func main() {
	dest := flag.String("dest", "", "directory to write the site into (required)")
	repo := flag.String("repo", "https://github.com/shaowenchen/applab",
		"repository web address, used for links that leave the site")
	branch := flag.String("branch", "master",
		"repository branch those links should point at")
	flag.Parse()

	if err := run(*dest, *repo, *branch); err != nil {
		fmt.Fprintf(os.Stderr, "gendocs: %v\n", err)
		os.Exit(1)
	}
}

func run(dest, repo, branch string) error {
	if dest == "" {
		return fmt.Errorf("-dest is required: refusing to guess where to write a site")
	}

	site := docs.DefaultSite(".", repo, branch)

	result, err := site.Build(dest)
	if err != nil {
		return err
	}

	for _, name := range result.Written {
		fmt.Printf("  %s\n", name)
	}
	fmt.Printf("wrote %d files to %s\n", len(result.Written), dest)
	return nil
}
