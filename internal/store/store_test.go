package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/objectstore"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := OpenLocal(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}

// TestAppsLiveInOneDirectoryEach is the layout check, and it is the one that
// makes listing cheap: an app's record sits at a fixed depth and everything else
// about the app sits below it.
func TestAppsLiveInOneDirectoryEach(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.CreateApp(ctx, &model.App{ID: "shop", Name: "Shop"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	objects, err := st.Objects().List(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	var keys []string
	for _, object := range objects {
		keys = append(keys, object.Key)
	}
	if strings.Join(keys, ",") != "apps/shop/app.json" {
		t.Errorf("the bucket holds %v, want just the app's record", keys)
	}
}

// TestOneAppIsOneDirectory is the layout, asserted against the bucket.
//
// Every path here is a promise to whoever opens the bucket with a browser or a
// CLI: an app is one directory, what is inside it is told apart by the name of
// the directory it is in, and nothing an app owns lives outside its own prefix.
//
// It is written by *doing* the operations — create an app, record a commit, start
// a build — and then reading back what the bucket actually received, rather than
// by calling the path helpers and comparing them to themselves. What it is
// pinning is where a person will find things, so the assertion has to be over
// objects that were really written.
func TestOneAppIsOneDirectory(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.CreateApp(ctx, &model.App{ID: "shop", Name: "Shop"}); err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := st.RecordCommit(ctx, &model.Commit{AppID: "shop", SHA: strings.Repeat("a", 40)}); err != nil {
		t.Fatalf("record commit: %v", err)
	}
	if err := st.CreateBuild(ctx, &model.Build{AppID: "shop", ID: "b1"}); err != nil {
		t.Fatalf("create build: %v", err)
	}
	// A repository is a directory of git's own objects, written by the source
	// package rather than the store, so one representative key stands in for it.
	if err := st.Objects().PutBytes(ctx, SourcePrefix("shop")+"/HEAD", []byte("ref: refs/heads/main\n")); err != nil {
		t.Fatalf("write repository: %v", err)
	}

	objects, err := st.Objects().List(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var keys []string
	for _, object := range objects {
		keys = append(keys, object.Key)
	}

	want := []string{
		"apps/shop/app.json",
		"apps/shop/builds/b1.json",
		"apps/shop/commits/" + strings.Repeat("a", 40) + ".json",
		"apps/shop/repo/HEAD",
	}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("the bucket holds:\n  %s\nwant:\n  %s", strings.Join(keys, "\n  "), strings.Join(want, "\n  "))
	}

	for _, key := range keys {
		if !strings.HasPrefix(key, "apps/shop/") {
			t.Errorf("%s is not under the app's own directory, so deleting the app would leave it behind", key)
		}
	}
}

// TestListAppsIgnoresEverythingBelowAnApp is what makes one listing enough.
//
// The prefix "apps/" also covers every app's commits, builds and repository —
// hundreds of objects per app. If the listing treated those as apps, every list
// would grow with the platform's whole history.
func TestListAppsIgnoresEverythingBelowAnApp(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.CreateApp(ctx, &model.App{ID: "shop", Name: "Shop"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Objects that live under an app but are not the app.
	for _, key := range []string{
		"apps/shop/key.json",
		"apps/shop/commits/abc.json",
		"apps/shop/builds/def.json",
		"apps/shop/repo/HEAD",
		"apps/shop/repo/objects/ab/cdef",
	} {
		if err := st.Objects().PutBytes(ctx, key, []byte("{}")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}

	apps, err := st.ListApps(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(apps) != 1 || apps[0].ID != "shop" {
		t.Errorf("list returned %d apps, want just shop", len(apps))
	}
}

// TestCreateRefusesAnIDThatIsTaken is the check that stops a create from taking
// an existing app's source and history with it.
func TestCreateRefusesAnIDThatIsTaken(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.CreateApp(ctx, &model.App{ID: "shop", Name: "Shop", Port: 8080}); err != nil {
		t.Fatalf("create: %v", err)
	}

	err := st.CreateApp(ctx, &model.App{ID: "shop", Name: "Replacement", Port: 9090})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("second create returned %v, want ErrExists", err)
	}

	// And the first app is untouched.
	got, err := st.GetApp(ctx, "shop")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Shop" || got.Port != 8080 {
		t.Errorf("the existing app was modified: %+v", got)
	}
}

// TestUpdateKeepsFieldsTheCallerDidNotKnowAbout is the read-modify-write check.
//
// The caller here has loaded an app, changed one field, and saved. If the write
// were the caller's struct straight out, every field it did not happen to carry
// would be blanked — and a caller in this package that loaded a partial view
// would erase the rest with no error anywhere.
func TestUpdateKeepsFieldsTheCallerDidNotKnowAbout(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.CreateApp(ctx, &model.App{
		ID: "shop", Name: "Shop", Port: 8080, Dockerfile: "Dockerfile",
		Env: map[string]string{"A": "1"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// A caller that loaded the app, changed one field and saved it back.
	loaded, err := st.GetApp(ctx, "shop")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	loaded.Name = "Shop Front"
	if err := st.UpdateApp(ctx, loaded); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := st.GetApp(ctx, "shop")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Port != 8080 || got.Dockerfile != "Dockerfile" || got.Env["A"] != "1" {
		t.Errorf("an update dropped fields the caller did not touch: %+v", got)
	}
	if got.Name != "Shop Front" {
		t.Errorf("name = %q, want the change that was made", got.Name)
	}
}

// TestUpdateDoesNotFreezeADerivedField asserts the namespace is not persisted.
//
// It depends on where AppLab runs, so a stored copy would follow a deployment
// that moved rather than the deployment that is.
func TestUpdateDoesNotFreezeADerivedField(t *testing.T) {
	st := newTestStore(t).Derives(func(app *model.App) { app.Namespace = "ns-" + app.ID })
	ctx := context.Background()

	if err := st.CreateApp(ctx, &model.App{ID: "shop", Name: "Shop"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := st.GetApp(ctx, "shop")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Namespace != "ns-shop" {
		t.Fatalf("namespace = %q, want ns-shop", got.Namespace)
	}

	// A write of what was read must not persist the derived value.
	if err := st.UpdateApp(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}

	raw, err := st.Objects().GetBytes(ctx, "apps/shop/app.json")
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	var stored model.App
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("decode the record: %v", err)
	}
	if stored.Namespace != "" {
		t.Errorf("the namespace was persisted as %q; it is derived and must not be", stored.Namespace)
	}
}

// TestDerivesRunsOnEveryRead asserts the hook is applied wherever an app comes
// from, not only from GetApp. A field filled in on one path and not another is
// the failure this exists to prevent, and the namespace is the expensive one to
// miss.
func TestDerivesRunsOnEveryRead(t *testing.T) {
	st := newTestStore(t).Derives(func(app *model.App) { app.Namespace = "ns-" + app.ID })
	ctx := context.Background()

	for _, id := range []string{"shop", "blog"} {
		if err := st.CreateApp(ctx, &model.App{ID: id, Name: id}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	if got, _ := st.GetApp(ctx, "shop"); got.Namespace != "ns-shop" {
		t.Errorf("GetApp namespace = %q", got.Namespace)
	}

	apps, err := st.ListApps(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, app := range apps {
		if app.Namespace != "ns-"+app.ID {
			t.Errorf("ListApps returned %s with namespace %q", app.ID, app.Namespace)
		}
	}
}

// TestDeleteAppRemovesEverythingUnderIt asserts nothing is left behind, which is
// what frees the id and what keeps a deleted app's source from being stored —
// and billed for — forever.
func TestDeleteAppRemovesEverythingUnderIt(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.CreateApp(ctx, &model.App{ID: "shop", Name: "Shop"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, key := range []string{
		"apps/shop/key.json",
		"apps/shop/commits/abc.json",
		"apps/shop/builds/def.json",
		"apps/shop/repo/HEAD",
	} {
		if err := st.Objects().PutBytes(ctx, key, []byte("{}")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}

	if err := st.DeleteApp(ctx, "shop"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	objects, err := st.Objects().List(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(objects) != 0 {
		var keys []string
		for _, object := range objects {
			keys = append(keys, object.Key)
		}
		t.Errorf("delete left %v behind", keys)
	}

	// And the id is free again.
	if err := st.CreateApp(ctx, &model.App{ID: "shop", Name: "Again"}); err != nil {
		t.Errorf("a deleted app's id was not reusable: %v", err)
	}
}

// TestAppsAreListedNewestFirst asserts the ordering a listing has to impose
// itself, because a bucket returns keys in name order rather than by time.
func TestAppsAreListedNewestFirst(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Created out of order, and with the ids chosen so key order and creation
	// order disagree: "aaa" sorts first and was made last.
	for i, id := range []string{"ccc", "bbb", "aaa"} {
		app := &model.App{ID: id, Name: id}
		if err := st.CreateApp(ctx, app); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		// A distinct timestamp per app, because two created in the same second
		// would order by the id tiebreak and the assertion would be about that
		// rather than about time.
		app.CreatedAt = app.CreatedAt.Add(time.Duration(i) * time.Second)
		if err := st.UpdateApp(ctx, app); err != nil {
			t.Fatalf("stamp %s: %v", id, err)
		}
	}

	apps, err := st.ListApps(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	var ids []string
	for _, app := range apps {
		ids = append(ids, app.ID)
	}
	if strings.Join(ids, ",") != "aaa,bbb,ccc" {
		t.Errorf("list = %v, want newest first (aaa,bbb,ccc)", ids)
	}
}

// TestAMissingAppIsErrNotFound asserts the distinction the API layer needs: an
// app that is not there and an app that failed to read are different answers,
// and only one of them is a 404.
func TestAMissingAppIsErrNotFound(t *testing.T) {
	st := newTestStore(t)

	if _, err := st.GetApp(context.Background(), "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetApp returned %v, want ErrNotFound", err)
	}
}

// TestAMalformedRecordIsReportedNotSwallowed asserts a corrupt object surfaces.
//
// Dropping it would make an app vanish from a listing with nothing to explain
// why — and the two causes, a write this build cannot read and a truncated
// write, are both things an operator needs to be told about.
func TestAMalformedRecordIsReportedNotSwallowed(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.Objects().PutBytes(ctx, "apps/broken/app.json", []byte("not json")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := st.GetApp(ctx, "broken"); err == nil {
		t.Error("a malformed record loaded without error")
	}
	if _, err := st.ListApps(ctx); err == nil {
		t.Error("a malformed record was skipped without error")
	}
}

// TestEnvRoundTrips covers the app's configuration through every path that
// touches it: create, read, list and update.
func TestEnvRoundTrips(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	app := &model.App{
		ID:         "shop",
		Name:       "Shop",
		Port:       8080,
		Dockerfile: "Dockerfile",
		Env:        map[string]string{"LOG_LEVEL": "debug", "FEATURE_X": "on"},
	}
	if err := st.CreateApp(ctx, app); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := st.GetApp(ctx, "shop")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Env) != 2 || got.Env["LOG_LEVEL"] != "debug" || got.Env["FEATURE_X"] != "on" {
		t.Errorf("env after create = %v, want both variables", got.Env)
	}

	all, err := st.ListApps(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || all[0].Env["LOG_LEVEL"] != "debug" {
		t.Errorf("env after list = %v, want the variable to survive", all)
	}

	got.Env["LOG_LEVEL"] = "warn"
	delete(got.Env, "FEATURE_X")
	if err := st.UpdateApp(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}

	again, err := st.GetApp(ctx, "shop")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if again.Env["LOG_LEVEL"] != "warn" {
		t.Errorf("LOG_LEVEL = %q, want warn", again.Env["LOG_LEVEL"])
	}
	if _, present := again.Env["FEATURE_X"]; present {
		t.Errorf("FEATURE_X survived a delete: %v", again.Env)
	}
}

// TestAnAppWithNoEnvRoundTripsAsNothing asserts the ordinary case: an app with no
// variables configured, which is most of them.
func TestAnAppWithNoEnvRoundTripsAsNothing(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.CreateApp(ctx, &model.App{ID: "bare", Name: "Bare"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := st.GetApp(ctx, "bare")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Env != nil {
		t.Errorf("env = %v, want nil for an app with no variables", got.Env)
	}
}

// TestTheStoreDoesNotOpenWithNothing asserts the failure a misconfiguration
// produces, at the point it happens rather than at the first write.
func TestTheStoreDoesNotOpenWithNothing(t *testing.T) {
	if _, err := Open(context.Background(), nil); err == nil {
		t.Error("opening a store with no object store succeeded")
	}
}

// TestKeysNeverCollideAcrossApps is the isolation check: two apps with the same
// commit sha and build id must not share an object.
func TestKeysNeverCollideAcrossApps(t *testing.T) {
	if commitKey("shop", "abc") == commitKey("blog", "abc") {
		t.Error("two apps share a commit key")
	}
	if buildKey("shop", "abc") == buildKey("blog", "abc") {
		t.Error("two apps share a build key")
	}
	if SourcePrefix("shop") == SourcePrefix("blog") {
		t.Error("two apps share a repository prefix")
	}
}

// TestThePrefixedStoreKeepsKeysInsideThePrefix covers mounting one AppLab under
// a prefix so a bucket can hold more than one.
func TestThePrefixedStoreKeepsKeysInsideThePrefix(t *testing.T) {
	inner, err := objectstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	mounted := objectstore.Prefixed(inner, "team-a")

	st, err := Open(context.Background(), mounted)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()

	if err := st.CreateApp(ctx, &model.App{ID: "shop", Name: "Shop"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// The app is found through the mount...
	apps, err := st.ListApps(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(apps) != 1 || apps[0].ID != "shop" {
		t.Fatalf("list through the mount = %v", apps)
	}

	// ...and written under the prefix, so a second mount shares the bucket
	// without sharing keys.
	raw, err := inner.GetBytes(ctx, "team-a/apps/shop/app.json")
	if err != nil {
		t.Fatalf("the object is not under the prefix: %v", err)
	}
	if !strings.Contains(string(raw), `"shop"`) {
		t.Errorf("the prefixed object holds %s", raw)
	}
}
