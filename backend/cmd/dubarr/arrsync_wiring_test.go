package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arr/arrmock"
	"github.com/Subject412/dubarr/backend/internal/arrsync"
	"github.com/Subject412/dubarr/backend/internal/secrets"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// arrSyncRig is one appliance data dir with settings and secrets for the M6-1
// wiring tests.
type arrSyncRig struct {
	t        *testing.T
	store    *store.Store
	settings *settings.Store
	secrets  *secrets.Service
}

func newArrSyncRig(t *testing.T) *arrSyncRig {
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
	sec, err := secrets.Open(st, secrets.Options{DataDir: dir})
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	return &arrSyncRig{t: t, store: st, settings: settingsStore, secrets: sec}
}

func (r *arrSyncRig) wire(interval time.Duration) *arrsync.Poller {
	r.t.Helper()
	p, err := wireArrSync(context.Background(), arrSyncWiring{
		Settings: r.settings,
		Store:    r.store,
		Secrets:  r.secrets,
		Interval: interval,
		Logf:     r.t.Logf,
	})
	if err != nil {
		r.t.Fatalf("wireArrSync: %v", err)
	}
	return p
}

func (r *arrSyncRig) setConnections(conns ...settings.ArrConnection) {
	r.t.Helper()
	raw, err := json.Marshal(settings.ArrConnections{Instances: conns})
	if err != nil {
		r.t.Fatalf("marshal connections: %v", err)
	}
	if err := r.settings.Set(context.Background(), settings.KeyArrConnections, raw); err != nil {
		r.t.Fatalf("set connections: %v", err)
	}
}

func (r *arrSyncRig) setKey(kind, apiKey string) {
	r.t.Helper()
	if err := r.secrets.Set("arr."+kind+".api_key", apiKey); err != nil {
		r.t.Fatalf("set %s key: %v", kind, err)
	}
}

// TestWireArrSyncDisabledWithoutInstances proves a fresh install (no arr
// connections) starts no poller: the wizard still works.
func TestWireArrSyncDisabledWithoutInstances(t *testing.T) {
	r := newArrSyncRig(t)
	if p := r.wire(0); p != nil {
		t.Fatal("wireArrSync started a poller with no configured instances")
	}
}

// TestWireArrSyncDisabledByNonPositiveInterval pins the documented disable
// switch: interval <= 0 means "do not poll".
func TestWireArrSyncDisabledByNonPositiveInterval(t *testing.T) {
	r := newArrSyncRig(t)
	r.setConnections(settings.ArrConnection{Kind: "radarr", BaseURL: "http://radarr.invalid", Enabled: true})
	if p := r.wire(-1); p != nil {
		t.Fatal("wireArrSync started a poller with a non-positive interval")
	}
}

// TestWireArrSyncReadsSettingsInterval proves arr.options.poll_interval_sec is
// the cadence source (settled default 300 s).
func TestWireArrSyncReadsSettingsInterval(t *testing.T) {
	r := newArrSyncRig(t)
	r.setConnections(settings.ArrConnection{Kind: "radarr", BaseURL: "http://radarr.invalid", Enabled: true})

	p := r.wire(0)
	if p == nil {
		t.Fatal("wireArrSync returned no poller for an enabled instance")
	}
	if got := p.Interval(); got != 300*time.Second {
		t.Errorf("interval = %v, want the seeded 300s", got)
	}

	raw, err := json.Marshal(settings.ArrOptions{PollIntervalSec: 90})
	if err != nil {
		t.Fatalf("marshal arr options: %v", err)
	}
	if err := r.settings.Set(context.Background(), settings.KeyArrOptions, raw); err != nil {
		t.Fatalf("set arr options: %v", err)
	}
	if got := r.wire(0).Interval(); got != 90*time.Second {
		t.Errorf("interval = %v, want 90s from arr.options", got)
	}
}

// TestWireArrSyncPopulatesCache is the M6-1 boot path: the wired poller fills
// arr_library_cache from the mocked Radarr and Sonarr instances.
func TestWireArrSyncPopulatesCache(t *testing.T) {
	r := newArrSyncRig(t)
	radarr := arrmock.New(arrmock.Radarr, "radarr-key", arrmock.Fixtures{
		Movies: []arrmock.Movie{{
			ID: 7, Title: "Shrek", Year: 2001, TmdbID: 808, HasFile: true,
			Path:             "/media/movies/Shrek (2001)",
			OriginalLanguage: arrmock.Language{ID: 1, Name: "English"},
			MovieFile:        &arrmock.MovieFile{Path: "/media/movies/Shrek (2001)/Shrek (2001) [ArcA310].mkv"},
		}},
	})
	defer radarr.Close()
	sonarr := arrmock.New(arrmock.Sonarr, "sonarr-key", arrmock.Fixtures{
		Series: []arrmock.Series{{
			ID: 3, Title: "The Expanse", Year: 2015, TvdbID: 280619,
			Path: "/media/tv/The Expanse", OriginalLanguage: arrmock.Language{ID: 1, Name: "English"},
		}},
		Episodes: []arrmock.Episode{{
			ID: 42, SeriesID: 3, SeasonNumber: 1, EpisodeNumber: 1, Title: "Dulcinea", HasFile: true,
			EpisodeFile: &arrmock.EpisodeFile{Path: "/media/tv/The Expanse/S01E01.mkv"},
		}},
	})
	defer sonarr.Close()

	r.setConnections(
		settings.ArrConnection{Kind: "radarr", BaseURL: radarr.URL, Enabled: true},
		settings.ArrConnection{Kind: "sonarr", BaseURL: sonarr.URL, Enabled: true},
	)
	r.setKey("radarr", radarr.APIKey())
	r.setKey("sonarr", sonarr.APIKey())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := r.wire(20 * time.Millisecond)
	if p == nil {
		t.Fatal("wireArrSync returned no poller")
	}
	p.Start(ctx)
	defer p.Stop()

	deadline := time.Now().Add(testPollDeadline)
	for time.Now().Before(deadline) {
		var count int
		if err := r.store.DB.QueryRow(`SELECT COUNT(*) FROM arr_library_cache`).Scan(&count); err != nil {
			t.Fatalf("count cache: %v", err)
		}
		if count == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	var (
		movies, episodes int
	)
	if err := r.store.DB.QueryRow(`SELECT COUNT(*) FROM arr_library_cache WHERE media_type = 'movie'`).Scan(&movies); err != nil {
		t.Fatalf("count movies: %v", err)
	}
	if err := r.store.DB.QueryRow(`SELECT COUNT(*) FROM arr_library_cache WHERE media_type = 'episode'`).Scan(&episodes); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if movies != 1 || episodes != 1 {
		t.Fatalf("cache rows = %d movies / %d episodes, want 1/1", movies, episodes)
	}

	var eligibility string
	if err := r.store.DB.QueryRow(
		`SELECT eligibility_status FROM arr_library_cache WHERE media_type = 'movie'`).Scan(&eligibility); err != nil {
		t.Fatalf("read eligibility: %v", err)
	}
	if eligibility != "eligible" {
		t.Errorf("movie eligibility = %q, want eligible", eligibility)
	}
}
