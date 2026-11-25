package library

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arr/arrmock"
	"github.com/Subject412/dubarr/backend/internal/arrsync"
	"github.com/Subject412/dubarr/backend/internal/secrets"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// rescanEnv is a library API wired with a real secret store, so the rescan route
// decrypts a genuine AES-256-GCM blob exactly as production does.
type rescanEnv struct {
	t        *testing.T
	mux      *http.ServeMux
	db       *store.Store
	settings *settings.Store
	cipher   *secrets.Cipher
}

func newRescanEnv(t *testing.T, withSecrets bool) *rescanEnv {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	key := make([]byte, secrets.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("random key: %v", err)
	}
	cipher, err := secrets.NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}

	deps := Dependencies{
		DB:           st.DB,
		Settings:     settings.New(st.DB),
		RequireAdmin: func(h http.Handler) http.Handler { return h },
	}
	if withSecrets {
		deps.Secrets = cipher
	}
	mux := http.NewServeMux()
	RegisterRoutes(mux, deps)
	return &rescanEnv{t: t, mux: mux, db: st, settings: deps.Settings, cipher: cipher}
}

// instance seeds one arr_instances row with an encrypted key and returns its id.
func (e *rescanEnv) instance(kind, baseURL, apiKey string, enabled bool) int64 {
	e.t.Helper()
	enc, err := e.cipher.EncryptString(apiKey)
	if err != nil {
		e.t.Fatalf("EncryptString: %v", err)
	}
	flag := 0
	if enabled {
		flag = 1
	}
	res, err := e.db.DB.ExecContext(context.Background(),
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4, enabled)
		 VALUES (?, ?, ?, ?, '0000', ?)`, kind, kind, baseURL, enc, flag)
	if err != nil {
		e.t.Fatalf("insert arr instance: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		e.t.Fatalf("arr instance id: %v", err)
	}
	return id
}

// movieRow seeds one movie cache row as a sync pass would have written it, and
// returns its id.
func (e *rescanEnv) movieRow(instanceID, externalID int64, title string) int64 {
	e.t.Helper()
	res, err := e.db.DB.ExecContext(context.Background(),
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, title, year,
		  media_folder_path, target_file_path, matches_target_tags, eligibility_status)
		 VALUES (?, 'movie', ?, ?, 2001, '/media/movies/old', NULL, 0, 'missing_target')`,
		instanceID, externalID, title)
	if err != nil {
		e.t.Fatalf("insert movie row: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		e.t.Fatalf("movie row id: %v", err)
	}
	return id
}

// episodeRow seeds one episode cache row as a sync pass would have written it,
// and returns its id.
func (e *rescanEnv) episodeRow(instanceID, seriesID, episodeID int64, title, episodeTitle string) int64 {
	e.t.Helper()
	res, err := e.db.DB.ExecContext(context.Background(),
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, series_id, title, original_title,
		  year, season_number, episode_number, episode_title, media_folder_path,
		  target_file_path, matches_target_tags, eligibility_status)
		 VALUES (?, 'episode', ?, ?, ?, ?, 2015, 1, 1, ?, '/media/tv/old', NULL, 0, 'missing_target')`,
		instanceID, episodeID, seriesID, title, title, episodeTitle)
	if err != nil {
		e.t.Fatalf("insert episode row: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		e.t.Fatalf("episode row id: %v", err)
	}
	return id
}

// freezeCacheTimestamps parks every content stamp in the past, so a rewrite is
// visible even when it happens within the same second as the read.
func (e *rescanEnv) freezeCacheTimestamps() {
	e.t.Helper()
	if _, err := e.db.DB.ExecContext(context.Background(),
		`UPDATE arr_library_cache
		 SET updated_at = '2000-01-01 00:00:00', last_seen_at = '2000-01-01 00:00:00'`); err != nil {
		e.t.Fatalf("freeze cache timestamps: %v", err)
	}
}

// cacheState is one cache row as the episode tests compare it. The struct is
// comparable on purpose: an untouched row must survive a failed re-read
// byte-for-byte.
type cacheState struct {
	RowID        int64
	Title        string
	EpisodeTitle string
	Target       string
	UpdatedAt    string
}

// cacheStates reads every cache row keyed by external_item_id.
func (e *rescanEnv) cacheStates() map[int64]cacheState {
	e.t.Helper()
	rows, err := e.db.DB.QueryContext(context.Background(),
		`SELECT id, external_item_id, title, COALESCE(episode_title, ''),
		        COALESCE(target_file_path, ''), COALESCE(updated_at, '')
		 FROM arr_library_cache`)
	if err != nil {
		e.t.Fatalf("read cache rows: %v", err)
	}
	defer rows.Close()

	out := map[int64]cacheState{}
	for rows.Next() {
		var (
			state      cacheState
			externalID int64
		)
		if err := rows.Scan(&state.RowID, &externalID, &state.Title, &state.EpisodeTitle,
			&state.Target, &state.UpdatedAt); err != nil {
			e.t.Fatalf("scan cache row: %v", err)
		}
		out[externalID] = state
	}
	if err := rows.Err(); err != nil {
		e.t.Fatalf("iterate cache rows: %v", err)
	}
	return out
}

func (e *rescanEnv) post(path string) *httptest.ResponseRecorder {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
	return rec
}

type rescanBody struct {
	Accepted    bool   `json:"accepted"`
	Reread      string `json:"reread"`
	RereadError string `json:"reread_error"`
	Error       string `json:"error"`
	Code        string `json:"code"`
}

func decodeRescan(t *testing.T, rec *httptest.ResponseRecorder) rescanBody {
	t.Helper()
	var body rescanBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return body
}

// radarrFixture is a fake Radarr instance holding movie 7 with an imported file
// and one arr tag.
func radarrFixture(t *testing.T, apiKey string) *arrmock.Server {
	t.Helper()
	srv := arrmock.New(arrmock.Radarr, apiKey, arrmock.Fixtures{
		Movies: []arrmock.Movie{{
			ID: 7, Title: "Shrek", OriginalTitle: "Shrek", Year: 2001, TmdbID: 808,
			Path: "/media/movies/Shrek (2001)", HasFile: true,
			Tags:             []int{4},
			OriginalLanguage: arrmock.Language{ID: 1, Name: "English"},
			MovieFile: &arrmock.MovieFile{
				Path: "/media/movies/Shrek (2001)/Shrek.2001.mkv",
			},
		}},
		RootFolders: []arrmock.RootFolder{{ID: 1, Path: "/media/movies"}},
		Tags:        []arrmock.Tag{{ID: 4, Label: "PL.DUB"}},
	})
	t.Cleanup(srv.Close)
	return srv
}

// TestLibraryRescanAcceptedAndReread is the happy path: the command is posted
// and the row is refreshed from arr within the same request.
func TestLibraryRescanAcceptedAndReread(t *testing.T) {
	env := newRescanEnv(t, true)
	srv := radarrFixture(t, "radarr-rescan-key")
	instanceID := env.instance("radarr", srv.URL, srv.APIKey(), true)
	rowID := env.movieRow(instanceID, 7, "Shrek (stale)")

	rec := env.post("/api/library/items/" + itoa(rowID) + "/rescan")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST rescan = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeRescan(t, rec)
	if !body.Accepted || body.Reread != "ok" {
		t.Fatalf("body = %+v, want accepted + reread ok", body)
	}
	if body.RereadError != "" {
		t.Errorf("reread_error = %q, want empty on success", body.RereadError)
	}

	cmds := srv.Commands()
	if len(cmds) != 1 || cmds[0].Name != "RescanMovie" || cmds[0].MovieID != 7 {
		t.Fatalf("commands = %+v, want one RescanMovie for movie 7", cmds)
	}

	item, err := env.db.LibraryItemByID(context.Background(), rowID)
	if err != nil {
		t.Fatalf("LibraryItemByID: %v", err)
	}
	if item.Title != "Shrek" {
		t.Errorf("title = %q, want the re-read title", item.Title)
	}
	if item.TargetFilePath != "/media/movies/Shrek (2001)/Shrek.2001.mkv" {
		t.Errorf("target_file_path = %q, want the file arr now reports", item.TargetFilePath)
	}
	if item.RootFolderPath != "/media/movies" {
		t.Errorf("root_folder_path = %q, want /media/movies", item.RootFolderPath)
	}
	if item.EligibilityStatus != "eligible" || !item.MatchesTargetTags {
		t.Errorf("eligibility = %q matches=%v, want eligible/true",
			item.EligibilityStatus, item.MatchesTargetTags)
	}
}

// TestLibraryRescanRereadFailureStillAccepted pins the honest-answer rule: the
// accepted command is reported as such even when the re-read fails, and the row
// keeps its previous content.
func TestLibraryRescanRereadFailureStillAccepted(t *testing.T) {
	env := newRescanEnv(t, true)
	var sawRead bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v3/command" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
		case strings.HasPrefix(r.URL.Path, "/api/v3/movie/"):
			sawRead = true
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	instanceID := env.instance("radarr", srv.URL, "radarr-rescan-key", true)
	rowID := env.movieRow(instanceID, 7, "Shrek (stale)")

	rec := env.post("/api/library/items/" + itoa(rowID) + "/rescan")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST rescan = %d, want 202 even when the re-read fails (body %s)",
			rec.Code, rec.Body.String())
	}
	body := decodeRescan(t, rec)
	if !body.Accepted {
		t.Error("accepted = false, want true: the command was queued")
	}
	if body.Reread != "failed" {
		t.Fatalf("reread = %q, want failed", body.Reread)
	}
	if body.RereadError == "" {
		t.Fatal("reread_error is empty, want the scrubbed cause")
	}
	if strings.Contains(body.RereadError, "radarr-rescan-key") {
		t.Fatalf("reread_error %q echoes the API key", body.RereadError)
	}
	if !strings.Contains(body.RereadError, "500") {
		t.Errorf("reread_error = %q, want the HTTP status in the message", body.RereadError)
	}
	if !sawRead {
		t.Error("the route never attempted the re-read")
	}

	item, err := env.db.LibraryItemByID(context.Background(), rowID)
	if err != nil {
		t.Fatalf("LibraryItemByID: %v", err)
	}
	if item.Title != "Shrek (stale)" {
		t.Errorf("title = %q, want the untouched previous row", item.Title)
	}
	if item.EligibilityStatus != "missing_target" || item.TargetFilePath != "" {
		t.Errorf("row changed on a failed re-read: %+v", item)
	}
}

// TestLibraryRescanCommandFailureIsBadGateway keeps a refused command a 502 with
// a diagnostic that cannot carry the API key, and proves the re-read is not
// attempted after it.
func TestLibraryRescanCommandFailureIsBadGateway(t *testing.T) {
	env := newRescanEnv(t, true)
	const apiKey = "super-secret-rescan-key"
	var reads []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v3/movie") {
			reads = append(reads, r.URL.Path)
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	instanceID := env.instance("radarr", srv.URL, apiKey, true)
	rowID := env.movieRow(instanceID, 7, "Shrek")

	rec := env.post("/api/library/items/" + itoa(rowID) + "/rescan")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("POST rescan = %d, want 502 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeRescan(t, rec)
	if body.Code != "rescan_failed" {
		t.Errorf("code = %q, want rescan_failed", body.Code)
	}
	if body.Error == "" {
		t.Error("error is empty, want an operator-facing message")
	}
	if strings.Contains(rec.Body.String(), apiKey) {
		t.Fatalf("response body %q leaks the API key", rec.Body.String())
	}
	if len(reads) != 0 {
		t.Errorf("the re-read was attempted after a failed command: %v", reads)
	}
}

// TestLibraryRescanUnknownItemIsNotFound covers both id problems: a real id the
// cache no longer holds is a 404, a non-numeric id is a 400.
func TestLibraryRescanUnknownItemIsNotFound(t *testing.T) {
	env := newRescanEnv(t, true)

	rec := env.post("/api/library/items/999/rescan")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}

	for _, path := range []string{
		"/api/library/items/abc/rescan",
		"/api/library/items/0/rescan",
		"/api/library/items/-3/rescan",
	} {
		if rec := env.post(path); rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s = %d, want 400 (body %s)", path, rec.Code, rec.Body.String())
		}
	}
}

// TestLibraryRescanNotConfiguredIsConflict covers the rows that exist but cannot
// be rescanned: a disabled instance, and an API that has no secret store.
func TestLibraryRescanNotConfiguredIsConflict(t *testing.T) {
	t.Run("disabled instance", func(t *testing.T) {
		env := newRescanEnv(t, true)
		instanceID := env.instance("radarr", "http://radarr.invalid", "key", false)
		rowID := env.movieRow(instanceID, 7, "Shrek")

		rec := env.post("/api/library/items/" + itoa(rowID) + "/rescan")
		if rec.Code != http.StatusConflict {
			t.Fatalf("disabled instance = %d, want 409 (body %s)", rec.Code, rec.Body.String())
		}
		if body := decodeRescan(t, rec); body.Code != "not_configured" {
			t.Errorf("code = %q, want not_configured", body.Code)
		}
	})

	t.Run("no secret store", func(t *testing.T) {
		env := newRescanEnv(t, false)
		instanceID := env.instance("radarr", "http://radarr.invalid", "key", true)
		rowID := env.movieRow(instanceID, 7, "Shrek")

		rec := env.post("/api/library/items/" + itoa(rowID) + "/rescan")
		if rec.Code != http.StatusConflict {
			t.Fatalf("no secret store = %d, want 409 (body %s)", rec.Code, rec.Body.String())
		}
		if body := decodeRescan(t, rec); body.Code != "not_configured" {
			t.Errorf("code = %q, want not_configured", body.Code)
		}
	})
}

// TestLibraryRescanRequiresAdmin proves a missing admin wrapper answers 503
// instead of rescanning unauthenticated.
func TestLibraryRescanRequiresAdmin(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	mux := http.NewServeMux()
	RegisterRoutes(mux, Dependencies{DB: st.DB})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/library/items/1/rescan", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("rescan with nil RequireAdmin = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if _, ok := body["error"]; !ok {
		t.Errorf("body %s has no error key", rec.Body.String())
	}
}

// itoa formats a row id for a request path.
func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// sonarrFixture is a fake Sonarr holding TWO series with two episodes each, so
// a rescan has to prove it targets the right series and re-reads the right
// episode rather than just "some episode".
func sonarrFixture(t *testing.T, apiKey string) *arrmock.Server {
	t.Helper()
	srv := arrmock.New(arrmock.Sonarr, apiKey, arrmock.Fixtures{
		Series: []arrmock.Series{
			{
				ID: 3, Title: "The Expanse", OriginalTitle: "The Expanse", Year: 2015, TvdbID: 280619,
				Path: "/media/tv/The Expanse", Tags: []int{2},
				OriginalLanguage: arrmock.Language{ID: 2, Name: "English"},
			},
			{
				ID: 5, Title: "Dark", Year: 2017, TvdbID: 333333,
				Path:             "/media/tv/Dark",
				OriginalLanguage: arrmock.Language{ID: 3, Name: "German"},
			},
		},
		Episodes: []arrmock.Episode{
			{
				ID: 42, SeriesID: 3, SeasonNumber: 1, EpisodeNumber: 1, Title: "Dulcinea", HasFile: true,
				EpisodeFile: &arrmock.EpisodeFile{
					ID: 7, SeriesID: 3, SeasonNumber: 1,
					Path: "/media/tv/The Expanse/Season 01/S01E01.mkv",
				},
			},
			{
				ID: 43, SeriesID: 3, SeasonNumber: 1, EpisodeNumber: 2, Title: "The Big Empty", HasFile: true,
				EpisodeFile: &arrmock.EpisodeFile{
					ID: 8, SeriesID: 3, SeasonNumber: 1,
					Path: "/media/tv/The Expanse/Season 01/S01E02.mkv",
				},
			},
			{
				ID: 51, SeriesID: 5, SeasonNumber: 1, EpisodeNumber: 1, Title: "Secrets", HasFile: true,
				EpisodeFile: &arrmock.EpisodeFile{
					ID: 9, SeriesID: 5, SeasonNumber: 1,
					Path: "/media/tv/Dark/Season 01/S01E01.mkv",
				},
			},
			{
				ID: 52, SeriesID: 5, SeasonNumber: 1, EpisodeNumber: 2, Title: "Lies", HasFile: true,
				EpisodeFile: &arrmock.EpisodeFile{
					ID: 10, SeriesID: 5, SeasonNumber: 1,
					Path: "/media/tv/Dark/Season 01/S01E02.mkv",
				},
			},
		},
		RootFolders: []arrmock.RootFolder{{ID: 1, Path: "/media/tv"}},
		Tags:        []arrmock.Tag{{ID: 2, Label: "PL.DUB"}},
	})
	t.Cleanup(srv.Close)
	return srv
}

// TestLibraryRescanEpisodeTargetsTheRightSeriesAndEpisode is the episode
// coverage: the command names the episode's series, the re-read rewrites exactly
// that episode's row, and every sibling row (same series and other series) is
// left alone.
func TestLibraryRescanEpisodeTargetsTheRightSeriesAndEpisode(t *testing.T) {
	env := newRescanEnv(t, true)
	srv := sonarrFixture(t, "sonarr-rescan-key")
	instanceID := env.instance("sonarr", srv.URL, srv.APIKey(), true)

	env.episodeRow(instanceID, 3, 42, "The Expanse", "Dulcinea (stale)")
	rowID := env.episodeRow(instanceID, 3, 43, "The Expanse", "The Big Empty (stale)")
	env.episodeRow(instanceID, 5, 51, "Dark", "Secrets (stale)")
	env.episodeRow(instanceID, 5, 52, "Dark", "Lies (stale)")

	env.freezeCacheTimestamps()
	before := env.cacheStates()

	rec := env.post("/api/library/items/" + itoa(rowID) + "/rescan")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST rescan = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	if body := decodeRescan(t, rec); !body.Accepted || body.Reread != "ok" {
		t.Fatalf("body = %+v, want accepted + reread ok", body)
	}

	cmds := srv.Commands()
	if len(cmds) != 1 || cmds[0].Name != "RescanSeries" || cmds[0].SeriesID != 3 {
		t.Fatalf("commands = %+v, want one RescanSeries for series 3", cmds)
	}

	after := env.cacheStates()
	target, ok := after[43]
	if !ok {
		t.Fatal("episode 43 row disappeared")
	}
	if target.RowID != rowID {
		t.Errorf("row id = %d, want %d (the re-read replaced the row)", target.RowID, rowID)
	}
	if target.Title != "The Expanse" || target.EpisodeTitle != "The Big Empty" {
		t.Errorf("episode 43 = %+v, want the re-read title/episode title", target)
	}
	if target.Target != "/media/tv/The Expanse/Season 01/S01E02.mkv" {
		t.Errorf("episode 43 target = %q, want the file arr now reports", target.Target)
	}
	if target.UpdatedAt == before[43].UpdatedAt {
		t.Error("the target episode's updated_at did not move: the re-read wrote nothing")
	}

	for _, id := range []int64{42, 51, 52} {
		if after[id] != before[id] {
			t.Errorf("episode %d changed by a rescan of episode 43: %+v -> %+v",
				id, before[id], after[id])
		}
	}
	if len(after) != 4 {
		t.Errorf("rows = %d, want 4: the re-read touched the row set", len(after))
	}
}

// TestLibraryRescanEpisodeNoLongerListedFailsReread covers the missing-match
// branch: the series still exists but no longer reports the episode. The rescan
// itself succeeded, so the route still answers 202, reports the re-read as
// failed, and leaves every row exactly as it was.
func TestLibraryRescanEpisodeNoLongerListedFailsReread(t *testing.T) {
	env := newRescanEnv(t, true)
	srv := sonarrFixture(t, "sonarr-rescan-key")
	instanceID := env.instance("sonarr", srv.URL, srv.APIKey(), true)

	env.episodeRow(instanceID, 3, 42, "The Expanse", "Dulcinea")
	rowID := env.episodeRow(instanceID, 3, 999, "The Expanse", "Deleted Episode")

	env.freezeCacheTimestamps()
	before := env.cacheStates()

	rec := env.post("/api/library/items/" + itoa(rowID) + "/rescan")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST rescan = %d, want 202 even when the episode is gone (body %s)",
			rec.Code, rec.Body.String())
	}
	body := decodeRescan(t, rec)
	if !body.Accepted {
		t.Error("accepted = false, want true: the series rescan was queued")
	}
	if body.Reread != "failed" {
		t.Fatalf("reread = %q, want failed", body.Reread)
	}
	if !strings.Contains(body.RereadError, "no longer listed") {
		t.Errorf("reread_error = %q, want the missing-episode cause", body.RereadError)
	}

	cmds := srv.Commands()
	if len(cmds) != 1 || cmds[0].Name != "RescanSeries" || cmds[0].SeriesID != 3 {
		t.Fatalf("commands = %+v, want one RescanSeries for series 3", cmds)
	}

	after := env.cacheStates()
	if len(after) != len(before) {
		t.Fatalf("rows = %d, want %d left untouched", len(after), len(before))
	}
	for id, state := range before {
		if after[id] != state {
			t.Errorf("episode %d changed on a failed re-read: %+v -> %+v", id, state, after[id])
		}
	}
}

// TestLibraryRescanGoneFromArrIsPlainLanguage pins the client-facing wording: a
// cache row arr no longer holds reports a sentence an operator can act on, not
// the client's "unexpected HTTP status 404 from GET ..." detail, and the row
// keeps its previous content.
func TestLibraryRescanGoneFromArrIsPlainLanguage(t *testing.T) {
	const want = "the item is no longer in arr"

	t.Run("movie", func(t *testing.T) {
		env := newRescanEnv(t, true)
		srv := radarrFixture(t, "radarr-rescan-key") // holds movie 7 only
		instanceID := env.instance("radarr", srv.URL, srv.APIKey(), true)
		rowID := env.movieRow(instanceID, 42, "Shrek (stale)")

		rec := env.post("/api/library/items/" + itoa(rowID) + "/rescan")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("POST rescan = %d, want 202 (body %s)", rec.Code, rec.Body.String())
		}
		body := decodeRescan(t, rec)
		if body.Reread != "failed" {
			t.Fatalf("reread = %q, want failed", body.Reread)
		}
		if body.RereadError != want {
			t.Errorf("reread_error = %q, want %q", body.RereadError, want)
		}
		if strings.Contains(body.RereadError, "unexpected HTTP status") {
			t.Errorf("reread_error %q still carries the raw client status", body.RereadError)
		}

		item, err := env.db.LibraryItemByID(context.Background(), rowID)
		if err != nil {
			t.Fatalf("LibraryItemByID: %v", err)
		}
		if item.Title != "Shrek (stale)" || item.EligibilityStatus != "missing_target" {
			t.Errorf("row changed on a failed re-read: %+v", item)
		}
	})

	t.Run("episode", func(t *testing.T) {
		env := newRescanEnv(t, true)
		srv := sonarrFixture(t, "sonarr-rescan-key") // holds series 3 and 5
		instanceID := env.instance("sonarr", srv.URL, srv.APIKey(), true)
		rowID := env.episodeRow(instanceID, 99, 900, "Gone", "Gone S01E01")

		rec := env.post("/api/library/items/" + itoa(rowID) + "/rescan")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("POST rescan = %d, want 202 (body %s)", rec.Code, rec.Body.String())
		}
		body := decodeRescan(t, rec)
		if body.Reread != "failed" {
			t.Fatalf("reread = %q, want failed", body.Reread)
		}
		if body.RereadError != want {
			t.Errorf("reread_error = %q, want %q", body.RereadError, want)
		}

		item, err := env.db.LibraryItemByID(context.Background(), rowID)
		if err != nil {
			t.Fatalf("LibraryItemByID: %v", err)
		}
		if item.Title != "Gone" || item.EpisodeTitle != "Gone S01E01" {
			t.Errorf("row changed on a failed re-read: %+v", item)
		}
	})
}

// rescanSecrets is the arrsync.SecretStore view of the cipher the route uses, so
// the sync pass below reads the same instance key the rescan decrypted.
type rescanSecrets struct {
	cipher *secrets.Cipher
	keys   map[string]string
}

func (s rescanSecrets) Get(key string) (string, error) {
	v, ok := s.keys[key]
	if !ok {
		return "", errors.New("rescanSecrets: no key for " + key)
	}
	return v, nil
}

func (s rescanSecrets) EncryptString(plaintext string) ([]byte, error) {
	return s.cipher.EncryptString(plaintext)
}

func (s rescanSecrets) DecryptString(blob []byte) (string, error) {
	return s.cipher.DecryptString(blob)
}

// TestLibraryRescanThenSyncPassIsNotAChange is the phantom-diff guard: the
// poller and the rescan must write the SAME field set, or the next pass sees a
// difference it "fixes", bumping updated_at for a change nobody made. After a
// rescan, a sync pass must therefore find the row unchanged, keep its id and
// leave updated_at alone.
func TestLibraryRescanThenSyncPassIsNotAChange(t *testing.T) {
	env := newRescanEnv(t, true)
	srv := radarrFixture(t, "radarr-rescan-key")
	instanceID := env.instance("radarr", srv.URL, srv.APIKey(), true)
	rowID := env.movieRow(instanceID, 7, "Shrek (stale)")

	rec := env.post("/api/library/items/" + itoa(rowID) + "/rescan")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST rescan = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	if body := decodeRescan(t, rec); body.Reread != "ok" {
		t.Fatalf("reread = %q (%s), want ok", body.Reread, body.RereadError)
	}

	ctx := context.Background()
	// Freeze the content stamp so a rewrite is visible even within the same
	// second as the pass (only updated_at tracks content changes).
	if _, err := env.db.DB.ExecContext(ctx,
		`UPDATE arr_library_cache SET updated_at = '2000-01-01 00:00:00' WHERE id = ?`, rowID); err != nil {
		t.Fatalf("freeze updated_at: %v", err)
	}

	conns, err := json.Marshal(settings.ArrConnections{Instances: []settings.ArrConnection{
		{Kind: "radarr", BaseURL: srv.URL, Enabled: true},
	}})
	if err != nil {
		t.Fatalf("marshal arr connections: %v", err)
	}
	if err := env.settings.Set(ctx, settings.KeyArrConnections, conns); err != nil {
		t.Fatalf("set arr connections: %v", err)
	}

	p, err := arrsync.New(arrsync.Deps{
		Store:    env.db,
		Settings: env.settings,
		Secrets:  rescanSecrets{cipher: env.cipher, keys: map[string]string{"arr.radarr.api_key": srv.APIKey()}},
		Timeout:  2 * time.Second,
		Logf:     t.Logf,
	})
	if err != nil {
		t.Fatalf("arrsync.New: %v", err)
	}

	stats, err := p.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.Updated != 0 || stats.Inserted != 0 || stats.Deleted != 0 {
		t.Fatalf("the sync pass rewrote the rescanned row: %+v", stats)
	}
	if stats.Unchanged != 1 {
		t.Errorf("unchanged = %d, want 1", stats.Unchanged)
	}

	var (
		id            int64
		updatedAt     string
		originalTitle string
		target        string
	)
	if err := env.db.DB.QueryRowContext(ctx,
		`SELECT id, COALESCE(updated_at, ''), COALESCE(original_title, ''),
		        COALESCE(target_file_path, '')
		 FROM arr_library_cache WHERE arr_instance_id = ? AND external_item_id = 7`,
		instanceID).Scan(&id, &updatedAt, &originalTitle, &target); err != nil {
		t.Fatalf("read the rescanned row: %v", err)
	}
	if id != rowID {
		t.Errorf("row id = %d, want %d (the pass replaced the row)", id, rowID)
	}
	if updatedAt != "2000-01-01 00:00:00" {
		t.Errorf("updated_at = %q, want it untouched: the pass reported a change", updatedAt)
	}
	if originalTitle != "Shrek" {
		t.Errorf("original_title = %q, want the value the re-read wrote (poller and rescan agree)", originalTitle)
	}
	if target != "/media/movies/Shrek (2001)/Shrek.2001.mkv" {
		t.Errorf("target_file_path = %q, want the re-read file", target)
	}
}
