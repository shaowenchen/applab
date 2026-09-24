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
