package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// seeded names the files every app's tree carries.
var seeded = []string{"applab.sh", "AGENT.md"}

// treeNames lists every path at a repository's tip.
func treeNames(t *testing.T, s *Store, appID string) []string {
	t.Helper()
	repo, err := s.RepoPath(appID)
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	out, err := s.run(context.Background(), repo, "ls-tree", "-r", "--name-only", "refs/heads/main")
	if err != nil {
		t.Fatalf("ls-tree: %v", err)
	}
	return strings.Fields(string(out))
}

// TestANewAppIsCloneableWithItsSeedFiles asserts the repository an app gets at
// creation is not empty.
//
// A bare repository with no commits clones to nothing and reports a HEAD that
// points at a ref which does not exist, so the first thing a caller does with a
// new app — clone it and look — fails before it starts.
func TestANewAppIsCloneableWithItsSeedFiles(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, "shop"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	names := treeNames(t, s, "shop")
	for _, want := range seeded {
		if !contains(names, want) {
			t.Errorf("a new app's tree is missing %s; it has %v", want, names)
		}
	}
}

// TestTheSeedFilesSurviveAnUpload is the test that matters most here.
//
// A commit is built from the uploaded tree alone, so everything an earlier
// commit held is replaced wholesale by the next upload. Seeding only at creation
// therefore produces files that survive until the first push and then vanish —
// which is exactly what a caller would not notice until they needed them.
func TestTheSeedFilesSurviveAnUpload(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, "shop"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	body := buildTar(t, []tarEntry{
		{name: "main.go", body: "package main\n"},
		{name: "Dockerfile", body: "FROM scratch\n"},
	})
	if _, err := s.Ingest(ctx, "shop", strings.NewReader(string(body)), "first push", "", DefaultIngestLimits); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	names := treeNames(t, s, "shop")
	for _, want := range seeded {
		if !contains(names, want) {
			t.Errorf("%s did not survive the upload; the tip has %v", want, names)
		}
	}
	// And the upload still landed. A version that seeded by overwriting the tree
	// rather than adding to it would pass the loop above and fail here.
	for _, want := range []string{"main.go", "Dockerfile"} {
		if !contains(names, want) {
			t.Errorf("the uploaded %s is missing; the tip has %v", want, names)
		}
	}
}

// TestTheSeedIsRefreshedOnEveryUpload asserts an uploaded copy does not win.
//
// The seeded files describe the deployment's own API, so a stale copy carried in
// someone's source tree is one that tells an agent about endpoints that may no
// longer exist. The copy in the tree is AppLab's.
func TestTheSeedIsRefreshedOnEveryUpload(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, "shop"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	body := buildTar(t, []tarEntry{
		{name: "applab.sh", body: "#!/bin/sh\necho stale\n"},
		{name: "main.go", body: "package main\n"},
	})
	if _, err := s.Ingest(ctx, "shop", strings.NewReader(string(body)), "upload a stale copy", "", DefaultIngestLimits); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	got := fileAtTip(t, s, "shop", "applab.sh")
	if strings.Contains(got, "echo stale") {
		t.Error("the uploaded applab.sh was kept; the seeded copy has to replace it or it goes stale in place")
	}
	if !strings.Contains(got, "APPLAB_KEY") {
		t.Errorf("applab.sh at the tip does not look like the seeded script:\n%s", got)
	}
}

// TestTheSeedCarriesTheAppID asserts the placeholder substitution happened.
//
// The templates are rendered by a plain string replacement rather than by
// text/template — the content is shell, and a template engine would interpret
// its `$` and backticks — so the one failure mode of that choice is a token left
// unsubstituted, which is what this catches.
func TestTheSeedCarriesTheAppID(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create(context.Background(), "shop"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, name := range seeded {
		got := fileAtTip(t, s, "shop", name)
		if strings.Contains(got, "{{APP}}") {
			t.Errorf("%s still contains the unsubstituted {{APP}} placeholder", name)
		}
		if !strings.Contains(got, "shop") {
			t.Errorf("%s does not name the app it was written for", name)
		}
	}
}

// TestTheSeededScriptIsValidShell asserts the script parses.
//
// It is the deliverable an agent runs, and the templates are edited by hand, so
// the failure mode is a shell file that is subtly unbalanced. An apostrophe in a
// `${VAR:?message}` is exactly that: the quote opens a string that never closes,
// and the script fails to parse at all — with the error pointing at the end of
// the file rather than at the line that caused it. Checking that the rendered
// result parses is what turns that into a named failure here.
//
// Skipped when sh is absent; every platform that builds this has one, but the
// test is not worth a hard dependency.
func TestTheSeededScriptIsValidShell(t *testing.T) {
	shBin, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not on PATH")
	}

	for _, f := range seedFor("shop") {
		if !strings.HasSuffix(f.Name, ".sh") {
			continue
		}
		path := filepath.Join(t.TempDir(), f.Name)
		if err := os.WriteFile(path, []byte(f.Body), 0o644); err != nil {
			t.Fatalf("write %s: %v", f.Name, err)
		}
		out, err := exec.Command(shBin, "-n", path).CombinedOutput()
		if err != nil {
			t.Errorf("%s is not valid shell: %v\n%s", f.Name, err, out)
		}
	}
}

// TestTheSeededScriptNeverCarriesAKey asserts the one thing the seed must not do.
//
// The script drives the app's whole lifecycle, so the easy version of it would
// have the key written in. That key is appended to the app's own clone URL, and
// this source is cloned, uploaded, built into an image and mirrored — so a key
// baked in here travels with all of it. It takes the key from the environment
// instead, and this is what keeps it that way.
func TestTheSeededScriptNeverCarriesAKey(t *testing.T) {
	for _, f := range seedFor("shop") {
		for _, line := range strings.Split(f.Body, "\n") {
			trimmed := strings.TrimSpace(line)
			// An assignment to either variable with a literal value would be the
			// mistake; the reads are `${APPLAB_KEY...}` and `$APPLAB_KEY`.
			if strings.HasPrefix(trimmed, "APPLAB_KEY=") || strings.HasPrefix(trimmed, "APPLAB_URL=") {
				t.Errorf("%s assigns %s directly: %q", f.Name, strings.SplitN(trimmed, "=", 2)[0], trimmed)
			}
		}
	}
}

// TestTheSeededScriptIsExecutable asserts the mode, because a script that has to
// be chmodded before its first use is one an agent will fail on.
func TestTheSeededScriptIsExecutable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, "shop"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	repo, _ := s.RepoPath("shop")

	out, err := s.run(ctx, repo, "ls-tree", "-r", "refs/heads/main")
	if err != nil {
		t.Fatalf("ls-tree: %v", err)
	}
	if !strings.Contains(string(out), "100755 blob") {
		t.Errorf("no file at the tip is executable:\n%s", out)
	}
}

// TestSeedReplacesASymlinkRatherThanFollowingIt asserts the write cannot be
// redirected by the uploaded tree.
//
// The tree came out of an archive from an untrusted caller, so `applab.sh` could
// be a symlink pointing anywhere the process can write. Following it would put
// the seed's content outside the tree — and, worse, overwrite whatever it points
// at.
func TestSeedReplacesASymlinkRatherThanFollowingIt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, "shop"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	body := buildTar(t, []tarEntry{
		{name: "applab.sh", typeflag: '2', linkname: "../../../../etc/passwd"},
		{name: "main.go", body: "package main\n"},
	})
	if _, err := s.Ingest(ctx, "shop", strings.NewReader(string(body)), "symlink", "", DefaultIngestLimits); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// The seed is a regular file at the tip, not a symlink to somewhere else.
	repo, _ := s.RepoPath("shop")
	out, err := s.run(ctx, repo, "ls-tree", "-r", "refs/heads/main")
	if err != nil {
		t.Fatalf("ls-tree: %v", err)
	}
	if strings.Contains(string(out), "120000 blob") {
		t.Errorf("a symlink survived into the tree:\n%s", out)
	}
	if !strings.Contains(string(out), "applab.sh") {
		t.Errorf("applab.sh is not at the tip:\n%s", out)
	}
}

// TestTheScriptCoversWhatTheConsoleDoes is the check that keeps the two surfaces
// in step.
//
// The console's app page and this script are two front ends for the same API, and
// "the script can do everything the page can" is the property that makes either
// of them sufficient. It is not a property that stays true by itself: the page
// gains a button, the script is not thought of, and the divergence is invisible
// because nothing exercises both.
//
// So the endpoints the console calls for one app are listed here, and each has to
// have a matching call in the script. The list is of the *console's* usage rather
// than of the API's routes — a route nothing offers to a user is not something
// the script owes parity with.
//
// When the console gains a call, this fails and names it. Adding the command to
// the script is the fix; widening the exemption list is the thing to resist.
func TestTheScriptCoversWhatTheConsoleDoes(t *testing.T) {
	console, err := os.ReadFile(filepath.Join("..", "console", "static", "index.html"))
	if err != nil {
		t.Fatalf("read the console: %v", err)
	}
	script := ""
	for _, f := range seedFor("shop") {
		if strings.HasSuffix(f.Name, ".sh") {
			script = f.Body
		}
	}
	if script == "" {
		t.Fatal("no seeded script to compare against")
	}

	// Every app-scoped route the console calls, with the method it uses. Written
	// as the path the script would have to build, so a match is a real match
	// rather than a substring of something else.
	wanted := []struct{ method, path string }{
		{"GET", "/api/v1/apps/"},
		{"DELETE", "/api/v1/apps/"},
		{"POST", "/builds"},
		{"GET", "/builds"},
		{"DELETE", "/builds/"},
		{"GET", "/commits"},
		{"GET", "/config"},
		{"POST", "/deploy"},
		{"PUT", "/env"},
		{"DELETE", "/env/"},
		{"GET", "/key"},
		{"GET", "/logs?"},
		{"GET", "/pods"},
		{"POST", "/restart"},
		{"POST", "/rollback"},
		{"PUT", "/secrets"},
		{"DELETE", "/secrets/"},
		{"GET", "/status"},
		{"POST", "/stop"},
		{"PATCH", "/api/v1/apps/"},
	}

	missing := []string{}
	for _, w := range wanted {
		// The console call has to exist, or this list has drifted from it.
		if !strings.Contains(string(console), w.path) {
			t.Errorf("the console no longer calls %s %s; this list is out of date", w.method, w.path)
			continue
		}
		// And the script has to make it.
		if !strings.Contains(script, w.method+" ") || !strings.Contains(script, w.path) {
			missing = append(missing, w.method+" "+w.path)
		}
	}
	if len(missing) > 0 {
		t.Errorf("the console can do these and applab.sh cannot:\n  %s\n"+
			"add a command for each, or the two front ends have drifted",
			strings.Join(missing, "\n  "))
	}
}

// TestTheScriptsRequestsCarryTheSameFields checks the two front ends ask for the
// same thing, not merely that they call the same URL.
//
// The test above proves the script can reach every route the console uses, which
// is necessary and not sufficient: a deploy with `build` and one without hit the
// same endpoint and behave differently the moment the commit has not been built.
// The console's button and the script's command are the same operation and have
// to make the same request.
//
// The two cannot be compared as bytes — one writes a JavaScript object literal
// and the other a JSON string inside a shell string — so what is compared is the
// **set of fields** each one sends, per operation. Each pair is anchored to the
// call it belongs to rather than searched for globally, because "build" appears
// in the script for reasons that have nothing to do with the deploy body.
func TestTheScriptsRequestsCarryTheSameFields(t *testing.T) {
	console, err := os.ReadFile(filepath.Join("..", "console", "static", "index.html"))
	if err != nil {
		t.Fatalf("read the console: %v", err)
	}
	script := ""
	for _, f := range seedFor("shop") {
		if strings.HasSuffix(f.Name, ".sh") {
			script = f.Body
		}
	}
	if script == "" {
		t.Fatal("no seeded script to compare against")
	}

	cases := []struct {
		op string
		// console is a literal from the console's request body for this call.
		console string
		// script is the same field as it appears inside the script's JSON string.
		script string
	}{
		// The decisive one. The console deploys with build:true so a commit that
		// was never built does not make the button useless; the script has to ask
		// for the same thing or the command that is supposed to do what the button
		// does will fail where the button succeeds.
		{"deploy", `commit_sha: head, build: true`, `commit_sha\":\"${1:-}\",\"build\":true`},
		{"replicas", `JSON.stringify({ replicas: wanted })`, `{\"replicas\":$1}`},
		{"env set", `JSON.stringify({ env: {`, `json_pairs env`},
		{"secret set", `JSON.stringify({ secrets: {`, `json_pairs secrets`},
	}

	for _, tc := range cases {
		if !strings.Contains(string(console), tc.console) {
			t.Errorf("the console no longer sends %s as %q; this list is out of date, and the check below is now vacuous",
				tc.op, tc.console)
			continue
		}
		if !strings.Contains(script, tc.script) {
			t.Errorf("%s: the console sends %q and applab.sh does not send %q.\n"+
				"The two are the same operation, so a request that works from one must work from the other.",
				tc.op, tc.console, tc.script)
		}
	}
}

// fileAtTip returns a file's content at a repository's tip.
func fileAtTip(t *testing.T, s *Store, appID, name string) string {
	t.Helper()
	repo, err := s.RepoPath(appID)
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	out, err := s.run(context.Background(), repo, "show", "refs/heads/main:"+name)
	if err != nil {
		t.Fatalf("show %s: %v", name, err)
	}
	return string(out)
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
