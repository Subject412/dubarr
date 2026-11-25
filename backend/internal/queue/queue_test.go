package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/gate"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

func TestMatchesDonorPattern(t *testing.T) {
	tests := []struct {
		name string
		file string
		want bool
	}{
		{"fixture donor", "Minions and Monsters 2026 PL.DUB.MD.1080p.AMZN.WEB-DL.XviD-MAXX.mp4", true},
		{"case insensitive", "Shrek.2001.pl.dub.1080p.mkv", true},
		{"target is not a donor", "Minions and Monsters (2026) - [ArcA310][Remux-2160p][HDR10][Opus 7.1][AV1][imdb-tt32890033]-FraMeSToR.mkv", false},
		{"wildcard pattern", "Movie.2024.LEKTOR.PL.mkv", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			patterns := []string{"PL.DUB"}
			if tc.name == "wildcard pattern" {
				patterns = []string{"*LEKTOR.PL*"}
			}
			if got := matchesDonorPattern(tc.file, patterns); got != tc.want {
				t.Errorf("matchesDonorPattern(%q, %v) = %v, want %v", tc.file, patterns, got, tc.want)
			}
		})
	}
}

type harness struct {
	t     *testing.T
	db    *sql.DB
	mux   *http.ServeMux
	media string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	settingsStore := settings.New(st.DB)
	if err := settingsStore.Seed(context.Background(), dir); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	mux := http.NewServeMux()
	RegisterRoutes(mux, Dependencies{
		DB:       st.DB,
		Settings: settingsStore,
		RequireAdmin: func(h http.Handler) http.Handler {
			return h
		},
	})
	return &harness{t: t, db: st.DB, mux: mux, media: t.TempDir()}
}

func (h *harness) request(method, path string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// seedCandidate inserts an arr target plus a pending donor candidate and
// returns the import_queue id.
func (h *harness) seedCandidate(t *testing.T, targetName, gateStatus string, audioLangs string) int64 {
	t.Helper()
	ctx := context.Background()
	targetPath := filepath.Join(h.media, targetName)
	if err := os.WriteFile(targetPath, []byte("target"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	res, err := h.db.ExecContext(ctx,
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		 VALUES ('main', 'radarr', 'http://radarr.invalid', x'00', '0000')`)
	if err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	instanceID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("instance id: %v", err)
	}

	res, err = h.db.ExecContext(ctx,
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, title, original_language_iso2, year,
		  target_file_path, audio_languages_json, eligibility_status)
		 VALUES (?, 'movie', 1, 'Minions and Monsters', 'en', 2026, ?, ?, ?)`,
		instanceID, targetPath, audioLangs, gateStatus)
	if err != nil {
		t.Fatalf("insert library cache: %v", err)
	}
	cacheID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("cache id: %v", err)
	}

	source := filepath.Join(t.TempDir(), "Minions and Monsters 2026 PL.DUB.MD.1080p.AMZN.WEB-DL.XviD-MAXX.mp4")
	st := &store.Store{DB: h.db}
	created, err := st.RecordImportCandidate(ctx, source, filepath.Base(source), 42)
	if err != nil || !created {
		t.Fatalf("RecordImportCandidate: created=%v err=%v", created, err)
	}
	if _, err := h.db.ExecContext(ctx,
		`UPDATE import_queue SET matched_arr_instance_id = ?, matched_library_cache_id = ? WHERE source_file_path = ?`,
		instanceID, cacheID, source); err != nil {
		t.Fatalf("link candidate: %v", err)
	}
	if err := st.SetImportCandidateGate(ctx, source, store.ImportGateStatus{
		Status:     gateStatus,
		TargetPath: targetPath,
	}); err != nil {
		t.Fatalf("SetImportCandidateGate: %v", err)
	}

	var id int64
	if err := h.db.QueryRowContext(ctx, `SELECT id FROM import_queue WHERE source_file_path = ?`, source).Scan(&id); err != nil {
		t.Fatalf("candidate id: %v", err)
	}
	return id
}

// applyGate persists a gate classification for the seeded candidate the way the
// gate service does, so the queue reads a real verdict instead of the bare
// status seedCandidate writes.
func (h *harness) applyGate(t *testing.T, id int64, targetPath string, res gate.Result) {
	t.Helper()
	var source string
	if err := h.db.QueryRow(
		`SELECT source_file_path FROM import_queue WHERE id = ?`, id).Scan(&source); err != nil {
		t.Fatalf("gate row: %v", err)
	}
	st := &store.Store{DB: h.db}
	if err := st.SetImportCandidateGate(context.Background(), source, store.ImportGateStatus{
		Status:      string(res.Status),
		TargetPath:  targetPath,
		MatchedTags: res.Matched,
		MissingTags: res.Missing,
	}); err != nil {
		t.Fatalf("SetImportCandidateGate: %v", err)
	}
}

// clearGateVerdict drops the stored gate record while leaving the arr library
// row's eligibility_status in place: the shape arrsync writes for a target it
// classified without the gate service.
func (h *harness) clearGateVerdict(t *testing.T, id int64) {
	t.Helper()
	if _, err := h.db.Exec(`UPDATE import_queue SET parsed_raw_json = '{}' WHERE id = ?`, id); err != nil {
		t.Fatalf("clear gate verdict: %v", err)
	}
}

var donorName = "Minions and Monsters 2026 PL.DUB.MD.1080p.AMZN.WEB-DL.XviD-MAXX.mp4"

// writeDonor creates the sibling donor the queue pairs with h.media targets.
func (h *harness) writeDonor(t *testing.T) string {
	t.Helper()
	path := filepath.Join(h.media, donorName)
	if err := os.WriteFile(path, []byte("donor"), 0o644); err != nil {
		t.Fatalf("write donor: %v", err)
	}
	return path
}

func TestListQueuePairsDonorAndExposesGate(t *testing.T) {
	h := newHarness(t)
	id := h.seedCandidate(t, "Minions and Monsters (2026) - [ArcA310][Remux-2160p][HDR10][Opus 7.1][AV1][imdb-tt32890033]-FraMeSToR.mkv", "eligible", `["en","pl"]`)

	donor := h.writeDonor(t)

	rec := h.request(http.MethodGet, "/api/queue")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/queue = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []struct {
			ID              int64    `json:"id"`
			Title           string   `json:"title"`
			TargetPath      string   `json:"target_path"`
			AudioLanguages  []string `json:"audio_languages"`
			OriginalLang    string   `json:"original_language"`
			Eligibility     string   `json:"eligibility"`
			DonorCandidates []struct {
				Filename string `json:"filename"`
				Path     string `json:"path"`
			} `json:"donor_candidates"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(body.Items))
	}
	item := body.Items[0]
	if item.ID != id || item.Title != "Minions and Monsters" || item.Eligibility != "eligible" {
		t.Errorf("item = %+v", item)
	}
	if item.TargetPath == "" || item.OriginalLang != "en" {
		t.Errorf("target/language missing: %+v", item)
	}
	if len(item.AudioLanguages) != 2 || item.AudioLanguages[1] != "pl" {
		t.Errorf("audio languages = %v", item.AudioLanguages)
	}
	if len(item.DonorCandidates) != 1 || item.DonorCandidates[0].Filename != filepath.Base(donor) {
		t.Errorf("donor candidates = %+v", item.DonorCandidates)
	}
}

func TestListQueueShowsWaitingForTags(t *testing.T) {
	h := newHarness(t)
	h.seedCandidate(t, "Shrek (2001) - [Remux-2160p][HDR10]-GROUP.mkv", "waiting_for_tags", `["en"]`)

	rec := h.request(http.MethodGet, "/api/queue")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/queue = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []struct {
			Eligibility string   `json:"eligibility"`
			MissingTags []string `json:"missing_tags"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 || body.Items[0].Eligibility != "waiting_for_tags" {
		t.Fatalf("items = %+v", body.Items)
	}
}

func TestApproveAndRejectTransitions(t *testing.T) {
	h := newHarness(t)
	id := h.seedCandidate(t, "Target [ArcA310].mkv", "eligible", `["en"]`)
	donor := h.writeDonor(t)

	rec := h.request(http.MethodPost, "/api/queue/"+itoa(id)+"/approve")
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, body %s", rec.Code, rec.Body.String())
	}
	var status, raw string
	if err := h.db.QueryRow(`SELECT status, COALESCE(parsed_raw_json, '{}') FROM import_queue WHERE id = ?`, id).Scan(&status, &raw); err != nil {
		t.Fatalf("status: %v", err)
	}
	var doc struct {
		Donor string `json:"donor"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("decode parsed_raw_json: %v", err)
	}
	if status != "extracting" {
		t.Errorf("status after approve = %q, want extracting", status)
	}
	if doc.Donor != donor {
		t.Errorf("persisted donor = %q, want %q", doc.Donor, donor)
	}

	// The candidate is no longer pending: a second action is a conflict.
	if rec := h.request(http.MethodPost, "/api/queue/"+itoa(id)+"/reject"); rec.Code != http.StatusConflict {
		t.Errorf("second transition = %d, want 409", rec.Code)
	}
	if rec := h.request(http.MethodPost, "/api/queue/99999/reject"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown candidate = %d, want 404", rec.Code)
	}
}

// TestApproveWithoutDonorStaysPending pins the live failure: an eligible target
// with no sibling donor must not enter extracting (the extractor would then
// probe the target file as if it were the donor).
func TestApproveWithoutDonorStaysPending(t *testing.T) {
	h := newHarness(t)
	id := h.seedCandidate(t, "Target [ArcA310].mkv", "eligible", `["en"]`)

	rec := h.request(http.MethodPost, "/api/queue/"+itoa(id)+"/approve")
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve without donor = %d, want 409, body %s", rec.Code, rec.Body.String())
	}
	var status string
	if err := h.db.QueryRow(`SELECT status FROM import_queue WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("status: %v", err)
	}
	if status != "pending_approval" {
		t.Errorf("status after rejected approve = %q, want pending_approval", status)
	}
}

func TestRejectMarksIgnored(t *testing.T) {
	h := newHarness(t)
	id := h.seedCandidate(t, "Target [ArcA310].mkv", "eligible", `["en"]`)

	if rec := h.request(http.MethodPost, "/api/queue/"+itoa(id)+"/reject"); rec.Code != http.StatusOK {
		t.Fatalf("reject = %d, body %s", rec.Code, rec.Body.String())
	}
	var status string
	if err := h.db.QueryRow(`SELECT status FROM import_queue WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("status: %v", err)
	}
	if status != "ignored" {
		t.Errorf("status after reject = %q, want ignored", status)
	}
	// Rejected rows leave the pending list.
	rec := h.request(http.MethodGet, "/api/queue")
	var body struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 0 {
		t.Errorf("pending items = %d, want 0", len(body.Items))
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// seedLibraryRow inserts an arr instance and one arr_library_cache row whose
// target file exists in h.media, returning the cache id and target path.
func (h *harness) seedLibraryRow(t *testing.T, title string, year int) (int64, string) {
	t.Helper()
	ctx := context.Background()
	res, err := h.db.ExecContext(ctx,
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		 VALUES ('aux', 'radarr', 'http://radarr.invalid', x'00', '0000')`)
	if err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	instanceID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("instance id: %v", err)
	}
	targetPath := filepath.Join(h.media, title+" (target).mkv")
	if err := os.WriteFile(targetPath, []byte("target"), 0o644); err != nil {
		t.Fatalf("write library target: %v", err)
	}
	res, err = h.db.ExecContext(ctx,
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, title, original_language_iso2, year,
		  target_file_path, audio_languages_json, eligibility_status)
		 VALUES (?, 'movie', ?, ?, 'en', ?, ?, '["en"]', 'eligible')`,
		instanceID, year, title, year, targetPath)
	if err != nil {
		t.Fatalf("insert library row: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("library id: %v", err)
	}
	return id, targetPath
}

// writeDonorInto drops the PL.DUB donor next to dir so approve can pair it.
func (h *harness) writeDonorInto(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, donorName)
	if err := os.WriteFile(path, []byte("donor"), 0o644); err != nil {
		t.Fatalf("write donor: %v", err)
	}
	return path
}

// seedUnmatchedCandidate records a pending donor with no arr match: the live
// watch-drop case that has no media identity yet.
func (h *harness) seedUnmatchedCandidate(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	source := filepath.Join(t.TempDir(), "Kraina.Lodu.2013.PL.mkv")
	st := &store.Store{DB: h.db}
	created, err := st.RecordImportCandidate(ctx, source, filepath.Base(source), 7)
	if err != nil || !created {
		t.Fatalf("RecordImportCandidate: created=%v err=%v", created, err)
	}
	var id int64
	if err := h.db.QueryRowContext(ctx,
		`SELECT id FROM import_queue WHERE source_file_path = ?`, source).Scan(&id); err != nil {
		t.Fatalf("candidate id: %v", err)
	}
	return id
}

func (h *harness) postJSON(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

func TestListQueueExposesMatchState(t *testing.T) {
	h := newHarness(t)
	h.seedCandidate(t, "Shrek (2001).mkv", "eligible", `["en"]`)
	h.seedUnmatchedCandidate(t)

	rec := h.request(http.MethodGet, "/api/queue")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/queue = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []struct {
			Filename     string `json:"filename"`
			Matched      bool   `json:"matched"`
			LibraryTitle string `json:"library_title"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}
	var matched, unmatched bool
	for _, item := range body.Items {
		if item.Matched {
			matched = true
			if item.LibraryTitle != "Minions and Monsters" {
				t.Errorf("library_title = %q, want Minions and Monsters", item.LibraryTitle)
			}
			continue
		}
		unmatched = true
		if item.LibraryTitle != "" {
			t.Errorf("unmatched library_title = %q, want empty", item.LibraryTitle)
		}
	}
	if !matched || !unmatched {
		t.Errorf("matched=%v unmatched=%v, want both", matched, unmatched)
	}
}

func TestQueueMatchStateUnmatched(t *testing.T) {
	h := newHarness(t)
	h.seedUnmatchedCandidate(t)

	item := onlyMatchItem(t, h.listMatchItems(t))
	if item.Matched {
		t.Errorf("matched = true, want false")
	}
	if item.MatchState != MatchStateUnmatched {
		t.Errorf("match_state = %q, want %q", item.MatchState, MatchStateUnmatched)
	}
	if !strings.Contains(strings.ToLower(item.MatchReason), "no arr title matched") {
		t.Errorf("match_reason = %q, want it to say no arr title matched", item.MatchReason)
	}
}

func TestQueueMatchStateMatchedNoDonor(t *testing.T) {
	h := newHarness(t)
	h.seedCandidate(t, "Shrek (2001) [Remux].mkv", "eligible", `["en"]`)

	item := onlyMatchItem(t, h.listMatchItems(t))
	if !item.Matched {
		t.Errorf("matched = false, want true")
	}
	if item.MatchState != MatchStateMatchedNoDonor {
		t.Errorf("match_state = %q, want %q", item.MatchState, MatchStateMatchedNoDonor)
	}
	if len(item.Donors) != 0 {
		t.Errorf("donor candidates = %d, want 0", len(item.Donors))
	}
	if !strings.Contains(item.MatchReason, "PL.DUB") {
		t.Errorf("match_reason = %q, want it to name the dubbing marker", item.MatchReason)
	}
}

func TestQueueMatchStateReady(t *testing.T) {
	h := newHarness(t)
	h.seedCandidate(t, "Target [ArcA310].mkv", "eligible", `["en"]`)
	h.writeDonor(t)

	item := onlyMatchItem(t, h.listMatchItems(t))
	if !item.Matched {
		t.Errorf("matched = false, want true")
	}
	if item.MatchState != MatchStateReady {
		t.Errorf("match_state = %q, want %q", item.MatchState, MatchStateReady)
	}
	if len(item.Donors) == 0 {
		t.Errorf("donor candidates = 0, want at least one")
	}
	if item.MatchReason != "Ready to approve." {
		t.Errorf("match_reason = %q, want %q", item.MatchReason, "Ready to approve.")
	}
}

func TestQueueMatchStateIneligibleTarget(t *testing.T) {
	h := newHarness(t)
	h.seedCandidate(t, "Shrek (2001) [Remux].mkv", "waiting_for_tags", `["en"]`)
	// A donor exists, but the target is not gate-eligible: the row must not
	// claim `ready`. handleApprove does not check eligibility, so this row is
	// in fact approvable and the button stays enabled — the card still must not
	// advertise it as ready, because that is a claim about the gate.
	h.writeDonor(t)

	item := onlyMatchItem(t, h.listMatchItems(t))
	if !item.Matched {
		t.Errorf("matched = false, want true")
	}
	if len(item.Donors) == 0 {
		t.Fatalf("donor candidates = 0, want at least one so eligibility alone decides")
	}
	if item.MatchState == MatchStateReady {
		t.Errorf("match_state = %q, want it not to be %q for an ineligible target", item.MatchState, MatchStateReady)
	}
	if item.MatchState != MatchStateMatchedNoDonor {
		t.Errorf("match_state = %q, want %q", item.MatchState, MatchStateMatchedNoDonor)
	}
	if !strings.Contains(item.MatchReason, "not eligible") {
		t.Errorf("match_reason = %q, want it to explain the eligibility verdict", item.MatchReason)
	}
	// The seed records no missing tag, so a waiting verdict can only come from
	// an exclusion: the reason must not invent a missing tag.
	if strings.Contains(strings.ToLower(item.MatchReason), "missing") {
		t.Errorf("match_reason = %q, want no missing-tag claim when the gate recorded none", item.MatchReason)
	}
	if !strings.Contains(strings.ToLower(item.MatchReason), "excluded") {
		t.Errorf("match_reason = %q, want it to name the excluded tag that blocked the target", item.MatchReason)
	}
}

// TestQueueMatchStateExcludedTagReason classifies a target that carries an
// excluded tag with the real gate rule, so the stored verdict has every
// required tag matched and nothing missing: the queue must blame the exclusion.
func TestQueueMatchStateExcludedTagReason(t *testing.T) {
	h := newHarness(t)
	target := "Shrek (2001) [ArcA310] [X].mkv"
	id := h.seedCandidate(t, target, "waiting_for_tags", `["en"]`)

	rule := gate.Rule{Required: []string{"[ArcA310]"}, Excluded: []string{"[X]"}, Mode: gate.MatchAny, Enforce: true}
	res := rule.Classify(target)
	if res.Status != gate.StatusWaitingForTags || len(res.Missing) != 0 {
		t.Fatalf("gate verdict = %+v, want waiting_for_tags with no missing tag", res)
	}
	h.applyGate(t, id, filepath.Join(h.media, target), res)

	item := onlyMatchItem(t, h.listMatchItems(t))
	if item.MatchState != MatchStateMatchedNoDonor {
		t.Errorf("match_state = %q, want %q", item.MatchState, MatchStateMatchedNoDonor)
	}
	lower := strings.ToLower(item.MatchReason)
	if !strings.Contains(lower, "excluded") {
		t.Errorf("match_reason = %q, want it to name the exclusion", item.MatchReason)
	}
	if strings.Contains(lower, "missing") {
		t.Errorf("match_reason = %q, want no missing-tag claim for an excluded tag", item.MatchReason)
	}
}

// TestQueueMatchStateMissingTagReason covers the other proven cause: the gate
// recorded the required tags it could not find.
func TestQueueMatchStateMissingTagReason(t *testing.T) {
	h := newHarness(t)
	target := "Shrek (2001) [Remux].mkv"
	id := h.seedCandidate(t, target, "waiting_for_tags", `["en"]`)

	rule := gate.Rule{Required: []string{"[ArcA310]"}, Mode: gate.MatchAny, Enforce: true}
	res := rule.Classify(target)
	if res.Status != gate.StatusWaitingForTags || len(res.Missing) != 1 {
		t.Fatalf("gate verdict = %+v, want waiting_for_tags naming one missing tag", res)
	}
	h.applyGate(t, id, filepath.Join(h.media, target), res)

	item := onlyMatchItem(t, h.listMatchItems(t))
	lower := strings.ToLower(item.MatchReason)
	if !strings.Contains(item.MatchReason, "[ArcA310]") {
		t.Errorf("match_reason = %q, want it to name the missing tag", item.MatchReason)
	}
	if !strings.Contains(lower, "missing") {
		t.Errorf("match_reason = %q, want it to say the required tag is missing", item.MatchReason)
	}
	if strings.Contains(lower, "excluded") {
		t.Errorf("match_reason = %q, want no exclusion claim when the gate recorded a missing tag", item.MatchReason)
	}
}

// TestQueueMatchStateNoGateVerdictIsNeutral covers a row whose eligibility came
// from the arr library row (arrsync writes waiting_for_tags for a missing arr
// tag or a target outside the root-folder allowlist) with no gate record: the
// queue cannot tell those causes apart and must not name one.
func TestQueueMatchStateNoGateVerdictIsNeutral(t *testing.T) {
	h := newHarness(t)
	id := h.seedCandidate(t, "Shrek (2001) [Remux].mkv", "waiting_for_tags", `["en"]`)
	h.clearGateVerdict(t, id)

	item := onlyMatchItem(t, h.listMatchItems(t))
	if item.MatchState != MatchStateMatchedNoDonor {
		t.Errorf("match_state = %q, want %q", item.MatchState, MatchStateMatchedNoDonor)
	}
	lower := strings.ToLower(item.MatchReason)
	if !strings.Contains(lower, "waiting for tags") {
		t.Errorf("match_reason = %q, want the cause-neutral gate wording", item.MatchReason)
	}
	for _, forbidden := range []string{"missing", "excluded"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("match_reason = %q, want no %q claim without a gate record", item.MatchReason, forbidden)
		}
	}
}

// TestQueueMatchStateUnreadableTargetFolder pins that an unlistable folder is
// reported as such: an unmounted library has no donors to see, so claiming that
// no sibling carries the marker would be a confident falsehood.
func TestQueueMatchStateUnreadableTargetFolder(t *testing.T) {
	h := newHarness(t)
	h.seedCandidate(t, "Target [ArcA310].mkv", "eligible", `["en"]`)
	h.writeDonor(t)
	// Simulate the mount going away after ingest: the target path survives in
	// the row, its folder does not.
	if err := os.RemoveAll(h.media); err != nil {
		t.Fatalf("remove media dir: %v", err)
	}

	item := onlyMatchItem(t, h.listMatchItems(t))
	if item.MatchState != MatchStateMatchedNoDonor {
		t.Errorf("match_state = %q, want %q", item.MatchState, MatchStateMatchedNoDonor)
	}
	if len(item.Donors) != 0 {
		t.Errorf("donor candidates = %d, want 0", len(item.Donors))
	}
	if !strings.Contains(item.MatchReason, "could not be read") {
		t.Errorf("match_reason = %q, want it to report the unreadable folder", item.MatchReason)
	}
	if !strings.Contains(item.MatchReason, "library mount") {
		t.Errorf("match_reason = %q, want it to point at the library mount", item.MatchReason)
	}
	if strings.Contains(item.MatchReason, "dubbing marker") {
		t.Errorf("match_reason = %q, want no marker claim for a folder it could not list", item.MatchReason)
	}
}

// TestQueueMatchStateBlankDonorPattern pins that a blank pattern list produces
// no marker claim: there is no marker to look for, so the reason only says no
// sibling file is eligible.
func TestQueueMatchStateBlankDonorPattern(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "Target [ArcA310].mkv")
	if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	donors, err := pairDonors(target, []string{""})
	if err != nil {
		t.Fatalf("pairDonors: %v", err)
	}
	if len(donors) != 0 {
		t.Fatalf("donor candidates = %d, want 0", len(donors))
	}

	row := store.ImportCandidateSummary{Matched: true, TargetPath: target, Eligibility: "eligible"}
	state, reason := deriveMatchState(row, donors, err, []string{""})
	if state != MatchStateMatchedNoDonor {
		t.Errorf("match_state = %q, want %q", state, MatchStateMatchedNoDonor)
	}
	if reason != "No sibling file is eligible as a donor." {
		t.Errorf("match_reason = %q, want the neutral donor wording", reason)
	}
}

// TestApproveUnreadableTargetFolderReportsMount pins that approve words the
// same failure as the card: an unlistable folder is a mount problem, not proof
// that no donor exists.
func TestApproveUnreadableTargetFolderReportsMount(t *testing.T) {
	h := newHarness(t)
	id := h.seedCandidate(t, "Target [ArcA310].mkv", "eligible", `["en"]`)
	h.writeDonor(t)
	if err := os.RemoveAll(h.media); err != nil {
		t.Fatalf("remove media dir: %v", err)
	}

	rec := h.request(http.MethodPost, "/api/queue/"+itoa(id)+"/approve")
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve with unreadable folder = %d, want 409, body %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "library mount") {
		t.Errorf("body = %s, want it to point at the library mount", body)
	}
	var status string
	if err := h.db.QueryRow(`SELECT status FROM import_queue WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("status: %v", err)
	}
	if status != "pending_approval" {
		t.Errorf("status after refused approve = %q, want pending_approval", status)
	}
}

// TestQueueMatchStateReadyRequiresEligibility pins that ready means approvable
// now: it is produced only for a matched, gate-eligible target that has a donor.
func TestQueueMatchStateReadyRequiresEligibility(t *testing.T) {
	tests := []struct {
		name       string
		target     string
		seedElig   string
		withDonor  bool
		wantReady  bool
		wantReason string
		// wantReasonAny accepts any of several phrasings of the same blocker;
		// used where the wording is not pinned (the reason may name either the
		// missing donor or the dubbing marker the donor must carry).
		wantReasonAny []string
	}{
		{
			name:      "eligible with donor",
			target:    "Target [ArcA310].mkv",
			seedElig:  "eligible",
			withDonor: true,
			wantReady: true,
		},
		{
			name:      "eligible without donor",
			target:    "Target [ArcA310].mkv",
			seedElig:  "eligible",
			wantReady: false,
			// The reason must blame the missing donor, naming either the donor
			// itself or the dubbing marker a donor release has to carry.
			wantReasonAny: []string{"no donor", "dubbing marker", "sibling"},
		},
		{
			name:       "waiting for tags with donor",
			target:     "Target [ArcA310].mkv",
			seedElig:   "waiting_for_tags",
			withDonor:  true,
			wantReady:  false,
			wantReason: "not eligible",
		},
		{
			name:       "missing target with donor",
			target:     "Target [ArcA310].mkv",
			seedElig:   "missing_target",
			withDonor:  true,
			wantReady:  false,
			wantReason: "not eligible",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.seedCandidate(t, tc.target, tc.seedElig, `["en"]`)
			if tc.withDonor {
				h.writeDonor(t)
			}

			item := onlyMatchItem(t, h.listMatchItems(t))
			if !item.Matched {
				t.Fatalf("matched = false, want true")
			}
			if gotReady := item.MatchState == MatchStateReady; gotReady != tc.wantReady {
				t.Errorf("match_state = %q, want ready=%v", item.MatchState, tc.wantReady)
			}
			if !tc.wantReady {
				if item.MatchState != MatchStateMatchedNoDonor {
					t.Errorf("match_state = %q, want %q", item.MatchState, MatchStateMatchedNoDonor)
				}
				lowerReason := strings.ToLower(item.MatchReason)
				if len(tc.wantReasonAny) > 0 {
					mentioned := false
					for _, want := range tc.wantReasonAny {
						if strings.Contains(lowerReason, want) {
							mentioned = true
							break
						}
					}
					if !mentioned {
						t.Errorf("match_reason = %q, want it to name the missing donor (any of %v)", item.MatchReason, tc.wantReasonAny)
					}
					// The target is eligible here: the reason must blame the donor,
					// not the gate verdict.
					if strings.Contains(lowerReason, "not eligible") {
						t.Errorf("match_reason = %q, want it to blame the missing donor, not eligibility", item.MatchReason)
					}
				}
				if tc.wantReason != "" && !strings.Contains(lowerReason, tc.wantReason) {
					t.Errorf("match_reason = %q, want it to mention %q", item.MatchReason, tc.wantReason)
				}
			}
		})
	}
}

// TestQueueMatchStateConsistentWithMatched pins the invariant the UI relies on:
// match_state is unmatched exactly when matched is false.
func TestQueueMatchStateConsistentWithMatched(t *testing.T) {
	tests := []struct {
		name        string
		seed        func(*harness)
		wantMatched bool
		wantState   string
	}{
		{
			name:        "no arr target",
			seed:        func(h *harness) { h.seedUnmatchedCandidate(h.t) },
			wantMatched: false,
			wantState:   MatchStateUnmatched,
		},
		{
			name:        "target without donor",
			seed:        func(h *harness) { h.seedCandidate(h.t, "Shrek (2001).mkv", "eligible", `["en"]`) },
			wantMatched: true,
			wantState:   MatchStateMatchedNoDonor,
		},
		{
			name: "target with donor",
			seed: func(h *harness) {
				h.seedCandidate(h.t, "Target [ArcA310].mkv", "eligible", `["en"]`)
				h.writeDonor(h.t)
			},
			wantMatched: true,
			wantState:   MatchStateReady,
		},
		{
			name:        "ineligible target",
			seed:        func(h *harness) { h.seedCandidate(h.t, "Shrek (2001).mkv", "waiting_for_tags", `["en"]`) },
			wantMatched: true,
			wantState:   MatchStateMatchedNoDonor,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.seed(h)

			item := onlyMatchItem(t, h.listMatchItems(t))
			if item.Matched != tc.wantMatched {
				t.Errorf("matched = %v, want %v", item.Matched, tc.wantMatched)
			}
			if gotUnmatched := item.MatchState == MatchStateUnmatched; gotUnmatched != !item.Matched {
				t.Errorf("invariant broken: matched=%v match_state=%q", item.Matched, item.MatchState)
			}
			if item.MatchState != tc.wantState {
				t.Errorf("match_state = %q, want %q", item.MatchState, tc.wantState)
			}
			if strings.TrimSpace(item.MatchReason) == "" {
				t.Errorf("match_reason is empty, want a human explanation")
			}
		})
	}
}

// queueMatchItem is the match-state slice of one GET /api/queue item.
type queueMatchItem struct {
	ID          int64  `json:"id"`
	Filename    string `json:"filename"`
	Matched     bool   `json:"matched"`
	MatchState  string `json:"match_state"`
	MatchReason string `json:"match_reason"`
	Donors      []struct {
		Path string `json:"path"`
	} `json:"donor_candidates"`
}

func (h *harness) listMatchItems(t *testing.T) []queueMatchItem {
	t.Helper()
	rec := h.request(http.MethodGet, "/api/queue")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/queue = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []queueMatchItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Items
}

func onlyMatchItem(t *testing.T, items []queueMatchItem) queueMatchItem {
	t.Helper()
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	return items[0]
}

func TestSearchLibraryEndpoint(t *testing.T) {
	h := newHarness(t)
	want, _ := h.seedLibraryRow(t, "Kraina Lodu", 2013)
	h.seedLibraryRow(t, "Shrek", 2001)

	rec := h.request(http.MethodGet, "/api/imports/search-library?q=lodu")
	if rec.Code != http.StatusOK {
		t.Fatalf("search = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []struct {
			ID               int64  `json:"id"`
			Title            string `json:"title"`
			Year             int    `json:"year"`
			MediaType        string `json:"media_type"`
			OriginalLanguage string `json:"original_language_iso2"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items = %+v, want 1", body.Items)
	}
	got := body.Items[0]
	if got.ID != want || got.Title != "Kraina Lodu" || got.Year != 2013 ||
		got.MediaType != "movie" || got.OriginalLanguage != "en" {
		t.Errorf("item = %+v", got)
	}

	blank := h.request(http.MethodGet, "/api/imports/search-library?q=")
	if blank.Code != http.StatusOK {
		t.Fatalf("blank search = %d", blank.Code)
	}
	var empty struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(blank.Body.Bytes(), &empty); err != nil {
		t.Fatalf("decode blank: %v", err)
	}
	if len(empty.Items) != 0 {
		t.Errorf("blank query items = %d, want 0", len(empty.Items))
	}
}

func TestSearchLibraryWithoutStoreIs503(t *testing.T) {
	mux := http.NewServeMux()
	RegisterRoutes(mux, Dependencies{RequireAdmin: func(h http.Handler) http.Handler { return h }})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/imports/search-library?q=x", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("search without store = %d, want 503", rec.Code)
	}
}

func TestApproveWithManualLibraryMatch(t *testing.T) {
	h := newHarness(t)
	libraryID, libraryPath := h.seedLibraryRow(t, "Kraina Lodu", 2013)
	id := h.seedUnmatchedCandidate(t)
	h.writeDonorInto(t, filepath.Dir(libraryPath))

	rec := h.postJSON(t, "/api/queue/"+itoa(id)+"/approve", `{"library_cache_id":`+itoa(libraryID)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, body %s", rec.Code, rec.Body.String())
	}
	var status string
	var matched *int64
	if err := h.db.QueryRow(`SELECT status, matched_library_cache_id FROM import_queue WHERE id = ?`, id).
		Scan(&status, &matched); err != nil {
		t.Fatalf("row: %v", err)
	}
	if status != "extracting" {
		t.Errorf("status = %q, want extracting", status)
	}
	if matched == nil || *matched != libraryID {
		t.Errorf("matched_library_cache_id = %v, want %d", matched, libraryID)
	}
	job, ok, err := (&store.Store{DB: h.db}).NextImportExtraction(context.Background())
	if err != nil || !ok {
		t.Fatalf("NextImportExtraction = (ok=%v, err=%v)", ok, err)
	}
	if job.Title != "Kraina Lodu" || job.Year != 2013 {
		t.Errorf("extraction identity = %+v, want Kraina Lodu 2013", job)
	}
}

func TestApproveUnmatchedWithoutLibraryIsConflict(t *testing.T) {
	h := newHarness(t)
	id := h.seedUnmatchedCandidate(t)

	rec := h.postJSON(t, "/api/queue/"+itoa(id)+"/approve", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve unmatched = %d, want 409, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no matched library target") {
		t.Errorf("body = %s, want 'no matched library target'", rec.Body.String())
	}
	var status string
	if err := h.db.QueryRow(`SELECT status FROM import_queue WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("row: %v", err)
	}
	if status != "pending_approval" {
		t.Errorf("status = %q, want pending_approval", status)
	}
}

func TestApproveRejectsForeignLibraryMatch(t *testing.T) {
	h := newHarness(t)
	id := h.seedCandidate(t, "Minions (2026).mkv", "eligible", `["en"]`)
	h.writeDonor(t)
	other, _ := h.seedLibraryRow(t, "Shrek", 2001)

	rec := h.postJSON(t, "/api/queue/"+itoa(id)+"/approve", `{"library_cache_id":`+itoa(other)+`}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve with foreign library = %d, want 409, body %s", rec.Code, rec.Body.String())
	}
	// An already-matched row rejects any foreign id, including unknown ones.
	if rec := h.postJSON(t, "/api/queue/"+itoa(id)+"/approve", `{"library_cache_id":99999}`); rec.Code != http.StatusConflict {
		t.Errorf("approve matched row with unknown library = %d, want 409", rec.Code)
	}
	// An unmatched row reports an unknown id as not found.
	unmatched := h.seedUnmatchedCandidate(t)
	if rec := h.postJSON(t, "/api/queue/"+itoa(unmatched)+"/approve", `{"library_cache_id":99999}`); rec.Code != http.StatusNotFound {
		t.Errorf("approve unmatched row with unknown library = %d, want 404", rec.Code)
	}
}
