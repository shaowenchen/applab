package model_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/shaowenchen/applab/internal/model"
)

// TestValidateBranchName covers the names that must be refused.
//
// Each rejection is a defect that would appear somewhere other than where it was
// typed: a name with ".." in it becomes a path that leaves the repository, a name
// starting with a dash becomes an option to git, and a name with a slash is two
// directories in a bucket key.
func TestValidateBranchName(t *testing.T) {
	cases := []struct {
		name    string
		wantErr bool
		why     string
	}{
		{name: "main", wantErr: false},
		{name: "dev", wantErr: false},
		{name: "feature/branches", wantErr: false, why: "git nests branches under a slash and so does a bucket key"},
		{name: "release-1.2", wantErr: false},
		{name: "v1.0.0", wantErr: false},
		{name: "_working", wantErr: false},

		{name: "", wantErr: true, why: "an empty branch is not a name; the one place it means something is resolved before this"},
		{name: "-dash", wantErr: true, why: "git accepts this ref, and git would also read it as an option"},
		{name: "--upload-pack=x", wantErr: true, why: "the reason the leading-dash rule exists rather than relying on a -- separator at each call site"},
		{name: "has space", wantErr: true, why: "legal to git, awkward as a key and a URL"},
		{name: "a..b", wantErr: true, why: "a name becomes a path"},
		{name: "..", wantErr: true},
		{name: "a.lock", wantErr: true, why: "git keeps a lock file of that name"},
		{name: "a@{b}", wantErr: true},
		{name: "@", wantErr: true, why: "git's shorthand for HEAD, so a branch of that name is unreachable"},
		{name: "/lead", wantErr: true},
		{name: "trail/", wantErr: true},
		{name: "a//b", wantErr: true},
		{name: "trail.", wantErr: true},
		{name: "a~b", wantErr: true},
		{name: "a^b", wantErr: true},
		{name: "a:b", wantErr: true},
		{name: "a?b", wantErr: true},
		{name: "a*b", wantErr: true},
		{name: "a[b", wantErr: true},
		{name: `a\b`, wantErr: true},
		{name: "tab\there", wantErr: true, why: "a control character"},
		{name: "del\x7f", wantErr: true},
		{name: strings.Repeat("a", 256), wantErr: true, why: "longer than a key segment can be"},
		{name: strings.Repeat("a", 255), wantErr: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := model.ValidateBranchName(tc.name)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateBranchName(%q) = nil, want an error%s", tc.name, explain(tc.why))
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateBranchName(%q) = %v, want nil%s", tc.name, err, explain(tc.why))
			}
		})
	}
}

// TestAutoDeploysResolvesUnsetToOn asserts the one resolution there is.
//
// A plain bool would have made "off" the zero value, so every app written before
// the field existed would have silently stopped deploying on a push. The pointer
// is what makes "no value recorded" mean what those apps actually do.
func TestAutoDeploysResolvesUnsetToOn(t *testing.T) {
	if !(model.App{}).AutoDeploys() {
		t.Error("an app with no auto-deploy setting recorded reports it as off; every existing app would stop deploying on a push")
	}

	off := false
	if (model.App{AutoDeploy: &off}).AutoDeploys() {
		t.Error("an app explicitly set to false reports auto-deploy on; the switch would do nothing")
	}

	on := true
	if !(model.App{AutoDeploy: &on}).AutoDeploys() {
		t.Error("an app explicitly set to true reports auto-deploy off")
	}
}

// TestValidatePortAndReplicas pins the bounds the four surfaces enforce.
//
// The API, the console, the CLI's create and the CLI's update all refuse these,
// and they read the bounds from here so there is one definition. What this
// asserts is that the definition is the one they were built against: a port is a
// TCP port, and the replica ceiling is the number the API's own message names.
func TestValidatePortAndReplicas(t *testing.T) {
	ports := []struct {
		port    int32
		wantErr bool
	}{
		{1, false}, {80, false}, {8080, false}, {65535, false},
		{0, true}, {-1, true}, {65536, true}, {70000, true},
	}
	for _, tc := range ports {
		err := model.ValidatePort(tc.port)
		if tc.wantErr && err == nil {
			t.Errorf("ValidatePort(%d) = nil, want an error", tc.port)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("ValidatePort(%d) = %v, want nil", tc.port, err)
		}
	}

	replicas := []struct {
		n       int32
		wantErr bool
	}{
		{1, false}, {3, false}, {model.MaxReplicas, false},
		{0, true}, {-1, true}, {model.MaxReplicas + 1, true},
	}
	for _, tc := range replicas {
		err := model.ValidateReplicas(tc.n)
		if tc.wantErr && err == nil {
			t.Errorf("ValidateReplicas(%d) = nil, want an error", tc.n)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("ValidateReplicas(%d) = %v, want nil", tc.n, err)
		}
	}

	// The bounds themselves, so a change to one is a deliberate act rather than
	// something that slips in with an unrelated edit — four surfaces accept what
	// these say.
	if model.MinPort != 1 || model.MaxPort != 65535 {
		t.Errorf("the port bounds moved to %d-%d; every surface that sets a port reads these, "+
			"and the console's copy is asserted against them in internal/console",
			model.MinPort, model.MaxPort)
	}
	if model.MaxReplicas != 50 {
		t.Errorf("the replica ceiling moved to %d; the API's own message and the console's both name it",
			model.MaxReplicas)
	}
}

// TestValidateBranchNameAgreesWithGit is the check that makes the rules above
// verifiable rather than merely asserted.
//
// Every name this validator accepts has to be one git accepts, because a name it
// let through and git refused would fail at the push with a message from git
// about a ref rather than from AppLab about the name. The converse is deliberately
// not asserted: this refuses two shapes git allows — a leading dash and a space —
// and the table above says why.
//
// git is skipped rather than required, since it is not needed to build or test the
// rest of the package.
func TestValidateBranchNameAgreesWithGit(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH; skipping the cross-check against git's own rule")
	}

	accepted := []string{
		"main", "dev", "feature/branches", "release-1.2", "v1.0.0", "_working",
		"a", "a/b/c", "UPPER", "with.dots", "with-dashes", "with_underscores",
		"123", "a1", strings.Repeat("a", 200),
	}
	for _, name := range accepted {
		if err := model.ValidateBranchName(name); err != nil {
			t.Errorf("ValidateBranchName(%q) = %v, but git accepts it", name, err)
			continue
		}
		if out, err := exec.Command(gitBin, "check-ref-format", "--branch", name).CombinedOutput(); err != nil {
			t.Errorf("the validator accepts %q and git does not: %v (%s)", name, err, strings.TrimSpace(string(out)))
		}
	}
}

// TestActiveBranchIsNeverEmpty asserts the one resolution there is.
func TestActiveBranchIsNeverEmpty(t *testing.T) {
	if got := (model.App{}).ActiveBranch(); got != model.DefaultBranch {
		t.Errorf("an app with no branch active is on %q, want %q", got, model.DefaultBranch)
	}
	if got := (model.App{Branch: "dev"}).ActiveBranch(); got != "dev" {
		t.Errorf("an app on dev is on %q", got)
	}
	// The default is itself a valid branch, or a new app would be created on a
	// branch the validator refuses.
	if err := model.ValidateBranchName(model.DefaultBranch); err != nil {
		t.Errorf("the default branch is not a valid branch name: %v", err)
	}
}

func explain(why string) string {
	if why == "" {
		return ""
	}
	return " — " + why
}
