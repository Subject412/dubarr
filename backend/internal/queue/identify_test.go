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

// identifyHarness is a queue API wired to one in-process arr fake of each kind.
type identifyHarness struct {
	t      *testing.T
	db     *sql.DB
	mux    *http.ServeMux
	media  string
	srv    *arrmock.Server
	sonarr *arrmock.Server
	instID int64
	// svc is the service behind the mux, so a test can inject the countability
	// seams the routes otherwise keep private.
	svc *Service
	// sonarrID is the Sonarr instance row, so an episode cache row can be
	// synced by the instance that actually serves it.
	sonarrID int64
	// settings is kept so a test can declare which libraries are enabled: the
	// route's ambiguity is a property of arr.connections, not of the mocks.
	settings *settings.Store
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

	// The Sonarr fake answers the same release in its own shape, so one fixture
	// serves both kinds: a test asks for a title, not for a product payload.
	sonarrTitle := fx.Parse.SeriesTitle
	if sonarrTitle == "" {
		sonarrTitle = fx.Parse.Title
	}
	sonarrYear := fx.Parse.SeriesYear
	if sonarrYear == 0 {
		sonarrYear = fx.Parse.Year
	}
	sonarrSrv := arrmock.New(arrmock.Sonarr, "", arrmock.Fixtures{
		Parse: arrmock.ParseResponse{SeriesTitle: sonarrTitle, SeriesYear: sonarrYear, Quality: fx.Parse.Quality},
	})
	t.Cleanup(sonarrSrv.Close)

	blob := []byte("blob-1")
	sonarrBlob := []byte("blob-2")
	instID := insertArrInstance(t, st.DB, "main", "radarr", srv.URL, blob)
	sonarrID := insertArrInstance(t, st.DB, "tv", "sonarr", sonarrSrv.URL, sonarrBlob)

	mux := http.NewServeMux()
	svc := RegisterRoutes(mux, Dependencies{
		DB:       st.DB,
		Settings: settingsStore,
		Secrets: fakeDecrypter{
			string(blob):       srv.APIKey(),
			string(sonarrBlob): sonarrSrv.APIKey(),
		},
		IdentifyTimeout: timeout,
		RequireAdmin: func(h http.Handler) http.Handler {
			return h
		},
	})
	return &identifyHarness{
		t:        t,
		db:       st.DB,
		mux:      mux,
		media:    t.TempDir(),
		srv:      srv,
		sonarr:   sonarrSrv,
		svc:      svc,
		instID:   instID,
		sonarrID: sonarrID,
		settings: settingsStore,
	}
}

// insertArrInstance records one arr instance row and returns its id.
func insertArrInstance(t *testing.T, db *sql.DB, name, kind, baseURL string, blob []byte) int64 {
	t.Helper()
	res, err := db.ExecContext(context.Background(),
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		 VALUES (?, ?, ?, ?, '0000')`, name, kind, baseURL, blob)
	if err != nil {
		t.Fatalf("insert arr instance %s: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("%s instance id: %v", name, err)
	}
	return id
}

// enableArrConnections declares which libraries the appliance has enabled.
// Called with nothing, the section stays empty: no library is configured.
func (h *identifyHarness) enableArrConnections(t *testing.T, conns ...settings.ArrConnection) {
	t.Helper()
	if conns == nil {
		conns = []settings.ArrConnection{}
	}
	raw, err := json.Marshal(settings.ArrConnections{Instances: conns})
	if err != nil {
		t.Fatalf("marshal arr connections: %v", err)
	}
	if err := h.settings.Set(context.Background(), settings.KeyArrConnections, raw); err != nil {
		t.Fatalf("set arr connections: %v", err)
	}
}

// seedUnmatchedCandidate records a pending queue row with no arr instance and
// no library link: it proves no kind, exactly like the live unmatched release
// Identify was built for.
func (h *identifyHarness) seedUnmatchedCandidate(t *testing.T, filename string) int64 {
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
	return id
}

// linkCandidate points a queue row at one library cache row, which is what
// makes the release prove a media kind.
func (h *identifyHarness) linkCandidate(t *testing.T, queueID, cacheID int64) {
	t.Helper()
	if _, err := h.db.ExecContext(context.Background(),
		`UPDATE import_queue SET matched_library_cache_id = ? WHERE id = ?`, cacheID, queueID); err != nil {
		t.Fatalf("link candidate %d to cache %d: %v", queueID, cacheID, err)
	}
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
// (empty) folder. An episode row belongs to the Sonarr instance, a movie row to
// the Radarr one.
func (h *identifyHarness) seedCacheRow(t *testing.T, mediaType, title string, year int, externalID int64) int64 {
	t.Helper()
	targetPath := filepath.Join(h.media, title+".mkv")
	if err := os.WriteFile(targetPath, []byte("target"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	instanceID := h.instID
	if mediaType == "episode" {
		instanceID = h.sonarrID
	}
	res, err := h.db.ExecContext(context.Background(),
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, title, original_language_iso2, year,
		  target_file_path, audio_languages_json, eligibility_status)
		 VALUES (?, ?, ?, ?, 'en', ?, ?, '["en"]', 'eligible')`,
		instanceID, mediaType, externalID, title, year, targetPath)
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
		Error      string  `json:"error"`
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
	Error string   `json:"error"`
	Code  string   `json:"code"`
	Class string   `json:"class"`
	Kinds []string `json:"kinds"`
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
// ranked deterministically with the release name sent to arr.
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

// TestIdentifyUnparsableReleaseIsDistinct pins the shape a release arr could not
// read answers with: parsed=false, no invented title and no arr failure. With
// nothing in the synced library that fits the name, the answer is still 200 with
// an empty candidate list — never a 502, which would read as "identify broke".
func TestIdentifyUnparsableReleaseIsDistinct(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{
		// An all-zero fixture is an instance that could not parse anything: it
		// echoes the release name back as the top-level title and stops there.
		Parse: arrmock.ParseResponse{},
	}, 2*time.Second)
	h.seedCacheRow(t, "movie", "Kraina Lodu", 2013, 1)
	id := h.seedCandidateWithTarget(t, "Nieznany.Film.PL.DUB-GRP.mkv")

	rec := h.post("/api/queue/"+itoa(id)+"/identify", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("identify = %d, body %s (an unparsable name is not an arr failure)", rec.Code, rec.Body.String())
	}
	body := decodeIdentify(t, rec)
	if body.Parse.Parsed {
		t.Errorf("parse.parsed = true for an unparsable release: %+v", body.Parse)
	}
	if body.Parse.Title != "" {
		t.Errorf("parse.title = %q, want empty (no invented title)", body.Parse.Title)
	}
	// arr answered; it simply could not read the name, so there is no failure
	// message to show.
	if body.Parse.Error != "" {
		t.Errorf("parse.error = %q, want empty: the instance answered", body.Parse.Error)
	}
	if !strings.Contains(rec.Body.String(), `"candidates":[]`) {
		t.Errorf("body %s does not carry an empty candidate array", rec.Body.String())
	}
	if len(body.Candidates) != 0 {
		t.Errorf("candidates = %+v, want none: nothing in the library fits", body.Candidates)
	}
}

// TestIdentifyFallsBackToTheMirrorWhenParseFails pins the fallback: a name arr
// cannot parse is exactly the case where the synced library should answer, so
// identify still searches it and offers what fits. Failing today would answer
// 200 with an empty candidate array and the operator would see nothing.
func TestIdentifyFallsBackToTheMirrorWhenParseFails(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{
		// {"parsed": false} in arr terms: a 200 with no recognised title.
		Parse: arrmock.ParseResponse{},
	}, 2*time.Second)
	shrek := h.seedCacheRow(t, "movie", "Shrek", 2001, 1)
	id := h.seedCandidateWithTarget(t, "Shrek.2001.1080p.PL.DUB-GRP.mkv")

	rec := h.post("/api/queue/"+itoa(id)+"/identify", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("identify = %d, body %s", rec.Code, rec.Body.String())
	}
	body := decodeIdentify(t, rec)
	if body.Parse.Parsed {
		t.Fatalf("parse.parsed = true, want false (the fixture parses nothing): %+v", body.Parse)
	}
	if len(body.Candidates) != 1 {
		t.Fatalf("candidates = %+v, want the one Shrek row from the mirror", body.Candidates)
	}
	got := body.Candidates[0]
	if got.LibraryCacheID != shrek {
		t.Errorf("candidate id = %d, want %d", got.LibraryCacheID, shrek)
	}
	if got.MatchedBy != "mirror" {
		t.Errorf("matched_by = %q, want mirror", got.MatchedBy)
	}
	if got.Confidence <= 0 {
		t.Errorf("confidence = %v, want a positive score", got.Confidence)
	}
	// A substring hit must never outrank a parse, and 0.5 is the parse floor.
	if got.Confidence > 0.5 {
		t.Errorf("confidence = %v, want <= 0.5 (a mirror hit is the weaker evidence)", got.Confidence)
	}
	if got.Title != "Shrek" || got.Year != 2001 {
		t.Errorf("candidate = %+v, want Shrek (2001)", got)
	}
}

// TestIdentifyMergePutsParseBeforeMirrorAndDedupes pins the merge order: the
// rows arr's parser named come first, in their ranked order, then only the
// mirror hits the parse did not already offer. A row both searches found is
// listed once, carrying the parse score, because a second entry would read to
// the operator as two different films.
//
// The release is named "Dune.2024" while arr resolves it to "Dune Part Two",
// which is how the 2024 file is really named, so the parse and the mirror
// really do search the same library rows here.
func TestIdentifyMergePutsParseBeforeMirrorAndDedupes(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{
		Parse: arrmock.ParseResponse{Title: "Dune Part Two", Year: 2024, Quality: "Bluray-1080p"},
	}, 2*time.Second)
	// The sequel: the parse's exact hit, and a weaker mirror hit on "Dune".
	sequel := h.seedCacheRow(t, "movie", "Dune Part Two", 2024, 1)
	// The 1984 original: both lists carry it, so the merge must keep it once.
	original := h.seedCacheRow(t, "movie", "Dune", 1984, 2)
	// A third Dune the parser does not name: it shares no title with the parse,
	// so it reaches the operator from the mirror alone.
	third := h.seedCacheRow(t, "movie", "Dune Prophecy", 2024, 3)
	id := h.seedCandidateWithTarget(t, "Dune.2024.PL.1080p-GRP.mkv")

	rec := h.post("/api/queue/"+itoa(id)+"/identify", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("identify = %d, body %s", rec.Code, rec.Body.String())
	}
	body := decodeIdentify(t, rec)
	if !body.Parse.Parsed {
		t.Fatalf("parse.parsed = false, want the fixture's verdict: %+v", body.Parse)
	}
	if len(body.Candidates) != 3 {
		t.Fatalf("candidates = %+v, want the parse hits then the mirror-only row", body.Candidates)
	}

	// Parse first (ranked 1 then 0.6), the mirror-only row last.
	want := []int64{sequel, original, third}
	for i, wantID := range want {
		if body.Candidates[i].LibraryCacheID != wantID {
			t.Errorf("candidate[%d].library_cache_id = %d, want %d (candidates %+v)",
				i, body.Candidates[i].LibraryCacheID, wantID, body.Candidates)
		}
	}

	// The row both searches found appears exactly once.
	counts := make(map[int64]int, len(body.Candidates))
	for _, cand := range body.Candidates {
		counts[cand.LibraryCacheID]++
	}
	if counts[original] != 1 {
		t.Errorf("candidate %d appears %d times, want exactly once: %+v", original, counts[original], body.Candidates)
	}

	if got := body.Candidates[0]; got.Confidence != 1 || got.MatchedBy != "parse" {
		t.Errorf("candidate[0] = %+v, want the parsed sequel at confidence 1 labelled parse", got)
	}
	// The doubly-matched row keeps the parse score: 0.6 outranks the 0.5 mirror
	// cap, and its label must not flip to mirror just because the mirror saw it.
	if got := body.Candidates[1]; got.Confidence != 0.6 || got.MatchedBy != "parse" {
		t.Errorf("the doubly-matched row = %+v, want the parse confidence 0.6 labelled parse, not the capped mirror 0.5", got)
	}
	if got := body.Candidates[2]; got.Confidence != 0.5 || got.MatchedBy != "mirror" {
		t.Errorf("candidate[2] = %+v, want the mirror-only row at the 0.5 ceiling labelled mirror", got)
	}
}

// TestMergeIdentifyCandidatesCapsTheTotal pins the limit against the mirror: it
// is a cap on the whole list, not on each search. A parse list already at the
// cap must not receive one mirror row on top of it — the check used to sit after
// the append, so a full first call still let the second call add a 21st row.
func TestMergeIdentifyCandidatesCapsTheTotal(t *testing.T) {
	parse := make([]identifyCandidate, 0, identifyCandidateLimit)
	for i := 0; i < identifyCandidateLimit; i++ {
		parse = append(parse, identifyCandidate{
			LibraryCacheID: int64(100 + i),
			Title:          "Kraina Lodu",
			Confidence:     1,
			MatchedBy:      "parse",
		})
	}
	mirror := []identifyCandidate{{
		LibraryCacheID: 999,
		Title:          "Shrek",
		Confidence:     mirrorCandidateCeiling,
		MatchedBy:      "mirror",
	}}

	got := mergeIdentifyCandidates(parse, mirror)
	if len(got) != identifyCandidateLimit {
		t.Fatalf("merged %d candidates, want %d: the limit caps the total, so a full parse list leaves no room for the mirror",
			len(got), identifyCandidateLimit)
	}
	for i, cand := range got {
		if cand.LibraryCacheID != parse[i].LibraryCacheID || cand.MatchedBy != "parse" {
			t.Errorf("candidate[%d] = %+v, want the parse candidate %d intact",
				i, cand, parse[i].LibraryCacheID)
		}
	}
	for _, cand := range got {
		if cand.LibraryCacheID == mirror[0].LibraryCacheID {
			t.Errorf("mirror row %d was padded onto a full parse list: %+v", cand.LibraryCacheID, got)
		}
	}
}

// TestIdentifyArrUnreachableStillOffersTheMirror pins the shape an unreachable
// instance answers with: 200, parse.error set, parsed still false, and the one
// row the synced library already holds. A transport failure must not deny the
// operator the library they have — answering 502 because arr is down would hide
// a match the appliance can prove from its own cache.
func TestIdentifyArrUnreachableStillOffersTheMirror(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{
		Parse: arrmock.ParseResponse{Title: "Kraina Lodu", Year: 2013, Quality: "Bluray-1080p"},
	}, 2*time.Second)
	shrek := h.seedCacheRow(t, "movie", "Shrek", 2001, 1)
	id := h.seedCandidateWithTarget(t, "Shrek.2001.1080p.PL.DUB-GRP.mkv")

	// Nothing listens on loopback port 1, so the parse dies in transport before
	// arr ever reads a body.
	if _, err := h.db.ExecContext(context.Background(),
		`UPDATE arr_instances SET base_url = 'http://127.0.0.1:1' WHERE id = ?`, h.instID); err != nil {
		t.Fatalf("break the arr base URL: %v", err)
	}

	rec := h.post("/api/queue/"+itoa(id)+"/identify", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("identify = %d, body %s (an unreachable arr with a mirror hit is not a 502)", rec.Code, rec.Body.String())
	}
	body := decodeIdentify(t, rec)
	if body.Parse.Error == "" {
		t.Errorf("parse.error is empty: an unreachable instance must say why it could not parse: %+v", body.Parse)
	}
	if body.Parse.Parsed {
		t.Errorf("parse.parsed = true, want false (the verdict is unknown, not successful): %+v", body.Parse)
	}
	if len(body.Candidates) != 1 {
		t.Fatalf("candidates = %+v, want the one cached Shrek row", body.Candidates)
	}
	got := body.Candidates[0]
	if got.LibraryCacheID != shrek {
		t.Errorf("candidate id = %d, want %d", got.LibraryCacheID, shrek)
	}
	if got.MatchedBy != "mirror" {
		t.Errorf("matched_by = %q, want mirror: arr never named this row", got.MatchedBy)
	}
}

// TestIdentifyMirrorSearchIsBounded pins the ladder's cost: however unhelpful the
// release name is, one identify reads the pool at most four times. The counting
// lookup answers every rung with an empty pool, which is the worst case — no
// rung satisfies the ladder, so it must walk its rungs and stop there.
func TestIdentifyMirrorSearchIsBounded(t *testing.T) {
	cases := map[string]string{
		"forty words": strings.TrimSpace(strings.Repeat("slowowolny ", 40)),
		// Wide gaps between words: Fields keeps three words, and the ladder must
		// still not re-query the same rung.
		"many spaces": "slowowolny" + strings.Repeat(" ", 60) + "film" + strings.Repeat(" ", 60) + "tytul",
	}
	for name, title := range cases {
		t.Run(name, func(t *testing.T) {
			// An instance that parses nothing, so the parse pool is never read:
			// every pool read counted here belongs to the mirror ladder.
			h := newIdentifyHarness(t, arrmock.Fixtures{Parse: arrmock.ParseResponse{}}, 2*time.Second)
			var terms []string
			h.svc.identifyPool = func(_ context.Context, _ int64, _ string, term string) ([]store.LibraryCandidate, error) {
				terms = append(terms, term)
				return nil, nil
			}
			id := h.seedCandidateWithTarget(t, "Nieznany.Film.PL.DUB-GRP.mkv")

			body, err := json.Marshal(map[string]string{"title": title})
			if err != nil {
				t.Fatalf("marshal override: %v", err)
			}
			rec := h.post("/api/queue/"+itoa(id)+"/identify", string(body))
			if rec.Code != http.StatusOK {
				t.Fatalf("identify = %d, body %s", rec.Code, rec.Body.String())
			}

			if len(terms) == 0 {
				t.Fatal("the mirror never searched the library: the bound must not be met by not searching")
			}
			if len(terms) > 4 {
				t.Errorf("IdentifyCandidatePool calls = %d (%v), want at most 4: the ladder must stop", len(terms), terms)
			}
			seen := make(map[string]struct{}, len(terms))
			for _, term := range terms {
				if _, dup := seen[term]; dup {
					t.Errorf("the ladder queried %q twice: %v", term, terms)
				}
				seen[term] = struct{}{}
			}
		})
	}
}

// TestIdentifyOverrideSteersTheMirror pins that an operator's override is also
// the name the mirror searches: a file named Shrek.2001... posted with the
// override "Dune.Part.Two.2024.1080p" offers the Dune (1984) row and not Shrek,
// even though Shrek is in the same library and is the file's own name.
//
// This is intended, not a bug. The override is the operator saying what the
// release IS, so the mirror searching it agrees with the parse instead of
// contradicting it. A wrong override is an operator decision; quietly falling
// back to the filename would second-guess a human who just told the route the
// answer. The parse fixture parses nothing, so the Dune row can only be the
// mirror's answer for the override.
func TestIdentifyOverrideSteersTheMirror(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{
		Parse: arrmock.ParseResponse{},
	}, 2*time.Second)
	h.seedCacheRow(t, "movie", "Shrek", 2001, 1)
	dune := h.seedCacheRow(t, "movie", "Dune", 1984, 2)
	id := h.seedCandidateWithTarget(t, "Shrek.2001.1080p.PL.DUB-GRP.mkv")

	// The operator's override names a different film; the route may not ignore it.
	override := `{"title":"Dune.Part.Two.2024.1080p"}`
	rec := h.post("/api/queue/"+itoa(id)+"/identify", override)
	if rec.Code != http.StatusOK {
		t.Fatalf("identify with override = %d, body %s", rec.Code, rec.Body.String())
	}
	body := decodeIdentify(t, rec)
	if body.Parse.Parsed {
		t.Fatalf("parse.parsed = true, want false (the fixture parses nothing): %+v", body.Parse)
	}
	if len(body.Candidates) != 1 {
		t.Fatalf("candidates = %+v, want only the Dune row the override names", body.Candidates)
	}
	got := body.Candidates[0]
	if got.LibraryCacheID != dune {
		t.Errorf("candidate id = %d, want %d: the mirror must search the override, not the file stem", got.LibraryCacheID, dune)
	}
	if got.MatchedBy != "mirror" {
		t.Errorf("matched_by = %q, want mirror", got.MatchedBy)
	}
	if got.Title != "Dune" || got.Year != 1984 {
		t.Errorf("candidate = %+v, want Dune (1984)", got)
	}
	for _, cand := range body.Candidates {
		if cand.Title == "Shrek" {
			t.Errorf("candidates carry Shrek %+v: the override named Dune, so the file's own name must not be searched too", cand)
		}
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
	if !confidence.Valid || confidence.String != "manual" {
		t.Errorf("parse_confidence = %+v, want manual (a human pick is not an exact parse)", confidence)
	}
	if confidence.String == "ambiguous" {
		t.Errorf("parse_confidence = %q, want manual, not ambiguous", confidence.String)
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

// TestIdentifyAmbiguousKindOffersChoice pins the live defect: with both
// libraries enabled, an unmatched release is a choice, not an absence. The
// route answers 409 ambiguous_kind carrying both kinds in a fixed order, and it
// asks neither instance anything — no library was chosen, so no parse may run.
func TestIdentifyAmbiguousKindOffersChoice(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{
		Parse: arrmock.ParseResponse{Title: "Shrek", Year: 2001, Quality: "WEBDL-1080p"},
	}, 2*time.Second)
	h.enableArrConnections(t,
		settings.ArrConnection{Kind: "radarr", BaseURL: h.srv.URL, Enabled: true},
		settings.ArrConnection{Kind: "sonarr", BaseURL: h.sonarr.URL, Enabled: true},
	)
	id := h.seedUnmatchedCandidate(t, "Shrek.2001.1080p.SKST.WEB-DL.PL.KIFR.mkv")

	rec := h.post("/api/queue/"+itoa(id)+"/identify", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("identify = %d, body %s (an ambiguous appliance is not an unconfigured one)", rec.Code, rec.Body.String())
	}
	body := decodeErrorBody(t, rec)
	if body.Code != "ambiguous_kind" {
		t.Fatalf("code = %q, want ambiguous_kind (body %s)", body.Code, rec.Body.String())
	}
	if len(body.Kinds) != 2 || body.Kinds[0] != "radarr" || body.Kinds[1] != "sonarr" {
		t.Errorf("kinds = %v, want [radarr sonarr]", body.Kinds)
	}
	for _, kind := range []string{"radarr", "sonarr"} {
		if !strings.Contains(body.Error, kind) {
			t.Errorf("message %q does not name %s", body.Error, kind)
		}
	}

	// The choice is the answer: nothing may be parsed before the operator picks.
	if posted := h.srv.ParseRequests(); len(posted) != 0 {
		t.Errorf("radarr parses = %v, want none", posted)
	}
	if posted := h.sonarr.ParseRequests(); len(posted) != 0 {
		t.Errorf("sonarr parses = %v, want none", posted)
	}
}

// TestIdentifyChosenKindParsesThatLibrary pins the way forward the 409 offers:
// the operator's kind decides which instance parses the release, and the pool is
// narrowed to that library's media kind.
func TestIdentifyChosenKindParsesThatLibrary(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{
		Parse: arrmock.ParseResponse{Title: "Kraina Lodu", Year: 2013, Quality: "WEBDL-1080p"},
	}, 2*time.Second)
	h.enableArrConnections(t,
		settings.ArrConnection{Kind: "radarr", BaseURL: h.srv.URL, Enabled: true},
		settings.ArrConnection{Kind: "sonarr", BaseURL: h.sonarr.URL, Enabled: true},
	)
	h.seedCacheRow(t, "movie", "Kraina Lodu", 2013, 1)
	episode := h.seedCacheRow(t, "episode", "Kraina Lodu", 2013, 2)
	id := h.seedUnmatchedCandidate(t, "Kraina.Lodu.2013.PL.mkv")

	rec := h.post("/api/queue/"+itoa(id)+"/identify", `{"kind":"sonarr"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("identify = %d, body %s", rec.Code, rec.Body.String())
	}
	body := decodeIdentify(t, rec)
	if len(body.Candidates) != 1 || body.Candidates[0].LibraryCacheID != episode {
		t.Fatalf("candidates = %+v, want only the episode row %d", body.Candidates, episode)
	}
	if body.Candidates[0].MediaType != "episode" {
		t.Errorf("candidate media_type = %q, want episode", body.Candidates[0].MediaType)
	}

	posted := h.sonarr.ParseRequests()
	if len(posted) != 1 || posted[0] != "Kraina.Lodu.2013.PL" {
		t.Errorf("sonarr parses = %v, want the filename stem once", posted)
	}
	if posted := h.srv.ParseRequests(); len(posted) != 0 {
		t.Errorf("radarr parses = %v, want none: the operator chose sonarr", posted)
	}
}

// TestIdentifySingleEnabledKindAutoSelects pins that one enabled library needs
// no body kind: the appliance has nothing to choose between.
func TestIdentifySingleEnabledKindAutoSelects(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{
		Parse: arrmock.ParseResponse{Title: "Kraina Lodu", Year: 2013, Quality: "WEBDL-1080p"},
	}, 2*time.Second)
	h.enableArrConnections(t,
		settings.ArrConnection{Kind: "sonarr", BaseURL: h.sonarr.URL, Enabled: true},
	)
	episode := h.seedCacheRow(t, "episode", "Kraina Lodu", 2013, 2)
	id := h.seedUnmatchedCandidate(t, "Kraina.Lodu.2013.PL.mkv")

	rec := h.post("/api/queue/"+itoa(id)+"/identify", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("identify = %d, body %s", rec.Code, rec.Body.String())
	}
	body := decodeIdentify(t, rec)
	if len(body.Candidates) != 1 || body.Candidates[0].LibraryCacheID != episode {
		t.Errorf("candidates = %+v, want the episode row %d", body.Candidates, episode)
	}
	if posted := h.sonarr.ParseRequests(); len(posted) != 1 {
		t.Errorf("sonarr parses = %v, want exactly one", posted)
	}
	if posted := h.srv.ParseRequests(); len(posted) != 0 {
		t.Errorf("radarr parses = %v, want none (radarr is not enabled)", posted)
	}
}

// TestIdentifyNoEnabledKindIsNotConfigured keeps not_configured for genuine
// absence: a secret store is wired and instances exist, but nothing is enabled.
func TestIdentifyNoEnabledKindIsNotConfigured(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{}, 2*time.Second)
	id := h.seedUnmatchedCandidate(t, "Shrek.2001.1080p.SKST.WEB-DL.PL.KIFR.mkv")

	rec := h.post("/api/queue/"+itoa(id)+"/identify", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("identify = %d, body %s", rec.Code, rec.Body.String())
	}
	body := decodeErrorBody(t, rec)
	if body.Code != "not_configured" {
		t.Errorf("code = %q, want not_configured", body.Code)
	}
	if body.Error != "no arr instance is configured for this release" {
		t.Errorf("message = %q, want the not_configured message", body.Error)
	}
	if len(body.Kinds) != 0 {
		t.Errorf("kinds = %v, want none: nothing is configured, so nothing is offered", body.Kinds)
	}
}

// TestIdentifyKindConflictIsRefused pins that the operator's kind is never
// silently overridden: a release the row already proves to be a film cannot be
// identified as a series, and nothing is parsed on the way to saying so.
func TestIdentifyKindConflictIsRefused(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{
		Parse: arrmock.ParseResponse{Title: "Kraina Lodu", Year: 2013, Quality: "Bluray-1080p"},
	}, 2*time.Second)
	movie := h.seedCacheRow(t, "movie", "Kraina Lodu", 2013, 1)
	id := h.seedCandidateWithTarget(t, "Kraina.Lodu.2013.PL.mkv")
	h.linkCandidate(t, id, movie)

	rec := h.post("/api/queue/"+itoa(id)+"/identify", `{"kind":"sonarr"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("identify = %d, body %s", rec.Code, rec.Body.String())
	}
	body := decodeErrorBody(t, rec)
	if body.Code != "kind_conflict" {
		t.Fatalf("code = %q, want kind_conflict (body %s)", body.Code, rec.Body.String())
	}
	if !strings.Contains(body.Error, "radarr") || !strings.Contains(body.Error, "sonarr") {
		t.Errorf("message %q does not name both kinds", body.Error)
	}
	if posted := h.srv.ParseRequests(); len(posted) != 0 {
		t.Errorf("radarr parses = %v, want none", posted)
	}
	if posted := h.sonarr.ParseRequests(); len(posted) != 0 {
		t.Errorf("sonarr parses = %v, want none", posted)
	}
}

// TestIdentifyRejectsUnknownKind pins the 400: a body kind outside the two
// served libraries is a bad request, never a silently ignored field.
func TestIdentifyRejectsUnknownKind(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{
		Parse: arrmock.ParseResponse{Title: "Kraina Lodu", Year: 2013, Quality: "Bluray-1080p"},
	}, 2*time.Second)
	id := h.seedCandidateWithTarget(t, "Kraina.Lodu.2013.PL.mkv")

	rec := h.post("/api/queue/"+itoa(id)+"/identify", `{"kind":"plex"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("identify = %d, body %s", rec.Code, rec.Body.String())
	}
	if body := decodeErrorBody(t, rec); body.Code != "invalid_kind" {
		t.Errorf("code = %q, want invalid_kind", body.Code)
	}
	if posted := h.srv.ParseRequests(); len(posted) != 0 {
		t.Errorf("parses = %v, want none for a rejected body", posted)
	}
}

// TestIdentifySettingsReadFailureIsNotAbsence pins the 500: a configuration
// that could not be read must never be reported as "nothing configured", which
// would send the operator to add an instance that already exists.
func TestIdentifySettingsReadFailureIsNotAbsence(t *testing.T) {
	h := newIdentifyHarness(t, arrmock.Fixtures{}, 2*time.Second)
	id := h.seedUnmatchedCandidate(t, "Shrek.2001.1080p.SKST.WEB-DL.PL.KIFR.mkv")

	// A row the settings decoder cannot read: the read fails, nothing is known.
	if _, err := h.db.ExecContext(context.Background(),
		`UPDATE settings SET value = '{not json' WHERE key = 'arr.connections'`); err != nil {
		t.Fatalf("corrupt arr.connections: %v", err)
	}

	rec := h.post("/api/queue/"+itoa(id)+"/identify", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("identify = %d, body %s (a failed read is not an empty configuration)", rec.Code, rec.Body.String())
	}
	if body := decodeErrorBody(t, rec); body.Code == "not_configured" {
		t.Errorf("code = %q, want a server fault, not not_configured", body.Code)
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
