// Package docs renders this repository's own documentation into the static site
// published beside the Helm chart.
//
// The site exists because the chart repository's root would otherwise be a bare
// directory listing — index.yaml and a .tgz — and someone who arrives there from
// `helm repo add` has nowhere to read what they are installing.
//
// It publishes one page: the chart's README, as the site's root. That reader
// came to install something, so installing, upgrading, uninstalling and the
// values reference are the whole of what they need; the repository's own README
// describes building AppLab itself and stays where its audience is.
//
// The page is generated from the markdown already in the repository rather than
// written again for the site, for the reason that decides most of this codebase:
// a second copy of a document is a copy that goes stale. The chart's README is
// the one people actually maintain, so it is the one published, and the
// generator is what has to adapt.
//
// Three consequences shape the design:
//
//   - A relative link in the source markdown points at a file — ../../LICENSE,
//     values.yaml — which is meaningless on the site. Links are therefore
//     rewritten to a resolved target: another generated page, GitHub, or the
//     repository's own raw file.
//   - A link that resolves to nothing is a dead end someone was meant to walk
//     through, so it fails the build rather than rendering as a link to a 404.
//   - The markdown uses backtick fences and pipes that the templates also use,
//     so nothing is ever interpolated into a page as raw text without being
//     escaped first.
//
// Stale output is pruned from a manifest rather than by wiping the destination,
// because the destination is also the chart repository: gh-pages holds
// index.yaml and the packaged charts, which a docs build has no business
// deleting. That pruning is also how a page this site stops publishing is
// removed — the last build's manifest names it and this one does not produce it.
package docs

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

// Site is one page to publish, and where the repository's files are read from.
type Site struct {
	// Root is the repository root. Every source path is relative to it.
	Root string

	// Pages are the documents to publish, in navigation order.
	Pages []Page

	// RepoURL is the repository's web address, used for links that leave the
	// site but have a natural home in the repository.
	RepoURL string

	// RepoBranch is the branch those links point at.
	RepoBranch string

	// Title is the site's name.
	Title string
}

// Page is one source document and where it is published.
type Page struct {
	// Source is the markdown file's path, relative to the repository root.
	Source string

	// Output is the page's path within the site, ending in ".html".
	Output string

	// Title is used in the navigation.
	Title string

	// Nav is the label shown in the sidebar, defaulting to Title.
	Nav string
}

// Result describes what a build produced, so the caller can report it without
// re-deriving anything.
type Result struct {
	// Written is every file the build created, as site-relative paths.
	Written []string
}

// manifestName records what the previous build wrote, so the next one can remove
// exactly that and nothing else.
const manifestName = ".docs-manifest"

// DefaultSite returns the pages this repository publishes.
//
// One page: the chart's README, served as the site's root. The site sits at the
// Helm repository's own address, so everyone who arrives there came through
// `helm repo add` and is deciding whether and how to install this — which makes
// installing, upgrading, uninstalling and the values reference the whole of what
// that reader needs, and makes anything else a detour. The chart README is
// already exactly that document, so it is published rather than a second one
// being written for the site.
//
// The repository's own README and the debugger's build instructions stay in the
// repository and are linked out to GitHub. They are for people working on
// AppLab, not for someone installing it.
func DefaultSite(root, repoURL, branch string) Site {
	return Site{
		Root:       root,
		RepoURL:    strings.TrimSuffix(repoURL, "/"),
		RepoBranch: branch,
		Title:      "AppLab",
		Pages: []Page{
			{
				Source: "charts/applab/README.md",
				Output: "index.html",
				Title:  "Installing the AppLab chart",
				Nav:    "Installing",
			},
		},
	}
}

// Build renders every page into dest and removes what the previous build left
// behind.
func (s Site) Build(dest string) (*Result, error) {
	if s.RepoURL == "" {
		return nil, fmt.Errorf("no repository URL given: links out of the site would have no target")
	}

	// Rendered in memory first, so a link that resolves to nothing fails before
	// anything is written. A half-published site is worse than an old one.
	rendered := make(map[string][]byte, len(s.Pages))

	// Where a link may land, keyed by every address a document might use for it:
	// the output path, and the source path it is generated from.
	//
	// Both, because a document pointing at charts/applab means "the chart
	// instructions" and the site publishes exactly that document — sending the
	// reader to GitHub for it would be technically correct and plainly not what
	// was meant.
	landing := make(map[string]string, len(s.Pages)*2)
	for _, p := range s.Pages {
		landing[p.Output] = p.Output
		landing[p.Source] = p.Output
	}

	for _, p := range s.Pages {
		body, err := s.render(p, landing)
		if err != nil {
			return nil, err
		}
		page, err := s.wrap(p, body)
		if err != nil {
			return nil, err
		}
		rendered[p.Output] = page
	}

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dest, err)
	}

	// The previous build's files, read before anything is written over them.
	previous, err := readManifest(filepath.Join(dest, manifestName))
	if err != nil {
		return nil, err
	}

	written := make([]string, 0, len(rendered)+1)
	for _, p := range s.Pages {
		target := filepath.Join(dest, filepath.FromSlash(p.Output))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", filepath.Dir(target), err)
		}
		if err := os.WriteFile(target, rendered[p.Output], 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", target, err)
		}
		written = append(written, p.Output)
	}

	// A page this build no longer produces is a page the site no longer has.
	// Only files a previous build claimed are removed: the directory is also the
	// chart repository, and index.yaml lives there.
	mine := make(map[string]bool, len(rendered))
	for _, p := range s.Pages {
		mine[p.Output] = true
	}
	var removed []string
	for _, old := range previous {
		if mine[old] {
			continue
		}
		if err := os.Remove(filepath.Join(dest, filepath.FromSlash(old))); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove stale %s: %w", old, err)
		}
		removed = append(removed, old)
	}

	written = append(written, manifestName)
	if err := os.WriteFile(filepath.Join(dest, manifestName), []byte(strings.Join(written, "\n")+"\n"), 0o644); err != nil {
		return nil, fmt.Errorf("write %s: %w", manifestName, err)
	}

	return &Result{Written: written}, nil
}

// render converts one source document to an HTML fragment.
func (s Site) render(p Page, landing map[string]string) (template.HTML, error) {
	source, err := os.ReadFile(filepath.Join(s.Root, filepath.FromSlash(p.Source)))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", p.Source, err)
	}

	var b bytes.Buffer
	if err := markdown.Convert([]byte(source), &b); err != nil {
		return "", fmt.Errorf("render %s: %w", p.Source, err)
	}

	out, err := s.rewriteLinks(b.String(), p, landing)
	if err != nil {
		return "", err
	}
	return template.HTML(out), nil
}

var markdown = goldmark.New(
	// Tables and strikethrough are not CommonMark, and the documents use tables
	// heavily — every values reference in the chart README is one. Without this
	// they render as a paragraph of pipe characters, which looks like the
	// markdown failed to parse rather than like a missing extension.
	goldmark.WithExtensions(extension.GFM),
	// Heading ids, without which every in-page link in a published document is
	// dead. The chart README links to its own sections — "#keys",
	// "#1-a-registry-..." — and those anchors are written for GitHub, which
	// generates ids from heading text. Without this the site rendered the same
	// headings with no id at all, so every one of those links scrolled nowhere:
	// a reader following "see Keys" stayed exactly where they were. The scheme
	// below is goldmark's, which matches GitHub's for the headings these
	// documents use.
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(
		// Source documents are trusted — they are this repository's own — so
		// goldmark's unsafe mode is what lets a raw HTML block through if one is
		// ever written. Nothing in the current documents uses it.
		gmhtml.WithUnsafe(),
	),
)

// rewriteLinks resolves every repository-relative link to something that works
// on the site, and fails on one that would not.
//
// The rule is that a link must resolve, and its target decides how:
//
//   - another published page, linked directly;
//   - a directory holding a published page, linked to that page;
//   - anything else in the repository — a license, a values file — linked to
//     GitHub, where it is browsable and always current.
//
// Silence is not an option: a link that resolves to none of those is a dead end
// in a document someone is reading, and the person who can fix it is the one
// writing the markdown, not the one who finds the 404.
func (s Site) rewriteLinks(html string, p Page, landing map[string]string) (string, error) {
	// Literal since LinkTargetsHTML returns the resolved address and the text
	// already in the document, so a relative address is identifiable as one.
	const (
		attrStart = `href="`
		attrEnd   = `"`
	)

	sourceDir := path.Dir(p.Source)
	var out strings.Builder
	rest := html

	var unresolved []string
	for {
		i := strings.Index(rest, attrStart)
		if i < 0 {
			out.WriteString(rest)
			break
		}
		out.WriteString(rest[:i+len(attrStart)])
		rest = rest[i+len(attrStart):]

		j := strings.Index(rest, attrEnd)
		if j < 0 {
			out.WriteString(rest)
			break
		}
		href := rest[:j]
		rest = rest[j:]

		resolved, err := s.resolveLink(href, sourceDir, landing)
		if err != nil {
			unresolved = append(unresolved, err.Error())
			// Left as written rather than dropped, so the error names the link
			// and the page still renders for whoever is fixing it.
			resolved = href
		}
		out.WriteString(resolved)
	}

	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		return "", fmt.Errorf("%s has links that would be dead on the site:\n  %s\n"+
			"point them at a published page, or at a file that exists in the repository",
			p.Source, strings.Join(unresolved, "\n  "))
	}
	return out.String(), nil
}

// resolveLink turns one href into an address that works from the page being
// rendered.
func (s Site) resolveLink(href, sourceDir string, landing map[string]string) (string, error) {
	// Anything with a scheme, or a bare fragment, is not this function's
	// business: it already points somewhere, or nowhere by design.
	if href == "" || strings.HasPrefix(href, "#") ||
		strings.Contains(href, "://") || strings.HasPrefix(href, "mailto:") {
		return href, nil
	}

	target := href
	if !strings.HasPrefix(target, "/") {
		target = path.Join(sourceDir, target)
	}
	target = strings.TrimPrefix(path.Clean(target), "/")

	// A published page, whether the link names its output or the document it is
	// generated from.
	if out, ok := landing[target]; ok {
		return out, nil
	}
	if out, ok := landing[target+"/index.html"]; ok {
		return out, nil
	}

	info, statErr := os.Stat(filepath.Join(s.Root, filepath.FromSlash(target)))
	if statErr != nil {
		return href, fmt.Errorf("%q: no such file %s in the repository", href, target)
	}

	// A directory holding a published page, linked to that page. The
	// repository's own README points at charts/applab, which is a directory —
	// on the site there is no listing to land on, and the chart's README is
	// plainly what it meant.
	if info.IsDir() {
		page := s.pageUnder(target, landing)
		if page == "" {
			return href, fmt.Errorf("%q: %s is a directory with no published page", href, target)
		}
		return page, nil
	}

	return s.repoLink(target), nil
}

// pageUnder returns the output of a published page whose source is inside dir,
// preferring the one directly in it.
func (s Site) pageUnder(dir string, landing map[string]string) string {
	for _, p := range s.Pages {
		if path.Dir(p.Source) == dir {
			return landing[p.Source]
		}
	}
	for _, p := range s.Pages {
		if strings.HasPrefix(p.Source, dir+"/") {
			return landing[p.Source]
		}
	}
	return ""
}

// repoLink is where a repository file is browsable.
func (s Site) repoLink(target string) string {
	return fmt.Sprintf("%s/blob/%s/%s", s.RepoURL, s.RepoBranch, target)
}

// wrap puts a rendered document into the site's page shell.
func (s Site) wrap(p Page, body template.HTML) ([]byte, error) {
	nav := p.Nav
	if nav == "" {
		nav = p.Title
	}

	data := struct {
		Title   string
		Nav     string
		Body    template.HTML
		Pages   []Page
		RepoURL string
		Source  string
		BlobURL string
	}{
		Title:   p.Title,
		Nav:     nav,
		Body:    body,
		Pages:   s.Pages,
		RepoURL: s.RepoURL,
		Source:  p.Source,
		BlobURL: s.repoLink(p.Source),
	}

	var b bytes.Buffer
	if err := pageTemplate.Execute(&b, data); err != nil {
		return nil, fmt.Errorf("render page %s: %w", p.Output, err)
	}
	return b.Bytes(), nil
}

// readManifest returns the files a previous build recorded, or nothing when
// there was none — a first build into a directory that already holds charts.
func readManifest(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}
