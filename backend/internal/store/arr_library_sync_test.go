package store

import (
	"context"
	"errors"
	"testing"
)

// syncFixture seeds one arr instance and returns its id.
func syncFixture(t *testing.T, s *Store, ctx context.Context) int64 {
	t.Helper()
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		 VALUES ('main', 'radarr', 'http://radarr.invalid', x'00', '0000')`)
	if err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("instance id: %v", err)
	}
	return id
}

func movieItem(externalID int64, title string) LibraryCacheItem {
	return LibraryCacheItem{
		MediaType:            "movie",
		ExternalItemID:       externalID,
		TMDBID:               100 + externalID,
		Title:                title,
		OriginalLanguageISO2: "en",
		Year:                 2001,
		MediaFolderPath:      "/media/movies/" + title,
		TargetFilePath:       "/media/movies/" + title + "/" + title + " [ArcA310].mkv",
		MatchesTargetTags:    true,
		EligibilityStatus:    "eligible",
	}
}

func episodeItem(externalID, seriesID int64, season, episode int) LibraryCacheItem {
	return LibraryCacheItem{
		MediaType:            "episode",
		ExternalItemID:       externalID,
		SeriesID:             seriesID,
		TVDBID:               200 + seriesID,
		Title:                "Show",
		OriginalLanguageISO2: "en",
		Year:                 2015,
		SeasonNumber:         &season,
		EpisodeNumber:        &episode,
		EpisodeTitle:         "Pilot",
		MediaFolderPath:      "/media/tv/Show",
		TargetFilePath:       "/media/tv/Show/S01E01.mkv",
		MatchesTargetTags:    false,
		EligibilityStatus:    "waiting_for_tags",
	}
}

// TestSyncLibraryCacheDiffUpsert pins the M6-1 diff semantics: new rows are
// inserted, changed rows are updated in place (ids preserved), unchanged rows
// are left alone and vanished rows are deleted.
func TestSyncLibraryCacheDiffUpsert(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	instanceID := syncFixture(t, s, ctx)

	first := []LibraryCacheItem{
		movieItem(1, "Shrek"),
		movieItem(2, "Minions"),
		episodeItem(10, 3, 1, 1),
	}
	stats, err := s.SyncLibraryCache(ctx, instanceID, first)
	if err != nil {
		t.Fatalf("SyncLibraryCache (insert): %v", err)
	}
	if stats.Inserted != 3 || stats.Updated != 0 || stats.Unchanged != 0 || stats.Deleted != 0 {
		t.Fatalf("first pass stats = %+v, want 3 inserted", stats)
	}

	ids := map[string]int64{}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, media_type, external_item_id FROM arr_library_cache ORDER BY id`)
	if err != nil {
		t.Fatalf("select rows: %v", err)
	}
	for rows.Next() {
		var (
			id         int64
			mediaType  string
			externalID int64
		)
		if err := rows.Scan(&id, &mediaType, &externalID); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		ids[keyOf(mediaType, externalID)] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate rows: %v", err)
	}
	_ = rows.Close()

	// Second pass: one unchanged, one changed (title/language/tags), one new,
	// one vanished (the episode disappears from the snapshot).
	shrek := movieItem(1, "Shrek")
	changed := movieItem(2, "Minions")
	changed.OriginalLanguageISO2 = "ja"
	changed.Year = 2015
	changed.TargetFilePath = "/media/movies/Minions/Minions.mkv"
	changed.MatchesTargetTags = false
	changed.EligibilityStatus = "waiting_for_tags"
	second := []LibraryCacheItem{shrek, changed, movieItem(4, "Arrival")}

	stats, err = s.SyncLibraryCache(ctx, instanceID, second)
	if err != nil {
		t.Fatalf("SyncLibraryCache (diff): %v", err)
	}
	if stats.Inserted != 1 || stats.Updated != 1 || stats.Unchanged != 1 || stats.Deleted != 1 {
		t.Fatalf("second pass stats = %+v, want 1 inserted / 1 updated / 1 unchanged / 1 deleted", stats)
	}

	var (
		movieID     int64
		title       string
		lang        string
		year        int
		target      string
		matches     bool
		eligibility string
	)
	if err := s.DB.QueryRowContext(ctx,
		`SELECT id, title, original_language_iso2, year, target_file_path,
		        matches_target_tags, eligibility_status
		 FROM arr_library_cache WHERE media_type = 'movie' AND external_item_id = 2`).
		Scan(&movieID, &title, &lang, &year, &target, &matches, &eligibility); err != nil {
		t.Fatalf("select changed row: %v", err)
	}
	if title != "Minions" || lang != "ja" || year != 2015 ||
		target != "/media/movies/Minions/Minions.mkv" || matches || eligibility != "waiting_for_tags" {
		t.Fatalf("changed row = %q/%q/%d/%q/%v/%q", title, lang, year, target, matches, eligibility)
	}

	var shrekID int64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT id FROM arr_library_cache WHERE media_type = 'movie' AND external_item_id = 1`).
		Scan(&shrekID); err != nil {
		t.Fatalf("select unchanged row: %v", err)
	}
	if shrekID != ids[keyOf("movie", 1)] {
		t.Errorf("unchanged row id = %d, want %d (ids must be preserved)", shrekID, ids[keyOf("movie", 1)])
	}

	var episodeCount int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM arr_library_cache WHERE media_type = 'episode'`).Scan(&episodeCount); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if episodeCount != 0 {
		t.Errorf("vanished episode rows = %d, want 0", episodeCount)
	}
}

// TestSyncLibraryCacheDeletedRowUnlinksManualMatch documents the deletion
// choice: vanishing arr items are deleted, and the FK (ON DELETE SET NULL) only
// unlinks the manual match instead of forbidding it.
func TestSyncLibraryCacheDeletedRowUnlinksManualMatch(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	instanceID := syncFixture(t, s, ctx)

	if _, err := s.SyncLibraryCache(ctx, instanceID, []LibraryCacheItem{movieItem(1, "Shrek")}); err != nil {
		t.Fatalf("SyncLibraryCache: %v", err)
	}
	var cacheID int64
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM arr_library_cache`).Scan(&cacheID); err != nil {
		t.Fatalf("select cache id: %v", err)
	}
	if _, err := s.RecordImportCandidate(ctx, "/watch/donor.mkv", "donor.mkv", 1); err != nil {
		t.Fatalf("record candidate: %v", err)
	}
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE import_queue SET matched_arr_instance_id = ?, matched_library_cache_id = ?
		 WHERE source_file_path = '/watch/donor.mkv'`, instanceID, cacheID); err != nil {
		t.Fatalf("link candidate: %v", err)
	}

	if _, err := s.SyncLibraryCache(ctx, instanceID, nil); err != nil {
		t.Fatalf("SyncLibraryCache (empty snapshot): %v", err)
	}

	var stillLinked bool
	if err := s.DB.QueryRowContext(ctx,
		`SELECT matched_library_cache_id IS NOT NULL FROM import_queue
		 WHERE source_file_path = '/watch/donor.mkv'`).Scan(&stillLinked); err != nil {
		t.Fatalf("read candidate link: %v", err)
	}
	if stillLinked {
		t.Error("candidate still links a deleted library row; ON DELETE SET NULL must unlink it")
	}
}

// TestSyncLibraryCacheScopedToInstance proves one instance's sync never touches
// rows cached for another instance.
func TestSyncLibraryCacheScopedToInstance(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	first := syncFixture(t, s, ctx)
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		 VALUES ('tv', 'sonarr', 'http://sonarr.invalid', x'00', '0000')`)
	if err != nil {
		t.Fatalf("insert second instance: %v", err)
	}
	second, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("second id: %v", err)
	}

	if _, err := s.SyncLibraryCache(ctx, first, []LibraryCacheItem{movieItem(1, "Shrek")}); err != nil {
		t.Fatalf("sync first: %v", err)
	}
	if _, err := s.SyncLibraryCache(ctx, second, []LibraryCacheItem{episodeItem(5, 3, 1, 1)}); err != nil {
		t.Fatalf("sync second: %v", err)
	}
	// Resyncing the first instance with an empty snapshot must not delete the
	// second instance's episode.
	if _, err := s.SyncLibraryCache(ctx, first, nil); err != nil {
		t.Fatalf("sync first (empty): %v", err)
	}
	var count int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM arr_library_cache WHERE arr_instance_id = ?`, second).Scan(&count); err != nil {
		t.Fatalf("count second rows: %v", err)
	}
	if count != 1 {
		t.Errorf("second instance rows = %d, want 1", count)
	}
}

func TestArrInstanceByKindAndSave(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if _, ok, err := s.ArrInstanceByKind(ctx, "radarr"); err != nil || ok {
		t.Fatalf("ArrInstanceByKind(absent) = ok %v, err %v", ok, err)
	}

	id, err := s.SaveArrInstance(ctx, ArrInstanceSyncRow{
		Name:        "Radarr",
		Kind:        "radarr",
		BaseURL:     "http://radarr:7878",
		APIKeyEnc:   []byte{1, 2, 3},
		APIKeyLast4: "1234",
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("SaveArrInstance (insert): %v", err)
	}

	row, ok, err := s.ArrInstanceByKind(ctx, "radarr")
	if err != nil || !ok {
		t.Fatalf("ArrInstanceByKind = ok %v, err %v", ok, err)
	}
	if row.ID != id || row.BaseURL != "http://radarr:7878" || !row.Enabled || row.APIKeyLast4 != "1234" {
		t.Fatalf("row = %+v", row)
	}

	row.BaseURL = "http://radarr.local:7878"
	row.Enabled = false
	updatedID, err := s.SaveArrInstance(ctx, row)
	if err != nil {
		t.Fatalf("SaveArrInstance (update): %v", err)
	}
	if updatedID != id {
		t.Errorf("update id = %d, want %d", updatedID, id)
	}
	var name, baseURL string
	var enabled bool
	if err := s.DB.QueryRowContext(ctx,
		`SELECT name, base_url, enabled FROM arr_instances WHERE id = ?`, id).
		Scan(&name, &baseURL, &enabled); err != nil {
		t.Fatalf("read updated row: %v", err)
	}
	if baseURL != "http://radarr.local:7878" || enabled {
		t.Errorf("updated row = %q/%v", baseURL, enabled)
	}
	if name != "Radarr" {
		t.Errorf("name = %q, want Radarr", name)
	}

	if err := s.TouchArrInstanceSync(ctx, id); err != nil {
		t.Fatalf("TouchArrInstanceSync: %v", err)
	}
	var lastSync *string
	if err := s.DB.QueryRowContext(ctx,
		`SELECT last_sync_at FROM arr_instances WHERE id = ?`, id).Scan(&lastSync); err != nil {
		t.Fatalf("read last_sync_at: %v", err)
	}
	if lastSync == nil || *lastSync == "" {
		t.Error("last_sync_at not recorded")
	}
}

func TestSyncLibraryCacheRejectsUnknownMediaType(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	instanceID := syncFixture(t, s, ctx)

	bad := movieItem(1, "Shrek")
	bad.MediaType = "series"
	if _, err := s.SyncLibraryCache(ctx, instanceID, []LibraryCacheItem{bad}); err == nil {
		t.Fatal("SyncLibraryCache accepted media_type series")
	} else if !errors.Is(err, ErrInvalidLibraryItem) {
		t.Fatalf("error = %v, want ErrInvalidLibraryItem", err)
	}
}

// keyOf builds the identity key the sync matches on.
func keyOf(mediaType string, externalID int64) string {
	return mediaType + "|" + itoa(externalID)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
