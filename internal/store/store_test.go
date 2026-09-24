package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/shaowenchen/applab/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestMigrationAppliesToAFreshDatabase asserts a new database ends up at the
// current schema version.
func TestMigrationAppliesToAFreshDatabase(t *testing.T) {
	st := newTestStore(t)

	var version int
	if err := st.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != len(migrations) {
		t.Errorf("user_version = %d, want %d", version, len(migrations))
	}
}

// TestMigrationUpgradesAVersionOneDatabase is the test that matters.
//
// Every database already in the field is at version 1, and the migration that
// adds the env column has to bring it forward without losing what is in it. A
// fresh-database test would never exercise this: it starts from nothing and
// would pass even if the ALTER were wrong.
func TestMigrationUpgradesAVersionOneDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	ctx := context.Background()

	// Build a version-1 database by hand: the shipped first migration, a row,
	// and the version it recorded.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.ExecContext(ctx, migrations[0]); err != nil {
		t.Fatalf("apply migration 1: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `
		INSERT INTO apps (id, name, port, replicas, dockerfile, domain, commit_sha, image,
		                  status, status_reason, created_at, updated_at)
		VALUES ('shop', 'Shop', 8080, 1, 'Dockerfile', '', '', '', 'running', '', 1000, 1000)`); err != nil {
		t.Fatalf("insert a v1 app: %v", err)
	}
	if _, err := raw.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		t.Fatalf("set version 1: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	// Opening it now runs the remaining migrations.
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v1 database: %v", err)
	}
	defer st.Close()

	var version int
	if err := st.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != len(migrations) {
		t.Errorf("user_version after upgrade = %d, want %d", version, len(migrations))
	}

	// The row that was there is still there, and its new column is empty rather
	// than a decode error — which is what the DEFAULT on the ALTER has to
	// achieve for every app that existed before this column did.
	app, err := st.GetApp(ctx, "shop")
	if err != nil {
		t.Fatalf("read the app that predates the migration: %v", err)
	}
	if app.Name != "Shop" || app.Port != 8080 || app.Status != model.AppStatusRunning {
		t.Errorf("the migrated app lost its fields: %+v", app)
	}
	if len(app.Env) != 0 {
		t.Errorf("env = %v, want none for an app that predates the column", app.Env)
	}
}

// TestRefusesADatabaseFromANewerBuild asserts a rollback fails loudly.
//
// Running an older applab against a newer schema could corrupt data this version
// does not know about, so it refuses rather than guessing.
func TestRefusesADatabaseFromANewerBuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	ctx := context.Background()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.ExecContext(ctx, "PRAGMA user_version = 999"); err != nil {
		t.Fatalf("set version: %v", err)
	}
	raw.Close()

	if _, err := Open(ctx, path); err == nil {
		t.Fatal("opening a database written by a newer build succeeded, want a refusal")
	}
}

// TestEnvRoundTrips covers the JSON column through every path that touches it.
func TestEnvRoundTrips(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	app := &model.App{
		ID:         "shop",
		Name:       "Shop",
		Port:       8080,
		Replicas:   1,
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

	// Through the list, which shares scanApp but has its own column list — the
	// duplication that a new column has to be added to in both places.
	all, err := st.ListApps(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || all[0].Env["LOG_LEVEL"] != "debug" {
		t.Errorf("env after list = %v, want the variable to survive", all)
	}

	// And through an update.
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

// TestAppWithNoEnvDecodesToNothing asserts the ordinary case, and the one every
// migrated row is in.
func TestAppWithNoEnvDecodesToNothing(t *testing.T) {
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

// TestMalformedEnvIsReportedNotSwallowed asserts a corrupt value surfaces.
//
// Dropping it would silently strip an app's configuration — the app would deploy
// with no variables and nothing would say why.
func TestMalformedEnvIsReportedNotSwallowed(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.CreateApp(ctx, &model.App{ID: "broken", Name: "Broken"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE apps SET env = 'not json' WHERE id = 'broken'`); err != nil {
		t.Fatalf("corrupt the row: %v", err)
	}

	if _, err := st.GetApp(ctx, "broken"); err == nil {
		t.Fatal("a malformed env column loaded without error; it should be reported")
	}
}

// TestEncodeEnvWritesAnObjectForNoVariables asserts the stored shape is uniform.
func TestEncodeEnvWritesAnObjectForNoVariables(t *testing.T) {
	for _, env := range []map[string]string{nil, {}} {
		if got := encodeEnv(env); got != "{}" {
			t.Errorf("encodeEnv(%v) = %q, want {}", env, got)
		}
	}

	if got := encodeEnv(map[string]string{"A": "1"}); got != `{"A":"1"}` {
		t.Errorf("encodeEnv = %q", got)
	}
}

// TestDecodeEnvTreatsAnEmptyColumnAsNoVariables covers the value ALTER TABLE
// backfills, which every pre-existing row carries.
func TestDecodeEnvTreatsAnEmptyColumnAsNoVariables(t *testing.T) {
	env, err := decodeEnv("")
	if err != nil {
		t.Fatalf("decodeEnv(\"\") = %v, want nil", err)
	}
	if env != nil {
		t.Errorf("decodeEnv(\"\") = %v, want nil", env)
	}

	env, err = decodeEnv("{}")
	if err != nil || env != nil {
		t.Errorf("decodeEnv(\"{}\") = %v, %v; want nil, nil", env, err)
	}
}
