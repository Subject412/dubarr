package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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
