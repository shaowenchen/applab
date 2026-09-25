package docs

import "html/template"

// pageTemplate is the shell every page is rendered into.
//
// The CSS is embedded rather than linked because the site is served from a
// gh-pages branch that also holds a Helm repository: a stray .css file next to
// index.yaml is one more thing for a chart repository to be confused by, and one
// request per page is one request too many for a document.
//
// The markdown is deliberately not styled to look like a marketing page. It is
// long-form technical writing with tables, code blocks and long paragraphs, and
// the layout is built around reading it: a fixed measure, a sidebar that stays
// put, and code that wraps rather than forcing the whole page to scroll.
var pageTemplate = template.Must(template.New("page").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{ .Title }}</title>
<meta name="description" content="Deploy an application to Kubernetes by uploading its source.">
<style>
:root {
  color-scheme: light dark;
  --bg: #ffffff;
  --fg: #1a1a1a;
  --muted: #6a6a6a;
  --rule: #e3e3e3;
  --accent: #0b6bcb;
  --code-bg: #f5f5f5;
  --sidebar-bg: #fafafa;
  --measure: 46rem;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #14161a;
    --fg: #e6e6e6;
    --muted: #9a9a9a;
    --rule: #2b2f36;
    --accent: #6cb2ff;
    --code-bg: #1c1f24;
    --sidebar-bg: #17191d;
  }
}
* { box-sizing: border-box; }
html { -webkit-text-size-adjust: 100%; }
body {
  margin: 0;
  background: var(--bg);
  color: var(--fg);
  font: 16px/1.65 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
}
.layout { display: flex; align-items: flex-start; min-height: 100vh; }

nav {
  flex: 0 0 15rem;
  position: sticky;
  top: 0;
  height: 100vh;
  overflow-y: auto;
  padding: 1.75rem 1.25rem;
  background: var(--sidebar-bg);
  border-right: 1px solid var(--rule);
}
nav .brand { font-weight: 650; font-size: 1.05rem; letter-spacing: -0.01em; }
nav .brand a { color: var(--fg); text-decoration: none; }
nav ul { list-style: none; margin: 1.5rem 0 0; padding: 0; }
nav li { margin: 0 0 0.15rem; }
nav a {
  display: block;
  padding: 0.3rem 0.6rem;
  border-radius: 5px;
  color: var(--muted);
  text-decoration: none;
  font-size: 0.9rem;
}
nav a:hover { color: var(--fg); background: var(--code-bg); }
nav a[aria-current="page"] { color: var(--fg); background: var(--code-bg); font-weight: 600; }
nav .repo { margin-top: 1.5rem; font-size: 0.85rem; }
nav .repo a { padding: 0; }

main { flex: 1 1 auto; min-width: 0; padding: 2.5rem 2.5rem 5rem; }
article { max-width: var(--measure); }

h1, h2, h3, h4 { line-height: 1.25; letter-spacing: -0.015em; }
h1 { font-size: 1.9rem; margin: 0 0 1.25rem; }
h2 { font-size: 1.35rem; margin: 2.5rem 0 0.9rem; padding-top: 0.75rem; border-top: 1px solid var(--rule); }
h3 { font-size: 1.1rem; margin: 2rem 0 0.7rem; }
h4 { font-size: 1rem; margin: 1.5rem 0 0.5rem; }

a { color: var(--accent); text-decoration-thickness: 1px; text-underline-offset: 2px; }
p, ul, ol { margin: 0 0 1.05rem; }
li { margin: 0.25rem 0; }

code {
  font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace;
  font-size: 0.875em;
  background: var(--code-bg);
  padding: 0.12em 0.35em;
  border-radius: 4px;
}
pre {
  background: var(--code-bg);
  border: 1px solid var(--rule);
  border-radius: 8px;
  padding: 0.9rem 1.1rem;
  overflow-x: auto;
}
pre code { background: none; padding: 0; font-size: 0.85em; }
pre.contract { max-height: none; white-space: pre-wrap; word-break: break-word; }

blockquote {
  margin: 0 0 1.05rem;
  padding: 0.1rem 0 0.1rem 1rem;
  border-left: 3px solid var(--rule);
  color: var(--muted);
}

table { border-collapse: collapse; width: 100%; margin: 0 0 1.25rem; display: block; overflow-x: auto; }
th, td { border: 1px solid var(--rule); padding: 0.45rem 0.7rem; text-align: left; vertical-align: top; }
th { background: var(--code-bg); font-weight: 600; }

hr { border: none; border-top: 1px solid var(--rule); margin: 2.5rem 0; }
img { max-width: 100%; height: auto; }

.source {
  margin-top: 3.5rem;
  padding-top: 1rem;
  border-top: 1px solid var(--rule);
  color: var(--muted);
  font-size: 0.85rem;
}

@media (max-width: 46rem) {
  .layout { flex-direction: column; }
  nav { position: static; height: auto; width: 100%; flex: none; border-right: none; border-bottom: 1px solid var(--rule); }
  nav ul { margin-top: 1rem; }
  main { padding: 1.5rem 1.25rem 3.5rem; }
}
</style>
</head>
<body>
<div class="layout">
  <nav>
    <div class="brand"><a href="index.html">AppLab</a></div>
    {{- if gt (len .Pages) 1 }}
    <ul>
      {{- range .Pages }}
      <li><a href="{{ .Output }}"{{ if eq .Nav $.Nav }} aria-current="page"{{ end }}>{{ .Nav }}</a></li>
      {{- end }}
    </ul>
    {{- end }}
    <div class="repo"><a href="{{ .RepoURL }}">Source on GitHub</a></div>
  </nav>
  <main>
    <article>
{{ .Body }}
      <p class="source">This page is rendered from <a href="{{ .BlobURL }}"><code>{{ .Source }}</code></a> in the repository.</p>
    </article>
  </main>
</div>
</body>
</html>
`))
