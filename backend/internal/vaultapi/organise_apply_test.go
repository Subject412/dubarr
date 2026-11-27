package vaultapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// organiseHarness is one vault API service over a temporary database and a
// temporary vault directory. Nothing here ever touches a configured VAULT_DIR.
type organiseHarness struct {
	t     *testing.T
	svc   *Service
	mux   *http.ServeMux
	st    *store.Store
	vault string
	seq   int
}

func newOrganiseHarness(t *testing.T) *organiseHarness {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	ss := settings.New(st.DB)
	if err := ss.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("settings.Seed: %v", err)
	}
	vault := t.TempDir()
	paths, err := json.Marshal(map[string]any{"vault_dir": vault})
	if err != nil {
		t.Fatalf("marshal storage paths: %v", err)
	}
	if err := ss.Set(ctx, settings.KeyStoragePaths, paths); err != nil {
		t.Fatalf("set storage.paths: %v", err)
	}

	mux := http.NewServeMux()
	svc := RegisterRoutes(mux, Dependencies{
		DB:           st.DB,
		RequireAdmin: func(h http.Handler) http.Handler { return h },
	})
	return &organiseHarness{t: t, svc: svc, mux: mux, st: st, vault: vault}
}

// setNaming writes the vault.naming section through the settings validator, so
// a test pattern is exactly one an operator could have saved.
func (h *organiseHarness) setNaming(movie, episode, season string) {
	h.t.Helper()
	body, err := json.Marshal(settings.VaultNaming{
		MovieTemplate:   movie,
		EpisodeTemplate: episode,
		SeasonFolder:    season,
	})
	if err != nil {
		h.t.Fatalf("marshal vault.naming: %v", err)
	}
	if err := settings.New(h.st.DB).Set(context.Background(), settings.KeyVaultNaming, body); err != nil {
		h.t.Fatalf("set vault.naming: %v", err)
	}
}

// media seeds one vault_media row with a real folder inside the vault.
func (h *organiseHarness) media(kind, title string, year int, externalID int64) (int64, string) {
	h.t.Helper()
	folder := filepath.Join(h.vault, fmt.Sprintf("%s (%d)", title, year))
	if err := os.MkdirAll(folder, 0o755); err != nil {
		h.t.Fatalf("mkdir %s: %v", folder, err)
	}
	rec := store.VaultMediaRecord{MediaType: kind, Title: title, Year: year, FolderPath: folder}
	if kind == "movie" {
		rec.TMDBID = externalID
	} else {
		rec.TVDBID = externalID
	}
	id, err := h.st.EnsureVaultMedia(context.Background(), rec)
	if err != nil {
		h.t.Fatalf("EnsureVaultMedia(%s): %v", title, err)
	}
	return id, folder
}

// track writes an archived .mka at legacyName and records it.
func (h *organiseHarness) track(mediaID int64, folder, legacyName, variant, codec, channels, lang string, season, episode *int) (int64, string) {
	h.t.Helper()
	path := filepath.Join(folder, legacyName)
	if err := os.WriteFile(path, []byte("vault payload:"+legacyName), 0o444); err != nil {
		h.t.Fatalf("write %s: %v", path, err)
	}
	h.seq++
	channelCount := 2
	if channels == "5.1" {
		channelCount = 6
	}
	id, err := h.st.RecordVaultTrack(context.Background(), store.VaultTrackRecord{
		VaultMediaID:  mediaID,
		SeasonNumber:  season,
		EpisodeNumber: episode,
		GroupUUID:     fmt.Sprintf("g-%d", h.seq),
		LangISO2:      lang,
		Variant:       variant,
		FilePath:      path,
		SHA256:        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Codec:         codec,
		Channels:      channels,
		ChannelCount:  channelCount,
	})
	if err != nil {
		h.t.Fatalf("RecordVaultTrack(%s): %v", path, err)
	}
	return id, path
}

func (h *organiseHarness) post(path string, body any) (int, []byte) {
	h.t.Helper()
	var reader *strings.Reader
	switch payload := body.(type) {
	case nil:
		reader = strings.NewReader("")
	case string:
		reader = strings.NewReader(payload)
	default:
		blob, err := json.Marshal(payload)
		if err != nil {
			h.t.Fatalf("marshal request: %v", err)
		}
		reader = strings.NewReader(string(blob))
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, reader))
	return rec.Code, rec.Body.Bytes()
}

func (h *organiseHarness) previewAll() (int, PlanResponse, []byte) {
	h.t.Helper()
	status, body := h.post("/api/vault/organise/preview", previewRequest{Scope: organiseScope{MediaType: "all"}})
	var plan PlanResponse
	if status == http.StatusOK {
		if err := json.Unmarshal(body, &plan); err != nil {
			h.t.Fatalf("decode plan: %v (%s)", err, body)
		}
	}
	return status, plan, body
}

func (h *organiseHarness) mustPreview() PlanResponse {
	h.t.Helper()
	status, plan, body := h.previewAll()
	if status != http.StatusOK {
		h.t.Fatalf("preview = %d %s, want 200", status, body)
	}
	if plan.PlanID == "" {
		h.t.Fatalf("preview returned no plan_id: %s", body)
	}
	return plan
}

func (h *organiseHarness) apply(planID string) (int, []byte) {
	h.t.Helper()
	return h.post("/api/vault/organise/apply", applyRequest{PlanID: planID, Confirm: true})
}

func (h *organiseHarness) runs() []RunRow {
	h.t.Helper()
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/vault/organise/runs?limit=50", nil))
	if rec.Code != http.StatusOK {
		h.t.Fatalf("runs = %d %s", rec.Code, rec.Body.String())
	}
	var resp runsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		h.t.Fatalf("decode runs: %v", err)
	}
	return resp.Items
}

// paths snapshots audio_tracks so a refusal can be compared field for field.
func (h *organiseHarness) paths() map[int64]string {
	h.t.Helper()
	rows, err := h.st.DB.Query(`SELECT id, file_path FROM audio_tracks ORDER BY id`)
	if err != nil {
		h.t.Fatalf("snapshot paths: %v", err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var (
			id   int64
			path string
		)
		if err := rows.Scan(&id, &path); err != nil {
			h.t.Fatalf("scan path: %v", err)
		}
		out[id] = path
	}
	return out
}

func equalPaths(a, b map[int64]string) bool {
	if len(a) != len(b) {
		return false
	}
	for id, path := range a {
		if b[id] != path {
			return false
		}
	}
	return true
}

func countMKA(t *testing.T, root string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".mka") {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return count
}

func decodeFailure(t *testing.T, body []byte) organiseError {
	t.Helper()
	var failure organiseError
	if err := json.Unmarshal(body, &failure); err != nil {
		t.Fatalf("decode error body %s: %v", body, err)
	}
	return failure
}

func fileRowCount(t *testing.T, st *store.Store, runID int64) map[string]int {
	t.Helper()
	rows, err := st.DB.Query(`SELECT result, COUNT(*) FROM vault_organise_files WHERE run_id = ? GROUP BY result`, runID)
	if err != nil {
		t.Fatalf("count audit files: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var (
			result string
			count  int
		)
		if err := rows.Scan(&result, &count); err != nil {
			t.Fatalf("scan audit file: %v", err)
		}
		out[result] = count
	}
	return out
}

func TestPreviewReturnsEverySourceToDestination(t *testing.T) {
	h := newOrganiseHarness(t)
	movieID, movieFolder := h.media("movie", "Spirited Away", 2001, 129)
	trackID, source := h.track(movieID, movieFolder, "legacy name.pl.mka", "dubbing", "eac3", "5.1", "pl", nil, nil)

	seriesID, showFolder := h.media("series", "The Office", 2005, 42)
	season, episode := 2, 3
	if _, err := h.st.DB.Exec(
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4, enabled)
		 VALUES ('sonarr', 'sonarr', 'http://sonarr:8989', 'enc', '0000', 1)`); err != nil {
		t.Fatalf("insert instance: %v", err)
	}
	if _, err := h.st.DB.Exec(
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, series_id, tvdb_id, title,
		  year, season_number, episode_number, episode_title, eligibility_status)
		 VALUES (1, 'episode', 900, 42, 42, 'The Office', 2005, 2, 3, 'Office Olympics', 'eligible')`); err != nil {
		t.Fatalf("insert episode: %v", err)
	}
	episodeID, episodeSource := h.track(seriesID, showFolder, "ep.pl.mka", "lektor", "eac3", "2.0", "pl", &season, &episode)

	status, plan, body := h.previewAll()
	if status != http.StatusOK {
		t.Fatalf("preview = %d %s", status, body)
	}
	if !strings.HasPrefix(plan.PlanID, "sha256:") {
		t.Fatalf("plan_id = %q, want a sha256 hash", plan.PlanID)
	}
	if plan.ExpiresAt == "" {
		t.Fatal("expires_at is empty")
	}
	if len(plan.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", plan.Conflicts)
	}
	if len(plan.Files) != 2 {
		t.Fatalf("files = %d, want 2 (one row per track)", len(plan.Files))
	}

	byID := map[int64]PlanFile{}
	for _, f := range plan.Files {
		byID[f.TrackID] = f
	}
	movie := byID[trackID]
	wantMovie := filepath.Join(movieFolder, "Spirited Away (2001) [Dubbing EAC3 5.1].pl.mka")
	if movie.Source != source || movie.Destination != wantMovie || !movie.Changed {
		t.Fatalf("movie row = %+v, want source %q destination %q", movie, source, wantMovie)
	}
	if movie.Conflict != nil {
		t.Fatalf("movie conflict = %v, want null", *movie.Conflict)
	}

	episodeRow := byID[episodeID]
	wantEpisode := filepath.Join(showFolder, "Season 02", "The Office - S02E03 - Office Olympics [Lektor EAC3 2.0].pl.mka")
	if episodeRow.Destination != wantEpisode {
		t.Fatalf("episode destination = %q, want %q", episodeRow.Destination, wantEpisode)
	}
	if episodeRow.Source != episodeSource {
		t.Fatalf("episode source = %q, want %q", episodeRow.Source, episodeSource)
	}

	if plan.Impact.Renamed != 2 || plan.Impact.Unchanged != 0 {
		t.Fatalf("impact = %+v, want 2 renamed", plan.Impact)
	}
	// Preview writes nothing at all.
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("preview moved the source: %v", err)
	}
	if _, err := os.Stat(wantMovie); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview created %q", wantMovie)
	}
	if _, err := os.Stat(filepath.Join(showFolder, "Season 02")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preview created the season folder")
	}
}

func TestPreviewRefusesDuplicateDestination(t *testing.T) {
	h := newOrganiseHarness(t)
	seriesID, showFolder := h.media("series", "The Office", 2005, 7)
	season, episode := 2, 3
	first, sourceA := h.track(seriesID, showFolder, "one.pl.mka", "lektor", "eac3", "2.0", "pl", &season, &episode)
	second, sourceB := h.track(seriesID, showFolder, "two.pl.mka", "lektor", "eac3", "2.0", "pl", &season, &episode)

	status, plan, body := h.previewAll()
	if status != http.StatusOK {
		t.Fatalf("preview = %d %s", status, body)
	}
	if plan.PlanID != "" {
		t.Fatalf("plan_id = %q, want it absent for a conflicted plan", plan.PlanID)
	}
	if len(plan.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want one", plan.Conflicts)
	}
	conflict := plan.Conflicts[0]
	if conflict.Kind != conflictDestDup {
		t.Fatalf("kind = %q, want %q", conflict.Kind, conflictDestDup)
	}
	if len(conflict.TrackIDs) != 2 || conflict.TrackIDs[0] != first || conflict.TrackIDs[1] != second {
		t.Fatalf("track ids = %v, want [%d %d]", conflict.TrackIDs, first, second)
	}
	if len(plan.Files) != 2 {
		t.Fatalf("files = %d, want both rows so the operator sees the clash", len(plan.Files))
	}
	for _, f := range plan.Files {
		if f.Conflict == nil || *f.Conflict != conflictDestDup {
			t.Fatalf("row conflict = %v, want %q", f.Conflict, conflictDestDup)
		}
	}
	// Nothing moved.
	for _, source := range []string{sourceA, sourceB} {
		if _, err := os.Stat(source); err != nil {
			t.Fatalf("preview moved %s: %v", source, err)
		}
	}
}

func TestPreviewReportsJobsReferencingTracks(t *testing.T) {
	h := newOrganiseHarness(t)
	movieID, folder := h.media("movie", "Spirited Away", 2001, 129)
	trackID, source := h.track(movieID, folder, "legacy.pl.mka", "dubbing", "eac3", "5.1", "pl", nil, nil)
	if _, err := h.st.DB.Exec(
		`INSERT INTO sync_mux_jobs (target_track_id, target_video_path, target_lang_iso2, status)
		 VALUES (?, '/media/target.mkv', 'pl', 'queued')`, trackID); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	before := h.paths()
	status, plan, body := h.previewAll()
	if status != http.StatusOK {
		t.Fatalf("preview = %d %s", status, body)
	}
	if plan.Impact.JobsReferencingTracks != 1 {
		t.Fatalf("jobs_referencing_tracks = %d, want 1", plan.Impact.JobsReferencingTracks)
	}
	if plan.Impact.JobsBlocked != 0 {
		t.Fatalf("jobs_blocked = %d, want 0 for a queued job", plan.Impact.JobsBlocked)
	}
	if plan.PlanID == "" {
		t.Fatal("a queued job must not stop a preview")
	}
	if !equalPaths(before, h.paths()) {
		t.Fatal("preview rewrote a track path")
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("preview moved the source: %v", err)
	}
}

func TestApplyRenamesAndRewritesPathsInOneTransaction(t *testing.T) {
	h := newOrganiseHarness(t)
	movieID, folder := h.media("movie", "Spirited Away", 2001, 129)
	trackID, source := h.track(movieID, folder, "legacy.pl.mka", "dubbing", "eac3", "5.1", "pl", nil, nil)

	plan := h.mustPreview()
	destination := plan.Files[0].Destination
	if plan.Files[0].TrackID != trackID {
		t.Fatalf("planned track = %d, want %d", plan.Files[0].TrackID, trackID)
	}

	status, body := h.apply(plan.PlanID)
	if status != http.StatusOK {
		t.Fatalf("apply = %d %s", status, body)
	}
	var run RunResponse
	if err := json.Unmarshal(body, &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Status != store.OrganiseStatusApplied || run.FileCount != 1 || run.Renamed != 1 {
		t.Fatalf("run = %+v", run)
	}

	if _, err := os.Stat(destination); err != nil {
		t.Fatalf("destination missing after apply: %v", err)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists after apply: %v", err)
	}
	if got := h.paths()[trackID]; got != destination {
		t.Fatalf("file_path = %q, want %q", got, destination)
	}

	runs := h.runs()
	if len(runs) != 1 || runs[0].ID != run.RunID || runs[0].Status != store.OrganiseStatusApplied {
		t.Fatalf("runs = %+v", runs)
	}
	if runs[0].FileCount != 1 || runs[0].PlanHash != plan.PlanID {
		t.Fatalf("run row = %+v", runs[0])
	}
	results := fileRowCount(t, h.st, run.RunID)
	if results[store.OrganiseResultRenamed] != 1 {
		t.Fatalf("audit results = %v, want one renamed row", results)
	}

	// The plan is spent: a second apply is stale rather than a second move.
	if status, body := h.apply(plan.PlanID); status != http.StatusConflict {
		t.Fatalf("second apply = %d %s, want 409", status, body)
	}
}

func TestApplyCollisionRefusesWholePlan(t *testing.T) {
	h := newOrganiseHarness(t)
	titles := []string{"Alpha", "Beta", "Gamma", "Delta", "Epsilon"}
	for i, title := range titles {
		mediaID, folder := h.media("movie", title, 2000+i, int64(100+i))
		h.track(mediaID, folder, "legacy.pl.mka", "dubbing", "eac3", "5.1", "pl", nil, nil)
	}
	plan := h.mustPreview()
	if len(plan.Files) != len(titles) {
		t.Fatalf("files = %d, want %d", len(plan.Files), len(titles))
	}

	// A file appears at one destination between preview and apply.
	taken := plan.Files[2].Destination
	if err := os.WriteFile(taken, []byte("someone else's file"), 0o444); err != nil {
		t.Fatalf("write colliding file: %v", err)
	}
	before := h.paths()

	status, body := h.apply(plan.PlanID)
	if status != http.StatusConflict {
		t.Fatalf("apply = %d %s, want 409", status, body)
	}
	failure := decodeFailure(t, body)
	if failure.Code != codeConflict || failure.Kind != conflictDestExists {
		t.Fatalf("failure = %+v, want conflict/%s", failure, conflictDestExists)
	}
	if !equalPaths(before, h.paths()) {
		t.Fatal("a refused apply rewrote a track path")
	}
	for _, f := range plan.Files {
		if _, err := os.Stat(f.Source); err != nil {
			t.Fatalf("a refused apply moved %s: %v", f.Source, err)
		}
	}
	if data, err := os.ReadFile(taken); err != nil || string(data) != "someone else's file" {
		t.Fatalf("the colliding file was touched: %q %v", data, err)
	}
	if runs := h.runs(); len(runs) != 0 {
		t.Fatalf("a refused apply wrote an audit run: %+v", runs)
	}
}

func TestApplyRefusesWhileJobMuxing(t *testing.T) {
	h := newOrganiseHarness(t)
	movieID, folder := h.media("movie", "Spirited Away", 2001, 129)
	trackID, source := h.track(movieID, folder, "legacy.pl.mka", "dubbing", "eac3", "5.1", "pl", nil, nil)
	plan := h.mustPreview()
	before := h.paths()

	res, err := h.st.DB.Exec(
		`INSERT INTO sync_mux_jobs (target_track_id, target_video_path, target_lang_iso2, status)
		 VALUES (?, '/media/target.mkv', 'pl', 'muxing')`, trackID)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	jobID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("job id: %v", err)
	}

	status, body := h.apply(plan.PlanID)
	if status != http.StatusConflict {
		t.Fatalf("apply = %d %s, want 409", status, body)
	}
	if failure := decodeFailure(t, body); failure.Code != codeJobsRunning {
		t.Fatalf("failure = %+v, want %s", failure, codeJobsRunning)
	}
	if !equalPaths(before, h.paths()) {
		t.Fatal("a refused apply rewrote a track path")
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("a refused apply moved the source: %v", err)
	}

	// A held job cannot be mid-mux (#48), so it must not block.
	if _, err := h.st.DB.Exec(`UPDATE sync_mux_jobs SET status = 'held' WHERE id = ?`, jobID); err != nil {
		t.Fatalf("hold job: %v", err)
	}
	status, body = h.apply(plan.PlanID)
	if status != http.StatusOK {
		t.Fatalf("apply with only held jobs = %d %s, want 200", status, body)
	}
	if got := h.paths()[trackID]; got != plan.Files[0].Destination {
		t.Fatalf("file_path = %q, want %q", got, plan.Files[0].Destination)
	}
}

func TestApplyStalePlanAfterFileChanges(t *testing.T) {
	h := newOrganiseHarness(t)
	movieID, folder := h.media("movie", "Spirited Away", 2001, 129)
	trackID, source := h.track(movieID, folder, "legacy.pl.mka", "dubbing", "eac3", "5.1", "pl", nil, nil)
	plan := h.mustPreview()
	before := h.paths()

	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(source, later, later); err != nil {
		t.Fatalf("touch source: %v", err)
	}

	status, body := h.apply(plan.PlanID)
	if status != http.StatusConflict {
		t.Fatalf("apply = %d %s, want 409", status, body)
	}
	if failure := decodeFailure(t, body); failure.Code != codeStalePlan {
		t.Fatalf("failure = %+v, want %s", failure, codeStalePlan)
	}
	if !equalPaths(before, h.paths()) {
		t.Fatal("a stale apply rewrote a track path")
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("a stale apply moved the source: %v", err)
	}
	if _, err := os.Stat(plan.Files[0].Destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a stale apply created the destination")
	}
	if got := h.paths()[trackID]; got != source {
		t.Fatalf("file_path = %q, want %q", got, source)
	}
}

func TestApplyRefusesPathCycle(t *testing.T) {
	h := newOrganiseHarness(t)
	// The template is {Variant}, and both rows are named after the *other*
	// variant, so each destination is the other track's source.
	h.setNaming("{Variant}", "{Title} - {Episode Title}", "")
	mediaID, folder := h.media("movie", "Swapped", 2020, 9)
	first, firstSource := h.track(mediaID, folder, "Lektor.pl.mka", "dubbing", "eac3", "5.1", "pl", nil, nil)
	second, secondSource := h.track(mediaID, folder, "Dubbing.pl.mka", "lektor", "eac3", "2.0", "pl", nil, nil)

	plan := h.mustPreview()
	destinations := map[string]int64{}
	for _, f := range plan.Files {
		destinations[f.Destination] = f.TrackID
	}
	if destinations[firstSource] != second || destinations[secondSource] != first {
		t.Fatalf("plan is not a swap: %+v", plan.Files)
	}
	before := h.paths()

	status, body := h.apply(plan.PlanID)
	if status != http.StatusConflict {
		t.Fatalf("apply = %d %s, want 409", status, body)
	}
	if failure := decodeFailure(t, body); failure.Code != codePathCycle {
		t.Fatalf("failure = %+v, want %s", failure, codePathCycle)
	}
	if !equalPaths(before, h.paths()) {
		t.Fatal("a refused swap rewrote a track path")
	}
	for _, source := range []string{firstSource, secondSource} {
		if _, err := os.Stat(source); err != nil {
			t.Fatalf("a refused swap moved %s: %v", source, err)
		}
	}
}

func TestApplyRollsBackRenamesWhenSQLFails(t *testing.T) {
	h := newOrganiseHarness(t)
	movieID, folder := h.media("movie", "Spirited Away", 2001, 129)
	trackID, source := h.track(movieID, folder, "legacy.pl.mka", "dubbing", "eac3", "5.1", "pl", nil, nil)
	plan := h.mustPreview()
	before := h.paths()

	// The audited outcome cannot be written, so phase C fails after the
	// renames: this is exactly the "rename succeeded, commit failed" case.
	if _, err := h.st.DB.Exec(`
		CREATE TRIGGER fail_applied BEFORE INSERT ON vault_organise_runs
		WHEN NEW.status = 'applied'
		BEGIN SELECT RAISE(ABORT, 'injected phase C failure'); END;`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	status, body := h.apply(plan.PlanID)
	if status != http.StatusInternalServerError {
		t.Fatalf("apply = %d %s, want 500", status, body)
	}
	failure := decodeFailure(t, body)
	if failure.Code != codeApplyFailed || failure.Stage != "sql" || !failure.Undone {
		t.Fatalf("failure = %+v, want apply_failed stage=sql undone", failure)
	}

	if _, err := os.Stat(source); err != nil {
		t.Fatalf("the source was not restored: %v", err)
	}
	if _, err := os.Stat(plan.Files[0].Destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the destination survived the rollback")
	}
	if !equalPaths(before, h.paths()) {
		t.Fatalf("the database changed: %v", h.paths())
	}
	if got := h.paths()[trackID]; got != source {
		t.Fatalf("file_path = %q, want %q", got, source)
	}

	runs := h.runs()
	if len(runs) != 1 || runs[0].Status != store.OrganiseStatusRolledBack {
		t.Fatalf("runs = %+v, want one rolled_back run", runs)
	}
	if results := fileRowCount(t, h.st, runs[0].ID); results[store.OrganiseResultRenamedBack] != 1 {
		t.Fatalf("audit results = %v, want one renamed_back row", results)
	}
}

func TestApplyReconcilesWhenRollbackAlsoFails(t *testing.T) {
	h := newOrganiseHarness(t)
	movieID, folder := h.media("movie", "Spirited Away", 2001, 129)
	trackID, source := h.track(movieID, folder, "legacy.pl.mka", "dubbing", "eac3", "5.1", "pl", nil, nil)
	plan := h.mustPreview()
	destination := plan.Files[0].Destination

	if _, err := h.st.DB.Exec(`
		CREATE TRIGGER fail_applied BEFORE INSERT ON vault_organise_runs
		WHEN NEW.status = 'applied'
		BEGIN SELECT RAISE(ABORT, 'injected phase C failure'); END;`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	// The undo cannot rename the file back: the only path to reconcile.
	h.svc.renameFile = func(oldpath, newpath string) error {
		if newpath == source {
			return errors.New("injected rename-back failure")
		}
		return os.Rename(oldpath, newpath)
	}

	status, body := h.apply(plan.PlanID)
	if status != http.StatusInternalServerError {
		t.Fatalf("apply = %d %s, want 500", status, body)
	}
	failure := decodeFailure(t, body)
	if failure.Code != codeManualAttention {
		t.Fatalf("failure = %+v, want %s", failure, codeManualAttention)
	}
	if len(failure.Files) != 1 || failure.Files[0] != destination {
		t.Fatalf("files = %v, want [%q]", failure.Files, destination)
	}

	// The database now points at the file that actually exists.
	if got := h.paths()[trackID]; got != destination {
		t.Fatalf("file_path = %q, want the destination %q", got, destination)
	}
	if _, err := os.Stat(destination); err != nil {
		t.Fatalf("the file is not at the destination: %v", err)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the file is still at the source after a failed undo")
	}

	runs := h.runs()
	if len(runs) != 1 || runs[0].Status != store.OrganiseStatusNeedsAttention {
		t.Fatalf("runs = %+v, want one needs_attention run", runs)
	}
	if results := fileRowCount(t, h.st, runs[0].ID); results[store.OrganiseResultReconciled] != 1 {
		t.Fatalf("audit results = %v, want one reconciled row", results)
	}
}

func TestApplyRenameFailureMidwayUndoesEarlierRenames(t *testing.T) {
	h := newOrganiseHarness(t)
	for i, title := range []string{"Alpha", "Beta", "Gamma"} {
		mediaID, folder := h.media("movie", title, 2000+i, int64(200+i))
		h.track(mediaID, folder, "legacy.pl.mka", "dubbing", "eac3", "5.1", "pl", nil, nil)
	}
	plan := h.mustPreview()
	before := h.paths()

	calls := 0
	h.svc.renameFile = func(oldpath, newpath string) error {
		calls++
		if calls == 3 {
			return errors.New("injected rename failure")
		}
		return os.Rename(oldpath, newpath)
	}

	status, body := h.apply(plan.PlanID)
	if status != http.StatusInternalServerError {
		t.Fatalf("apply = %d %s, want 500", status, body)
	}
	failure := decodeFailure(t, body)
	if failure.Code != codeApplyFailed || failure.Stage != "rename" || !failure.Undone {
		t.Fatalf("failure = %+v, want apply_failed stage=rename undone", failure)
	}

	for _, f := range plan.Files {
		if _, err := os.Stat(f.Source); err != nil {
			t.Fatalf("source %s was not restored: %v", f.Source, err)
		}
		if _, err := os.Stat(f.Destination); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination %s survived the undo", f.Destination)
		}
	}
	if !equalPaths(before, h.paths()) {
		t.Fatal("the database changed on a rolled-back apply")
	}

	runs := h.runs()
	if len(runs) != 1 || runs[0].Status != store.OrganiseStatusRolledBack {
		t.Fatalf("runs = %+v, want one rolled_back run", runs)
	}
	results := fileRowCount(t, h.st, runs[0].ID)
	if results[store.OrganiseResultRenamedBack] != 2 || results[store.OrganiseResultFailed] != 1 {
		t.Fatalf("audit results = %v, want 2 renamed_back and 1 failed", results)
	}
}

func TestApplyNeverDeletesMedia(t *testing.T) {
	h := newOrganiseHarness(t)
	seriesID, showFolder := h.media("series", "The Office", 2005, 42)
	season, episode := 2, 3
	h.track(seriesID, showFolder, "one.pl.mka", "lektor", "eac3", "2.0", "pl", &season, &episode)
	h.track(seriesID, showFolder, "two.pl.mka", "dubbing", "ac3", "5.1", "pl", &season, &episode)

	before := countMKA(t, h.vault)
	if before != 2 {
		t.Fatalf("seeded %d .mka files, want 2", before)
	}
	plan := h.mustPreview()
	if status, body := h.apply(plan.PlanID); status != http.StatusOK {
		t.Fatalf("apply = %d %s", status, body)
	}
	if after := countMKA(t, h.vault); after != before {
		t.Fatalf(".mka files = %d after apply, want %d: nothing may be deleted", after, before)
	}
}

func TestApplyRefusesPathOutsideVault(t *testing.T) {
	h := newOrganiseHarness(t)
	outside := t.TempDir()
	mediaID, err := h.st.EnsureVaultMedia(context.Background(), store.VaultMediaRecord{
		MediaType: "movie", Title: "Elsewhere", Year: 2020, TMDBID: 77, FolderPath: outside,
	})
	if err != nil {
		t.Fatalf("EnsureVaultMedia: %v", err)
	}
	trackID, source := h.track(mediaID, outside, "legacy.pl.mka", "dubbing", "eac3", "5.1", "pl", nil, nil)

	plan := h.mustPreview()
	before := h.paths()

	status, body := h.apply(plan.PlanID)
	if status != http.StatusConflict {
		t.Fatalf("apply = %d %s, want 409", status, body)
	}
	if failure := decodeFailure(t, body); failure.Code != codePathEscape {
		t.Fatalf("failure = %+v, want %s", failure, codePathEscape)
	}
	if !equalPaths(before, h.paths()) {
		t.Fatal("an escaping apply rewrote a track path")
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("an escaping apply moved the source: %v", err)
	}
	if got := h.paths()[trackID]; got != source {
		t.Fatalf("file_path = %q, want %q", got, source)
	}
}

func TestApplyRequiresConfirm(t *testing.T) {
	h := newOrganiseHarness(t)
	movieID, folder := h.media("movie", "Spirited Away", 2001, 129)
	_, source := h.track(movieID, folder, "legacy.pl.mka", "dubbing", "eac3", "5.1", "pl", nil, nil)
	plan := h.mustPreview()

	status, body := h.post("/api/vault/organise/apply", applyRequest{PlanID: plan.PlanID, Confirm: false})
	if status != http.StatusBadRequest {
		t.Fatalf("apply without confirm = %d %s, want 400", status, body)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("an unconfirmed apply moved the source: %v", err)
	}
	if runs := h.runs(); len(runs) != 0 {
		t.Fatalf("an unconfirmed apply wrote an audit run: %+v", runs)
	}
}

func TestOrganiseSurvivesRestart(t *testing.T) {
	h := newOrganiseHarness(t)
	movieID, folder := h.media("movie", "Spirited Away", 2001, 129)
	h.track(movieID, folder, "legacy.pl.mka", "dubbing", "eac3", "5.1", "pl", nil, nil)
	plan := h.mustPreview()
	if status, body := h.apply(plan.PlanID); status != http.StatusOK {
		t.Fatalf("apply = %d %s", status, body)
	}

	// A new service over the same database: the in-memory plan cache is gone,
	// the audit history is not.
	restarted := http.NewServeMux()
	RegisterRoutes(restarted, Dependencies{
		DB:           h.st.DB,
		RequireAdmin: func(next http.Handler) http.Handler { return next },
	})
	rec := httptest.NewRecorder()
	restarted.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/vault/organise/runs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("runs after restart = %d %s", rec.Code, rec.Body.String())
	}
	var resp runsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode runs: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].Status != store.OrganiseStatusApplied {
		t.Fatalf("runs = %+v, want the applied run", resp.Items)
	}
	if resp.Items[0].PlanHash != plan.PlanID {
		t.Fatalf("plan_hash = %q, want %q", resp.Items[0].PlanHash, plan.PlanID)
	}
}

// TestApplyWritesAuditRunForEveryOutcome pins the audit contract: applied,
// rolled_back and needs_attention each leave exactly one run row, and the run
// rows are the record a restart reads.
func TestApplyWritesAuditRunForEveryOutcome(t *testing.T) {
	h := newOrganiseHarness(t)
	failApplied := `
		CREATE TRIGGER fail_applied BEFORE INSERT ON vault_organise_runs
		WHEN NEW.status = 'applied'
		BEGIN SELECT RAISE(ABORT, 'injected phase C failure'); END;`

	// 1. applied
	appliedMedia, appliedFolder := h.media("movie", "Applied", 2001, 401)
	appliedTrack, _ := h.track(appliedMedia, appliedFolder, "legacy.pl.mka", "dubbing", "eac3", "5.1", "pl", nil, nil)
	appliedPlan := h.mustPreview()
	if status, body := h.apply(appliedPlan.PlanID); status != http.StatusOK {
		t.Fatalf("apply(applied) = %d %s", status, body)
	}

	// 2. rolled_back
	rolledMedia, rolledFolder := h.media("movie", "Rolled Back", 2002, 402)
	rolledTrack, rolledSource := h.track(rolledMedia, rolledFolder, "legacy.pl.mka", "dubbing", "eac3", "5.1", "pl", nil, nil)
	if _, err := h.st.DB.Exec(failApplied); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	rolledPlan := h.mustPreview()
	status, body := h.apply(rolledPlan.PlanID)
	if status != http.StatusInternalServerError {
		t.Fatalf("apply(rolled_back) = %d %s, want 500", status, body)
	}
	if failure := decodeFailure(t, body); failure.Code != codeApplyFailed || failure.Stage != "sql" {
		t.Fatalf("failure = %+v, want apply_failed stage=sql", failure)
	}

	// 3. needs_attention: the same SQL failure, with the rename-back refused.
	attentionMedia, attentionFolder := h.media("movie", "Needs Attention", 2003, 403)
	attentionTrack, attentionSource := h.track(attentionMedia, attentionFolder, "legacy.pl.mka", "dubbing", "eac3", "5.1", "pl", nil, nil)
	h.svc.renameFile = func(oldpath, newpath string) error {
		if newpath == attentionSource {
			return errors.New("injected rename-back failure")
		}
		return os.Rename(oldpath, newpath)
	}
	attentionPlan := h.mustPreview()
	status, body = h.apply(attentionPlan.PlanID)
	if status != http.StatusInternalServerError {
		t.Fatalf("apply(needs_attention) = %d %s, want 500", status, body)
	}
	if failure := decodeFailure(t, body); failure.Code != codeManualAttention {
		t.Fatalf("failure = %+v, want %s", failure, codeManualAttention)
	}

	runs := h.runs()
	if len(runs) != 3 {
		t.Fatalf("runs = %+v, want three (one per outcome)", runs)
	}
	want := []string{
		store.OrganiseStatusNeedsAttention,
		store.OrganiseStatusRolledBack,
		store.OrganiseStatusApplied,
	}
	for i, status := range want {
		if runs[i].Status != status {
			t.Fatalf("run %d status = %q, want %q (%+v)", i, runs[i].Status, status, runs)
		}
		if runs[i].PlanHash == "" || runs[i].FileCount == 0 {
			t.Fatalf("run %d = %+v, want a hash and a file count", i, runs[i])
		}
	}

	// The two compensations left the vault exactly as they found it, apart from
	// the file the last one could not put back.
	if got := h.paths()[rolledTrack]; got != rolledSource {
		t.Fatalf("rolled_back track path = %q, want %q", got, rolledSource)
	}
	if got := h.paths()[attentionTrack]; got != attentionPlan.Files[len(attentionPlan.Files)-1].Destination {
		t.Fatalf("needs_attention track path = %q, want the destination", got)
	}
	if got := h.paths()[appliedTrack]; got != appliedPlan.Files[0].Destination {
		t.Fatalf("applied track path = %q, want the destination", got)
	}
}
