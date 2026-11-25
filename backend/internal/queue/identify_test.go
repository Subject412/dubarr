package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arr/arrmock"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// fakeDecrypter maps an opaque blob back to the plaintext key, so the identify
// route exercises its real decryption path without a keyring.
type fakeDecrypter map[string]string

func (f fakeDecrypter) DecryptString(blob []byte) (string, error) {
	key, ok := f[string(blob)]
	if !ok {
		return "", errors.New("fakeDecrypter: unknown blob")
	}
	return key, nil
}

// identifyHarness is a queue API wired to one in-process arr fake.
type identifyHarness struct {
	t      *testing.T
	db     *sql.DB
	mux    *http.ServeMux
	media  string
	srv    *arrmock.Server
	instID int64
}

func newIdentifyHarness(t *testing.T, fx arrmock.Fixtures, timeout time.Duration) *identifyHarness {
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

	srv := arrmock.New(arrmock.Radarr, "", fx)
	t.Cleanup(srv.Close)

	blob := "blob-1"
	res, err := st.DB.ExecContext(context.Background(),
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		 VALUES ('main', 'radarr', ?, ?, '0000')`, srv.URL, []byte(blob))
	if err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	instID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("instance id: %v", err)
	}

	mux := http.NewServeMux()
	RegisterRoutes(mux, Dependencies{
		DB:              st.DB,
		Settings:        settingsStore,
		Secrets:         fakeDecrypter{blob: srv.APIKey()},
		IdentifyTimeout: timeout,
		RequireAdmin: func(h http.Handler) http.Handler {
			return h
		},
	})
	return &identifyHarness{t: t, db: st.DB, mux: mux, media: t.TempDir(), srv: srv, instID: instID}
}

// seedCandidateWithTarget records a pending queue row linked to the harness
// instance, and returns its id plus the release filename.
func (h *identifyHarness) seedCandidateWithTarget(t *testing.T, filename string) int64 {
	t.Helper()
	source := filepath.Join(t.TempDir(), filename)
	st := &store.Store{DB: h.db}
	created, err := st.RecordImportCandidate(context.Background(), source, filename, 42)
	if err != nil || !created {
		t.Fatalf("RecordImportCandidate: created=%v err=%v", created, err)
	}
	var id int64
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT id FROM import_queue WHERE source_file_path = ?`, source).Scan(&id); err != nil {
		t.Fatalf("candidate id: %v", err)
	}
	if _, err := h.db.ExecContext(context.Background(),
		`UPDATE import_queue SET matched_arr_instance_id = ? WHERE id = ?`, h.instID, id); err != nil {
		t.Fatalf("link candidate instance: %v", err)
	}
	return id
}

// seedCacheRow inserts one arr_library_cache row of the harness instance and
// returns its id. The target file is created so the donor pairing reads a real
// (empty) folder.
func (h *identifyHarness) seedCacheRow(t *testing.T, mediaType, title string, year int, externalID int64) int64 {
	t.Helper()
	targetPath := filepath.Join(h.media, title+".mkv")
	if err := os.WriteFile(targetPath, []byte("target"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	res, err := h.db.ExecContext(context.Background(),
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, title, original_language_iso2, year,
		  target_file_path, audio_languages_json, eligibility_status)
		 VALUES (?, ?, ?, ?, 'en', ?, ?, '["en"]', 'eligible')`,
		h.instID, mediaType, externalID, title, year, targetPath)
	if err != nil {
		t.Fatalf("insert library cache: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("cache id: %v", err)
	}
	return id
}

func (h *identifyHarness) post(path, body string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

type identifyBody struct {
	Parse struct {
		Parsed     bool    `json:"parsed"`
		Title      string  `json:"title"`
		Year       int     `json:"year"`
		Quality    string  `json:"quality"`
		Confidence float64 `json:"confidence"`
	} `json:"parse"`
	Candidates []struct {
		LibraryCacheID int64   `json:"library_cache_id"`
		Title          string  `json:"title"`
		Year           int     `json:"year"`
		MediaType      string  `json:"media_type"`
		ExternalItemID int64   `json:"external_item_id"`
		Confidence     float64 `json:"confidence"`
		MatchedBy      string  `json:"matched_by"`
	} `json:"candidates"`
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
	Class string `json:"class"`
}

func decodeIdentify(t *testing.T, rec *httptest.ResponseRecorder) identifyBody {
	t.Helper()
	var out identifyBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode identify body %q: %v", rec.Body.String(), err)
	}
	return out
}

func decodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var out errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return out
}

// TestIdentifyParsesViaArrMock pins the happy path: the release is parsed by the
// instance behind the row, the verdict is returned, and the candidates are
// ranked deterministically with the release name posted to arr.
func TestIdentifyParsesViaArrMock(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{
		Parse: arrmock.ParseResponse{Title: "Kraina Lodu", Year: 2013, Quality: "Bluray-1080p"},
	}, 2*time.Second)
	exact := h.seedCacheRow(t, "movie", "Kraina Lodu", 2013, 1)
	sequel := h.seedCacheRow(t, "movie", "Kraina Lodu 2", 2019, 2)
	unrelated := h.seedCacheRow(t, "movie", "Kraina", 1999, 3)
	id := h.seedCandidateWithTarget(t, "Kraina.Lodu.2013.PL.mkv")

	rec := h.post("/api/queue/"+itoa(id)+"/identify", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("identify = %d, body %s", rec.Code, rec.Body.String())
	}
	body := decodeIdentify(t, rec)
	if body.Parse.Title != "Kraina Lodu" || body.Parse.Year != 2013 || body.Parse.Quality != "Bluray-1080p" {
		t.Errorf("parse = %+v", body.Parse)
	}
	if body.Parse.Confidence != 1 {
		t.Errorf("parse confidence = %v, want 1", body.Parse.Confidence)
	}
	if len(body.Candidates) != 3 {
		t.Fatalf("candidates = %+v, want 3", body.Candidates)
	}
	// Best first: exact title + year, then the shorter title contained in it,
	// then the longer title that contains the parsed one.
	if body.Candidates[0].LibraryCacheID != exact || body.Candidates[0].Confidence != 1 {
		t.Errorf("best candidate = %+v, want id %d at confidence 1", body.Candidates[0], exact)
	}
	if body.Candidates[1].LibraryCacheID != unrelated || body.Candidates[1].Confidence != 0.6 {
		t.Errorf("second candidate = %+v, want id %d at confidence 0.6", body.Candidates[1], unrelated)
	}
	if body.Candidates[2].LibraryCacheID != sequel || body.Candidates[2].Confidence != 0.5 {
		t.Errorf("third candidate = %+v, want the sequel id %d at confidence 0.5", body.Candidates[2], sequel)
	}
	for i, cand := range body.Candidates {
		if cand.MatchedBy != "parse" {
			t.Errorf("candidate[%d].matched_by = %q, want parse", i, cand.MatchedBy)
		}
		if cand.MediaType != "movie" || cand.ExternalItemID == 0 {
			t.Errorf("candidate[%d] metadata = %+v", i, cand)
		}
	}

	// The row's filename stem is what arr parsed: the extension is dropped.
	posted := h.srv.ParseRequests()
	if len(posted) != 1 || posted[0] != "Kraina.Lodu.2013.PL" {
		t.Errorf("posted titles = %v, want the filename stem", posted)
	}
}

// TestIdentifyColdCache pins that an unsynced library is not an error: the parse
// verdict is returned with an empty, non-null candidate list.
func TestIdentifyColdCache(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{
		Parse: arrmock.ParseResponse{Title: "Kraina Lodu", Year: 2013, Quality: "Bluray-1080p"},
	}, 2*time.Second)
	id := h.seedCandidateWithTarget(t, "Kraina.Lodu.2013.PL.mkv")

	rec := h.post("/api/queue/"+itoa(id)+"/identify", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("identify = %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"candidates":[]`) {
		t.Errorf("body %s does not carry an empty candidate array", rec.Body.String())
	}
	body := decodeIdentify(t, rec)
	if body.Parse.Title != "Kraina Lodu" || len(body.Candidates) != 0 {
		t.Errorf("cold cache body = %+v", body)
	}

	// An operator override replaces the filename stem as the parsed title.
	rec = h.post("/api/queue/"+itoa(id)+"/identify", `{"title":"Frozen.2013.1080p"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("identify with override = %d, body %s", rec.Code, rec.Body.String())
	}
	posted := h.srv.ParseRequests()
	if len(posted) != 2 || posted[1] != "Frozen.2013.1080p" {
		t.Errorf("posted titles = %v, want the override last", posted)
	}
}

// TestIdentifyUnparsableReleaseIsDistinct pins that an instance which answers
// 200 with no recognised title is reported as *unparsable*, not as a cold cache:
// parse.parsed is false and no candidate scan happens at all.
//
// The scan is made impossible rather than merely invisible: the cache's title
// column is renamed away first, so any IdentifyCandidatePool query would fail
// and the route would answer 500. A 200 with an empty candidate array therefore
// proves the short-circuit ran before the library read.
func TestIdentifyUnparsableReleaseIsDistinct(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{
		// An all-zero fixture is an instance that could not parse anything: it
		// echoes the posted release name as the top-level title and stops there.
		Parse: arrmock.ParseResponse{},
	}, 2*time.Second)
	h.seedCacheRow(t, "movie", "Kraina Lodu", 2013, 1)
	id := h.seedCandidateWithTarget(t, "Kraina.Lodu.2013.PL.mkv")

	if _, err := h.db.ExecContext(context.Background(),
		`ALTER TABLE arr_library_cache RENAME COLUMN title TO title_offline`); err != nil {
		t.Fatalf("rename cache title column: %v", err)
	}

	rec := h.post("/api/queue/"+itoa(id)+"/identify", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("identify = %d, body %s (a candidate scan must not run)", rec.Code, rec.Body.String())
	}
	body := decodeIdentify(t, rec)
	if body.Parse.Parsed {
		t.Errorf("parse.parsed = true for an unparsable release: %+v", body.Parse)
	}
	if body.Parse.Title != "" {
		t.Errorf("parse.title = %q, want empty (no invented title)", body.Parse.Title)
	}
	if !strings.Contains(rec.Body.String(), `"candidates":[]`) {
		t.Errorf("body %s does not carry an empty candidate array", rec.Body.String())
	}
	if len(body.Candidates) != 0 {
		t.Errorf("candidates = %+v, want none", body.Candidates)
	}
}

// TestIdentifyDoesNotAutoPick pins the server's contract that identify only ever
// suggests: a release with candidates available leaves the row unmatched and in
// its current status until the operator posts /match.
func TestIdentifyDoesNotAutoPick(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{
		Parse: arrmock.ParseResponse{Title: "Kraina Lodu", Year: 2013, Quality: "Bluray-1080p"},
	}, 2*time.Second)
	cacheID := h.seedCacheRow(t, "movie", "Kraina Lodu", 2013, 1)
	id := h.seedCandidateWithTarget(t, "Kraina.Lodu.2013.PL.mkv")

	before := readIdentifyRowState(t, h.db, id)

	rec := h.post("/api/queue/"+itoa(id)+"/identify", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("identify = %d, body %s", rec.Code, rec.Body.String())
	}
	body := decodeIdentify(t, rec)
	if len(body.Candidates) == 0 || body.Candidates[0].LibraryCacheID != cacheID {
		t.Fatalf("candidates = %+v, want the seeded cache row id %d", body.Candidates, cacheID)
	}

	after := readIdentifyRowState(t, h.db, id)
	if after.matchedID.Valid {
		t.Errorf("identify wrote matched_library_cache_id = %+v; the server must only suggest", after.matchedID)
	}
	if after.status != before.status {
		t.Errorf("identify changed status %q -> %q", before.status, after.status)
	}
	if after.status != "pending_approval" {
		t.Errorf("status = %q, want pending_approval", after.status)
	}
}

// identifyRowState is the part of a queue row identify must never mutate.
type identifyRowState struct {
	matchedID sql.NullInt64
	status    string
}

func readIdentifyRowState(t *testing.T, db *sql.DB, id int64) identifyRowState {
	t.Helper()
	var state identifyRowState
	if err := db.QueryRowContext(context.Background(),
		`SELECT matched_library_cache_id, status FROM import_queue WHERE id = ?`, id).
		Scan(&state.matchedID, &state.status); err != nil {
		t.Fatalf("read queue row %d: %v", id, err)
	}
	return state
}

// TestIdentifyPersistsPick pins the match route: the pick is persisted with its
// confidence label, and the returned match_state is the one the list derives.
func TestIdentifyPersistsPick(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{
		Parse: arrmock.ParseResponse{Title: "Kraina Lodu", Year: 2013, Quality: "Bluray-1080p"},
	}, 2*time.Second)
	cacheID := h.seedCacheRow(t, "movie", "Kraina Lodu", 2013, 1)
	id := h.seedCandidateWithTarget(t, "Kraina.Lodu.2013.PL.mkv")

	identified := h.post("/api/queue/"+itoa(id)+"/identify", "")
	if identified.Code != http.StatusOK {
		t.Fatalf("identify = %d, body %s", identified.Code, identified.Body.String())
	}
	body := decodeIdentify(t, identified)
	if len(body.Candidates) != 1 || body.Candidates[0].LibraryCacheID != cacheID {
		t.Fatalf("candidates = %+v, want the seeded cache row", body.Candidates)
	}

	rec := h.post("/api/queue/"+itoa(id)+"/match",
		`{"library_cache_id":`+itoa(cacheID)+`,"confidence":`+formatFloat(body.Candidates[0].Confidence)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("match = %d, body %s", rec.Code, rec.Body.String())
	}
	var matched struct {
		MatchState string `json:"match_state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &matched); err != nil {
		t.Fatalf("decode match body: %v", err)
	}
	// A matched target with no sibling donor is exactly matched_no_donor.
	if matched.MatchState != MatchStateMatchedNoDonor {
		t.Errorf("match_state = %q, want %q", matched.MatchState, MatchStateMatchedNoDonor)
	}

	var (
		storedID   sql.NullInt64
		confidence sql.NullString
		status     string
	)
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT matched_library_cache_id, parse_confidence, status FROM import_queue WHERE id = ?`,
		id).Scan(&storedID, &confidence, &status); err != nil {
		t.Fatalf("read queue row: %v", err)
	}
	if !storedID.Valid || storedID.Int64 != cacheID {
		t.Errorf("matched_library_cache_id = %+v, want %d", storedID, cacheID)
	}
	if !confidence.Valid || confidence.String != "ambiguous" {
		t.Errorf("parse_confidence = %+v, want ambiguous (a human pick is not an exact parse)", confidence)
	}
	if status != "pending_approval" {
		t.Errorf("status = %q, want pending_approval (matching must not approve)", status)
	}

	// The list now reports the same state the match response did.
	list := httptest.NewRecorder()
	h.mux.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/queue", nil))
	var listed struct {
		Items []struct {
			ID         int64  `json:"id"`
			MatchState string `json:"match_state"`
			Matched    bool   `json:"matched"`
		} `json:"items"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode queue list: %v", err)
	}
	if len(listed.Items) != 1 || !listed.Items[0].Matched || listed.Items[0].MatchState != MatchStateMatchedNoDonor {
		t.Errorf("queue list = %+v", listed.Items)
	}
}

// TestIdentifyConflictingPick pins that a pick contradicting the row's existing
// match is refused instead of silently overwritten.
func TestIdentifyConflictingPick(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{
		Parse: arrmock.ParseResponse{Title: "Kraina Lodu", Year: 2013, Quality: "Bluray-1080p"},
	}, 2*time.Second)
	first := h.seedCacheRow(t, "movie", "Kraina Lodu", 2013, 1)
	second := h.seedCacheRow(t, "movie", "Kraina Lodu 2", 2019, 2)
	id := h.seedCandidateWithTarget(t, "Kraina.Lodu.2013.PL.mkv")

	if rec := h.post("/api/queue/"+itoa(id)+"/match", `{"library_cache_id":`+itoa(first)+`}`); rec.Code != http.StatusOK {
		t.Fatalf("first match = %d, body %s", rec.Code, rec.Body.String())
	}
	rec := h.post("/api/queue/"+itoa(id)+"/match", `{"library_cache_id":`+itoa(second)+`}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflicting match = %d, body %s", rec.Code, rec.Body.String())
	}
	if msg := decodeErrorBody(t, rec); msg.Code != "match_conflict" {
		t.Errorf("conflict code = %q, want match_conflict", msg.Code)
	}
	var stored sql.NullInt64
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT matched_library_cache_id FROM import_queue WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatalf("read queue row: %v", err)
	}
	if !stored.Valid || stored.Int64 != first {
		t.Errorf("match after conflict = %+v, want the first pick %d", stored, first)
	}

	// Re-picking the same id is idempotent.
	if rec := h.post("/api/queue/"+itoa(id)+"/match", `{"library_cache_id":`+itoa(first)+`}`); rec.Code != http.StatusOK {
		t.Errorf("re-pick = %d, body %s", rec.Code, rec.Body.String())
	}
}

// TestIdentifyRequiresAdmin pins the shared admin contract: with no
// RequireAdmin wired every route, identify included, answers 503.
func TestIdentifyRequiresAdmin(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	settingsStore := settings.New(st.DB)
	if err := settingsStore.Seed(context.Background(), dir); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	mux := http.NewServeMux()
	RegisterRoutes(mux, Dependencies{DB: st.DB, Settings: settingsStore})

	for _, path := range []string{"/api/queue/1/identify", "/api/queue/1/match"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"library_cache_id":1}`)))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s = %d, want 503", path, rec.Code)
		}
	}
}

// TestIdentifyTimeoutClassified pins the operator-facing timeout class: the
// route's own deadline bounds the parse, and a 502 carries class "timeout".
func TestIdentifyTimeoutClassified(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{
		Parse: arrmock.ParseResponse{Title: "Kraina Lodu", Year: 2013, Quality: "Bluray-1080p"},
	}, 50*time.Millisecond)
	h.srv.SetDelay(400 * time.Millisecond)
	id := h.seedCandidateWithTarget(t, "Kraina.Lodu.2013.PL.mkv")

	rec := h.post("/api/queue/"+itoa(id)+"/identify", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("identify = %d, body %s", rec.Code, rec.Body.String())
	}
	body := decodeErrorBody(t, rec)
	if body.Code != "identify_failed" {
		t.Errorf("code = %q, want identify_failed", body.Code)
	}
	if body.Class != "timeout" {
		t.Errorf("class = %q, want timeout", body.Class)
	}
}

// TestIdentifyNotConfigured pins the 409 the UI turns into a Settings pointer:
// without a secret store the route never parses unauthenticated.
func TestIdentifyNotConfigured(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	settingsStore := settings.New(st.DB)
	if err := settingsStore.Seed(context.Background(), dir); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	source := filepath.Join(t.TempDir(), "Shrek.2001.mkv")
	created, err := (&store.Store{DB: st.DB}).RecordImportCandidate(context.Background(), source, "Shrek.2001.mkv", 1)
	if err != nil || !created {
		t.Fatalf("RecordImportCandidate: created=%v err=%v", created, err)
	}
	var id int64
	if err := st.DB.QueryRowContext(context.Background(),
		`SELECT id FROM import_queue WHERE source_file_path = ?`, source).Scan(&id); err != nil {
		t.Fatalf("candidate id: %v", err)
	}

	mux := http.NewServeMux()
	RegisterRoutes(mux, Dependencies{
		DB:           st.DB,
		Settings:     settingsStore,
		RequireAdmin: func(h http.Handler) http.Handler { return h },
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/queue/"+itoa(id)+"/identify", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("identify = %d, body %s", rec.Code, rec.Body.String())
	}
	if body := decodeErrorBody(t, rec); body.Code != "not_configured" {
		t.Errorf("code = %q, want not_configured", body.Code)
	}
}

// TestMatchRejectsNonPendingRow pins the 422: a well-formed pick for a row that
// already left pending_approval cannot apply.
func TestMatchRejectsNonPendingRow(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{}, 2*time.Second)
	cacheID := h.seedCacheRow(t, "movie", "Kraina Lodu", 2013, 1)
	id := h.seedCandidateWithTarget(t, "Kraina.Lodu.2013.PL.mkv")
	if _, err := h.db.ExecContext(context.Background(),
		`UPDATE import_queue SET status = 'ignored' WHERE id = ?`, id); err != nil {
		t.Fatalf("set status: %v", err)
	}

	rec := h.post("/api/queue/"+itoa(id)+"/match", `{"library_cache_id":`+itoa(cacheID)+`}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("match = %d, body %s", rec.Code, rec.Body.String())
	}
	if body := decodeErrorBody(t, rec); body.Code != "not_pending" {
		t.Errorf("code = %q, want not_pending", body.Code)
	}
	var stored sql.NullInt64
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT matched_library_cache_id FROM import_queue WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatalf("read queue row: %v", err)
	}
	if stored.Valid {
		t.Errorf("a rejected match still wrote %+v", stored)
	}
}

// TestMatchRejectsOtherInstanceTarget pins the route mapping for the instance
// guard: a cache row synced by another arr instance of the same kind is refused
// with 409 and the instance_mismatch code, and nothing is written.
func TestMatchRejectsOtherInstanceTarget(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{}, 2*time.Second)
	ctx := context.Background()

	res, err := h.db.ExecContext(ctx,
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		 VALUES ('second', 'radarr', 'http://second.invalid', x'00', '0000')`)
	if err != nil {
		t.Fatalf("insert second instance: %v", err)
	}
	otherInst, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("second instance id: %v", err)
	}
	res, err = h.db.ExecContext(ctx,
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, title, original_language_iso2, year,
		  target_file_path, audio_languages_json, eligibility_status)
		 VALUES (?, 'movie', 9, 'Kraina Lodu', 'en', 2013, '', '[]', 'eligible')`, otherInst)
	if err != nil {
		t.Fatalf("insert other-instance cache row: %v", err)
	}
	otherCache, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("other-instance cache id: %v", err)
	}
	id := h.seedCandidateWithTarget(t, "Kraina.Lodu.2013.PL.mkv")

	rec := h.post("/api/queue/"+itoa(id)+"/match", `{"library_cache_id":`+itoa(otherCache)+`}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("match = %d, body %s", rec.Code, rec.Body.String())
	}
	if body := decodeErrorBody(t, rec); body.Code != "instance_mismatch" {
		t.Errorf("conflict code = %q, want instance_mismatch", body.Code)
	}
	var stored sql.NullInt64
	if err := h.db.QueryRowContext(ctx,
		`SELECT matched_library_cache_id FROM import_queue WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatalf("read queue row: %v", err)
	}
	if stored.Valid {
		t.Errorf("a rejected match still wrote %+v", stored)
	}
}

// formatFloat renders a confidence so the test posts a real value instead of a
// hand-written literal.
func formatFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
