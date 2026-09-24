package api

import (
	"net/http"

	"github.com/shaowenchen/applab/internal/source"
)

// handleAgentFile serves one of the files AppLab keeps in an app's source tree.
//
// It exists so those files can update themselves. They are committed into every
// repository, which means the copy someone has is as old as their last upload —
// and AppLab's own API changes between releases, so a stale script can call an
// endpoint that no longer exists or miss one that does.
//
// Serving them from the running server is what makes that recoverable: the
// content comes from the same embedded files the injector uses, so what a script
// fetches is exactly what the deployment would write on the next upload. There
// is no second copy to keep in step.
//
// Authenticated with the app's own key — the {app} in the path is scoped by the
// same middleware as every other app route — because the file names an app and
// is being handed to something that acts on it. It is not a secret, but it is
// per-app and there is no reason to serve it to a caller who has presented
// nothing.
func (s *Server) handleAgentFile(w http.ResponseWriter, r *http.Request) {
	appID := r.PathValue("app")
	name := r.PathValue("file")

	file, ok := source.AgentFile(appID, name)
	if !ok {
		fail(w, r, NotFound("no such AppLab file %q; this deployment serves %v", name, source.AgentFileNames()))
		return
	}

	// text/plain rather than a download type: a shell script and a markdown
	// document are both text, and a browser or a curl asked for one should get
	// something it can read rather than a file it has to save first.
	writeText(w, http.StatusOK, "text/plain", file.Body)
}

// handleAgentFiles lists them.
func (s *Server) handleAgentFiles(w http.ResponseWriter, r *http.Request) {
	appID := r.PathValue("app")
	names := source.AgentFileNames()
	files := make([]map[string]string, 0, len(names))
	for _, name := range names {
		files = append(files, map[string]string{
			"name": name,
			"url":  s.baseURL(r) + "/api/v1/apps/" + appID + "/agent/files/" + name,
		})
	}
	respond(w, http.StatusOK, map[string]any{"files": files})
}
