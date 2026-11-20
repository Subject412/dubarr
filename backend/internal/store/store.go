// Package store owns Dubarr's SQLite persistence layer: it creates DATA_DIR,
// opens the WAL-mode database with hardened permissions, and applies the
// forward-only migrations embedded from migrations/*.sql.
package store

import (
	"database/sql"
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

	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	// SQLite allows a single writer; one pooled connection keeps PRAGMAs and
	// write serialization consistent and avoids spurious SQLITE_BUSY.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping database: %w", err)
	}
	if _, err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	// WAL mode and migration writes must not widen the file mode.
	if err := os.Chmod(dbPath, filePerm); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: chmod database file: %w", err)
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

// Path returns the absolute database file path.
func (s *Store) Path() string { return s.dbPath }

// DataDir returns the directory the store was opened against.
func (s *Store) DataDir() string { return s.dataDir }

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.DB == nil {
		return nil
	}
	err := s.DB.Close()
	s.DB = nil
	return err
}
