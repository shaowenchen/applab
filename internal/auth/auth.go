// Package auth decides whether a request may act on this deployment.
//
// There is no user store. An API key is the whole identity: presenting a
// configured key means "you may do anything this service can do", and nothing
// is looked up beyond the keys configured at boot. That is what lets an
// instance be replaced at any moment with no database to fail over — a key is
// rotated by restarting with a new value.
//
// There is deliberately one tier. A key that authenticates may also delete.
// The consequence is worth stating plainly: every key is a credential that can
// destroy data, so hand one out the way you would hand out any other such
// credential.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// Authenticator checks presented credentials against the configured keys.
type Authenticator struct {
	// digests holds the SHA-256 of each configured key rather than the keys
	// themselves. Comparison then happens over fixed-length values, which is
	// what allows a constant-time compare: subtle.ConstantTimeCompare returns
	// immediately on a length mismatch, so comparing raw keys would leak each
	// configured key's length to anyone probing the endpoint.
	digests [][sha256.Size]byte
}

// New returns an Authenticator over keys. A key that is empty after trimming is
// dropped rather than becoming a wildcard, so a stray comma in a configuration
// cannot open the API.
func New(keys []string) *Authenticator {
	a := &Authenticator{}
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		a.digests = append(a.digests, sha256.Sum256([]byte(k)))
	}
	return a
}

// Authenticated reports whether presented is one of the configured keys.
//
// Every digest is compared even after a match, rather than returning early:
// stopping at the first match would make the response time depend on which key
// matched — that is, on its position in the configured list — and a caller has
// no business learning that.
func (a *Authenticator) Authenticated(presented string) bool {
	presented = strings.TrimSpace(presented)
	if presented == "" {
		return false
	}
	sum := sha256.Sum256([]byte(presented))

	matched := false
	for _, want := range a.digests {
		if subtle.ConstantTimeCompare(sum[:], want[:]) == 1 {
			matched = true
		}
	}
	return matched
}

// KeyFromRequest extracts the presented credential, if any.
//
// Only a header is read. A key is never accepted as a query parameter or a form
// field: URLs are written to access logs, kept in shell history and sent in
// Referer headers, so a credential placed in one leaks by default.
//
// Both "Authorization: Bearer <key>" and a bare "Authorization: <key>" are
// accepted, because a caller pasting a key by hand routinely omits the scheme.
func KeyFromRequest(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return ""
	}
	if rest, ok := cutPrefixFold(header, "bearer "); ok {
		return strings.TrimSpace(rest)
	}
	return header
}

// cutPrefixFold trims prefix from s when it matches case-insensitively, which
// is what RFC 7235 requires for the auth scheme.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) {
		return "", false
	}
	if !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}

// Middleware rejects any request that does not carry a configured key.
//
// It is applied to every route that serves or mutates data, so a route cannot
// become reachable by someone forgetting to opt in.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.Authenticated(KeyFromRequest(r)) {
			// The challenge is what tells a client the scheme to use; without it
			// a 401 is unactionable for anything but a human reading prose.
			w.Header().Set("WWW-Authenticate", `Bearer realm="applab"`)
			http.Error(w, "unauthorized: present an API key as \"Authorization: Bearer <key>\"", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
