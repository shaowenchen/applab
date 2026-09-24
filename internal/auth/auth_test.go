package auth

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAuthenticated covers the credential-checking rules directly, without the
// HTTP layer, so a failure points at the rule rather than at the wiring.
func TestAuthenticated(t *testing.T) {
	a := New([]string{"first-key", "second-key"})

	cases := []struct {
		name      string
		presented string
		want      bool
	}{
		{"first configured key", "first-key", true},
		{"second configured key", "second-key", true},
		{"unknown key", "nope", false},
		{"empty", "", false},
		{"whitespace only", "   ", false},
		{"key with surrounding whitespace", "  first-key  ", true},
		{"prefix of a real key", "first", false},
		{"real key plus a suffix", "first-key-x", false},
		{"case differs", "FIRST-KEY", false},
		{"newline appended", "first-key\n", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.Authenticated(tc.presented); got != tc.want {
				t.Errorf("Authenticated(%q) = %v, want %v", tc.presented, got, tc.want)
			}
		})
	}
}

// TestEmptyKeysAreDropped asserts that a stray separator in configuration does
// not become a key that matches an empty credential — which would open the API
// to anyone who sends no Authorization header at all.
func TestEmptyKeysAreDropped(t *testing.T) {
	// The shape a deployment gets from "APPLAB_KEYS=real-key,," or from an
	// unset variable that expanded to nothing.
	a := New([]string{"", "  ", "real-key", ""})

	if a.Authenticated("") {
		t.Error("an empty configured key became a wildcard; an empty credential must never authenticate")
	}
	if a.Authenticated("   ") {
		t.Error("a whitespace-only credential authenticated")
	}
	if !a.Authenticated("real-key") {
		t.Error("the real key did not authenticate")
	}
}

// TestNoKeysAuthenticatesNothing pins the boot invariant's consequence: a
// deployment with no keys refuses everything rather than accepting everything.
// config.Load rejects that configuration outright, so this is defence in depth.
func TestNoKeysAuthenticatesNothing(t *testing.T) {
	a := New(nil)

	for _, presented := range []string{"", "anything", "Bearer"} {
		if a.Authenticated(presented) {
			t.Errorf("Authenticated(%q) = true with no keys configured", presented)
		}
	}
}

// TestKeyFromRequest covers how a credential is read, including the forms a
// caller pastes by hand.
func TestKeyFromRequest(t *testing.T) {
	// Built rather than written out, so the case names stay readable.
	basic := func(user, pass string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	}

	cases := []struct {
		name    string
		header  string
		wantKey string
	}{
		{"bearer scheme", "Bearer abc123", "abc123"},
		{"lowercase bearer", "bearer abc123", "abc123"},
		{"uppercase bearer", "BEARER abc123", "abc123"},
		{"mixed case bearer", "BeArEr abc123", "abc123"},
		{"bare key", "abc123", "abc123"},
		{"extra spaces after scheme", "Bearer    abc123", "abc123"},
		{"trailing spaces", "Bearer abc123   ", "abc123"},
		{"no header", "", ""},
		{"bare scheme word", "Bearer", "Bearer"},

		// Basic is what git sends when the credential is in the URL, which is
		// the spelling anyone reaches for: git clone https://user:key@host/...
		{"basic, app id as user", basic("shop", "abc123"), "abc123"},
		{"basic, git as user", basic("git", "abc123"), "abc123"},
		{"basic, x as user", basic("x", "abc123"), "abc123"},
		{"basic, empty user", basic("", "abc123"), "abc123"},
		{"lowercase basic", "basic " + base64.StdEncoding.EncodeToString([]byte("shop:abc123")), "abc123"},
		// A key pasted into the username field of a prompt, with the password
		// left empty, sends "abc123:" — which is not the same as no colon at
		// all, and both are worth accepting since neither is a mistake anyone
		// would recognise as one.
		{"basic, key in user and empty password", basic("abc123", ""), "abc123"},
		{"basic with no colon at all", "Basic " + base64.StdEncoding.EncodeToString([]byte("abc123")), "abc123"},
		// A password holding a colon keeps all of it: the split is on the first.
		{"basic with a colon in the password", basic("shop", "ab:cd"), "ab:cd"},
		// Not valid base64. Returned as-is so it is refused by comparison rather
		// than looking like no credential at all.
		{"basic that is not base64", "Basic not!base64", "not!base64"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			if got := KeyFromRequest(req); got != tc.wantKey {
				t.Errorf("KeyFromRequest with %q = %q, want %q", tc.header, got, tc.wantKey)
			}
		})
	}
}

// TestBasicCredentialAuthenticates is the end-to-end half: a key read from a
// Basic header is accepted by the same comparison as one read from a Bearer.
//
// The unit test above pins what is extracted; this pins that the extraction
// reaches Authenticated at all, which is the thing a clone depends on.
func TestBasicCredentialAuthenticates(t *testing.T) {
	a := New([]string{"real-key"})

	header := "Basic " + base64.StdEncoding.EncodeToString([]byte("shop:real-key"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", header)

	if !a.Authenticated(KeyFromRequest(req)) {
		t.Error("a key presented as a Basic credential was refused; git clone against a URL carrying the key would not work")
	}

	// And a wrong password is still refused, so the form is not a bypass.
	bad := "Basic " + base64.StdEncoding.EncodeToString([]byte("shop:wrong"))
	req.Header.Set("Authorization", bad)
	if a.Authenticated(KeyFromRequest(req)) {
		t.Error("a wrong key presented as Basic was accepted")
	}
}

// TestKeyFromRequestIgnoresQueryAndCookie asserts the credential is read from
// exactly one place.
//
// A key accepted from a URL would be recorded in access logs, kept in shell
// history and leaked in Referer headers; a key accepted from a cookie would make
// a browser session a credential that scripts could carry. Both are refused.
func TestKeyFromRequestIgnoresQueryAndCookie(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/?key=abc123&api_key=abc123&token=abc123", nil)
	req.Header.Set("Cookie", "applab_key=abc123")

	if got := KeyFromRequest(req); got != "" {
		t.Errorf("KeyFromRequest read a credential from somewhere other than the header: %q", got)
	}
}

// TestMiddlewareRejects covers the middleware's own behaviour, including the
// challenge header that tells a client which scheme to use.
func TestMiddlewareRejects(t *testing.T) {
	a := New([]string{"good-key"})
	sentinel := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentinel = true
		w.WriteHeader(http.StatusOK)
	})
	h := a.Middleware(next)

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if sentinel {
		t.Error("the protected handler ran despite a failed authentication")
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Error("no WWW-Authenticate header on a 401; a client cannot know which scheme to use")
	}
}

func TestMiddlewareAccepts(t *testing.T) {
	a := New([]string{"good-key"})
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	})
	h := a.Middleware(next)

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer good-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !reached {
		t.Error("the handler did not run for a valid key")
	}
}
