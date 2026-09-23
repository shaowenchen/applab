// Package sourcetoken issues short-lived, single-use credentials that let a
// build Job fetch exactly one commit's source.
//
// The alternative would be to give a build Job applab's own API key, since a
// Job fetching its source is just another API client. That is what must not
// happen: an API key is the whole identity and can delete every app this
// installation manages. A build runs in the app's own namespace, where anyone who
// can read a pod spec or a Secret could lift it — so a build Job must never hold
// a credential with more reach than the one commit it was started to build.
//
// A token therefore names one app and one commit, expires quickly, and is
// consumed on use. Holding one grants the ability to download that single source
// tree and nothing else.
package sourcetoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// DefaultTTL is how long a token stays valid.
//
// It is deliberately short: a build Job fetches its source in the first seconds
// of its life, so a token only has to outlive the gap between the Job being
// created and its init container starting. A generous window would only widen
// the period in which a leaked token is usable.
const DefaultTTL = 30 * time.Minute

// Token is what a build Job is given. It is never stored; only its digest is.
type Token struct {
	// Value is the opaque string to present. It is returned once, at issue time.
	Value string

	AppID     string
	CommitSHA string
	ExpiresAt time.Time
}

// Grant is the record kept for an issued token.
type Grant struct {
	AppID     string
	CommitSHA string
	ExpiresAt time.Time
}

// Issuer issues and redeems tokens.
//
// Grants are held in memory rather than in the database. They are ephemeral by
// design — a token that outlives the process that issued it has outlived its
// purpose — and losing them on restart costs at most one retried build, since
// applab reconciles unfinished builds at startup and can issue a fresh token.
type Issuer struct {
	mu     sync.Mutex
	grants map[string]Grant // keyed by the token's digest, never by the token

	ttl time.Duration

	// now is injectable so expiry can be tested without sleeping.
	now func() time.Time
}

// NewIssuer creates an Issuer.
func NewIssuer(ttl time.Duration) *Issuer {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Issuer{
		grants: make(map[string]Grant),
		ttl:    ttl,
		now:    time.Now,
	}
}

// Issue mints a token for one app and commit.
//
// The token is 32 bytes of cryptographic randomness. It is not derived from the
// app or the commit, so holding one token tells its holder nothing about any
// other, and tokens cannot be enumerated by guessing.
func (i *Issuer) Issue(appID, commitSHA string) (Token, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Token{}, fmt.Errorf("generate source token: %w", err)
	}

	value := base64.RawURLEncoding.EncodeToString(raw[:])
	grant := Grant{
		AppID:     appID,
		CommitSHA: commitSHA,
		ExpiresAt: i.now().Add(i.ttl),
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	i.pruneLocked()

	i.grants[digest(value)] = grant

	return Token{
		Value:     value,
		AppID:     grant.AppID,
		CommitSHA: grant.CommitSHA,
		ExpiresAt: grant.ExpiresAt,
	}, nil
}

// Redeem consumes a token and returns what it grants.
//
// Consumption is the point: a source fetch is not repeatable, so a token that
// has been used cannot be replayed by anyone who saw it in a log or a pod spec.
// The cost is that a build whose init container restarts needs a fresh token,
// which is why the fetch step is written to be retried by applab rather than by
// Kubernetes.
func (i *Issuer) Redeem(value string) (Grant, error) {
	if value == "" {
		return Grant{}, fmt.Errorf("no token presented")
	}
	key := digest(value)

	i.mu.Lock()
	defer i.mu.Unlock()

	grant, ok := i.grants[key]
	if !ok {
		return Grant{}, fmt.Errorf("token is not valid or has already been used")
	}

	// Consumed whether or not it turns out to be expired, so a stale token
	// cannot be retried into existence.
	delete(i.grants, key)

	if i.now().After(grant.ExpiresAt) {
		return Grant{}, fmt.Errorf("token expired at %s", grant.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return grant, nil
}

// Peek reports what a token grants without consuming it.
//
// Used only for diagnostics — a log line saying which commit a fetch was for —
// and never on the path that grants access.
func (i *Issuer) Peek(value string) (Grant, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()

	grant, ok := i.grants[digest(value)]
	return grant, ok
}

// Len reports how many grants are outstanding, for a test or a metric.
func (i *Issuer) Len() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.grants)
}

// pruneLocked drops expired grants.
//
// Without it the map grows for the life of the process: nothing else removes an
// entry whose token was never redeemed, and a build that is created and then
// abandoned leaves one behind every time.
func (i *Issuer) pruneLocked() {
	now := i.now()
	for key, grant := range i.grants {
		if now.After(grant.ExpiresAt) {
			delete(i.grants, key)
		}
	}
}

// digest is the map key for a token.
//
// Grants are keyed by digest rather than by the token itself so that a heap dump
// of this process does not contain usable credentials. The comparison in Redeem
// is a map lookup, which is not constant time — but the key is a hash of a
// 256-bit random value, so there is nothing to learn from its timing: an attacker
// who could exploit a comparison oracle would still have to guess 256 bits.
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
