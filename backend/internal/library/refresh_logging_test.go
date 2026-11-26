package library

import (
	"bytes"
	"errors"
	"net/http"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/arrsync"
	"github.com/Subject412/dubarr/backend/internal/logs"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// newRingLogger builds the real logger over a throwaway stderr buffer, so these
// tests read the rotated ring exactly like internal/logs/logs_test.go does.
func newRingLogger(t *testing.T) *logs.Logger {
	t.Helper()
	var stderr bytes.Buffer
	lg, err := logs.New(logs.Options{Level: "info", Stderr: &stderr})
	if err != nil {
		t.Fatalf("logs.New: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	return lg
}

// newLoggingRefreshHarness is newRefreshHarness with the refresh log seam
// wired to the real logger, so a click's decision lands in the ring.
func newLoggingRefreshHarness(t *testing.T, refresher Refresher, lg *logs.Logger) *harness {
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
		Logf: lg.Logf("library"),
	})
	return &harness{t: t, mux: mux, db: st, settings: settingsStore}
}

// TestRefreshLogsOneInfoLinePerDecision pins the U1.6 refresh coverage: every
// POST /api/library/refresh writes exactly one info line under component
// "library" naming the requested kind and the decision it reached, and nothing
// below info. Four clicks, four lines — not one per branch and not zero.
func TestRefreshLogsOneInfoLinePerDecision(t *testing.T) {
	refresher := &fakeRefresher{}
	lg := newRingLogger(t)
	h := newLoggingRefreshHarness(t, refresher, lg)

	steps := []struct {
		name     string
		body     string
		status   int
		wantLine string
	}{
		{
			name:     "accepted",
			body:     `{"kind":"radarr"}`,
			status:   http.StatusAccepted,
			wantLine: "library: refresh accepted (kind=radarr): the sync pass runs in the background",
		},
		{
			name:     "busy",
			body:     `{"kind":"sonarr"}`,
			status:   http.StatusConflict,
			wantLine: "library: refresh skipped (kind=sonarr): a sync pass is already running",
		},
		{
			name:     "stopped",
			body:     ``,
			status:   http.StatusConflict,
			wantLine: "library: refresh refused (kind=all): the sync poller is stopping",
		},
		{
			name:     "failed",
			body:     `{"kind":"radarr"}`,
			status:   http.StatusInternalServerError,
			wantLine: "library: refresh failed (kind=radarr): arr is unreachable",
		},
	}
	// Each step replays one decision through the real refresher seam.
	for i, step := range steps {
		switch step.name {
		case "busy":
			refresher.err = arrsync.ErrSyncBusy
		case "stopped":
			refresher.err = arrsync.ErrPollerStopped
		case "failed":
			refresher.err = errors.New("arr is unreachable")
		}

		rec := h.post("/api/library/refresh", step.body)
		if rec.Code != step.status {
			t.Fatalf("%s: status = %d, want %d (body %s)", step.name, rec.Code, step.status, rec.Body.String())
		}

		entries := lg.Entries(logs.Query{Component: "library"})
		if len(entries) != i+1 {
			t.Fatalf("%s: %d library line(s) after %d click(s), want exactly one per click: %+v",
				step.name, len(entries), i+1, entries)
		}
		got := entries[i]
		if got.Level != "info" {
			t.Errorf("%s: level = %q, want info", step.name, got.Level)
		}
		if got.Message != step.wantLine {
			t.Errorf("%s: message = %q, want %q", step.name, got.Message, step.wantLine)
		}
	}

	// The refresh seam is info-only: no warning or error noise per click.
	if loud := lg.Entries(logs.Query{Component: "library", MinLevel: "warn"}); len(loud) != 0 {
		t.Errorf("%d library line(s) at warn or above, want 0: %+v", len(loud), loud)
	}
}
