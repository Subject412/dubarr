package arrsync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arr"
	"github.com/Subject412/dubarr/backend/internal/arr/arrmock"
	"github.com/Subject412/dubarr/backend/internal/secrets"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// testHTTPTimeout bounds one request to a fake arr instance in these tests. It
// is a hang guard, not a performance assertion: a loaded runner slowing the
// mock down is not a defect.
const testHTTPTimeout = 30 * time.Second

// testPassTimeout bounds waiting on a pass, a shutdown drain or a loop rebuild
// in these tests. Same rule as testHTTPTimeout: it is a hang guard, never a
// performance assertion, so it stays at least as long as the per-request budget
// a pass may legitimately spend before the waiter gives up.
const testPassTimeout = 30 * time.Second

// rig is one appliance data dir with settings, secrets and the real store.
type rig struct {
	t        *testing.T
	store    *store.Store
	settings *settings.Store
	secrets  *secrets.Service
}

func newRig(t *testing.T) *rig {
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
	return &rig{t: t, store: st, settings: settingsStore, secrets: sec}
}

// configure stores the arr.connections section.
func (r *rig) configure(conns ...settings.ArrConnection) {
	r.t.Helper()
	raw, err := json.Marshal(settings.ArrConnections{Instances: conns})
	if err != nil {
		r.t.Fatalf("marshal connections: %v", err)
	}
	if err := r.settings.Set(r.context(), settings.KeyArrConnections, raw); err != nil {
		r.t.Fatalf("set connections: %v", err)
	}
}

// context returns the test context; the rig is per-test and never cancelled.
func (r *rig) context() context.Context { return context.Background() }

// key stores the API key the secret store hands the poller for one kind.
func (r *rig) key(kind, apiKey string) {
	r.t.Helper()
	if err := r.secrets.Set("arr."+kind+".api_key", apiKey); err != nil {
		r.t.Fatalf("store secret %s: %v", kind, err)
	}
}

func (r *rig) newPoller(interval time.Duration) *Poller {
	r.t.Helper()
	p, err := New(Deps{
		Store:    r.store,
		Settings: r.settings,
		Secrets:  r.secrets,
		Interval: interval,
		Timeout:  testHTTPTimeout,
		Logf:     r.t.Logf,
	})
	if err != nil {
		r.t.Fatalf("New: %v", err)
	}
	return p
}

// radarrServer starts a fake Radarr holding one tagged and one fileless movie.
func radarrServer(t *testing.T) *arrmock.Server {
	t.Helper()
	srv := arrmock.New(arrmock.Radarr, "key-radarr", arrmock.Fixtures{
		Movies: []arrmock.Movie{
			{
				ID: 7, Title: "Shrek", Year: 2001, TmdbID: 808, HasFile: true,
				Path:             "/media/movies/Shrek (2001)",
				OriginalLanguage: arrmock.Language{ID: 1, Name: "English"},
				MovieFile: &arrmock.MovieFile{
					ID: 3, MovieID: 7,
					RelativePath: "Shrek (2001) [ArcA310].mkv",
					Path:         "/media/movies/Shrek (2001)/Shrek (2001) [ArcA310].mkv",
				},
			},
			{
				ID: 9, Title: "Minions", Year: 2015, TmdbID: 211672, HasFile: false,
				Path:             "/media/movies/Minions (2015)",
				OriginalLanguage: arrmock.Language{ID: 1, Name: "English"},
			},
		},
	})
	t.Cleanup(srv.Close)
	return srv
}

// sonarrServer starts a fake Sonarr holding one tagged episode, one untagged
// episode and one fileless episode.
func sonarrServer(t *testing.T) *arrmock.Server {
	t.Helper()
	srv := arrmock.New(arrmock.Sonarr, "key-sonarr", arrmock.Fixtures{
		Series: []arrmock.Series{
			{ID: 3, Title: "The Expanse", Year: 2015, TvdbID: 280619,
				Path: "/media/tv/The Expanse", OriginalLanguage: arrmock.Language{ID: 2, Name: "English"}},
		},
		Episodes: []arrmock.Episode{
			{
				ID: 42, SeriesID: 3, SeasonNumber: 1, EpisodeNumber: 1, Title: "Dulcinea", HasFile: true,
				EpisodeFile: &arrmock.EpisodeFile{
					ID: 7, SeriesID: 3, SeasonNumber: 1,
					Path: "/media/tv/The Expanse/Season 01/S01E01 [ArcA310].mkv",
				},
			},
			{
				ID: 43, SeriesID: 3, SeasonNumber: 1, EpisodeNumber: 2, Title: "The Big Empty", HasFile: true,
				EpisodeFile: &arrmock.EpisodeFile{
					ID: 8, SeriesID: 3, SeasonNumber: 1,
					Path: "/media/tv/The Expanse/Season 01/S01E02.mkv",
				},
			},
			{ID: 44, SeriesID: 3, SeasonNumber: 1, EpisodeNumber: 3, Title: "Remember the Cant"},
		},
	})
	t.Cleanup(srv.Close)
	return srv
}

// TestSyncCachesMoviesAndEpisodes is the M6-1 acceptance path: one pass caches
// Radarr movies and Sonarr episodes with resolved target paths and gate
// verdicts, and creates the arr_instances rows the cache points at.
func TestSyncCachesMoviesAndEpisodes(t *testing.T) {
	r := newRig(t)
	radarr := radarrServer(t)
	sonarr := sonarrServer(t)
	r.configure(
		settings.ArrConnection{Kind: "radarr", BaseURL: radarr.URL, Enabled: true},
		settings.ArrConnection{Kind: "sonarr", BaseURL: sonarr.URL, Enabled: true},
	)
	r.key("radarr", radarr.APIKey())
	r.key("sonarr", sonarr.APIKey())
	// ADR-0012 ships an empty default; this test asserts the filename criterion.
	r.tagFilter(settings.TargetTagFilter{
		RequiredFilenameTags: []string{"[ArcA310]"},
		TagMatchMode:         "ANY",
		EnforceTagFilter:     true,
	})

	p := r.newPoller(time.Minute)
	stats, err := p.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.Instances != 2 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want 2 instances", stats)
	}
	if stats.Inserted != 5 {
		t.Fatalf("inserted = %d, want 5 (2 movies + 3 episodes), stats %+v", stats.Inserted, stats)
	}

	rows := readCache(t, r.store)
	if len(rows) != 5 {
		t.Fatalf("cache rows = %d, want 5", len(rows))
	}

	shrek := rows["movie|7"]
	if shrek.MediaType != "movie" || shrek.SeriesID != 0 || shrek.Title != "Shrek" ||
		shrek.Year != 2001 || shrek.TMDBID != 808 || shrek.OriginalLanguageISO2 != "en" {
		t.Errorf("shrek row = %+v", shrek)
	}
	if shrek.TargetFilePath != "/media/movies/Shrek (2001)/Shrek (2001) [ArcA310].mkv" ||
		!shrek.MatchesTargetTags || shrek.EligibilityStatus != "eligible" {
		t.Errorf("shrek target/gate = %q %v %q", shrek.TargetFilePath, shrek.MatchesTargetTags, shrek.EligibilityStatus)
	}

	minions := rows["movie|9"]
	if minions.TargetFilePath != "" || minions.MatchesTargetTags || minions.EligibilityStatus != "missing_target" {
		t.Errorf("fileless movie = %+v", minions)
	}

	taggedEpisode := rows["episode|42"]
	if taggedEpisode.SeriesID != 3 || taggedEpisode.TVDBID != 280619 || taggedEpisode.SeasonNumber == nil ||
		*taggedEpisode.SeasonNumber != 1 || taggedEpisode.EpisodeNumber == nil || *taggedEpisode.EpisodeNumber != 1 ||
		taggedEpisode.EpisodeTitle != "Dulcinea" || taggedEpisode.Year != 2015 {
		t.Errorf("episode row = %+v", taggedEpisode)
	}
	if !taggedEpisode.MatchesTargetTags || taggedEpisode.EligibilityStatus != "eligible" {
		t.Errorf("tagged episode gate = %v/%q", taggedEpisode.MatchesTargetTags, taggedEpisode.EligibilityStatus)
	}

	untagged := rows["episode|43"]
	if untagged.MatchesTargetTags || untagged.EligibilityStatus != "waiting_for_tags" {
		t.Errorf("untagged episode gate = %v/%q", untagged.MatchesTargetTags, untagged.EligibilityStatus)
	}

	fileless := rows["episode|44"]
	if fileless.TargetFilePath != "" || fileless.EligibilityStatus != "missing_target" {
		t.Errorf("fileless episode = %+v", fileless)
	}

	// arr_instances rows exist, are enabled, carry an encrypted key and record
	// the pass.
	for kind, id := range map[string]int64{"radarr": rows["movie|7"].InstanceID, "sonarr": rows["episode|42"].InstanceID} {
		var (
			baseURL   string
			enc       []byte
			enabled   bool
			lastSync  *string
			last4     string
			instances int
		)
		if err := r.store.DB.QueryRow(
			`SELECT base_url, api_key_enc, enabled, last_sync_at, api_key_last4
			 FROM arr_instances WHERE id = ?`, id).
			Scan(&baseURL, &enc, &enabled, &lastSync, &last4); err != nil {
			t.Fatalf("read %s instance: %v", kind, err)
		}
		if baseURL == "" || len(enc) == 0 || !enabled {
			t.Errorf("%s instance row = %q enc=%d enabled=%v", kind, baseURL, len(enc), enabled)
		}
		if last4 == "" || lastSync == nil || *lastSync == "" {
			t.Errorf("%s instance last4/last_sync = %q/%v", kind, last4, lastSync)
		}
		if err := r.store.DB.QueryRow(`SELECT COUNT(*) FROM arr_instances WHERE type = ?`, kind).Scan(&instances); err != nil {
			t.Fatalf("count %s instances: %v", kind, err)
		}
		if instances != 1 {
			t.Errorf("%s instance rows = %d, want 1", kind, instances)
		}
	}

	// A second pass is a no-op: everything is unchanged and no id moves.
	ids := cacheIDs(t, r.store)
	stats, err = p.Sync(context.Background())
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if stats.Unchanged != 5 || stats.Inserted != 0 || stats.Updated != 0 || stats.Deleted != 0 {
		t.Fatalf("second pass stats = %+v, want 5 unchanged", stats)
	}
	for key, id := range cacheIDs(t, r.store) {
		if ids[key] != id {
			t.Errorf("row %s id = %d, want %d (stable ids)", key, id, ids[key])
		}
	}
}

// TestSyncSkipsDisabledInstancesAndMissingKeys documents the enable gate: a
// disabled connection is never contacted, and an enabled instance without a
// stored key fails alone instead of aborting the pass.
func TestSyncSkipsDisabledInstancesAndMissingKeys(t *testing.T) {
	r := newRig(t)
	// Enabled connection whose secret is deliberately absent.
	raw, err := json.Marshal(settings.ArrConnections{Instances: []settings.ArrConnection{
		{Kind: "radarr", BaseURL: "http://radarr.invalid", Enabled: false},
		{Kind: "sonarr", BaseURL: sonarrServer(t).URL, Enabled: true},
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := r.settings.Set(context.Background(), settings.KeyArrConnections, raw); err != nil {
		t.Fatalf("set connections: %v", err)
	}

	stats, err := r.newPoller(time.Minute).Sync(context.Background())
	if err == nil {
		t.Fatal("Sync succeeded with a missing API key")
	}
	if !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("error = %v, want ErrMissingAPIKey", err)
	}
	if stats.Instances != 1 || stats.Failed != 1 {
		t.Fatalf("stats = %+v, want 1 instance / 1 failure", stats)
	}
	var count int
	if err := r.store.DB.QueryRow(`SELECT COUNT(*) FROM arr_instances`).Scan(&count); err != nil {
		t.Fatalf("count instances: %v", err)
	}
	if count != 0 {
		t.Errorf("arr_instances rows = %d, want 0 (disabled instance must not be created)", count)
	}
}

// panickingStore fails the cache write with a panic, like a driver bug would.
type panickingStore struct {
	LibraryStore
	mu       sync.Mutex
	attempts int
}

func (p *panickingStore) ArrInstanceByKind(ctx context.Context, kind string) (store.ArrInstanceSyncRow, bool, error) {
	return store.ArrInstanceSyncRow{ID: 1, Kind: kind, BaseURL: "http://x", Enabled: true}, true, nil
}

func (p *panickingStore) SaveArrInstance(ctx context.Context, row store.ArrInstanceSyncRow) (int64, error) {
	return row.ID, nil
}

func (p *panickingStore) TouchArrInstanceSync(ctx context.Context, id int64) error { return nil }

func (p *panickingStore) RecordArrInstanceSyncFailure(ctx context.Context, id int64, cause string) error {
	return nil
}

func (p *panickingStore) SyncLibraryCache(ctx context.Context, instanceID int64, items []store.LibraryCacheItem) (store.LibrarySyncStats, error) {
	p.mu.Lock()
	p.attempts++
	p.mu.Unlock()
	panic("cache write exploded")
}

// TestLoopSurvivesPanic proves the background loop keeps polling after a panic
// in one pass instead of dying silently.
func TestLoopSurvivesPanic(t *testing.T) {
	r := newRig(t)
	radarr := radarrServer(t)
	r.configure(settings.ArrConnection{Kind: "radarr", BaseURL: radarr.URL, Enabled: true})
	r.key("radarr", radarr.APIKey())

	fake := &panickingStore{}
	p, err := New(Deps{
		Store:    fake,
		Settings: r.settings,
		Secrets:  r.secrets,
		Interval: 10 * time.Millisecond,
		Timeout:  testHTTPTimeout,
		Logf:     t.Logf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The loop re-reads arr.options (300 s after seeding) on every iteration,
	// so the ticks are hand driven instead of waiting out a real cadence.
	tr := newFakeTicker()
	p.newTicker = func(time.Duration) pollTicker { return tr }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	deadline := time.Now().Add(5 * time.Second)
	attempts := 0
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		attempts = fake.attempts
		fake.mu.Unlock()
		if attempts >= 2 {
			return
		}
		tr.ch <- time.Now()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("poller made %d attempts, want at least 2 (loop must survive a panic)", attempts)
}

// TestNewDefaultsInterval pins the settled 300 s cadence.
func TestNewDefaultsInterval(t *testing.T) {
	r := newRig(t)
	p, err := New(Deps{Store: r.store, Settings: r.settings, Secrets: r.secrets})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Interval() != DefaultInterval {
		t.Errorf("Interval() = %v, want %v", p.Interval(), DefaultInterval)
	}
	if DefaultInterval != 300*time.Second {
		t.Errorf("DefaultInterval = %v, want 300s", DefaultInterval)
	}
}

func TestNewRejectsMissingDeps(t *testing.T) {
	if _, err := New(Deps{}); err == nil {
		t.Fatal("New accepted empty Deps")
	}
}

// readCache loads every cache row keyed by "media|externalID".
func readCache(t *testing.T, st *store.Store) map[string]cacheRow {
	t.Helper()
	rows, err := st.DB.Query(
		`SELECT id, arr_instance_id, media_type, external_item_id, COALESCE(series_id,0),
		        COALESCE(tmdb_id,0), COALESCE(tvdb_id,0), title, COALESCE(original_language_iso2,''),
		        COALESCE(year,0), season_number, episode_number, COALESCE(episode_title,''),
		        COALESCE(media_folder_path,''), COALESCE(target_file_path,''),
		        matches_target_tags, eligibility_status,
		        COALESCE(arr_tag_ids_json,'[]'), COALESCE(arr_tag_labels_json,'[]'),
		        COALESCE(root_folder_path,''), COALESCE(audio_languages_json,'[]'),
		        COALESCE(monitored,0)
		 FROM arr_library_cache`)
	if err != nil {
		t.Fatalf("query cache: %v", err)
	}
	defer rows.Close()

	out := map[string]cacheRow{}
	for rows.Next() {
		var (
			row      cacheRow
			season   *int64
			episode  *int64
			media    string
			ext      int64
			idsJSON  string
			lblJSON  string
			langJSON string
		)
		if err := rows.Scan(&row.ID, &row.InstanceID, &media, &ext, &row.SeriesID,
			&row.TMDBID, &row.TVDBID, &row.Title, &row.OriginalLanguageISO2, &row.Year,
			&season, &episode, &row.EpisodeTitle, &row.MediaFolderPath, &row.TargetFilePath,
			&row.MatchesTargetTags, &row.EligibilityStatus, &idsJSON, &lblJSON,
			&row.RootFolderPath, &langJSON, &row.Monitored); err != nil {
			t.Fatalf("scan cache row: %v", err)
		}
		if err := json.Unmarshal([]byte(idsJSON), &row.ArrTagIDs); err != nil {
			t.Fatalf("decode arr tag ids %q: %v", idsJSON, err)
		}
		if err := json.Unmarshal([]byte(lblJSON), &row.ArrTagLabels); err != nil {
			t.Fatalf("decode arr tag labels %q: %v", lblJSON, err)
		}
		if err := json.Unmarshal([]byte(langJSON), &row.AudioLanguages); err != nil {
			t.Fatalf("decode audio languages %q: %v", langJSON, err)
		}
		if season != nil {
			v := int(*season)
			row.SeasonNumber = &v
		}
		if episode != nil {
			v := int(*episode)
			row.EpisodeNumber = &v
		}
		row.MediaType = media
		row.ExternalItemID = ext
		out[fmt.Sprintf("%s|%d", media, ext)] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate cache: %v", err)
	}
	return out
}

type cacheRow struct {
	ID                   int64
	InstanceID           int64
	MediaType            string
	ExternalItemID       int64
	SeriesID             int64
	TMDBID               int64
	TVDBID               int64
	Title                string
	OriginalLanguageISO2 string
	Year                 int
	SeasonNumber         *int
	EpisodeNumber        *int
	EpisodeTitle         string
	MediaFolderPath      string
	TargetFilePath       string
	MatchesTargetTags    bool
	EligibilityStatus    string
	ArrTagIDs            []int64
	ArrTagLabels         []string
	RootFolderPath       string
	// AudioLanguages is the decoded audio_languages_json; Monitored the
	// arr-reported flag.
	AudioLanguages []string
	Monitored      bool
}

func cacheIDs(t *testing.T, st *store.Store) map[string]int64 {
	t.Helper()
	rows := readCache(t, st)
	out := make(map[string]int64, len(rows))
	for key, row := range rows {
		out[key] = row.ID
	}
	return out
}

// TestSyncUpdatesChangedRowAndDeletesVanished drives the diff through the
// poller with a swappable client source: the same instance id must be reused
// for an updated item, and a vanished item must be deleted.
func TestSyncUpdatesChangedRowAndDeletesVanished(t *testing.T) {
	r := newRig(t)
	r.configure(settings.ArrConnection{Kind: "radarr", BaseURL: "http://radarr.invalid", Enabled: true})
	if err := r.secrets.Set("arr.radarr.api_key", "key"); err != nil {
		t.Fatalf("set secret: %v", err)
	}
	// This test asserts the filename criterion; ADR-0012's default is empty.
	r.tagFilter(settings.TargetTagFilter{
		RequiredFilenameTags: []string{"[ArcA310]"},
		TagMatchMode:         "ANY",
		EnforceTagFilter:     true,
	})

	src := &fakeRadarr{movies: []arr.Movie{{
		ID: 7, Title: "Shrek", Year: 2001, HasFile: true,
		OriginalLanguage: arr.Language{ID: 1, Name: "English"},
		MovieFile:        &arr.MediaFile{Path: "/media/movies/Shrek/Shrek [ArcA310].mkv"},
	}}}
	p, err := New(Deps{
		Store:     r.store,
		Settings:  r.settings,
		Secrets:   r.secrets,
		Interval:  time.Minute,
		NewRadarr: func(baseURL, apiKey string) (RadarrSource, error) { return src, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stats, err := p.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.Inserted != 1 {
		t.Fatalf("inserted = %d, want 1", stats.Inserted)
	}
	before := readCache(t, r.store)["movie|7"]

	// The instance renamed the movie and re-imported an untagged file.
	src.movies = []arr.Movie{{
		ID: 7, Title: "Shrek 2", Year: 2004, HasFile: true,
		OriginalLanguage: arr.Language{ID: 2, Name: "Polish"},
		MovieFile:        &arr.MediaFile{Path: "/media/movies/Shrek 2/Shrek 2.mkv"},
	}}
	stats, err = p.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync (changed): %v", err)
	}
	if stats.Updated != 1 || stats.Inserted != 0 || stats.Deleted != 0 {
		t.Fatalf("changed stats = %+v", stats)
	}
	after := readCache(t, r.store)["movie|7"]
	if after.ID != before.ID {
		t.Errorf("row id = %d, want %d (update in place)", after.ID, before.ID)
	}
	if after.Title != "Shrek 2" || after.Year != 2004 || after.OriginalLanguageISO2 != "pl" ||
		after.EligibilityStatus != "waiting_for_tags" || after.MatchesTargetTags {
		t.Errorf("updated row = %+v", after)
	}

	// The movie vanishes from arr: the row goes away.
	src.movies = nil
	stats, err = p.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync (vanished): %v", err)
	}
	if stats.Deleted != 1 {
		t.Fatalf("vanished stats = %+v, want 1 deleted", stats)
	}
	if _, ok := readCache(t, r.store)["movie|7"]; ok {
		t.Error("vanished movie row still cached")
	}
}

type fakeRadarr struct {
	movies  []arr.Movie
	tags    []arr.Tag
	roots   []arr.RootFolder
	tagsErr error
}

func (f *fakeRadarr) Movies(context.Context) ([]arr.Movie, error) {
	return f.movies, nil
}

func (f *fakeRadarr) Tags(context.Context) ([]arr.Tag, error) {
	if f.tagsErr != nil {
		return nil, f.tagsErr
	}
	return f.tags, nil
}

func (f *fakeRadarr) RootFolders(context.Context) ([]arr.RootFolder, error) {
	return f.roots, nil
}

// TestSyncCachesArrTagsAndRootFolder is the M9c foundation path: tags are
// resolved to labels from /api/v3/tag, episodes inherit their series' tags and
// every row records the longest matching canonical root-folder path.
func TestSyncCachesArrTagsAndRootFolder(t *testing.T) {
	r := newRig(t)
	radarr := arrmock.New(arrmock.Radarr, "key-radarr", arrmock.Fixtures{
		Tags: []arrmock.Tag{
			{ID: 1, Label: "dub"},
			{ID: 4, Label: "Polish Dubbing"},
			{ID: 9, Label: "unused"},
		},
		// /media/movies wins over /media: the canonical root is the most
		// specific configured prefix.
		RootFolders: []arrmock.RootFolder{{ID: 1, Path: "/media"}, {ID: 2, Path: "/media/movies"}},
		Movies: []arrmock.Movie{{
			ID: 7, Title: "Shrek", Year: 2001, TmdbID: 808, HasFile: true,
			Path: "/media/movies/Shrek (2001)", Tags: []int{4, 1},
			OriginalLanguage: arrmock.Language{ID: 1, Name: "English"},
			MovieFile:        &arrmock.MovieFile{Path: "/media/movies/Shrek (2001)/Shrek [ArcA310].mkv"},
		}},
	})
	defer radarr.Close()
	sonarr := arrmock.New(arrmock.Sonarr, "key-sonarr", arrmock.Fixtures{
		Tags:        []arrmock.Tag{{ID: 2, Label: "keep"}},
		RootFolders: []arrmock.RootFolder{{ID: 1, Path: "/media/tv"}},
		Series: []arrmock.Series{{
			ID: 3, Title: "The Expanse", Year: 2015, TvdbID: 280619,
			Path: "/media/tv/The Expanse", Tags: []int{2},
			OriginalLanguage: arrmock.Language{ID: 2, Name: "English"},
		}},
		Episodes: []arrmock.Episode{{
			ID: 42, SeriesID: 3, SeasonNumber: 1, EpisodeNumber: 1, Title: "Dulcinea", HasFile: true,
			EpisodeFile: &arrmock.EpisodeFile{Path: "/media/tv/The Expanse/Season 01/S01E01.mkv"},
		}},
	})
	defer sonarr.Close()

	r.configure(
		settings.ArrConnection{Kind: "radarr", BaseURL: radarr.URL, Enabled: true},
		settings.ArrConnection{Kind: "sonarr", BaseURL: sonarr.URL, Enabled: true},
	)
	r.key("radarr", radarr.APIKey())
	r.key("sonarr", sonarr.APIKey())

	if _, err := r.newPoller(time.Minute).Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	rows := readCache(t, r.store)
	movie := rows["movie|7"]
	if want := []int64{1, 4}; !equalInt64s(movie.ArrTagIDs, want) {
		t.Errorf("movie tag ids = %v, want %v", movie.ArrTagIDs, want)
	}
	if want := []string{"dub", "Polish Dubbing"}; !equalStrings(movie.ArrTagLabels, want) {
		t.Errorf("movie tag labels = %v, want %v", movie.ArrTagLabels, want)
	}
	if movie.RootFolderPath != "/media/movies" {
		t.Errorf("movie root folder = %q, want /media/movies", movie.RootFolderPath)
	}

	episode := rows["episode|42"]
	if want := []int64{2}; !equalInt64s(episode.ArrTagIDs, want) {
		t.Errorf("episode tag ids = %v, want the series tags %v", episode.ArrTagIDs, want)
	}
	if want := []string{"keep"}; !equalStrings(episode.ArrTagLabels, want) {
		t.Errorf("episode tag labels = %v, want %v", episode.ArrTagLabels, want)
	}
	if episode.RootFolderPath != "/media/tv" {
		t.Errorf("episode root folder = %q, want /media/tv", episode.RootFolderPath)
	}
}

// TestSyncRefreshesArrTagSnapshot pins the poll refresh: a tag removed from the
// item, and a tag deleted from the instance, both disappear on the next pass
// instead of lingering in the cache.
func TestSyncRefreshesArrTagSnapshot(t *testing.T) {
	r := newRig(t)
	r.configure(settings.ArrConnection{Kind: "radarr", BaseURL: "http://radarr.invalid", Enabled: true})
	if err := r.secrets.Set("arr.radarr.api_key", "key"); err != nil {
		t.Fatalf("set secret: %v", err)
	}

	src := &fakeRadarr{
		tags:  []arr.Tag{{ID: 1, Label: "dub"}, {ID: 4, Label: "Polish Dubbing"}},
		roots: []arr.RootFolder{{Path: "/media/movies"}},
		movies: []arr.Movie{{
			ID: 7, Title: "Shrek", Year: 2001, HasFile: true,
			Path: "/media/movies/Shrek", Tags: []int{1, 4},
			OriginalLanguage: arr.Language{ID: 1, Name: "English"},
			MovieFile:        &arr.MediaFile{Path: "/media/movies/Shrek/Shrek.mkv"},
		}},
	}
	p, err := New(Deps{
		Store: r.store, Settings: r.settings, Secrets: r.secrets,
		Interval:  time.Minute,
		NewRadarr: func(baseURL, apiKey string) (RadarrSource, error) { return src, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	before := readCache(t, r.store)["movie|7"]
	if len(before.ArrTagLabels) != 2 {
		t.Fatalf("initial labels = %v, want 2", before.ArrTagLabels)
	}

	// The operator removed tag 1 from the movie and deleted tag 4 from Radarr.
	src.movies[0].Tags = []int{4}
	src.tags = []arr.Tag{{ID: 1, Label: "dub"}}
	stats, err := p.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync (changed): %v", err)
	}
	if stats.Updated != 1 {
		t.Fatalf("stats = %+v, want 1 updated", stats)
	}
	after := readCache(t, r.store)["movie|7"]
	if after.ID != before.ID {
		t.Errorf("row id = %d, want %d (update in place)", after.ID, before.ID)
	}
	if len(after.ArrTagLabels) != 0 {
		t.Errorf("labels = %v, want empty: id 4 is no longer reachable from the instance tag list", after.ArrTagLabels)
	}
	if len(after.ArrTagIDs) != 0 {
		t.Errorf("ids = %v, want empty (unresolvable ids are not persisted)", after.ArrTagIDs)
	}
}

// TestSyncFailsInstanceWhenTagsUnreadable pins fail-closed metadata: a failed
// tag read leaves the previous snapshot in place instead of caching an empty
// one (which would silently change eligibility later).
func TestSyncFailsInstanceWhenTagsUnreadable(t *testing.T) {
	r := newRig(t)
	r.configure(settings.ArrConnection{Kind: "radarr", BaseURL: "http://radarr.invalid", Enabled: true})
	if err := r.secrets.Set("arr.radarr.api_key", "key"); err != nil {
		t.Fatalf("set secret: %v", err)
	}

	src := &fakeRadarr{
		tags: []arr.Tag{{ID: 1, Label: "dub"}},
		movies: []arr.Movie{{
			ID: 7, Title: "Shrek", Year: 2001, HasFile: true,
			Path: "/media/movies/Shrek", Tags: []int{1},
			OriginalLanguage: arr.Language{ID: 1, Name: "English"},
			MovieFile:        &arr.MediaFile{Path: "/media/movies/Shrek/Shrek.mkv"},
		}},
	}
	p, err := New(Deps{
		Store: r.store, Settings: r.settings, Secrets: r.secrets,
		Interval:  time.Minute,
		NewRadarr: func(baseURL, apiKey string) (RadarrSource, error) { return src, nil },
		Logf:      t.Logf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	before := readCache(t, r.store)["movie|7"]

	src.tagsErr = errors.New("radarr unavailable")
	stats, err := p.Sync(context.Background())
	if err == nil {
		t.Fatal("Sync succeeded although the tag read failed")
	}
	if stats.Failed != 1 || stats.Updated != 0 || stats.Deleted != 0 {
		t.Fatalf("stats = %+v, want 1 failed instance and no cache writes", stats)
	}
	after := readCache(t, r.store)["movie|7"]
	if after.ID != before.ID || len(after.ArrTagLabels) != 1 || after.ArrTagLabels[0] != "dub" {
		t.Errorf("cache row = %+v, want the previous snapshot untouched", after)
	}
}

// TestFailedPassWritesHealth pins the poller side of #29: a failed pass records
// last_sync_ok = 0 with the cause instead of leaving health stale, keeps the
// previous last_sync_at, and leaves the cached snapshot alone.
func TestFailedPassWritesHealth(t *testing.T) {
	r := newRig(t)
	r.configure(settings.ArrConnection{Kind: "radarr", BaseURL: "http://radarr.invalid", Enabled: true})
	r.key("radarr", "key-radarr")

	src := &fakeRadarr{
		tags: []arr.Tag{{ID: 1, Label: "dub"}},
		movies: []arr.Movie{{
			ID: 7, Title: "Shrek", Year: 2001, HasFile: true,
			Path: "/media/movies/Shrek", Tags: []int{1},
			OriginalLanguage: arr.Language{ID: 1, Name: "English"},
			MovieFile:        &arr.MediaFile{Path: "/media/movies/Shrek/Shrek.mkv"},
		}},
	}
	p, err := New(Deps{
		Store: r.store, Settings: r.settings, Secrets: r.secrets,
		Interval:  time.Minute,
		NewRadarr: func(baseURL, apiKey string) (RadarrSource, error) { return src, nil },
		Logf:      t.Logf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := p.Sync(context.Background()); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	var instanceID int64
	if err := r.store.DB.QueryRow(`SELECT id FROM arr_instances WHERE type = 'radarr'`).Scan(&instanceID); err != nil {
		t.Fatalf("read instance id: %v", err)
	}
	readHealth := func() (ok sql.NullInt64, cause, attempt, lastSync sql.NullString) {
		t.Helper()
		if err := r.store.DB.QueryRow(
			`SELECT last_sync_ok, last_sync_error, last_attempt_at, last_sync_at
			 FROM arr_instances WHERE id = ?`, instanceID).
			Scan(&ok, &cause, &attempt, &lastSync); err != nil {
			t.Fatalf("read sync health: %v", err)
		}
		return ok, cause, attempt, lastSync
	}

	okBefore, causeBefore, _, lastBefore := readHealth()
	if !okBefore.Valid || okBefore.Int64 != 1 || causeBefore.Valid || !lastBefore.Valid {
		t.Fatalf("health after a good pass = ok %v cause %v last %v, want ok=1/no error/last_sync_at",
			okBefore, causeBefore, lastBefore)
	}
	rowBefore := readCache(t, r.store)["movie|7"]

	src.tagsErr = errors.New("radarr unavailable")
	stats, err := p.Sync(context.Background())
	if err == nil {
		t.Fatal("second Sync succeeded although the tag read failed")
	}
	if stats.Failed != 1 {
		t.Fatalf("stats = %+v, want 1 failed instance", stats)
	}

	ok, cause, attempt, lastAfter := readHealth()
	if !ok.Valid || ok.Int64 != 0 {
		t.Errorf("last_sync_ok = %v, want 0 after a failed pass", ok)
	}
	if !cause.Valid || !strings.Contains(cause.String, "radarr unavailable") {
		t.Errorf("last_sync_error = %v, want the pass cause", cause)
	}
	if !attempt.Valid || attempt.String == "" {
		t.Error("last_attempt_at not stamped by a failed pass")
	}
	if lastAfter != lastBefore {
		t.Errorf("last_sync_at = %v, want %v (a failed pass must not rewrite it)",
			lastAfter, lastBefore)
	}

	rowAfter, still := readCache(t, r.store)["movie|7"]
	if !still {
		t.Fatal("cache row vanished after a failed pass")
	}
	if rowAfter.ID != rowBefore.ID || len(rowAfter.ArrTagLabels) != 1 {
		t.Errorf("cache row = %+v, want the previous snapshot", rowAfter)
	}
}

// moviesErrRadarr fails the movie listing after an optional hook (used to
// cancel the pass context mid-flight) or with a canned error.
type moviesErrRadarr struct {
	fakeRadarr
	hook func()
	err  error
}

func (m *moviesErrRadarr) Movies(ctx context.Context) ([]arr.Movie, error) {
	if m.hook != nil {
		m.hook()
	}
	if m.err != nil {
		return nil, m.err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.fakeRadarr.Movies(ctx)
}

// singleMovieRadarr is the one-movie snapshot the health tests sync.
func singleMovieRadarr() fakeRadarr {
	return fakeRadarr{movies: []arr.Movie{{
		ID: 7, Title: "Shrek", Year: 2001, HasFile: true,
		Path:             "/media/movies/Shrek",
		OriginalLanguage: arr.Language{ID: 1, Name: "English"},
		MovieFile:        &arr.MediaFile{Path: "/media/movies/Shrek/Shrek.mkv"},
	}}}
}

// TestMissingAPIKeyRecordsHealth pins the review fix: a failure that happens
// before ensureInstance (the secret read) must still turn the instance row red.
// The id is resolved up front, so a green row from an earlier pass is replaced
// by last_sync_ok = 0 with a scrubbed cause instead of staying green forever.
func TestMissingAPIKeyRecordsHealth(t *testing.T) {
	r := newRig(t)
	r.configure(settings.ArrConnection{Kind: "radarr", BaseURL: "http://radarr.invalid", Enabled: true})
	r.key("radarr", "key-radarr")

	src := singleMovieRadarr()
	p, err := New(Deps{
		Store: r.store, Settings: r.settings, Secrets: r.secrets,
		Interval:  time.Minute,
		NewRadarr: func(baseURL, apiKey string) (RadarrSource, error) { return &src, nil },
		Logf:      t.Logf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A good pass creates the arr_instances row and leaves it green.
	if _, err := p.Sync(context.Background()); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	var instanceID int64
	if err := r.store.DB.QueryRow(`SELECT id FROM arr_instances WHERE type = 'radarr'`).Scan(&instanceID); err != nil {
		t.Fatalf("read instance id: %v", err)
	}
	readHealth := func() (ok sql.NullInt64, cause, attempt sql.NullString) {
		t.Helper()
		if err := r.store.DB.QueryRow(
			`SELECT last_sync_ok, last_sync_error, last_attempt_at
			 FROM arr_instances WHERE id = ?`, instanceID).
			Scan(&ok, &cause, &attempt); err != nil {
			t.Fatalf("read sync health: %v", err)
		}
		return ok, cause, attempt
	}
	if ok, cause, _ := readHealth(); !ok.Valid || ok.Int64 != 1 || cause.Valid {
		t.Fatalf("baseline health = ok %v cause %v, want green with no cause", ok, cause)
	}

	// The stored key vanishes: this pass fails at the secret read, before
	// ensureInstance has had any chance to supply the id.
	if err := r.secrets.Delete("arr.radarr.api_key"); err != nil {
		t.Fatalf("delete secret: %v", err)
	}
	stats, err := p.Sync(context.Background())
	if !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("error = %v, want ErrMissingAPIKey", err)
	}
	if stats.Failed != 1 {
		t.Fatalf("stats = %+v, want 1 failed instance", stats)
	}

	ok, cause, attempt := readHealth()
	if !ok.Valid || ok.Int64 != 0 {
		t.Errorf("last_sync_ok = %v, want 0: a missing API key must not leave the row green", ok)
	}
	if !cause.Valid || !strings.Contains(cause.String, "no API key") {
		t.Errorf("last_sync_error = %v, want the missing-key cause", cause)
	}
	if cause.Valid && strings.Contains(cause.String, "key-radarr") {
		t.Errorf("last_sync_error leaked the API key: %q", cause.String)
	}
	if !attempt.Valid || attempt.String == "" {
		t.Error("last_attempt_at not stamped by a missing-key pass")
	}
}

// TestCancelledPassLeavesHealthUntouched pins the shutdown rule: a pass aborted
// by the caller's context is not a sync failure, while a per-request timeout
// still is.
func TestCancelledPassLeavesHealthUntouched(t *testing.T) {
	r := newRig(t)
	r.configure(settings.ArrConnection{Kind: "radarr", BaseURL: "http://radarr.invalid", Enabled: true})
	r.key("radarr", "key-radarr")

	src := &moviesErrRadarr{fakeRadarr: singleMovieRadarr()}
	p, err := New(Deps{
		Store: r.store, Settings: r.settings, Secrets: r.secrets,
		Interval:  time.Minute,
		NewRadarr: func(baseURL, apiKey string) (RadarrSource, error) { return src, nil },
		Logf:      t.Logf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Sync(context.Background()); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	var instanceID int64
	if err := r.store.DB.QueryRow(`SELECT id FROM arr_instances WHERE type = 'radarr'`).Scan(&instanceID); err != nil {
		t.Fatalf("read instance id: %v", err)
	}
	readHealth := func() (ok sql.NullInt64, cause sql.NullString) {
		t.Helper()
		if err := r.store.DB.QueryRow(
			`SELECT last_sync_ok, last_sync_error FROM arr_instances WHERE id = ?`, instanceID).
			Scan(&ok, &cause); err != nil {
			t.Fatalf("read sync health: %v", err)
		}
		return ok, cause
	}
	if ok, cause := readHealth(); !ok.Valid || ok.Int64 != 1 || cause.Valid {
		t.Fatalf("baseline health = ok %v cause %v, want green", ok, cause)
	}

	// Graceful shutdown: the caller cancels the pass while it is in flight.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src.hook = cancel
	stats, err := p.Sync(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled pass error = %v, want context.Canceled", err)
	}
	if stats.Failed != 1 {
		t.Fatalf("stats = %+v, want the cancelled instance counted as failed for the pass", stats)
	}
	ok, cause := readHealth()
	if !ok.Valid || ok.Int64 != 1 {
		t.Errorf("last_sync_ok = %v, want 1: a shutdown cancellation must not be recorded as a failure", ok)
	}
	if cause.Valid {
		t.Errorf("last_sync_error = %v, want untouched by a cancelled pass", cause)
	}

	// A genuine per-request timeout keeps recording the failure.
	src.hook = nil
	src.err = context.DeadlineExceeded
	if _, err := p.Sync(context.Background()); err == nil {
		t.Fatal("Sync succeeded although the request timed out")
	}
	ok, cause = readHealth()
	if !ok.Valid || ok.Int64 != 0 {
		t.Errorf("last_sync_ok = %v, want 0 after a timed-out pass", ok)
	}
	if !cause.Valid || !strings.Contains(cause.String, "deadline exceeded") {
		t.Errorf("last_sync_error = %v, want the timeout cause", cause)
	}
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// tagFilter stores the target.tag_filter section explicitly. ADR-0012 ships an
// empty default, so tests that assert filename gating set their own criterion.
func (r *rig) tagFilter(tf settings.TargetTagFilter) {
	r.t.Helper()
	raw, err := json.Marshal(tf)
	if err != nil {
		r.t.Fatalf("marshal tag filter: %v", err)
	}
	if err := r.settings.Set(r.context(), settings.KeyTargetTagFilter, raw); err != nil {
		r.t.Fatalf("set tag filter: %v", err)
	}
}

// TestSyncArrSourceEligibility is the M9c call-site proof: the poller
// classifies each movie/episode with its own arr tag labels and root folder,
// so an arr criterion narrows eligibility without a second filename pass.
func TestSyncArrSourceEligibility(t *testing.T) {
	r := newRig(t)
	radarr := arrmock.New(arrmock.Radarr, "key-radarr", arrmock.Fixtures{
		Tags:        []arrmock.Tag{{ID: 1, Label: "dub"}, {ID: 2, Label: "keep"}},
		RootFolders: []arrmock.RootFolder{{ID: 1, Path: "/media/movies"}, {ID: 2, Path: "/media/other"}},
		Movies: []arrmock.Movie{
			{
				ID: 7, Title: "Shrek", Year: 2001, HasFile: true, Tags: []int{1},
				Path: "/media/movies/Shrek", OriginalLanguage: arrmock.Language{ID: 1, Name: "English"},
				MovieFile: &arrmock.MovieFile{Path: "/media/movies/Shrek/Shrek.mkv"},
			},
			{
				ID: 8, Title: "Kept", Year: 2002, HasFile: true, Tags: []int{2},
				Path: "/media/movies/Kept", OriginalLanguage: arrmock.Language{ID: 1, Name: "English"},
				MovieFile: &arrmock.MovieFile{Path: "/media/movies/Kept/Kept.mkv"},
			},
			{
				// Right tag, wrong root folder: root scoping narrows it out.
				ID: 9, Title: "Elsewhere", Year: 2003, HasFile: true, Tags: []int{1},
				Path: "/media/other/Elsewhere", OriginalLanguage: arrmock.Language{ID: 1, Name: "English"},
				MovieFile: &arrmock.MovieFile{Path: "/media/other/Elsewhere/Elsewhere.mkv"},
			},
		},
	})
	defer radarr.Close()
	sonarr := arrmock.New(arrmock.Sonarr, "key-sonarr", arrmock.Fixtures{
		Tags:        []arrmock.Tag{{ID: 3, Label: "dub"}},
		RootFolders: []arrmock.RootFolder{{ID: 1, Path: "/media/tv"}},
		Series: []arrmock.Series{{
			ID: 3, Title: "The Expanse", Year: 2015, TvdbID: 280619,
			Path: "/media/tv/The Expanse", Tags: []int{3},
			OriginalLanguage: arrmock.Language{ID: 2, Name: "English"},
		}},
		Episodes: []arrmock.Episode{{
			ID: 42, SeriesID: 3, SeasonNumber: 1, EpisodeNumber: 1, Title: "Dulcinea", HasFile: true,
			EpisodeFile: &arrmock.EpisodeFile{Path: "/media/tv/The Expanse/Season 01/S01E01 - Dulcinea.mkv"},
		}},
	})
	defer sonarr.Close()

	r.configure(
		settings.ArrConnection{Kind: "radarr", BaseURL: radarr.URL, Enabled: true},
		settings.ArrConnection{Kind: "sonarr", BaseURL: sonarr.URL, Enabled: true},
	)
	r.key("radarr", radarr.APIKey())
	r.key("sonarr", sonarr.APIKey())
	r.tagFilter(settings.TargetTagFilter{
		SourceMode:       "arr",
		RequiredArrTags:  []string{"DUB"},
		RootFolderPaths:  []string{"/media/movies", "/media/tv"},
		TagMatchMode:     "ANY",
		EnforceTagFilter: true,
	})

	if _, err := r.newPoller(time.Minute).Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	rows := readCache(t, r.store)
	if got := rows["movie|7"]; got.EligibilityStatus != "eligible" || !got.MatchesTargetTags {
		t.Errorf("arr-tagged movie = %v/%q, want eligible (filename has no tag)", got.MatchesTargetTags, got.EligibilityStatus)
	}
	if got := rows["movie|8"]; got.EligibilityStatus != "waiting_for_tags" {
		t.Errorf("movie without the required arr tag = %q, want waiting_for_tags", got.EligibilityStatus)
	}
	if got := rows["movie|9"]; got.EligibilityStatus != "waiting_for_tags" {
		t.Errorf("movie outside the root allowlist = %q, want waiting_for_tags", got.EligibilityStatus)
	}
	if got := rows["episode|42"]; got.EligibilityStatus != "eligible" {
		t.Errorf("series-tagged episode = %q, want eligible", got.EligibilityStatus)
	}
}

// blockingRadarr holds the pass inside Movies until release is closed, so a
// test can observe the exclusive pass slot while one pass is in flight.
type blockingRadarr struct {
	fakeRadarr
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	calls   int
}

func newBlockingRadarr() *blockingRadarr {
	return &blockingRadarr{
		fakeRadarr: singleMovieRadarr(),
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
}

func (b *blockingRadarr) Movies(ctx context.Context) ([]arr.Movie, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.fakeRadarr.Movies(ctx)
}

func (b *blockingRadarr) listCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// waitForList blocks until the in-flight pass has reached the listing.
func (b *blockingRadarr) waitForList(t *testing.T) {
	t.Helper()
	select {
	case <-b.started:
	case <-time.After(testPassTimeout):
		t.Fatal("the sync pass never reached the arr listing")
	}
}

// newBlockingPoller builds a poller over one enabled Radarr connection whose
// client blocks inside Movies.
func newBlockingPoller(t *testing.T, r *rig) (*Poller, *blockingRadarr) {
	t.Helper()
	r.configure(settings.ArrConnection{Kind: "radarr", BaseURL: "http://radarr.invalid", Enabled: true})
	r.key("radarr", "key-radarr")
	src := newBlockingRadarr()
	p, err := New(Deps{
		Store: r.store, Settings: r.settings, Secrets: r.secrets,
		Interval:  time.Minute,
		NewRadarr: func(baseURL, apiKey string) (RadarrSource, error) { return src, nil },
		Logf:      t.Logf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.startManualOnly(t)
	return p, src
}

// startManualOnly marks the poller as running without launching the poll loop,
// so a manual Trigger is accepted without a scheduled pass holding the slot.
// Stop is registered, so a blocked pass is cancelled when the test ends.
func (p *Poller) startManualOnly(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started || p.stopped {
		t.Fatal("startManualOnly: poller is already started or stopped")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.started = true
	p.runCtx = ctx
	p.cancel = cancel
	t.Cleanup(p.Stop)
}

// TestTriggerAfterStopIsRefused pins the shutdown seam: a poller that was never
// started, and one whose Stop already ran, both refuse a manual pass instead of
// running one on an uncancellable context.
func TestTriggerAfterStopIsRefused(t *testing.T) {
	r := newRig(t)
	p, src := newBlockingPoller(t, r)

	// Not started: refused before any arr request.
	unstarted := r.newPoller(time.Minute)
	if err := unstarted.Trigger(context.Background(), "radarr"); !errors.Is(err, ErrPollerStopped) {
		t.Fatalf("Trigger before Start = %v, want ErrPollerStopped", err)
	}

	// Started, then stopped: refused as well.
	p.Stop()
	if err := p.Trigger(context.Background(), "radarr"); !errors.Is(err, ErrPollerStopped) {
		t.Fatalf("Trigger after Stop = %v, want ErrPollerStopped", err)
	}
	p.Wait()
	if got := src.listCalls(); got != 0 {
		t.Errorf("arr listings = %d, want 0: a refused trigger must not reach arr", got)
	}
}

// TestStopDrainsManualPass pins the shutdown drain: a manual pass blocked
// mid-listing is cancelled by Stop, Wait returns, and the cancellation is not
// recorded as a sync failure.
func TestStopDrainsManualPass(t *testing.T) {
	r := newRig(t)
	p, src := newBlockingPoller(t, r)

	if err := p.Trigger(context.Background(), "radarr"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	src.waitForList(t)

	stopped := make(chan struct{})
	go func() {
		p.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(testPassTimeout):
		t.Fatal("Stop did not return: the in-flight manual pass was not drained")
	}
	p.Wait()

	// The instance row exists (ensureInstance ran before the listing), but a
	// shutdown cancellation must leave its health untouched.
	var ok sql.NullInt64
	var cause sql.NullString
	if err := r.store.DB.QueryRow(
		`SELECT last_sync_ok, last_sync_error FROM arr_instances WHERE type = 'radarr'`).
		Scan(&ok, &cause); err != nil {
		t.Fatalf("read sync health: %v", err)
	}
	if ok.Valid {
		t.Errorf("last_sync_ok = %v, want NULL: a drained pass is not a failure", ok)
	}
	if cause.Valid {
		t.Errorf("last_sync_error = %v, want empty: a drained pass records no cause", cause)
	}

	if err := p.Trigger(context.Background(), "radarr"); !errors.Is(err, ErrPollerStopped) {
		t.Errorf("Trigger after the drained stop = %v, want ErrPollerStopped", err)
	}
}

// TestTriggerRejectsConcurrentPass pins the single pass slot: a manual pass in
// flight makes a second Trigger report ErrSyncBusy instead of queueing.
func TestTriggerRejectsConcurrentPass(t *testing.T) {
	r := newRig(t)
	p, src := newBlockingPoller(t, r)

	if err := p.Trigger(context.Background(), "radarr"); err != nil {
		t.Fatalf("first Trigger: %v", err)
	}
	src.waitForList(t)

	if err := p.Trigger(context.Background(), "radarr"); !errors.Is(err, ErrSyncBusy) {
		t.Fatalf("second Trigger = %v, want ErrSyncBusy", err)
	}
	if err := p.Trigger(context.Background(), ""); !errors.Is(err, ErrSyncBusy) {
		t.Fatalf("all-kinds Trigger during a pass = %v, want ErrSyncBusy", err)
	}
	if got := src.listCalls(); got != 1 {
		t.Errorf("arr listings = %d, want 1: a refused trigger must not reach arr", got)
	}

	// Once the pass lands the slot is free again.
	close(src.release)
	p.Wait()
	if err := p.Trigger(context.Background(), "radarr"); err != nil {
		t.Fatalf("Trigger after the pass = %v, want nil", err)
	}
	p.Wait()
}

// TestScheduledPassSkipsWhileManualRuns proves the loop shares the slot with
// manual triggers: a scheduled tick during a manual pass returns without
// contacting arr, rather than queueing a second pass.
func TestScheduledPassSkipsWhileManualRuns(t *testing.T) {
	r := newRig(t)
	p, src := newBlockingPoller(t, r)

	if err := p.Trigger(context.Background(), "radarr"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	src.waitForList(t)

	done := make(chan struct{})
	go func() {
		p.syncPass(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(testPassTimeout):
		t.Fatal("the scheduled pass waited for the manual pass instead of skipping its tick")
	}
	if got := src.listCalls(); got != 1 {
		t.Errorf("arr listings = %d, want 1: the skipped tick must not list anything", got)
	}

	close(src.release)
	p.Wait()
}

// TestManualPassRecordsHealth pins that a manual pass writes sync health
// exactly like a scheduled one: green on success, red with the cause on
// failure, and the cached snapshot untouched by the failure.
func TestManualPassRecordsHealth(t *testing.T) {
	r := newRig(t)
	r.configure(settings.ArrConnection{Kind: "radarr", BaseURL: "http://radarr.invalid", Enabled: true})
	r.key("radarr", "key-radarr")

	src := singleMovieRadarr()
	p, err := New(Deps{
		Store: r.store, Settings: r.settings, Secrets: r.secrets,
		Interval:  time.Minute,
		NewRadarr: func(baseURL, apiKey string) (RadarrSource, error) { return &src, nil },
		Logf:      t.Logf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.startManualOnly(t)

	if err := p.Trigger(context.Background(), "radarr"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	p.Wait()

	var instanceID int64
	if err := r.store.DB.QueryRow(`SELECT id FROM arr_instances WHERE type = 'radarr'`).Scan(&instanceID); err != nil {
		t.Fatalf("read instance id: %v", err)
	}
	readHealth := func() (ok sql.NullInt64, cause, attempt sql.NullString) {
		t.Helper()
		if err := r.store.DB.QueryRow(
			`SELECT last_sync_ok, last_sync_error, last_attempt_at
			 FROM arr_instances WHERE id = ?`, instanceID).
			Scan(&ok, &cause, &attempt); err != nil {
			t.Fatalf("read sync health: %v", err)
		}
		return ok, cause, attempt
	}

	ok, cause, attempt := readHealth()
	if !ok.Valid || ok.Int64 != 1 || cause.Valid {
		t.Fatalf("health after a manual pass = ok %v cause %v, want green with no cause", ok, cause)
	}
	if !attempt.Valid || attempt.String == "" {
		t.Error("last_attempt_at not stamped by a manual pass")
	}
	if _, still := readCache(t, r.store)["movie|7"]; !still {
		t.Fatal("the manual pass cached no rows")
	}

	// A failing manual pass turns the instance red with the cause.
	src.tagsErr = errors.New("radarr unavailable")
	if err := p.Trigger(context.Background(), "radarr"); err != nil {
		t.Fatalf("second Trigger: %v", err)
	}
	p.Wait()

	ok, cause, attempt = readHealth()
	if !ok.Valid || ok.Int64 != 0 {
		t.Errorf("last_sync_ok = %v, want 0 after a failed manual pass", ok)
	}
	if !cause.Valid || !strings.Contains(cause.String, "radarr unavailable") {
		t.Errorf("last_sync_error = %v, want the manual pass cause", cause)
	}
	if !attempt.Valid || attempt.String == "" {
		t.Error("last_attempt_at not stamped by a failed manual pass")
	}
}

// fakeTicker is a hand-driven pollTicker: ch delivers a tick when a test sends
// one and resets records every cadence the loop asks for, so no wall-clock
// period is ever waited on.
type fakeTicker struct {
	ch     chan time.Time
	resets chan time.Duration
}

func newFakeTicker() *fakeTicker {
	return &fakeTicker{ch: make(chan time.Time, 64), resets: make(chan time.Duration, 64)}
}

func (f *fakeTicker) Chan() <-chan time.Time { return f.ch }
func (f *fakeTicker) Reset(d time.Duration)  { f.resets <- d }
func (f *fakeTicker) Stop()                  {}

// logCapture records the poller's log lines without touching *testing.T from
// the loop goroutine.
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCapture) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.mu.Unlock()
}

// count reports how many captured lines contain sub.
func (l *logCapture) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, sub) {
			n++
		}
	}
	return n
}

// waitFor blocks until a captured line contains sub.
func (l *logCapture) waitFor(t *testing.T, sub string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if l.count(sub) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no poller log line containing %q", sub)
}

// pollInterval stores arr.options.poll_interval_sec through the validated API.
func (r *rig) pollInterval(sec int) {
	r.t.Helper()
	raw, err := json.Marshal(settings.ArrOptions{PollIntervalSec: sec})
	if err != nil {
		r.t.Fatalf("marshal arr options: %v", err)
	}
	if err := r.settings.Set(r.context(), settings.KeyArrOptions, raw); err != nil {
		r.t.Fatalf("set arr options: %v", err)
	}
}

// rawPollInterval writes arr.options straight into the settings table, past
// Set's 30..86400 validation, so the loop's guard against a hand-edited
// non-positive (or sub-30 s) value can be exercised.
func (r *rig) rawPollInterval(sec int) {
	r.t.Helper()
	if _, err := r.store.DB.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		settings.KeyArrOptions, fmt.Sprintf(`{"poll_interval_sec":%d}`, sec)); err != nil {
		r.t.Fatalf("store raw arr options: %v", err)
	}
}

// loopRig wires one enabled Radarr connection and starts the poll loop with a
// hand-driven ticker.
func loopRig(
	t *testing.T,
	boot time.Duration,
	newRadarr func(baseURL, apiKey string) (RadarrSource, error),
	logf func(format string, args ...any),
) (*rig, *Poller, *fakeTicker) {
	t.Helper()
	r := newRig(t)
	r.configure(settings.ArrConnection{Kind: "radarr", BaseURL: "http://radarr.invalid", Enabled: true})
	r.key("radarr", "key-radarr")
	if logf == nil {
		logf = t.Logf
	}
	p, err := New(Deps{
		Store:     r.store,
		Settings:  r.settings,
		Secrets:   r.secrets,
		Interval:  boot,
		Timeout:   testHTTPTimeout,
		Logf:      logf,
		NewRadarr: newRadarr,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tr := newFakeTicker()
	p.newTicker = func(time.Duration) pollTicker { return tr }
	p.Start(context.Background())
	t.Cleanup(p.Stop)
	return r, p, tr
}

// waitForFirstPass blocks until the loop's opening pass cached a row, so the
// ticker the test drives belongs to a loop that already ran.
func waitForFirstPass(t *testing.T, r *rig) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := readCache(t, r.store)["movie|7"]; ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the poll loop never completed its first pass")
}

// TestLoopAppliesPollIntervalChangeLive pins U1.4: an edit of
// arr.options.poll_interval_sec moves the running loop's cadence, so it is no
// longer silently ignored until the next restart.
func TestLoopAppliesPollIntervalChangeLive(t *testing.T) {
	src := singleMovieRadarr()
	r, p, tr := loopRig(t, 300*time.Second,
		func(baseURL, apiKey string) (RadarrSource, error) { return &src, nil }, nil)
	r.pollInterval(300)
	waitForFirstPass(t, r)

	if got := p.Interval(); got != 300*time.Second {
		t.Fatalf("Interval() = %v, want the boot 300s", got)
	}

	r.pollInterval(60)
	tr.ch <- time.Now() // wake the loop so it re-reads settings

	select {
	case got := <-tr.resets:
		if got != 60*time.Second {
			t.Fatalf("ticker rebuilt to %v, want 60s", got)
		}
	case <-time.After(testPassTimeout):
		t.Fatal("the loop did not rebuild its ticker after the interval changed")
	}
	if got := p.Interval(); got != 60*time.Second {
		t.Errorf("Interval() = %v, want the live 60s", got)
	}

	// An unchanged value must not rebuild the ticker on every pass.
	tr.ch <- time.Now()
	select {
	case got := <-tr.resets:
		t.Errorf("ticker rebuilt to %v although the interval did not change", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestLoopKeepsIntervalOnNonPositiveSetting pins the guard: a non-positive
// stored poll_interval_sec (only reachable by editing the row) keeps the last
// known cadence and is reported once, not once per tick — the supervisor, not
// the loop, owns "disabled".
func TestLoopKeepsIntervalOnNonPositiveSetting(t *testing.T) {
	src := singleMovieRadarr()
	logs := &logCapture{}
	r, p, tr := loopRig(t, 300*time.Second,
		func(baseURL, apiKey string) (RadarrSource, error) { return &src, nil }, logs.logf)
	waitForFirstPass(t, r)

	r.rawPollInterval(0)
	tr.ch <- time.Now()
	logs.waitFor(t, "not positive")

	// A negative value and further ticks change neither the cadence nor the
	// number of warnings.
	r.rawPollInterval(-5)
	tr.ch <- time.Now()
	tr.ch <- time.Now()
	time.Sleep(200 * time.Millisecond)

	if got := p.Interval(); got != 300*time.Second {
		t.Errorf("Interval() = %v, want the previous 300s", got)
	}
	select {
	case got := <-tr.resets:
		t.Errorf("ticker rebuilt to %v for a non-positive interval", got)
	default:
	}
	if n := logs.count("not positive"); n != 1 {
		t.Errorf("non-positive interval logged %d times, want once", n)
	}
}

// TestReadPollIntervalReportsReadError pins the signal the loop keeps its
// cadence on: an unreadable section reports an error instead of a zero
// duration, so "unknown right now" is never mistaken for "disabled".
func TestReadPollIntervalReportsReadError(t *testing.T) {
	r := newRig(t)
	p := r.newPoller(time.Minute)
	if err := r.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if _, found, err := p.readPollInterval(context.Background()); err == nil {
		t.Fatalf("readPollInterval on an unreadable store = found %v, want an error", found)
	}
}

// gatedRadarr lets the first listing through and blocks every later one until
// the test releases it, so a manual pass can hold the shared slot while the
// loop keeps ticking.
type gatedRadarr struct {
	fakeRadarr
	mu      sync.Mutex
	calls   int
	second  chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedRadarr() *gatedRadarr {
	return &gatedRadarr{
		fakeRadarr: singleMovieRadarr(),
		second:     make(chan struct{}),
		release:    make(chan struct{}),
	}
}

func (g *gatedRadarr) Movies(ctx context.Context) ([]arr.Movie, error) {
	g.mu.Lock()
	g.calls++
	call := g.calls
	g.mu.Unlock()
	if call == 1 {
		return g.fakeRadarr.Movies(ctx)
	}
	g.once.Do(func() { close(g.second) })
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.fakeRadarr.Movies(ctx)
}

// TestSkippedPassRetriesWithLiveInterval pins the retry path: a skipped tick
// re-attempts after min(current interval, maxSkipRetryDelay), and "current" is
// the value the loop just read, not the cadence captured at boot. The boot 5
// min would cap the retry at 30 s, so only the live 1 s explains a second
// attempt inside the deadline.
func TestSkippedPassRetriesWithLiveInterval(t *testing.T) {
	gated := newGatedRadarr()
	logs := &logCapture{}
	r, p, tr := loopRig(t, 5*time.Minute,
		func(baseURL, apiKey string) (RadarrSource, error) { return gated, nil }, logs.logf)
	waitForFirstPass(t, r)
	t.Cleanup(func() { close(gated.release) })

	// A manual pass now owns the shared slot, so the next tick must skip.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := p.Trigger(context.Background(), "radarr")
		if err == nil {
			break
		}
		if !errors.Is(err, ErrSyncBusy) || time.Now().After(deadline) {
			t.Fatalf("Trigger: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-gated.second:
	case <-time.After(testPassTimeout):
		t.Fatal("the manual pass never reached the arr listing")
	}

	// 1 s is below Set's 30 s floor, so it stands in for a hand-edited row.
	r.rawPollInterval(1)
	tr.ch <- time.Now()

	deadline = time.Now().Add(5 * time.Second)
	for logs.count("skipping scheduled pass") < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := logs.count("skipping scheduled pass"); got < 2 {
		t.Fatalf("skipped passes = %d, want at least 2: the retry did not use the live 1s interval", got)
	}
}

// TestTriggerRejectsWhileScheduledPassRuns pins the shared slot: while the
// loop's own pass is in flight a manual trigger still reports ErrSyncBusy
// instead of queueing a second pass.
func TestTriggerRejectsWhileScheduledPassRuns(t *testing.T) {
	src := newBlockingRadarr()
	_, p, _ := loopRig(t, time.Minute,
		func(baseURL, apiKey string) (RadarrSource, error) { return src, nil }, nil)
	src.waitForList(t)

	if err := p.Trigger(context.Background(), "radarr"); !errors.Is(err, ErrSyncBusy) {
		t.Fatalf("Trigger during a scheduled pass = %v, want ErrSyncBusy", err)
	}
	if got := src.listCalls(); got != 1 {
		t.Errorf("arr listings = %d, want 1: a refused trigger must not reach arr", got)
	}
	close(src.release)
}
