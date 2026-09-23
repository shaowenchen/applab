package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/shaowenchen/applab/internal/model"
)

// tokenAuthMiddleware requires a single-use source token.
//
// It exists for the one route a build Job calls. The alternative — giving a build
// Job an API key, since fetching source is just another API call — would put a
// credential that can delete every app this installation manages inside a
// namespace where a build runs arbitrary code from whoever pushed the source. A
// token instead reaches exactly one commit of one app, for a few minutes, once.
//
// The token is read from the same Authorization header as an API key, so a client
// needs to know only one way to present a credential. The routes are distinct, so
// there is no ambiguity about which kind is expected.
func (s *Server) tokenAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.sourceTokens == nil {
			fail(w, r, Errorf(http.StatusNotImplemented, "this deployment does not issue source tokens"))
			return
		}

		token := bearerToken(r)
		if token == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="applab-source"`)
			fail(w, r, Errorf(http.StatusUnauthorized, "present a source token as \"Authorization: Bearer <token>\""))
			return
		}

		grant, err := s.sourceTokens.Redeem(token)
		if err != nil {
			// Not retryable: the token is consumed on use, so the same value can
			// never succeed again. Saying so stops a client from looping.
			fail(w, r, Errorf(http.StatusUnauthorized, "%s", err.Error()))
			return
		}

		// The token names the app and the commit, so the request has to match
		// both. Without this a token for one app would read another's source by
		// changing the path.
		if appID := r.PathValue("app"); appID != grant.AppID {
			fail(w, r, NotFound("app %q", appID))
			return
		}
		if sha := r.PathValue("sha"); sha != grant.CommitSHA {
			fail(w, r, NotFound("commit %q in app %q", sha, grant.AppID))
			return
		}

		next.ServeHTTP(w, r)
	})
}

// handleSourceArchive streams a commit's source as a tar.gz.
//
// It is what a build Job's init container calls, and it is the only route
// authenticated by a source token rather than by the API key.
func (s *Server) handleSourceArchive(w http.ResponseWriter, r *http.Request) {
	appID := r.PathValue("app")
	sha := r.PathValue("sha")

	if s.sourceArchive == nil {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment has no source storage configured"))
		return
	}

	if !model.ValidSHA(sha) {
		fail(w, r, BadRequest("commit must be a full 40-character commit id"))
		return
	}

	// The size is computed first so the response carries a Content-Length: a
	// client can then report progress, and a truncated transfer is detectable
	// rather than looking like a short but complete archive.
	size, err := s.sourceArchiveSize(r.Context(), appID, sha)
	if err != nil {
		fail(w, r, ingestError(err))
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", appID+"-"+shortSHA(sha)+".tar.gz"))
	w.WriteHeader(http.StatusOK)

	// Written after the header, so a failure here can only be reported by the
	// connection ending — the client's tar will notice a truncated stream, which
	// is the honest signal available.
	if err := s.sourceArchive(r.Context(), appID, sha, w); err != nil {
		// A client that disconnected mid-download is the common case and is not
		// worth logging as a failure.
		if !errors.Is(err, r.Context().Err()) {
			fail(w, r, Errorf(http.StatusInternalServerError, "stream source archive").Wrap(err))
		}
	}
}

// bearerToken extracts a token, accepting the bare form as well as the scheme.
//
// The bare form is accepted because a caller pasting a credential by hand
// routinely omits "Bearer", and refusing it would be a puzzle rather than a
// security boundary.
func bearerToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if rest, ok := cutPrefixFold(header, "bearer "); ok {
		return strings.TrimSpace(rest)
	}
	return header
}

// cutPrefixFold trims prefix from s when it matches case-insensitively, which is
// what RFC 7235 requires for the auth scheme.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}
