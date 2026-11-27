package store

import (
	"context"
	"database/sql"
	"io/fs"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
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

// TestSyncJobAnalysisSourcePersists covers migration 0007 and the job
// projection: both columns exist on a fresh database, a new row has no mode,
// FinishSyncJobAnalysis records what the job did, and a later finish overwrites
// it (the worker writes the row again after the mux).
func TestSyncJobAnalysisSourcePersists(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	// Forward migrations, not part of schema.sql's 0001 mirror.
	for _, column := range []string{"analysis_source", "analysis_fallback"} {
		var columns int
		if err := st.DB.QueryRow(
			`SELECT count(*) FROM pragma_table_info('sync_mux_jobs') WHERE name = ?`, column,
		).Scan(&columns); err != nil {
			t.Fatalf("inspect sync_mux_jobs.%s: %v", column, err)
		}
		if columns != 1 {
			t.Fatalf("sync_mux_jobs.%s columns = %d, want 1 (migration 0007)", column, columns)
		}
	}

	jobID := insertAnalysisJob(t, st, dir)
	rec, ok, err := st.SyncJobByID(ctx, jobID)
	if err != nil || !ok {
		t.Fatalf("SyncJobByID = %v/%v", ok, err)
	}
	if rec.AnalysisSource != "" || rec.AnalysisFallback != "" {
		t.Fatalf("fresh row analysis = %q/%q, want empty", rec.AnalysisSource, rec.AnalysisFallback)
	}

	if err := st.FinishSyncJobAnalysis(ctx, jobID, "completed", "same_lang_reference", 120, nil, "", "windows", ""); err != nil {
		t.Fatalf("FinishSyncJobAnalysis: %v", err)
	}
	rec, _, err = st.SyncJobByID(ctx, jobID)
	if err != nil {
		t.Fatalf("SyncJobByID after finish: %v", err)
	}
	if rec.AnalysisSource != "windows" || rec.AnalysisFallback != "" {
		t.Errorf("analysis = %q/%q, want windows/empty", rec.AnalysisSource, rec.AnalysisFallback)
	}

	// The worker finishes the row again after the mux; the mode must survive it.
	if err := st.FinishSyncJobAnalysis(ctx, jobID, "completed", "same_lang_reference", 120, nil, "", "full", "2-suspect-windows"); err != nil {
		t.Fatalf("FinishSyncJobAnalysis (worker): %v", err)
	}
	rec, _, err = st.SyncJobByID(ctx, jobID)
	if err != nil {
		t.Fatalf("SyncJobByID after worker finish: %v", err)
	}
	if rec.AnalysisSource != "full" || rec.AnalysisFallback != "2-suspect-windows" {
		t.Errorf("analysis = %q/%q, want full/2-suspect-windows", rec.AnalysisSource, rec.AnalysisFallback)
	}

	// An empty mode is stored as NULL, so a job without one reads the same as a
	// row that predates the column.
	if err := st.FinishSyncJobAnalysis(ctx, jobID, "completed", "same_lang_reference", 120, nil, "", "", ""); err != nil {
		t.Fatalf("FinishSyncJobAnalysis (cleared): %v", err)
	}
	rec, _, err = st.SyncJobByID(ctx, jobID)
	if err != nil {
		t.Fatalf("SyncJobByID after clear: %v", err)
	}
	if rec.AnalysisSource != "" || rec.AnalysisFallback != "" {
		t.Errorf("analysis = %q/%q, want empty after clear", rec.AnalysisSource, rec.AnalysisFallback)
	}
}

// TestSyncJobAnalysisSourceMigrationOnPopulatedDB applies 0007 to a database
// that stopped at 0006 and already holds a job: the ALTER TABLE runs over data,
// and the existing row simply has no analysis mode.
func TestSyncJobAnalysisSourceMigrationOnPopulatedDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, DBFile)
	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()

	// The schema an upgrade finds: everything up to 0006, with a row in it.
	if _, err := migrate(db, migrationsBefore(t, 7)); err != nil {
		t.Fatalf("migrate up to 0006: %v", err)
	}
	st := &Store{DB: db, dataDir: dir, dbPath: dbPath}
	jobID := insertAnalysisJob(t, st, dir)

	version, err := migrate(db, embeddedMigrations)
	if err != nil {
		t.Fatalf("migrate 0007 onto populated database: %v", err)
	}
	if version != 7 {
		t.Fatalf("schema version = %d, want 7", version)
	}

	rec, ok, err := st.SyncJobByID(ctx, jobID)
	if err != nil || !ok {
		t.Fatalf("SyncJobByID after migration = %v/%v", ok, err)
	}
	if rec.Status != "queued" || rec.AnalysisSource != "" || rec.AnalysisFallback != "" {
		t.Fatalf("existing row = status %q, analysis %q/%q; want queued with no mode",
			rec.Status, rec.AnalysisSource, rec.AnalysisFallback)
	}
}

// migrationsBefore is the embedded migration set below version: the schema an
// upgrade starts from.
func migrationsBefore(t *testing.T, version int) fs.FS {
	t.Helper()
	entries, err := fs.ReadDir(embeddedMigrations, "migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	out := fstest.MapFS{}
	for _, entry := range entries {
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			t.Fatalf("migration %q has no version prefix", entry.Name())
		}
		n, err := strconv.Atoi(prefix)
		if err != nil {
			t.Fatalf("migration %q version: %v", entry.Name(), err)
		}
		if n >= version {
			continue
		}
		name := path.Join("migrations", entry.Name())
		body, err := fs.ReadFile(embeddedMigrations, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		out[name] = &fstest.MapFile{Data: body}
	}
	return out
}

// insertAnalysisJob creates one queued job for the analysis-source tests.
func insertAnalysisJob(t *testing.T, st *Store, dir string) int64 {
	t.Helper()
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
	return jobID
}
