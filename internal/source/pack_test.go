package source

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaowenchen/applab/internal/objectstore"
)

// What a repository costs to move is the thing these pin. A clone reads every
// object and a push writes what changed, both against a bucket where each
// request is a round trip — so the number of objects, and whether they are loose
// or packed, is the difference between a clone taking seconds and taking
// minutes.

// TestRepoIsPackedOnceItHasEnoughLooseObjects asserts objects do not accumulate
// loose without bound.
//
// A loose object is one file, and one file is one request to download. Without
// packing, a repository grows one request per object written, forever — a
// hundred commits is thousands — and every clone pays all of them.
func TestRepoIsPackedOnceItHasEnoughLooseObjects(t *testing.T) {
	s, objs := newTestStoreAndObjects(t)
	ctx := context.Background()

	if err := s.Create(ctx, "shop", main); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Enough commits to cross the threshold.
	for i := 0; i < 40; i++ {
		entries := make([]tarEntry, 0, 12)
		for j := 0; j < 12; j++ {
			entries = append(entries, tarEntry{
				name: fmt.Sprintf("src/f%02d.txt", j),
				body: fmt.Sprintf("c%d-%d", i, j) + strings.Repeat("x", 900),
			})
		}
		if _, err := s.Ingest(ctx, "shop", main, bytes.NewReader(buildTar(t, entries)), fmt.Sprintf("c%d", i), "", DefaultIngestLimits); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}

	loose, packed := countObjects(t, objs)
	if packed == 0 {
		t.Fatalf("no pack was written after enough objects to cross the threshold; every clone would read %d loose objects", loose)
	}
	// The bound that matters: objects on disk must be a small number, not one
	// per object written. 480 files went in and were collapsed.
	if total := loose + packed; total > 100 {
		t.Errorf("the repository holds %d objects (%d loose, %d packed); packing did not collapse them", total, loose, packed)
	}
}

// TestAPushBelowTheThresholdStaysIncremental asserts packing does not make every
// push upload the whole repository.
//
// Packing rewrites the pack, so repacking on every operation would send all of
// its bytes every time. The threshold is what prevents that: below it, a push
// uploads only what the push actually added, which is what keeps a push cheap
// while a clone is fast.
//
// The repository here is deliberately kept well under the threshold, because
// "does not repack" is only a property while there is no crossing to trigger
// one — a test that ran past the threshold and asserted the same thing would be
// asserting the opposite of what it means.
func TestAPushBelowTheThresholdStaysIncremental(t *testing.T) {
	s, objs := newTestStoreAndObjects(t)
	ctx := context.Background()

	if err := s.Create(ctx, "shop", main); err != nil {
		t.Fatalf("create: %v", err)
	}

	// A few commits: enough to have a repository, far short of the threshold.
	for i := 0; i < 5; i++ {
		entries := make([]tarEntry, 0, 4)
		for j := 0; j < 4; j++ {
			entries = append(entries, tarEntry{
				name: fmt.Sprintf("src/f%02d.txt", j),
				body: fmt.Sprintf("c%d-%d", i, j) + strings.Repeat("x", 200),
			})
		}
		if _, err := s.Ingest(ctx, "shop", main, bytes.NewReader(buildTar(t, entries)), fmt.Sprintf("c%d", i), "", DefaultIngestLimits); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}

	// The precondition the assertion below rests on, checked rather than assumed.
	repoPath := materializeRepo(t, s, "shop")
	loose, err := countLooseObjects(repoPath)
	if err != nil {
		t.Fatalf("count loose objects: %v", err)
	}
	if loose >= repackThreshold {
		t.Fatalf("this test set up %d loose objects, at or past the threshold of %d; it needs a repository that is below it",
			loose, repackThreshold)
	}

	counted := &countingStore{Store: objs}
	s.objects = counted

	// One more commit: a handful of new objects.
	if _, err := s.Ingest(ctx, "shop", main, bytes.NewReader(buildTar(t, []tarEntry{
		{name: "src/new.txt", body: "one more\n"},
	})), "one more", "", DefaultIngestLimits); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// A commit is its blobs, its trees and itself — a handful of small objects.
	// A repacking push would additionally rewrite the pack.
	if counted.puts == 0 {
		t.Error("a push wrote nothing, so the commit was not stored")
	}
	if counted.puts > 20 {
		t.Errorf("a push below the threshold wrote %d objects; it should upload only what the push added", counted.puts)
	}
	if counted.repacked {
		t.Error("a push below the threshold repacked the repository, so it uploaded the whole pack")
	}
}

// TestCountLooseObjectsIgnoresPacks asserts the count that decides repacking
// counts the right things.
//
// It gates whether git runs at all, so a count that included the pack would make
// every operation repack after the first, and one that missed the two-character
// depth would never repack anything.
func TestCountLooseObjectsIgnoresPacks(t *testing.T) {
	dir := t.TempDir()

	// A bare repository's objects directory: two-hex-digit subdirectories hold
	// objects, and pack/ and info/ do not.
	for _, rel := range []string{
		"objects/ab/1111111111111111111111111111111111111111",
		"objects/ab/2222222222222222222222222222222222222222",
		"objects/cd/3333333333333333333333333333333333333333",
		"objects/pack/pack-abc.pack",
		"objects/pack/pack-abc.idx",
		"objects/info/packs",
		"objects/notadigit/4444",
	} {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("create %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}

	got, err := countLooseObjects(dir)
	if err != nil {
		t.Fatalf("countLooseObjects: %v", err)
	}
	if got != 3 {
		t.Errorf("counted %d loose objects, want 3 — the packs and the info directory are not loose objects", got)
	}
}

// TestCountLooseObjectsOnARepositoryWithNoObjects asserts an empty repository is
// zero rather than an error.
//
// It is the state of an app that has just been created, which is the first thing
// a push to a new app does.
func TestCountLooseObjectsOnARepositoryWithNoObjects(t *testing.T) {
	got, err := countLooseObjects(t.TempDir())
	if err != nil {
		t.Fatalf("countLooseObjects on a directory with no objects: %v", err)
	}
	if got != 0 {
		t.Errorf("counted %d, want 0", got)
	}
}

// TestAHookIsNotDeletedOnEveryOperation asserts the executable-bit marker is not
// mistaken for a deletion.
//
// A hook is stored under "<name>.exec" and materialised as "<name>". The
// download used to record what it wrote under the *key*, so the very next upload
// walk saw a file that was not in that record and treated it as deleted — one
// delete and one re-push per hook, on every single operation including a plain
// clone.
func TestAHookIsNotDeletedOnEveryOperation(t *testing.T) {
	s, objs := newTestStoreAndObjects(t)
	ctx := context.Background()

	if err := s.Create(ctx, "shop", main); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Ingest(ctx, "shop", main, bytes.NewReader(buildTar(t, []tarEntry{
		{name: "a.txt", body: "one\n"},
	})), "initial", "", DefaultIngestLimits); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// Read the repository the way a clone does, and finish it — the session holds
	// a per-branch lock until it is finished, so an Open that is not finished
	// blocks every later operation on that branch.
	if _, finish, err := s.Open(ctx, "shop", main); err != nil {
		t.Fatalf("open: %v", err)
	} else if err := finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}

	// A second read that changes nothing at all. Nothing git wrote means nothing
	// to upload, so the only writes this can produce are the marker's own.
	counted := &countingStore{Store: objs}
	s.objects = counted
	if _, finish, err := s.Open(ctx, "shop", main); err != nil {
		t.Fatalf("open: %v", err)
	} else if err := finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}

	if counted.puts != 0 || counted.deletes != 0 {
		t.Errorf("a read that changed nothing wrote %d and deleted %d objects; the executable-bit marker is being mistaken for a deletion",
			counted.puts, counted.deletes)
	}
}

// countingStore counts the writes and deletes made through it.
type countingStore struct {
	objectstore.Store
	puts, deletes int
	repacked      bool
}

func (c *countingStore) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	c.puts++
	if strings.Contains(key, "/objects/pack/") {
		c.repacked = true
	}
	return c.Store.Put(ctx, key, r, size)
}

func (c *countingStore) PutBytes(ctx context.Context, key string, body []byte) error {
	c.puts++
	if strings.Contains(key, "/objects/pack/") {
		c.repacked = true
	}
	return c.Store.PutBytes(ctx, key, body)
}

func (c *countingStore) Delete(ctx context.Context, key string) error {
	c.deletes++
	return c.Store.Delete(ctx, key)
}

// countObjects reports how many loose and packed objects the bucket holds.
func countObjects(t *testing.T, objs objectstore.Store) (loose, packed int) {
	t.Helper()

	all, err := objs.List(context.Background(), "apps/shop/repo/branches/"+main)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, object := range all {
		switch {
		case strings.Contains(object.Key, "/objects/pack/") && strings.HasSuffix(object.Key, ".pack"):
			packed++
		case strings.Contains(object.Key, "/objects/"):
			loose++
		}
	}
	return loose, packed
}
