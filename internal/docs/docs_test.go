package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// testRepo writes a small repository to a temporary directory.
func testRepo(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	files := map[string]string{
		"README.md": `# applab

See [the chart](charts/applab) and [the debugger](debugger/README.md).

Also [the license](LICENSE) and [values](charts/applab/values.yaml).

An [external link](https://example.com) and [a fragment](#section).
`,
		"charts/applab/README.md":   "# The chart\n\nBack to [the overview](../../README.md) and [the values](values.yaml).\n",
		"charts/applab/values.yaml": "replicaCount: 1\n",
		"debugger/README.md":        "# The debugger\n\nPlain text contract.\n",
		"LICENSE":                   "MIT\n",
		"docs/internal-note.md":     "not published\n",
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

func testSite(t *testing.T, root string) Site {
	t.Helper()
	return Site{
		Root:       root,
		RepoURL:    "https://github.com/example/applab",
		RepoBranch: "main",
		Title:      "applab",
		Pages: []Page{
			{Source: "README.md", Output: "index.html", Title: "Overview", Nav: "Overview"},
			{Source: "charts/applab/README.md", Output: "chart.html", Title: "Chart", Nav: "Installing"},
			{Source: "debugger/README.md", Output: "debugger.html", Title: "Debugger", Nav: "Debugger"},
		},
	}
}

func TestBuildProducesEveryPage(t *testing.T) {
	root := testRepo(t)
	dest := t.TempDir()

	result, err := testSite(t, root).Build(dest)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(result.Written) != 4 {
		t.Errorf("wrote %v, want three pages and a manifest", result.Written)
	}

	for _, name := range []string{"index.html", "chart.html", "debugger.html"} {
		body, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil {
			t.Fatalf("%s was not written: %v", name, err)
		}
		if !strings.Contains(string(body), "<title>") {
			t.Errorf("%s has no title", name)
		}
	}
}

// TestLinksResolveToSomethingReal is the reason links are rewritten at all: a
// link in the source markdown names a repository file, and on the site that path
// is not where anything lives.
func TestLinksResolveToSomethingReal(t *testing.T) {
	root := testRepo(t)
	dest := t.TempDir()

	if _, err := testSite(t, root).Build(dest); err != nil {
		t.Fatalf("Build: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dest, "index.html"))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	html := string(body)

	for _, want := range []struct{ href, why string }{
		{`href="chart.html"`, "a directory holding a published page links to that page"},
		{`href="debugger.html"`, "a directory holding a published page links to that page"},
		{`href="https://github.com/example/applab/blob/main/LICENSE"`, "a repository file links to where it is readable"},
		{`href="https://github.com/example/applab/blob/main/charts/applab/values.yaml"`, "and so does one in a subdirectory, resolved from the page that linked it"},
		{`href="https://example.com"`, "an external link is left alone"},
		{`href="#section"`, "a fragment is left alone"},
	} {
		if !strings.Contains(html, want.href) {
			t.Errorf("missing %s: %s", want.href, want.why)
		}
	}

	// The paths that only exist in the repository must not survive as links:
	// on the site they are 404s.
	for _, unwanted := range []string{`href="charts/applab/"`, `href="charts/applab"`, `href="debugger/"`, `href="debugger"`, `href="LICENSE"`} {
		if strings.Contains(html, unwanted) {
			t.Errorf("the page still links to %s, which is not a path the site serves", unwanted)
		}
	}
}

// TestRelativeLinksResolveFromTheirOwnPage asserts a link is resolved against
// the document that contains it, not against the repository root.
func TestRelativeLinksResolveFromTheirOwnPage(t *testing.T) {
	root := testRepo(t)
	dest := t.TempDir()

	if _, err := testSite(t, root).Build(dest); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// charts/applab/README.md links to ../../README.md, which is the overview.
	// Resolved from the repository root it would be nothing at all, so this
	// fails if links are resolved against the wrong base.
	body, err := os.ReadFile(filepath.Join(dest, "chart.html"))
	if err != nil {
		t.Fatalf("read chart: %v", err)
	}
	if !strings.Contains(string(body), `href="index.html"`) {
		t.Error("../../README.md did not resolve to the overview page")
	}
	// And a link to a sibling file resolves within its own directory.
	if !strings.Contains(string(body), `href="values.yaml"`) &&
		!strings.Contains(string(body), "charts/applab/values.yaml") {
		t.Error("values.yaml did not resolve from the document that linked it")
	}
}

// TestDeadLinkFailsTheBuild is the guard that keeps the site from shipping a
// link to nowhere. The person who can fix it is the one writing the markdown, so
// it fails where they are rather than being found by a reader.
func TestDeadLinkFailsTheBuild(t *testing.T) {
	root := testRepo(t)
	// A link to a file that does not exist.
	if err := os.WriteFile(filepath.Join(root, "README.md"),
		[]byte("# applab\n\nSee [nothing](does-not-exist.md).\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}

	dest := t.TempDir()
	_, err := testSite(t, root).Build(dest)
	if err == nil {
		t.Fatal("a link to a file that does not exist should fail the build")
	}
	if !strings.Contains(err.Error(), "does-not-exist.md") {
		t.Errorf("the error does not name the link: %v", err)
	}
	if !strings.Contains(err.Error(), "README.md") {
		t.Errorf("the error does not name the document: %v", err)
	}

	// And nothing was written, so a failed build cannot leave a half-published
	// site behind.
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed build wrote %d files", len(entries))
	}
}

// TestUnpublishedDirectoryIsRefused asserts a link into a directory the site
// does not publish is an error rather than a link to a directory listing that
// does not exist.
func TestUnpublishedDirectoryIsRefused(t *testing.T) {
	root := testRepo(t)
	if err := os.WriteFile(filepath.Join(root, "README.md"),
		[]byte("# applab\n\nSee [notes](docs).\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}

	_, err := testSite(t, root).Build(t.TempDir())
	if err == nil {
		t.Fatal("a link to an unpublished directory should fail the build")
	}
}

// TestTablesRender asserts the GFM extension is on: the documents are full of
// tables, and without it they render as a paragraph of pipe characters.
func TestTablesRender(t *testing.T) {
	root := testRepo(t)
	if err := os.WriteFile(filepath.Join(root, "README.md"),
		[]byte("# applab\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}

	dest := t.TempDir()
	if _, err := testSite(t, root).Build(dest); err != nil {
		t.Fatalf("Build: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dest, "index.html"))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if !strings.Contains(string(body), "<table>") {
		t.Error("a markdown table did not render as one")
	}
	if strings.Contains(string(body), "|---|---|") {
		t.Error("the table's delimiter row was rendered as text")
	}
}

// TestStalePagesArePrunedAndChartsSurvive is the one that matters for CI: the
// destination is also the chart repository, so pruning has to remove the
// previous build's pages and nothing else.
func TestStalePagesArePrunedAndChartsSurvive(t *testing.T) {
	root := testRepo(t)
	dest := t.TempDir()

	site := testSite(t, root)
	if _, err := site.Build(dest); err != nil {
		t.Fatalf("first build: %v", err)
	}

	// The chart repository's own files, which a docs build must not touch.
	for name, content := range map[string]string{
		"index.yaml":           "apiVersion: v1\nentries: {}\n",
		"applab-0.1.0.tgz":     "not really a chart\n",
		"applab-1.0.0-dev.tgz": "nor this\n",
	} {
		if err := os.WriteFile(filepath.Join(dest, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	// A second build that no longer publishes one of the pages.
	site.Pages = site.Pages[:2]
	result, err := site.Build(dest)
	if err != nil {
		t.Fatalf("second build: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, "debugger.html")); !os.IsNotExist(err) {
		t.Error("the page no longer produced was not removed")
	}
	for _, name := range []string{"index.yaml", "applab-0.1.0.tgz", "applab-1.0.0-dev.tgz"} {
		if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
			t.Errorf("%s was removed; the chart repository shares this directory", name)
		}
	}
	for _, name := range []string{"index.html", "chart.html"} {
		if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
			t.Errorf("%s is missing after the second build", name)
		}
	}

	// A first build into a directory that already holds charts must not treat
	// them as its own leftovers either.
	_ = result
	fresh := t.TempDir()
	if err := os.WriteFile(filepath.Join(fresh, "index.yaml"), []byte("entries: {}\n"), 0o644); err != nil {
		t.Fatalf("write index.yaml: %v", err)
	}
	if _, err := site.Build(fresh); err != nil {
		t.Fatalf("build into a chart directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fresh, "index.yaml")); err != nil {
		t.Error("a first build removed a file it never wrote")
	}
}

// TestNoRepoURLIsRefused asserts the site cannot be built without knowing where
// its outbound links point, rather than emitting links to nowhere.
func TestNoRepoURLIsRefused(t *testing.T) {
	site := testSite(t, testRepo(t))
	site.RepoURL = ""

	if _, err := site.Build(t.TempDir()); err == nil {
		t.Fatal("building without a repository URL should be refused")
	}
}

// TestRealRepositoryBuilds asserts this repository's own documentation is
// publishable — the documents, the links and the page list all together.
//
// It is the check that keeps the site from breaking when a document is renamed:
// the failure surfaces here, in `make check`, rather than on the published site.
func TestRealRepositoryBuilds(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skipf("not running from the repository: %v", err)
	}

	dest := t.TempDir()
	site := DefaultSite(root, "https://github.com/shaowenchen/applab", "master")

	result, err := site.Build(dest)
	if err != nil {
		t.Fatalf("this repository's documentation does not build:\n%v", err)
	}

	// The published set is asserted rather than the count, so a change to
	// DefaultSite is caught here instead of on the site. It is one page: the
	// site is the Helm repository's address, so it is read by someone deciding
	// whether and how to install, and the chart's README is that document.
	if len(result.Written) != 2 {
		t.Errorf("published %v, want one page and a manifest", result.Written)
	}
	if _, err := os.Stat(filepath.Join(dest, "index.html")); err != nil {
		t.Errorf("index.html was not published: %v", err)
	}
	t.Logf("published %v", result.Written)
}

// TestEveryInPageLinkLandsSomewhere is the missing half of
// TestLinksResolveToSomethingReal.
//
// That test covers links that leave the site — a repository file, another page —
// and fails when one points at nothing. A link to a section of the *same* page
// was never checked, and headings were rendered without ids, so every "#keys"
// and "#installing-a-development-build" in the chart README scrolled nowhere: a
// reader following "see Keys" stayed exactly where they were, with nothing to
// say the link was dead.
//
// Checked against the rendered HTML rather than the source, because that is what
// the reader gets — the anchor has to match an id the renderer actually emitted.
func TestEveryInPageLinkLandsSomewhere(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skipf("not running from the repository: %v", err)
	}

	dest := t.TempDir()
	site := DefaultSite(root, "https://github.com/shaowenchen/applab", "master")
	if _, err := site.Build(dest); err != nil {
		t.Fatalf("build: %v", err)
	}

	for _, page := range site.Pages {
		body, err := os.ReadFile(filepath.Join(dest, page.Output))
		if err != nil {
			t.Fatalf("read %s: %v", page.Output, err)
		}
		html := string(body)

		ids := map[string]bool{}
		for _, m := range regexp.MustCompile(`id="([^"]+)"`).FindAllStringSubmatch(html, -1) {
			ids[m[1]] = true
		}

		for _, m := range regexp.MustCompile(`href="#([^"]+)"`).FindAllStringSubmatch(html, -1) {
			if !ids[m[1]] {
				t.Errorf("%s links to #%s, which is not a heading on that page; the link scrolls nowhere",
					page.Output, m[1])
			}
		}
	}
}
