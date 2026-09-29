package api

import (
	"net/http"

	"github.com/shaowenchen/applab/internal/source"
)

// handleBootstrapScript serves applab.sh with no app and no key in it.
//
// It exists for the person who has neither yet. The script AppLab writes into a
// repository is rendered for one app and carries that app's key, which is
// exactly what makes a fresh clone work with no setup — and exactly what makes
// it useless as a starting point. Someone with nothing to clone had no way to
// get the tool: the only route serving it is app-scoped and needs a key for the
// app they do not have.
//
// So this serves the same file, rendered against the deployment's address and
// nothing else. APP and the key are empty, and the script says what to do about
// that — see its `create` and `use` commands, and the check at the top that
// names APPLAB_KEY when it is missing.
//
// **Unauthenticated**, and deliberately. It is the same bytes for every caller,
// it names no app, and it holds no credential — a script with an empty key is
// not a secret, and requiring one to fetch it would defeat the whole point. That
// is the line: this route serves the file and never an app's copy of it, so
// nothing here can leak one app's key to another's reader.
//
// What it does hand out is the deployment's public address, which is already
// public — the console, the API and every app answer on it.
func (s *Server) handleBootstrapScript(w http.ResponseWriter, r *http.Request) {
	file, ok := source.BootstrapFile(source.SeedValues{URL: s.publicURL(r)})
	if !ok {
		// Unreachable: the bootstrap name is one of the seeded files.
		fail(w, r, NotFound("this deployment serves no bootstrap script"))
		return
	}

	// text/plain for the same reason the app-scoped route uses it: a shell
	// script is text, and `curl <url> | sh` should read it rather than have to
	// save it first.
	writeText(w, http.StatusOK, "text/plain", file.Body)
}

// handleBootstrapFiles lists what the bootstrap route serves.
//
// A listing rather than a bare route so a caller can find the file without
// knowing its name, which is the position someone in this situation is in.
func (s *Server) handleBootstrapFiles(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, map[string]any{
		"files": []map[string]string{
			{
				"name": "applab.sh",
				// A person fetches this, so it is the public address: the
				// in-cluster one resolves only inside the cluster. See publicURL.
				"url":  s.publicURL(r) + "/bootstrap/applab.sh",
				"note": "The same script AppLab writes into every app's repository, with no app and no key in it. Fetch it, export an admin key, and run `./applab.sh create <app>`.",
			},
		},
	})
}
