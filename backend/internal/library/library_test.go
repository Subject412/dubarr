package library

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

type harness struct {
	t        *testing.T
	mux      *http.ServeMux
	db       *store.Store
	settings *settings.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	settingsStore := settings.New(st.DB)

	mux := http.NewServeMux()
	RegisterRoutes(mux, Dependencies{
		DB:       st.DB,
		Settings: settingsStore,
		RequireAdmin: func(h http.Handler) http.Handler {
			return h
		},
	})
	return &harness{t: t, mux: mux, db: st, settings: settingsStore}
}

func (h *harness) get(path string) *httptest.ResponseRecorder {
	h.t.Helper()
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// itemsPayload is the decoded /api/library/items body the filter tests assert
// on: only the fields those tests read.
type itemsPayload struct {
	Items []struct {
		ID         int64  `json:"id"`
		Title      string `json:"title"`
		InstanceID int64  `json:"instance_id"`
	} `json:"items"`
	Total int64 `json:"total"`
}

// items performs the GET and decodes the page, failing on a non-200 so the
// filter tests can concentrate on what came back.
func (h *harness) items(t *testing.T, path string) itemsPayload {
	t.Helper()
	rec := h.get(path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, body %s", path, rec.Code, rec.Body.String())
	}
	var body itemsPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return body
}

// insertInstance seeds one arr_instances row and returns its id.
func (h *harness) insertInstance(t *testing.T, name, kind string, enabled bool) int64 {
	t.Helper()
	enabledFlag := 0
	if enabled {
		enabledFlag = 1
	}
	res, err := h.db.DB.ExecContext(context.Background(),
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4, enabled)
		 VALUES (?, ?, 'http://arr.invalid', x'00', '0000', ?)`, name, kind, enabledFlag)
	if err != nil {
		t.Fatalf("insert arr instance %s: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("arr instance id: %v", err)
	}
	return id
}

// insertMovie seeds a fully populated movie cache row so the contract test
// asserts against the real store projection rather than a hand-built item.
func (h *harness) insertMovie(t *testing.T, instanceID int64, title string, year int) int64 {
	t.Helper()
	res, err := h.db.DB.ExecContext(context.Background(),
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, tmdb_id, title, original_title,
		  year, media_folder_path, root_folder_path, target_file_path,
		  matches_target_tags, eligibility_status, updated_at, last_seen_at)
		 VALUES (?, 'movie', 7, 1234, ?, ?, ?, '/media/movies/' || ?, '/media/movies',
		         '/media/movies/' || ? || '.mkv', 1, 'eligible',
		         '2026-01-02 03:04:05', '2026-01-03 04:05:06')`,
		instanceID, title, title, year, title, title)
	if err != nil {
		t.Fatalf("insert library movie %s: %v", title, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("library row id: %v", err)
	}
	return id
}

// insertCacheRow seeds a minimal cache row for the per-instance counts.
func (h *harness) insertCacheRow(t *testing.T, instanceID, externalID int64) {
	t.Helper()
	if _, err := h.db.DB.ExecContext(context.Background(),
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, title, eligibility_status)
		 VALUES (?, 'movie', ?, 'Row', 'missing_target')`,
		instanceID, externalID); err != nil {
		t.Fatalf("insert cache row %d: %v", externalID, err)
	}
}

// insertEpisodeCacheRow seeds one episode cache row the way a Sonarr sync
// writes it: media_type 'episode' with the series id set. Many rows share one
// series_id, which is why the series count is distinct.
func (h *harness) insertEpisodeCacheRow(t *testing.T, instanceID, externalID, seriesID int64) {
	t.Helper()
	if _, err := h.db.DB.ExecContext(context.Background(),
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, series_id, title, eligibility_status)
		 VALUES (?, 'episode', ?, ?, 'Episode', 'missing_target')`,
		instanceID, externalID, seriesID); err != nil {
		t.Fatalf("insert episode cache row %d: %v", externalID, err)
	}
}

// sortedKeys returns the object's keys in a stable order for exact-set checks.
func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// TestLibraryItemsContract pins the payload: exactly the documented keys, with
// a real cache row projected through the store.
func TestLibraryItemsContract(t *testing.T) {
	h := newHarness(t)
	instanceID := h.insertInstance(t, "main", "radarr", true)
	id := h.insertMovie(t, instanceID, "Shrek", 2001)

	rec := h.get("/api/library/items")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/library/items = %d, body %s", rec.Code, rec.Body.String())
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &top); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	wantTop := []string{"items", "limit", "offset", "total"}
	if got := sortedKeys(top); !slices.Equal(got, wantTop) {
		t.Fatalf("top-level keys = %v, want %v", got, wantTop)
	}

	var rawItems []map[string]json.RawMessage
	if err := json.Unmarshal(top["items"], &rawItems); err != nil {
		t.Fatalf("decode items: %v", err)
	}
	if len(rawItems) != 1 {
		t.Fatalf("items = %d, want 1", len(rawItems))
	}
	wantKeys := []string{
		"eligibility_status", "episode", "id", "instance_id", "instance_name",
		"kind", "last_seen_at", "matches_target_tags", "media_type",
		"root_folder_path", "season", "target_file_path", "title", "updated_at", "year",
	}
	if got := sortedKeys(rawItems[0]); !slices.Equal(got, wantKeys) {
		t.Fatalf("item keys = %v, want %v", got, wantKeys)
	}
	// Movies carry no season/episode numbers, so both must be JSON null.
	if string(rawItems[0]["season"]) != "null" || string(rawItems[0]["episode"]) != "null" {
		t.Fatalf("season/episode = %s/%s, want null/null",
			rawItems[0]["season"], rawItems[0]["episode"])
	}

	var body struct {
		Items []struct {
			ID                int64  `json:"id"`
			MediaType         string `json:"media_type"`
			Title             string `json:"title"`
			Year              int    `json:"year"`
			Kind              string `json:"kind"`
			InstanceID        int64  `json:"instance_id"`
			InstanceName      string `json:"instance_name"`
			EligibilityStatus string `json:"eligibility_status"`
			MatchesTargetTags bool   `json:"matches_target_tags"`
			RootFolderPath    string `json:"root_folder_path"`
			TargetFilePath    string `json:"target_file_path"`
			LastSeenAt        string `json:"last_seen_at"`
			UpdatedAt         string `json:"updated_at"`
		} `json:"items"`
		Total  int64 `json:"total"`
		Limit  int   `json:"limit"`
		Offset int   `json:"offset"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if body.Total != 1 || body.Limit != store.LibraryPageDefaultLimit || body.Offset != 0 {
		t.Errorf("total/limit/offset = %d/%d/%d, want 1/%d/0",
			body.Total, body.Limit, body.Offset, store.LibraryPageDefaultLimit)
	}
	item := body.Items[0]
	if item.ID != id || item.MediaType != "movie" || item.Title != "Shrek" || item.Year != 2001 {
		t.Errorf("item identity = %+v, want movie Shrek (2001) id %d", item, id)
	}
	if item.Kind != "radarr" || item.InstanceID != instanceID || item.InstanceName != "main" {
		t.Errorf("item instance = %s/%d/%s, want radarr/%d/main",
			item.Kind, item.InstanceID, item.InstanceName, instanceID)
	}
	if item.EligibilityStatus != "eligible" || !item.MatchesTargetTags {
		t.Errorf("eligibility = %q matches=%v, want eligible/true",
			item.EligibilityStatus, item.MatchesTargetTags)
	}
	if item.RootFolderPath != "/media/movies" {
		t.Errorf("root_folder_path = %q, want /media/movies", item.RootFolderPath)
	}
	if item.TargetFilePath != "/media/movies/Shrek.mkv" {
		t.Errorf("target_file_path = %q", item.TargetFilePath)
	}
	// The API passes the store projection through unchanged, so compare the
	// timestamps against the store's own read rather than a literal: the
	// SQLite driver may hand DATETIME columns back formatted.
	page, err := h.db.ListLibrary(context.Background(), store.ListLibraryFilter{})
	if err != nil {
		t.Fatalf("ListLibrary: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("store library rows = %d, want 1", len(page.Items))
	}
	if item.LastSeenAt == "" || item.UpdatedAt == "" {
		t.Errorf("last_seen_at/updated_at = %q/%q, want both stamped",
			item.LastSeenAt, item.UpdatedAt)
	}
	if item.LastSeenAt != page.Items[0].LastSeenAt || item.UpdatedAt != page.Items[0].UpdatedAt {
		t.Errorf("api timestamps %q/%q differ from the store's %q/%q",
			item.LastSeenAt, item.UpdatedAt, page.Items[0].LastSeenAt, page.Items[0].UpdatedAt)
	}
}

// TestLibraryFiltersRejectUnknown covers the values the read surface refuses:
// both must be a 400, not an empty page.
func TestLibraryFiltersRejectUnknown(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/api/library/items?kind=plex",
		"/api/library/items?eligibility=maybe",
	} {
		rec := h.get(path)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400 (body %s)", path, rec.Code, rec.Body.String())
		}
	}
}

// TestLibraryLimitOffsetClamping checks the paging contract: the store decides
// the applied page and the response echoes it.
func TestLibraryLimitOffsetClamping(t *testing.T) {
	h := newHarness(t)
	instanceID := h.insertInstance(t, "main", "radarr", true)
	h.insertMovie(t, instanceID, "Shrek", 2001)

	decode := func(t *testing.T, path string) (limit, offset int) {
		t.Helper()
		rec := h.get(path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, body %s", path, rec.Code, rec.Body.String())
		}
		var body struct {
			Limit  int `json:"limit"`
			Offset int `json:"offset"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return body.Limit, body.Offset
	}

	if limit, _ := decode(t, "/api/library/items?limit=0"); limit != store.LibraryPageDefaultLimit {
		t.Errorf("limit=0 -> %d, want default %d", limit, store.LibraryPageDefaultLimit)
	}
	if limit, _ := decode(t, "/api/library/items?limit=9999"); limit != store.LibraryPageMaxLimit {
		t.Errorf("limit=9999 -> %d, want cap %d", limit, store.LibraryPageMaxLimit)
	}
	if _, offset := decode(t, "/api/library/items?offset=-5"); offset != 0 {
		t.Errorf("offset=-5 -> %d, want 0", offset)
	}
	for _, path := range []string{
		"/api/library/items?limit=abc",
		"/api/library/items?offset=abc",
	} {
		if rec := h.get(path); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400 (body %s)", path, rec.Code, rec.Body.String())
		}
	}
}

// TestLibraryRoutesRequireAdmin proves a missing admin wrapper answers 503 on
// every route instead of serving the library unauthenticated.
func TestLibraryRoutesRequireAdmin(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	mux := http.NewServeMux()
	RegisterRoutes(mux, Dependencies{DB: st.DB})
	for _, path := range []string{"/api/library/items", "/api/library/health"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s with nil RequireAdmin = %d, want 503 (body %s)",
				path, rec.Code, rec.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("GET %s body is not JSON: %v", path, err)
			continue
		}
		if _, ok := body["error"]; !ok {
			t.Errorf("GET %s body %s has no error key", path, rec.Body.String())
		}
	}
}

// TestLibraryItemsEmptyReturnsZeroTotal pins the empty library shape: an empty
// array (never null) and a zero total.
func TestLibraryItemsEmptyReturnsZeroTotal(t *testing.T) {
	h := newHarness(t)
	rec := h.get("/api/library/items")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/library/items = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items  []json.RawMessage `json:"items"`
		Total  int64             `json:"total"`
		Limit  int               `json:"limit"`
		Offset int               `json:"offset"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Items == nil {
		t.Fatal("items is JSON null, want []")
	}
	if len(body.Items) != 0 || body.Total != 0 {
		t.Errorf("items/total = %d/%d, want 0/0", len(body.Items), body.Total)
	}
	if body.Limit != store.LibraryPageDefaultLimit || body.Offset != 0 {
		t.Errorf("limit/offset = %d/%d, want %d/0", body.Limit, body.Offset, store.LibraryPageDefaultLimit)
	}
}

// TestLibraryItemsInstanceIDFilter proves instance_id reaches the store from
// this route: two instances with one item each, and only the asked-for
// instance's item comes back (with the matching total).
func TestLibraryItemsInstanceIDFilter(t *testing.T) {
	h := newHarness(t)
	first := h.insertInstance(t, "first", "radarr", true)
	second := h.insertInstance(t, "second", "sonarr", true)
	h.insertMovie(t, first, "Shrek", 2001)
	h.insertMovie(t, second, "Zulu", 1964)

	all := h.items(t, "/api/library/items")
	if all.Total != 2 || len(all.Items) != 2 {
		t.Fatalf("unfiltered items/total = %d/%d, want 2/2", len(all.Items), all.Total)
	}

	body := h.items(t, fmt.Sprintf("/api/library/items?instance_id=%d", first))
	if len(body.Items) != 1 || body.Total != 1 {
		t.Fatalf("instance_id=%d items/total = %d/%d, want 1/1",
			first, len(body.Items), body.Total)
	}
	if body.Items[0].InstanceID != first || body.Items[0].Title != "Shrek" {
		t.Errorf("instance_id=%d returned %+v, want Shrek on instance %d",
			first, body.Items[0], first)
	}

	other := h.items(t, fmt.Sprintf("/api/library/items?instance_id=%d", second))
	if len(other.Items) != 1 || other.Total != 1 ||
		other.Items[0].InstanceID != second || other.Items[0].Title != "Zulu" {
		t.Fatalf("instance_id=%d items/total = %d/%d (%+v), want 1/1 Zulu",
			second, len(other.Items), other.Total, other.Items)
	}
}

// TestLibraryItemsQueryFilter proves q reaches the store from this route: it
// must narrow two rows down to the one whose title matches.
func TestLibraryItemsQueryFilter(t *testing.T) {
	h := newHarness(t)
	first := h.insertInstance(t, "first", "radarr", true)
	second := h.insertInstance(t, "second", "sonarr", true)
	h.insertMovie(t, first, "Shrek", 2001)
	h.insertMovie(t, second, "Zulu", 1964)

	body := h.items(t, "/api/library/items?q=shr")
	if len(body.Items) != 1 || body.Total != 1 {
		t.Fatalf("q=shr items/total = %d/%d, want 1/1 (%+v)",
			len(body.Items), body.Total, body.Items)
	}
	if body.Items[0].Title != "Shrek" {
		t.Errorf("q=shr returned %q, want Shrek", body.Items[0].Title)
	}
}

// TestLibraryInvalidInstanceIDRejected pins that a non-instance id is a 400
// rather than a silently dropped filter (the store reads <= 0 as "every
// instance", which would hand a typo the whole library).
func TestLibraryInvalidInstanceIDRejected(t *testing.T) {
	h := newHarness(t)
	instanceID := h.insertInstance(t, "main", "radarr", true)
	h.insertMovie(t, instanceID, "Shrek", 2001)

	for _, path := range []string{
		"/api/library/items?instance_id=-1",
		"/api/library/items?instance_id=0",
		"/api/library/items?instance_id=abc",
	} {
		rec := h.get(path)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400 (body %s)", path, rec.Code, rec.Body.String())
		}
	}
}

// TestLibraryGatingRejectsBeforeWork proves the admin wrapper runs before any
// handler work: the DB is closed, so a handler that reached the store would
// answer 500, and the body must carry only the rejection (the handlers' own
// keys — items/instances — must be absent).
func TestLibraryGatingRejectsBeforeWork(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	const rejection = `{"error":"unauthorized"}`
	reject := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(rejection))
		})
	}

	mux := http.NewServeMux()
	RegisterRoutes(mux, Dependencies{DB: st.DB, RequireAdmin: reject})
	for _, path := range []string{"/api/library/items", "/api/library/health"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s behind a rejecting wrapper = %d, want 401 (body %s)",
				path, rec.Code, rec.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s body is not JSON: %v", path, err)
		}
		if _, ok := body["error"]; !ok || len(body) != 1 {
			t.Fatalf("GET %s body %s = %v, want only the rejection: the handler ran",
				path, rec.Body.String(), sortedKeys(body))
		}
	}
}
