package library

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/arrsync"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// fakeRefresher records what the route asked for and replays one canned error.
type fakeRefresher struct {
	err   error
	kinds []string
}

func (f *fakeRefresher) Trigger(_ context.Context, kind string) error {
	f.kinds = append(f.kinds, kind)
	return f.err
}

// disabledRefresher is a poller that exists while every arr instance is
// disabled: it reports no enabled instance to sync.
type disabledRefresher struct {
	fakeRefresher
}

func (f *disabledRefresher) HasEnabledInstance(context.Context) bool { return false }

// newRefreshHarness mounts the library routes with a refresher (nil means "no
// poller was built"), so the refresh contract is tested through the real mux.
func newRefreshHarness(t *testing.T, refresher Refresher) *harness {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	settingsStore := settings.New(st.DB)
	mux := http.NewServeMux()
	RegisterRoutes(mux, Dependencies{
		DB:        st.DB,
		Settings:  settingsStore,
		Refresher: refresher,
		RequireAdmin: func(h http.Handler) http.Handler {
			return h
		},
	})
	return &harness{t: t, mux: mux, db: st, settings: settingsStore}
}

// post sends a POST with the given JSON body ("" = no body).
func (h *harness) post(path, body string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// refreshError decodes the {"error", "code"} conflict payload.
type refreshError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func decodeRefreshError(t *testing.T, rec *httptest.ResponseRecorder) refreshError {
	t.Helper()
	var body refreshError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode refresh error %q: %v", rec.Body.String(), err)
	}
	return body
}

// TestLibraryRefreshAccepted pins the happy path: 202 with the accepted kind,
// and the refresher receives exactly that kind.
func TestLibraryRefreshAccepted(t *testing.T) {
	refresher := &fakeRefresher{}
	h := newRefreshHarness(t, refresher)

	rec := h.post("/api/library/refresh", `{"kind":"radarr"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /api/library/refresh = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Accepted bool   `json:"accepted"`
		Kind     string `json:"kind"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !body.Accepted || body.Kind != "radarr" {
		t.Errorf("body = %+v, want accepted=true kind=radarr", body)
	}
	if len(refresher.kinds) != 1 || refresher.kinds[0] != "radarr" {
		t.Errorf("Trigger kinds = %v, want [radarr]", refresher.kinds)
	}

	// An empty body (and an empty kind) means every instance, reported as all.
	rec = h.post("/api/library/refresh", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST with no body = %d, body %s", rec.Code, rec.Body.String())
	}
	body = struct {
		Accepted bool   `json:"accepted"`
		Kind     string `json:"kind"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !body.Accepted || body.Kind != "all" {
		t.Errorf("body = %+v, want accepted=true kind=all", body)
	}
	if got := refresher.kinds[len(refresher.kinds)-1]; got != "" {
		t.Errorf("Trigger kind = %q, want \"\" for all instances", got)
	}
}

// TestLibraryRefreshBusyIsConflict pins the shared-owner conflict.
func TestLibraryRefreshBusyIsConflict(t *testing.T) {
	h := newRefreshHarness(t, &fakeRefresher{err: arrsync.ErrSyncBusy})

	rec := h.post("/api/library/refresh", `{"kind":"sonarr"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeRefreshError(t, rec)
	if body.Code != "busy" {
		t.Errorf("code = %q, want busy", body.Code)
	}
	if body.Error != "a sync pass is already running" {
		t.Errorf("error = %q, want the busy message", body.Error)
	}
}

// TestLibraryRefreshDisabledIsConflict pins arr.options.manual_sync = false.
func TestLibraryRefreshDisabledIsConflict(t *testing.T) {
	h := newRefreshHarness(t, &fakeRefresher{err: arrsync.ErrManualSyncDisabled})

	rec := h.post("/api/library/refresh", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeRefreshError(t, rec)
	if body.Code != "manual_sync_disabled" {
		t.Errorf("code = %q, want manual_sync_disabled", body.Code)
	}
	if body.Error != "manual sync is disabled" {
		t.Errorf("error = %q, want the disabled message", body.Error)
	}
}

// TestLibraryRefreshNotConfigured pins the no-poller case: a conflict, not a
// panic and not a 500.
func TestLibraryRefreshNotConfigured(t *testing.T) {
	h := newRefreshHarness(t, nil)

	rec := h.post("/api/library/refresh", `{"kind":"radarr"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeRefreshError(t, rec)
	if body.Code != "not_configured" {
		t.Errorf("code = %q, want not_configured", body.Code)
	}
	if body.Error != "no arr instance is configured yet" {
		t.Errorf("error = %q, want the not-configured message", body.Error)
	}
}

// TestLibraryRefreshNoEnabledInstance pins the "poller exists but nothing is
// enabled" conflict: it must name the missing configuration, not repeat the
// fresh-install message, and it must not reach the poller.
func TestLibraryRefreshNoEnabledInstance(t *testing.T) {
	refresher := &disabledRefresher{}
	h := newRefreshHarness(t, refresher)

	rec := h.post("/api/library/refresh", `{"kind":"radarr"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeRefreshError(t, rec)
	if body.Code != "not_configured" {
		t.Errorf("code = %q, want not_configured", body.Code)
	}
	if body.Error == "no arr instance is configured yet" {
		t.Error("error repeats the unconfigured-install message")
	}
	if !strings.Contains(body.Error, "no enabled arr instance") {
		t.Errorf("error = %q, want it to name the missing arr instance", body.Error)
	}
	if len(refresher.kinds) != 0 {
		t.Errorf("Trigger calls = %v, want none while no instance is enabled", refresher.kinds)
	}
}

// TestLibraryRefreshRejectsUnknownKind pins 400 for an unknown kind and for a
// malformed body, and that neither reaches the poller.
func TestLibraryRefreshRejectsUnknownKind(t *testing.T) {
	refresher := &fakeRefresher{}
	h := newRefreshHarness(t, refresher)

	for _, body := range []string{`{"kind":"lidarr"}`, `{"kind":`} {
		rec := h.post("/api/library/refresh", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("POST %s = %d, want 400 (body %s)", body, rec.Code, rec.Body.String())
		}
	}
	if len(refresher.kinds) != 0 {
		t.Errorf("Trigger calls = %v, want none for a rejected body", refresher.kinds)
	}
}

// TestLibraryRefreshMalformedBodyBeforeNotConfigured pins the route order: the
// body is validated before the no-poller check, so a malformed request on an
// instance-less install is 400, not 409.
func TestLibraryRefreshMalformedBodyBeforeNotConfigured(t *testing.T) {
	h := newRefreshHarness(t, nil)

	rec := h.post("/api/library/refresh", `{"kind":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a malformed body (body %s)", rec.Code, rec.Body.String())
	}

	// A valid body still reports the missing configuration as a conflict.
	rec = h.post("/api/library/refresh", `{"kind":"radarr"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a valid body with no poller (body %s)", rec.Code, rec.Body.String())
	}
	if body := decodeRefreshError(t, rec); body.Code != "not_configured" {
		t.Errorf("code = %q, want not_configured", body.Code)
	}
}

// TestLibraryRefreshStoppedIsConflict pins the shutdown mapping: the poller's
// stopped sentinel is a conflict the UI can name, never a 500.
func TestLibraryRefreshStoppedIsConflict(t *testing.T) {
	h := newRefreshHarness(t, &fakeRefresher{err: arrsync.ErrPollerStopped})

	rec := h.post("/api/library/refresh", `{"kind":"radarr"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeRefreshError(t, rec)
	if body.Code != "stopped" {
		t.Errorf("code = %q, want stopped", body.Code)
	}
	if body.Error != "the sync poller is stopping" {
		t.Errorf("error = %q, want the stopping message", body.Error)
	}
}

// TestLibraryRefreshRequiresAdmin pins the 503 contract shared with the other
// library routes when no authorization wrapper is wired.
func TestLibraryRefreshRequiresAdmin(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	mux := http.NewServeMux()
	RegisterRoutes(mux, Dependencies{
		DB:        st.DB,
		Settings:  settings.New(st.DB),
		Refresher: &fakeRefresher{},
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/library/refresh", strings.NewReader("")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
}
