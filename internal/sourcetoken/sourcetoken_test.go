package sourcetoken

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestIssueAndRedeem covers the happy path.
func TestIssueAndRedeem(t *testing.T) {
	issuer := NewIssuer(time.Minute)

	token, err := issuer.Issue("shop", "main", strings.Repeat("a", 40))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if token.Value == "" {
		t.Fatal("Issue returned an empty token")
	}

	grant, err := issuer.Redeem(token.Value)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if grant.AppID != "shop" {
		t.Errorf("grant AppID = %q, want %q", grant.AppID, "shop")
	}
	if grant.CommitSHA != strings.Repeat("a", 40) {
		t.Errorf("grant CommitSHA = %q, want the commit that was issued for", grant.CommitSHA)
	}
}

// TestRedeemIsSingleUse is the property that makes a token safe to put in a pod
// spec: a build fetches its source once, so a token that has been used — or that
// someone copied out of a pod — must not work a second time.
func TestRedeemIsSingleUse(t *testing.T) {
	issuer := NewIssuer(time.Minute)

	token, err := issuer.Issue("shop", "main", strings.Repeat("a", 40))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if _, err := issuer.Redeem(token.Value); err != nil {
		t.Fatalf("first Redeem: %v", err)
	}

	if _, err := issuer.Redeem(token.Value); err == nil {
		t.Error("a token was redeemed twice; it must be consumed on first use")
	}
	if got := issuer.Len(); got != 0 {
		t.Errorf("%d grants remain after redemption, want 0", got)
	}
}

// TestUnknownTokenIsRefused asserts a made-up token grants nothing.
func TestUnknownTokenIsRefused(t *testing.T) {
	issuer := NewIssuer(time.Minute)

	for _, value := range []string{"", "not-a-token", strings.Repeat("x", 43)} {
		if _, err := issuer.Redeem(value); err == nil {
			t.Errorf("Redeem(%q) succeeded; an unknown token must be refused", value)
		}
	}
}

// TestExpiredTokenIsRefused asserts the TTL actually bounds a token's life.
func TestExpiredTokenIsRefused(t *testing.T) {
	issuer := NewIssuer(time.Minute)

	// Move the clock rather than sleeping, so the test is instant and does not
	// depend on scheduling.
	now := time.Now()
	issuer.now = func() time.Time { return now }

	token, err := issuer.Issue("shop", "main", strings.Repeat("a", 40))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	issuer.now = func() time.Time { return now.Add(2 * time.Minute) }

	if _, err := issuer.Redeem(token.Value); err == nil {
		t.Error("an expired token was accepted")
	} else if !strings.Contains(err.Error(), "expired") {
		t.Errorf("the error should say the token expired, got: %v", err)
	}
}

// TestExpiredTokenIsConsumed asserts an expired token is removed even when
// refused, so it cannot be retried into existence.
func TestExpiredTokenIsConsumed(t *testing.T) {
	issuer := NewIssuer(time.Minute)
	now := time.Now()
	issuer.now = func() time.Time { return now }

	token, _ := issuer.Issue("shop", "main", strings.Repeat("a", 40))
	issuer.now = func() time.Time { return now.Add(2 * time.Minute) }

	_, _ = issuer.Redeem(token.Value)

	if got := issuer.Len(); got != 0 {
		t.Errorf("%d grants remain after an expired token was presented, want 0", got)
	}
}

// TestTokensAreUnique asserts two tokens for the same app and commit differ, so
// one build's token cannot be used to fetch another's source.
func TestTokensAreUnique(t *testing.T) {
	issuer := NewIssuer(time.Minute)

	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		token, err := issuer.Issue("shop", "main", strings.Repeat("a", 40))
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		if seen[token.Value] {
			t.Fatal("Issue produced a duplicate token")
		}
		seen[token.Value] = true
	}
}

// TestIssuePrunesExpired asserts the grant map does not grow without bound. An
// unredeemed token — a build that was created and then abandoned — leaves an
// entry nothing else removes.
func TestIssuePrunesExpired(t *testing.T) {
	issuer := NewIssuer(time.Minute)
	now := time.Now()
	issuer.now = func() time.Time { return now }

	for i := 0; i < 50; i++ {
		if _, err := issuer.Issue("shop", "main", strings.Repeat("a", 40)); err != nil {
			t.Fatalf("Issue: %v", err)
		}
	}
	if got := issuer.Len(); got != 50 {
		t.Fatalf("outstanding grants = %d, want 50", got)
	}

	// Move past the TTL and issue one more; the prune should have swept the rest.
	issuer.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, err := issuer.Issue("shop", "main", strings.Repeat("a", 40)); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if got := issuer.Len(); got != 1 {
		t.Errorf("outstanding grants = %d, want 1 (the expired ones should have been pruned)", got)
	}
}

// TestConcurrentIssueAndRedeem asserts the issuer is safe under the concurrent
// use a running server produces: several builds starting while others fetch.
func TestConcurrentIssueAndRedeem(t *testing.T) {
	issuer := NewIssuer(time.Minute)

	const workers = 50
	var wg sync.WaitGroup
	wg.Add(workers)

	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()

			token, err := issuer.Issue("shop", "main", strings.Repeat("a", 40))
			if err != nil {
				errs <- err
				return
			}
			if _, err := issuer.Redeem(token.Value); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent issue/redeem failed: %v", err)
	}
	if got := issuer.Len(); got != 0 {
		t.Errorf("%d grants remain, want 0", got)
	}
}

// TestDefaultTTLIsShort asserts the default is not accidentally generous: a
// token only has to outlive the gap between a Job being created and its init
// container starting.
func TestDefaultTTLIsShort(t *testing.T) {
	if DefaultTTL > time.Hour {
		t.Errorf("DefaultTTL = %v; a source token should not stay usable for that long", DefaultTTL)
	}
	if DefaultTTL < 5*time.Minute {
		t.Errorf("DefaultTTL = %v; too short for a job to be scheduled and start", DefaultTTL)
	}

	// A non-positive TTL falls back to the default rather than meaning "never
	// expires".
	issuer := NewIssuer(0)
	if issuer.ttl != DefaultTTL {
		t.Errorf("ttl = %v with a zero TTL passed, want the default %v", issuer.ttl, DefaultTTL)
	}
	issuer = NewIssuer(-time.Hour)
	if issuer.ttl != DefaultTTL {
		t.Errorf("ttl = %v with a negative TTL passed, want the default %v", issuer.ttl, DefaultTTL)
	}
}
