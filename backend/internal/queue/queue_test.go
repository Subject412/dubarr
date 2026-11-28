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
	t        *testing.T
	db       *sql.DB
	mux      *http.ServeMux
	media    string
	settings *settings.Store
	// seq keeps the vault paths seedSyncJob writes unique per test: audio_tracks
	// .file_path is UNIQUE.
	seq int
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
	return &harness{t: t, db: st.DB, mux: mux, media: t.TempDir(), settings: settingsStore}
}

// enableForceTagOverride turns settings.security.options.allow_force_tag_override
// on the way the Settings screen does, so the approval gate reads a real
// section instead of a test-invented flag.
func (h *harness) enableForceTagOverride(t *testing.T) {
	t.Helper()
	raw, err := json.Marshal(settings.SecurityOptions{AllowForceTagOverride: true})
	if err != nil {
		t.Fatalf("marshal security options: %v", err)
	}
	if err := h.settings.Set(context.Background(), settings.KeySecurityOptions, raw); err != nil {
		t.Fatalf("enable force override: %v", err)
	}
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
		Reason:      res.Reason,
	}); err != nil {
		t.Fatalf("SetImportCandidateGate: %v", err)
	}
}

// writeTagFilterRow writes target.tag_filter straight into the settings table,
// bypassing save validation. A pattern the regex engine refuses can only reach
// the store as a hand-written/legacy row now — saving one is refused (#155) —
// and that row is exactly what a queue card has to explain, so a test that needs
// one writes it the way it can still exist.
func (h *harness) writeTagFilterRow(t *testing.T, section string) {
	t.Helper()
	if _, err := h.db.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		settings.KeyTargetTagFilter, section, "2026-01-01 00:00:00"); err != nil {
		t.Fatalf("write target.tag_filter row: %v", err)
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

// TestQueueExposesGateReason pins #155 end to end for the API: a target the
// gate classified as waiting on a pattern that does not compile carries the
// reason on its row, next to the missing tags, so the card can say why the
// wait cannot end. The status is unchanged — the target still waits.
func TestQueueExposesGateReason(t *testing.T) {
	h := newHarness(t)
	const targetName = "Shrek (2001) [ArcA310].mkv"
	id := h.seedCandidate(t, targetName, "waiting_for_tags", `["en"]`)

	targetPath := filepath.Join(h.media, targetName)
	// A pattern the engine refuses can only be in the store as a hand-written or
	// legacy row: saving one is refused now (#155). The card still has to explain
	// that row, so it is written the way it can exist — and the listing must keep
	// reporting it, because re-proving a still-broken pattern changes nothing.
	h.writeTagFilterRow(t, `{"required_filename_tags":["\\d+\\1"],"tag_match_mode":"REGEX","enforce_tag_filter":true}`)
	rule := gate.Rule{Required: []string{`\d+\1`}, Mode: gate.MatchRegex, Enforce: true}
	res := rule.Classify(targetPath)
	if res.Status != gate.StatusWaitingForTags || res.Reason == "" {
		t.Fatalf("classification = %+v, want waiting_for_tags carrying a reason", res)
	}
	h.applyGate(t, id, targetPath, res)

	rec := h.request(http.MethodGet, "/api/queue")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/queue = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []struct {
			Eligibility string   `json:"eligibility"`
			MissingTags []string `json:"missing_tags"`
			GateReason  string   `json:"gate_reason"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items = %+v", body.Items)
	}
	item := body.Items[0]
	if item.Eligibility != "waiting_for_tags" {
		t.Errorf("eligibility = %q, want waiting_for_tags", item.Eligibility)
	}
	if !strings.Contains(item.GateReason, "filename tag pattern") {
		t.Errorf("gate_reason = %q, want it to name the pattern", item.GateReason)
	}
	if len(item.MissingTags) != 1 || item.MissingTags[0] != `\d+\1` {
		t.Errorf("missing_tags = %v, want the offending pattern", item.MissingTags)
	}
}

// TestQueueReclassifiesAStaleGateReason pins #155's other half: the gate verdict
// and its reason are written once, at discovery, and nothing reclassified an
// existing row — so an operator who fixed the pattern kept reading "Cannot
// become eligible" for a target that is fine now. Listing the queue re-proves
// the verdict with the rule as it stands, so the reason is gone and the status
// is the fixed pattern's verdict, not the typo's.
func TestQueueReclassifiesAStaleGateReason(t *testing.T) {
	h := newHarness(t)
	const targetName = "Shrek (2001) [ArcA310].mkv"
	id := h.seedCandidate(t, targetName, "waiting_for_tags", `["en"]`)
	targetPath := filepath.Join(h.media, targetName)

	// What discovery stored: the configured pattern did not compile, so the row
	// waits with a reason no tag can answer.
	broken := gate.Rule{Required: []string{`\d+\1`}, Mode: gate.MatchRegex, Enforce: true}
	stale := broken.Classify(targetPath)
	if stale.Status != gate.StatusWaitingForTags || stale.Reason == "" {
		t.Fatalf("stale classification = %+v, want waiting_for_tags carrying a reason", stale)
	}
	h.applyGate(t, id, targetPath, stale)

	// The operator fixes the typo and saves: the section now carries a pattern
	// that compiles and matches the target's tag, which the settings form
	// accepts. The stored row is not touched by the save.
	raw, err := json.Marshal(settings.TargetTagFilter{
		RequiredFilenameTags: []string{`\[ArcA310\]`},
		TagMatchMode:         "REGEX",
		EnforceTagFilter:     true,
	})
	if err != nil {
		t.Fatalf("marshal target.tag_filter: %v", err)
	}
	if err := h.settings.Set(context.Background(), settings.KeyTargetTagFilter, raw); err != nil {
		t.Fatalf("save target.tag_filter: %v", err)
	}

	rec := h.request(http.MethodGet, "/api/queue")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/queue = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []struct {
			Eligibility string   `json:"eligibility"`
			MissingTags []string `json:"missing_tags"`
			GateReason  string   `json:"gate_reason"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items = %+v", body.Items)
	}
	item := body.Items[0]
	if item.GateReason != "" {
		t.Errorf("gate_reason = %q, want it gone after the pattern was fixed", item.GateReason)
	}
	if item.Eligibility != "eligible" {
		t.Errorf("eligibility = %q, want eligible: the target carries the tag", item.Eligibility)
	}
	if len(item.MissingTags) != 0 {
		t.Errorf("missing_tags = %v, want none: the pattern that produced them is fixed", item.MissingTags)
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
	if item.MatchBlocker != MatchBlockerNoMatch {
		t.Errorf("match_blocker = %q, want %q", item.MatchBlocker, MatchBlockerNoMatch)
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
	if item.MatchBlocker != MatchBlockerNoDonor {
		t.Errorf("match_blocker = %q, want %q", item.MatchBlocker, MatchBlockerNoDonor)
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
	if item.MatchBlocker != MatchBlockerNone {
		t.Errorf("match_blocker = %q, want %q", item.MatchBlocker, MatchBlockerNone)
	}
}

// TestQueueMatchStateIneligibleTargetIsReady is the #48 widening: a matched row
// with a target file and a donor is approvable whatever the gate says, because
// approve records intent and the job born after the extraction is the thing that
// holds. The card must therefore offer the button and still report the verdict
// beside it, which is what Eligibility carries.
func TestQueueMatchStateIneligibleTargetIsReady(t *testing.T) {
	h := newHarness(t)
	h.seedCandidate(t, "Shrek (2001) [Remux].mkv", "waiting_for_tags", `["en"]`)
	h.writeDonor(t)

	item := onlyMatchItem(t, h.listMatchItems(t))
	if !item.Matched {
		t.Errorf("matched = false, want true")
	}
	if len(item.Donors) == 0 {
		t.Fatalf("donor candidates = 0, want at least one so the target can be approved")
	}
	if item.MatchState != MatchStateReady {
		t.Errorf("match_state = %q, want %q: an ineligible pair is approvable and simply held",
			item.MatchState, MatchStateReady)
	}
	if item.MatchBlocker != MatchBlockerNone {
		t.Errorf("match_blocker = %q, want %q", item.MatchBlocker, MatchBlockerNone)
	}
	if item.MatchReason != "Ready to approve." {
		t.Errorf("match_reason = %q, want %q", item.MatchReason, "Ready to approve.")
	}
}

// TestQueueMatchStateGateVerdictIsNotABlocker covers the three verdict shapes the
// derivation used to blame — an excluded tag, a missing required tag and a
// waiting verdict with no gate record — and pins that none of them is a blocker:
// with a donor the row is ready, and without one the only blocker left is the
// donor.
func TestQueueMatchStateGateVerdictIsNotABlocker(t *testing.T) {
	verdicts := []struct {
		name      string
		target    string
		hasRecord bool
		gate      gate.Result
	}{
		{
			name:      "excluded tag",
			target:    "Shrek (2001) [ArcA310] [X].mkv",
			hasRecord: true,
			gate: gate.Rule{Required: []string{"[ArcA310]"}, Excluded: []string{"[X]"}, Mode: gate.MatchAny, Enforce: true}.
				Classify("Shrek (2001) [ArcA310] [X].mkv"),
		},
		{
			name:      "missing required tag",
			target:    "Shrek (2001) [Remux].mkv",
			hasRecord: true,
			gate: gate.Rule{Required: []string{"[ArcA310]"}, Mode: gate.MatchAny, Enforce: true}.
				Classify("Shrek (2001) [Remux].mkv"),
		},
		{
			name:   "no gate record at all",
			target: "Shrek (2001) [Remux].mkv",
		},
	}

	for _, tc := range verdicts {
		t.Run(tc.name, func(t *testing.T) {
			if tc.hasRecord && tc.gate.Status != gate.StatusWaitingForTags {
				t.Fatalf("gate verdict = %+v, want %q", tc.gate, gate.StatusWaitingForTags)
			}
			h := newHarness(t)
			id := h.seedCandidate(t, tc.target, "waiting_for_tags", `["en"]`)
			if tc.hasRecord {
				h.applyGate(t, id, filepath.Join(h.media, tc.target), tc.gate)
			} else {
				h.clearGateVerdict(t, id)
			}

			// Without a donor the row is matched but not actionable, and the
			// only cause left to name is the missing donor.
			blocked := onlyMatchItem(t, h.listMatchItems(t))
			if blocked.MatchState != MatchStateMatchedNoDonor {
				t.Errorf("no donor: match_state = %q, want %q", blocked.MatchState, MatchStateMatchedNoDonor)
			}
			if blocked.MatchBlocker != MatchBlockerNoDonor {
				t.Errorf("no donor: match_blocker = %q, want %q", blocked.MatchBlocker, MatchBlockerNoDonor)
			}
			if strings.Contains(strings.ToLower(blocked.MatchReason), "not eligible") {
				t.Errorf("no donor: match_reason = %q, want no eligibility claim", blocked.MatchReason)
			}

			// With a donor the same row is approvable: the verdict moved into
			// the hold the job will be born with, not into the button.
			h.writeDonor(t)
			ready := onlyMatchItem(t, h.listMatchItems(t))
			if ready.MatchState != MatchStateReady {
				t.Errorf("with donor: match_state = %q, want %q", ready.MatchState, MatchStateReady)
			}
			if ready.MatchBlocker != MatchBlockerNone {
				t.Errorf("with donor: match_blocker = %q, want %q", ready.MatchBlocker, MatchBlockerNone)
			}
		})
	}
}

// TestDonorPatternsAreFixed is U11: the markers are a property of how Polish
// dubs are named, not a preference, so the fixed list is the only source and it
// carries exactly the three the reference workflow uses.
func TestDonorPatternsAreFixed(t *testing.T) {
	want := []string{"PL.DUB", "LEKTOR.PL", "DUB.PL"}
	patterns := DonorPatterns()
	if len(patterns) != len(want) {
		t.Fatalf("DonorPatterns() = %v, want %v", patterns, want)
	}
	for i, marker := range want {
		if patterns[i] != marker {
			t.Fatalf("DonorPatterns() = %v, want %v", patterns, want)
		}
	}
	for _, marker := range want {
		if !matchesDonorPattern("Film.2020."+marker+".mkv", patterns) {
			t.Errorf("a filename carrying %q was not detected as a donor", marker)
		}
	}
	// The accessor hands out a copy: mutating it must not rewrite the markers
	// the pairing reads on its next call.
	patterns[0] = "CUSTOM"
	if again := DonorPatterns(); again[0] != want[0] {
		t.Fatalf("DonorPatterns() = %v after a caller mutated its copy, want %v", again, want)
	}
}

// TestDonorMarkerMatchingSemantics pins what the fixed list inherits from
// pairDonors' matcher, because the markers are now the only thing an operator
// cannot tune (identity-based pairing is #130).
//
// A marker without "*" is a substring match, compared case-insensitively:
//
//   - pl.dub and PL.DUB are the same marker, so a lower-case rip still pairs;
//   - PL.DUBB and DUB.PL1 still pair, because "PL.DUB"/"DUB.PL" are substrings
//     of them. Widening this to exact tokens is a matching-rule change, not a
//     settings change, so it is recorded here rather than fixed here.
//   - a different naming convention ("POL", "DUB PL") is not a donor at all:
//     the row reports the donor_pattern blocker and the approval card still
//     lists every sibling, so the operator can pick one by hand.
func TestDonorMarkerMatchingSemantics(t *testing.T) {
	tests := []struct {
		name string
		file string
		want bool
	}{
		{"pl.dub marker", "Film.2020.PL.DUB.mkv", true},
		{"lektor.pl marker", "Film.2020.LEKTOR.PL.mkv", true},
		{"dub.pl marker", "Film.2020.DUB.PL.mkv", true},
		{"lower-case marker still pairs", "film.2020.pl.dub.mkv", true},
		{"trailing letter is still a substring", "Film.2020.PL.DUBB.mkv", true},
		{"trailing digit is still a substring", "Film.2020.DUB.PL1.mkv", true},
		{"a different convention does not pair", "Film.2020.POL.mkv", false},
		{"a spaced convention does not pair", "Film.2020.DUB PL.mkv", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesDonorPattern(tc.file, DonorPatterns()); got != tc.want {
				t.Errorf("matchesDonorPattern(%q, DonorPatterns()) = %v, want %v", tc.file, got, tc.want)
			}
		})
	}
}

// TestRetiredDonorPatternSettingIsInert is U11: a row written by an install that
// still had the `imports.options` card is kept but never read, so a stored
// "CUSTOM" list neither suppresses the PL.DUB pairing nor pairs a CUSTOM
// sibling. The row is left byte-identical.
func TestRetiredDonorPatternSettingIsInert(t *testing.T) {
	tests := []struct {
		name  string
		store bool
	}{
		{name: "stored CUSTOM list", store: true},
		{name: "no stored row", store: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.seedCandidate(t,
				"Minions and Monsters (2026) - [ArcA310][Remux-2160p][HDR10][Opus 7.1][AV1][imdb-tt32890033]-FraMeSToR.mkv",
				"eligible", `["en","pl"]`)
			for _, name := range []string{"Film.2020.PL.DUB.mkv", "Film.2020.CUSTOM.mkv"} {
				if err := os.WriteFile(filepath.Join(h.media, name), []byte("donor"), 0o644); err != nil {
					t.Fatalf("write %s: %v", name, err)
				}
			}
			legacy := `{"donor_patterns":["CUSTOM"]}`
			if tc.store {
				if _, err := h.db.Exec(
					`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES ('imports.options', ?, '2026-01-01 00:00:00')`,
					legacy); err != nil {
					t.Fatalf("store retired row: %v", err)
				}
			}

			item := onlyMatchItem(t, h.listMatchItems(t))
			if item.MatchState != MatchStateReady {
				t.Fatalf("match_state = %q (%s), want %q", item.MatchState, item.MatchReason, MatchStateReady)
			}
			if len(item.Donors) != 1 {
				t.Fatalf("donor candidates = %d, want only the PL.DUB sibling", len(item.Donors))
			}
			if got := filepath.Base(item.Donors[0].Path); got != "Film.2020.PL.DUB.mkv" {
				t.Errorf("paired donor = %q, want Film.2020.PL.DUB.mkv", got)
			}
			patterns := DonorPatterns()
			if len(item.DonorPatterns) != len(patterns) {
				t.Fatalf("item donor_patterns = %v, want the fixed list %v", item.DonorPatterns, patterns)
			}
			for i, marker := range patterns {
				if item.DonorPatterns[i] != marker {
					t.Errorf("item donor_patterns = %v, want %v", item.DonorPatterns, patterns)
				}
			}
			if tc.store {
				var stored string
				if err := h.db.QueryRow(`SELECT value FROM settings WHERE key = 'imports.options'`).Scan(&stored); err != nil {
					t.Fatalf("read retired row: %v", err)
				}
				if stored != legacy {
					t.Errorf("retired row was rewritten: %s, want %s", stored, legacy)
				}
			} else {
				var n int
				if err := h.db.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = 'imports.options'`).Scan(&n); err != nil {
					t.Fatalf("count retired row: %v", err)
				}
				if n != 0 {
					t.Errorf("settings seeded a retired imports.options row")
				}
			}
		})
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
	if item.MatchBlocker != MatchBlockerMount {
		t.Errorf("match_blocker = %q, want %q", item.MatchBlocker, MatchBlockerMount)
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
	state, reason, blocker := deriveMatchState(row, donors, err, []string{""})
	if state != MatchStateMatchedNoDonor {
		t.Errorf("match_state = %q, want %q", state, MatchStateMatchedNoDonor)
	}
	if reason != "No sibling file is eligible as a donor." {
		t.Errorf("match_reason = %q, want the neutral donor wording", reason)
	}
	if blocker != MatchBlockerNoDonor {
		t.Errorf("match_blocker = %q, want %q", blocker, MatchBlockerNoDonor)
	}
}

// TestQueueMatchStateNoTargetFileBlocker pins the machine-readable side of the
// no-target case: a matched row without a target path reports no_target. A
// missing_target verdict on its own is not the blocker any more (#48) — the pair
// is approvable and the job is born held — so a row that does record a path
// falls through to the donor branches.
func TestQueueMatchStateNoTargetFileBlocker(t *testing.T) {
	tests := []struct {
		name string
		row  store.ImportCandidateSummary
		want string
	}{
		{
			name: "matched without target path",
			row:  store.ImportCandidateSummary{Matched: true, Eligibility: "eligible"},
			want: MatchBlockerNoTarget,
		},
		{
			name: "gate recorded a missing target file and there is no path",
			row:  store.ImportCandidateSummary{Matched: true, Eligibility: "missing_target"},
			want: MatchBlockerNoTarget,
		},
		{
			name: "missing target file with a recorded path is a donor question",
			row:  store.ImportCandidateSummary{Matched: true, TargetPath: "/media/Target.mkv", Eligibility: "missing_target"},
			want: MatchBlockerNoDonor,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state, reason, blocker := deriveMatchState(tc.row, nil, nil, DonorPatterns())
			if state != MatchStateMatchedNoDonor {
				t.Errorf("match_state = %q, want %q", state, MatchStateMatchedNoDonor)
			}
			if strings.TrimSpace(reason) == "" {
				t.Errorf("match_reason is empty, want a human explanation")
			}
			if blocker != tc.want {
				t.Errorf("match_blocker = %q, want %q", blocker, tc.want)
			}
		})
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

// TestQueueMatchStateReadyIsApprovalReadiness pins that ready means approvable
// now: a matched target with a donor, whatever the gate said. The verdict a
// target carries decides whether the job born after the extraction waits, not
// whether the operator may approve (see TestApproveIneligibleTargetRecordsIntent).
func TestQueueMatchStateReadyIsApprovalReadiness(t *testing.T) {
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
			name:      "waiting for tags with donor is approvable",
			target:    "Target [ArcA310].mkv",
			seedElig:  "waiting_for_tags",
			withDonor: true,
			wantReady: true,
		},
		{
			name:      "missing target with donor is approvable",
			target:    "Target [ArcA310].mkv",
			seedElig:  "missing_target",
			withDonor: true,
			wantReady: true,
		},
		{
			name:          "waiting for tags without donor",
			target:        "Target [ArcA310].mkv",
			seedElig:      "waiting_for_tags",
			wantReady:     false,
			wantReasonAny: []string{"no donor", "dubbing marker", "sibling"},
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

// TestQueueMatchBlockerConsistentWithState pins the invariant clients rely on:
// match_blocker is "none" exactly when match_state is "ready", and every other
// row names a blocker.
func TestQueueMatchBlockerConsistentWithState(t *testing.T) {
	tests := []struct {
		name string
		seed func(*harness)
	}{
		{
			name: "ready",
			seed: func(h *harness) {
				h.seedCandidate(h.t, "Target [ArcA310].mkv", "eligible", `["en"]`)
				h.writeDonor(h.t)
			},
		},
		{
			name: "no arr target",
			seed: func(h *harness) { h.seedUnmatchedCandidate(h.t) },
		},
		{
			name: "target without donor",
			seed: func(h *harness) { h.seedCandidate(h.t, "Shrek (2001).mkv", "eligible", `["en"]`) },
		},
		{
			name: "excluded tag",
			seed: func(h *harness) { h.seedCandidate(h.t, "Shrek (2001) [X].mkv", "waiting_for_tags", `["en"]`) },
		},
		{
			name: "missing required tag",
			seed: func(h *harness) {
				target := "Shrek (2001) [Remux].mkv"
				id := h.seedCandidate(h.t, target, "waiting_for_tags", `["en"]`)
				rule := gate.Rule{Required: []string{"[ArcA310]"}, Mode: gate.MatchAny, Enforce: true}
				h.applyGate(h.t, id, filepath.Join(h.media, target), rule.Classify(target))
			},
		},
		{
			name: "unreadable target folder",
			seed: func(h *harness) {
				h.seedCandidate(h.t, "Target [ArcA310].mkv", "eligible", `["en"]`)
				if err := os.RemoveAll(h.media); err != nil {
					h.t.Fatalf("remove media dir: %v", err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.seed(h)

			item := onlyMatchItem(t, h.listMatchItems(t))
			if strings.TrimSpace(item.MatchBlocker) == "" {
				t.Fatalf("match_blocker is empty, want a machine-readable blocker")
			}
			if gotReady := item.MatchState == MatchStateReady; gotReady != (item.MatchBlocker == MatchBlockerNone) {
				t.Errorf("match_state = %q with match_blocker = %q: %q must hold exactly for %q",
					item.MatchState, item.MatchBlocker, MatchBlockerNone, MatchStateReady)
			}
		})
	}
}

// queueMatchItem is the match-state slice of one GET /api/queue item.
type queueMatchItem struct {
	ID           int64  `json:"id"`
	Filename     string `json:"filename"`
	Matched      bool   `json:"matched"`
	MatchState   string `json:"match_state"`
	MatchReason  string `json:"match_reason"`
	MatchBlocker string `json:"match_blocker"`
	// DonorPatterns is the per-item report of the markers that were searched
	// for. The item field stays (clients branch on it); only the *setting* is
	// gone, so it always carries the fixed list now.
	DonorPatterns []string `json:"donor_patterns"`
	Donors        []struct {
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

// queueDiagnosticsBody is the diagnostics slice of GET /api/queue. LastPassAt
// is a pointer so JSON null and a real timestamp stay distinguishable.
type queueDiagnosticsBody struct {
	Diagnostics struct {
		InstancesConfigured int64   `json:"instances_configured"`
		NeverSynced         int64   `json:"never_synced"`
		SyncFailed          int64   `json:"sync_failed"`
		LastPassAt          *string `json:"last_pass_at"`
		CacheItems          int64   `json:"cache_items"`
		HealthAvailable     *bool   `json:"sync_health_available"`
		ForceTagOverride    *bool   `json:"force_tag_override"`
	} `json:"diagnostics"`
}

// listDiagnostics reads the diagnostics object alongside the (possibly empty)
// item list and asserts the envelope still answers 200.
func (h *harness) listDiagnostics(t *testing.T) queueDiagnosticsBody {
	t.Helper()
	rec := h.request(http.MethodGet, "/api/queue")
	if rec.Code != http.StatusOK {
		h.t.Fatalf("GET /api/queue = %d, body %s", rec.Code, rec.Body.String())
	}
	var body queueDiagnosticsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		h.t.Fatalf("decode: %v", err)
	}
	return body
}

// seedArrInstance inserts an arr_instances row with no recorded attempt, the
// state of a configured instance the sync layer has not polled yet.
func (h *harness) seedArrInstance(t *testing.T, kind string) int64 {
	t.Helper()
	res, err := h.db.ExecContext(context.Background(),
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		 VALUES (?, ?, 'http://arr.invalid', x'00', '0000')`, kind, kind)
	if err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("arr instance id: %v", err)
	}
	return id
}

// TestQueueDiagnosticsNoInstances pins the "no instances configured" shape:
// nothing configured means nothing to count and no pass time.
func TestQueueDiagnosticsNoInstances(t *testing.T) {
	h := newHarness(t)

	diag := h.listDiagnostics(t).Diagnostics
	if diag.InstancesConfigured != 0 {
		t.Errorf("instances_configured = %d, want 0", diag.InstancesConfigured)
	}
	if diag.NeverSynced != 0 || diag.SyncFailed != 0 || diag.CacheItems != 0 {
		t.Errorf("never_synced=%d sync_failed=%d cache_items=%d, want all 0",
			diag.NeverSynced, diag.SyncFailed, diag.CacheItems)
	}
	if diag.LastPassAt != nil {
		t.Errorf("last_pass_at = %q, want null", *diag.LastPassAt)
	}
}

// TestQueueDiagnosticsNeverSynced pins "configured but never attempted": one
// instance with no recorded attempt, so no pass time exists.
func TestQueueDiagnosticsNeverSynced(t *testing.T) {
	h := newHarness(t)
	h.seedArrInstance(t, "radarr")

	diag := h.listDiagnostics(t).Diagnostics
	if diag.InstancesConfigured != 1 {
		t.Errorf("instances_configured = %d, want 1", diag.InstancesConfigured)
	}
	if diag.NeverSynced != 1 {
		t.Errorf("never_synced = %d, want 1", diag.NeverSynced)
	}
	if diag.SyncFailed != 0 {
		t.Errorf("sync_failed = %d, want 0", diag.SyncFailed)
	}
	if diag.LastPassAt != nil {
		t.Errorf("last_pass_at = %q, want null", *diag.LastPassAt)
	}
}

// TestQueueDiagnosticsSyncFailed pins the failed-pass shape. RecordArrInstanceSyncFailure
// stamps last_attempt_at and clears last_sync_ok but deliberately leaves
// last_sync_at (the last SUCCESS) untouched, so with no earlier success the
// attempt exists yet there is still no pass: last_pass_at stays null.
func TestQueueDiagnosticsSyncFailed(t *testing.T) {
	h := newHarness(t)
	id := h.seedArrInstance(t, "radarr")
	st := &store.Store{DB: h.db}
	if err := st.RecordArrInstanceSyncFailure(context.Background(), id, "connection refused"); err != nil {
		t.Fatalf("RecordArrInstanceSyncFailure: %v", err)
	}

	diag := h.listDiagnostics(t).Diagnostics
	if diag.SyncFailed != 1 {
		t.Errorf("sync_failed = %d, want 1", diag.SyncFailed)
	}
	if diag.NeverSynced != 0 {
		t.Errorf("never_synced = %d, want 0 (the attempt happened)", diag.NeverSynced)
	}
	if diag.InstancesConfigured != 1 {
		t.Errorf("instances_configured = %d, want 1", diag.InstancesConfigured)
	}
	if diag.LastPassAt != nil {
		t.Errorf("last_pass_at = %q, want null: a failure is not a pass", *diag.LastPassAt)
	}
}

// TestQueueDiagnosticsCountsCacheItems pins that a synced-but-empty queue still
// reports how much of the library is cached (all kinds, all instances).
func TestQueueDiagnosticsCountsCacheItems(t *testing.T) {
	h := newHarness(t)
	h.seedLibraryRow(t, "Kraina Lodu", 2013)
	h.seedLibraryRow(t, "Shrek", 2001)

	diag := h.listDiagnostics(t).Diagnostics
	if diag.CacheItems != 2 {
		t.Errorf("cache_items = %d, want 2", diag.CacheItems)
	}
	if diag.InstancesConfigured != 2 {
		t.Errorf("instances_configured = %d, want 2", diag.InstancesConfigured)
	}
}

// TestQueueDiagnosticsSurvivesStoreError pins that an unreadable library cache
// cannot take the approval queue down: the list still answers 200 with its
// rows and zeroed diagnostics.
func TestQueueDiagnosticsSurvivesStoreError(t *testing.T) {
	h := newHarness(t)
	h.seedCandidate(t, "Target [ArcA310].mkv", "eligible", `["en"]`)
	// Simulate the health projection being unreadable while the approval
	// queue itself stays intact (arr_instances is not part of the list query,
	// so the queue rows still load).
	if _, err := h.db.Exec(`ALTER TABLE arr_instances RENAME TO arr_instances_unreadable`); err != nil {
		t.Fatalf("rename arr_instances: %v", err)
	}

	rec := h.request(http.MethodGet, "/api/queue")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/queue with unreadable health = %d, want 200, body %s",
			rec.Code, rec.Body.String())
	}
	var body struct {
		Items       []json.RawMessage `json:"items"`
		Diagnostics struct {
			InstancesConfigured int64   `json:"instances_configured"`
			NeverSynced         int64   `json:"never_synced"`
			SyncFailed          int64   `json:"sync_failed"`
			LastPassAt          *string `json:"last_pass_at"`
			CacheItems          int64   `json:"cache_items"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 {
		t.Errorf("items = %d, want the queue rows intact", len(body.Items))
	}
	diag := body.Diagnostics
	if diag.InstancesConfigured != 0 || diag.NeverSynced != 0 || diag.SyncFailed != 0 || diag.CacheItems != 0 {
		t.Errorf("diagnostics = %+v, want zeroed", diag)
	}
	if diag.LastPassAt != nil {
		t.Errorf("last_pass_at = %q, want null", *diag.LastPassAt)
	}
}

// TestQueueDiagnosticsMarksUnavailableOnStoreError pins the explicit signal an
// unreadable health read owes the UI: the counts are zeroed, so without a flag
// an empty queue cannot be told apart from "no arr instance is configured". The
// healthy path must claim the opposite, or the flag would be useless.
func TestQueueDiagnosticsMarksUnavailableOnStoreError(t *testing.T) {
	h := newHarness(t)
	h.seedArrInstance(t, "radarr")

	healthy := h.listDiagnostics(t).Diagnostics
	if healthy.HealthAvailable == nil || !*healthy.HealthAvailable {
		t.Fatalf("sync_health_available = %v, want true on a readable health projection", healthy.HealthAvailable)
	}

	// Break the health projection only: the queue rows still load.
	if _, err := h.db.Exec(`ALTER TABLE arr_instances RENAME TO arr_instances_unreadable`); err != nil {
		t.Fatalf("rename arr_instances: %v", err)
	}
	unavailable := h.listDiagnostics(t).Diagnostics
	if unavailable.HealthAvailable == nil {
		t.Fatal("sync_health_available missing from the unavailable payload")
	}
	if *unavailable.HealthAvailable {
		t.Errorf("sync_health_available = true, want false when the health read failed")
	}
	if unavailable.InstancesConfigured != 0 || unavailable.NeverSynced != 0 || unavailable.SyncFailed != 0 {
		t.Errorf("diagnostics = %+v, want zeroed counts alongside the flag", unavailable)
	}
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

// TestApproveIneligibleTargetRecordsIntent pins the ruling this slice encodes:
// approval records intent and never consults the Processing Gate. A target that
// is not tag-ready approves fine — the job is born after extraction and starts
// held, so an operator can say "dub this title" days before the upstream
// pipeline tags the file. Approve itself creates no sync job (its target track
// does not exist yet) and records no force override nobody asked for.
func TestApproveIneligibleTargetRecordsIntent(t *testing.T) {
	h := newHarness(t)
	target := "Shrek (2001) [ArcA310] [X].mkv"
	id := h.seedCandidate(t, target, "waiting_for_tags", `["en"]`)
	// A real gate verdict: required tag present, excluded tag carried, so the
	// row proves an exclusion rather than a missing tag.
	rule := gate.Rule{Required: []string{"[ArcA310]"}, Excluded: []string{"[X]"}, Mode: gate.MatchAny, Enforce: true}
	h.applyGate(t, id, filepath.Join(h.media, target), rule.Classify(target))
	h.writeDonor(t)

	rec := h.request(http.MethodPost, "/api/queue/"+itoa(id)+"/approve")
	if rec.Code != http.StatusOK {
		t.Fatalf("approve ineligible target = %d, want 200, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Approved bool `json:"approved"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode approve body: %v", err)
	}
	if !body.Approved {
		t.Errorf("body = %s, want approved true", rec.Body.String())
	}

	var status string
	if err := h.db.QueryRow(`SELECT status FROM import_queue WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("status: %v", err)
	}
	if status != "extracting" {
		t.Errorf("status = %q, want extracting", status)
	}
	if n := countSyncJobs(t, h, id); n != 0 {
		t.Errorf("sync jobs = %d, want 0: approve records intent, extraction breeds the job", n)
	}
	job, ok, err := (&store.Store{DB: h.db}).NextImportExtraction(context.Background())
	if err != nil || !ok {
		t.Fatalf("NextImportExtraction = (ok=%v, err=%v)", ok, err)
	}
	if job.ForceIgnoreTagFilter {
		t.Error("force_ignore_tag_filter recorded although the request never asked for it")
	}
	if job.DonorPath != filepath.Join(h.media, donorName) {
		t.Errorf("donor = %q, want the paired donor", job.DonorPath)
	}
}

// TestApproveRecordsForceOnlyWhenTheSettingAllows pins the escape hatch: a
// request may ask for "approve without waiting for tags", but the intent is
// recorded only when security.options.allow_force_tag_override is on. The same
// body proves both halves, so the setting is the only difference between them.
func TestApproveRecordsForceOnlyWhenTheSettingAllows(t *testing.T) {
	tests := []struct {
		name      string
		enable    bool
		wantForce bool
	}{
		{name: "setting off ignores the request", enable: false, wantForce: false},
		{name: "setting on records the request", enable: true, wantForce: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			id := h.seedCandidate(t, "Shrek (2001) [ArcA310] [X].mkv", "waiting_for_tags", `["en"]`)
			h.writeDonor(t)
			if tc.enable {
				h.enableForceTagOverride(t)
			}

			rec := h.postJSON(t, "/api/queue/"+itoa(id)+"/approve", `{"force_ignore_tag_filter":true}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("approve with force = %d, want 200, body %s", rec.Code, rec.Body.String())
			}
			job, ok, err := (&store.Store{DB: h.db}).NextImportExtraction(context.Background())
			if err != nil || !ok {
				t.Fatalf("NextImportExtraction = (ok=%v, err=%v)", ok, err)
			}
			if job.ForceIgnoreTagFilter != tc.wantForce {
				t.Errorf("force_ignore_tag_filter = %v, want %v", job.ForceIgnoreTagFilter, tc.wantForce)
			}
		})
	}
}

// TestApproveRefusesWithoutTargetFile pins the target-file half of the four
// unformable cases: a matched identity whose target path is gone has nothing to
// pair a donor with, so approve answers 409 and leaves the row pending.
func TestApproveRefusesWithoutTargetFile(t *testing.T) {
	h := newHarness(t)
	id := h.seedCandidate(t, "Target [ArcA310].mkv", "eligible", `["en"]`)
	h.writeDonor(t)
	// The match survives, its target file does not: the target is read from the
	// cache row and the stored gate verdict, so both have to go.
	if _, err := h.db.Exec(`UPDATE arr_library_cache SET target_file_path = NULL
		WHERE id = (SELECT matched_library_cache_id FROM import_queue WHERE id = ?)`, id); err != nil {
		t.Fatalf("clear cache target: %v", err)
	}
	h.clearGateVerdict(t, id)

	rec := h.request(http.MethodPost, "/api/queue/"+itoa(id)+"/approve")
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve without target file = %d, want 409, body %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "no target file") {
		t.Errorf("body = %s, want it to name the missing target file", body)
	}
	var status string
	if err := h.db.QueryRow(`SELECT status FROM import_queue WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("status: %v", err)
	}
	if status != "pending_approval" {
		t.Errorf("status after refused approve = %q, want pending_approval", status)
	}
}

// TestApproveWithLiveJobIsIdempotent pins approval's idempotence in time: once
// the row's extraction has bred a live job, a second approve answers success
// instead of breeding another job or resetting the first one's progress. The
// row is already extracting here, so without the live-job check this would be a
// 409.
func TestApproveWithLiveJobIsIdempotent(t *testing.T) {
	h := newHarness(t)
	id := h.seedCandidate(t, "Target [ArcA310].mkv", "eligible", `["en"]`)
	h.writeDonor(t)

	if rec := h.request(http.MethodPost, "/api/queue/"+itoa(id)+"/approve"); rec.Code != http.StatusOK {
		t.Fatalf("first approve = %d, body %s", rec.Code, rec.Body.String())
	}
	jobID := h.seedSyncJob(t, id, store.SyncJobHeld, store.HeldReasonWaitingForTags, false)
	var intentBefore string
	if err := h.db.QueryRow(`SELECT parsed_raw_json FROM import_queue WHERE id = ?`, id).Scan(&intentBefore); err != nil {
		t.Fatalf("read intent: %v", err)
	}

	rec := h.request(http.MethodPost, "/api/queue/"+itoa(id)+"/approve")
	if rec.Code != http.StatusOK {
		t.Fatalf("second approve = %d, want 200, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Approved bool `json:"approved"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode approve body: %v", err)
	}
	if !body.Approved {
		t.Errorf("body = %s, want approved true", rec.Body.String())
	}

	if n := countSyncJobs(t, h, id); n != 1 {
		t.Errorf("sync jobs = %d, want 1: a retry must not breed a second job", n)
	}
	if got := syncJobStatusOf(t, h, jobID); got != store.SyncJobHeld {
		t.Errorf("job status = %q, want %q: a retry must not touch progress", got, store.SyncJobHeld)
	}
	var intentAfter, rowStatus string
	if err := h.db.QueryRow(`SELECT parsed_raw_json, status FROM import_queue WHERE id = ?`, id).
		Scan(&intentAfter, &rowStatus); err != nil {
		t.Fatalf("read row after retry: %v", err)
	}
	if intentAfter != intentBefore {
		t.Errorf("parsed_raw_json changed on retry: %q -> %q", intentBefore, intentAfter)
	}
	if rowStatus != "extracting" {
		t.Errorf("status = %q, want extracting (unchanged by the retry)", rowStatus)
	}
}

// seedSyncJob inserts the vault track a sync job requires and creates one job
// owned by the queue row — the ledger shape extract.BirthSyncJob writes after
// the extraction archived the track, so approve and reject run against a real
// row instead of a stub.
func (h *harness) seedSyncJob(t *testing.T, queueID int64, status, heldReason string, force bool) int64 {
	t.Helper()
	ctx := context.Background()
	var cacheID int64
	if err := h.db.QueryRowContext(ctx,
		`SELECT COALESCE(matched_library_cache_id, 0) FROM import_queue WHERE id = ?`,
		queueID).Scan(&cacheID); err != nil {
		t.Fatalf("queue row cache id: %v", err)
	}
	if cacheID == 0 {
		t.Fatalf("queue row %d has no matched library target to key a job on", queueID)
	}

	h.seq++
	res, err := h.db.ExecContext(ctx,
		`INSERT INTO vault_media (media_type, title, vault_folder_path)
		 VALUES ('movie', 'Minions and Monsters', ?)`, h.media)
	if err != nil {
		t.Fatalf("insert vault media: %v", err)
	}
	mediaID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("vault media id: %v", err)
	}
	res, err = h.db.ExecContext(ctx,
		`INSERT INTO audio_tracks
		 (vault_media_id, group_uuid, lang_iso2, variant, file_path, sha256,
		  codec, channels, channel_count)
		 VALUES (?, ?, 'pl', 'dubbing', ?, 'sha', 'pcm_s16le', '2.0', 2)`,
		mediaID, "group-"+itoa(int64(h.seq)),
		filepath.Join(h.media, "track-"+itoa(int64(h.seq))+".pl.mka"))
	if err != nil {
		t.Fatalf("insert audio track: %v", err)
	}
	trackID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("audio track id: %v", err)
	}

	// Only queued|held may be born, so a terminal status is reached the way the
	// worker reaches it: by transitioning an existing job.
	born, bornReason := status, heldReason
	if born != store.SyncJobQueued && born != store.SyncJobHeld {
		born, bornReason = store.SyncJobQueued, ""
	}
	origin := queueID
	rec, err := (&store.Store{DB: h.db}).CreateSyncJob(ctx, store.SyncJobCreate{
		VaultTrackID:         trackID,
		TargetLibraryCacheID: cacheID,
		TargetVideoPath:      filepath.Join(h.media, "Target [ArcA310].mkv"),
		TargetLangISO2:       "pl",
		Status:               born,
		HeldReason:           bornReason,
		ForceIgnoreTagFilter: force,
		OriginQueueID:        &origin,
	})
	if err != nil {
		t.Fatalf("create sync job (%s): %v", born, err)
	}
	if born != status {
		if _, err := h.db.ExecContext(ctx,
			`UPDATE sync_mux_jobs SET status = ? WHERE id = ?`, status, rec.ID); err != nil {
			t.Fatalf("move sync job %d to %q: %v", rec.ID, status, err)
		}
	}
	return rec.ID
}

// countSyncJobs counts the jobs an approval bred, by origin.
func countSyncJobs(t *testing.T, h *harness, queueID int64) int {
	t.Helper()
	var n int
	if err := h.db.QueryRow(
		`SELECT count(*) FROM sync_mux_jobs WHERE origin_queue_id = ?`, queueID).Scan(&n); err != nil {
		t.Fatalf("count sync jobs: %v", err)
	}
	return n
}

// syncJobStatusOf reads one job's status, so a test can prove the handler left
// it alone.
func syncJobStatusOf(t *testing.T, h *harness, jobID int64) string {
	t.Helper()
	var status string
	if err := h.db.QueryRow(`SELECT status FROM sync_mux_jobs WHERE id = ?`, jobID).Scan(&status); err != nil {
		t.Fatalf("sync job %d status: %v", jobID, err)
	}
	return status
}

// TestRejectCancelsHeldJobByOrigin pins rejection's other half: withdrawing the
// intent cancels the job it bred while that job is still queued|held, so a
// released-later mux cannot happen behind a rejected row.
func TestRejectCancelsHeldJobByOrigin(t *testing.T) {
	h := newHarness(t)
	id := h.seedCandidate(t, "Target [ArcA310].mkv", "waiting_for_tags", `["en"]`)
	jobID := h.seedSyncJob(t, id, store.SyncJobHeld, store.HeldReasonWaitingForTags, false)

	if rec := h.request(http.MethodPost, "/api/queue/"+itoa(id)+"/reject"); rec.Code != http.StatusOK {
		t.Fatalf("reject = %d, body %s", rec.Code, rec.Body.String())
	}
	var rowStatus string
	if err := h.db.QueryRow(`SELECT status FROM import_queue WHERE id = ?`, id).Scan(&rowStatus); err != nil {
		t.Fatalf("row status: %v", err)
	}
	if rowStatus != "ignored" {
		t.Errorf("row status = %q, want ignored", rowStatus)
	}
	if got := syncJobStatusOf(t, h, jobID); got != store.SyncJobCancelled {
		t.Errorf("job status = %q, want %q", got, store.SyncJobCancelled)
	}
	var reason string
	if err := h.db.QueryRow(`SELECT COALESCE(held_reason, '') FROM sync_mux_jobs WHERE id = ?`, jobID).Scan(&reason); err != nil {
		t.Fatalf("job reason: %v", err)
	}
	if reason != store.CancelReasonRejection {
		t.Errorf("held_reason = %q, want %q", reason, store.CancelReasonRejection)
	}
}

// TestRejectLeavesCompletedJobAlone pins the guard on the cancel: rejection
// withdraws an intent, it never rewrites history, so a job that already
// completed keeps its status while the still-live one next to it is cancelled.
func TestRejectLeavesCompletedJobAlone(t *testing.T) {
	h := newHarness(t)
	id := h.seedCandidate(t, "Target [ArcA310].mkv", "waiting_for_tags", `["en"]`)
	completed := h.seedSyncJob(t, id, store.SyncJobCompleted, "", false)
	held := h.seedSyncJob(t, id, store.SyncJobHeld, store.HeldReasonWaitingForTags, false)

	if rec := h.request(http.MethodPost, "/api/queue/"+itoa(id)+"/reject"); rec.Code != http.StatusOK {
		t.Fatalf("reject = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := syncJobStatusOf(t, h, completed); got != store.SyncJobCompleted {
		t.Errorf("completed job status = %q, want %q (terminal rows are untouched)", got, store.SyncJobCompleted)
	}
	if got := syncJobStatusOf(t, h, held); got != store.SyncJobCancelled {
		t.Errorf("held job status = %q, want %q", got, store.SyncJobCancelled)
	}
}

// TestQueueMatchStateReadyWhateverTheForceOverride pins the card's remaining use
// of the escape hatch: security.options.allow_force_tag_override is reported in
// the diagnostics (the card offers the "start it now" control on it), but it no
// longer decides the match state — the row is ready either way, because an
// ineligible pair is approvable and the override only chooses whether the job
// born after the extraction starts immediately or waits.
func TestQueueMatchStateReadyWhateverTheForceOverride(t *testing.T) {
	h := newHarness(t)
	target := "Shrek (2001) [ArcA310] [X].mkv"
	id := h.seedCandidate(t, target, "waiting_for_tags", `["en"]`)
	rule := gate.Rule{Required: []string{"[ArcA310]"}, Excluded: []string{"[X]"}, Mode: gate.MatchAny, Enforce: true}
	h.applyGate(t, id, filepath.Join(h.media, target), rule.Classify(target))
	h.writeDonor(t)

	// Override OFF: the row is approvable, and the override is simply not
	// offered.
	off := onlyMatchItem(t, h.listMatchItems(t))
	if off.MatchState != MatchStateReady {
		t.Errorf("override off: match_state = %q, want %q", off.MatchState, MatchStateReady)
	}
	if off.MatchBlocker != MatchBlockerNone {
		t.Errorf("override off: match_blocker = %q, want %q", off.MatchBlocker, MatchBlockerNone)
	}
	if diag := h.listDiagnostics(t).Diagnostics; diag.ForceTagOverride == nil || *diag.ForceTagOverride {
		t.Errorf("override off: force_tag_override = %v, want false", diag.ForceTagOverride)
	}

	h.enableForceTagOverride(t)

	// Override ON: same state, and now the diagnostics offer the control.
	on := onlyMatchItem(t, h.listMatchItems(t))
	if on.MatchState != MatchStateReady {
		t.Errorf("override on: match_state = %q, want %q", on.MatchState, MatchStateReady)
	}
	if on.MatchBlocker != MatchBlockerNone {
		t.Errorf("override on: match_blocker = %q, want %q", on.MatchBlocker, MatchBlockerNone)
	}
	if diag := h.listDiagnostics(t).Diagnostics; diag.ForceTagOverride == nil || !*diag.ForceTagOverride {
		t.Errorf("override on: force_tag_override = %v, want true", diag.ForceTagOverride)
	}
}
