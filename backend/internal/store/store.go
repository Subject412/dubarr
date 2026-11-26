// Store construction and permission hardening for the Dubarr database; see
// doc.go for the package overview.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go driver, CGO_ENABLED=0 compatible
)

const (
	// DBFile is the SQLite database filename inside DATA_DIR.
	DBFile = "dubarr.db"

	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// Store is an open handle on the Dubarr database. DB is exported so later
// packages (auth, secrets, settings) can run queries against the same pool.
type Store struct {
	DB      *sql.DB
	dataDir string
	dbPath  string
}

// Open ensures dataDir exists with mode 0700, opens dubarr.db (0600) with WAL
// journaling and foreign keys enforced, then applies any pending migrations.
// It is idempotent: re-opening an up-to-date database applies nothing and
// records no new schema_migrations rows.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("store: data dir must not be empty")
	}
	if err := os.MkdirAll(dataDir, dirPerm); err != nil {
		return nil, fmt.Errorf("store: create data dir: %w", err)
	}
	// MkdirAll keeps the mode of an existing directory, so tighten it explicitly.
	if err := os.Chmod(dataDir, dirPerm); err != nil {
		return nil, fmt.Errorf("store: chmod data dir: %w", err)
	}

	dbPath := filepath.Join(dataDir, DBFile)
	// Pre-create the file with 0600 so the driver never creates it with the
	// umask-derived mode.
	f, err := os.OpenFile(dbPath, os.O_RDWR|os.O_CREATE, filePerm)
	if err != nil {
		return nil, fmt.Errorf("store: create database file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("store: close database file: %w", err)
	}
	// Tighten a pre-existing loose file now, before the driver opens it: WAL and
	// -shm sidecars appear on the first write, so the db must not remain
	// group/world readable during open+ping+migration.
	if err := os.Chmod(dbPath, filePerm); err != nil {
		return nil, fmt.Errorf("store: chmod database file: %w", err)
	}

	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	// SQLite allows a single writer; one pooled connection keeps PRAGMAs and
	// write serialization consistent and avoids spurious SQLITE_BUSY.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		return nil, errors.Join(fmt.Errorf("store: ping database: %w", err), db.Close())
	}
	if _, err := migrate(db, embeddedMigrations); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	// Belt and braces: WAL mode and migration writes must not widen the mode.
	if err := os.Chmod(dbPath, filePerm); err != nil {
		return nil, errors.Join(fmt.Errorf("store: chmod database file: %w", err), db.Close())
	}

	return &Store{DB: db, dataDir: dataDir, dbPath: dbPath}, nil
}

// dsn builds a modernc.org/sqlite connection string with per-connection
// PRAGMAs. journal_mode cannot be changed inside a transaction, so it is set
// here rather than in schema.sql.
func dsn(dbPath string) string {
	return "file:" + filepath.ToSlash(dbPath) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"
}

// Path returns the database file path as configured, i.e.
// filepath.Join(dataDir, DBFile). It is absolute only when the data directory
// passed to Open was absolute; Open does not resolve it with filepath.Abs.
func (s *Store) Path() string { return s.dbPath }

// DataDir returns the directory the store was opened against.
func (s *Store) DataDir() string { return s.dataDir }

// Close releases the database handle and is idempotent. It deliberately leaves
// s.DB pointing at the closed *sql.DB: every package captures the handle once at
// wiring time, so clearing it here would race those readers (issue #62: the
// health handler dereferenced a nil DB while shutting down). After Close,
// queries fail with database/sql's "database is closed" error, which callers
// already report as "database unavailable". database/sql makes the second Close
// a no-op (DB.Close returns nil once its internal closed flag is set, and it is
// safe to call concurrently with queries), so the guards below are defence for
// a zero-value Store, not the idempotence mechanism.
func (s *Store) Close() error {
	if s == nil || s.DB == nil {
		return nil
	}
	return s.DB.Close()
}
