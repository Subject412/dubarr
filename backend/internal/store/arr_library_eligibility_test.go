package store

import (
	"context"
	"reflect"
	"testing"
)

// TestArrEligibilityColumnsExist covers migration 0004: the arr tag snapshot and
// the canonical root-folder path live on arr_library_cache.
func TestArrEligibilityColumnsExist(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	want := map[string]bool{
		"arr_tag_ids_json":    false,
		"arr_tag_labels_json": false,
		"root_folder_path":    false,
	}
	rows, err := s.DB.Query(`PRAGMA table_info(arr_library_cache)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid     int
			name    string
			typ     string
			notNull int
			dflt    any
			pk      int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info: %v", err)
	}
	for name, found := range want {
		if !found {
			t.Errorf("arr_library_cache is missing column %q (migration 0004)", name)
		}
	}
}

// TestSyncLibraryCachePersistsArrMetadata pins the write/read round trip: tag
// ids, labels and the canonical root-folder path survive the sync, and an
// unchanged pass sees identical metadata (so ids stay stable).
func TestSyncLibraryCachePersistsArrMetadata(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	instanceID := syncFixture(t, s, ctx)

	item := movieItem(1, "Shrek")
	item.ArrTagIDs = []int64{4, 1}
	item.ArrTagLabels = []string{"Polish Dubbing", "dub"}
	item.RootFolderPath = "/media/movies"

	if _, err := s.SyncLibraryCache(ctx, instanceID, []LibraryCacheItem{item}); err != nil {
		t.Fatalf("SyncLibraryCache (insert): %v", err)
	}

	var (
		idsJSON    string
		labelsJSON string
		root       string
	)
	if err := s.DB.QueryRow(
		`SELECT arr_tag_ids_json, arr_tag_labels_json, COALESCE(root_folder_path, '')
		 FROM arr_library_cache WHERE external_item_id = 1`).
		Scan(&idsJSON, &labelsJSON, &root); err != nil {
		t.Fatalf("read stored metadata: %v", err)
	}
	if idsJSON != "[1,4]" || labelsJSON != `["dub","Polish Dubbing"]` || root != "/media/movies" {
		t.Errorf("stored metadata = %q %q %q", idsJSON, labelsJSON, root)
	}

	// Second pass reads the stored columns back and must see no change.
	stats, err := s.SyncLibraryCache(ctx, instanceID, []LibraryCacheItem{item})
	if err != nil {
		t.Fatalf("SyncLibraryCache (unchanged): %v", err)
	}
	if stats.Unchanged != 1 || stats.Updated != 0 {
		t.Fatalf("second pass stats = %+v, want 1 unchanged", stats)
	}
}

// TestSyncLibraryCacheReplacesArrTagSnapshot pins "removed tags disappear": the
// new snapshot replaces the old one instead of appending to it, and a cleared
// snapshot reaches the row.
func TestSyncLibraryCacheReplacesArrTagSnapshot(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	instanceID := syncFixture(t, s, ctx)

	twoTags := movieItem(1, "Shrek")
	twoTags.ArrTagIDs = []int64{1, 4}
	twoTags.ArrTagLabels = []string{"dub", "Polish Dubbing"}

	if _, err := s.SyncLibraryCache(ctx, instanceID, []LibraryCacheItem{twoTags}); err != nil {
		t.Fatalf("SyncLibraryCache (two tags): %v", err)
	}

	oneTag := movieItem(1, "Shrek")
	oneTag.ArrTagIDs = []int64{4}
	oneTag.ArrTagLabels = []string{"Polish Dubbing"}
	stats, err := s.SyncLibraryCache(ctx, instanceID, []LibraryCacheItem{oneTag})
	if err != nil {
		t.Fatalf("SyncLibraryCache (one tag): %v", err)
	}
	if stats.Updated != 1 {
		t.Fatalf("stats = %+v, want 1 updated", stats)
	}

	rows, err := s.libraryCacheRows(ctx, instanceID)
	if err != nil {
		t.Fatalf("libraryCacheRows: %v", err)
	}
	got := rows[libraryKey{mediaType: "movie", externalID: 1}]
	if !reflect.DeepEqual(got.ArrTagIDs, []int64{4}) || !reflect.DeepEqual(got.ArrTagLabels, []string{"Polish Dubbing"}) {
		t.Errorf("tag snapshot = %v %v, want only the surviving tag", got.ArrTagIDs, got.ArrTagLabels)
	}

	cleared := movieItem(1, "Shrek")
	stats, err = s.SyncLibraryCache(ctx, instanceID, []LibraryCacheItem{cleared})
	if err != nil {
		t.Fatalf("SyncLibraryCache (cleared): %v", err)
	}
	if stats.Updated != 1 {
		t.Fatalf("stats = %+v, want 1 updated after tags were removed", stats)
	}
	rows, err = s.libraryCacheRows(ctx, instanceID)
	if err != nil {
		t.Fatalf("libraryCacheRows (cleared): %v", err)
	}
	got = rows[libraryKey{mediaType: "movie", externalID: 1}]
	if len(got.ArrTagIDs) != 0 || len(got.ArrTagLabels) != 0 {
		t.Errorf("tag snapshot = %v %v, want empty", got.ArrTagIDs, got.ArrTagLabels)
	}
}

// TestLegacyLibraryRowDecodesEmptyArrMetadata simulates a row written before
// migration 0004: it must decode with empty metadata, not crash, and an empty
// snapshot must be considered unchanged.
func TestLegacyLibraryRowDecodesEmptyArrMetadata(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	instanceID := syncFixture(t, s, ctx)

	// Pre-0004 insert: the new columns are simply not mentioned, exactly like
	// rows that existed before the migration ran (they receive the defaults).
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, tmdb_id, title,
		  original_language_iso2, year, media_folder_path, target_file_path,
		  matches_target_tags, eligibility_status)
		 VALUES (?, 'movie', 1, 101, 'Shrek', 'en', 2001, '/media/movies/Shrek',
		         '/media/movies/Shrek/Shrek [ArcA310].mkv', 1, 'eligible')`,
		instanceID); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	rows, err := s.libraryCacheRows(ctx, instanceID)
	if err != nil {
		t.Fatalf("libraryCacheRows: %v", err)
	}
	legacy := rows[libraryKey{mediaType: "movie", externalID: 1}]
	if legacy.ArrTagIDs == nil || legacy.ArrTagLabels == nil {
		t.Errorf("legacy metadata = %v/%v, want empty non-nil slices", legacy.ArrTagIDs, legacy.ArrTagLabels)
	}
	if len(legacy.ArrTagIDs) != 0 || len(legacy.ArrTagLabels) != 0 || legacy.RootFolderPath != "" {
		t.Errorf("legacy metadata = %v/%v/%q, want empty", legacy.ArrTagIDs, legacy.ArrTagLabels, legacy.RootFolderPath)
	}

	// The same item with no arr metadata is a no-op pass, so the migration does
	// not rewrite every legacy row once.
	stats, err := s.SyncLibraryCache(ctx, instanceID, []LibraryCacheItem{movieItem(1, "Shrek")})
	if err != nil {
		t.Fatalf("SyncLibraryCache: %v", err)
	}
	if stats.Unchanged != 1 || stats.Updated != 0 {
		t.Fatalf("stats = %+v, want 1 unchanged legacy row", stats)
	}
}
