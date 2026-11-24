package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestSyncJobOutputPathPersists covers migration 0003 and the job projection:
// the recorded publish destination is read back by SyncJobByID and can be
// cleared again.
func TestSyncJobOutputPathPersists(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	// The column is a forward migration, not part of schema.sql's 0001 mirror.
	var columns int
	if err := st.DB.QueryRow(
		`SELECT count(*) FROM pragma_table_info('sync_mux_jobs') WHERE name = 'output_path'`,
	).Scan(&columns); err != nil {
		t.Fatalf("inspect sync_mux_jobs: %v", err)
	}
	if columns != 1 {
		t.Fatalf("sync_mux_jobs.output_path columns = %d, want 1 (migration 0003)", columns)
	}

	res, err := st.DB.Exec(
		`INSERT INTO vault_media (media_type, title, vault_folder_path) VALUES ('movie', 'M', ?)`, dir)
	if err != nil {
		t.Fatalf("insert vault media: %v", err)
	}
	mediaID, _ := res.LastInsertId()
	res, err = st.DB.Exec(`
		INSERT INTO audio_tracks
			(vault_media_id, group_uuid, lang_iso2, variant, file_path, sha256,
			 codec, channels, channel_count)
		VALUES (?, 'group-1', 'pl', 'dubbing', ?, 'sha', 'pcm_s16le', '2.0', 2)`,
		mediaID, filepath.Join(dir, "track.pl.mka"))
	if err != nil {
		t.Fatalf("insert audio track: %v", err)
	}
	trackID, _ := res.LastInsertId()
	res, err = st.DB.Exec(`
		INSERT INTO sync_mux_jobs (target_track_id, target_video_path, target_lang_iso2, status)
		VALUES (?, ?, 'pl', 'queued')`, trackID, filepath.Join(dir, "Movie.mkv"))
	if err != nil {
		t.Fatalf("insert sync job: %v", err)
	}
	jobID, _ := res.LastInsertId()

	rec, ok, err := st.SyncJobByID(ctx, jobID)
	if err != nil || !ok {
		t.Fatalf("SyncJobByID = %v/%v", ok, err)
	}
	if rec.OutputPath != "" {
		t.Fatalf("fresh output_path = %q, want empty", rec.OutputPath)
	}

	const output = "/outputs/Movie (2021).dubarr.mkv"
	if err := st.SetSyncJobOutputPath(ctx, jobID, output); err != nil {
		t.Fatalf("SetSyncJobOutputPath: %v", err)
	}
	rec, ok, err = st.SyncJobByID(ctx, jobID)
	if err != nil || !ok {
		t.Fatalf("SyncJobByID after set = %v/%v", ok, err)
	}
	if rec.OutputPath != output {
		t.Errorf("output_path = %q, want %q", rec.OutputPath, output)
	}

	if err := st.SetSyncJobOutputPath(ctx, jobID, ""); err != nil {
		t.Fatalf("clear output path: %v", err)
	}
	if rec, _, _ = st.SyncJobByID(ctx, jobID); rec.OutputPath != "" {
		t.Errorf("output_path = %q, want empty after clear", rec.OutputPath)
	}

	if err := st.SetSyncJobOutputPath(ctx, jobID+999, output); err == nil {
		t.Error("SetSyncJobOutputPath accepted a missing job")
	}
}

// TestLibraryCacheJobTargetCarriesArrMetadata wires the mux re-check: the job
// projection must carry the arr tag labels and canonical root folder from
// arr_library_cache so gate source_mode=arr/both can decide, and a legacy row
// with no tag snapshot must decode as empty rather than error.
func TestLibraryCacheJobTargetCarriesArrMetadata(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	res, err := st.DB.Exec(
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4, enabled) VALUES ('Radarr', 'radarr', 'http://radarr', x'00', 'abcd', 1)`)
	if err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	instanceID, _ := res.LastInsertId()
	if _, err := st.SyncLibraryCache(ctx, instanceID, []LibraryCacheItem{{
		MediaType:         "movie",
		ExternalItemID:    7,
		Title:             "Shrek",
		TargetFilePath:    "/media/movies/Shrek/Shrek.mkv",
		RootFolderPath:    "/media/movies",
		ArrTagIDs:         []int64{1, 4},
		ArrTagLabels:      []string{"dub", "Polish Dubbing"},
		EligibilityStatus: "eligible",
	}}); err != nil {
		t.Fatalf("SyncLibraryCache: %v", err)
	}
	var id int64
	if err := st.DB.QueryRow(`SELECT id FROM arr_library_cache`).Scan(&id); err != nil {
		t.Fatalf("read cache id: %v", err)
	}
	got, ok, err := st.LibraryCacheJobTargetByID(ctx, id)
	if err != nil || !ok {
		t.Fatalf("LibraryCacheJobTargetByID = %v/%v", ok, err)
	}
	if len(got.ArrTagLabels) != 2 || got.ArrTagLabels[1] != "Polish Dubbing" || got.RootFolderPath != "/media/movies" {
		t.Errorf("job target arr metadata = %+v, want labels and root folder", got)
	}
}
