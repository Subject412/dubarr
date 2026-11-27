package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// hierarchyMovie inserts one cached Radarr row. targetFile "" models a movie
// Radarr has no file for; the eligibility verdict is passed explicitly because
// the series-level tests depend on the worst-verdict rank.
func hierarchyMovie(t *testing.T, s *Store, ctx context.Context, instanceID, externalID int64,
	title, originalTitle string, year int, targetFile, eligibility string) int64 {
	t.Helper()
	var target any
	if targetFile != "" {
		target = targetFile
	}
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, title, original_title, year,
		  media_folder_path, target_file_path, matches_target_tags, eligibility_status,
		  video_codec, updated_at)
		 VALUES (?, 'movie', ?, ?, ?, ?, '/media/movies', ?, 1, ?, 'hevc',
		         '2026-01-02 03:04:05')`,
		instanceID, externalID, title, originalTitle, year, target, eligibility)
	if err != nil {
		t.Fatalf("insert movie %q: %v", title, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("movie id %q: %v", title, err)
	}
	return id
}

// hierarchyEpisodeOpts tunes one cached Sonarr episode row. The zero value is
// the common case: a downloaded, tag-eligible episode of a series whose title
// and year are passed to hierarchyEpisode.
type hierarchyEpisodeOpts struct {
	// TargetFile "" writes NULL (arr has no episode file).
	TargetFile    string
	Eligibility   string
	EpisodeTitle  string
	OriginalTitle string
	UpdatedAt     string
}

// hierarchyEpisode inserts one cached Sonarr episode. external_item_id is
// derived from (series, season, episode) so the unique identity index never
// fires and callers do not have to thread a counter through fixtures.
func hierarchyEpisode(t *testing.T, s *Store, ctx context.Context, instanceID, seriesID int64,
	season, episode int, seriesTitle string, year int, opts hierarchyEpisodeOpts) int64 {
	t.Helper()
	eligibility := opts.Eligibility
	if eligibility == "" {
		eligibility = "eligible"
	}
	updatedAt := opts.UpdatedAt
	if updatedAt == "" {
		updatedAt = "2026-01-02 03:04:05"
	}
	externalID := seriesID*100000 + int64(season)*1000 + int64(episode)
	episodeTitle := opts.EpisodeTitle
	if episodeTitle == "" {
		episodeTitle = fmt.Sprintf("Episode %d", episode)
	}
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, series_id, title, original_title,
		  year, season_number, episode_number, episode_title, media_folder_path,
		  target_file_path, matches_target_tags, eligibility_status, video_codec, updated_at)
		 VALUES (?, 'episode', ?, ?, ?, ?, ?, ?, ?, ?, '/media/tv/series',
		         ?, 1, ?, 'hevc', ?)`,
		instanceID, externalID, seriesID, seriesTitle, opts.OriginalTitle, year,
		season, episode, episodeTitle, nullIfEmpty(opts.TargetFile), eligibility, updatedAt)
	if err != nil {
		t.Fatalf("insert episode s%d e%d of %q: %v", season, episode, seriesTitle, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("episode id: %v", err)
	}
	return id
}

func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// hierarchyStore opens an empty store plus one Sonarr instance.
func hierarchyStore(t *testing.T) (*Store, context.Context, int64) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	return s, ctx, libraryInstance(t, s, ctx, "Sonarr", "sonarr")
}

// TestListMoviesPaginatesAndCountsWholeSet pins that Total counts the whole
// filtered set, that pages do not overlap and that ties on title fall back to
// id so paging is deterministic.
func TestListMoviesPaginatesAndCountsWholeSet(t *testing.T) {
	s, ctx, sonarr := hierarchyStore(t)
	radarr := libraryInstance(t, s, ctx, "Radarr", "radarr")

	// 250 rows over 50 distinct titles: every title is a five-way tie, so a
	// missing id tiebreaker would make paging repeat or skip rows.
	for i := 0; i < 250; i++ {
		hierarchyMovie(t, s, ctx, radarr, int64(1000+i),
			fmt.Sprintf("Movie %03d", i%50), "", 2001, "/media/x/f.mkv", "eligible")
	}
	// A Sonarr episode must never leak into the Radarr list.
	hierarchyEpisode(t, s, ctx, sonarr, 1, 1, 1, "The Office", 2005, hierarchyEpisodeOpts{})

	seen := make(map[int64]bool, 250)
	for page := 1; page <= 5; page++ {
		total, rows, err := s.ListMovies(ctx, HierarchyFilter{Page: page, PageSize: 50})
		if err != nil {
			t.Fatalf("ListMovies page %d: %v", page, err)
		}
		if total != 250 {
			t.Fatalf("page %d total = %d, want 250", page, total)
		}
		if len(rows) != 50 {
			t.Fatalf("page %d rows = %d, want 50", page, len(rows))
		}
		for _, row := range rows {
			if seen[row.ID] {
				t.Fatalf("page %d repeated movie id %d", page, row.ID)
			}
			seen[row.ID] = true
		}
	}
	if len(seen) != 250 {
		t.Fatalf("distinct ids across pages = %d, want 250", len(seen))
	}

	// A page past the end is empty, not an error, and still reports the total.
	total, rows, err := s.ListMovies(ctx, HierarchyFilter{Page: 6, PageSize: 50})
	if err != nil {
		t.Fatalf("ListMovies page 6: %v", err)
	}
	if total != 250 || len(rows) != 0 {
		t.Fatalf("page 6 = %d rows / total %d, want 0/250", len(rows), total)
	}
}

// TestListMoviesSearchMatchesTitleAndOriginalTitle pins a case-insensitive
// substring match on both title columns, including a non-Latin original title.
func TestListMoviesSearchMatchesTitleAndOriginalTitle(t *testing.T) {
	s, ctx, _ := hierarchyStore(t)
	radarr := libraryInstance(t, s, ctx, "Radarr", "radarr")

	hierarchyMovie(t, s, ctx, radarr, 1, "Spirited Away", "千と千尋の神隠し", 2001,
		"/media/x/a.mkv", "eligible")
	hierarchyMovie(t, s, ctx, radarr, 2, "Arrival", "Przybycie", 2016,
		"/media/x/b.mkv", "eligible")
	hierarchyMovie(t, s, ctx, radarr, 3, "Shrek", "", 2001, "/media/x/c.mkv", "eligible")

	cases := map[string]string{
		"AWAY":      "Spirited Away",
		"千と千尋":      "Spirited Away",
		"przybycie": "Arrival",
	}
	for q, want := range cases {
		total, rows, err := s.ListMovies(ctx, HierarchyFilter{Q: q})
		if err != nil {
			t.Fatalf("ListMovies q=%q: %v", q, err)
		}
		if total != 1 || len(rows) != 1 {
			t.Fatalf("q=%q = %d rows / total %d, want 1/1", q, len(rows), total)
		}
		if rows[0].Title != want {
			t.Fatalf("q=%q matched %q, want %q", q, rows[0].Title, want)
		}
	}
}

// TestListSeriesGroupsEpisodeRowsIntoOneRow pins that ten episode rows collapse
// into one series row, without loading the episodes into Go.
func TestListSeriesGroupsEpisodeRowsIntoOneRow(t *testing.T) {
	s, ctx, sonarr := hierarchyStore(t)

	// Three series, ten episodes each over two seasons (5 + 5).
	for series := int64(1); series <= 3; series++ {
		title := fmt.Sprintf("Series %d", series)
		for season := 1; season <= 2; season++ {
			for episode := 1; episode <= 5; episode++ {
				hierarchyEpisode(t, s, ctx, sonarr, series, season, episode, title, 2005,
					hierarchyEpisodeOpts{})
			}
		}
	}

	total, rows, err := s.ListSeries(ctx, HierarchyFilter{})
	if err != nil {
		t.Fatalf("ListSeries: %v", err)
	}
	if total != 3 || len(rows) != 3 {
		t.Fatalf("ListSeries = %d rows / total %d, want 3/3", len(rows), total)
	}
	for i, row := range rows {
		wantTitle := fmt.Sprintf("Series %d", i+1)
		if row.Title != wantTitle {
			t.Errorf("row %d title = %q, want %q (ordered by title)", i, row.Title, wantTitle)
		}
		if row.EpisodeCount != 10 || row.SeasonCount != 2 {
			t.Errorf("%s = %d episodes / %d seasons, want 10/2",
				row.Title, row.EpisodeCount, row.SeasonCount)
		}
		if row.ArrInstanceID != sonarr || row.InstanceName != "Sonarr" {
			t.Errorf("%s instance = %d/%q, want %d/Sonarr",
				row.Title, row.ArrInstanceID, row.InstanceName, sonarr)
		}
		if row.Year != 2005 {
			t.Errorf("%s year = %d, want 2005", row.Title, row.Year)
		}
	}
}

// TestListSeriesAggregatesEpisodeFileCount pins NULL and empty target paths as
// "no file" and only a real path as a file.
func TestListSeriesAggregatesEpisodeFileCount(t *testing.T) {
	s, ctx, sonarr := hierarchyStore(t)

	hierarchyEpisode(t, s, ctx, sonarr, 1, 1, 1, "Half Files", 2005,
		hierarchyEpisodeOpts{TargetFile: ""})
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE arr_library_cache SET target_file_path = '' WHERE episode_number = 1`); err != nil {
		t.Fatalf("blank target path: %v", err)
	}
	hierarchyEpisode(t, s, ctx, sonarr, 1, 1, 2, "Half Files", 2005,
		hierarchyEpisodeOpts{TargetFile: "/media/tv/e2.mkv"})
	hierarchyEpisode(t, s, ctx, sonarr, 1, 1, 3, "Half Files", 2005,
		hierarchyEpisodeOpts{TargetFile: "/media/tv/e3.mkv"})

	_, rows, err := s.ListSeries(ctx, HierarchyFilter{})
	if err != nil {
		t.Fatalf("ListSeries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].EpisodeCount != 3 || rows[0].EpisodeFileCount != 2 {
		t.Fatalf("episodes/files = %d/%d, want 3/2",
			rows[0].EpisodeCount, rows[0].EpisodeFileCount)
	}
}

// TestListSeriesEligibilityIsWorstVerdict pins the rank: waiting_for_tags beats
// missing_target beats eligible, whatever the row order.
func TestListSeriesEligibilityIsWorstVerdict(t *testing.T) {
	s, ctx, sonarr := hierarchyStore(t)

	hierarchyEpisode(t, s, ctx, sonarr, 1, 1, 1, "Tags Beat All", 2005,
		hierarchyEpisodeOpts{Eligibility: "missing_target"})
	hierarchyEpisode(t, s, ctx, sonarr, 1, 1, 2, "Tags Beat All", 2005,
		hierarchyEpisodeOpts{Eligibility: "eligible"})
	hierarchyEpisode(t, s, ctx, sonarr, 1, 1, 3, "Tags Beat All", 2005,
		hierarchyEpisodeOpts{Eligibility: "waiting_for_tags"})

	hierarchyEpisode(t, s, ctx, sonarr, 2, 1, 1, "Missing Target", 2005,
		hierarchyEpisodeOpts{Eligibility: "eligible"})
	hierarchyEpisode(t, s, ctx, sonarr, 2, 1, 2, "Missing Target", 2005,
		hierarchyEpisodeOpts{Eligibility: "missing_target"})

	hierarchyEpisode(t, s, ctx, sonarr, 3, 1, 1, "All Eligible", 2005,
		hierarchyEpisodeOpts{Eligibility: "eligible"})
	hierarchyEpisode(t, s, ctx, sonarr, 3, 1, 2, "All Eligible", 2005,
		hierarchyEpisodeOpts{Eligibility: "eligible"})

	_, rows, err := s.ListSeries(ctx, HierarchyFilter{})
	if err != nil {
		t.Fatalf("ListSeries: %v", err)
	}
	want := map[string]string{
		"All Eligible":   "eligible",
		"Missing Target": "missing_target",
		"Tags Beat All":  "waiting_for_tags",
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(rows), len(want))
	}
	for _, row := range rows {
		if row.Eligibility != want[row.Title] {
			t.Errorf("%s eligibility = %q, want %q", row.Title, row.Eligibility, want[row.Title])
		}
	}
}

// TestSeasonsForSeriesAggregatesAndOrders pins numeric season ordering (1, 2,
// 10 — not 1, 10, 2) and the per-season statistics computed in SQL.
func TestSeasonsForSeriesAggregatesAndOrders(t *testing.T) {
	s, ctx, sonarr := hierarchyStore(t)

	// Season 10: 2 episodes, 1 file. Season 1: 4 episodes, 3 files. Season 2:
	// 1 episode, 1 file.
	for episode := 1; episode <= 2; episode++ {
		hierarchyEpisode(t, s, ctx, sonarr, 7, 10, episode, "The Office", 2005,
			hierarchyEpisodeOpts{TargetFile: fmt.Sprintf("/media/tv/s10e%d.mkv", episode)})
	}
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE arr_library_cache SET target_file_path = NULL
		 WHERE season_number = 10 AND episode_number = 2`); err != nil {
		t.Fatalf("null second season-10 file: %v", err)
	}
	for episode := 1; episode <= 4; episode++ {
		target := ""
		if episode <= 3 {
			target = fmt.Sprintf("/media/tv/s01e%d.mkv", episode)
		}
		hierarchyEpisode(t, s, ctx, sonarr, 7, 1, episode, "The Office", 2005,
			hierarchyEpisodeOpts{TargetFile: target})
	}
	hierarchyEpisode(t, s, ctx, sonarr, 7, 2, 1, "The Office", 2005,
		hierarchyEpisodeOpts{TargetFile: "/media/tv/s02e1.mkv"})

	seasons, err := s.SeasonsForSeries(ctx, 7)
	if err != nil {
		t.Fatalf("SeasonsForSeries: %v", err)
	}
	if len(seasons) != 3 {
		t.Fatalf("seasons = %d, want 3", len(seasons))
	}
	wantNumbers := []int{1, 2, 10}
	for i, want := range wantNumbers {
		if seasons[i].SeasonNumber != want {
			t.Fatalf("season %d = %d, want %d (numeric order)", i, seasons[i].SeasonNumber, want)
		}
	}
	if seasons[0].EpisodeCount != 4 || seasons[0].EpisodeFileCount != 3 {
		t.Errorf("season 1 = %d episodes / %d files, want 4/3",
			seasons[0].EpisodeCount, seasons[0].EpisodeFileCount)
	}
	if seasons[0].PercentEpisodes != 75 {
		t.Errorf("season 1 percent = %v, want 75", seasons[0].PercentEpisodes)
	}
	if seasons[2].EpisodeCount != 2 || seasons[2].EpisodeFileCount != 1 {
		t.Errorf("season 10 = %d episodes / %d files, want 2/1",
			seasons[2].EpisodeCount, seasons[2].EpisodeFileCount)
	}
	if seasons[2].PercentEpisodes != 50 {
		t.Errorf("season 10 percent = %v, want 50", seasons[2].PercentEpisodes)
	}
}

// TestEpisodesForSeasonExcludesOtherSeasonsAndSeries pins the classic cross-
// series leak: a season number shared by two series must not merge.
func TestEpisodesForSeasonExcludesOtherSeasonsAndSeries(t *testing.T) {
	s, ctx, sonarr := hierarchyStore(t)

	for _, series := range []int64{1, 2} {
		for _, season := range []int{1, 2} {
			for episode := 1; episode <= 2; episode++ {
				hierarchyEpisode(t, s, ctx, sonarr, series, season, episode,
					fmt.Sprintf("Series %d", series), 2005, hierarchyEpisodeOpts{})
			}
		}
	}

	rows, err := s.EpisodesForSeason(ctx, 1, 1)
	if err != nil {
		t.Fatalf("EpisodesForSeason: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, row := range rows {
		if row.SeriesID != 1 || row.SeasonNumber != 1 {
			t.Fatalf("row = series %d season %d, want series 1 season 1",
				row.SeriesID, row.SeasonNumber)
		}
	}
}

// TestEpisodesForSeasonOrdersByEpisodeNumber pins season-internal ordering.
func TestEpisodesForSeasonOrdersByEpisodeNumber(t *testing.T) {
	s, ctx, sonarr := hierarchyStore(t)

	for _, episode := range []int{3, 1, 2} {
		hierarchyEpisode(t, s, ctx, sonarr, 4, 1, episode, "Out Of Order", 2005,
			hierarchyEpisodeOpts{EpisodeTitle: fmt.Sprintf("Ep %d", episode)})
	}

	rows, err := s.EpisodesForSeason(ctx, 4, 1)
	if err != nil {
		t.Fatalf("EpisodesForSeason: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	for i, want := range []int{1, 2, 3} {
		if rows[i].EpisodeNumber != want {
			t.Fatalf("row %d episode = %d, want %d", i, rows[i].EpisodeNumber, want)
		}
	}
}

// TestEpisodesForSeasonCapsAtSeasonMaxRows pins the hard cap: one season above
// LibrarySeasonMaxRows returns exactly the cap, and the count still reports the
// real total so the handler can flag truncation.
func TestEpisodesForSeasonCapsAtSeasonMaxRows(t *testing.T) {
	s, ctx, sonarr := hierarchyStore(t)
	seedHierarchy(t, s.DB, sonarr, 1, LibrarySeasonMaxRows+1)

	rows, err := s.EpisodesForSeason(ctx, 1, 1)
	if err != nil {
		t.Fatalf("EpisodesForSeason: %v", err)
	}
	if len(rows) != LibrarySeasonMaxRows {
		t.Fatalf("rows = %d, want the cap %d", len(rows), LibrarySeasonMaxRows)
	}
	total, err := s.SeasonEpisodeCount(ctx, 1, 1)
	if err != nil {
		t.Fatalf("SeasonEpisodeCount: %v", err)
	}
	if total != LibrarySeasonMaxRows+1 {
		t.Fatalf("count = %d, want %d", total, LibrarySeasonMaxRows+1)
	}
}

// TestEpisodesForSeasonPagePagesInsideOneSeason pins the episode route's paging:
// a page is a slice of one season, and the season total stays the whole season.
func TestEpisodesForSeasonPagePagesInsideOneSeason(t *testing.T) {
	s, ctx, sonarr := hierarchyStore(t)
	seedHierarchy(t, s.DB, sonarr, 1, 10)

	rows, err := s.EpisodesForSeasonPage(ctx, 1, 1, 2, 3)
	if err != nil {
		t.Fatalf("EpisodesForSeasonPage: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	for i, want := range []int{4, 5, 6} {
		if rows[i].EpisodeNumber != want {
			t.Fatalf("row %d episode = %d, want %d", i, rows[i].EpisodeNumber, want)
		}
	}
}

// TestHierarchyFiltersRespectInstanceID pins that instance scoping applies to
// both the movie list and the series aggregate.
func TestHierarchyFiltersRespectInstanceID(t *testing.T) {
	s, ctx, first := hierarchyStore(t)
	second := libraryInstance(t, s, ctx, "Sonarr 2", "sonarr")

	hierarchyMovie(t, s, ctx, first, 1, "First Movie", "", 2001, "/media/x/1.mkv", "eligible")
	hierarchyMovie(t, s, ctx, first, 2, "Second Movie", "", 2001, "/media/x/2.mkv", "eligible")
	hierarchyEpisode(t, s, ctx, first, 1, 1, 1, "First Series", 2005, hierarchyEpisodeOpts{})
	hierarchyEpisode(t, s, ctx, second, 2, 1, 1, "Second Series", 2005, hierarchyEpisodeOpts{})

	moviesTotal, movies, err := s.ListMovies(ctx, HierarchyFilter{InstanceID: second})
	if err != nil {
		t.Fatalf("ListMovies instance %d: %v", second, err)
	}
	if moviesTotal != 0 || len(movies) != 0 {
		t.Fatalf("movies for instance %d = %d rows / total %d, want 0/0",
			second, len(movies), moviesTotal)
	}

	seriesTotal, series, err := s.ListSeries(ctx, HierarchyFilter{InstanceID: second})
	if err != nil {
		t.Fatalf("ListSeries instance %d: %v", second, err)
	}
	if seriesTotal != 1 || len(series) != 1 || series[0].Title != "Second Series" {
		t.Fatalf("series for instance %d = %+v / total %d, want Second Series",
			second, series, seriesTotal)
	}

	// Every-instance is the zero value.
	allTotal, allSeries, err := s.ListSeries(ctx, HierarchyFilter{})
	if err != nil {
		t.Fatalf("ListSeries all instances: %v", err)
	}
	if allTotal != 2 || len(allSeries) != 2 {
		t.Fatalf("all series = %d rows / total %d, want 2/2", len(allSeries), allTotal)
	}
}

// TestListSeriesSearchMatchesSubstring pins server-side search on the series
// title: a fragment of a name must find it, case-insensitively.
func TestListSeriesSearchMatchesSubstring(t *testing.T) {
	s, ctx, sonarr := hierarchyStore(t)
	hierarchyEpisode(t, s, ctx, sonarr, 1, 1, 1, "The Office", 2005, hierarchyEpisodeOpts{})
	hierarchyEpisode(t, s, ctx, sonarr, 2, 1, 1, "The Expanse", 2015, hierarchyEpisodeOpts{})

	total, rows, err := s.ListSeries(ctx, HierarchyFilter{Q: "offi"})
	if err != nil {
		t.Fatalf("ListSeries q=offi: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].Title != "The Office" {
		t.Fatalf("q=offi = %+v / total %d, want The Office", rows, total)
	}
}

// TestSeriesByIDReportsFound pins the header read and the missing case the
// handler turns into a 404.
func TestSeriesByIDReportsFound(t *testing.T) {
	s, ctx, sonarr := hierarchyStore(t)
	hierarchyEpisode(t, s, ctx, sonarr, 42, 1, 1, "Found Me", 2005, hierarchyEpisodeOpts{})

	row, found, err := s.SeriesByID(ctx, 42)
	if err != nil {
		t.Fatalf("SeriesByID(42): %v", err)
	}
	if !found || row.Title != "Found Me" || row.EpisodeCount != 1 {
		t.Fatalf("SeriesByID(42) = %+v found=%v, want Found Me with 1 episode", row, found)
	}

	if _, found, err := s.SeriesByID(ctx, 99); err != nil {
		t.Fatalf("SeriesByID(99): %v", err)
	} else if found {
		t.Fatal("SeriesByID(99) reported found")
	}
}

// TestHierarchyReadsDoNotWriteArrMirror pins that the whole read surface is
// read-only: the SQLite data version and the cache contents are unchanged.
func TestHierarchyReadsDoNotWriteArrMirror(t *testing.T) {
	s, ctx, sonarr := hierarchyStore(t)
	hierarchyMovie(t, s, ctx, sonarr, 1, "Read Only", "", 2001, "/media/x/1.mkv", "eligible")
	hierarchyEpisode(t, s, ctx, sonarr, 1, 1, 1, "Read Only Series", 2005, hierarchyEpisodeOpts{})

	var before int64
	if err := s.DB.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&before); err != nil {
		t.Fatalf("data_version before: %v", err)
	}
	var rowsBefore int64
	var newestBefore string
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MAX(updated_at), '') FROM arr_library_cache`).
		Scan(&rowsBefore, &newestBefore); err != nil {
		t.Fatalf("snapshot before: %v", err)
	}

	if _, _, err := s.ListMovies(ctx, HierarchyFilter{}); err != nil {
		t.Fatalf("ListMovies: %v", err)
	}
	if _, _, err := s.ListSeries(ctx, HierarchyFilter{}); err != nil {
		t.Fatalf("ListSeries: %v", err)
	}
	if _, _, err := s.SeriesByID(ctx, 1); err != nil {
		t.Fatalf("SeriesByID: %v", err)
	}
	if _, err := s.SeasonsForSeries(ctx, 1); err != nil {
		t.Fatalf("SeasonsForSeries: %v", err)
	}
	if _, err := s.EpisodesForSeason(ctx, 1, 1); err != nil {
		t.Fatalf("EpisodesForSeason: %v", err)
	}

	var after int64
	if err := s.DB.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&after); err != nil {
		t.Fatalf("data_version after: %v", err)
	}
	var rowsAfter int64
	var newestAfter string
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MAX(updated_at), '') FROM arr_library_cache`).
		Scan(&rowsAfter, &newestAfter); err != nil {
		t.Fatalf("snapshot after: %v", err)
	}
	if before != after {
		t.Fatalf("data_version changed %d -> %d: a read wrote the mirror", before, after)
	}
	if rowsBefore != rowsAfter || newestBefore != newestAfter {
		t.Fatalf("cache changed %d/%q -> %d/%q: a read wrote the mirror",
			rowsBefore, newestBefore, rowsAfter, newestAfter)
	}
}

// seedHierarchy bulk-inserts seriesCount series with episodesPerSeries episodes
// each in season 1, all eligible with a file. One transaction and one prepared
// statement keep the 12k fixture fast enough to run in tests.
func seedHierarchy(tb testing.TB, db *sql.DB, instanceID int64, seriesCount, episodesPerSeries int) {
	tb.Helper()
	tx, err := db.Begin()
	if err != nil {
		tb.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, series_id, title, original_title,
		  year, season_number, episode_number, episode_title, media_folder_path,
		  target_file_path, matches_target_tags, eligibility_status, video_codec, updated_at)
		 VALUES (?, 'episode', ?, ?, ?, '', 2005, 1, ?, ?, '/media/tv/seed',
		         '/media/tv/seed/e.mkv', 1, 'eligible', 'hevc', '2026-01-02 03:04:05')`)
	if err != nil {
		tb.Fatalf("prepare seed: %v", err)
	}
	defer stmt.Close()
	for series := 1; series <= seriesCount; series++ {
		for episode := 1; episode <= episodesPerSeries; episode++ {
			externalID := int64(series)*100000 + int64(episode)
			title := fmt.Sprintf("Seed Series %03d", series)
			episodeTitle := fmt.Sprintf("Episode %d", episode)
			if _, err := stmt.Exec(instanceID, externalID, int64(series), title,
				episode, episodeTitle); err != nil {
				tb.Fatalf("seed series %d episode %d: %v", series, episode, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit seed: %v", err)
	}
}
