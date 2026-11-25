package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// testFS builds an in-memory migrations tree rooted at "migrations/" so
// loadMigrations/migrate can be exercised without touching the go:embed copy.
func testFS(files map[string]string) fstest.MapFS {
	fsys := make(fstest.MapFS, len(files))
	for name, body := range files {
		fsys["migrations/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

// testDB opens a throwaway SQLite database for migration-runner tests.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(filepath.Join(t.TempDir(), "test.db")))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name,
	).Scan(&n); err != nil {
		t.Fatalf("query sqlite_master for %q: %v", name, err)
	}
	return n == 1
}

// columnExists reports whether table has a column named column (PRAGMA
// table_info is the only portable way SQLite exposes a column list).
func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("pragma table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultVal, &pk); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info(%s): %v", table, err)
	}
	return false
}

// TestMigration0005Additive applies 0005 to a COPY of an existing database, so
// the pre-migration fixture stays untouched, and pins that the revision is
// purely additive (four new columns) and that re-running it is a no-op.
func TestMigration0005Additive(t *testing.T) {
	dir := t.TempDir()
	livePath := filepath.Join(dir, "live.db")

	all, err := loadMigrations(embeddedMigrations)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}

	// Build the pre-0005 database: every revision below 5, none of 5+.
	live, err := sql.Open("sqlite", dsn(livePath))
	if err != nil {
		t.Fatalf("open live db: %v", err)
	}
	live.SetMaxOpenConns(1)
	if _, err := live.Exec(schemaMigrationsDDL); err != nil {
		_ = live.Close()
		t.Fatalf("create schema_migrations: %v", err)
	}
	for _, m := range all {
		if m.Version >= 5 {
			continue
		}
		if err := applyMigration(live, m); err != nil {
			t.Fatalf("apply migration %d (%s): %v", m.Version, m.Name, err)
		}
	}
	if columnExists(t, live, "arr_instances", "last_sync_ok") {
		t.Fatal("live fixture already has last_sync_ok; it was not built pre-0005")
	}

	// Copy the database and migrate the copy — never the source.
	copyPath := filepath.Join(dir, "copy.db")
	if _, err := live.Exec(`VACUUM INTO ?`, copyPath); err != nil {
		_ = live.Close()
		t.Fatalf("copy database: %v", err)
	}
	if err := live.Close(); err != nil {
		t.Fatalf("close live db: %v", err)
	}

	copied, err := sql.Open("sqlite", dsn(copyPath))
	if err != nil {
		t.Fatalf("open copied db: %v", err)
	}
	defer copied.Close()
	copied.SetMaxOpenConns(1)

	latest, err := migrate(copied, embeddedMigrations)
	if err != nil {
		t.Fatalf("migrate copy: %v", err)
	}
	if latest != 5 {
		t.Fatalf("migrated copy version = %d, want 5", latest)
	}

	for _, tc := range []struct{ table, column string }{
		{"arr_instances", "last_sync_ok"},
		{"arr_instances", "last_sync_error"},
		{"arr_instances", "last_attempt_at"},
		{"arr_library_cache", "last_seen_at"},
	} {
		if !columnExists(t, copied, tc.table, tc.column) {
			t.Errorf("%s.%s missing after 0005", tc.table, tc.column)
		}
	}

	// Idempotent: a second pass records nothing and adds no duplicate column.
	again, err := migrate(copied, embeddedMigrations)
	if err != nil {
		t.Fatalf("migrate copy (second pass): %v", err)
	}
	if again != latest {
		t.Errorf("second pass version = %d, want %d", again, latest)
	}
	var recorded int
	if err := copied.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&recorded); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if recorded != len(all) {
		t.Errorf("schema_migrations rows = %d, want %d", recorded, len(all))
	}

	// The source database never saw 0005.
	untouched, err := sql.Open("sqlite", dsn(livePath))
	if err != nil {
		t.Fatalf("reopen live db: %v", err)
	}
	defer untouched.Close()
	untouched.SetMaxOpenConns(1)
	if columnExists(t, untouched, "arr_library_cache", "last_seen_at") {
		t.Error("live database was migrated; migrations must run on a copy")
	}
}

// TestMigrateRollsBackFailedMigration pins the atomicity guarantee: a migration
// that fails halfway must leave neither its bookkeeping row nor any table it
// created before the failing statement.
func TestMigrateRollsBackFailedMigration(t *testing.T) {
	fsys := testFS(map[string]string{
		"0001_ok.sql":  "CREATE TABLE alpha (id INTEGER PRIMARY KEY);",
		"0002_bad.sql": "CREATE TABLE beta (id INTEGER PRIMARY KEY);\nINSERT INTO missing_table (x) VALUES (1);",
	})
	db := testDB(t)

	if _, err := migrate(db, fsys); err == nil {
		t.Fatal("migrate succeeded, want error from failing migration 0002")
	} else if !strings.Contains(err.Error(), "migration 2") {
		t.Errorf("error = %v, want mention of migration 2", err)
	}

	var rows, maxVersion int
	if err := db.QueryRow(`SELECT count(*), max(version) FROM schema_migrations`).Scan(&rows, &maxVersion); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if rows != 1 || maxVersion != 1 {
		t.Errorf("schema_migrations rows=%d max=%d, want rows=1 max=1", rows, maxVersion)
	}
	if !tableExists(t, db, "alpha") {
		t.Error("alpha table from successful migration 0001 is missing")
	}
	if tableExists(t, db, "beta") {
		t.Error("beta table from failed migration 0002 survived: transaction was not rolled back")
	}
}

// TestLoadMigrationsDuplicateVersion covers the duplicate-version guard.
func TestLoadMigrationsDuplicateVersion(t *testing.T) {
	fsys := testFS(map[string]string{
		"0001_a.sql": "SELECT 1;",
		"0001_b.sql": "SELECT 1;",
	})
	_, err := loadMigrations(fsys)
	if err == nil {
		t.Fatal("loadMigrations accepted two migrations with version 1")
	}
	if !strings.Contains(err.Error(), "duplicate migration version 1") {
		t.Errorf("error = %v, want duplicate version message", err)
	}
}

// TestLoadMigrationsInvalidNames covers the filename/version guards.
func TestLoadMigrationsInvalidNames(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		wantErr string // substring; empty means the file must be skipped
	}{
		{name: "no separator", files: map[string]string{"0001.sql": "SELECT 1;"}, wantErr: "expected <version>_<name>.sql"},
		{name: "non numeric version", files: map[string]string{"abc_x.sql": "SELECT 1;"}, wantErr: "invalid version prefix"},
		{name: "zero version", files: map[string]string{"0000_zero.sql": "SELECT 1;"}, wantErr: "invalid version prefix"},
		{name: "negative version", files: map[string]string{"-5_neg.sql": "SELECT 1;"}, wantErr: "invalid version prefix"},
		{name: "non sql ignored", files: map[string]string{"README.md": "not a migration"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadMigrations(testFS(tc.files))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("loadMigrations: %v", err)
				}
				if len(got) != 0 {
					t.Fatalf("loadMigrations returned %d migrations, want 0", len(got))
				}
				return
			}
			if err == nil {
				t.Fatalf("loadMigrations accepted %v", tc.files)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestMigrateRejectsGapFill covers forward-only semantics: an unapplied
// migration below the highest recorded version must be rejected rather than
// silently applied out of order.
func TestMigrateRejectsGapFill(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(schemaMigrationsDDL); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (version, name) VALUES (2, 'two')`); err != nil {
		t.Fatalf("record version 2: %v", err)
	}

	fsys := testFS(map[string]string{
		"0001_one.sql": "CREATE TABLE one_t (id INTEGER PRIMARY KEY);",
		"0002_two.sql": "CREATE TABLE two_t (id INTEGER PRIMARY KEY);",
	})
	if _, err := migrate(db, fsys); err == nil {
		t.Fatal("migrate gap-filled version 1 below recorded version 2, want error")
	}

	if tableExists(t, db, "one_t") {
		t.Error("gap-fill migration 0001 was applied despite the guard")
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&rows); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if rows != 1 {
		t.Errorf("schema_migrations rows = %d, want 1", rows)
	}
}

// TestOpenCorrectsLoosePermissions pre-creates a world-readable data dir and
// database file, then requires Open to tighten both.
func TestOpenCorrectsLoosePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(dir, DBFile)
	if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
		t.Fatalf("create db file: %v", err)
	}
	if err := os.Chmod(dbPath, 0o644); err != nil { // defeat umask
		t.Fatalf("chmod db file: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // defeat umask
		t.Fatalf("chmod dir: %v", err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat data dir: %v", err)
	}
	if got := di.Mode().Perm(); got != dirPerm {
		t.Errorf("data dir perms = %o, want %o", got, dirPerm)
	}
	fi, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if got := fi.Mode().Perm(); got != filePerm {
		t.Errorf("db perms = %o, want %o", got, filePerm)
	}
}

// TestOpenHardensDatabaseFileBeforeMigrating pins the perms-window fix: a
// pre-existing loose db file must be tightened right after pre-creation, before
// open/ping/migration can fail. A corrupt file forces Open to fail after that
// point, so only an early Chmod can leave the file at 0600.
func TestOpenHardensDatabaseFileBeforeMigrating(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(dir, DBFile)
	if err := os.WriteFile(dbPath, []byte("this is not a sqlite database"), 0o644); err != nil {
		t.Fatalf("create corrupt db file: %v", err)
	}
	if err := os.Chmod(dbPath, 0o644); err != nil {
		t.Fatalf("chmod db file: %v", err)
	}

	if s, err := Open(dir); err == nil {
		s.Close()
		t.Fatal("Open succeeded on a corrupt database file, want error")
	}

	fi, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if got := fi.Mode().Perm(); got != filePerm {
		t.Errorf("db perms after failed Open = %o, want %o (loose file must be tightened before open/migrate)", got, filePerm)
	}
}
