package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shaowenchen/applab/internal/client"
)

// resolveBuildID is what lets someone paste the eight characters the listing
// shows. Sending them to the server would be a 404 for an id that exists, so the
// expansion happens here — and an ambiguous prefix has to be refused rather than
// guessed at, because cancelling a build cannot be undone by looking it up
// again.
func TestResolveBuildIDExpandsWhatTheListingShows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/apps/shop/builds" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "aaaaaaaa1111222233334444555566667", "commit_sha": "c1"},
			{"id": "bbbbbbbb1111222233334444555566667", "commit_sha": "c2"},
		}})
	}))
	defer srv.Close()

	c, err := client.New(client.Options{BaseURL: srv.URL, Key: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	got, err := resolveBuildID(ctx, c, "shop", "aaaaaaaa")
	if err != nil {
		t.Fatalf("resolveBuildID: %v", err)
	}
	if got != "aaaaaaaa1111222233334444555566667" {
		t.Errorf("resolved to %q, want the full id", got)
	}

	// A full id passes through unchanged, which is what a script would pass.
	if got, err := resolveBuildID(ctx, c, "shop", "bbbbbbbb1111222233334444555566667"); err != nil {
		t.Fatalf("resolveBuildID with a full id: %v", err)
	} else if got != "bbbbbbbb1111222233334444555566667" {
		t.Errorf("a full id resolved to %q", got)
	}
}

func TestResolveBuildIDRefusesWhatItCannotResolve(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "aaaaaaaa1111222233334444555566667"},
			{"id": "aaaaaaaabbbb2222333344445555666"},
		}})
	}))
	defer srv.Close()

	c, err := client.New(client.Options{BaseURL: srv.URL, Key: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	// Nothing matches.
	if _, err := resolveBuildID(ctx, c, "shop", "zzzz"); err == nil {
		t.Error("an id matching no build was accepted; it would be a 404 from the server instead")
	}

	// More than one matches. Guessing would cancel an arbitrary build.
	if _, err := resolveBuildID(ctx, c, "shop", "aaaaaaaa"); err == nil {
		t.Error("an ambiguous prefix was resolved; cancelling the wrong build is not recoverable by looking it up again")
	}
}
