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
	instanceID int64
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
		store: st, instanceID: instanceID, seriesID: seriesID, movieID: movieID,
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

// TestPlanRowCarriesIDs pins U07's source columns: the numeric ids come from
// vault_media itself, the IMDb id from the arr mirror row matched on the same id
// pair the episode-title subquery uses.
func TestPlanRowCarriesIDs(t *testing.T) {
	f := seedOrganiseFixture(t)
	ctx := context.Background()

	// The movie's mirror row: same tmdb id, so the correlated subquery finds it.
	if _, err := f.store.DB.ExecContext(ctx,
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, tmdb_id, imdb_id, title,
		  year, eligibility_status)
		 VALUES (?, 'movie', 7, 129, 'tt0245429', 'Spirited Away', 2001, 'eligible')`,
		f.instanceID); err != nil {
		t.Fatalf("insert movie cache row: %v", err)
	}

	rows, err := f.store.VaultTrackPlanRows(ctx, "all", nil)
	if err != nil {
		t.Fatalf("VaultTrackPlanRows: %v", err)
	}
	byID := map[int64]VaultTrackPlanRow{}
	for _, row := range rows {
		byID[row.TrackID] = row
	}

	movie := byID[f.movieTrack.ID]
	if movie.TMDBID != 129 {
		t.Fatalf("movie tmdb id = %d, want 129 (vault_media)", movie.TMDBID)
	}
	if movie.TVDBID != 0 {
		t.Fatalf("movie tvdb id = %d, want 0 (vault_media has none)", movie.TVDBID)
	}
	if movie.IMDBID != "tt0245429" {
		t.Fatalf("movie imdb id = %q, want the mirror's tt0245429", movie.IMDBID)
	}

	// The series' tvdb id is 42 in the fixture; its mirror rows carry no IMDb id,
	// so the token would render empty rather than invent one.
	ep := byID[f.episode1.ID]
	if ep.TVDBID != 42 {
		t.Fatalf("episode tvdb id = %d, want 42 (vault_media)", ep.TVDBID)
	}
	if ep.TMDBID != 0 {
		t.Fatalf("episode tmdb id = %d, want 0 (the series has no tmdb id)", ep.TMDBID)
	}
	if ep.IMDBID != "" {
		t.Fatalf("episode imdb id = %q, want empty (never invented)", ep.IMDBID)
	}
}

// TestPlanRowIMDBIDResolvesToTheOwningMirrorRow pins U07's imdb scoping: the
// media's own kind decides which cache rows may answer, the row agreeing with
// vault_media's tmdb id beats a newer tvdb-only row, and two rows that both
// agree resolve by the documented tiebreak (highest cache id) rather than by
// whichever row a subquery happened to reach first.
func TestPlanRowIMDBIDResolvesToTheOwningMirrorRow(t *testing.T) {
	f := seedOrganiseFixture(t)
	ctx := context.Background()

	// A movie carrying both ids — the shape the unscoped subquery got wrong: it
	// joined on either id and ordered by c.id alone.
	mediaID, err := f.store.EnsureVaultMedia(ctx, VaultMediaRecord{
		MediaType: "movie", Title: "Both Ids", Year: 1999, TMDBID: 129, TVDBID: 77,
		FolderPath: "/vault/Both Ids (1999)",
	})
	if err != nil {
		t.Fatalf("EnsureVaultMedia(both ids): %v", err)
	}
	trackID, err := f.store.RecordVaultTrack(ctx, VaultTrackRecord{
		VaultMediaID: mediaID, GroupUUID: "g-both", LangISO2: "pl", Variant: "dubbing",
		FilePath: "/vault/Both Ids (1999)/both.pl.mka", SHA256: "dd",
		Codec: "eac3", Channels: "5.1", ChannelCount: 6,
	})
	if err != nil {
		t.Fatalf("RecordVaultTrack(both ids): %v", err)
	}

	// A second connection, so the two mirror rows come from different instances.
	res, err := f.store.DB.ExecContext(ctx,
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4, enabled)
		 VALUES ('radarr-2', 'radarr', 'http://radarr2:7878', 'enc', '0001', 1)`)
	if err != nil {
		t.Fatalf("insert second arr instance: %v", err)
	}
	secondInstance, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("second instance id: %v", err)
	}

	insertCache := func(instanceID int64, mediaType string, externalID int64, tmdbID, tvdbID *int64, imdb string) int64 {
		t.Helper()
		res, err := f.store.DB.ExecContext(ctx,
			`INSERT INTO arr_library_cache
			 (arr_instance_id, media_type, external_item_id, tmdb_id, tvdb_id, imdb_id,
			  title, year, eligibility_status)
			 VALUES (?, ?, ?, ?, ?, ?, 'Both Ids', 1999, 'eligible')`,
			instanceID, mediaType, externalID, tmdbID, tvdbID, imdb)
		if err != nil {
			t.Fatalf("insert %s cache row %q: %v", mediaType, imdb, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("cache row id: %v", err)
		}
		return id
	}
	clearCache := func() {
		t.Helper()
		if _, err := f.store.DB.ExecContext(ctx, `DELETE FROM arr_library_cache`); err != nil {
			t.Fatalf("clear arr cache: %v", err)
		}
	}
	imdbOfTrack := func() string {
		t.Helper()
		rows, err := f.store.VaultTrackPlanRows(ctx, "movie", []int64{trackID})
		if err != nil {
			t.Fatalf("VaultTrackPlanRows: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("movie rows = %d, want 1", len(rows))
		}
		return rows[0].IMDBID
	}

	tmdb, tvdb := int64(129), int64(77)

	// Two rows for one media, from two instances: the tmdb row is inserted
	// first (lower id), the tvdb row second (higher id), so ordering by c.id
	// alone would answer ttBY_TVDB. The tmdb row is the one that agrees with
	// vault_media's own id, so it wins regardless of insertion order.
	clearCache()
	tmdbRow := insertCache(f.instanceID, "movie", 301, &tmdb, nil, "ttBY_TMDB")
	tvdbRow := insertCache(secondInstance, "movie", 302, nil, &tvdb, "ttBY_TVDB")
	if tvdbRow <= tmdbRow {
		t.Fatalf("fixture order: tvdb row %d should be newer than tmdb row %d", tvdbRow, tmdbRow)
	}
	if got := imdbOfTrack(); got != "ttBY_TMDB" {
		t.Fatalf("imdb id = %q, want ttBY_TMDB (the row agreeing with vault_media.tmdb_id), not the newer tvdb row", got)
	}

	// A row of the wrong kind never answers, even when both ids and the cache id
	// point at it: a movie does not borrow an episode's imdb id.
	clearCache()
	insertCache(secondInstance, "episode", 303, &tmdb, &tvdb, "ttEPISODE")
	if got := imdbOfTrack(); got != "" {
		t.Fatalf("imdb id from an episode row = %q, want empty (wrong media kind)", got)
	}

	// Two connections that agree on the tmdb id: vault_media records no
	// arr_instance_id, so the tiebreak is the highest cache id (the most
	// recently mirrored row) — arbitrary but deterministic, so the same plan is
	// built every time.
	clearCache()
	firstRow := insertCache(f.instanceID, "movie", 304, &tmdb, nil, "ttFIRST")
	secondRow := insertCache(secondInstance, "movie", 305, &tmdb, nil, "ttSECOND")
	if secondRow <= firstRow {
		t.Fatalf("fixture order: second row %d should be newer than first row %d", secondRow, firstRow)
	}
	if got := imdbOfTrack(); got != "ttSECOND" {
		t.Fatalf("imdb id with two agreeing instances = %q, want ttSECOND (highest cache id)", got)
	}
	if _, err := f.store.DB.ExecContext(ctx, `DELETE FROM arr_library_cache WHERE id = ?`, secondRow); err != nil {
		t.Fatalf("delete newer mirror row: %v", err)
	}
	if got := imdbOfTrack(); got != "ttFIRST" {
		t.Fatalf("imdb id after the newer row left = %q, want ttFIRST", got)
	}
}

// TestPlanRowWithoutAMirrorRowStillPlans: the ids in vault_media survive a cache
// row that left the mirror, and the IMDb token simply renders empty — the plan
// must not fail because the mirror forgot a title.
func TestPlanRowWithoutAMirrorRowStillPlans(t *testing.T) {
	f := seedOrganiseFixture(t)
	ctx := context.Background()

	// Seed a movie mirror row, read it, then delete it as a sync pass would.
	res, err := f.store.DB.ExecContext(ctx,
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, tmdb_id, imdb_id, title,
		  year, eligibility_status)
		 VALUES (?, 'movie', 7, 129, 'tt0245429', 'Spirited Away', 2001, 'eligible')`,
		f.instanceID)
	if err != nil {
		t.Fatalf("insert movie cache row: %v", err)
	}
	mirrorID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("mirror row id: %v", err)
	}

	read := func() VaultTrackPlanRow {
		t.Helper()
		rows, err := f.store.VaultTrackPlanRows(ctx, "movie", nil)
		if err != nil {
			t.Fatalf("VaultTrackPlanRows: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("movie rows = %d, want 1", len(rows))
		}
		return rows[0]
	}

	if got := read().IMDBID; got != "tt0245429" {
		t.Fatalf("imdb id with a mirror row = %q, want tt0245429", got)
	}

	if _, err := f.store.DB.ExecContext(ctx, `DELETE FROM arr_library_cache WHERE id = ?`, mirrorID); err != nil {
		t.Fatalf("delete mirror row: %v", err)
	}
	row := read()
	if row.IMDBID != "" {
		t.Fatalf("imdb id without a mirror row = %q, want empty", row.IMDBID)
	}
	if row.TMDBID != 129 {
		t.Fatalf("tmdb id without a mirror row = %d, want 129 from vault_media", row.TMDBID)
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

func TestListOrganiseFilesForRunsFiltersAndOrders(t *testing.T) {
	f := seedOrganiseFixture(t)
	ctx := context.Background()

	attentionID, err := f.store.InsertOrganiseRun(ctx, OrganiseRunRecord{
		PlanHash:  "sha256:attention",
		FileCount: 4,
		Status:    OrganiseStatusNeedsAttention,
		Detail:    "stage=rename: injected",
		Files: []OrganiseFileRecord{
			// Deliberately not in path order: the read orders by source_path.
			{SourcePath: "/vault/zeta.pl.mka", DestinationPath: "/vault/zeta-new.pl.mka", Result: OrganiseResultFailed},
			{SourcePath: "/vault/beta.pl.mka", DestinationPath: "/vault/beta-new.pl.mka", Result: OrganiseResultRenamed},
			{SourcePath: "/vault/alpha.pl.mka", DestinationPath: "/vault/alpha-new.pl.mka", Result: OrganiseResultReconciled},
			{SourcePath: "/vault/gamma.pl.mka", DestinationPath: "/vault/gamma.pl.mka", Result: OrganiseResultRenamedBack},
		},
	})
	if err != nil {
		t.Fatalf("InsertOrganiseRun(needs_attention): %v", err)
	}
	appliedID, err := f.store.InsertOrganiseRun(ctx, OrganiseRunRecord{
		PlanHash: "sha256:applied", FileCount: 1, Status: OrganiseStatusApplied,
		Files: []OrganiseFileRecord{
			{SourcePath: "/vault/one.pl.mka", DestinationPath: "/vault/one-new.pl.mka", Result: OrganiseResultRenamed},
		},
	})
	if err != nil {
		t.Fatalf("InsertOrganiseRun(applied): %v", err)
	}

	files, err := f.store.ListOrganiseFilesForRuns(ctx, []int64{attentionID, appliedID})
	if err != nil {
		t.Fatalf("ListOrganiseFilesForRuns: %v", err)
	}
	if _, ok := files[appliedID]; ok {
		t.Fatalf("an applied run with only renamed rows came back: %+v", files[appliedID])
	}
	rows := files[attentionID]
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want exactly reconciled and failed", rows)
	}
	if rows[0].SourcePath != "/vault/alpha.pl.mka" || rows[0].Result != OrganiseResultReconciled {
		t.Fatalf("row 0 = %+v, want the reconciled alpha row ordered by source path", rows[0])
	}
	if rows[1].SourcePath != "/vault/zeta.pl.mka" || rows[1].Result != OrganiseResultFailed {
		t.Fatalf("row 1 = %+v, want the failed zeta row", rows[1])
	}
	if rows[1].DestinationPath != "/vault/zeta-new.pl.mka" || rows[1].RunID != attentionID {
		t.Fatalf("row 1 = %+v, want destination and run id kept", rows[1])
	}

	// An empty list is answered without a query (no IN () syntax error).
	empty, err := f.store.ListOrganiseFilesForRuns(ctx, nil)
	if err != nil {
		t.Fatalf("ListOrganiseFilesForRuns(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("nil run ids = %+v, want an empty map", empty)
	}
	unknown, err := f.store.ListOrganiseFilesForRuns(ctx, []int64{4242})
	if err != nil {
		t.Fatalf("ListOrganiseFilesForRuns(unknown): %v", err)
	}
	if len(unknown) != 0 {
		t.Fatalf("unknown run id = %+v, want an empty map", unknown)
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
	insertJob(f.episode1.ID, nil, SyncJobHeld)             // referencing, not blocking
	insertJob(f.episode2.ID, f.episode1.ID, SyncJobMuxing) // referencing and blocking
	insertJob(f.movieTrack.ID, nil, SyncJobQueued)         // referencing, not blocking

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
