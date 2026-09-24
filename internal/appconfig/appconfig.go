// Package appconfig stores an app's secret configuration.
//
// An app has two kinds of configuration, and the split is the whole design.
// Environment variables are plain — LOG_LEVEL, FEATURE_X — and live in the
// database beside the app, because they are not sensitive and the API returns
// them. Secrets are not: they are passwords, tokens and connection strings, and
// they must never appear in a Pod spec, in kubectl describe, or in
// `kubectl get deploy -o yaml`.
//
// So secrets live here, in one Kubernetes Secret per app, and are never mirrored
// into the database. That is a deliberate trade with two halves. It buys the
// properties a credential in the cluster should have: an ordinary object kubectl
// can inspect, coverage by the chart's existing Role (which already grants
// secrets, so no permission had to be widened), and removal with the app through
// the label-based teardown that already exists. It costs availability, in the
// same way internal/appkey does — a deployment with no cluster can hold no
// secrets, and then the affected endpoints report the capability as unavailable.
//
// Nothing is cached, and the read-merge-write in Set is not transactional.
package appconfig

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	"github.com/shaowenchen/applab/internal/k8s"
	"github.com/shaowenchen/applab/internal/model"
)

const (
	// labelApp identifies the app a Secret belongs to. It is the same label
	// every other object applab creates carries, which is what makes deleting
	// an app remove its configuration with no extra code: k8s.Client.DeleteAppObjects
	// lists Secrets by this selector.
	labelApp = k8s.LabelApp

	// namePrefix makes a configuration Secret identifiable by name as well as by
	// label, which is what someone debugging with kubectl will reach for.
	namePrefix = "applab-env-"
)

// Reserved is the one name that may not be set.
//
// The deployer sets it itself from the app's port setting, because it is how the
// Service finds the container. Letting a caller also set it would put two
// declarations of the same variable in the pod spec — Kubernetes allows it and
// the later one wins, so the app would run on a port nothing routes to, with no
// error anywhere to explain it. Refusing it is the only outcome that is not a
// silent failure.
const Reserved = model.PortEnv

// Store reads and writes app configuration Secrets in one namespace.
type Store struct {
	client    kubernetes.Interface
	namespace string
}

// New creates a Store.
//
// A nil client is not an error here: a deployment without a cluster is a
// legitimate way to run applab, and the caller decides whether to attach a store
// at all. Methods on a Store built around a nil client are not called — the API
// layer reports the capability as unavailable instead.
func New(client kubernetes.Interface, namespace string) *Store {
	return &Store{client: client, namespace: namespace}
}

// Ready reports whether the store can reach a cluster.
func (s *Store) Ready() bool { return s != nil && s.client != nil }

// Name returns the Secret name for an app.
func Name(appID string) string { return namePrefix + appID }

// ValidateName reports whether name may be used as a configuration key.
//
// Both kinds of value share one namespace in the container — plain variables
// through the env list, secrets through envFrom — so one rule covers both.
func ValidateName(name string) error {
	if name == Reserved {
		return fmt.Errorf("%s is set by applab from the app's port setting and cannot be configured here", Reserved)
	}
	if errs := validation.IsEnvVarName(name); len(errs) > 0 {
		return fmt.Errorf("%q is not a valid environment variable name: %s", name, errs[0])
	}
	return nil
}

// Set writes the given values, preserving any key not mentioned, and returns the
// app's full set of names.
//
// It merges rather than replaces so that setting one variable does not silently
// drop the other five. The cost is that the read and the write are separate API
// calls: two callers setting different keys at the same moment can lose one of
// them. Retrying on a resource-version conflict is the honest fix and is not
// worth its complexity until a caller actually does this concurrently.
func (s *Store) Set(ctx context.Context, appID string, values map[string]string) ([]string, error) {
	if appID == "" {
		return nil, fmt.Errorf("app id must not be empty")
	}
	if len(values) == 0 {
		return s.Names(ctx, appID)
	}
	for name := range values {
		if err := ValidateName(name); err != nil {
			return nil, err
		}
	}

	secrets := s.client.CoreV1().Secrets(s.namespace)
	existing, err := secrets.Get(ctx, Name(appID), metav1.GetOptions{})

	if apierrors.IsNotFound(err) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      Name(appID),
				Namespace: s.namespace,
				Labels:    map[string]string{labelApp: appID},
			},
			Type: corev1.SecretTypeOpaque,
			// Data rather than StringData. The two are equivalent against a real
			// API server, which folds StringData into Data on the way in — but
			// the fake clientset used by the tests does not, so a store written
			// against StringData would be exercised through a shape the real
			// cluster never produces. Writing Data directly makes the object
			// identical in both.
			Data: encode(values),
		}
		if _, err := secrets.Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("create configuration for app %s: %w", appID, err)
		}
		return sortedNames(values), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read configuration for app %s: %w", appID, err)
	}

	if existing.Data == nil {
		existing.Data = map[string][]byte{}
	}
	for name, value := range values {
		existing.Data[name] = []byte(value)
	}
	// Re-applied on every write: an app created before this label was set, or one
	// whose Secret was edited by hand, would otherwise be invisible to the
	// teardown selector and survive the app it belongs to.
	existing.Labels = mergeLabels(existing.Labels, map[string]string{labelApp: appID})

	if _, err := secrets.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return nil, fmt.Errorf("update configuration for app %s: %w", appID, err)
	}
	return sortedNames(existing.Data), nil
}

// Remove deletes the named values and returns the names that remain.
//
// The Secret is deleted once its last value goes, so that the object existing
// means the app has secrets. That is the invariant the deployer relies on when
// it decides whether to reference the Secret at all, and keeping it here rather
// than leaving empty Secrets around is what makes it hold.
func (s *Store) Remove(ctx context.Context, appID string, names []string) ([]string, error) {
	if appID == "" {
		return nil, fmt.Errorf("app id must not be empty")
	}

	secrets := s.client.CoreV1().Secrets(s.namespace)
	existing, err := secrets.Get(ctx, Name(appID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read configuration for app %s: %w", appID, err)
	}

	for _, name := range names {
		delete(existing.Data, name)
	}

	if len(existing.Data) == 0 {
		return nil, s.Delete(ctx, appID)
	}

	existing.Labels = mergeLabels(existing.Labels, map[string]string{labelApp: appID})
	if _, err := secrets.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return nil, fmt.Errorf("update configuration for app %s: %w", appID, err)
	}
	return sortedNames(existing.Data), nil
}

// Names lists the keys an app has, never their values.
//
// This is what the API and the console can show. There is deliberately no
// corresponding method that returns values for a request to call: Contents
// exists for the deployer, and keeping it out of the request path is what makes
// "no route returns a secret" structural rather than a matter of remembering.
func (s *Store) Names(ctx context.Context, appID string) ([]string, error) {
	contents, err := s.Contents(ctx, appID)
	if err != nil {
		return nil, err
	}
	return sortedNames(contents), nil
}

// Contents returns an app's secret values.
//
// It is the one method here that returns a value, and it is called from exactly
// one place: the deployer, which needs them to hash the configuration into the
// pod template. Nothing in the API layer calls it.
//
// No secret is not an error, and reports as an empty map — an app configured
// with no secrets is the ordinary case, not a failure.
func (s *Store) Contents(ctx context.Context, appID string) (map[string]string, error) {
	if appID == "" {
		return nil, fmt.Errorf("app id must not be empty")
	}

	secret, err := s.client.CoreV1().Secrets(s.namespace).Get(ctx, Name(appID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read configuration for app %s: %w", appID, err)
	}

	out := make(map[string]string, len(secret.Data))
	for name, value := range secret.Data {
		out[name] = string(value)
	}
	return out, nil
}

// Delete removes an app's configuration Secret.
//
// Deleting an app does not need this — k8s.Client.DeleteAppObjects already
// removes the Secret by label. It exists for the one case that teardown does not
// cover: deleting an app while keeping its source, where the record goes and the
// secrets should go with it rather than being left behind for an app that no
// longer exists.
func (s *Store) Delete(ctx context.Context, appID string) error {
	err := s.client.CoreV1().Secrets(s.namespace).Delete(ctx, Name(appID), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete configuration for app %s: %w", appID, err)
	}
	return nil
}

// Has reports whether an app has any secrets stored.
//
// The deployer asks this to decide whether to reference the Secret at all: an
// envFrom naming a Secret that was never created makes the pod unschedulable,
// which would turn "no secrets configured" into an app that will not start.
func (s *Store) Has(ctx context.Context, appID string) (bool, error) {
	names, err := s.Names(ctx, appID)
	if err != nil {
		return false, err
	}
	return len(names) > 0, nil
}

// encode turns values into the Secret's data map.
func encode(values map[string]string) map[string][]byte {
	out := make(map[string][]byte, len(values))
	for name, value := range values {
		out[name] = []byte(value)
	}
	return out
}

// sortedNames returns the keys of a map of values, in a stable order.
//
// It is generic over the value type because the same names are held two ways
// here — as bytes in the Secret's data and as strings when they have just been
// written — and neither ordering should differ from the other.
//
// Sorted rather than in map order because these names are displayed — in the
// console and by the CLI — and an unordered map would shuffle them on every
// read, which reads as a change that did not happen.
func sortedNames[V any](data map[string]V) []string {
	if len(data) == 0 {
		return nil
	}
	names := make([]string, 0, len(data))
	for name := range data {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
