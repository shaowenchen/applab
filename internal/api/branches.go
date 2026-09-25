package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/source"
)

// branchesResponse is what the branch endpoints return.
type branchesResponse struct {
	AppID string `json:"app_id"`

	// Branches is every branch the app has a repository for, sorted. An app that
	// has never been pushed to has one: the default, created with the app.
	Branches []string `json:"branches"`

	// Active is the branch a deploy builds from.
	Active string `json:"active"`
}

// handleListBranches reports the branches an app has.
//
// The list comes from the repositories that exist rather than from git refs, so
// it is one listing of one prefix — see source.Store.Branches. That keeps this
// cheap enough to call from a console on every page load, which is where the
// question "what branches are there?" is usually asked.
func (s *Server) handleListBranches(w http.ResponseWriter, r *http.Request) {
	if s.sourceBranches == nil {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment has no source storage configured"))
		return
	}

	app, err := s.loadApp(r)
	if err != nil {
		fail(w, r, err)
		return
	}

	branches, listErr := s.sourceBranches(r.Context(), app.ID)
	if listErr != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "list the branches of app %q", app.ID).Wrap(listErr))
		return
	}

	respond(w, http.StatusOK, branchesResponse{
		AppID:    app.ID,
		Branches: nonNilStrings(branches),
		Active:   app.ActiveBranch(),
	})
}

// switchBranchRequest is the body of PUT .../branch.
type switchBranchRequest struct {
	Branch string `json:"branch"`
}

// handleSwitchBranch makes a branch the app's active one and deploys it.
//
// Setting the field and deploying are one operation rather than two because a
// branch that is active but not deployed is not active in any sense a caller
// cares about: the app would report itself on a branch whose code is not
// running. `PATCH /apps/{app}` can change the field alone, for the case where
// the caller wants to record the choice without a rollout — but the route that
// exists to *switch* does both.
//
// Switching to a branch with no repository is refused rather than accepted and
// left alone: the app would then be pointed at code that does not exist, and the
// next deploy would fail with a message about a missing commit rather than about
// a missing branch.
func (s *Server) handleSwitchBranch(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r)
	if err != nil {
		fail(w, r, err)
		return
	}

	var req switchBranchRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}

	branch := strings.TrimSpace(req.Branch)
	if err := model.ValidateBranchName(branch); err != nil {
		fail(w, r, BadRequest("%s", err.Error()))
		return
	}

	if s.sourceBranches == nil {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment has no source storage configured"))
		return
	}
	exists, listErr := s.branchExists(r, app.ID, branch)
	if listErr != nil {
		fail(w, r, listErr)
		return
	}
	if !exists {
		fail(w, r, NotFound("app %q has no branch %q; push to %s to create it",
			app.ID, branch, gitURLWithPassword(s.cfg.BaseURL, app.ID, branch)))
		return
	}

	if s.deployer == nil || !s.deployer.Ready() {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment cannot deploy: no cluster is configured"))
		return
	}

	// Recorded before the deploy, so a deploy that fails leaves the app on the
	// branch the caller asked for rather than silently staying on the old one.
	// The alternative — roll the record back — would mean a failed rollout also
	// undid a decision that was made deliberately.
	previous := app.ActiveBranch()
	app.Branch = branch
	if err := s.store.UpdateApp(r.Context(), app); err != nil {
		fail(w, r, fromStoreError(err, fmt.Sprintf("app %q", app.ID)))
		return
	}

	// The tip of the branch being switched to. It is read after the record is
	// written and the branch is known to exist, so an app that was just pushed
	// to has something to deploy.
	head, headErr := s.headCommit(r.Context(), app.ID, branch)
	if headErr != nil {
		if errors.Is(headErr, source.ErrNoCommits) {
			fail(w, r, BadRequest("branch %q of app %q has no commits yet", branch, app.ID))
			return
		}
		fail(w, r, Errorf(http.StatusInternalServerError, "read the tip of branch %q", branch).Wrap(headErr))
		return
	}

	slog.InfoContext(r.Context(), "branch switched",
		"app", app.ID, "from", previous, "to", branch, "commit", shortSHA(head))

	// From here it is the deploy path, exactly as POST /deploy runs it — the
	// same image lookup, the same build-or-refuse, the same response. A branch
	// switch that deployed by some other route would be a second implementation
	// of deploying, and the two would differ first in the failure cases.
	//
	// build is true, unlike POST /deploy's default of false. The difference is
	// what the caller is asking for: a deploy of a named commit is a request to
	// run *that*, and being told to build it first is useful; a branch switch is
	// a request to run whatever is on that branch, and a branch that was just
	// pushed has no image yet — so refusing would make the ordinary case fail
	// with advice the caller has already followed.
	s.deployResolved(w, r, app, branch, head, true)
}

// branchExists reports whether an app has a repository for a branch.
func (s *Server) branchExists(r *http.Request, appID, branch string) (bool, *apiError) {
	branches, err := s.sourceBranches(r.Context(), appID)
	if err != nil {
		return false, Errorf(http.StatusInternalServerError, "list the branches of app %q", appID).Wrap(err)
	}
	for _, b := range branches {
		if b == branch {
			return true, nil
		}
	}
	return false, nil
}

// nonNilStrings returns a slice that marshals as [] rather than null.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
