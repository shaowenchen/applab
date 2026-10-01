// Package serverreg keeps the other AppLab deployments this one can manage.
//
// An entry is an address and an admin key: enough for this deployment to act as
// a client of that one, over the same HTTP API everything else uses. It is not a
// cluster and not a kubeconfig — this AppLab never dials the remote's Kubernetes
// API server, only its own API, which is why registering a server grants nothing
// beyond what that server's admin key already grants.
//
// # Stored in Secrets, not in the object store
//
// An app's key lives in the bucket beside the app (see internal/appkey). A
// server's key does not, and the difference is deliberate: a bucket credential is
// held by whoever runs the build pipeline, and a server key is an admin key to a
// whole other platform. Keeping it in a Secret means the blast radius of a leaked
// bucket credential stops at the apps in that bucket rather than extending to
// every platform this one can reach.
//
// # The label is not applab.io/app
//
// Every object AppLab creates for an app carries `applab.io/app`, and
// k8s.Client.DeleteEveryAppObject sweeps the namespace for exactly that label on
// uninstall. A registration that carried it would therefore be deleted when the
// release was uninstalled — every server forgotten at the moment someone
// reinstalls. So registrations carry `applab.io/server` and nothing else, and
// that is asserted by a test rather than left to the next person to notice.
//
// # Nothing is cached
//
// Every read goes to the API server. That is what lets two replicas agree on what
// is registered, and what makes a registration survive a restart without being
// replayed from a file. The cost is one call per read, which is bounded by the
// handful of servers an operator registers. Do not add a cache for speed: it
// would reintroduce per-replica divergence for a saving nobody is waiting on.
package serverreg

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// LocalID is the built-in entry that stands for this deployment itself.
//
// It is never stored: it is composed by the API layer on every read, because
// "this deployment" is not a registration — it is where this process runs, the
// same distinction a kubeconfig-less cluster once had. It exists so a caller
// choosing where to act has one list rather than one list plus a special case.
const LocalID = "local"

// LabelServer marks a Secret as a registered server.
//
// It is the only label on the object. See the package comment for why it must
// not be LabelApp.
const LabelServer = "applab.io/server"

// secretPrefix is the beginning of every registration's Secret name.
const secretPrefix = "applab-server-"

// Annotation keys, so a person reading the bucket of Secrets in a terminal can
// tell what they are without decoding a name.
const (
	AnnotationName      = "applab.io/server-name"
	AnnotationCreatedAt = "applab.io/created-at"
)

// ErrNotFound is returned by Get and Key when no such server is registered.
var ErrNotFound = errors.New("serverreg: not found")

// ErrReserved is returned when a caller tries to register or remove the built-in
// entry, which is not a registration.
var ErrReserved = errors.New("serverreg: the local entry is not a registration")

// Server is one registered remote.
//
// It has no key field, and that absence is the design: a value that cannot be
// expressed in this struct cannot be returned by a handler that responds with
// one, however the handler is later edited. The key is reachable only through
// Store.Key, which the proxy calls and no responder does.
type Server struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`

	// URL is the remote's base address, path prefix included, exactly as the
	// caller gave it. No scheme is assumed and none is stripped: this is the
	// string a client is built from, so anything done to it here is something a
	// person reading the registration cannot see.
	URL string `json:"url"`

	// Builtin is true only for the composed local entry. A stored registration
	// never sets it, which is why a value read back from a Secret is always
	// false here.
	Builtin bool `json:"builtin,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// Store reads and writes registrations in one namespace.
type Store struct {
	client    kubernetes.Interface
	namespace string
}

// New builds a store over a clientset.
//
// A nil clientset is allowed and reported by Ready as false; the API layer
// attaches nil when AppLab came up without cluster access, which is a real state
// (a console-only install) rather than an error.
func New(client kubernetes.Interface, namespace string) *Store {
	return &Store{client: client, namespace: namespace}
}

// Ready reports whether registrations can be read and written at all.
func (s *Store) Ready() bool { return s != nil && s.client != nil && s.namespace != "" }

// secretName is where one server's registration lives.
//
// The id is validated by the caller before it reaches here — model.ValidateAppID
// is stricter than a DNS label, so the concatenation is always a legal Secret
// name. Nothing in this package re-checks it, for the same reason the store does
// not re-check a branch name: the check belongs where the value is accepted from
// a caller, and a second one here would suggest this function is safe to call
// with anything.
func secretName(id string) string { return secretPrefix + id }

// List returns every registered server, ordered by id so two calls agree.
//
// The local entry is not included: it is not a registration. The API layer is
// what puts it in front of this list.
func (s *Store) List(ctx context.Context) ([]Server, error) {
	if !s.Ready() {
		return nil, fmt.Errorf("serverreg: no cluster to read registrations from")
	}

	objects, err := s.client.CoreV1().Secrets(s.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelServer,
	})
	if err != nil {
		return nil, fmt.Errorf("serverreg: list: %w", err)
	}

	out := make([]Server, 0, len(objects.Items))
	for i := range objects.Items {
		secret := &objects.Items[i]

		// A list is a weaker guarantee than a name lookup — a selector matches
		// anything wearing the label — so the name is checked too, the same
		// second read k8s.Client.checkAppLabels makes for the same reason. A
		// Secret that is not ours is skipped rather than reported: it is not an
		// error, it is something else in the namespace.
		if secret.Labels[LabelServer] == "" || !hasPrefix(secret.Name, secretPrefix) {
			continue
		}
		out = append(out, serverFrom(secret))
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Get returns one registration.
func (s *Store) Get(ctx context.Context, id string) (*Server, error) {
	if id == LocalID {
		return nil, ErrReserved
	}
	secret, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	server := serverFrom(secret)
	return &server, nil
}

// Key returns the admin key registered for a server.
//
// This is the only accessor for a key, and it exists for one caller: the proxy,
// which builds a client from it and discards it. No handler that writes a
// Server-shaped response calls this.
func (s *Store) Key(ctx context.Context, id string) (string, error) {
	if id == LocalID {
		return "", ErrReserved
	}
	secret, err := s.get(ctx, id)
	if err != nil {
		return "", err
	}
	key := string(secret.Data["key"])
	if key == "" {
		// A registration with no key cannot be dialled. Reporting it here rather
		// than letting the client fail with an empty Bearer token names the real
		// problem — the record is broken, not the remote.
		return "", fmt.Errorf("serverreg: the registration for %q has no key", id)
	}
	return key, nil
}

// Put writes a registration, creating or replacing it whole.
//
// The key is written into Data rather than StringData on purpose: Kubernetes
// folds StringData into Data on write, but the fake clientset the tests use does
// not, so a record written as StringData reads back empty under test while
// working in a cluster — a divergence that hides a bug until production. Writing
// Data is the one shape both do the same thing with.
func (s *Store) Put(ctx context.Context, server Server, key string) error {
	if !s.Ready() {
		return fmt.Errorf("serverreg: no cluster to write registrations to")
	}
	if server.ID == LocalID {
		return ErrReserved
	}
	if server.URL == "" || key == "" {
		return fmt.Errorf("serverreg: a registration needs both an address and a key")
	}

	created := server.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName(server.ID),
			Namespace: s.namespace,
			Labels:    map[string]string{LabelServer: server.ID},
			Annotations: map[string]string{
				AnnotationCreatedAt: created.Format(time.RFC3339),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"url": []byte(server.URL),
			"key": []byte(key),
		},
	}
	if server.Name != "" {
		secret.Annotations[AnnotationName] = server.Name
	}

	secrets := s.client.CoreV1().Secrets(s.namespace)
	existing, err := secrets.Get(ctx, secret.Name, metav1.GetOptions{})
	switch {
	case err == nil:
		// Replace the record whole, carrying the resourceVersion the API server
		// requires for an update. Replacing rather than patching is right here:
		// a registration is one address and one key, and a partial write would
		// leave a record that is neither the old one nor the new.
		secret.ResourceVersion = existing.ResourceVersion
		if _, err := secrets.Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("serverreg: update %s: %w", server.ID, err)
		}
		return nil
	case apierrors.IsNotFound(err):
		if _, err := secrets.Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("serverreg: create %s: %w", server.ID, err)
		}
		return nil
	default:
		return fmt.Errorf("serverreg: read %s: %w", server.ID, err)
	}
}

// Remove forgets a registration.
//
// It is idempotent — removing something already gone is not an error — and it
// deliberately does not reach the remote. Decommissioning a server must not
// require that server to still answer, or a registration could never be cleaned
// up after the thing it names had been turned off.
func (s *Store) Remove(ctx context.Context, id string) error {
	if !s.Ready() {
		return fmt.Errorf("serverreg: no cluster to remove registrations from")
	}
	if id == LocalID {
		return ErrReserved
	}
	err := s.client.CoreV1().Secrets(s.namespace).Delete(ctx, secretName(id), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("serverreg: delete %s: %w", id, err)
	}
	return nil
}

// get reads one registration's Secret, mapping a missing one onto ErrNotFound.
func (s *Store) get(ctx context.Context, id string) (*corev1.Secret, error) {
	if !s.Ready() {
		return nil, fmt.Errorf("serverreg: no cluster to read registrations from")
	}
	secret, err := s.client.CoreV1().Secrets(s.namespace).Get(ctx, secretName(id), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("serverreg: read %s: %w", id, err)
	}
	return secret, nil
}

// serverFrom builds the public record from a Secret.
//
// The key is not read. A function that returns everything it found is one whose
// caller eventually replies with it.
func serverFrom(secret *corev1.Secret) Server {
	server := Server{
		ID:        secret.Labels[LabelServer],
		Name:      secret.Annotations[AnnotationName],
		URL:       string(secret.Data["url"]),
		Builtin:   false,
		CreatedAt: parseTime(secret.Annotations[AnnotationCreatedAt]),
	}
	if server.Name == "" {
		server.Name = server.ID
	}
	return server
}

// parseTime reads the created-at annotation, treating an unreadable one as the
// zero time rather than failing the read. A timestamp is for display; a record
// whose date cannot be parsed is still a usable registration.
func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
