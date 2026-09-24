// Package store persists applab's state in SQLite.
//
// The database holds only applab's own view of the world — which apps exist,
// what was uploaded, what was built and deployed. It is deliberately not a
// mirror of the cluster: the cluster stays the source of truth for what is
// actually running, and a row here records the last thing applab attempted.
//
// SQLite is chosen because it needs no server to operate and the whole control
// plane is a single replica. That single-replica assumption is load-bearing:
// see Open for why sharing this file between pods is not safe.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	// The pure-Go driver, so the binary needs no cgo and the image can be
	// built with CGO_ENABLED=0 and run on a distroless base.
	_ "modernc.org/sqlite"
)

// Store is a handle to the database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and brings the
// schema up to date.
//
// The connection pool is capped at a single connection. SQLite allows one
// writer at a time, and with a larger pool two connections can deadlock
// upgrading from a read to a write lock — a failure that surfaces as random
// SQLITE_BUSY errors under load and is miserable to debug. applab's queries are
// short, so serialising them costs nothing that matters and removes the whole
// class of flakiness.
//
// This also means the database file must not be shared between replicas: two
// processes cannot coordinate through one SQLite file over a network volume.
// v1 therefore runs one replica, and scaling out means moving to a database
// built for it.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	// WAL lets a reader proceed while a writer holds the lock, and the busy
	// timeout makes a concurrent access wait rather than fail outright.
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to database %s: %w", path, err)
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for tests that need to inspect state directly.
func (s *Store) DB() *sql.DB { return s.db }

// migrations are applied in order, and the count applied is recorded in
// PRAGMA user_version. To change the schema, append — never edit an entry that
// has shipped, because a database in the field has already applied it and will
// not apply the edit.
//
// Timestamps are integer Unix seconds rather than a native date type: the
// format is unambiguous, sorts correctly as an integer, and needs no
// driver-specific conversion.
var migrations = []string{
	// 1 — apps, commits, builds, uploads.
	`
	CREATE TABLE apps (
		id            TEXT    PRIMARY KEY,
		name          TEXT    NOT NULL DEFAULT '',
		port          INTEGER NOT NULL DEFAULT 8080,
		replicas      INTEGER NOT NULL DEFAULT 1,
		dockerfile    TEXT    NOT NULL DEFAULT 'Dockerfile',
		domain        TEXT    NOT NULL DEFAULT '',
		commit_sha    TEXT    NOT NULL DEFAULT '',
		image         TEXT    NOT NULL DEFAULT '',
		status        TEXT    NOT NULL,
		status_reason TEXT    NOT NULL DEFAULT '',
		created_at    INTEGER NOT NULL,
		updated_at    INTEGER NOT NULL
	);

	CREATE TABLE commits (
		app_id     TEXT    NOT NULL,
		sha        TEXT    NOT NULL,
		message    TEXT    NOT NULL DEFAULT '',
		author     TEXT    NOT NULL DEFAULT '',
		files      INTEGER NOT NULL DEFAULT 0,
		bytes      INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		PRIMARY KEY (app_id, sha)
	);

	CREATE INDEX idx_commits_app_created ON commits (app_id, created_at DESC);

	CREATE TABLE builds (
		id          TEXT    PRIMARY KEY,
		app_id      TEXT    NOT NULL,
		commit_sha  TEXT    NOT NULL,
		image       TEXT    NOT NULL DEFAULT '',
		job_name    TEXT    NOT NULL DEFAULT '',
		status      TEXT    NOT NULL,
		reason      TEXT    NOT NULL DEFAULT '',
		created_at  INTEGER NOT NULL,
		started_at  INTEGER NOT NULL DEFAULT 0,
		finished_at INTEGER NOT NULL DEFAULT 0
	);

	CREATE INDEX idx_builds_app_created ON builds (app_id, created_at DESC);

	CREATE TABLE uploads (
		id         TEXT    PRIMARY KEY,
		app_id     TEXT    NOT NULL,
		total      INTEGER NOT NULL,
		chunk_size INTEGER NOT NULL,
		message    TEXT    NOT NULL DEFAULT '',
		author     TEXT    NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL
	);
	`,

	// 2 — an app's environment variables.
	//
	// JSON in a TEXT column rather than a table of its own. The map is small,
	// always read and written with the app it belongs to, and never queried
	// independently — so a join would buy nothing and cost a second thing to
	// keep consistent. Secrets are not here at all; they live in Kubernetes,
	// where a credential belongs.
	`
	ALTER TABLE apps ADD COLUMN env TEXT NOT NULL DEFAULT '{}';
	`,
}

// migrate applies every migration the database has not seen yet.
func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(migrations) {
		// A newer applab wrote this file. Continuing could corrupt data in ways
		// this version does not know about, so refuse rather than guess.
		return fmt.Errorf("database schema version %d is newer than this build understands (%d); upgrade applab", version, len(migrations))
	}

	for i := version; i < len(migrations); i++ {
		// Each migration and its version bump commit together, so an
		// interrupted upgrade leaves a consistent version rather than a
		// half-applied schema.
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", i+1, err)
		}
		// user_version cannot be a bound parameter; the value is a loop index,
		// so interpolation is safe.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", i+1, err)
		}
	}
	return nil
}
