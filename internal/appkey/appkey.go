// Package appkey issues and resolves the per-app API keys.
//
// AppLab has two tiers of credential. An admin key is configured at boot (the
// APPLAB_KEYS environment variable) and may do anything. An app key belongs to
// one app, is created with it, and reaches only that app — see internal/api for
// where the line is drawn.
//
// The keys live in Kubernetes, one Secret per app, rather than in the database.
// That is a deliberate trade with two halves. It buys the properties a credential
// in the cluster should have: it is an ordinary object that kubectl can inspect,
// it is covered by the chart's existing Role (which already grants secrets, so
// no permission had to be widened), and deleting an app removes its key through
// the label-based teardown that already exists. It costs availability: if the API
// server cannot be reached, nothing can authenticate, the admin key included.
// Nothing is cached here on purpose — a cached credential is one that outlives
// its rotation, and rotation taking effect immediately is the whole point.
package appkey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/shaowenchen/applab/internal/k8s"
)

// ErrNoKey means the app has no key stored.
//
// It is a sentinel because the callers act on it differently: the endpoint that
// reads a key reports it as a missing resource, while the resolver treats it as
// simply "this presentation matched nothing".
var ErrNoKey = errors.New("no app key")

const (
	// labelApp identifies the app a key belongs to. It is the same label every
	// other object AppLab creates carries, which is what makes deleting an app
	// remove its key with no extra code: k8s.Client.DeleteAppObjects lists
	// Secrets by this selector.
	labelApp = k8s.LabelApp

	// labelDigest is the searchable fingerprint of the key.
	//
	// Resolve has to find the Secret for a presented key without reading every
	// Secret in the namespace, so the digest is stored as a label and looked up
	// directly. It is a *prefix* of the sha256 rather than the whole thing
	// because a Kubernetes label value is capped at 63 bytes and the hex digest
	// is 64 — the full value is rejected by the API server, which would surface
	// as a Secret that cannot be created at all.
	//
	// 32 hex characters is 128 bits. A prefix narrows the candidates rather than
	// granting access: the comparison after the lookup is over the whole key, so
	// a collision would only add a candidate to check.
	labelDigest = "applab.io/key-digest"

	// dataKey is the Secret's data field holding the key.
	dataKey = "key"

	// namePrefix makes a key Secret identifiable by name as well as by label,
	// which is what someone debugging with kubectl will reach for.
	namePrefix = "applab-key-"
)

// Store reads and writes app keys in one namespace.
type Store struct {
	client    kubernetes.Interface
	namespace string
}

// New creates a Store.
//
// A nil client is not an error here: a deployment without a cluster is a
// legitimate way to run AppLab, and the caller decides whether to attach a store
// at all. Methods on a Store built around a nil client are not called — the API
// layer reports the capability as unavailable instead.
func New(client kubernetes.Interface, namespace string) *Store {
	return &Store{client: client, namespace: namespace}
}

// Ready reports whether the store can reach a cluster.
func (s *Store) Ready() bool { return s != nil && s.client != nil }

// Name returns the Secret name for an app.
func Name(appID string) string { return namePrefix + appID }

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

	key, err := generate()
	if err != nil {
		return "", err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      Name(appID),
			Namespace: s.namespace,
			Labels: map[string]string{
				labelApp:    appID,
				labelDigest: digestLabel(key),
			},
		},
		Type: corev1.SecretTypeOpaque,
		// Data rather than StringData. The two are equivalent against a real API
		// server, which folds StringData into Data on the way in — but the fake
		// clientset used by the tests does not, so a store written against
		// StringData would be exercised through a shape the real cluster never
		// produces. Writing Data directly makes the object identical in both.
		Data: map[string][]byte{dataKey: []byte(key)},
	}

	if _, err := s.client.CoreV1().Secrets(s.namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("app %q already has a key: %w", appID, ErrExists)
		}
		return "", fmt.Errorf("store key for app %s: %w", appID, err)
	}
	return key, nil
}

// ErrExists means the app already has a key.
var ErrExists = errors.New("already exists")

// Get returns an app's key.
//
// The value is returned in full. The key is stored reversibly rather than as a
// hash precisely so it can be read back: a key nobody can recover is a key that
// has to be rotated the moment it is lost, and the deployment deliberately
// allows reading it instead.
func (s *Store) Get(ctx context.Context, appID string) (string, error) {
	secret, err := s.client.CoreV1().Secrets(s.namespace).Get(ctx, Name(appID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", fmt.Errorf("app %q: %w", appID, ErrNoKey)
	}
	if err != nil {
		return "", fmt.Errorf("read key for app %s: %w", appID, err)
	}

	key := secretValue(secret)
	if key == "" {
		// The Secret exists but does not hold a key. Treated as absent rather
		// than as an empty credential, which would authenticate nothing and be
		// confusing to debug.
		return "", fmt.Errorf("app %q: %w", appID, ErrNoKey)
	}
	return key, nil
}

// Rotate replaces an app's key, invalidating the previous one at once.
//
// There is no grace period and the old value is not retained. A rotation is
// normally performed because a key leaked, and a key that still works after
// being rotated away from has not been rotated.
//
// It also creates the key when there is none, so an app whose key was lost — or
// one created while AppLab had no cluster — can be brought back with the same
// operation rather than an error telling the caller to create it first.
func (s *Store) Rotate(ctx context.Context, appID string) (string, error) {
	if appID == "" {
		return "", fmt.Errorf("app id must not be empty")
	}

	key, err := generate()
	if err != nil {
		return "", err
	}

	secrets := s.client.CoreV1().Secrets(s.namespace)
	existing, err := secrets.Get(ctx, Name(appID), metav1.GetOptions{})

	if apierrors.IsNotFound(err) {
		return s.Create(ctx, appID)
	}
	if err != nil {
		return "", fmt.Errorf("read key for app %s: %w", appID, err)
	}

	// Updated in place rather than deleted and recreated: a delete-then-create
	// leaves a window in which the app has no key at all, and the window is
	// exactly as long as the gap between two API calls.
	existing.Labels = mergeLabels(existing.Labels, map[string]string{
		labelApp:    appID,
		labelDigest: digestLabel(key),
	})
	existing.Data = map[string][]byte{dataKey: []byte(key)}

	if _, err := secrets.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return "", fmt.Errorf("rotate key for app %s: %w", appID, err)
	}
	return key, nil
}

// Remove deletes an app's key.
//
// Deleting an app does not need this — k8s.Client.DeleteAppObjects already
// removes the Secret by label. It exists for the one case that teardown does not
// cover: deleting an app while keeping its source, where the record goes and the
// key should go with it rather than being left behind for an app that no longer
// exists.
func (s *Store) Remove(ctx context.Context, appID string) error {
	err := s.client.CoreV1().Secrets(s.namespace).Delete(ctx, Name(appID), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete key for app %s: %w", appID, err)
	}
	return nil
}

// ResolveAppKey maps a presented key back to the app that owns it.
//
// This runs on every authenticated request made with an app key, so it is one
// lookup rather than a scan: the digest label addresses the Secret directly. A
// key that matches no app reports ok=false, which the caller turns into the same
// 401 an unrecognised admin key gets — an attacker learns nothing about whether
// a key was merely wrong or belonged to an app that no longer exists.
func (s *Store) ResolveAppKey(ctx context.Context, presented string) (appID string, ok bool, err error) {
	presented = strings.TrimSpace(presented)
	if presented == "" {
		return "", false, nil
	}

	selector := labelDigest + "=" + digestLabel(presented)
	list, err := s.client.CoreV1().Secrets(s.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", false, fmt.Errorf("resolve app key: %w", err)
	}

	for i := range list.Items {
		secret := &list.Items[i]
		// The label only narrowed the candidates; the decision is made over the
		// whole key, in constant time. A digest-prefix collision therefore costs
		// a comparison, not access.
		if !sameKey(secretValue(secret), presented) {
			continue
		}
		app := secret.Labels[labelApp]
		if app == "" {
			// A labeled-by-digest Secret that does not name an app is not
			// something this package wrote, so it grants nothing.
			continue
		}
		return app, true, nil
	}
	return "", false, nil
}

// generate returns a new key: 32 bytes of cryptographic randomness, base64url
// encoded without padding.
//
// It is not derived from the app id, so holding one app's key says nothing about
// any other's and keys cannot be enumerated by guessing.
func generate() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate app key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// digestLabel is the label value used to find a key.
//
// Truncated to 32 hex characters because a Kubernetes label value may not exceed
// 63 bytes and the full digest is 64 — the full value makes the Secret
// uncreatable, which is a failure that only appears against a real API server.
func digestLabel(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:32]
}

// sameKey compares two keys in constant time.
//
// Constant time matters even here: the digest lookup already told an attacker
// that *some* key matched the prefix, and a byte-by-byte comparison would let
// them extend that to the whole key by timing.
func sameKey(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// secretValue reads the key out of a Secret.
//
// Only Data is consulted. StringData is write-only sugar — a real API server
// folds it into Data on the way in and never returns it — so reading it here
// would be reading a field that is always empty against a real cluster while
// appearing to work against the fake clientset, which does not fold. Writing
// Data directly (see Create) keeps the two identical.
func secretValue(secret *corev1.Secret) string {
	return string(secret.Data[dataKey])
}

// mergeLabels returns existing with overrides applied, without mutating either.
func mergeLabels(existing, overrides map[string]string) map[string]string {
	merged := make(map[string]string, len(existing)+len(overrides))
	for k, v := range existing {
		merged[k] = v
	}
	for k, v := range overrides {
		merged[k] = v
	}
	return merged
}
