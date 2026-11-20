package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// expectedTables are the nine specification tables from IDEA.md §4 plus the
// migration bookkeeping table owned by the runner.
var expectedTables = []string{
	"users",
	"sessions",
	"api_tokens",
	"settings",
	"arr_instances",
	"arr_library_cache",
	"import_queue",
	"vault_media",
	"audio_tracks",
	"sync_mux_jobs",
	"secrets",
	"schema_migrations",
}

func TestOpenCreatesDatabaseAndSchema(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	exists := 0
	for _, table := range expectedTables {
		var n int
		if err := s.DB.QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&n); err != nil {
			t.Fatalf("query sqlite_master for %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("table %q missing from schema", table)
			continue
		}
		exists++
	}
	if exists != len(expectedTables) {
		t.Fatalf("got %d/%d tables", exists, len(expectedTables))
	}
}

func TestOpenEnablesWAL(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var mode string
	if err := s.DB.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}

func TestOpenEnablesForeignKeys(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var fk int
	if err := s.DB.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}

	// Behavioural proof, not just the pragma readback: an orphan child row
	// must be rejected.
	_, err = s.DB.Exec(
		`INSERT INTO arr_library_cache (arr_instance_id, media_type, external_item_id, title)
		 VALUES (9999, 'movie', 1, 'Orphan')`,
	)
	if err == nil {
		t.Fatal("insert with dangling arr_instance_id succeeded, foreign keys are not enforced")
	}
}

func TestOpenFilePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat data dir: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("data dir perms = %o, want 700", got)
	}

	fi, err := os.Stat(filepath.Join(dir, "dubarr.db"))
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("db perms = %o, want 600", got)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()

	var rows, version int
	if err := s2.DB.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&rows); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if rows != 1 {
		t.Errorf("schema_migrations rows = %d, want 1", rows)
	}
	if err := s2.DB.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("max version: %v", err)
	}
	if version != 1 {
		t.Errorf("latest version = %d, want 1", version)
	}

	// Tables must still be intact after the second open.
	var n int
	if err := s2.DB.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'sync_mux_jobs'`,
	).Scan(&n); err != nil {
		t.Fatalf("query schema: %v", err)
	}
	if n != 1 {
		t.Error("sync_mux_jobs vanished after re-open")
	}
}

func TestSchemaDefaults(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if _, err := s.DB.Exec(`INSERT INTO users (username, password_hash) VALUES ('admin', 'argon2id$v=19$m=65536,t=3,p=4$abc$def')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	var userID int64
	if err := s.DB.QueryRow(`SELECT id FROM users WHERE username = 'admin'`).Scan(&userID); err != nil {
		t.Fatalf("select user: %v", err)
	}

	if _, err := s.DB.Exec(
		`INSERT INTO sync_mux_jobs (target_track_id, target_video_path, target_lang_iso2, status)
		 VALUES (0, '/tmp/x.mkv', 'pl', 'queued')`,
	); err == nil {
		t.Fatal("sync_mux_jobs accepted missing audio_tracks reference")
	}

	if _, err := s.DB.Exec(
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		 VALUES ('Radarr', 'radarr', 'http://localhost:7878', X'00', 'abcd')`,
	); err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	var enabled int
	if err := s.DB.QueryRow(`SELECT enabled FROM arr_instances WHERE name = 'Radarr'`).Scan(&enabled); err != nil {
		t.Fatalf("select enabled: %v", err)
	}
	if enabled != 1 {
		t.Errorf("arr_instances.enabled default = %d, want 1", enabled)
	}
}

// TestSchemaSQLMirrorsMigration keeps backend/schema.sql — the human-facing
// schema referenced by IDEA.md and PLAN.md — byte-identical to the migration
// the runner actually applies. schema.sql must never drift from 0001_initial.sql.
func TestSchemaSQLMirrorsMigration(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("migrations", "0001_initial.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	got, err := os.ReadFile(filepath.Join("..", "..", "schema.sql"))
	if err != nil {
		t.Fatalf("read backend/schema.sql: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("backend/schema.sql drifted from internal/store/migrations/0001_initial.sql")
	}
}
