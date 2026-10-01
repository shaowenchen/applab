package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// TestTheExampleDockerfileDoesNotNeedRoot is the regression this file exists for.
//
// nginx's stock image starts as root and chowns its cache directory before
// dropping to its own user. AppLab drops every capability from an app's
// container, CAP_CHOWN included, so that chown fails:
//
//	[emerg] chown("/var/cache/nginx/client_temp", 101) failed (1: Operation not permitted)
//
// and the container exits before serving. The symptom is the worst kind: the
// build succeeds, the deploy succeeds, and the app sits not-ready with a log
// that names nginx rather than AppLab. The example therefore has to run
// unprivileged from its first instruction.
//
// Asserted on the rendered tree rather than on the template, so a substitution
// that broke the file would be caught here too.
// TestTheExampleDockerfileDoesNotTouchVarRun is the regression for a build that
// failed on the very first deploy of a new app.
//
// The example chowned nginx's pid file through /var/run, and the build died:
//
//	touch: /var/run/nginx.pid: No such file or directory
//
// Two things are wrong with that path, and either alone breaks it. On Alpine
// /var/run is a *relative symlink* to ../run, and kaniko excludes /var/run from
// the filesystem it unpacks — --ignore-var-run, on by default — so the symlink
// dangles and the redirect fails. And even where it resolves, nginx's own config
// says `pid /run/nginx.pid`, so the /var/run spelling wrote a file nothing read.
//
// The check is on the real path rather than on the absence of a string: /run is
// a directory in the base image, is not excluded, and is what nginx uses.
func TestTheExampleDockerfileDoesNotTouchVarRun(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, "shop", main); err != nil {
		t.Fatalf("Create: %v", err)
	}

	body := fileAtTip(t, s, "shop", "Dockerfile")

	// The RUN instructions, joined across their continuations. Read per
	// instruction rather than per line: a RUN is allowed to wrap, and the broken
	// version put /var/run on the continuation line — a scan that looked at one
	// line at a time passed against it, which is how this check was written
	// wrong the first time.
	//
	// Comments are dropped, since the comment above explains this very path and
	// a check over the raw text would force the explanation out of the file.
	var instructions []string
	var current string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if current == "" && !strings.HasPrefix(strings.ToUpper(trimmed), "RUN ") {
			continue
		}
		if strings.HasSuffix(trimmed, "\\") {
			current += strings.TrimSuffix(trimmed, "\\") + " "
			continue
		}
		instructions = append(instructions, current+trimmed)
		current = ""
	}

	if len(instructions) == 0 {
		t.Fatal("the example Dockerfile has no RUN instruction, so this check is reading nothing")
	}
	for _, ins := range instructions {
		if strings.Contains(ins, "/var/run") {
			t.Errorf("the example's RUN touches /var/run, which kaniko excludes and which is only a symlink to /run:\n  %s", ins)
		}
	}

	// And it does prepare the pid file's directory, or nginx cannot write there
	// as the unprivileged user this file drops to.
	if !strings.Contains(body, "/run") {
		t.Error("the example prepares no pid path; nginx as a non-root user cannot write /run/nginx.pid without it")
	}
}

func TestTheExampleDockerfileDoesNotNeedRoot(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, "shop", main); err != nil {
		t.Fatalf("Create: %v", err)
	}

	body := fileAtTip(t, s, "shop", "Dockerfile")

	// A USER instruction, and it is not root. Without one the container is root
	// and every chown in the base image's start-up runs into the dropped
	// capability set.
	user := ""
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(trimmed), "USER ") {
			user = strings.TrimSpace(trimmed[len("USER "):])
		}
	}
	if user == "" {
		t.Error("the example Dockerfile never drops root, so nginx's start-up chown fails against the capabilities AppLab drops")
	} else if user == "root" || user == "0" {
		t.Errorf("the example Dockerfile runs as %q, which is the case this guards against", user)
	}

	// And the base image is nginx, since the comment above is about nginx's
	// entrypoint specifically. A different base would need its own check.
	if !strings.Contains(body, "FROM nginx:") {
		t.Errorf("the example no longer builds from nginx, so the note about its start-up does not apply:\n%s", body)
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
		// The key is guarded per command rather than at the top, because
		// `create` is the command someone with no key runs — a top-level guard
		// would make the script unable to explain itself to exactly the person
		// who needs it to.
		if !strings.Contains(f.Body, "require_key()") {
			t.Error("the script should have a key guard the commands can call")
		}
		if !strings.Contains(f.Body, "APPLAB_KEY=<key>") {
			t.Error("the guard should say how to supply a key rather than failing silently")
		}
		// It must not be a top-level guard any more: `create` has to be
		// reachable without one.
		if strings.Contains(f.Body, `: "${APPLAB_KEY:?`) {
			t.Error("the key is required at the top of the script, so `create` cannot be run without one")
		}
		// The URL still is guarded there, because every command needs it and it
		// is the one value with no meaning when empty.
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
		// The live reading of what the pods are using. It is here for the same
		// reason as the rest: the console shows it and the script has to be able
		// to ask, or "api, cli and web do the same things" stops being true at
		// exactly the moment someone is diagnosing a pod at its limit.
		{"GET", "/resources"},
		{"POST", "/restart"},
		{"POST", "/rollback"},
		{"GET", "/status"},
		{"POST", "/stop"},
		{"PATCH", "/api/v1/apps/"},
		// The servers surface. It is admin-level rather than app-scoped, so the
		// list above was written without it — but the console offers it, so the
		// script owes the same reach or "the two front ends do the same things"
		// stops being true for a whole capability. The routes are the collection
		// and one member; the relay prefix is what makes them reach a remote.
		{"GET", "/api/v1/servers"},
		{"POST", "/api/v1/servers"},
		{"DELETE", "/api/v1/servers/"},
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
		// The commit is escaped rather than interpolated bare, because the same
		// variable now carries whatever the caller typed after --branch. The
		// fields are what this compares, and they are unchanged.
		{"deploy", `commit_sha: head, build: true`, `commit_sha\":\"$(json_escape "$deploy_commit")\",\"build\":true`},
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

// TestTheScriptCanBuildAndDeployAnotherBranch drives the shipped shell, which is
// the only thing that catches a bug in the script itself.
//
// The console's State card gained a branch picker, and the parity rule means the
// script has to offer the same choice. What makes that worth testing rather than
// reading is the deploy half: an app runs one branch, so deploying another one
// has to *switch* the app to it. A script that sent POST /deploy?branch= would
// look right and leave the app serving one branch while reporting another — the
// clone address, the history and the branch auto-deploy builds would all name
// the wrong one.
//
// Run rather than grepped, because the two forms differ only in which request
// they end up making, and the shell is where the argument parsing happens.
func TestTheScriptCanBuildAndDeployAnotherBranch(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		// wantMethod and wantPath are the request the command must end with.
		wantMethod string
		wantPath   string
		// wantBody is a substring the request body must contain, when the command
		// sends one at all.
		wantBody string
	}{
		{
			name:       "building another branch names it",
			argv:       []string{"build", "--branch", "dev"},
			wantMethod: "POST",
			wantPath:   "/api/v1/apps/shop/builds?branch=dev",
			wantBody:   "",
		},
		{
			name:       "deploying the active branch is a plain deploy",
			argv:       []string{"deploy"},
			wantMethod: "POST",
			wantPath:   "/api/v1/apps/shop/deploy",
			wantBody:   `"build":true`,
		},
		{
			// The case the feature is for, and the one a naive implementation gets
			// wrong: another branch is a switch.
			name:       "deploying another branch switches the app to it",
			argv:       []string{"deploy", "--branch", "dev"},
			wantMethod: "PUT",
			wantPath:   "/api/v1/apps/shop/branch",
			wantBody:   `{"branch":"dev"}`,
		},
		{
			name:       "deploying an explicitly named active branch is still a plain deploy",
			argv:       []string{"deploy", "--branch", "main"},
			wantMethod: "POST",
			wantPath:   "/api/v1/apps/shop/deploy?branch=main",
			wantBody:   `"build":true`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shBin, err := exec.LookPath("sh")
			if err != nil {
				t.Skip("sh is not on PATH")
			}

			type call struct {
				method, path, body string
			}
			var mu sync.Mutex
			var calls []call

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				calls = append(calls, call{r.Method, r.URL.RequestURI(), string(body)})
				mu.Unlock()

				w.Header().Set("Content-Type", "application/json")
				// The one read the deploy arm makes before it decides: which branch
				// the app is on. main is active, so "dev" is a switch and "main" is
				// not.
				if strings.HasSuffix(r.URL.Path, "/branches") {
					_, _ = io.WriteString(w, `{"data":{"app_id":"shop","branches":["main","dev"],"active":"main"}}`)
					return
				}
				_, _ = io.WriteString(w, `{"data":{"id":"b1","commit_sha":"abc123","status":"running"}}`)
			}))
			defer server.Close()

			script := ""
			for _, f := range seedFor(SeedValues{App: "shop"}) {
				if strings.HasSuffix(f.Name, ".sh") {
					script = f.Body
				}
			}
			if script == "" {
				t.Fatal("no seeded script")
			}

			dir := t.TempDir()
			path := filepath.Join(dir, "applab.sh")
			if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
				t.Fatalf("write the script: %v", err)
			}

			cmd := exec.Command(shBin, append([]string{path}, tc.argv...)...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"APPLAB_URL="+server.URL,
				"APPLAB_KEY=k",
				// The refresh would fetch a script this stub does not serve.
				"APPLAB_NO_REFRESH=1",
			)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if _, err := cmd.Output(); err != nil {
				t.Fatalf("run %v: %v\nstderr: %s", tc.argv, err, stderr.String())
			}

			mu.Lock()
			defer mu.Unlock()

			// The last call is the one that decides what happened: each command may
			// read first (the branches, to tell a switch from a deploy).
			if len(calls) == 0 {
				t.Fatal("the script made no request at all")
			}
			last := calls[len(calls)-1]
			if last.method != tc.wantMethod || last.path != tc.wantPath {
				t.Errorf("last request was %s %s, want %s %s",
					last.method, last.path, tc.wantMethod, tc.wantPath)
			}
			if tc.wantBody != "" && !strings.Contains(last.body, tc.wantBody) {
				t.Errorf("body was %s, want it to contain %s", last.body, tc.wantBody)
			}
		})
	}
}

// The script's server commands and its --server routing, driven through a real
// `sh` against a stand-in deployment.
//
// This is where the relay's path shape is pinned: `--server lab-2` has to turn
// `create shop` into a POST to /api/v1/servers/lab-2/apps, and `servers-add` has
// to send the remote's key in the body and never on a command line. A test that
// only grepped the script for the routes would pass on a script that built the
// wrong URL.
func TestTheScriptReachesAnotherDeployment(t *testing.T) {
	shBin, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not on PATH")
	}

	type call struct {
		method, path, body, auth string
	}

	cases := []struct {
		name       string
		argv       []string
		env        []string
		wantMethod string
		wantPath   string
		wantBody   string
	}{
		{
			name:       "listing servers",
			argv:       []string{"servers"},
			wantMethod: "GET",
			wantPath:   "/api/v1/servers",
		},
		{
			name:       "registering a server",
			argv:       []string{"servers-add", "lab-2", "https://applab-2.example.com/applab"},
			env:        []string{"APPLAB_SERVER_KEY=remote-admin"},
			wantMethod: "POST",
			wantPath:   "/api/v1/servers",
			wantBody:   `"key":"remote-admin"`,
		},
		{
			name:       "forgetting a server",
			argv:       []string{"servers-remove", "lab-2"},
			wantMethod: "DELETE",
			wantPath:   "/api/v1/servers/lab-2",
		},
		{
			name:       "listing a remote's apps through the relay",
			argv:       []string{"list", "--server", "lab-2"},
			wantMethod: "GET",
			wantPath:   "/api/v1/servers/lab-2/apps",
		},
		{
			name:       "creating on a remote through the relay",
			argv:       []string{"create", "--server", "lab-2", "shop"},
			wantMethod: "POST",
			wantPath:   "/api/v1/servers/lab-2/apps",
			wantBody:   `"id":"shop"`,
		},
		{
			name:       "deleting on a remote through the relay",
			argv:       []string{"delete", "--server", "lab-2", "shop"},
			wantMethod: "DELETE",
			wantPath:   "/api/v1/servers/lab-2/apps/shop",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var calls []call

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				calls = append(calls, call{r.Method, r.URL.RequestURI(), string(body), r.Header.Get("Authorization")})
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data":{"id":"lab-2","url":"https://applab-2.example.com/applab","app_key":"k"}}`)
			}))
			defer server.Close()

			script := ""
			for _, f := range seedFor(SeedValues{App: "shop"}) {
				if strings.HasSuffix(f.Name, ".sh") {
					script = f.Body
				}
			}
			if script == "" {
				t.Fatal("no seeded script")
			}

			dir := t.TempDir()
			path := filepath.Join(dir, "applab.sh")
			if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
				t.Fatalf("write the script: %v", err)
			}

			cmd := exec.Command(shBin, append([]string{path}, tc.argv...)...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"APPLAB_URL="+server.URL,
				"APPLAB_KEY=k",
				"APPLAB_NO_REFRESH=1",
			)
			cmd.Env = append(cmd.Env, tc.env...)
			// A delete reads the app id to confirm; feed it the id so the command
			// reaches the request rather than failing at the prompt.
			cmd.Stdin = strings.NewReader("shop\n")

			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if _, err := cmd.Output(); err != nil {
				t.Fatalf("run %v: %v\nstderr: %s", tc.argv, err, stderr.String())
			}

			mu.Lock()
			defer mu.Unlock()
			if len(calls) == 0 {
				t.Fatal("the script made no request at all")
			}
			last := calls[len(calls)-1]
			if last.method != tc.wantMethod || last.path != tc.wantPath {
				t.Errorf("last request was %s %s, want %s %s",
					last.method, last.path, tc.wantMethod, tc.wantPath)
			}
			if tc.wantBody != "" && !strings.Contains(last.body, tc.wantBody) {
				t.Errorf("body was %s, want it to contain %s", last.body, tc.wantBody)
			}
			// The key this deployment holds is what authenticates the relay; the
			// remote's key is in the body of a registration and nowhere else.
			if last.auth != "Bearer k" {
				t.Errorf("Authorization was %q, want the deployment's own key", last.auth)
			}
		})
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
	// Guarded per command, not at the top — see the note in the test above.
	if !strings.Contains(got, "require_key()") {
		t.Error("with no key, the script should have a guard the commands call rather than calling with an empty credential")
	}
}

// TestTheSeededTreeCarriesTheAppsAddress asserts the app's own URL is in the
// repository, in AGENT.md, rather than only behind an API call.
//
// The repository is where someone arrives — they clone an app, and the first
// question is where it is. Answering that with a curl command means running the
// API before you can learn the address, which is a lot of machinery for a string
// the deployment already knows.
func TestTheSeededTreeCarriesTheAppsAddress(t *testing.T) {
	objs, err := objectstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	const appURL = "https://applab.example.com/applab/apps/shop"
	s, err := New(Options{
		Objects:   objs,
		DataDir:   t.TempDir(),
		PublicURL: "https://applab.example.com/applab",
		AppURL:    func(string) string { return appURL },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := s.Create(ctx, "shop", main); err != nil {
		t.Fatalf("Create: %v", err)
	}

	agent := fileAtTip(t, s, "shop", "AGENT.md")
	if !strings.Contains(agent, appURL) {
		t.Errorf("AGENT.md does not carry the app's own address:\n%s", firstLines(agent, 20))
	}
	if strings.Contains(agent, "{{APP_ADDRESS}}") {
		t.Error("AGENT.md still has the address placeholder in it")
	}
	if !strings.Contains(agent, "served at") {
		t.Error("AGENT.md does not say what the address is for")
	}

	// And an upload rewrites it, the same as the rest of the file.
	body := buildTar(t, []tarEntry{{name: "main.go", body: "package main\n"}})
	if _, err := s.Ingest(ctx, "shop", main, strings.NewReader(string(body)), "later", "", DefaultIngestLimits); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !strings.Contains(fileAtTip(t, s, "shop", "AGENT.md"), appURL) {
		t.Error("an upload rewrote AGENT.md without the app's address")
	}
}

// TestWithNoAppDomainTheSeedSaysSo asserts the empty case reads as an answer.
//
// An installation with no domain serves its apps only inside the cluster, which
// is a legitimate way to run it. The section must say that rather than printing
// a heading with nothing under it — a substitution that renders an empty string
// produces exactly the kind of file someone concludes is truncated.
func TestWithNoAppDomainTheSeedSaysSo(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create(context.Background(), "shop", main); err != nil {
		t.Fatalf("Create: %v", err)
	}

	agent := fileAtTip(t, s, "shop", "AGENT.md")
	if strings.Contains(agent, "{{") {
		t.Error("AGENT.md has an unsubstituted placeholder with no address to write")
	}
	if !strings.Contains(agent, "no address outside the cluster") {
		t.Errorf("with no domain the section should say so:\n%s", firstLines(agent, 16))
	}
}

// firstLines is the head of a file, for a failure message that names the start
// of it rather than printing the whole document.
func firstLines(body string, n int) string {
	lines := strings.SplitN(body, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// TestTheSeededScriptCanReachBothLogsAndTheEvents asserts the four commands a
// debugging session actually needs are in the script, and reach the right
// endpoints.
//
// The two logs are different endpoints and it is easy to write a script that
// offers only one of them: an app's log comes from its pods and a build's from
// its Job, and a failed build is diagnosed from the second while a crash loop is
// diagnosed from the first. A script that could only read one would send whoever
// hit the other to the API by hand, which is the round trip these files exist to
// remove.
//
// Asserted on the rendered script rather than by running it, because what would
// break is the routing — a command that fell through to the app's log while
// looking like it read the build's — and that is visible in the request it
// builds.
func TestTheSeededScriptCanReachBothLogsAndTheEvents(t *testing.T) {
	script := ""
	for _, f := range seedFor(SeedValues{App: "shop"}) {
		if strings.HasSuffix(f.Name, ".sh") {
			script = f.Body
		}
	}
	if script == "" {
		t.Fatal("no seeded script")
	}

	for _, want := range []struct {
		name string
		path string
	}{
		{"an app's log", `/api/v1/apps/$APP/logs`},
		{"one pod's log", "pod=$(urlencode"},
		{"a build's log", `/api/v1/apps/$APP/builds/$build/logs`},
		{"the app's events", `/api/v1/apps/$APP/events`},
	} {
		if !strings.Contains(script, want.path) {
			t.Errorf("the seeded script cannot reach %s (no %q)", want.name, want.path)
		}
	}

	// The commands are named in the usage, or they are unreachable in practice:
	// the script is read by someone deciding what to type, and a command that is
	// only in the case statement is one nobody finds.
	for _, cmd := range []string{"build-logs", "--previous", "events"} {
		if !strings.Contains(script, cmd) {
			t.Errorf("%q is not in the script at all", cmd)
		}
	}
	if !strings.Contains(script, "Start with \"diagnose\"") {
		t.Error("the usage does not say where to start; the four commands are only useful if the first one to reach for is named")
	}
}

// seededScript returns the rendered applab.sh for an app.
func seededScript(t *testing.T) string {
	t.Helper()
	for _, f := range seedFor(SeedValues{App: "shop", URL: "http://deployment.invalid", Key: "k"}) {
		if strings.HasSuffix(f.Name, ".sh") {
			return f.Body
		}
	}
	t.Fatal("no seeded script")
	return ""
}

// TestTheRefreshReplacesAStaleScriptAndReRuns drives the shipped refresh against
// a real server, because the whole of this behaviour is process-level: a query
// form that signals on the wrong stream, or an exec that does not happen, looks
// exactly like success in the source and is invisible in a static check.
//
// The script under test is the stale one; the "deployment" serves a version with
// a marker in url(), so which copy ran is observable from the output. That is
// the property that matters: the command the caller typed is interpreted by the
// deployment's current script, not by the one in the checkout.
func TestTheRefreshReplacesAStaleScriptAndReRuns(t *testing.T) {
	shBin, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not on PATH")
	}

	stale := seededScript(t)
	current := strings.Replace(stale,
		`api GET "/api/v1/apps/$APP" | json_field url`,
		`echo "CURRENT: https://shop.example.test"`,
		1)
	if current == stale {
		t.Fatal("the marker was not inserted; url() has changed shape and this test is no longer testing what it says")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "applab.sh":
			_, _ = io.WriteString(w, current)
		case "AGENT.md":
			_, _ = io.WriteString(w, "CURRENT AGENT\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	script := filepath.Join(dir, "applab.sh")
	if err := os.WriteFile(script, []byte(stale), 0o755); err != nil {
		t.Fatalf("write the stale script: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("STALE AGENT\n"), 0o644); err != nil {
		t.Fatalf("write the stale AGENT.md: %v", err)
	}

	cmd := exec.Command(shBin, script, "url")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"APPLAB_URL="+server.URL,
		"APPLAB_KEY=k",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run the script: %v\nstderr: %s", err, stderr.String())
	}

	if got := strings.TrimSpace(string(out)); got != "CURRENT: https://shop.example.test" {
		t.Errorf("printed %q; the command did not run against the deployment's current script", got)
	}
	if !strings.Contains(stderr.String(), "re-running") {
		t.Errorf("stderr was %q; the refresh did not say what it did", stderr.String())
	}

	// Both files, not just the one that triggered the re-run: a checkout left
	// with a current script and a stale AGENT.md documents an API it no longer
	// matches.
	after, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("read back the script: %v", err)
	}
	if string(after) != current {
		t.Error("the script on disk is not the deployment's current one")
	}
	agent, err := os.ReadFile(filepath.Join(dir, "AGENT.md"))
	if err != nil {
		t.Fatalf("read back AGENT.md: %v", err)
	}
	if string(agent) != "CURRENT AGENT\n" {
		t.Errorf("AGENT.md is %q; the refresh updated the script and not the document beside it", agent)
	}

	// Second run: nothing to fetch that differs, so nothing is printed and the
	// command still works. Without this the refresh could be re-execing forever
	// and every assertion above would still pass.
	second := exec.Command(shBin, script, "url")
	second.Dir = dir
	second.Env = cmd.Env
	var secondErr bytes.Buffer
	second.Stderr = &secondErr
	out2, err := second.Output()
	if err != nil {
		t.Fatalf("second run: %v\nstderr: %s", err, secondErr.String())
	}
	if got := strings.TrimSpace(string(out2)); got != "CURRENT: https://shop.example.test" {
		t.Errorf("second run printed %q", got)
	}
	if strings.Contains(secondErr.String(), "re-running") {
		t.Error("the second run refreshed again; an already-current checkout must not re-exec")
	}
}

// TestTheRefreshNeverStopsTheCommandFromRunning is the half that matters when
// something is wrong: an unreachable deployment, a rotated key, a directory that
// cannot be written. None of those are a reason for the command the caller asked
// for to fail — they were working with this copy a moment ago.
//
// The command is `unknown-command` rather than `help`, and that is the point:
// help and self-update are exempt from the refresh, so a test that drives them
// never reaches the code it means to test — it passes whatever that code does.
// This one takes the refresh path and then fails at the command, which is the
// signal that the script got all the way there.
func TestTheRefreshNeverStopsTheCommandFromRunning(t *testing.T) {
	shBin, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not on PATH")
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "applab.sh")
	if err := os.WriteFile(script, []byte(seededScript(t)), 0o755); err != nil {
		t.Fatalf("write the script: %v", err)
	}

	// A port nothing is listening on: the fetch fails at the connection.
	cmd := exec.Command(shBin, script, "unknown-command")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "APPLAB_URL=http://127.0.0.1:1", "APPLAB_KEY=k")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()

	// Exit 2 is the script's own "unknown command", which is only reached if the
	// refresh let it through. The message goes to stderr — that is where the
	// script reports problems — so it is stderr that shows the command reached
	// its own handler.
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("an unreachable deployment changed what the command did: err %v, stdout %q, stderr %q",
			err, out, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown command: unknown-command") {
		t.Errorf("stderr was %q; the command did not reach its own handler", stderr.String())
	}

	// And APPLAB_NO_REFRESH is the way to pin a version: with it set, no request
	// is made at all, so even a script whose deployment is a black hole runs.
	cmd = exec.Command(shBin, script, "unknown-command")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "APPLAB_URL=http://127.0.0.1:1", "APPLAB_KEY=k", "APPLAB_NO_REFRESH=1")
	var pinnedErr bytes.Buffer
	cmd.Stderr = &pinnedErr
	out, err = cmd.Output()
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("with APPLAB_NO_REFRESH=1 the run errored differently: %v, stdout %q, stderr %q", err, out, pinnedErr.String())
	}
	if strings.Contains(pinnedErr.String(), "could not fetch") {
		t.Error("APPLAB_NO_REFRESH=1 still made a request")
	}
}

// TestBootstrapFileRendersNoAppAndNoKey is the guard on the unauthenticated
// bootstrap route, and it has to be here rather than at the route.
//
// The API test drives the real handler, and the handler passes only a URL — so
// it would pass whatever BootstrapFile did with an app's values, because it
// never supplies any. The clearing inside BootstrapFile is the actual
// protection, and this is what exercises it.
//
// The values are deliberately real: a route that renders these into a response
// hands out a working credential to an unauthenticated caller.
func TestBootstrapFileRendersNoAppAndNoKey(t *testing.T) {
	f, ok := BootstrapFile(SeedValues{
		App:    "shop",
		URL:    "https://applab.example.com",
		AppURL: "http://shop.apps.example.com",
		Key:    "s3cret-key",
	})
	if !ok {
		t.Fatal("BootstrapFile found no script; the bootstrap route has nothing to serve")
	}

	for _, leak := range []struct {
		name string
		text string
	}{
		{"the app's key", "s3cret-key"},
		{"the app's id", `APP="shop"`},
		{"the app's own address", "shop.apps.example.com"},
	} {
		if strings.Contains(f.Body, leak.text) {
			t.Errorf("the bootstrap script contains %s (%q); that route is unauthenticated", leak.name, leak.text)
		}
	}

	// And it does carry what makes it usable: the deployment's address, and the
	// empty app assignment the `use` command rebinds.
	if !strings.Contains(f.Body, "https://applab.example.com") {
		t.Error("the bootstrap script does not carry the deployment's address, so it cannot reach anything")
	}
	if !strings.Contains(f.Body, `APP="${APPLAB_APP:-}"`) {
		t.Error("the app assignment is not the empty, overridable form")
	}
	if strings.Contains(f.Body, "{{") {
		t.Error("the bootstrap script has an unsubstituted placeholder")
	}
}

// TestTheSeededScriptFallsBackToPartsWhenRefusedForSize drives the shipped
// upload against a server that refuses the single request, because the fallback
// is process-level: a status that is read wrongly, a re-package that reuses a
// consumed pipe, or a part count off by one all look fine in the source and
// only show up when the script is actually run.
//
// The deployment refuses `POST /source` with a 413 — which is what it now does
// for an archive over the limit it advertises — and accepts the three chunked
// calls. What has to hold is that the parts reassemble into the same archive
// `package` produced: the retry has to re-run tar, because the first pipe was
// partly read and cannot be rewound.
func TestTheSeededScriptFallsBackToPartsWhenRefusedForSize(t *testing.T) {
	shBin, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not on PATH")
	}
	for _, tool := range []string{"tar", "dd", "wc", "mktemp", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH", tool)
		}
	}

	script := seededScript(t)

	var (
		mu            sync.Mutex
		declaredTotal int
		assembled     []byte
		completed     bool
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/api/v1/config":
			writeJSON(t, w, map[string]any{"chunk_size": 4096})
		case strings.HasSuffix(path, "/source"):
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			writeJSON(t, w, map[string]any{"error": "too large; use /source/uploads"})
		case strings.HasSuffix(path, "/source/uploads"):
			var body struct {
				Total int `json:"total"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			declaredTotal = body.Total
			mu.Unlock()
			writeJSON(t, w, map[string]any{"upload_id": "up-1"})
		case strings.Contains(path, "/source/uploads/up-1/parts/"):
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			assembled = append(assembled, body...)
			mu.Unlock()
			writeJSON(t, w, map[string]any{"ok": true})
		case strings.HasSuffix(path, "/complete"):
			mu.Lock()
			completed = true
			mu.Unlock()
			writeJSON(t, w, map[string]any{"commit_sha": strings.Repeat("c", 40)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	// A tree big enough to be cut into more than one 4 KiB part, and with the
	// Dockerfile the script's own packaging expects to carry.
	//
	// The payload is incompressible on purpose. A repeated string is the obvious
	// fixture and it does not work: 30 KB of "applab-payload-" gzips to about
	// 270 bytes, so the archive is a single part on any platform. It passed
	// locally only because GNU tar pads its output to a 20-block record — on
	// Linux, where the CI runs, the same test declared one part and failed. The
	// payload has to survive gzip to make the part count mean anything.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "payload.bin"), incompressible(30<<10), 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	scriptPath := filepath.Join(dir, "applab.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write the script: %v", err)
	}

	cmd := exec.Command(shBin, scriptPath, "upload")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"APPLAB_URL="+server.URL,
		"APPLAB_KEY=k",
		// The script refreshes itself before every command, which here would
		// fetch a script the stub server does not serve.
		"APPLAB_NO_REFRESH=1",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run the script: %v\nstderr: %s", err, stderr.String())
	}

	if got := strings.TrimSpace(string(out)); got != strings.Repeat("c", 40) {
		t.Errorf("printed %q, want the commit the deployment returned\nstderr: %s", got, stderr.String())
	}

	mu.Lock()
	defer mu.Unlock()

	if !completed {
		t.Fatal("the chunked upload was never completed")
	}
	if declaredTotal < 2 {
		t.Errorf("declared %d parts; the archive should have needed several at a 4 KiB part size", declaredTotal)
	}

	// The parts, in order, have to be a tar.gz holding the tree. This is the
	// assertion that catches a retry which reuses the consumed pipe: those bytes
	// are not a valid gzip stream at all.
	//
	// Read as a tar rather than draining the gzip reader, which is what the
	// server does and is not the same thing. tar pads its output to its own
	// record size with NULs, and on macOS that padding lands *after* the gzip
	// member — so a ReadAll on the decompressor gets every byte of the archive
	// and then an error about the trailing zeros. The server never sees it
	// because the tar reader stops at the end-of-archive marker first.
	gz, err := gzip.NewReader(bytes.NewReader(assembled))
	if err != nil {
		t.Fatalf("the assembled archive is not a gzip stream (%d bytes): %v", len(assembled), err)
	}
	tr := tar.NewReader(gz)
	names := map[string]bool{}
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read the assembled archive as tar: %v", err)
		}
		names[strings.TrimPrefix(header.Name, "./")] = true
	}
	for _, want := range []string{"Dockerfile", "payload.bin"} {
		if !names[want] {
			t.Errorf("the assembled archive does not contain %s (it holds %v)", want, names)
		}
	}
}

// writeJSON answers with the API's success envelope.
func writeJSON(t *testing.T, w http.ResponseWriter, data map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

// incompressible returns n bytes that do not compress.
//
// The seed tests build a tree and measure the archive made from it, and gzip
// undoes anything patterned: 30 KB of a repeated string becomes a few hundred
// bytes, so an archive meant to need several parts arrives as one and the check
// passes vacuously. Seeded rather than drawn from the clock so a failure is
// reproducible, and drawn byte-by-byte rather than from arithmetic on the index
// — a counter cycling through a short range is compressed nearly as well as a
// constant, which is the same trap one step further along.
//
// The api package has the same helper; the two are separate packages, so it is
// written twice rather than exported from a test file that cannot be imported.
func incompressible(n int) []byte {
	rng := rand.New(rand.NewSource(1))
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = byte(rng.Intn(256))
	}
	return buf
}
