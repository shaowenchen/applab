// Package auth decides whether a request may act on this deployment.
//
// There are two tiers of credential, and the difference between them is reach.
//
// An **admin key** is configured at boot (APPLAB_KEYS) and may do anything this
// service can. There is no user store, so it is the whole identity: presenting
// one means "you may do everything", and a key is rotated by restarting with a
// new value. That is what lets an instance be replaced at any moment with no
// database to fail over.
//
// An **app key** belongs to one app and reaches only that app. It is issued by
// the API when the app is created (see internal/appkey) and resolved per
// request, so it is not a second copy of the configuration but a lookup. Its
// purpose is the one the single tier could not serve: letting someone deploy
// their own app without handing them a credential that can delete every app this
// installation manages.
//
// Where the line between the tiers falls is decided by the API layer, which is
// the only place that knows what a request is addressing — this package answers
// "who is this", not "may they do this".
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
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
// Three forms are accepted:
//
//   - "Authorization: Bearer <key>", what this API's own clients send;
//   - "Authorization: <key>", because a caller pasting a key by hand routinely
//     omits the scheme;
//   - "Authorization: Basic <base64>", because that is the only thing git sends
//     when the credential is in the URL. `git clone https://user:key@host/...`
//     produces Basic and nothing else, so without this a clone against a URL
//     carrying the key would be refused — and the extraHeader spelling would be
//     the only way in, which is not what anyone reaches for.
//
// The Basic password is what is taken, and the username is ignored. Git needs
// *some* username in the URL to send a password at all, and callers fill it with
// whatever comes to mind — the app id, "git", "x" — so requiring a particular
// one would reject working credentials for no gain. A username-only Basic header
// (no colon) is still read as a key, since a caller who pastes a key into the
// username field of a prompt is doing the obvious thing.
func KeyFromRequest(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return ""
	}
	if rest, ok := cutPrefixFold(header, "bearer "); ok {
		return strings.TrimSpace(rest)
	}
	if rest, ok := cutPrefixFold(header, "basic "); ok {
		return basicKey(rest)
	}
	return header
}

// basicKey decodes a Basic credential and returns the password, falling back to
// the username when there is nothing else.
//
// Both fallbacks are the same situation: a person confronted with a username and
// a password field, holding one key. They may put it in the password field with
// the username set to something arbitrary — which is the intended form — or they
// may type it into the username field and leave the password empty, which is
// just as common and not something they would recognise as a mistake.
//
// An undecodable value is returned as-is rather than dropped, so it is compared
// against the configured keys like any other and refused. Returning "" would
// make a malformed header indistinguishable from no header, and the two want
// different answers from the middleware.
func basicKey(encoded string) string {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return encoded
	}
	user, pass, found := strings.Cut(string(raw), ":")
	if !found {
		return strings.TrimSpace(user)
	}
	// The password, not the username: git puts the token there.
	if pass = strings.TrimSpace(pass); pass != "" {
		return pass
	}
	return strings.TrimSpace(user)
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

// Identity is who a request is: which tier, and — for an app key — which app.
type Identity struct {
	// App is the app an app key belongs to. Empty means the admin tier.
	App string
}

// Admin reports whether this identity is the unrestricted tier.
func (i Identity) Admin() bool { return i.App == "" }

// AppKeyResolver maps a presented app key to the app that owns it.
//
// It is an interface rather than a concrete dependency so this package does not
// import the Kubernetes client, and so a test can supply a resolver without a
// cluster. A nil resolver means this deployment has no app keys at all — which
// is the case without a cluster — and then only the admin tier exists.
type AppKeyResolver interface {
	// ResolveAppKey returns the app owning the presented key. ok is false when
	// the key belongs to no app, which is not an error: an unrecognised key is
	// simply not a credential.
	ResolveAppKey(ctx context.Context, presented string) (appID string, ok bool, err error)
}

// Identify reports who a request is, or that it is nobody.
//
// The admin tier is checked first and short-circuits: an admin key is a fixed
// set held in memory, so the common case costs no cluster call, and an admin key
// that happened to equal an app key would still be admin — the more privileged
// reading of an ambiguous credential is not the safe one to pick by accident,
// but it is the one the operator explicitly configured at boot.
//
// A resolver failure is returned rather than swallowed. The caller turns it into
// a 503: a key that could not be checked is not a key that failed to check out,
// and reporting it as 401 would tell an operator their credential is wrong when
// the truth is that the cluster is unreachable.
func (a *Authenticator) Identify(ctx context.Context, r *http.Request, resolver AppKeyResolver) (Identity, error) {
	presented := KeyFromRequest(r)

	if a.Authenticated(presented) {
		return Identity{}, nil
	}

	// An unusable key is not worth a cluster call: an empty or whitespace value
	// cannot be an app key, and asking is a free round trip for an unauthenticated
	// caller to spend.
	if resolver == nil || strings.TrimSpace(presented) == "" {
		return Identity{}, ErrUnauthenticated
	}

	appID, ok, err := resolver.ResolveAppKey(ctx, presented)
	if err != nil {
		return Identity{}, fmt.Errorf("resolve app key: %w", err)
	}
	if !ok {
		return Identity{}, ErrUnauthenticated
	}
	return Identity{App: appID}, nil
}

// ErrUnauthenticated means the presented credential is not one this deployment
// recognises, at either tier.
var ErrUnauthenticated = errors.New("unauthenticated")
