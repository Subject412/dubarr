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
	// Later revisions (0008) ride along on the same database, so the expected
	// end version is the highest embedded one.
	if want := latestMigrationVersion(t); version != want {
		t.Fatalf("schema version = %d, want %d", version, want)
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

// latestMigrationVersion is the head of the embedded migration set, i.e. what
// any upgrade ends on.
func latestMigrationVersion(t *testing.T) int {
	t.Helper()
	all, err := loadMigrations(embeddedMigrations)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no embedded migrations")
	}
	return all[len(all)-1].Version
}

// insertArrInstance creates the Radarr row an arr_library_cache row hangs off.
func insertArrInstance(t *testing.T, st *Store) int64 {
	t.Helper()
	res, err := st.DB.Exec(
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4, enabled)
		 VALUES ('Radarr', 'radarr', 'http://radarr', x'00', 'abcd', 1)`)
	if err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// insertArrCacheRow creates one matched library row with an arr tag snapshot.
func insertArrCacheRow(t *testing.T, st *Store, instanceID, externalID int64, title, rootFolder, labelsJSON, targetPath string) int64 {
	t.Helper()
	res, err := st.DB.Exec(`
		INSERT INTO arr_library_cache
		    (arr_instance_id, media_type, external_item_id, title, root_folder_path,
		     arr_tag_labels_json, target_file_path, eligibility_status)
		VALUES (?, 'movie', ?, ?, ?, ?, ?, 'waiting_for_tags')`,
		instanceID, externalID, title, rootFolder, labelsJSON, targetPath)
	if err != nil {
		t.Fatalf("insert arr library cache: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// insertApprovalQueueRow creates the pending_approval import_queue row an approved job
// records as its origin.
func insertApprovalQueueRow(t *testing.T, st *Store, dir, filename string) int64 {
	t.Helper()
	res, err := st.DB.Exec(`
		INSERT INTO import_queue (source_file_path, filename, file_size, status)
		VALUES (?, ?, 1024, 'pending_approval')`,
		filepath.Join(dir, filename), filename)
	if err != nil {
		t.Fatalf("insert import queue row: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// insertVaultTrack archives one vault track, the donor a sync job muxes in.
func insertVaultTrack(t *testing.T, st *Store, dir, name string) int64 {
	t.Helper()
	res, err := st.DB.Exec(
		`INSERT INTO vault_media (media_type, title, vault_folder_path) VALUES ('movie', ?, ?)`,
		name, dir)
	if err != nil {
		t.Fatalf("insert vault media: %v", err)
	}
	mediaID, _ := res.LastInsertId()
	res, err = st.DB.Exec(`
		INSERT INTO audio_tracks
		    (vault_media_id, group_uuid, lang_iso2, variant, file_path, sha256,
		     codec, channels, channel_count)
		VALUES (?, ?, 'pl', 'dubbing', ?, 'sha', 'pcm_s16le', '2.0', 2)`,
		mediaID, "group-"+name, filepath.Join(dir, name+".pl.mka"))
	if err != nil {
		t.Fatalf("insert audio track: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// setJobStatus forces a job into a status the constructor cannot produce, so the
// guarded transitions can be exercised from every starting state.
func setJobStatus(t *testing.T, st *Store, id int64, status string) {
	t.Helper()
	if _, err := st.DB.Exec(
		`UPDATE sync_mux_jobs SET status = ? WHERE id = ?`, status, id); err != nil {
		t.Fatalf("set sync job %d status %q: %v", id, status, err)
	}
}

// mustJob reads a job or fails the test.
func mustJob(t *testing.T, st *Store, id int64) SyncJobRecord {
	t.Helper()
	rec, ok, err := st.SyncJobByID(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("SyncJobByID(%d) = %v/%v", id, ok, err)
	}
	return rec
}

// TestCreateSyncJobRecordsHeldIntent pins what approval writes (migration 0008):
// the state the intent is recorded in, the closed held reason, the persisted
// force override and the origin queue row. A forced job can never be held, and
// only queued/held may be created.
func TestCreateSyncJobRecordsHeldIntent(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	instanceID := insertArrInstance(t, st)
	targetPath := filepath.Join(dir, "Shrek (2001).mkv")
	cacheID := insertArrCacheRow(t, st, instanceID, 1, "Shrek", "/media/movies", `["dub"]`, targetPath)
	queueID := insertApprovalQueueRow(t, st, dir, "Shrek.PL.DUB.mkv")
	trackID := insertVaultTrack(t, st, dir, "shrek")

	held, err := st.CreateSyncJob(ctx, SyncJobCreate{
		VaultTrackID: trackID, TargetLibraryCacheID: cacheID,
		TargetVideoPath: targetPath, TargetLangISO2: "pl",
		Status: SyncJobHeld, HeldReason: HeldReasonWaitingForTags,
		OriginQueueID: &queueID,
	})
	if err != nil {
		t.Fatalf("CreateSyncJob(held): %v", err)
	}
	if held.Status != SyncJobHeld || held.HeldReason != HeldReasonWaitingForTags {
		t.Errorf("held job = status %q reason %q, want held/waiting_for_tags", held.Status, held.HeldReason)
	}
	if held.OriginQueueID != queueID {
		t.Errorf("origin_queue_id = %d, want %d", held.OriginQueueID, queueID)
	}
	if held.ForceIgnoreTagFilter {
		t.Error("force_ignore_tag_filter = true on a non-forced job")
	}

	queued, err := st.CreateSyncJob(ctx, SyncJobCreate{
		VaultTrackID: trackID, TargetLibraryCacheID: cacheID,
		TargetVideoPath: targetPath, TargetLangISO2: "pl",
		OriginQueueID: &queueID,
	})
	if err != nil {
		t.Fatalf("CreateSyncJob(queued): %v", err)
	}
	if queued.Status != SyncJobQueued || queued.HeldReason != "" {
		t.Errorf("default job = status %q reason %q, want queued with no reason", queued.Status, queued.HeldReason)
	}

	forced, err := st.CreateSyncJob(ctx, SyncJobCreate{
		VaultTrackID: trackID, TargetLibraryCacheID: cacheID,
		TargetVideoPath: targetPath, TargetLangISO2: "pl",
		ForceIgnoreTagFilter: true,
	})
	if err != nil {
		t.Fatalf("CreateSyncJob(forced): %v", err)
	}
	if forced.Status != SyncJobQueued || !forced.ForceIgnoreTagFilter {
		t.Errorf("forced job = status %q force %v, want queued/true",
			forced.Status, forced.ForceIgnoreTagFilter)
	}
	if forced.OriginQueueID != 0 {
		t.Errorf("manual job origin_queue_id = %d, want 0", forced.OriginQueueID)
	}

	// A forced job is the operator's override: it is inserted queued and cannot
	// be created waiting for a gate that was already bypassed.
	for _, tc := range []struct {
		name string
		in   SyncJobCreate
	}{
		{name: "held without a reason", in: SyncJobCreate{Status: SyncJobHeld}},
		{name: "held with an unknown reason", in: SyncJobCreate{Status: SyncJobHeld, HeldReason: "because"}},
		{name: "held with a cancel reason", in: SyncJobCreate{Status: SyncJobHeld, HeldReason: CancelReasonRejection}},
		{name: "forced but held", in: SyncJobCreate{Status: SyncJobHeld, HeldReason: HeldReasonWaitingForTags, ForceIgnoreTagFilter: true}},
		{name: "a status the intent cannot start in", in: SyncJobCreate{Status: SyncJobMuxing}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.VaultTrackID, tc.in.TargetLibraryCacheID = trackID, cacheID
			tc.in.TargetVideoPath, tc.in.TargetLangISO2 = targetPath, "pl"
			if _, err := st.CreateSyncJob(ctx, tc.in); err == nil {
				t.Fatal("CreateSyncJob accepted an invalid intent")
			}
		})
	}
}

// TestReleaseHeldJobsOnlyPromotesEligible is the projection: only the held job
// whose target passes now is promoted, the released row keeps no hold reason,
// the ineligible row keeps its reason, and a forced queued row is never touched.
func TestReleaseHeldJobsOnlyPromotesEligible(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	instanceID := insertArrInstance(t, st)
	trackID := insertVaultTrack(t, st, dir, "shrek")
	eligiblePath := filepath.Join(dir, "eligible.mkv")
	ineligiblePath := filepath.Join(dir, "waiting.mkv")
	forcedPath := filepath.Join(dir, "forced.mkv")

	nextExternalID := int64(0)
	newJob := func(target string, c SyncJobCreate) SyncJobRecord {
		nextExternalID++
		c.VaultTrackID, c.TargetLangISO2 = trackID, "pl"
		c.TargetLibraryCacheID = insertArrCacheRow(t, st, instanceID,
			nextExternalID, "Shrek", "/media/movies", `[]`, target)
		c.TargetVideoPath = target
		rec, err := st.CreateSyncJob(ctx, c)
		if err != nil {
			t.Fatalf("CreateSyncJob(%s): %v", target, err)
		}
		return rec
	}
	eligible := newJob(eligiblePath, SyncJobCreate{Status: SyncJobHeld, HeldReason: HeldReasonWaitingForTags})
	ineligible := newJob(ineligiblePath, SyncJobCreate{Status: SyncJobHeld, HeldReason: HeldReasonMissingTarget})
	forced := newJob(forcedPath, SyncJobCreate{ForceIgnoreTagFilter: true})

	calls := 0
	released, err := st.ReleaseHeldJobs(ctx, func(job HeldJob) bool {
		calls++
		// A forced row is invisible to the projection, not merely skipped.
		if job.ForceIgnoreTagFilter {
			t.Errorf("projection was asked about forced job %d", job.ID)
		}
		return job.TargetVideoPath == eligiblePath
	})
	if err != nil {
		t.Fatalf("ReleaseHeldJobs: %v", err)
	}
	if released != 1 {
		t.Fatalf("released = %d, want 1", released)
	}
	if calls != 2 {
		t.Errorf("projection calls = %d, want 2 (the two held rows)", calls)
	}

	if got := mustJob(t, st, eligible.ID); got.Status != SyncJobQueued || got.HeldReason != "" {
		t.Errorf("eligible job = status %q reason %q, want queued with no reason", got.Status, got.HeldReason)
	}
	if got := mustJob(t, st, ineligible.ID); got.Status != SyncJobHeld || got.HeldReason != HeldReasonMissingTarget {
		t.Errorf("ineligible job = status %q reason %q, want held/missing_target", got.Status, got.HeldReason)
	}
	if got := mustJob(t, st, forced.ID); got.Status != SyncJobQueued || !got.ForceIgnoreTagFilter {
		t.Errorf("forced job = status %q force %v, want queued/true", got.Status, got.ForceIgnoreTagFilter)
	}

	// Idempotent: nothing is held any more, so a second pass releases nothing.
	again, err := st.ReleaseHeldJobs(ctx, func(HeldJob) bool { return true })
	if err != nil {
		t.Fatalf("ReleaseHeldJobs (second pass): %v", err)
	}
	if again != 0 {
		t.Errorf("second pass released = %d, want 0", again)
	}

	if _, err := st.ReleaseHeldJobs(ctx, nil); err == nil {
		t.Error("ReleaseHeldJobs accepted a nil projection")
	}
}

// TestHeldJobsCarriesArrMetadata pins the single-query join the projection
// needs: the target path, the persisted override and reason, and the arr title,
// root folder and decoded tag labels. A job without a cache row reports
// HasArrRow=false instead of a zero-valued row.
func TestHeldJobsCarriesArrMetadata(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	instanceID := insertArrInstance(t, st)
	trackID := insertVaultTrack(t, st, dir, "shrek")
	withArr := filepath.Join(dir, "with-arr.mkv")
	cacheID := insertArrCacheRow(t, st, instanceID, 1, "Shrek", "/media/movies",
		`["dub","Polish Dubbing"]`, withArr)
	if _, err := st.CreateSyncJob(ctx, SyncJobCreate{
		VaultTrackID: trackID, TargetLibraryCacheID: cacheID,
		TargetVideoPath: withArr, TargetLangISO2: "pl",
		Status: SyncJobHeld, HeldReason: HeldReasonWaitingForTags,
	}); err != nil {
		t.Fatalf("CreateSyncJob(with arr): %v", err)
	}

	// No cache row: a row deleted out from under the job is the SET NULL case.
	noArr := filepath.Join(dir, "no-arr.mkv")
	if _, err := st.DB.Exec(`
		INSERT INTO sync_mux_jobs
		    (target_track_id, target_video_path, target_lang_iso2, status, held_reason)
		VALUES (?, ?, 'pl', 'held', 'missing_target')`, trackID, noArr); err != nil {
		t.Fatalf("insert cache-less held job: %v", err)
	}
	// A queued job is not part of the projection.
	if _, err := st.CreateSyncJob(ctx, SyncJobCreate{
		VaultTrackID: trackID, TargetLibraryCacheID: cacheID,
		TargetVideoPath: filepath.Join(dir, "queued.mkv"), TargetLangISO2: "pl",
	}); err != nil {
		t.Fatalf("CreateSyncJob(queued): %v", err)
	}

	held, err := st.HeldJobs(ctx)
	if err != nil {
		t.Fatalf("HeldJobs: %v", err)
	}
	if len(held) != 2 {
		t.Fatalf("HeldJobs = %d rows, want 2", len(held))
	}
	first, second := held[0], held[1]
	if first.TargetVideoPath != withArr || !first.HasArrRow || first.ArrTitle != "Shrek" ||
		first.ArrRootFolder != "/media/movies" {
		t.Errorf("arr-backed held job = %+v, want Shrek in /media/movies", first)
	}
	if len(first.ArrTags) != 2 || first.ArrTags[1] != "Polish Dubbing" {
		t.Errorf("arr tags = %v, want the decoded label snapshot", first.ArrTags)
	}
	if !first.TargetLibraryCacheID.Valid || first.TargetLibraryCacheID.Int64 != cacheID {
		t.Errorf("target_library_cache_id = %+v, want %d", first.TargetLibraryCacheID, cacheID)
	}
	if second.HasArrRow || second.ArrTitle != "" || len(second.ArrTags) != 0 {
		t.Errorf("cache-less held job = %+v, want no arr metadata", second)
	}
	if second.HeldReason != HeldReasonMissingTarget {
		t.Errorf("cache-less held job reason = %q, want missing_target", second.HeldReason)
	}
}

// TestHoldSyncJobGuardsTransitions is the guarded matrix: only a claimed job
// (analyzing or muxing) may be demoted to held, so a completed, failed or
// cancelled job can never be re-held by a stale pass.
func TestHoldSyncJobGuardsTransitions(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	instanceID := insertArrInstance(t, st)
	trackID := insertVaultTrack(t, st, dir, "shrek")
	cacheID := insertArrCacheRow(t, st, instanceID, 1, "Shrek", "/media/movies", `[]`,
		filepath.Join(dir, "Shrek.mkv"))

	newJob := func() SyncJobRecord {
		rec, err := st.CreateSyncJob(ctx, SyncJobCreate{
			VaultTrackID: trackID, TargetLibraryCacheID: cacheID,
			TargetVideoPath: filepath.Join(dir, "Shrek.mkv"), TargetLangISO2: "pl",
		})
		if err != nil {
			t.Fatalf("CreateSyncJob: %v", err)
		}
		return rec
	}

	for _, tc := range []struct {
		status string
		want   bool
	}{
		{SyncJobAnalyzing, true},
		{SyncJobMuxing, true},
		{SyncJobQueued, false},
		{SyncJobHeld, false},
		{SyncJobReadyForReview, false},
		{SyncJobCompleted, false},
		{SyncJobFailed, false},
		{SyncJobCancelled, false},
	} {
		t.Run(tc.status, func(t *testing.T) {
			job := newJob()
			setJobStatus(t, st, job.ID, tc.status)
			held, err := st.HoldSyncJob(ctx, job.ID, HeldReasonWaitingForTags)
			if err != nil {
				t.Fatalf("HoldSyncJob: %v", err)
			}
			if held != tc.want {
				t.Fatalf("HoldSyncJob from %q = %v, want %v", tc.status, held, tc.want)
			}
			got := mustJob(t, st, job.ID)
			if !tc.want {
				if got.Status != tc.status {
					t.Errorf("status = %q, want it unchanged at %q", got.Status, tc.status)
				}
				return
			}
			if got.Status != SyncJobHeld || got.HeldReason != HeldReasonWaitingForTags {
				t.Errorf("held job = status %q reason %q, want held/waiting_for_tags",
					got.Status, got.HeldReason)
			}
			// The claim-time re-check is one-way per attempt: a re-held job
			// waits for the next projection, it is not held twice.
			if again, err := st.HoldSyncJob(ctx, job.ID, HeldReasonMissingTarget); err != nil || again {
				t.Errorf("second HoldSyncJob = %v/%v, want false/nil", again, err)
			}
		})
	}

	if _, err := st.HoldSyncJob(ctx, newJob().ID, "because"); err == nil {
		t.Error("HoldSyncJob accepted free-text reason")
	}
	if _, err := st.HoldSyncJob(ctx, newJob().ID, CancelReasonRejection); err == nil {
		t.Error("HoldSyncJob accepted a cancel reason")
	}
	if _, err := st.HoldSyncJob(ctx, 999999, HeldReasonWaitingForTags); err != nil {
		t.Errorf("HoldSyncJob(missing job) = %v, want false/nil", err)
	}
}

// TestCancelSyncJobGuardsTransitions pins that cancellation is only ever a
// queued|held transition: a running job is left to the worker, and a terminal
// row stays terminal.
func TestCancelSyncJobGuardsTransitions(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	instanceID := insertArrInstance(t, st)
	trackID := insertVaultTrack(t, st, dir, "shrek")
	cacheID := insertArrCacheRow(t, st, instanceID, 1, "Shrek", "/media/movies", `[]`,
		filepath.Join(dir, "Shrek.mkv"))

	newJob := func(status string) SyncJobRecord {
		rec, err := st.CreateSyncJob(ctx, SyncJobCreate{
			VaultTrackID: trackID, TargetLibraryCacheID: cacheID,
			TargetVideoPath: filepath.Join(dir, "Shrek.mkv"), TargetLangISO2: "pl",
		})
		if err != nil {
			t.Fatalf("CreateSyncJob: %v", err)
		}
		setJobStatus(t, st, rec.ID, status)
		return rec
	}

	for _, tc := range []struct {
		status string
		want   bool
	}{
		{SyncJobQueued, true},
		{SyncJobHeld, true},
		{SyncJobAnalyzing, false},
		{SyncJobReadyForReview, false},
		{SyncJobMuxing, false},
		{SyncJobCompleted, false},
		{SyncJobFailed, false},
		{SyncJobCancelled, false},
	} {
		t.Run(tc.status, func(t *testing.T) {
			job := newJob(tc.status)
			cancelled, err := st.CancelSyncJob(ctx, job.ID, CancelReasonRejection)
			if err != nil {
				t.Fatalf("CancelSyncJob: %v", err)
			}
			if cancelled != tc.want {
				t.Fatalf("CancelSyncJob from %q = %v, want %v", tc.status, cancelled, tc.want)
			}
			got := mustJob(t, st, job.ID)
			if !tc.want {
				if got.Status != tc.status {
					t.Errorf("status = %q, want it unchanged at %q", got.Status, tc.status)
				}
				return
			}
			if got.Status != SyncJobCancelled || got.HeldReason != CancelReasonRejection {
				t.Errorf("cancelled job = status %q reason %q, want cancelled/cancelled_by_rejection",
					got.Status, got.HeldReason)
			}
		})
	}

	if _, err := st.CancelSyncJob(ctx, newJob(SyncJobQueued).ID, "because"); err == nil {
		t.Error("CancelSyncJob accepted free-text reason")
	}
}

// TestCancelSyncJobsByOriginLeavesCompletedRows is rejection's half of the
// slice: every queued|held job created by the queue row is cancelled, a
// finished or running job is left alone, and another row's jobs never move.
func TestCancelSyncJobsByOriginLeavesCompletedRows(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	instanceID := insertArrInstance(t, st)
	trackID := insertVaultTrack(t, st, dir, "shrek")
	cacheID := insertArrCacheRow(t, st, instanceID, 1, "Shrek", "/media/movies", `[]`,
		filepath.Join(dir, "Shrek.mkv"))
	origin := insertApprovalQueueRow(t, st, dir, "Shrek.PL.DUB.mkv")
	otherOrigin := insertApprovalQueueRow(t, st, dir, "Other.PL.DUB.mkv")

	newJob := func(queueID int64, status string) SyncJobRecord {
		rec, err := st.CreateSyncJob(ctx, SyncJobCreate{
			VaultTrackID: trackID, TargetLibraryCacheID: cacheID,
			TargetVideoPath: filepath.Join(dir, "Shrek.mkv"), TargetLangISO2: "pl",
			OriginQueueID: &queueID,
		})
		if err != nil {
			t.Fatalf("CreateSyncJob: %v", err)
		}
		setJobStatus(t, st, rec.ID, status)
		return rec
	}
	held := newJob(origin, SyncJobHeld)
	queued := newJob(origin, SyncJobQueued)
	completed := newJob(origin, SyncJobCompleted)
	analyzing := newJob(origin, SyncJobAnalyzing)
	unrelated := newJob(otherOrigin, SyncJobHeld)

	n, err := st.CancelSyncJobsByOrigin(ctx, origin, CancelReasonRejection)
	if err != nil {
		t.Fatalf("CancelSyncJobsByOrigin: %v", err)
	}
	if n != 2 {
		t.Fatalf("cancelled = %d, want 2 (held + queued)", n)
	}
	for _, id := range []int64{held.ID, queued.ID} {
		got := mustJob(t, st, id)
		if got.Status != SyncJobCancelled || got.HeldReason != CancelReasonRejection {
			t.Errorf("job %d = status %q reason %q, want cancelled/cancelled_by_rejection",
				id, got.Status, got.HeldReason)
		}
	}
	if got := mustJob(t, st, completed.ID); got.Status != SyncJobCompleted {
		t.Errorf("completed job = %q, want completed", got.Status)
	}
	if got := mustJob(t, st, analyzing.ID); got.Status != SyncJobAnalyzing {
		t.Errorf("running job = %q, want analyzing (left to the worker)", got.Status)
	}
	if got := mustJob(t, st, unrelated.ID); got.Status != SyncJobHeld {
		t.Errorf("other origin job = %q, want held", got.Status)
	}

	// A second rejection pass finds nothing left to cancel.
	if again, err := st.CancelSyncJobsByOrigin(ctx, origin, CancelReasonRejection); err != nil || again != 0 {
		t.Errorf("second pass = %d/%v, want 0/nil", again, err)
	}
	if _, err := st.CancelSyncJobsByOrigin(ctx, 0, CancelReasonRejection); err == nil {
		t.Error("CancelSyncJobsByOrigin accepted a zero queue id")
	}
}

// TestMigration0008RebuildsSyncMuxJobsPreservingRows upgrades a populated
// pre-0008 database: the rebuild keeps every row, id and value (including the
// columns added by 0003 and 0007), defaults the three new columns, and accepts
// the two new statuses.
func TestMigration0008RebuildsSyncMuxJobsPreservingRows(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, DBFile)
	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()

	if _, err := migrate(db, migrationsBefore(t, 8)); err != nil {
		t.Fatalf("migrate up to 0007: %v", err)
	}
	st := &Store{DB: db, dataDir: dir, dbPath: dbPath}
	trackID := insertVaultTrack(t, st, dir, "shrek")

	// One row in every status the pre-0008 CHECK allows, each carrying the
	// forward-migration columns so a lost column shows up as a lost value.
	oldStatuses := []string{
		SyncJobQueued, SyncJobAnalyzing, SyncJobReadyForReview,
		SyncJobMuxing, SyncJobCompleted, SyncJobFailed,
	}
	ids := make(map[string]int64, len(oldStatuses))
	for i, status := range oldStatuses {
		res, err := st.DB.Exec(`
			INSERT INTO sync_mux_jobs
			    (target_track_id, target_video_path, target_lang_iso2, status,
			     work_dir_path, manual_offset_ms, output_path,
			     analysis_source, analysis_fallback)
			VALUES (?, ?, 'pl', ?, ?, ?, ?, 'windows', '')`,
			trackID, filepath.Join(dir, status+".mkv"), status,
			filepath.Join(dir, "work-"+status), i*10, filepath.Join(dir, "out-"+status))
		if err != nil {
			t.Fatalf("insert %s job: %v", status, err)
		}
		ids[status], _ = res.LastInsertId()
	}

	version, err := migrate(db, embeddedMigrations)
	if err != nil {
		t.Fatalf("migrate 0008 onto populated database: %v", err)
	}
	if want := latestMigrationVersion(t); version != want {
		t.Fatalf("schema version = %d, want %d", version, want)
	}

	for _, column := range []string{"force_ignore_tag_filter", "held_reason", "origin_queue_id"} {
		if !columnExists(t, db, "sync_mux_jobs", column) {
			t.Errorf("sync_mux_jobs.%s missing after 0008", column)
		}
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sync_mux_jobs`).Scan(&count); err != nil {
		t.Fatalf("count sync jobs: %v", err)
	}
	if count != len(oldStatuses) {
		t.Fatalf("sync_mux_jobs rows = %d, want %d", count, len(oldStatuses))
	}

	for _, status := range oldStatuses {
		got := mustJob(t, st, ids[status])
		if got.Status != status {
			t.Errorf("job %d status = %q, want %q", ids[status], got.Status, status)
		}
		if got.OutputPath != filepath.Join(dir, "out-"+status) {
			t.Errorf("job %d output_path = %q, want it preserved", ids[status], got.OutputPath)
		}
		if got.WorkDirPath != filepath.Join(dir, "work-"+status) {
			t.Errorf("job %d work_dir_path = %q, want it preserved", ids[status], got.WorkDirPath)
		}
		if got.AnalysisSource != "windows" {
			t.Errorf("job %d analysis_source = %q, want windows", ids[status], got.AnalysisSource)
		}
		if got.ForceIgnoreTagFilter || got.HeldReason != "" || got.OriginQueueID != 0 {
			t.Errorf("job %d new columns = force %v reason %q origin %d, want 0/\"\"/0",
				ids[status], got.ForceIgnoreTagFilter, got.HeldReason, got.OriginQueueID)
		}
	}

	// The new CHECK accepts held and cancelled, and still rejects a typo.
	if _, err := db.Exec(
		`UPDATE sync_mux_jobs SET status = 'held', held_reason = 'waiting_for_tags' WHERE id = ?`,
		ids[SyncJobQueued]); err != nil {
		t.Fatalf("0008 CHECK rejected held: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE sync_mux_jobs SET status = 'cancelled' WHERE id = ?`,
		ids[SyncJobAnalyzing]); err != nil {
		t.Fatalf("0008 CHECK rejected cancelled: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE sync_mux_jobs SET status = 'parked' WHERE id = ?`,
		ids[SyncJobFailed]); err == nil {
		t.Error("0008 CHECK accepted an unknown status")
	}

	// The four indexes the pre-0008 table had three of are rebuilt.
	for _, index := range []string{
		"idx_sync_jobs_status", "idx_sync_jobs_target_track",
		"idx_sync_jobs_target_cache", "idx_sync_jobs_origin_queue",
	} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`,
			index).Scan(&n); err != nil {
			t.Fatalf("inspect index %s: %v", index, err)
		}
		if n != 1 {
			t.Errorf("index %s missing after 0008", index)
		}
	}

	// Re-opening an up-to-date database applies nothing (the restart test's
	// premise: the hold is state, not an in-memory timer).
	if again, err := migrate(db, embeddedMigrations); err != nil || again != version {
		t.Errorf("second migrate pass = %d/%v, want %d/nil", again, err, version)
	}
	if got := mustJob(t, st, ids[SyncJobQueued]); got.Status != SyncJobHeld {
		t.Errorf("held row after re-migrate = %q, want held", got.Status)
	}
}
