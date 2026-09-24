package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/shaowenchen/applab/internal/appkey"
)

// appKeyResponse is what the key endpoints return.
//
// The key is returned in full and unredacted, which is the deployment's stated
// choice: it can be read back at any time rather than shown once. The reason is
// that a key nobody can recover is one that has to be rotated the moment it is
// mislaid, and for a per-app credential the person who needs it is whoever is
// deploying that app — not an operator who can mint a fresh one on request.
type appKeyResponse struct {
	AppID string `json:"app_id"`
	Key   string `json:"key"`
}

// handleGetAppKey returns an app's key.
//
// Reaching this route at all means the caller is either an admin or an app key
// for this exact app — that is enforced by the middleware from the {app} in the
// path, so the handler does not re-check the scope. An app key reading its own
// key is intended: the key is the app's identity, and whoever holds it is
// already acting as that app.
func (s *Server) handleGetAppKey(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r)
	if err != nil {
		fail(w, r, err)
		return
	}

	key, keyErr := s.appKeys.Get(r.Context(), app.ID)
	if keyErr != nil {
		// An app with no key is not a server fault: it is what an app created
		// before this feature existed, or one whose key was lost, looks like.
		// Reported as a missing resource with the fix named, because a 500 here
		// would send an operator looking for a bug that is not there.
		if isNoKey(keyErr) {
			fail(w, r, NotFound("app %q has no key yet; create one with POST /api/v1/apps/%s/key/rotate", app.ID, app.ID))
			return
		}
		fail(w, r, Errorf(http.StatusInternalServerError, "read the key for app %q", app.ID).Wrap(keyErr))
		return
	}

	respond(w, http.StatusOK, appKeyResponse{AppID: app.ID, Key: key})
}

// handleRotateAppKey mints a new key for an app, invalidating the old one.
//
// It also serves as "create" for an app that has no key, so a lost credential is
// recovered with the same call rather than an error that tells the caller to
// create it first — the store treats the two the same way.
func (s *Server) handleRotateAppKey(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r)
	if err != nil {
		fail(w, r, err)
		return
	}

	key, keyErr := s.appKeys.Rotate(r.Context(), app.ID)
	if keyErr != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "rotate the key for app %q", app.ID).Wrap(keyErr))
		return
	}

	// Logged because a rotation invalidates a working credential. If a
	// deployment breaks, "was the key rotated" is the first question, and this
	// is the only record of it — the key itself is never logged.
	slog.InfoContext(r.Context(), "app key rotated", "app", app.ID)

	respond(w, http.StatusOK, appKeyResponse{AppID: app.ID, Key: key})
}

// isNoKey reports whether err is the store's "this app has no key" sentinel.
func isNoKey(err error) bool {
	return errors.Is(err, appkey.ErrNoKey)
}
