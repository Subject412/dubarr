package library

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/settings"
)

// setArrConnections writes the arr.connections section, i.e. exactly what
// PUT /api/settings/arr/{kind} leaves behind when it saves a connection.
func (h *harness) setArrConnections(t *testing.T, conns ...settings.ArrConnection) {
	t.Helper()
	raw, err := json.Marshal(settings.ArrConnections{Instances: conns})
	if err != nil {
		t.Fatalf("marshal arr connections: %v", err)
	}
	if err := h.settings.Set(context.Background(), settings.KeyArrConnections, raw); err != nil {
		t.Fatalf("write arr connections: %v", err)
	}
}

// markSyncOK records a successful pass on the instance, exactly as
// store.TouchArrInstanceSync does.
func (h *harness) markSyncOK(t *testing.T, id int64, ts string) {
	t.Helper()
	if _, err := h.db.DB.ExecContext(context.Background(),
		`UPDATE arr_instances
		 SET last_sync_at = ?, last_sync_ok = 1, last_sync_error = NULL, last_attempt_at = ?
		 WHERE id = ?`, ts, ts, id); err != nil {
		t.Fatalf("mark sync ok: %v", err)
	}
}

// markSyncFailed records a failed pass without touching last_sync_at, exactly
// as store.RecordArrInstanceSyncFailure does.
func (h *harness) markSyncFailed(t *testing.T, id int64, ts, cause string) {
	t.Helper()
	if _, err := h.db.DB.ExecContext(context.Background(),
		`UPDATE arr_instances SET last_sync_ok = 0, last_sync_error = ?, last_attempt_at = ?
		 WHERE id = ?`, cause, ts, id); err != nil {
		t.Fatalf("mark sync failed: %v", err)
	}
}

type healthPayload struct {
	Instances []struct {
		Kind          string  `json:"kind"`
		Name          string  `json:"name"`
		Enabled       bool    `json:"enabled"`
		LastSyncAt    *string `json:"last_sync_at"`
		LastAttemptAt *string `json:"last_attempt_at"`
		LastOK        *bool   `json:"last_ok"`
		LastError     string  `json:"last_error"`
		ItemsSeen     int64   `json:"items_seen"`
	} `json:"instances"`
	CacheItems int64 `json:"cache_items"`
	// CacheMovies and CacheSeries are the library's size in films and shows;
	// CacheItems is every cache row (episodes included) and stays for the
	// empty-state and queue diagnostics.
	CacheMovies int64 `json:"cache_movies"`
	CacheSeries int64 `json:"cache_series"`
}

// healthRaw decodes the health payload and also returns the raw body so a test
// can assert on the JSON bytes themselves.
func (h *harness) healthRaw(t *testing.T) (healthPayload, []byte) {
	t.Helper()
	rec := h.get("/api/library/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/library/health = %d, body %s", rec.Code, rec.Body.String())
	}
	var body healthPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	return body, rec.Body.Bytes()
}

func (h *harness) health(t *testing.T) healthPayload {
	t.Helper()
	body, _ := h.healthRaw(t)
	return body
}

// TestLibraryHealthStates covers the tri-state last_ok: never attempted (null),
// failed (false + cause) and succeeded (true), and the kind/name ordering.
func TestLibraryHealthStates(t *testing.T) {
	h := newHarness(t)
	neverID := h.insertInstance(t, "alpha", "radarr", true)
	failedID := h.insertInstance(t, "beta", "radarr", false)
	okID := h.insertInstance(t, "gamma", "sonarr", true)

	h.markSyncFailed(t, failedID, "2026-01-02 03:04:05", "radarr returned 401")
	h.markSyncOK(t, okID, "2026-01-03 04:05:06")
	if neverID == failedID || failedID == okID {
		t.Fatal("seeded instance ids are not distinct")
	}

	body, raw := h.healthRaw(t)
	if len(body.Instances) != 3 {
		t.Fatalf("instances = %d, want 3 (%+v)", len(body.Instances), body.Instances)
	}
	wantOrder := []string{"alpha", "beta", "gamma"}
	for i, want := range wantOrder {
		if body.Instances[i].Name != want {
			t.Fatalf("instances[%d] = %q, want %q (order by kind then name)",
				i, body.Instances[i].Name, want)
		}
	}

	never := body.Instances[0]
	if never.LastOK != nil {
		t.Errorf("never-synced last_ok = %v, want null", *never.LastOK)
	}
	if never.LastSyncAt != nil || never.LastAttemptAt != nil {
		t.Errorf("never-synced timestamps = %v/%v, want both nil (never happened)",
			never.LastSyncAt, never.LastAttemptAt)
	}
	if never.LastError != "" {
		t.Errorf("never-synced last_error = %q, want empty", never.LastError)
	}
	// The keys must still be present and carry null: an omitted key would make
	// the field indistinguishable from an older payload shape.
	var rawBody struct {
		Instances []map[string]json.RawMessage `json:"instances"`
	}
	if err := json.Unmarshal(raw, &rawBody); err != nil {
		t.Fatalf("decode raw health: %v", err)
	}
	if len(rawBody.Instances) != 3 {
		t.Fatalf("raw instances = %d, want 3", len(rawBody.Instances))
	}
	for _, key := range []string{"last_sync_at", "last_attempt_at"} {
		value, ok := rawBody.Instances[0][key]
		if !ok {
			t.Fatalf("never-synced raw JSON has no %s key: %s", key, raw)
		}
		if string(value) != "null" {
			t.Errorf("never-synced raw %s = %s, want null (not \"\")", key, value)
		}
	}

	failed := body.Instances[1]
	if failed.LastOK == nil || *failed.LastOK {
		t.Errorf("failed last_ok = %v, want false", failed.LastOK)
	}
	if failed.LastError != "radarr returned 401" {
		t.Errorf("failed last_error = %q, want the cause", failed.LastError)
	}
	// The driver may hand DATETIME columns back formatted, so only presence is
	// asserted; LastSyncAt must stay null because the pass never succeeded.
	if failed.LastAttemptAt == nil || *failed.LastAttemptAt == "" {
		t.Errorf("failed last_attempt_at = %v, want the attempt stamp", failed.LastAttemptAt)
	}
	if failed.LastSyncAt != nil {
		t.Errorf("failed last_sync_at = %v, want nil (never succeeded)", *failed.LastSyncAt)
	}
	if failed.Enabled {
		t.Error("failed instance enabled = true, want false")
	}

	ok := body.Instances[2]
	if ok.LastOK == nil || !*ok.LastOK {
		t.Errorf("succeeded last_ok = %v, want true", ok.LastOK)
	}
	if ok.LastSyncAt == nil || *ok.LastSyncAt == "" ||
		ok.LastAttemptAt == nil || *ok.LastAttemptAt == "" {
		t.Errorf("succeeded timestamps = %v/%v, want both stamped",
			ok.LastSyncAt, ok.LastAttemptAt)
	}
	if ok.LastError != "" {
		t.Errorf("succeeded last_error = %q, want empty", ok.LastError)
	}
	if !ok.Enabled {
		t.Error("succeeded instance enabled = false, want true")
	}
}

// TestLibraryHealthNoInstances pins the fresh-install shape: 200, an empty
// array (never null) and a zero cache count.
func TestLibraryHealthNoInstances(t *testing.T) {
	h := newHarness(t)
	rec := h.get("/api/library/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/library/health = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Instances  []json.RawMessage `json:"instances"`
		CacheItems int64             `json:"cache_items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if body.Instances == nil {
		t.Fatal("instances is JSON null, want []")
	}
	if len(body.Instances) != 0 || body.CacheItems != 0 {
		t.Errorf("instances/cache_items = %d/%d, want 0/0", len(body.Instances), body.CacheItems)
	}
}

// TestLibraryHealthCountsItemsSeen checks the per-instance cache counts and
// their total.
func TestLibraryHealthCountsItemsSeen(t *testing.T) {
	h := newHarness(t)
	moviesID := h.insertInstance(t, "movies", "radarr", true)
	tvID := h.insertInstance(t, "tv", "sonarr", true)

	h.insertCacheRow(t, moviesID, 1)
	h.insertCacheRow(t, moviesID, 2)
	h.insertCacheRow(t, tvID, 3)
	h.insertCacheRow(t, tvID, 4)
	h.insertCacheRow(t, tvID, 5)

	body := h.health(t)
	if len(body.Instances) != 2 {
		t.Fatalf("instances = %d, want 2", len(body.Instances))
	}
	// kind order: radarr ("movies") before sonarr ("tv").
	if body.Instances[0].Name != "movies" || body.Instances[0].ItemsSeen != 2 {
		t.Errorf("movies items_seen = %d (%s), want 2", body.Instances[0].ItemsSeen, body.Instances[0].Name)
	}
	if body.Instances[1].Name != "tv" || body.Instances[1].ItemsSeen != 3 {
		t.Errorf("tv items_seen = %d (%s), want 3", body.Instances[1].ItemsSeen, body.Instances[1].Name)
	}
	if body.CacheItems != 5 {
		t.Errorf("cache_items = %d, want 5", body.CacheItems)
	}
}

// TestHealthReportsPerMediaCounts pins the header's two numbers. cache_items
// stays the whole-table row total (the empty state and the queue diagnostics
// read it), while cache_movies/cache_series say how big the library is in the
// terms the operator thinks in: 2 films and 2 shows, even though those shows
// are 6 episode rows.
func TestHealthReportsPerMediaCounts(t *testing.T) {
	h := newHarness(t)
	moviesID := h.insertInstance(t, "movies", "radarr", true)
	tvID := h.insertInstance(t, "tv", "sonarr", true)

	h.insertCacheRow(t, moviesID, 1)
	h.insertCacheRow(t, moviesID, 2)

	// One show is many episode rows: three rows for series 100, three for 200.
	for _, episodeID := range []int64{11, 12, 13} {
		h.insertEpisodeCacheRow(t, tvID, episodeID, 100)
	}
	for _, episodeID := range []int64{21, 22, 23} {
		h.insertEpisodeCacheRow(t, tvID, episodeID, 200)
	}

	body := h.health(t)
	if body.CacheItems != 8 {
		t.Errorf("cache_items = %d, want 8 (every cache row)", body.CacheItems)
	}
	if body.CacheMovies != 2 {
		t.Errorf("cache_movies = %d, want 2", body.CacheMovies)
	}
	if body.CacheSeries != 2 {
		t.Errorf("cache_series = %d, want 2 (distinct series_id, not episode rows)", body.CacheSeries)
	}
}

// TestLibraryHealthIncludesConfiguredButUnsynced covers the fix: a connection
// saved in arr.connections with no arr_instances row yet (nothing polled) must
// still be reported, as the never-attempted state.
func TestLibraryHealthIncludesConfiguredButUnsynced(t *testing.T) {
	h := newHarness(t)
	h.setArrConnections(t, settings.ArrConnection{
		Kind: "radarr", BaseURL: "http://radarr.invalid", Enabled: false,
	})

	body := h.health(t)
	if len(body.Instances) != 1 {
		t.Fatalf("instances = %d, want 1 (%+v)", len(body.Instances), body.Instances)
	}
	inst := body.Instances[0]
	if inst.Kind != "radarr" {
		t.Errorf("kind = %q, want radarr", inst.Kind)
	}
	// The name is the kind's display name, matching what the first poll will
	// write into arr_instances.
	if inst.Name != "Radarr" {
		t.Errorf("name = %q, want Radarr", inst.Name)
	}
	if inst.Enabled {
		t.Error("enabled = true, want the connection's false")
	}
	if inst.LastOK != nil {
		t.Errorf("last_ok = %v, want null (never attempted)", *inst.LastOK)
	}
	if inst.LastSyncAt != nil || inst.LastAttemptAt != nil {
		t.Errorf("timestamps = %v/%v, want both null", inst.LastSyncAt, inst.LastAttemptAt)
	}
	if inst.LastError != "" {
		t.Errorf("last_error = %q, want empty", inst.LastError)
	}
	if inst.ItemsSeen != 0 {
		t.Errorf("items_seen = %d, want 0", inst.ItemsSeen)
	}
}

// TestLibraryHealthMergesWithoutDuplicating pins that a kind present in both
// sources appears once, and that the store row wins.
func TestLibraryHealthMergesWithoutDuplicating(t *testing.T) {
	h := newHarness(t)
	id := h.insertInstance(t, "main", "radarr", true)
	h.markSyncOK(t, id, "2026-01-03 04:05:06")
	h.insertCacheRow(t, id, 42)
	h.setArrConnections(t,
		settings.ArrConnection{Kind: "radarr", BaseURL: "http://radarr.invalid", Enabled: true},
		settings.ArrConnection{Kind: "sonarr", BaseURL: "http://sonarr.invalid", Enabled: true},
	)

	body := h.health(t)
	if len(body.Instances) != 2 {
		t.Fatalf("instances = %d, want 2 (%+v)", len(body.Instances), body.Instances)
	}
	// Ordering stays kind then name: radarr (store row) before sonarr.
	merged := body.Instances[0]
	if merged.Kind != "radarr" || merged.Name != "main" {
		t.Fatalf("radarr entry = %s/%q, want the store row (radarr/main)", merged.Kind, merged.Name)
	}
	if merged.LastOK == nil || !*merged.LastOK {
		t.Errorf("radarr last_ok = %v, want the store's true", merged.LastOK)
	}
	if merged.ItemsSeen != 1 {
		t.Errorf("radarr items_seen = %d, want the store's 1", merged.ItemsSeen)
	}
	if body.Instances[1].Kind != "sonarr" || body.Instances[1].LastOK != nil {
		t.Errorf("sonarr entry = %+v, want the unsynced configured row", body.Instances[1])
	}
	if body.CacheItems != 1 {
		t.Errorf("cache_items = %d, want 1", body.CacheItems)
	}
}

// TestLibraryHealthToleratesSettingsError proves a broken settings read cannot
// take health down: the store-derived instances are still served with a 200.
func TestLibraryHealthToleratesSettingsError(t *testing.T) {
	h := newHarness(t)
	id := h.insertInstance(t, "main", "radarr", true)
	h.markSyncOK(t, id, "2026-01-03 04:05:06")
	h.setArrConnections(t, settings.ArrConnection{
		Kind: "sonarr", BaseURL: "http://sonarr.invalid", Enabled: true,
	})

	// Drop the section table: settings.Get fails while arr_instances still reads.
	if _, err := h.db.DB.ExecContext(context.Background(), `DROP TABLE settings`); err != nil {
		t.Fatalf("drop settings table: %v", err)
	}

	body := h.health(t) // fails on any non-200
	if len(body.Instances) != 1 {
		t.Fatalf("instances = %d, want the store's 1 (%+v)", len(body.Instances), body.Instances)
	}
	if body.Instances[0].Name != "main" || body.Instances[0].Kind != "radarr" {
		t.Errorf("instance = %s/%q, want radarr/main", body.Instances[0].Kind, body.Instances[0].Name)
	}
	if body.Instances[0].LastOK == nil || !*body.Instances[0].LastOK {
		t.Errorf("last_ok = %v, want true from the store", body.Instances[0].LastOK)
	}
}
