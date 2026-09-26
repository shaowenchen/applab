package api

import (
	"log/slog"
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
// is being handed to something that acts on it. It is a secret, in fact: the
// script carries the app's key. Handing it to a caller who presented nothing
// would be handing out a credential.
//
// The rendered copy is the one served, key and all, so `./applab.sh self-update`
// does not wipe the credential the checkout was working with — the file that
// arrives is the same file the next upload would write, which is the whole point
// of fetching it here.
func (s *Server) handleAgentFile(w http.ResponseWriter, r *http.Request) {
	appID := r.PathValue("app")
	name := r.PathValue("file")

	file, ok := source.AgentFile(s.agentFileValues(r, appID), name)
	if !ok {
		fail(w, r, NotFound("no such AppLab file %q; this deployment serves %v", name, source.AgentFileNames()))
		return
	}

	// text/plain rather than a download type: a shell script and a markdown
	// document are both text, and a browser or a curl asked for one should get
	// something it can read rather than a file it has to save first.
	writeText(w, http.StatusOK, "text/plain", file.Body)
}

// agentFileValues assembles what the seeded files are rendered against for one
// app: its id, the address people reach this deployment at, and its key.
//
// It mirrors what the source store does when it writes those files into a tree,
// and the two have to agree — `self-update` fetches the served copy and moves it
// over the one in the checkout, so a served file rendered without the key would
// delete the key from a working tree.
//
// A key that cannot be read is not an error, for the same reason it is not one
// on the write path: the file is rendered without it and says where to get one.
func (s *Server) agentFileValues(r *http.Request, appID string) source.SeedValues {
	v := source.SeedValues{App: appID, URL: s.publicURL(r)}
	if s.appKeys == nil {
		return v
	}
	key, err := s.appKeys.Get(r.Context(), appID)
	if err != nil {
		// Logged rather than reported: the caller asked for a script, and a
		// script without a key is still a script. A 500 here would make an app
		// whose key was never minted unable to fetch its own documentation.
		slog.DebugContext(r.Context(), "serving the AppLab files without an app key",
			"app", appID, "error", err)
		return v
	}
	v.Key = key
	return v
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
