package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testRepo writes a small repository to a temporary directory.
func testRepo(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	files := map[string]string{
		"README.md": `# applab

See [the chart](charts/applab) and [the API](api/llms.txt).

Also [the license](LICENSE) and [values](charts/applab/values.yaml).

An [external link](https://example.com) and [a fragment](#section).
`,
		"charts/applab/README.md":   "# The chart\n\nBack to [the overview](../../README.md) and [the values](values.yaml).\n",
		"charts/applab/values.yaml": "replicaCount: 1\n",
		"api/llms.txt":              "# AppLab\n\nPlain text contract.\n",
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
			{Source: "api/llms.txt", Output: "api.html", Title: "API", Nav: "API"},
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

	for _, name := range []string{"index.html", "chart.html", "api.html"} {
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
		{`href="api.html"`, "a link to a published source document links to its page"},
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
	for _, unwanted := range []string{`href="charts/applab/"`, `href="charts/applab"`, `href="api/llms.txt"`, `href="LICENSE"`} {
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

// TestTheContractIsNotParsedAsMarkdown asserts llms.txt is published verbatim.
//
// It is plain text by design — indented code blocks and all — and running it
// through a markdown parser would reflow exactly the formatting that makes it
// readable as a contract.
func TestTheContractIsNotParsedAsMarkdown(t *testing.T) {
	root := testRepo(t)
	contract := "# AppLab\n\n    Authorization: Bearer <key>\n\n<p>not html</p>\n"
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash("api/llms.txt")), []byte(contract), 0o644); err != nil {
		t.Fatalf("write contract: %v", err)
	}

	dest := t.TempDir()
	if _, err := testSite(t, root).Build(dest); err != nil {
		t.Fatalf("Build: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dest, "api.html"))
	if err != nil {
		t.Fatalf("read api: %v", err)
	}
	html := string(body)

	if !strings.Contains(html, `<pre class="contract">`) {
		t.Error("the contract was not published as preformatted text")
	}
	if !strings.Contains(html, "    Authorization: Bearer") {
		t.Error("the contract's indentation was lost")
	}
	// Escaped, not interpreted: it is a document, not a page.
	if !strings.Contains(html, "&lt;p&gt;not html&lt;/p&gt;") {
		t.Error("the contract's markup was not escaped")
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

	if _, err := os.Stat(filepath.Join(dest, "api.html")); !os.IsNotExist(err) {
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

	// The published set is asserted rather than the count, so removing a page
	// from DefaultSite is caught here instead of on the site.
	for _, want := range []string{"index.html", "chart.html", "api.html"} {
		if _, err := os.Stat(filepath.Join(dest, want)); err != nil {
			t.Errorf("%s was not published: %v", want, err)
		}
	}
	t.Logf("published %v", result.Written)
}
