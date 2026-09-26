package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaowenchen/applab/internal/objectstore"
)

// seeded names the files every app's tree carries.
var seeded = []string{"applab.sh", "AGENT.md"}

// treeNames lists every path at a repository's tip.
func treeNames(t *testing.T, s *Store, appID string) []string {
	t.Helper()
	repo := materializeRepo(t, s, appID)
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
	if err := s.Create(ctx, "shop", main); err != nil {
		t.Fatalf("Create: %v", err)
	}

	names := treeNames(t, s, "shop")
	for _, want := range seeded {
		if !contains(names, want) {
			t.Errorf("a new app's tree is missing %s; it has %v", want, names)
		}
	}
}

// TestANewAppStartsWithAnExampleDockerfile asserts a freshly created app has
// something to build.
//
// AppLab requires a Dockerfile to build and does not write one into an app that
// already has source, so without this an app created and immediately deployed
// fails with "Dockerfile not found" — a first experience that reads as the
// platform being broken rather than as a file being absent.
func TestANewAppStartsWithAnExampleDockerfile(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, "shop", main); err != nil {
		t.Fatalf("Create: %v", err)
	}

	names := treeNames(t, s, "shop")
	if !contains(names, "Dockerfile") {
		t.Fatalf("a new app has no Dockerfile to build; it has %v", names)
	}
}

// TestAnUploadedDockerfileIsNotOverwritten is the test that guards the seed-once
// rule, and it is the one whose absence would be expensive.
//
// The Dockerfile is the file an app's author is most certain to write — it is
// the first thing most uploads contain. If AppLab wrote its example on every
// upload, as it does for the files it keeps current, then every push would
// replace the app's own build with a stock nginx and the failure would look like
// the build system ignoring the source.
//
// The seed-once file is written into the opening commit and never again, so the
// assertion is that an upload carrying its own Dockerfile wins.
func TestAnUploadedDockerfileIsNotOverwritten(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, "shop", main); err != nil {
		t.Fatalf("Create: %v", err)
	}

	const mine = "FROM golang:1.24-alpine\n"
	body := buildTar(t, []tarEntry{
		{name: "main.go", body: "package main\n"},
		{name: "Dockerfile", body: mine},
	})
	if _, err := s.Ingest(ctx, "shop", main, strings.NewReader(string(body)), "my own build", "", DefaultIngestLimits); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	repo := materializeRepo(t, s, "shop")
	got, err := s.run(ctx, repo, "show", "refs/heads/main:Dockerfile")
	if err != nil {
		t.Fatalf("read the Dockerfile at the tip: %v", err)
	}
	if string(got) != mine {
		t.Errorf("the uploaded Dockerfile was replaced by the seeded one:\ngot:  %q\nwant: %q", string(got), mine)
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
	if err := s.Create(ctx, "shop", main); err != nil {
		t.Fatalf("Create: %v", err)
	}

	body := buildTar(t, []tarEntry{
		{name: "main.go", body: "package main\n"},
		{name: "Dockerfile", body: "FROM scratch\n"},
	})
	if _, err := s.Ingest(ctx, "shop", main, strings.NewReader(string(body)), "first push", "", DefaultIngestLimits); err != nil {
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
	if err := s.Create(ctx, "shop", main); err != nil {
		t.Fatalf("Create: %v", err)
	}

	body := buildTar(t, []tarEntry{
		{name: "applab.sh", body: "#!/bin/sh\necho stale\n"},
		{name: "main.go", body: "package main\n"},
	})
	if _, err := s.Ingest(ctx, "shop", main, strings.NewReader(string(body)), "upload a stale copy", "", DefaultIngestLimits); err != nil {
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
	if err := s.Create(context.Background(), "shop", main); err != nil {
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

	for _, f := range seedFor(SeedValues{App: "shop"}) {
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

// TestTheSeededScriptCarriesTheKeyAsADefault asserts the two variables are
// filled in, and filled in in the one shape that is safe.
//
// The decision was to write the deployment's address and the app's key into the
// script so a fresh clone runs with nothing exported. The risk is stated where
// the reader will meet it — the script's header — and it is bounded: the key
// reaches this app only, and an admin can rotate it.
//
// What must not happen is an assignment that *overrides* the environment. The
// script is written as `${APPLAB_KEY:-value}`, so an exported key wins; a plain
// `APPLAB_KEY=value` would silently ignore one, which is exactly how someone
// working against a second deployment, or with a rotated key, would get
// confusing 401s from a credential they never chose.
func TestTheSeededScriptCarriesTheKeyAsADefault(t *testing.T) {
	values := SeedValues{
		App: "shop",
		URL: "https://applab.example.com/applab",
		Key: "s3cret-key-value",
	}

	for _, f := range seedFor(values) {
		if f.Name != "applab.sh" {
			continue
		}
		if !strings.Contains(f.Body, `APPLAB_URL="${APPLAB_URL:-https://applab.example.com/applab}"`) {
			t.Error("applab.sh does not default APPLAB_URL to the deployment's address")
		}
		if !strings.Contains(f.Body, `APPLAB_KEY="${APPLAB_KEY:-s3cret-key-value}"`) {
			t.Error("applab.sh does not default APPLAB_KEY to this app's key")
		}
		for _, line := range strings.Split(f.Body, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			// A bare assignment is the mistake: it would override the caller's
			// environment rather than yielding to it. The defaulting form starts
			// the same way and is told apart by the expansion it must contain.
			for _, name := range []string{"APPLAB_KEY=", "APPLAB_URL="} {
				if strings.HasPrefix(trimmed, name) && !strings.Contains(trimmed, "${") {
					t.Errorf("applab.sh assigns %s directly instead of defaulting it: %q", strings.TrimSuffix(name, "="), trimmed)
				}
			}
		}
	}
}

// TestSeedingWithoutAKeyLeavesTheLineForTheCaller asserts the empty cases.
//
// A deployment that mints no app keys, and one that does not know its own public
// address, are both real configurations. The script has to render anyway — it is
// still the documentation and the driver — and it has to fail with a sentence
// that names the missing variable rather than calling an empty URL.
func TestSeedingWithoutAKeyLeavesTheLineForTheCaller(t *testing.T) {
	for _, f := range seedFor(SeedValues{App: "shop"}) {
		if f.Name != "applab.sh" {
			continue
		}
		if !strings.Contains(f.Body, `APPLAB_KEY="${APPLAB_KEY:-}"`) {
			t.Error("with no key to seed, applab.sh should leave APPLAB_KEY empty for the caller to export")
		}
		if !strings.Contains(f.Body, "set APPLAB_KEY") {
			t.Error("the script should say the key is missing rather than calling an empty one")
		}
		// The guards are what turn an empty value into a sentence; without them
		// the script would issue requests against a relative URL.
		if !strings.Contains(f.Body, ":?set APPLAB_URL") {
			t.Error("applab.sh does not guard against an empty APPLAB_URL")
		}
	}
}

// TestEveryPlaceholderIsSubstituted asserts the renderer left nothing behind.
//
// The substitution is a plain string replacement over a fixed vocabulary, so the
// failure mode is a token that was added to a template and never to the
// renderer. That ships as literal braces in someone's shell script — where
// `{{KEY}}` is a command substitution that fails at runtime, not at parse time,
// and only on the machine that has a key to lose.
func TestEveryPlaceholderIsSubstituted(t *testing.T) {
	values := SeedValues{App: "shop", URL: "https://applab.example.com", Key: "s3cret"}
	for _, f := range append(seedForFirstCommit(values), seedFor(values)...) {
		if i := strings.Index(f.Body, "{{"); i >= 0 {
			end := min(i+40, len(f.Body))
			t.Errorf("%s still carries an unsubstituted placeholder: %q", f.Name, f.Body[i:end])
		}
	}
}

// TestTheSeededScriptsBranchReaderWorks runs the script's own branch reader over
// the API's real response shape.
//
// The reader parses JSON with sed, because the script is expected to work on a
// base system with no jq — and a sed expression that matches nothing is
// indistinguishable from "this app has no branches", which is a wrong answer
// rather than a missing one. So it is executed rather than pattern-matched: the
// three shapes the endpoint can return are fed through it, and what it prints is
// asserted.
func TestTheSeededScriptsBranchReaderWorks(t *testing.T) {
	shBin, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not on PATH")
	}

	script := ""
	for _, f := range seedFor(SeedValues{App: "shop"}) {
		if strings.HasSuffix(f.Name, ".sh") {
			script = f.Body
		}
	}
	if script == "" {
		t.Fatal("no seeded script")
	}

	// The two functions the reader is built from, taken as they appear so the
	// test runs the shipped text rather than a copy of it, plus the one call
	// that runs it — a harness that only defines branch_list prints nothing, and
	// the "no branches" case would then pass for the wrong reason.
	//
	// "\n}\n" is the terminator because it only matches a closing brace at
	// column zero, and every brace nested inside these functions is indented —
	// so the first match is the function's own end.
	extract := func(name string) string {
		start := strings.Index(script, "\n"+name+"() {")
		if start < 0 {
			t.Fatalf("%s is not in the seeded script", name)
		}
		end := strings.Index(script[start:], "\n}\n")
		if end < 0 {
			t.Fatalf("%s has no closing brace", name)
		}
		return script[start+1 : start+end+3]
	}
	harness := extract("json_field") + extract("branch_list") + "\nbranch_list\n"

	cases := []struct {
		name     string
		response string
		want     []string
	}{
		{
			name:     "one branch",
			response: `{"app_id":"shop","branches":["main"],"active":"main"}`,
			want:     []string{"* main"},
		},
		{
			name:     "several, marking the active one",
			response: `{"app_id":"shop","branches":["main","dev","feature/x"],"active":"dev"}`,
			want:     []string{"  main", "* dev", "  feature/x"},
		},
		{
			name:     "none",
			response: `{"app_id":"shop","branches":[],"active":"main"}`,
			want:     nil, // reported, not listed: the message goes to stderr
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "reader.sh")
			if err := os.WriteFile(path, []byte(harness), 0o644); err != nil {
				t.Fatalf("write reader: %v", err)
			}

			cmd := exec.Command(shBin, path)
			cmd.Stdin = strings.NewReader(tc.response)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("run the reader: %v", err)
			}

			got := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
			if tc.want == nil {
				if strings.TrimSpace(string(out)) != "" {
					t.Errorf("an app with no branches listed %q, and should report rather than list", out)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("printed %q, want %q", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("line %d: printed %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestTheSeededScriptIsExecutable asserts the mode, because a script that has to
// be chmodded before its first use is one an agent will fail on.
func TestTheSeededScriptIsExecutable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, "shop", main); err != nil {
		t.Fatalf("Create: %v", err)
	}
	repo := materializeRepo(t, s, "shop")

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
	if err := s.Create(ctx, "shop", main); err != nil {
		t.Fatalf("Create: %v", err)
	}

	body := buildTar(t, []tarEntry{
		{name: "applab.sh", typeflag: '2', linkname: "../../../../etc/passwd"},
		{name: "main.go", body: "package main\n"},
	})
	if _, err := s.Ingest(ctx, "shop", main, strings.NewReader(string(body)), "symlink", "", DefaultIngestLimits); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// The seed is a regular file at the tip, not a symlink to somewhere else.
	repo := materializeRepo(t, s, "shop")
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
// One direction only. The script may reach routes the console does not, and
// does: `secret set` and `secret unset` have no console equivalent, because a
// secret's value cannot be read back and a page that only ever writes one is a
// worse place to keep it than a terminal. The reverse — a console button with no
// script command — is the divergence this catches.
//
// When the console gains a call, this fails and names it. Adding the command to
// the script is the fix; widening the exemption list is the thing to resist.
func TestTheScriptCoversWhatTheConsoleDoes(t *testing.T) {
	console, err := os.ReadFile(filepath.Join("..", "console", "static", "index.html"))
	if err != nil {
		t.Fatalf("read the console: %v", err)
	}
	script := ""
	for _, f := range seedFor(SeedValues{App: "shop"}) {
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
		{"GET", "/branches"},
		{"PUT", "/branch"},
		{"GET", "/commits"},
		{"GET", "/config"},
		{"POST", "/deploy"},
		{"PUT", "/env"},
		{"DELETE", "/env/"},
		{"GET", "/key"},
		// No trailing "?" on this one. It used to be "/logs?", which pinned the
		// fact that the console built its query inline — the dialog refactor
		// assembles it with URLSearchParams instead, so the literal stopped
		// appearing and this entry reported the console as having dropped the
		// route. The route is what matters here; how a client spells the query
		// is not.
		{"GET", "/logs"},
		{"GET", "/pods"},
		{"POST", "/restart"},
		{"POST", "/rollback"},
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
	for _, f := range seedFor(SeedValues{App: "shop"}) {
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
	repo := materializeRepo(t, s, appID)
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

// TestTheSeededTreeCarriesTheDeploymentsAddressAndTheAppsKey asserts the two
// values reach the repository, on both paths that write it.
//
// There are two, and they differ in a way that is easy to get wrong: the opening
// commit is written when the store created the repository, and every upload
// rewrites the kept-current files afterwards. A key wired into only one of them
// produces an app whose first checkout works and whose every later one does not,
// or the reverse — and both look like a bug in the script rather than in the
// seeding.
func TestTheSeededTreeCarriesTheDeploymentsAddressAndTheAppsKey(t *testing.T) {
	objs, err := objectstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	// A key lookup that answers for any app, the way the key store does once
	// handleCreateApp has minted one before the repository is created.
	const key = "the-apps-key"
	s, err := New(Options{
		Objects:   objs,
		DataDir:   t.TempDir(),
		PublicURL: "https://applab.example.com/applab/",
		SeedKey: func(context.Context, string) (string, error) {
			return key, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := s.Create(ctx, "shop", main); err != nil {
		t.Fatalf("Create: %v", err)
	}

	opening := fileAtTip(t, s, "shop", "applab.sh")
	if !strings.Contains(opening, `APPLAB_URL="${APPLAB_URL:-https://applab.example.com/applab}"`) {
		t.Error("the opening commit does not carry the deployment's address, with the trailing slash trimmed")
	}
	if !strings.Contains(opening, `APPLAB_KEY="${APPLAB_KEY:-`+key+`}"`) {
		t.Error("the opening commit does not carry the app's key")
	}

	// The upload path writes the same files, and the key has to survive it —
	// this is the rewrite that replaces whatever was in the tree.
	body := buildTar(t, []tarEntry{{name: "main.go", body: "package main\n"}})
	if _, err := s.Ingest(ctx, "shop", main, strings.NewReader(string(body)), "for a later commit", "", DefaultIngestLimits); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	uploaded := fileAtTip(t, s, "shop", "applab.sh")
	if !strings.Contains(uploaded, key) {
		t.Error("an upload rewrote applab.sh without the app's key")
	}
	if strings.Contains(uploaded, "{{KEY}}") || strings.Contains(uploaded, "{{URL}}") {
		t.Error("an upload left a placeholder unsubstituted")
	}
}

// TestASeedWithNoKeyOrAddressStillRenders asserts the two optional inputs are
// genuinely optional.
//
// A deployment that mints no app keys and one that does not know its own public
// address are both real: the second is an installation reachable only inside the
// cluster. Uploading source must not fail for either — the seeded files are
// documentation and a convenience — and the script has to leave the caller a
// sentence rather than an empty variable.
func TestASeedWithNoKeyOrAddressStillRenders(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, "shop", main); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got := fileAtTip(t, s, "shop", "applab.sh")
	if strings.Contains(got, "{{") {
		t.Error("applab.sh has an unsubstituted placeholder with neither value set")
	}
	if !strings.Contains(got, `APPLAB_KEY="${APPLAB_KEY:-}"`) {
		t.Error("with no key, the script should leave APPLAB_KEY to the environment")
	}
	if !strings.Contains(got, ":?set APPLAB_KEY") {
		t.Error("with no key, the script should say so rather than call with an empty credential")
	}
}
