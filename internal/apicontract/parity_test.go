package apicontract_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/apicontract"
	"github.com/shaowenchen/applab/internal/source"
)

// TestTheTableCoversEveryRoute is the check that makes this package worth having.
//
// It asks the server what it serves and insists every route has a row. Adding an
// endpoint and not classifying it fails here, by name, which is the whole point:
// the route that started this — the platform's events — was reachable from one
// surface and no other for weeks because nothing required anyone to decide.
func TestTheTableCoversEveryRoute(t *testing.T) {
	routes := map[string]bool{}
	for _, r := range (&api.Server{}).RouteTable() {
		routes[r.Method+" "+r.Path] = true
	}

	entries := map[string]bool{}
	for _, e := range apicontract.Table {
		if entries[e.Pattern()] {
			t.Errorf("%s appears twice in the table", e.Pattern())
		}
		entries[e.Pattern()] = true
	}

	var missing, extra []string
	for k := range routes {
		if !entries[k] {
			missing = append(missing, k)
		}
	}
	for k := range entries {
		if !routes[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("these routes are served but not in the parity table, so nothing checks who can reach them:\n  %s\n"+
			"add a row for each, saying for every surface whether it covers the route and, if not, why",
			strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("the parity table names routes the server does not serve:\n  %s", strings.Join(extra, "\n  "))
	}
}

// TestEverySurfaceIsDecided checks no cell was left blank: each route is either
// covered with evidence or exempted with a reason, never neither.
func TestEverySurfaceIsDecided(t *testing.T) {
	for _, e := range apicontract.Table {
		for _, s := range apicontract.Surfaces {
			c := e.CoverageFor(s)
			if c.Covered() && strings.TrimSpace(c.Needle) == "" {
				t.Errorf("%s: %s is neither covered nor exempt", e.Pattern(), s)
			}
			if !c.Covered() && strings.TrimSpace(c.Why) == "" {
				t.Errorf("%s: %s is exempt with no reason", e.Pattern(), s)
			}
		}
	}
}

// TestTheConsoleReachesWhatItClaims checks each console needle is really in the
// console's script.
//
// The needle is matched against the script with comments removed. Without that
// the check would pass on a path named only in a comment — the console's source
// has several, including the very route that went missing.
func TestTheConsoleReachesWhatItClaims(t *testing.T) {
	console := readConsoleScript(t)

	for _, e := range apicontract.Table {
		c := e.Console
		if !c.Covered() {
			continue
		}
		if !strings.Contains(console, c.Needle) {
			t.Errorf("%s: the console no longer contains %q, so either the console stopped calling it or this needle is stale",
				e.Pattern(), c.Needle)
		}
	}
}

// TestTheScriptReachesWhatItClaims checks each script needle against the rendered
// script — the bytes a caller actually gets, not the template.
func TestTheScriptReachesWhatItClaims(t *testing.T) {
	script := renderScript(t)

	for _, e := range apicontract.Table {
		c := e.Script
		if !c.Covered() {
			continue
		}
		if !strings.Contains(script, c.Needle) {
			t.Errorf("%s: the rendered applab.sh does not contain %q", e.Pattern(), c.Needle)
		}
	}
}

// TestTheCLIReachesWhatItClaims runs the CLI's own declaration of what it can
// reach and compares it with the table.
//
// This is the authoritative check for the CLI: it enumerates the command tree's
// routes rather than searching source, so a route marked covered here has to be
// one the CLI really offers. It is the check the CLI never had.
func TestTheCLIReachesWhatItClaims(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	out, err := exec.Command("go", "run", "./../../cmd/applab-cli", "__routes").Output()
	if err != nil {
		t.Fatalf("run `applab __routes`: %v", err)
	}

	reaches := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			reaches[line] = true
		}
	}
	if len(reaches) == 0 {
		t.Fatal("the CLI declared no routes at all")
	}

	for _, e := range apicontract.Table {
		if !e.CLI.Covered() {
			continue
		}
		if !reaches[e.Pattern()] {
			t.Errorf("%s: the table says the CLI reaches this, but `applab __routes` does not list it — "+
				"either the command was removed or the table is wrong", e.Pattern())
		}
	}

	// And the other direction: a route the CLI reaches that the table does not
	// mark covered is a table that has fallen behind the CLI.
	for pattern := range reaches {
		entry := findEntry(pattern)
		if entry == nil {
			t.Errorf("the CLI reaches %s, which is not in the table at all", pattern)
			continue
		}
		if !entry.CLI.Covered() {
			t.Errorf("the CLI reaches %s, but the table exempts it from the CLI: %s", pattern, entry.CLI.Why)
		}
	}
}

// TestTheSDKReachesEverything checks the specification contains every route.
//
// The SDK surface has no exemptions: a generated client is meant to cover the
// whole API, and every route a caller cannot reach is a route the SDK is missing.
// A route absent from api/openapi.yaml fails here — which is also what catches a
// route added to the table but never regenerated into the document.
func TestTheSDKReachesEverything(t *testing.T) {
	path := filepath.Join("..", "..", "api", "openapi.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	for _, e := range apicontract.Table {
		if !e.SDK.Covered() {
			t.Errorf("%s: the SDK is exempt from a route; a generated client covers the whole API", e.Pattern())
			continue
		}
		item, ok := doc.Paths[e.Path]
		if !ok {
			t.Errorf("%s: api/openapi.yaml has no path %q — run `make gen-openapi`", e.Pattern(), e.Path)
			continue
		}
		if _, ok := item[strings.ToLower(e.Method)]; !ok {
			t.Errorf("%s: api/openapi.yaml has %q but no %s operation", e.Pattern(), e.Path, e.Method)
		}
	}
}

// TestExemptionsAreReported prints the exemptions so a reviewer sees them.
//
// It never fails. Exemptions are legitimate — a browser cannot tar a directory —
// but a growing set of them is how "the surfaces agree" quietly stops being true,
// and the count is the cheapest signal that it is.
func TestExemptionsAreReported(t *testing.T) {
	counts := map[apicontract.Surface]int{}
	for _, e := range apicontract.Table {
		for _, s := range apicontract.Surfaces {
			if !e.CoverageFor(s).Covered() {
				counts[s]++
			}
		}
	}
	t.Logf("%d routes; exemptions by surface: console %d, cli %d, script %d, sdk %d",
		len(apicontract.Table), counts[apicontract.Console], counts[apicontract.CLI], counts[apicontract.Script], counts[apicontract.SDK])
	for _, f := range apicontract.Findings {
		t.Logf("finding: %s", f)
	}
}

// readConsoleScript returns the console's JavaScript with comments stripped.
func readConsoleScript(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "console", "static", "index.html"))
	if err != nil {
		t.Fatalf("read the console: %v", err)
	}
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("the console has no <script> block")
	}
	js := string(m[1])
	// Block comments, then line comments that are not part of a URL scheme. The
	// console's comments name real paths, which is exactly the false positive this
	// exists to prevent.
	js = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(js, "")
	js = regexp.MustCompile(`(?m)(^|[^:"])//.*$`).ReplaceAllString(js, "$1")
	return js
}

// renderScript returns the seeded applab.sh as a caller receives it.
func renderScript(t *testing.T) string {
	t.Helper()
	file, ok := source.BootstrapFile(source.SeedValues{URL: "https://applab.example.com/applab"})
	if !ok {
		t.Fatal("the bootstrap file is not one of the seeded files")
	}
	if !strings.HasSuffix(file.Name, ".sh") {
		t.Fatalf("the bootstrap file is %q, not the script", file.Name)
	}
	return file.Body
}

func findEntry(pattern string) *apicontract.Entry {
	for i := range apicontract.Table {
		if apicontract.Table[i].Pattern() == pattern {
			return &apicontract.Table[i]
		}
	}
	return nil
}
