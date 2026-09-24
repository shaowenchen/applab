package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/shaowenchen/applab/internal/auth"
)

// identityKey is the context key under which the middleware records who a
// request is.
//
// The identity is carried in the context rather than re-derived by each handler
// that needs it. Re-resolving would be a second look at the key store — and that
// store is the cluster, so asking twice costs a second API call per request and
// could in principle disagree with what the middleware enforced, if the key were
// rotated between the two calls. One resolution, one answer.
type identityKey struct{}

// identityFrom returns the identity the app-auth middleware established.
//
// A request that reached a handler without passing through that middleware
// reports the admin identity: every route that reads this is one an app key
// could only reach through the middleware, and the zero value is the admin tier
// — so the fallback is the reading that matches the routes' own declarations.
func identityFrom(ctx context.Context) auth.Identity {
	if identity, ok := ctx.Value(identityKey{}).(auth.Identity); ok {
		return identity
	}
	return auth.Identity{}
}

// withIdentity returns a request carrying an established identity.
func withIdentity(r *http.Request, identity auth.Identity) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), identityKey{}, identity))
}

// appAuthMiddleware authenticates a route against both key tiers.
//
// The rule it enforces is short: an admin key may do anything, and an app key
// may do anything *to its own app* — except destroy it. Everything else in this
// file exists to make those two sentences true without leaking, through an error
// message or a status code, anything about apps the caller does not own.
func (s *Server) appAuthMiddleware(next http.Handler, adminOnly bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, err := s.auth.Identify(r.Context(), r, s.appKeyResolver())
		if err != nil {
			// A resolver failure is the cluster being unreachable, not a bad
			// credential. Reporting it as 401 would tell an operator their key is
			// wrong when the truth is that the API server is down — and it would
			// send them to rotate a key that was never the problem.
			if !errors.Is(err, auth.ErrUnauthenticated) {
				fail(w, r, Errorf(http.StatusServiceUnavailable,
					"the app key store is unavailable, so credentials cannot be checked").Wrap(err))
				return
			}
			unauthorized(w)
			return
		}

		if identity.Admin() {
			next.ServeHTTP(w, withIdentity(r, identity))
			return
		}

		appID := r.PathValue("app")
		if appID == "" {
			// A route needing an app key to scope to, reached without one. This
			// is a routing mistake rather than a caller's, and it must not fall
			// through to the handler where the scope would be silently absent.
			slog.ErrorContext(r.Context(), "an AppAuth route has no {app} in its path, so an app key cannot be scoped",
				"pattern", r.Pattern, "path", r.URL.Path)
			fail(w, r, Errorf(http.StatusInternalServerError, "this route cannot be addressed with an app key"))
			return
		}

		// An app key naming a different app is answered as "no such app", not
		// "forbidden". A 403 would confirm that the name is taken, which turns
		// any route into a directory of every app this installation manages —
		// readable by anyone holding one app's key.
		if identity.App != appID {
			fail(w, r, NotFound("app %q", appID))
			return
		}

		// The one thing an app key may not do to its own app. Checked after the
		// scope check so that an attempt on someone else's app is a 404 rather
		// than a 403 that would disclose it exists.
		if adminOnly {
			fail(w, r, Errorf(http.StatusForbidden,
				"an app key may manage this app but not delete it; use an admin key"))
			return
		}

		next.ServeHTTP(w, withIdentity(r, identity))
	})
}

// appListAuthMiddleware authenticates a collection route against both tiers.
//
// It differs from appAuthMiddleware in one way: there is no {app} in the pattern
// to compare the key's app against, so it establishes *who* the caller is and
// leaves narrowing the result set to the handler. That is the only honest option
// — the middleware cannot know which subset of a collection a route means — and
// it is why the flag is separate rather than shared: a route that reads this way
// is one whose handler is responsible for scoping, which should be visible in
// the route table rather than inferred.
func (s *Server) appListAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, err := s.auth.Identify(r.Context(), r, s.appKeyResolver())
		if err != nil {
			if !errors.Is(err, auth.ErrUnauthenticated) {
				fail(w, r, Errorf(http.StatusServiceUnavailable,
					"the app key store is unavailable, so credentials cannot be checked").Wrap(err))
				return
			}
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, withIdentity(r, identity))
	})
}

// appKeyResolver returns the resolver the auth layer should use, or nil.
//
// A store that cannot reach the cluster is reported as nil rather than being
// passed on: without a cluster there is nothing to resolve a key against, and
// the admin tier keeps working — which is the whole point of applab being
// runnable with no cluster at all.
func (s *Server) appKeyResolver() auth.AppKeyResolver {
	if s.appKeys == nil || !s.appKeys.Ready() {
		return nil
	}
	return s.appKeys
}

// AuthorizeGitRepo reports whether the request may reach one app's repository.
//
// It exists because the git transport is mounted as a single prefix covering
// every repository, so the route table cannot scope it: there is no {app} in a
// pattern to compare a key against. The check is therefore made where the
// repository name becomes known, which is inside the transport, and this is the
// decision it calls back into.
//
// The admin tier is accepted outright. Otherwise the key must belong to the app
// whose repository was named — an app key that could clone a neighbour's source
// would make the whole tier pointless, since source is where the secrets in a
// repository live.
//
// Every refusal is a plain false; the caller renders it as the same 404 an
// unknown repository gives.
func (s *Server) AuthorizeGitRepo(r *http.Request, appID string) bool {
	if appID == "" {
		return false
	}
	identity, err := s.auth.Identify(r.Context(), r, s.appKeyResolver())
	if err != nil {
		return false
	}
	return identity.Admin() || identity.App == appID
}

// unauthorized writes the same refusal the admin middleware does.
//
// Both tiers produce one identical 401, so a caller cannot learn from the
// response whether a key merely was not recognised or belonged to no app — and
// so an app key that was revoked is indistinguishable from one that never
// existed.
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="applab"`)
	http.Error(w, "unauthorized: present an API key as \"Authorization: Bearer <key>\"", http.StatusUnauthorized)
}
