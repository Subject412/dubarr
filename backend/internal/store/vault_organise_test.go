package store

import (
	"context"
	"errors"
	"testing"
)

// organiseFixture seeds one series with two episode tracks and one movie track,
// all inside the given folders, plus the arr mirror row that names S02E03.
type organiseFixture struct {
	store      *Store
	seriesID   int64
	movieID    int64
	episode1   VaultTrackRecord
	episode2   VaultTrackRecord
	movieTrack VaultTrackRecord
}

func seedOrganiseFixture(t *testing.T) *organiseFixture {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	seriesID, err := st.EnsureVaultMedia(ctx, VaultMediaRecord{
		MediaType: "series", Title: "The Office", Year: 2005, TVDBID: 42,
		FolderPath: "/vault/The Office (2005)",
	})
	if err != nil {
		t.Fatalf("EnsureVaultMedia(series): %v", err)
	}
	movieID, err := st.EnsureVaultMedia(ctx, VaultMediaRecord{
		MediaType: "movie", Title: "Spirited Away", Year: 2001, TMDBID: 129,
		FolderPath: "/vault/Spirited Away (2001)",
	})
	if err != nil {
		t.Fatalf("EnsureVaultMedia(movie): %v", err)
	}

	season, episode := 2, 3
	episode1 := VaultTrackRecord{
		VaultMediaID: seriesID, SeasonNumber: &season, EpisodeNumber: &episode,
		GroupUUID: "g1", LangISO2: "pl", Variant: "lektor",
		FilePath: "/vault/The Office (2005)/ep3.pl.mka", SHA256: "aa",
		Codec: "eac3", Channels: "2.0", ChannelCount: 2,
	}
	episode2 := VaultTrackRecord{
		VaultMediaID: seriesID, SeasonNumber: &season, EpisodeNumber: &episode,
		GroupUUID: "g2", LangISO2: "pl", Variant: "dubbing",
		FilePath: "/vault/The Office (2005)/ep3-dub.pl.mka", SHA256: "bb",
		Codec: "ac3", Channels: "5.1", ChannelCount: 6,
	}
	movieTrack := VaultTrackRecord{
		VaultMediaID: movieID, GroupUUID: "g3", LangISO2: "pl", Variant: "dubbing",
		FilePath: "/vault/Spirited Away (2001)/old.pl.mka", SHA256: "cc",
		Codec: "eac3", Channels: "5.1", ChannelCount: 6,
	}
	for _, rec := range []*VaultTrackRecord{&episode1, &episode2, &movieTrack} {
		id, err := st.RecordVaultTrack(ctx, *rec)
		if err != nil {
			t.Fatalf("RecordVaultTrack(%s): %v", rec.FilePath, err)
		}
		rec.ID = id
	}

	res, err := st.DB.ExecContext(ctx,
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4, enabled)
		 VALUES ('sonarr', 'sonarr', 'http://sonarr:8989', 'enc', '0000', 1)`)
	if err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	instanceID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("instance id: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, series_id, tvdb_id, title,
		  year, season_number, episode_number, episode_title, eligibility_status)
		 VALUES (?, 'episode', 900, 42, 42, 'The Office', 2005, 2, 3, 'Office Olympics', 'eligible')`,
		instanceID); err != nil {
		t.Fatalf("insert episode cache row: %v", err)
	}

	return &organiseFixture{
		store: st, seriesID: seriesID, movieID: movieID,
		episode1: episode1, episode2: episode2, movieTrack: movieTrack,
	}
}

func TestVaultTrackPlanRowsJoinsEpisodeTitle(t *testing.T) {
	f := seedOrganiseFixture(t)
	rows, err := f.store.VaultTrackPlanRows(context.Background(), "all", nil)
	if err != nil {
		t.Fatalf("VaultTrackPlanRows: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	byID := map[int64]VaultTrackPlanRow{}
	for _, row := range rows {
		byID[row.TrackID] = row
	}

	ep := byID[f.episode1.ID]
	if ep.EpisodeTitle != "Office Olympics" {
		t.Fatalf("episode title = %q, want the arr mirror's title", ep.EpisodeTitle)
	}
	if ep.MediaType != "series" || ep.Title != "The Office" || ep.Year != 2005 {
		t.Fatalf("episode row = %+v", ep)
	}
	if ep.Season == nil || *ep.Season != 2 || ep.Episode == nil || *ep.Episode != 3 {
		t.Fatalf("season/episode = %v/%v", ep.Season, ep.Episode)
	}
	if ep.MediaFolderPath != "/vault/The Office (2005)" || ep.FilePath != f.episode1.FilePath {
		t.Fatalf("paths = %q / %q", ep.MediaFolderPath, ep.FilePath)
	}
	if ep.Variant != "lektor" || ep.Codec != "eac3" || ep.Channels != "2.0" || ep.LangISO2 != "pl" {
		t.Fatalf("track fields = %+v", ep)
	}

	movie := byID[f.movieTrack.ID]
	if movie.EpisodeTitle != "" {
		t.Fatalf("movie episode title = %q, want empty (never invented)", movie.EpisodeTitle)
	}
	if movie.Season != nil || movie.Episode != nil {
		t.Fatalf("movie season/episode = %v/%v, want nil", movie.Season, movie.Episode)
	}

	// Scope: media type and explicit ids.
	series, err := f.store.VaultTrackPlanRows(context.Background(), "series", nil)
	if err != nil {
		t.Fatalf("VaultTrackPlanRows(series): %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("series rows = %d, want 2", len(series))
	}
	only, err := f.store.VaultTrackPlanRows(context.Background(), "all", []int64{f.movieTrack.ID})
	if err != nil {
		t.Fatalf("VaultTrackPlanRows(ids): %v", err)
	}
	if len(only) != 1 || only[0].TrackID != f.movieTrack.ID {
		t.Fatalf("id-scoped rows = %+v", only)
	}
	if _, err := f.store.VaultTrackPlanRows(context.Background(), "episode", nil); err == nil {
		t.Fatal("an unknown media type was accepted")
	}
}

func TestUpdateTrackPathsTxGuardsTheSourceValue(t *testing.T) {
	f := seedOrganiseFixture(t)
	ctx := context.Background()

	tx, err := f.store.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	wrong := []TrackPathUpdate{{
		TrackID: f.movieTrack.ID,
		From:    "/vault/Spirited Away (2001)/not-the-current-name.pl.mka",
		To:      "/vault/Spirited Away (2001)/new.pl.mka",
	}}
	if err := UpdateTrackPathsTx(ctx, tx, wrong); !errors.Is(err, ErrTrackPathChanged) {
		t.Fatalf("UpdateTrackPathsTx(wrong source) = %v, want ErrTrackPathChanged", err)
	}

	right := []TrackPathUpdate{{
		TrackID: f.movieTrack.ID,
		From:    f.movieTrack.FilePath,
		To:      "/vault/Spirited Away (2001)/new.pl.mka",
	}}
	if err := UpdateTrackPathsTx(ctx, tx, right); err != nil {
		t.Fatalf("UpdateTrackPathsTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, err := f.store.TrackPath(ctx, f.movieTrack.ID)
	if err != nil {
		t.Fatalf("TrackPath: %v", err)
	}
	if got != "/vault/Spirited Away (2001)/new.pl.mka" {
		t.Fatalf("path = %q", got)
	}
	if _, err := f.store.TrackPath(ctx, 4242); !errors.Is(err, ErrTrackPathNotFound) {
		t.Fatalf("TrackPath(unknown) = %v, want ErrTrackPathNotFound", err)
	}
}

func TestInsertOrganiseRunAndList(t *testing.T) {
	f := seedOrganiseFixture(t)
	ctx := context.Background()

	runID, err := f.store.InsertOrganiseRun(ctx, OrganiseRunRecord{
		PlanHash:  "sha256:abc",
		FileCount: 2,
		Status:    OrganiseStatusApplied,
		Detail:    "template_change",
		Files: []OrganiseFileRecord{
			{TrackID: f.episode1.ID, SourcePath: "a", DestinationPath: "b", Result: OrganiseResultRenamed},
			{TrackID: f.episode2.ID, SourcePath: "c", DestinationPath: "d", Result: OrganiseResultRenamed},
		},
	})
	if err != nil {
		t.Fatalf("InsertOrganiseRun: %v", err)
	}
	if runID == 0 {
		t.Fatal("run id = 0")
	}

	var files int
	if err := f.store.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM vault_organise_files WHERE run_id = ?`, runID).Scan(&files); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if files != 2 {
		t.Fatalf("audit file rows = %d, want 2", files)
	}

	runs, err := f.store.ListOrganiseRuns(ctx, 50)
	if err != nil {
		t.Fatalf("ListOrganiseRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if runs[0].Status != OrganiseStatusApplied || runs[0].FileCount != 2 || runs[0].PlanHash != "sha256:abc" {
		t.Fatalf("run = %+v", runs[0])
	}
	if runs[0].StartedAt == "" {
		t.Fatal("run has no started_at; the audit row must be complete")
	}

	if _, err := f.store.InsertOrganiseRun(ctx, OrganiseRunRecord{
		PlanHash: "sha256:def", FileCount: 0, Status: "done",
	}); err == nil {
		t.Fatal("an unknown status was accepted by the CHECK constraint")
	}
}

func TestJobsInFlight(t *testing.T) {
	f := seedOrganiseFixture(t)
	ctx := context.Background()

	insertJob := func(target int64, ref any, status string) {
		t.Helper()
		if _, err := f.store.DB.ExecContext(ctx,
			`INSERT INTO sync_mux_jobs (target_track_id, ref_track_id, target_video_path, target_lang_iso2, status)
			 VALUES (?, ?, '/media/target.mkv', 'pl', ?)`, target, ref, status); err != nil {
			t.Fatalf("insert job: %v", err)
		}
	}
	insertJob(f.episode1.ID, nil, SyncJobHeld)          // referencing, not blocking
	insertJob(f.episode2.ID, f.episode1.ID, SyncJobMuxing) // referencing and blocking
	insertJob(f.movieTrack.ID, nil, SyncJobQueued)      // referencing, not blocking

	counts, err := f.store.JobsInFlight(ctx, []int64{f.episode1.ID, f.episode2.ID})
	if err != nil {
		t.Fatalf("JobsInFlight: %v", err)
	}
	if counts.Referencing != 2 {
		t.Fatalf("referencing = %d, want 2", counts.Referencing)
	}
	if counts.Blocking != 1 {
		t.Fatalf("blocking = %d, want 1", counts.Blocking)
	}

	empty, err := f.store.JobsInFlight(ctx, nil)
	if err != nil {
		t.Fatalf("JobsInFlight(nil): %v", err)
	}
	if empty.Referencing != 0 || empty.Blocking != 0 {
		t.Fatalf("empty counts = %+v", empty)
	}

	// A held job cannot be mid-mux by definition, so it never blocks (#48).
	f.store.DB.ExecContext(ctx, `UPDATE sync_mux_jobs SET status = 'held' WHERE target_track_id = ?`, f.episode2.ID)
	counts, err = f.store.JobsInFlight(ctx, []int64{f.episode1.ID, f.episode2.ID})
	if err != nil {
		t.Fatalf("JobsInFlight: %v", err)
	}
	if counts.Blocking != 0 || counts.Referencing != 2 {
		t.Fatalf("counts with only held jobs = %+v", counts)
	}
}
