package api_test

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// TestAGitClientCanAuthenticateWithAKeyInTheURL is the end-to-end check that a
// clone works, run with the real git binary.
//
// It exists because the unit test above and every curl in the world pass while a
// clone fails. The failure is in a protocol nobody here controls: git holds the
// credentials from a URL back until the server challenges for them with `Basic`,
// so what matters is not that the 401 is well-formed but that git — actually git
// — comes back with the credential after seeing it.
//
// curl cannot stand in for this. `curl -u` sends Basic preemptively and never
// exercises the challenge at all, which is why the bug survived a manual test.
//
// Skipped when git is not on PATH: this is worth testing but not worth a
// runtime dependency for everyone building the server.
func TestAGitClientCanAuthenticateWithAKeyInTheURL(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH; skipping the clone handshake check")
	}

	srv, _ := newTieredServer(t)

	// What the transport sees once it is reached. The middleware refuses first
	// when the credential is missing or wrong, so anything recorded here arrived
	// with one that was accepted.
	var mu sync.Mutex
	var seen string
	srv.WithGit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r.Header.Get("Authorization")
		mu.Unlock()
		// Enough of an answer that git stops, without pretending to be a real
		// transport: this test is about the handshake, not about packing objects.
		http.Error(w, "reached the transport", http.StatusInternalServerError)
	}))

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// The form the console hands out: the key as the password of the URL.
	url := strings.Replace(ts.URL, "http://", "http://x:"+adminKey+"@", 1) + "/git/shop.git"

	// GIT_TERMINAL_PROMPT=0 so a challenge git cannot satisfy fails instead of
	// stopping to ask a human for a password, which in a test would hang.
	cmd := exec.Command(gitBin, "ls-remote", url)
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, _ := cmd.CombinedOutput()

	mu.Lock()
	got := seen
	mu.Unlock()

	if got == "" {
		t.Fatalf("git never presented the key from the clone URL — the transport was not reached.\n"+
			"This is what a `Bearer` challenge does: git has nothing to answer it with and gives up.\n"+
			"git said: %s", strings.TrimSpace(string(out)))
	}
	if !strings.HasPrefix(strings.ToLower(got), "basic ") {
		t.Errorf("git presented %q, want a Basic credential — that is the only form a clone URL produces", got)
	}
}

// TestTheGitTransportRefusesAnUnauthenticatedClone is the same setup with no
// credential, asserting the handshake ends in a refusal rather than a hang or a
// served repository.
func TestTheGitTransportRefusesAnUnauthenticatedClone(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH")
	}

	srv, _ := newTieredServer(t)
	reached := false
	srv.WithGit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		http.Error(w, "reached the transport", http.StatusInternalServerError)
	}))

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cmd := exec.Command(gitBin, "ls-remote", ts.URL+"/git/shop.git")
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.CombinedOutput()

	if reached {
		t.Error("an unauthenticated clone reached the transport; every repository would be public")
	}
}
