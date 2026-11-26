package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// openPreMigrationsDB opens a throwaway database and applies every embedded
// migration whose version is below upTo, leaving the database at that baseline.
// The migration files are applied directly (not via migrate) so a test can seed
// data that the next revision has to cope with.
func openPreMigrationsDB(t *testing.T, upTo int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(filepath.Join(t.TempDir(), "pre.db")))
	if err != nil {
		t.Fatalf("open pre-migration db: %v", err)
	}
	// A single connection keeps the TEMP table the migration creates visible
	// to every later statement.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(schemaMigrationsDDL); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	all, err := loadMigrations(embeddedMigrations)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	for _, m := range all {
		if m.Version >= upTo {
			continue
		}
		if _, err := db.Exec(m.SQL); err != nil {
			t.Fatalf("apply migration %d (%s): %v", m.Version, m.Name, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`,
			m.Version, m.Name); err != nil {
			t.Fatalf("record migration %d: %v", m.Version, err)
		}
	}
	return db
}

// insertCacheRow seeds one arr_library_cache row and returns its id.
func insertCacheRow(t *testing.T, db *sql.DB, instanceID, externalID int64, title string, lastSeen sql.NullString) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, title, eligibility_status,
		  target_file_path, last_seen_at)
		 VALUES (?, 'movie', ?, ?, 'eligible', ?, ?)`,
		instanceID, externalID, title, "/media/movies/"+title+"/"+title+".mkv", lastSeen)
	if err != nil {
		t.Fatalf("insert cache row %q: %v", title, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("cache row id: %v", err)
	}
	return id
}

// insertQueueRow seeds one import_queue row matched to cacheID and returns its id.
func insertQueueRow(t *testing.T, db *sql.DB, name string, cacheID int64) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO import_queue (source_file_path, filename, file_size, status, matched_library_cache_id)
		 VALUES (?, ?, 1, 'pending_approval', ?)`,
		"/watch/"+name, name, cacheID)
	if err != nil {
		t.Fatalf("insert import queue row %q: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("queue row id: %v", err)
	}
	return id
}

// insertMuxJob seeds the minimum an audio_tracks-backed sync_mux_jobs row needs
// and returns its id.
func insertMuxJob(t *testing.T, db *sql.DB, cacheID int64) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO vault_media (media_type, title, vault_folder_path)
		 VALUES ('movie', 'Donor', '/vault/Donor')`)
	if err != nil {
		t.Fatalf("insert vault media: %v", err)
	}
	mediaID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("vault media id: %v", err)
	}
	res, err = db.Exec(
		`INSERT INTO audio_tracks
		 (vault_media_id, group_uuid, lang_iso2, file_path, sha256, codec, channels, channel_count)
		 VALUES (?, 'group-1', 'en', '/vault/Donor/Donor.en.mka', 'sha', 'aac', '2.0', 2)`,
		mediaID)
	if err != nil {
		t.Fatalf("insert audio track: %v", err)
	}
	trackID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("audio track id: %v", err)
	}
	res, err = db.Exec(
		`INSERT INTO sync_mux_jobs
		 (target_track_id, target_library_cache_id, target_video_path, target_lang_iso2, status)
		 VALUES (?, ?, '/media/target.mkv', 'pl', 'queued')`,
		trackID, cacheID)
	if err != nil {
		t.Fatalf("insert mux job: %v", err)
	}
	jobID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("mux job id: %v", err)
	}
	return jobID
}

// TestMigration0006RepointsDuplicatesBeforeDelete applies the 0006 SQL, read
// from disk, to a database seeded at the 0001-0005 baseline with duplicate
// arr_library_cache rows. It pins the two things that make the cleanup safe:
// the deterministic survivor rule, and that the queue row and the mux job are
// repointed to the survivor BEFORE the loser is deleted. A NULL there would
// mean the FK's ON DELETE SET NULL fired first — a failure, not a pass.
func TestMigration0006RepointsDuplicatesBeforeDelete(t *testing.T) {
	db := openPreMigrationsDB(t, 6)

	if _, err := db.Exec(
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		 VALUES ('main', 'radarr', 'http://radarr.invalid', x'00', '0000')`); err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}

	// Group A: the loser is inserted second (higher id) but was observed
	// EARLIER, so last_seen_at must win over the id tie-break.
	survivor := insertCacheRow(t, db, 1, 7, "Heat", sql.NullString{String: "2024-05-02 00:00:00", Valid: true})
	loser := insertCacheRow(t, db, 1, 7, "Heat (stale)", sql.NullString{String: "2024-05-01 00:00:00", Valid: true})
	if loser <= survivor {
		t.Fatalf("fixture ids = survivor %d / loser %d, want the loser to have the higher id", survivor, loser)
	}

	// Group B: one row was observed, the other never was (NULL). The NULL row
	// is inserted second so it holds the HIGHER id: only "NULLs sort last" can
	// leave the timestamped row standing, because the id tie-break points the
	// other way.
	seenSurvivor := insertCacheRow(t, db, 1, 8, "Alien", sql.NullString{String: "2024-05-01 00:00:00", Valid: true})
	nullLoser := insertCacheRow(t, db, 1, 8, "Alien (never observed)", sql.NullString{})
	if nullLoser <= seenSurvivor {
		t.Fatalf("fixture ids = observed %d / NULL %d, want the NULL row to have the higher id", seenSurvivor, nullLoser)
	}

	// Group C: both unobserved (NULL), so the id DESC tie-break alone decides.
	// That is a distinct rule from group B, where the timestamps differ.
	tieLoser := insertCacheRow(t, db, 1, 10, "Aliens (first)", sql.NullString{})
	tieSurvivor := insertCacheRow(t, db, 1, 10, "Aliens", sql.NullString{})

	// A row that is already unique must not be touched by the cleanup.
	unique := insertCacheRow(t, db, 1, 9, "Dune", sql.NullString{String: "2024-04-01 00:00:00", Valid: true})

	queueLoser := insertQueueRow(t, db, "heat.mkv", loser)
	queueUnique := insertQueueRow(t, db, "dune.mkv", unique)
	muxLoser := insertMuxJob(t, db, loser)

	body, err := os.ReadFile(filepath.Join("migrations", "0006_library_cache_unique_key.sql"))
	if err != nil {
		t.Fatalf("read 0006 migration: %v", err)
	}
	if _, err := db.Exec(string(body)); err != nil {
		t.Fatalf("apply 0006: %v", err)
	}

	// Group A: exactly one row left, and it is the most recently seen one.
	var rows int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM arr_library_cache WHERE arr_instance_id = 1 AND external_item_id = 7`,
	).Scan(&rows); err != nil {
		t.Fatalf("count group A: %v", err)
	}
	if rows != 1 {
		t.Fatalf("group A rows = %d, want 1", rows)
	}
	var keptID int64
	var keptTitle string
	if err := db.QueryRow(
		`SELECT id, title FROM arr_library_cache WHERE arr_instance_id = 1 AND external_item_id = 7`,
	).Scan(&keptID, &keptTitle); err != nil {
		t.Fatalf("read group A survivor: %v", err)
	}
	if keptID != survivor || keptTitle != "Heat" {
		t.Errorf("group A survivor = %d/%q, want %d/%q", keptID, keptTitle, survivor, "Heat")
	}

	// Group B: the observed row survives even though the NULL row has the
	// higher id — "NULLs last" must beat the id tie-break.
	var groupBRows int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM arr_library_cache WHERE arr_instance_id = 1 AND external_item_id = 8`,
	).Scan(&groupBRows); err != nil {
		t.Fatalf("count group B: %v", err)
	}
	if groupBRows != 1 {
		t.Fatalf("group B rows = %d, want 1", groupBRows)
	}
	var seenKept int64
	if err := db.QueryRow(
		`SELECT id FROM arr_library_cache WHERE arr_instance_id = 1 AND external_item_id = 8`,
	).Scan(&seenKept); err != nil {
		t.Fatalf("read group B survivor: %v", err)
	}
	if seenKept != seenSurvivor {
		t.Errorf("group B survivor = %d, want the observed row %d (a NULL last_seen_at loses to a timestamp, even with the higher id)",
			seenKept, seenSurvivor)
	}

	// Group C: two NULL last_seen_at fall back to the highest id.
	var groupCRows int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM arr_library_cache WHERE arr_instance_id = 1 AND external_item_id = 10`,
	).Scan(&groupCRows); err != nil {
		t.Fatalf("count group C: %v", err)
	}
	if groupCRows != 1 {
		t.Fatalf("group C rows = %d, want 1", groupCRows)
	}
	var tieKept int64
	if err := db.QueryRow(
		`SELECT id FROM arr_library_cache WHERE arr_instance_id = 1 AND external_item_id = 10`,
	).Scan(&tieKept); err != nil {
		t.Fatalf("read group C survivor: %v", err)
	}
	if tieKept != tieSurvivor || tieKept == tieLoser {
		t.Errorf("group C survivor = %d, want %d (highest id when both last_seen_at are NULL)", tieKept, tieSurvivor)
	}

	// The queue row must now point at the survivor, never at NULL.
	var queueMatch sql.NullInt64
	if err := db.QueryRow(
		`SELECT matched_library_cache_id FROM import_queue WHERE id = ?`, queueLoser,
	).Scan(&queueMatch); err != nil {
		t.Fatalf("read queue match: %v", err)
	}
	if !queueMatch.Valid {
		t.Fatal("import_queue.matched_library_cache_id is NULL: the loser was deleted before the repoint")
	}
	if queueMatch.Int64 != survivor {
		t.Errorf("import_queue matched_library_cache_id = %d, want the survivor %d", queueMatch.Int64, survivor)
	}

	// The mux job likewise.
	var muxMatch sql.NullInt64
	if err := db.QueryRow(
		`SELECT target_library_cache_id FROM sync_mux_jobs WHERE id = ?`, muxLoser,
	).Scan(&muxMatch); err != nil {
		t.Fatalf("read mux job match: %v", err)
	}
	if !muxMatch.Valid {
		t.Fatal("sync_mux_jobs.target_library_cache_id is NULL: the loser was deleted before the repoint")
	}
	if muxMatch.Int64 != survivor {
		t.Errorf("sync_mux_jobs target_library_cache_id = %d, want the survivor %d", muxMatch.Int64, survivor)
	}

	// The already-unique row kept its id and its queue link.
	var uniqueMatch sql.NullInt64
	if err := db.QueryRow(
		`SELECT matched_library_cache_id FROM import_queue WHERE id = ?`, queueUnique,
	).Scan(&uniqueMatch); err != nil {
		t.Fatalf("read unique row match: %v", err)
	}
	if !uniqueMatch.Valid || uniqueMatch.Int64 != unique {
		t.Errorf("unique row match = %v, want %d (an already-unique row must not be touched)", uniqueMatch, unique)
	}
	var uniqueRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM arr_library_cache WHERE id = ?`, unique).Scan(&uniqueRows); err != nil {
		t.Fatalf("count unique row: %v", err)
	}
	if uniqueRows != 1 {
		t.Error("the unique row was deleted by the dedupe")
	}

	// The helper table must not survive the migration.
	var leftover int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_temp_master WHERE name = '_arr_cache_survivor'`,
	).Scan(&leftover); err != nil {
		t.Fatalf("read sqlite_temp_master: %v", err)
	}
	if leftover != 0 {
		t.Error("the migration left its temporary survivor table behind")
	}
}

// TestLibraryCacheIdentityRejectsRawDuplicate pins that the unique index, not
// just the Go writers, owns the identity: a raw INSERT of an existing triple
// must fail.
func TestLibraryCacheIdentityRejectsRawDuplicate(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	instanceID := syncFixture(t, s, ctx)

	if _, err := s.SyncLibraryCache(ctx, instanceID, []LibraryCacheItem{movieItem(1, "Heat")}); err != nil {
		t.Fatalf("seed library row: %v", err)
	}
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO arr_library_cache (arr_instance_id, media_type, external_item_id, title)
		 VALUES (?, 'movie', 1, 'Heat (duplicate)')`, instanceID); err == nil {
		t.Fatal("a duplicate (arr_instance_id, media_type, external_item_id) INSERT succeeded; the unique index is missing")
	}

	var rows int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM arr_library_cache WHERE arr_instance_id = ? AND external_item_id = 1`,
		instanceID).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("rows = %d, want 1", rows)
	}
}

// TestInsertLibraryCacheRowUpsertsExistingKey pins that the rescan's zero-row
// fallback refreshes the row holding the key instead of adding a second one:
// the row count stays put, the id is preserved and the observed values land.
func TestInsertLibraryCacheRowUpsertsExistingKey(t *testing.T) {
	s, ctx, instanceID := libraryItemFixture(t)

	if _, err := s.SyncLibraryCache(ctx, instanceID, []LibraryCacheItem{movieItem(5, "Heat")}); err != nil {
		t.Fatalf("seed library row: %v", err)
	}
	var before int64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT id FROM arr_library_cache WHERE arr_instance_id = ? AND external_item_id = 5`,
		instanceID).Scan(&before); err != nil {
		t.Fatalf("read seeded row: %v", err)
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertLibraryCacheRow(ctx, tx, instanceID, movieItem(5, "Heat (reread)"), "2030-01-01 00:00:00"); err != nil {
		t.Fatalf("insertLibraryCacheRow on an existing key: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var rows int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM arr_library_cache WHERE arr_instance_id = ? AND external_item_id = 5`,
		instanceID).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want 1 (the upsert must not duplicate the key)", rows)
	}
	var (
		id      int64
		title   string
		target  string
		updated time.Time
	)
	if err := s.DB.QueryRowContext(ctx,
		`SELECT id, title, COALESCE(target_file_path, ''), updated_at
		 FROM arr_library_cache WHERE arr_instance_id = ? AND external_item_id = 5`,
		instanceID).Scan(&id, &title, &target, &updated); err != nil {
		t.Fatalf("read upserted row: %v", err)
	}
	if id != before {
		t.Errorf("id = %d, want the existing row %d", id, before)
	}
	if title != "Heat (reread)" {
		t.Errorf("title = %q, want the re-read title", title)
	}
	if want := "/media/movies/Heat (reread)/Heat (reread) [ArcA310].mkv"; target != want {
		t.Errorf("target_file_path = %q, want %q", target, want)
	}
	// updated_at is a DATETIME column: the stored text is the space-separated
	// "2006-01-02 15:04:05" every writer produces, but the driver hands a
	// DATETIME back as time.Time, so scan it as one and compare instants.
	if want := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC); !updated.Equal(want) {
		t.Errorf("updated_at = %v, want the write's timestamp %v", updated, want)
	}
}

// TestRescanFallbackAfterPollerReinsertStaysOneRow is the honest attempt to
// construct a duplicate through the current writers, driven end to end against
// the real paths:
//
//  1. a poller pass sees the item and INSERTs its row            (id A)
//  2. the rescan reads that row                                  (holds A)
//  3. a poller pass no longer sees it and DELETEs A
//  4. a later poller pass sees it again and INSERTs a fresh row  (id B)
//  5. the rescan's write lands, keyed on the stale id A
//
// Step 5 is updateLibraryCacheRow's zero-row fallback. Without the upsert it
// INSERTs a second row for the key the poller's row B already holds — and that
// pair is the one that would then be permanent, since SyncLibraryCache's map
// collapses it and only ever updates (or deletes) the map's winner.
//
// Two notes on reachability, so this is not read as a claim of observed
// corruption: a plain read → delete → fallback (steps 1-3, 5) produces a single
// row either way, so the duplicate needs the item to reappear at step 4 while
// the rescan is still between its read and its write. That is a timing window,
// not something the current code guarantees to avoid; the unique index plus the
// upsert make the outcome one row regardless of how the passes interleave.
func TestRescanFallbackAfterPollerReinsertStaysOneRow(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	instanceID := syncFixture(t, s, ctx)

	// 1. The poller caches the item.
	if _, err := s.SyncLibraryCache(ctx, instanceID, []LibraryCacheItem{movieItem(1, "Heat")}); err != nil {
		t.Fatalf("poller pass (insert): %v", err)
	}
	// 2. The rescan reads the row it is about to refresh.
	stale, ok, err := s.libraryCacheRowByKey(ctx, instanceID, "movie", 1)
	if err != nil {
		t.Fatalf("rescan read: %v", err)
	}
	if !ok {
		t.Fatal("rescan found no row to read")
	}
	// 3. The item vanishes: the poller's snapshot deletes the row.
	if _, err := s.SyncLibraryCache(ctx, instanceID, nil); err != nil {
		t.Fatalf("poller pass (delete): %v", err)
	}
	// 4. The item is back: the next pass inserts a NEW row for the same key.
	if _, err := s.SyncLibraryCache(ctx, instanceID, []LibraryCacheItem{movieItem(1, "Heat (re-added)")}); err != nil {
		t.Fatalf("poller pass (re-insert): %v", err)
	}
	var freshID int64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT id FROM arr_library_cache WHERE arr_instance_id = ? AND external_item_id = 1`,
		instanceID).Scan(&freshID); err != nil {
		t.Fatalf("read the re-inserted row: %v", err)
	}
	if freshID == stale.ID {
		t.Fatalf("re-insert reused id %d; the fixture needs a fresh row", freshID)
	}
	// 5. The rescan's write finally lands, matched on the id it read.
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := updateLibraryCacheRow(ctx, tx, instanceID, stale.ID, movieItem(1, "Heat (reread)"), "2030-01-01 00:00:00"); err != nil {
		t.Fatalf("rescan write: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var rows int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM arr_library_cache WHERE arr_instance_id = ? AND external_item_id = 1`,
		instanceID).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want exactly 1 (the stale-key fallback duplicated the poller's row)", rows)
	}
	var (
		id    int64
		title string
	)
	if err := s.DB.QueryRowContext(ctx,
		`SELECT id, title FROM arr_library_cache WHERE arr_instance_id = ? AND external_item_id = 1`,
		instanceID).Scan(&id, &title); err != nil {
		t.Fatalf("read surviving row: %v", err)
	}
	if id != freshID {
		t.Errorf("surviving id = %d, want the poller's row %d", id, freshID)
	}
	if title != "Heat (reread)" {
		t.Errorf("title = %q, want the rescan's observation", title)
	}
}
