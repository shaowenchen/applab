// Package appkey issues and resolves the per-app API keys.
//
// AppLab has two tiers of credential. An admin key is configured at boot (the
// APPLAB_KEYS environment variable) and may do anything. An app key belongs to
// one app, is created with it, and reaches only that app — see internal/api for
// where the line is drawn.
//
// # Where a key lives
//
// In the object store, beside the app it belongs to, rather than in a Kubernetes
// Secret. It was a Secret for two reasons, and neither survives the move to a
// bucket-backed AppLab.
//
// The first was that a credential belongs in the cluster's own credential store.
// That is true of a credential the cluster uses. This one AppLab issues and
// validates itself, using it only to decide what a caller may do, and the
// cluster never reads it — so a Secret was a place to keep it rather than a
// place that had a use for it. Removing it also removes the last reason AppLab
// held write permission on `secrets` at all.
//
// The second was that deleting an app removed its key for free, through the
// label-based teardown that already deleted the app's other objects. That still
// holds: the key is an object under the app's own directory, so removing the
// directory removes it.
//
// # What it costs
//
// The key is stored in the same bucket as the source, so the bucket's credential
// is now the thing that reaches every app's key. That is worth saying plainly
// because it is a real widening: whoever holds the bucket's access key can read
// every app's API key, where before they could read the source and the history
// but the keys were in a different system with a different credential.
//
// What is kept from the Secret version is that the key is stored *reversibly*.
// Hashing it would be the safer design in isolation — a leaked bucket would then
// yield nothing usable — but AppLab deliberately allows reading a key back, and
// a key nobody can recover is one that has to be rotated the moment it is lost.
// The API's own documentation promises the value can be read; this keeps that
// true.
package appkey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/shaowenchen/applab/internal/objectstore"
	"github.com/shaowenchen/applab/internal/store"
)

// Store reads and writes the per-app keys in the object store.
type Store struct {
	apps *store.Store
}

// New returns a key store over AppLab's own state.
//
// There is no Ready check and no nil-ness any more: a deployment always has an
// object store, because it has nowhere else to keep anything. That removes a
// whole class of "this deployment cannot do that" answers — an installation with
// no cluster now issues keys exactly like one with.
func New(apps *store.Store) *Store {
	return &Store{apps: apps}
}

// ErrNoKey means the app has no key stored.
//
// It is a sentinel because the callers act on it differently: the endpoint that
// reads a key reports it as a missing resource, while the resolver treats it as
// simply "this presentation matched nothing".
var ErrNoKey = errors.New("no app key")

// ErrExists means the app already has a key.
var ErrExists = errors.New("already exists")

// keyLength is the number of random bytes behind a key. 32 bytes is what the
// admin key uses and what the chart generates, so the two tiers are equally hard
// to guess.
const keyLength = 32

// objectKey is where one app's key lives.
//
// It is a file beside app.json rather than a field inside it. The record is
// rewritten whole by several callers — a deploy sets the deployed commit, a
// build sets the status — and a credential in it would be rewritten by each of
// them, so a read-modify-write that raced another could blank it and lock the
// app's owner out. A separate object has one writer.
func objectKey(appID string) string {
	return objectstore.Key("apps", appID, "key.json")
}

// keyRecord is what the object holds.
//
// The value is here in the clear, which the package comment explains. The digest
// is here so that ResolveAppKey can find the app without reading every key: a
// presented key is hashed and compared against the digests of every app's
// record, which is a field read rather than a credential compared.
type keyRecord struct {
	Key    string `json:"key"`
	Digest string `json:"digest"`
}

// Create mints a key for an app and stores it.
//
// It fails if the app already has one rather than silently replacing it: a
// create that overwrote an existing key would lock out whoever was already using
// it, and the caller's intent — "this app needs a key" — is not served by
// rotating it. Rotation is a separate, explicit operation.
func (s *Store) Create(ctx context.Context, appID string) (string, error) {
	if appID == "" {
		return "", fmt.Errorf("app id must not be empty")
	}

	// Refused before the app is read, so a create for an app that does not exist
	// fails for that reason rather than writing a key nothing belongs to.
	exists, err := s.has(ctx, appID)
	if err != nil {
		return "", err
	}
	if exists {
		return "", fmt.Errorf("app %q already has a key: %w", appID, ErrExists)
	}

	key, err := generate()
	if err != nil {
		return "", err
	}
	if err := s.put(ctx, appID, key); err != nil {
		return "", err
	}
	return key, nil
}

// Get returns an app's key.
//
// The value is returned in full. The key is stored reversibly rather than as a
// hash precisely so it can be read back: a key nobody can recover is a key that
// has to be rotated the moment it is lost, and the deployment deliberately
// allows reading it instead.
func (s *Store) Get(ctx context.Context, appID string) (string, error) {
	record, err := s.record(ctx, appID)
	if err != nil {
		return "", err
	}
	if record.Key == "" {
		// The object exists but does not hold a key. Treated as absent rather
		// than as an empty credential, which would authenticate nothing and be
		// confusing to debug.
		return "", fmt.Errorf("app %q: %w", appID, ErrNoKey)
	}
	return record.Key, nil
}

// Rotate replaces an app's key, invalidating the previous one at once.
//
// There is no grace period and the old value is not retained. A rotation is
// normally performed because a key leaked, and a key that still works after
// being rotated away from has not been rotated.
//
// It also creates the key when there is none, so an app whose key was lost can
// be brought back with the same operation rather than an error telling the
// caller to create it first.
func (s *Store) Rotate(ctx context.Context, appID string) (string, error) {
	key, err := generate()
	if err != nil {
		return "", err
	}
	if err := s.put(ctx, appID, key); err != nil {
		return "", err
	}
	return key, nil
}

// Remove deletes an app's key.
//
// Removing a key that is not there is not an error: the callers that remove one
// are settling a state — "this app has no key" — and that state is reached
// either way.
func (s *Store) Remove(ctx context.Context, appID string) error {
	if err := s.apps.Objects().Delete(ctx, objectKey(appID)); err != nil {
		return fmt.Errorf("remove key for app %s: %w", appID, err)
	}
	return nil
}

// ResolveAppKey finds the app a presented key belongs to.
//
// It lists every app's key record and compares digests, rather than reading each
// key and comparing values. Both are constant-time per comparison; the digest is
// what makes the comparison possible without the key itself being in memory for
// every request.
//
// The listing is the cost, and it grows with the number of apps: one key per app
// is one small object per app. That is the price of a per-app credential with no
// index, and it is paid on every authenticated request from an app key. An
// installation with hundreds of apps would want the digest indexed somewhere —
// which is a change of layout, not of interface.
func (s *Store) ResolveAppKey(ctx context.Context, presented string) (appID string, ok bool, err error) {
	// Trimmed before it is hashed, because the value reaching here has usually
	// been through a shell: `APPLAB_KEY=$(cat keyfile)` keeps the file's trailing
	// newline, and the client turns that into an Authorization header without
	// removing it. A key that fails to resolve because of a byte the caller
	// cannot see is a failure with no useful diagnostic.
	presented = strings.TrimSpace(presented)
	if presented == "" {
		return "", false, nil
	}
	digest := digestOf(presented)

	records, err := s.apps.ListKeyRecords(ctx)
	if err != nil {
		return "", false, err
	}

	for _, record := range records {
		if record.Digest == "" {
			continue
		}
		// Constant-time, and the candidate is the stored digest rather than the
		// presented string: a comparison that returns early leaks how much of a
		// key was guessed correctly.
		if sameDigest(record.Digest, digest) {
			return record.AppID, true, nil
		}
	}
	return "", false, nil
}

// has reports whether an app already has a key object.
func (s *Store) has(ctx context.Context, appID string) (bool, error) {
	exists, err := s.apps.Objects().Exists(ctx, objectKey(appID))
	if err != nil {
		return false, fmt.Errorf("look for a key for app %s: %w", appID, err)
	}
	return exists, nil
}

// record reads an app's key object.
func (s *Store) record(ctx context.Context, appID string) (keyRecord, error) {
	body, err := s.apps.Objects().GetBytes(ctx, objectKey(appID))
	if err != nil {
		if errors.Is(err, objectstore.ErrNotExist) {
			return keyRecord{}, fmt.Errorf("app %q: %w", appID, ErrNoKey)
		}
		return keyRecord{}, fmt.Errorf("read key for app %s: %w", appID, err)
	}

	var record keyRecord
	if err := decode(body, &record); err != nil {
		return keyRecord{}, fmt.Errorf("read key for app %s: %w", appID, err)
	}
	return record, nil
}

// put writes an app's key object, replacing anything at that key.
func (s *Store) put(ctx context.Context, appID, key string) error {
	body, err := encode(keyRecord{Key: key, Digest: digestOf(key)})
	if err != nil {
		return fmt.Errorf("encode the key for app %s: %w", appID, err)
	}
	if err := s.apps.Objects().PutBytes(ctx, objectKey(appID), body); err != nil {
		return fmt.Errorf("store key for app %s: %w", appID, err)
	}
	return nil
}

// generate returns a new key.
//
// base64url rather than hex: the same entropy is 43 characters instead of 64,
// and every character is safe to paste into a URL, a header or a git remote
// without escaping — which is exactly where a key ends up.
func generate() (string, error) {
	buf := make([]byte, keyLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// digestOf returns the digest a presented key is matched by.
func digestOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// sameDigest compares two digests without leaking where they differ.
func sameDigest(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// encode and decode are the object's JSON form, in one place so the two cannot
// drift.
func encode(record keyRecord) ([]byte, error) {
	return json.MarshalIndent(record, "", "  ")
}

func decode(body []byte, into *keyRecord) error {
	return json.Unmarshal(body, into)
}
