package serverreg

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testNS = "ops-system"

func testStore() (*Store, *fake.Clientset) {
	client := fake.NewSimpleClientset()
	return New(client, testNS), client
}

func TestPutThenGetRoundTrips(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore()

	if err := store.Put(ctx, Server{ID: "lab-2", Name: "The second lab", URL: "https://applab-2.example.com/applab"}, "sk-remote"); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := store.Get(ctx, "lab-2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != "lab-2" || got.Name != "The second lab" || got.URL != "https://applab-2.example.com/applab" {
		t.Fatalf("round trip lost a field: %+v", got)
	}
	if got.Builtin {
		t.Error("a stored registration reported itself as the built-in entry")
	}
	if got.CreatedAt.IsZero() {
		t.Error("no created-at was recorded")
	}
}

// A name is optional, and a registration with none reads back by its id rather
// than as a blank cell in a table.
func TestANameDefaultsToTheID(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore()

	if err := store.Put(ctx, Server{ID: "lab-2", URL: "https://applab-2.example.com"}, "sk-remote"); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.Get(ctx, "lab-2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "lab-2" {
		t.Fatalf("name = %q, want the id", got.Name)
	}
}

// The trap this pins: the real API server folds StringData into Data on write,
// the fake clientset does not. A record written as StringData would therefore
// read back empty under test and work in a cluster — the worst possible
// divergence. Writing Data is the one shape both treat identically.
func TestTheKeyIsStoredInDataNotStringData(t *testing.T) {
	ctx := context.Background()
	store, client := testStore()

	if err := store.Put(ctx, Server{ID: "lab-2", URL: "https://applab-2.example.com"}, "sk-remote"); err != nil {
		t.Fatalf("put: %v", err)
	}

	secret, err := client.CoreV1().Secrets(testNS).Get(ctx, secretName("lab-2"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the secret: %v", err)
	}
	if len(secret.Data["key"]) == 0 {
		t.Error("the key is not in Data")
	}
	if len(secret.StringData) != 0 {
		t.Error("the key was written as StringData, which the fake clientset drops — it would read back empty under test")
	}
	if len(secret.Data["url"]) == 0 {
		t.Error("the url is not in Data")
	}
}

// The registration must not wear the app label: DeleteEveryAppObject sweeps the
// namespace for it on uninstall, so a registration carrying it would be deleted
// when the release was removed — every server forgotten at the moment someone
// reinstalls.
func TestTheRegistrationDoesNotCarryTheAppLabel(t *testing.T) {
	ctx := context.Background()
	store, client := testStore()

	if err := store.Put(ctx, Server{ID: "lab-2", URL: "https://applab-2.example.com"}, "sk-remote"); err != nil {
		t.Fatalf("put: %v", err)
	}

	secret, err := client.CoreV1().Secrets(testNS).Get(ctx, secretName("lab-2"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the secret: %v", err)
	}
	if _, ok := secret.Labels["applab.io/app"]; ok {
		t.Fatal("the registration carries applab.io/app, so helm uninstall would delete every registered server")
	}
	if secret.Labels[LabelServer] != "lab-2" {
		t.Fatalf("labels = %v, want %s=lab-2", secret.Labels, LabelServer)
	}
}

func TestKeyReturnsWhatWasStored(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore()

	if err := store.Put(ctx, Server{ID: "lab-2", URL: "https://applab-2.example.com"}, "sk-remote"); err != nil {
		t.Fatalf("put: %v", err)
	}
	key, err := store.Key(ctx, "lab-2")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	if key != "sk-remote" {
		t.Fatalf("key = %q", key)
	}
}

func TestListOrdersByIDAndSkipsForeignSecrets(t *testing.T) {
	ctx := context.Background()
	store, client := testStore()

	// A registration.
	if err := store.Put(ctx, Server{ID: "lab-b", URL: "https://b.example.com"}, "k-b"); err != nil {
		t.Fatalf("put b: %v", err)
	}
	if err := store.Put(ctx, Server{ID: "lab-a", URL: "https://a.example.com"}, "k-a"); err != nil {
		t.Fatalf("put a: %v", err)
	}

	// Something else in the namespace that must not be read as a registration:
	// no label at all, and then the label on a name that is not ours.
	_, err := client.CoreV1().Secrets(testNS).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "applab-registry", Namespace: testNS},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed registry secret: %v", err)
	}
	_, err = client.CoreV1().Secrets(testNS).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "someone-elses-secret",
			Namespace: testNS,
			Labels:    map[string]string{LabelServer: "impostor"},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed impostor secret: %v", err)
	}

	servers, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("list returned %d servers, want 2: %+v", len(servers), servers)
	}
	if servers[0].ID != "lab-a" || servers[1].ID != "lab-b" {
		t.Fatalf("list is not ordered by id: %+v", servers)
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore()

	if err := store.Put(ctx, Server{ID: "lab-2", URL: "https://applab-2.example.com"}, "sk-remote"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.Remove(ctx, "lab-2"); err != nil {
		t.Fatalf("first remove: %v", err)
	}
	if err := store.Remove(ctx, "lab-2"); err != nil {
		t.Fatalf("removing something already gone must not be an error: %v", err)
	}
	if _, err := store.Get(ctx, "lab-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after remove, get = %v, want ErrNotFound", err)
	}
}

func TestGetAndKeyReportNotFound(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore()

	if _, err := store.Get(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get = %v, want ErrNotFound", err)
	}
	if _, err := store.Key(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("key = %v, want ErrNotFound", err)
	}
}

// The local entry is not a registration, so every write path refuses it rather
// than quietly storing a Secret named after this deployment.
func TestLocalIsNeverStored(t *testing.T) {
	ctx := context.Background()
	store, client := testStore()

	if err := store.Put(ctx, Server{ID: LocalID, URL: "https://self.example.com"}, "k"); !errors.Is(err, ErrReserved) {
		t.Fatalf("put local = %v, want ErrReserved", err)
	}
	if err := store.Remove(ctx, LocalID); !errors.Is(err, ErrReserved) {
		t.Fatalf("remove local = %v, want ErrReserved", err)
	}
	if _, err := store.Get(ctx, LocalID); !errors.Is(err, ErrReserved) {
		t.Fatalf("get local = %v, want ErrReserved", err)
	}

	secrets, err := client.CoreV1().Secrets(testNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	if len(secrets.Items) != 0 {
		t.Fatalf("a refused local write left %d secrets behind", len(secrets.Items))
	}
}

func TestPutRequiresAnAddressAndAKey(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore()

	if err := store.Put(ctx, Server{ID: "x", URL: ""}, "k"); err == nil {
		t.Error("a registration with no address was accepted")
	}
	if err := store.Put(ctx, Server{ID: "x", URL: "https://x.example.com"}, ""); err == nil {
		t.Error("a registration with no key was accepted")
	}
}

// A registration can be replaced — a rotated key, a moved address — and the
// replacement keeps the id and changes the rest.
func TestPutReplacesAnExistingRegistration(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore()

	if err := store.Put(ctx, Server{ID: "lab-2", Name: "Old", URL: "https://old.example.com"}, "old-key"); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := store.Put(ctx, Server{ID: "lab-2", Name: "New", URL: "https://new.example.com"}, "new-key"); err != nil {
		t.Fatalf("second put: %v", err)
	}

	got, err := store.Get(ctx, "lab-2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "New" || got.URL != "https://new.example.com" {
		t.Fatalf("the replacement did not take: %+v", got)
	}
	key, err := store.Key(ctx, "lab-2")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	if key != "new-key" {
		t.Fatalf("key = %q, want the rotated one", key)
	}

	servers, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("a replace left %d registrations, want 1", len(servers))
	}
}

// With no cluster attached — a console-only install — reads report that rather
// than panicking, and Ready says so.
func TestWithNoClientReadsReportRatherThanPanic(t *testing.T) {
	ctx := context.Background()
	store := New(nil, testNS)

	if store.Ready() {
		t.Error("a store with no client reported itself ready")
	}
	if _, err := store.List(ctx); err == nil {
		t.Error("list with no client returned no error")
	}
	if _, err := store.Get(ctx, "x"); err == nil {
		t.Error("get with no client returned no error")
	}
	if err := store.Put(ctx, Server{ID: "x", URL: "https://x"}, "k"); err == nil {
		t.Error("put with no client returned no error")
	}
	if err := store.Remove(ctx, "x"); err == nil {
		t.Error("remove with no client returned no error")
	}
}
