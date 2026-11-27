// HTTP contract tests for the library hierarchy routes (#56): the paging
// validation, the exact resource shapes the SPA is written against, and the
// 12k-row budget that pins "series must stay fast".
package library

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/store"
)

// insertHierarchyEpisode seeds one Sonarr episode row for the hierarchy routes.
func (h *harness) insertHierarchyEpisode(t *testing.T, instanceID, seriesID int64,
	season, episode int, seriesTitle string, target, eligibility string) int64 {
	t.Helper()
	var targetPath any
	if target != "" {
		targetPath = target
	}
	externalID := seriesID*100000 + int64(season)*1000 + int64(episode)
	res, err := h.db.DB.ExecContext(context.Background(),
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, series_id, title, original_title,
		  year, season_number, episode_number, episode_title, media_folder_path,
		  target_file_path, matches_target_tags, eligibility_status, video_codec, updated_at)
		 VALUES (?, 'episode', ?, ?, ?, '', 2005, ?, ?, ?, '/media/tv/' || ?,
		         ?, 1, ?, 'hevc', '2026-01-02 03:04:05')`,
		instanceID, externalID, seriesID, seriesTitle, season, episode,
		fmt.Sprintf("Episode %d", episode), seriesTitle, targetPath, eligibility)
	if err != nil {
		t.Fatalf("insert episode %s S%02dE%02d: %v", seriesTitle, season, episode, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("episode id: %v", err)
	}
	return id
}

// insertHierarchyMovie seeds one Radarr movie row.
func (h *harness) insertHierarchyMovie(t *testing.T, instanceID, externalID int64,
	title, originalTitle string, year int) int64 {
	t.Helper()
	res, err := h.db.DB.ExecContext(context.Background(),
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, title, original_title, year,
		  media_folder_path, target_file_path, matches_target_tags, eligibility_status,
		  video_codec, updated_at)
		 VALUES (?, 'movie', ?, ?, ?, ?, '/media/movies/' || ?, '/media/movies/f.mkv',
		         1, 'eligible', 'hevc', '2026-01-02 03:04:05')`,
		instanceID, externalID, title, originalTitle, year, title)
	if err != nil {
		t.Fatalf("insert movie %q: %v", title, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("movie id: %v", err)
	}
	return id
}

// decodeObject decodes one JSON object body and fails the test on a bad body,
// so the shape tests can assert on the top-level keys exactly.
func decodeObject(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("decode body %s: %v", body, err)
	}
	return top
}

// assertKeys compares a decoded object's keys (sorted) against want.
func assertKeys(t *testing.T, what string, got map[string]json.RawMessage, want []string) {
	t.Helper()
	if keys := sortedKeys(got); !equalStrings(keys, want) {
		t.Fatalf("%s keys = %v, want %v", what, keys, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// decodeError reads the documented {error, code} envelope.
func decodeError(t *testing.T, body []byte) (string, string) {
	t.Helper()
	var payload struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode error body %s: %v", body, err)
	}
	return payload.Error, payload.Code
}

// TestMoviesRouteValidatesPaging pins the documented 400 codes: a page or page
// size below 1, a non-numeric value, and an unknown sort/dir.
// rawJSON re-encodes a decoded resource map so a test can decode it into a
// typed struct while still asserting the exact key set of the envelope.
func rawJSON(t *testing.T, m map[string]json.RawMessage) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-encode resource: %v", err)
	}
	return b
}

func TestMoviesRouteValidatesPaging(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		path string
		code string
	}{
		{"/api/library/movies?page=0", "invalid_page"},
		{"/api/library/movies?page=-1", "invalid_page"},
		{"/api/library/movies?page=abc", "invalid_page"},
		{"/api/library/movies?pageSize=0", "invalid_page"},
		{"/api/library/movies?pageSize=abc", "invalid_page"},
		{"/api/library/series?page=0", "invalid_page"},
		{"/api/library/movies?sort=bogus", "invalid_sort"},
		{"/api/library/series?dir=sideways", "invalid_sort"},
	}
	for _, tc := range cases {
		rec := h.get(tc.path)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("GET %s = %d, want 400 (body %s)", tc.path, rec.Code, rec.Body.String())
		}
		message, code := decodeError(t, rec.Body.Bytes())
		if code != tc.code {
			t.Fatalf("GET %s code = %q, want %q", tc.path, code, tc.code)
		}
		if message == "" {
			t.Fatalf("GET %s returned an empty error message", tc.path)
		}
	}
}

// TestMoviesRouteClampsPageSize pins that an oversized page size is clamped,
// never rejected and never a 500, and that the response echoes what was applied.
func TestMoviesRouteClampsPageSize(t *testing.T) {
	h := newHarness(t)
	instanceID := h.insertInstance(t, "Radarr", "radarr", true)
	for i := 0; i < 3; i++ {
		h.insertHierarchyMovie(t, instanceID, int64(100+i), fmt.Sprintf("Movie %d", i), "", 2001)
	}

	for _, raw := range []string{"100000", "501", "201"} {
		rec := h.get("/api/library/movies?pageSize=" + raw)
		if rec.Code != http.StatusOK {
			t.Fatalf("pageSize=%s = %d, body %s", raw, rec.Code, rec.Body.String())
		}
		var body struct {
			Page     int   `json:"page"`
			PageSize int   `json:"pageSize"`
			Total    int64 `json:"total"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode pageSize=%s: %v", raw, err)
		}
		if body.PageSize != store.LibraryPageMaxLimit {
			t.Fatalf("pageSize=%s echoed %d, want the cap %d",
				raw, body.PageSize, store.LibraryPageMaxLimit)
		}
		if body.Page != 1 || body.Total != 3 {
			t.Fatalf("pageSize=%s page/total = %d/%d, want 1/3", raw, body.Page, body.Total)
		}
	}
}

// TestMoviesRouteReturnsPagedResource pins the MovieResource shape field for
// field, plus the paging envelope keys.
func TestMoviesRouteReturnsPagedResource(t *testing.T) {
	h := newHarness(t)
	radarr := h.insertInstance(t, "Radarr", "radarr", true)
	id := h.insertHierarchyMovie(t, radarr, 12, "Spirited Away", "千と千尋の神隠し", 2001)

	rec := h.get("/api/library/movies")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/library/movies = %d, body %s", rec.Code, rec.Body.String())
	}
	top := decodeObject(t, rec.Body.Bytes())
	assertKeys(t, "movies page", top, []string{"items", "page", "pageSize", "total"})

	var items []map[string]json.RawMessage
	if err := json.Unmarshal(top["items"], &items); err != nil {
		t.Fatalf("decode items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	assertKeys(t, "movie resource", items[0], []string{
		"arr_id", "eligibility", "has_file", "id", "instance", "instance_id",
		"original_title", "path", "size_on_disk", "target_file_path", "title",
		"updated_at", "video_codec", "year",
	})

	var movie struct {
		ID            int64  `json:"id"`
		ArrID         int64  `json:"arr_id"`
		InstanceID    int64  `json:"instance_id"`
		Instance      string `json:"instance"`
		Title         string `json:"title"`
		OriginalTitle string `json:"original_title"`
		Year          int    `json:"year"`
		Path          string `json:"path"`
		TargetPath    string `json:"target_file_path"`
		HasFile       bool   `json:"has_file"`
		SizeOnDisk    int64  `json:"size_on_disk"`
		Eligibility   string `json:"eligibility"`
		VideoCodec    string `json:"video_codec"`
		UpdatedAt     string `json:"updated_at"`
	}
	if err := json.Unmarshal(rawJSON(t, items[0]), &movie); err != nil {
		t.Fatalf("decode movie: %v", err)
	}
	if movie.ID != id || movie.ArrID != 12 || movie.InstanceID != radarr || movie.Instance != "Radarr" {
		t.Errorf("identity = %+v, want id %d arr 12 instance %d/Radarr", movie, id, radarr)
	}
	if movie.Title != "Spirited Away" || movie.OriginalTitle != "千と千尋の神隠し" || movie.Year != 2001 {
		t.Errorf("titles/year = %q/%q/%d", movie.Title, movie.OriginalTitle, movie.Year)
	}
	if movie.Path != "/media/movies/Spirited Away" || movie.TargetPath != "/media/movies/f.mkv" {
		t.Errorf("paths = %q/%q", movie.Path, movie.TargetPath)
	}
	if !movie.HasFile || movie.SizeOnDisk != 0 || movie.Eligibility != "eligible" || movie.VideoCodec != "hevc" {
		t.Errorf("file/gate = has_file %v size %d %q %q",
			movie.HasFile, movie.SizeOnDisk, movie.Eligibility, movie.VideoCodec)
	}
	if movie.UpdatedAt == "" {
		t.Error("updated_at is empty")
	}
}

// TestSeriesRouteReturnsNestedStatistics pins the SeriesResource shape, the
// series header route, and SeasonResource.statistics.
func TestSeriesRouteReturnsNestedStatistics(t *testing.T) {
	h := newHarness(t)
	sonarr := h.insertInstance(t, "Sonarr", "sonarr", true)
	for episode := 1; episode <= 2; episode++ {
		h.insertHierarchyEpisode(t, sonarr, 7, 2, episode, "The Office",
			fmt.Sprintf("/media/tv/s02e%d.mkv", episode), "eligible")
	}
	h.insertHierarchyEpisode(t, sonarr, 7, 2, 3, "The Office", "", "waiting_for_tags")

	// Collection route.
	rec := h.get("/api/library/series")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/library/series = %d, body %s", rec.Code, rec.Body.String())
	}
	top := decodeObject(t, rec.Body.Bytes())
	assertKeys(t, "series page", top, []string{"items", "page", "pageSize", "total"})
	var seriesItems []map[string]json.RawMessage
	if err := json.Unmarshal(top["items"], &seriesItems); err != nil {
		t.Fatalf("decode series items: %v", err)
	}
	if len(seriesItems) != 1 {
		t.Fatalf("series items = %d, want 1", len(seriesItems))
	}
	assertKeys(t, "series resource", seriesItems[0], []string{
		"eligibility", "episode_count", "episode_file_count", "id", "instance",
		"instance_id", "path", "season_count", "title", "updated_at", "year",
	})

	var series struct {
		ID               int64  `json:"id"`
		InstanceID       int64  `json:"instance_id"`
		Instance         string `json:"instance"`
		Title            string `json:"title"`
		Year             int    `json:"year"`
		Path             string `json:"path"`
		SeasonCount      int    `json:"season_count"`
		EpisodeCount     int    `json:"episode_count"`
		EpisodeFileCount int    `json:"episode_file_count"`
		Eligibility      string `json:"eligibility"`
		UpdatedAt        string `json:"updated_at"`
	}
	if err := json.Unmarshal(rawJSON(t, seriesItems[0]), &series); err != nil {
		t.Fatalf("decode series: %v", err)
	}
	if series.ID != 7 || series.InstanceID != sonarr || series.Instance != "Sonarr" ||
		series.Title != "The Office" || series.Year != 2005 {
		t.Errorf("series identity = %+v", series)
	}
	if series.Path != "/media/tv/The Office" {
		t.Errorf("series path = %q", series.Path)
	}
	if series.SeasonCount != 1 || series.EpisodeCount != 3 || series.EpisodeFileCount != 2 {
		t.Errorf("series counts = %d seasons / %d episodes / %d files, want 1/3/2",
			series.SeasonCount, series.EpisodeCount, series.EpisodeFileCount)
	}
	if series.Eligibility != "waiting_for_tags" {
		t.Errorf("series eligibility = %q, want waiting_for_tags", series.Eligibility)
	}

	// Header route: the same SeriesResource, not an envelope.
	headerRec := h.get("/api/library/series/7")
	if headerRec.Code != http.StatusOK {
		t.Fatalf("GET /api/library/series/7 = %d, body %s", headerRec.Code, headerRec.Body.String())
	}
	assertKeys(t, "series header", decodeObject(t, headerRec.Body.Bytes()), []string{
		"eligibility", "episode_count", "episode_file_count", "id", "instance",
		"instance_id", "path", "season_count", "title", "updated_at", "year",
	})

	// Seasons route: {items: SeasonResource[]} with nested statistics.
	seasonsRec := h.get("/api/library/series/7/seasons")
	if seasonsRec.Code != http.StatusOK {
		t.Fatalf("GET seasons = %d, body %s", seasonsRec.Code, seasonsRec.Body.String())
	}
	seasonsTop := decodeObject(t, seasonsRec.Body.Bytes())
	assertKeys(t, "seasons page", seasonsTop, []string{"items"})
	var seasons []map[string]json.RawMessage
	if err := json.Unmarshal(seasonsTop["items"], &seasons); err != nil {
		t.Fatalf("decode seasons: %v", err)
	}
	if len(seasons) != 1 {
		t.Fatalf("seasons = %d, want 1", len(seasons))
	}
	assertKeys(t, "season resource", seasons[0], []string{"season_number", "statistics"})
	var statistics map[string]json.RawMessage
	if err := json.Unmarshal(seasons[0]["statistics"], &statistics); err != nil {
		t.Fatalf("decode statistics: %v", err)
	}
	assertKeys(t, "season statistics", statistics,
		[]string{"episode_count", "episode_file_count", "percent_of_episodes"})

	var season struct {
		SeasonNumber int `json:"season_number"`
		Statistics   struct {
			EpisodeCount     int     `json:"episode_count"`
			EpisodeFileCount int     `json:"episode_file_count"`
			PercentEpisodes  float64 `json:"percent_of_episodes"`
		} `json:"statistics"`
	}
	if err := json.Unmarshal(rawJSON(t, seasons[0]), &season); err != nil {
		t.Fatalf("decode season: %v", err)
	}
	if season.SeasonNumber != 2 || season.Statistics.EpisodeCount != 3 ||
		season.Statistics.EpisodeFileCount != 2 {
		t.Fatalf("season = %+v, want season 2 with 3 episodes / 2 files", season)
	}
	if season.Statistics.PercentEpisodes < 66.6 || season.Statistics.PercentEpisodes > 66.7 {
		t.Fatalf("percent_of_episodes = %v, want 100*2/3", season.Statistics.PercentEpisodes)
	}
}

// TestSeasonEpisodesRouteShape pins the episode envelope and EpisodeResource.
func TestSeasonEpisodesRouteShape(t *testing.T) {
	h := newHarness(t)
	sonarr := h.insertInstance(t, "Sonarr", "sonarr", true)
	id := h.insertHierarchyEpisode(t, sonarr, 7, 2, 3, "The Office",
		"/media/tv/Season 02/The Office - S02E03.mkv", "eligible")
	h.insertHierarchyEpisode(t, sonarr, 7, 2, 4, "The Office", "", "missing_target")

	rec := h.get("/api/library/series/7/seasons/2/episodes")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET episodes = %d, body %s", rec.Code, rec.Body.String())
	}
	top := decodeObject(t, rec.Body.Bytes())
	assertKeys(t, "episodes page", top,
		[]string{"items", "seasonNumber", "total", "truncated"})

	var items []map[string]json.RawMessage
	if err := json.Unmarshal(top["items"], &items); err != nil {
		t.Fatalf("decode episodes: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("episodes = %d, want 2", len(items))
	}
	assertKeys(t, "episode resource", items[0], []string{
		"arr_id", "eligibility", "episode_number", "has_file", "id", "season_number",
		"series_id", "target_file_path", "title",
	})

	var episode struct {
		ID            int64  `json:"id"`
		SeriesID      int64  `json:"series_id"`
		ArrID         int64  `json:"arr_id"`
		SeasonNumber  int    `json:"season_number"`
		EpisodeNumber int    `json:"episode_number"`
		Title         string `json:"title"`
		HasFile       bool   `json:"has_file"`
		TargetPath    string `json:"target_file_path"`
		Eligibility   string `json:"eligibility"`
	}
	if err := json.Unmarshal(rawJSON(t, items[0]), &episode); err != nil {
		t.Fatalf("decode episode: %v", err)
	}
	if episode.ID != id || episode.SeriesID != 7 || episode.ArrID != 7*100000+2000+3 {
		t.Errorf("episode identity = %+v", episode)
	}
	if episode.SeasonNumber != 2 || episode.EpisodeNumber != 3 || episode.Title != "Episode 3" {
		t.Errorf("episode numbers/title = %+v", episode)
	}
	if !episode.HasFile || episode.TargetPath == "" || episode.Eligibility != "eligible" {
		t.Errorf("episode file/gate = %+v", episode)
	}

	var envelope struct {
		SeasonNumber int   `json:"seasonNumber"`
		Total        int64 `json:"total"`
		Truncated    bool  `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.SeasonNumber != 2 || envelope.Total != 2 || envelope.Truncated {
		t.Fatalf("envelope = %+v, want season 2, total 2, not truncated", envelope)
	}
}

// TestHierarchyRoutesReturn404ForUnknownSeries pins 404 not_found for the three
// routes that name a series.
func TestHierarchyRoutesReturn404ForUnknownSeries(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/api/library/series/999",
		"/api/library/series/999/seasons",
		"/api/library/series/999/seasons/1/episodes",
		"/api/library/series/not-a-number",
	} {
		rec := h.get(path)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404 (body %s)", path, rec.Code, rec.Body.String())
		}
		if _, code := decodeError(t, rec.Body.Bytes()); code != "not_found" {
			t.Fatalf("GET %s code = %q, want not_found", path, code)
		}
	}
}

// TestSeasonEpisodesRouteEmptySeasonIsEmptyPage pins that a known series with
// no rows in that season answers an empty page, not a 404: only an unknown
// series is not_found.
func TestSeasonEpisodesRouteEmptySeasonIsEmptyPage(t *testing.T) {
	h := newHarness(t)
	sonarr := h.insertInstance(t, "Sonarr", "sonarr", true)
	h.insertHierarchyEpisode(t, sonarr, 7, 1, 1, "The Office", "/media/tv/e1.mkv", "eligible")

	rec := h.get("/api/library/series/7/seasons/9/episodes")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET empty season = %d, body %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		SeasonNumber int               `json:"seasonNumber"`
		Total        int64             `json:"total"`
		Truncated    bool              `json:"truncated"`
		Items        []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode empty season: %v", err)
	}
	if envelope.SeasonNumber != 9 || envelope.Total != 0 || len(envelope.Items) != 0 {
		t.Fatalf("empty season = %+v, want season 9 with no rows", envelope)
	}
	if envelope.Items == nil {
		t.Fatal("items is null, want [] so the UI can map without a guard")
	}
}

// TestEpisodeRouteValidatesPaging pins page/pageSize validation on the episode
// route, where the default is 200 and the cap 500.
func TestEpisodeRouteValidatesPaging(t *testing.T) {
	h := newHarness(t)
	sonarr := h.insertInstance(t, "Sonarr", "sonarr", true)
	h.insertHierarchyEpisode(t, sonarr, 7, 1, 1, "The Office", "/media/tv/e1.mkv", "eligible")

	if rec := h.get("/api/library/series/7/seasons/1/episodes?page=0"); rec.Code != http.StatusBadRequest {
		t.Fatalf("page=0 = %d, want 400", rec.Code)
	}
	if rec := h.get("/api/library/series/7/seasons/1/episodes?pageSize=0"); rec.Code != http.StatusBadRequest {
		t.Fatalf("pageSize=0 = %d, want 400", rec.Code)
	}
	rec := h.get("/api/library/series/7/seasons/1/episodes?pageSize=100000")
	if rec.Code != http.StatusOK {
		t.Fatalf("pageSize=100000 = %d, want 200 (clamped)", rec.Code)
	}
	var envelope struct {
		Total int64 `json:"total"`
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode clamped page: %v", err)
	}
	if envelope.Total != 1 || len(envelope.Items) != 1 {
		t.Fatalf("clamped page = %+v, want one episode", envelope)
	}
}

// TestLibrarySeasonMaxRowsSetsTruncated pins the cap: a season above
// LibrarySeasonMaxRows is reported truncated with its real total, never
// silently shortened.
func TestLibrarySeasonMaxRowsSetsTruncated(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 2001 rows")
	}
	h := newHarness(t)
	sonarr := h.insertInstance(t, "Sonarr", "sonarr", true)
	seedHierarchyRows(t, h, sonarr, 1, store.LibrarySeasonMaxRows+1)

	rec := h.get("/api/library/series/1/seasons/1/episodes?pageSize=500")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET truncated season = %d, body %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		SeasonNumber int               `json:"seasonNumber"`
		Total        int64             `json:"total"`
		Truncated    bool              `json:"truncated"`
		Items        []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode truncated season: %v", err)
	}
	if envelope.Total != int64(store.LibrarySeasonMaxRows+1) {
		t.Fatalf("total = %d, want %d", envelope.Total, store.LibrarySeasonMaxRows+1)
	}
	if !envelope.Truncated {
		t.Fatal("truncated = false, want true above the season cap")
	}
	if len(envelope.Items) != store.LibraryEpisodePageMaxLimit {
		t.Fatalf("items = %d, want the episode page cap %d",
			len(envelope.Items), store.LibraryEpisodePageMaxLimit)
	}
}

// TestLegacyItemsRouteStillServesFlatShape pins that #56 left /api/library/items
// exactly as it was: same envelope keys, same flat item keys.
func TestLegacyItemsRouteStillServesFlatShape(t *testing.T) {
	h := newHarness(t)
	instanceID := h.insertInstance(t, "Radarr", "radarr", true)
	h.insertHierarchyMovie(t, instanceID, 1, "Shrek", "Shrek", 2001)

	rec := h.get("/api/library/items")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/library/items = %d, body %s", rec.Code, rec.Body.String())
	}
	top := decodeObject(t, rec.Body.Bytes())
	assertKeys(t, "legacy items page", top, []string{"items", "limit", "offset", "total"})
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(top["items"], &items); err != nil {
		t.Fatalf("decode legacy items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("legacy items = %d, want 1", len(items))
	}
	assertKeys(t, "legacy item", items[0], []string{
		"eligibility_status", "episode", "id", "instance_id", "instance_name",
		"kind", "last_seen_at", "matches_target_tags", "media_type",
		"root_folder_path", "season", "target_file_path", "title", "updated_at", "year",
	})
}

// seedHierarchyRows bulk-inserts seriesCount series with episodesPerSeries
// episodes each in season 1. One transaction keeps 12k rows cheap.
func seedHierarchyRows(tb testing.TB, h *harness, instanceID int64, seriesCount, episodesPerSeries int) {
	tb.Helper()
	tx, err := h.db.DB.Begin()
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

// BenchmarkListSeries12k pins the series aggregate read over a 12 000-episode
// library in 400 series — the shape the acceptance criterion names.
func BenchmarkListSeries12k(b *testing.B) {
	st, err := store.Open(b.TempDir())
	if err != nil {
		b.Fatalf("store.Open: %v", err)
	}
	b.Cleanup(func() { _ = st.Close() })
	h := &harness{t: nil, db: st}
	instanceID := int64(0)
	res, err := st.DB.Exec(
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		 VALUES ('Sonarr', 'sonarr', 'http://arr.invalid', x'00', '0000')`)
	if err != nil {
		b.Fatalf("insert instance: %v", err)
	}
	if instanceID, err = res.LastInsertId(); err != nil {
		b.Fatalf("instance id: %v", err)
	}
	seedHierarchyRows(b, h, instanceID, 400, 30)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		total, rows, err := st.ListSeries(context.Background(),
			store.HierarchyFilter{Page: 1, PageSize: store.LibraryPageMaxLimit})
		if err != nil {
			b.Fatalf("ListSeries: %v", err)
		}
		if total != 400 || len(rows) != store.LibraryPageMaxLimit {
			b.Fatalf("total/rows = %d/%d, want 400/%d",
				total, len(rows), store.LibraryPageMaxLimit)
		}
	}
}

// TestLibraryHierarchy12kUnder100ms is the acceptance guard: a series page over
// a 12 000-episode library answers in under 100 ms at p95.
func TestLibraryHierarchy12kUnder100ms(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 12 000 rows")
	}
	h := newHarness(t)
	sonarr := h.insertInstance(t, "Sonarr", "sonarr", true)
	seedHierarchyRows(t, h, sonarr, 400, 30)

	// One warm-up request so the timing measures the query, not first-page
	// decoding of the mux.
	if rec := h.get("/api/library/series?pageSize=200"); rec.Code != http.StatusOK {
		t.Fatalf("warm-up GET = %d, body %s", rec.Code, rec.Body.String())
	}

	const samples = 30
	durations := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		start := time.Now()
		rec := h.get("/api/library/series?pageSize=200&page=1")
		durations = append(durations, time.Since(start))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET = %d, body %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Total int64 `json:"total"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Total != 400 {
			t.Fatalf("total = %d, want 400 grouped series", body.Total)
		}
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95 := durations[(len(durations)*95)/100-1]
	if p95 > 100*time.Millisecond {
		t.Fatalf("p95 = %v over %d requests, want < 100ms", p95, samples)
	}
}
