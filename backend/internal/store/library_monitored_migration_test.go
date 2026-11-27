package store

import (
	"database/sql"
	"testing"
)

// TestMigration0011AddsMonitoredColumn applies 0011 to a database seeded at the
// 0001-0010 baseline. It pins what the release's only migration promises: the
// column exists afterwards, a row written before it existed reads 0 (the
// default) rather than an unknown/NULL, a later write of 1 reads back as 1, and
// the revision is recorded exactly once so a restart cannot apply it again.
func TestMigration0011AddsMonitoredColumn(t *testing.T) {
	db := openPreMigrationsDB(t, 11) // every revision below 0011

	if columnExists(t, db, "arr_library_cache", "monitored") {
		t.Fatal("baseline already has arr_library_cache.monitored; it was not built pre-0011")
	}

	if _, err := db.Exec(
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		 VALUES ('main', 'radarr', 'http://radarr.invalid', x'00', '0000')`); err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	// Written WITHOUT the column, exactly as a row from the previous release
	// would be: the migration must give it the default.
	rowID := insertCacheRow(t, db, 1, 42, "Heat", sql.NullString{})

	all, err := loadMigrations(embeddedMigrations)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	var target migration
	found := false
	for _, m := range all {
		if m.Version == 11 {
			target, found = m, true
		}
	}
	if !found {
		t.Fatal("migration 0011 is not embedded; the file must be migrations/0011_<name>.sql")
	}
	if target.Name != "library_monitored" {
		t.Errorf("migration 11 name = %q, want %q", target.Name, "library_monitored")
	}

	if err := applyMigration(db, target); err != nil {
		t.Fatalf("apply 0011: %v", err)
	}

	if !columnExists(t, db, "arr_library_cache", "monitored") {
		t.Fatal("arr_library_cache.monitored missing after 0011")
	}

	// The pre-existing row reads 0, not NULL: NOT NULL DEFAULT 0 is the whole
	// point, because an unknown state must not be presented as "unmonitored"
	// by a NULL sneaking through.
	var monitored int
	if err := db.QueryRow(
		`SELECT monitored FROM arr_library_cache WHERE id = ?`, rowID,
	).Scan(&monitored); err != nil {
		t.Fatalf("read monitored of the pre-0011 row: %v", err)
	}
	if monitored != 0 {
		t.Errorf("pre-0011 row monitored = %d, want 0 (the column default)", monitored)
	}

	// A row inserted after 0011 without the column gets the same default, and
	// an explicit NULL is refused by the NOT NULL constraint.
	seeded := insertCacheRow(t, db, 1, 43, "Alien", sql.NullString{})
	if err := db.QueryRow(
		`SELECT monitored FROM arr_library_cache WHERE id = ?`, seeded,
	).Scan(&monitored); err != nil {
		t.Fatalf("read monitored of a post-0011 row: %v", err)
	}
	if monitored != 0 {
		t.Errorf("post-0011 row monitored = %d, want 0 (the column default)", monitored)
	}
	if _, err := db.Exec(
		`UPDATE arr_library_cache SET monitored = NULL WHERE id = ?`, seeded); err == nil {
		t.Error("monitored accepted NULL; the column is still nullable")
	}

	// The sync pass (U02) can write the real value and read it back.
	if _, err := db.Exec(
		`UPDATE arr_library_cache SET monitored = 1 WHERE id = ?`, rowID); err != nil {
		t.Fatalf("set monitored = 1: %v", err)
	}
	if err := db.QueryRow(
		`SELECT monitored FROM arr_library_cache WHERE id = ?`, rowID,
	).Scan(&monitored); err != nil {
		t.Fatalf("read monitored after the update: %v", err)
	}
	if monitored != 1 {
		t.Errorf("monitored after UPDATE = %d, want 1", monitored)
	}

	// Restart: the runner sees 0011 recorded and applies nothing, so the
	// revision is recorded once and the written value survives.
	latest, err := migrate(db, embeddedMigrations)
	if err != nil {
		t.Fatalf("migrate after 0011 (restart): %v", err)
	}
	if latest != 11 {
		t.Errorf("migrated version = %d, want 11", latest)
	}
	var recorded int
	if err := db.QueryRow(
		`SELECT count(*) FROM schema_migrations WHERE version = 11`).Scan(&recorded); err != nil {
		t.Fatalf("count 0011 rows in schema_migrations: %v", err)
	}
	if recorded != 1 {
		t.Errorf("schema_migrations rows for version 11 = %d, want 1 (a restart must not re-apply it)", recorded)
	}
	var total int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&total); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if total != len(all) {
		t.Errorf("schema_migrations rows = %d, want %d (one per embedded migration)", total, len(all))
	}
	if err := db.QueryRow(
		`SELECT monitored FROM arr_library_cache WHERE id = ?`, rowID,
	).Scan(&monitored); err != nil {
		t.Fatalf("read monitored after the restart: %v", err)
	}
	if monitored != 1 {
		t.Errorf("monitored after the restart = %d, want the written 1", monitored)
	}
}
