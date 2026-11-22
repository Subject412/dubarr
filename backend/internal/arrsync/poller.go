package arrsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arr"
	"github.com/Subject412/dubarr/backend/internal/gate"
	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/secrets"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// DefaultInterval is the settled *arr poll cadence (PLAN.md settled defaults:
// "arr sync manual + 5-min poll"; settings arr.options.poll_interval_sec = 300).
const DefaultInterval = 300 * time.Second

// ErrMissingAPIKey reports an enabled instance whose API key is not in the
// secret store. The pass skips that instance and keeps its cache.
var ErrMissingAPIKey = errors.New("arrsync: arr instance has no API key")

// RadarrSource is the Radarr read surface the poller needs; *arr.RadarrClient
// satisfies it.
type RadarrSource interface {
	Movies(ctx context.Context) ([]arr.Movie, error)
}

// SonarrSource is the Sonarr read surface the poller needs; *arr.SonarrClient
// satisfies it.
type SonarrSource interface {
	Series(ctx context.Context) ([]arr.Series, error)
	Episodes(ctx context.Context, seriesID int) ([]arr.Episode, error)
}

// LibraryStore is the persistence seam; *store.Store satisfies it.
type LibraryStore interface {
	ArrInstanceByKind(ctx context.Context, kind string) (store.ArrInstanceSyncRow, bool, error)
	SaveArrInstance(ctx context.Context, row store.ArrInstanceSyncRow) (int64, error)
	TouchArrInstanceSync(ctx context.Context, id int64) error
	SyncLibraryCache(ctx context.Context, instanceID int64, items []store.LibraryCacheItem) (store.LibrarySyncStats, error)
}

// SecretStore is the subset of secrets.Service the poller uses: read the
// configured key and re-encrypt it for arr_instances.
type SecretStore interface {
	Get(key string) (string, error)
	EncryptString(plaintext string) ([]byte, error)
	DecryptString(blob []byte) (string, error)
}

// Deps configure a Poller. Store, Settings and Secrets are required.
type Deps struct {
	Store    LibraryStore
	Settings *settings.Store
	Secrets  SecretStore
	// Interval overrides DefaultInterval; <= 0 selects DefaultInterval. The
	// composition root disables the poller instead of starting a 0 interval.
	Interval time.Duration
	// Timeout bounds one arr HTTP request; 0 selects arr.DefaultTimeout.
	Timeout time.Duration
	// Logf reports pass failures. Nil means log.Printf.
	Logf func(format string, args ...any)
	// NewRadarr/NewSonarr build the clients; nil uses the M2 arr clients.
	NewRadarr func(baseURL, apiKey string) (RadarrSource, error)
	NewSonarr func(baseURL, apiKey string) (SonarrSource, error)
}

// Stats summarises one pass. LibrarySyncStats is embedded so callers can add
// per-instance results.
type Stats struct {
	// Instances is how many enabled connections were attempted.
	Instances int
	// Failed is how many of them errored (their cache is untouched).
	Failed int
	store.LibrarySyncStats
}

// Poller runs library sync passes on the configured cadence.
type Poller struct {
	store    LibraryStore
	settings *settings.Store
	secrets  SecretStore
	interval time.Duration
	timeout  time.Duration
	logf     func(format string, args ...any)
	radarr   func(baseURL, apiKey string) (RadarrSource, error)
	sonarr   func(baseURL, apiKey string) (SonarrSource, error)

	mu      sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New builds a Poller. It fails closed when a collaborator is missing so a
// mis-wired composition root is caught at boot.
func New(deps Deps) (*Poller, error) {
	if deps.Store == nil {
		return nil, errors.New("arrsync: library store is required")
	}
	if deps.Settings == nil {
		return nil, errors.New("arrsync: settings store is required")
	}
	if deps.Secrets == nil {
		return nil, errors.New("arrsync: secret store is required")
	}
	interval := deps.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	timeout := deps.Timeout
	if timeout <= 0 {
		timeout = arr.DefaultTimeout
	}
	logf := deps.Logf
	if logf == nil {
		logf = log.Printf
	}
	p := &Poller{
		store:    deps.Store,
		settings: deps.Settings,
		secrets:  deps.Secrets,
		interval: interval,
		timeout:  timeout,
		logf:     logf,
		radarr:   deps.NewRadarr,
		sonarr:   deps.NewSonarr,
	}
	if p.radarr == nil {
		p.radarr = func(baseURL, apiKey string) (RadarrSource, error) {
			return arr.NewRadarrClient(baseURL, apiKey, timeout)
		}
	}
	if p.sonarr == nil {
		p.sonarr = func(baseURL, apiKey string) (SonarrSource, error) {
			return arr.NewSonarrClient(baseURL, apiKey, timeout)
		}
	}
	return p, nil
}

// Interval reports the pass cadence.
func (p *Poller) Interval() time.Duration { return p.interval }

// Sync performs one pass over every enabled connection. It returns the
// aggregated stats and a joined error; a failing instance never stops the rest
// and never touches its own previous cache rows.
func (p *Poller) Sync(ctx context.Context) (Stats, error) {
	rule, err := p.gateRule(ctx)
	if err != nil {
		return Stats{}, err
	}
	conns, err := p.connections(ctx)
	if err != nil {
		return Stats{}, err
	}

	var stats Stats
	var errs []error
	for _, conn := range conns {
		if !conn.Enabled || strings.TrimSpace(conn.BaseURL) == "" {
			continue
		}
		stats.Instances++
		if err := p.syncInstance(ctx, rule, conn, &stats); err != nil {
			stats.Failed++
			errs = append(errs, err)
			p.logf("arrsync: %s: %v", conn.Kind, err)
		}
	}
	return stats, errors.Join(errs...)
}

// syncInstance lists and caches one connection. A panic is contained here so
// one broken instance cannot abort the pass.
func (p *Poller) syncInstance(ctx context.Context, rule gate.Rule, conn settings.ArrConnection, stats *Stats) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("arrsync: %s sync panic: %v", conn.Kind, r)
		}
	}()

	apiKey, err := p.secrets.Get(secretKey(conn.Kind))
	if err != nil {
		if errors.Is(err, secrets.ErrNotFound) {
			return fmt.Errorf("%w: %s", ErrMissingAPIKey, conn.Kind)
		}
		return fmt.Errorf("arrsync: read %s API key: %w", conn.Kind, err)
	}

	instanceID, err := p.ensureInstance(ctx, conn, apiKey)
	if err != nil {
		return err
	}

	items, err := p.listItems(ctx, conn, apiKey, rule)
	if err != nil {
		return err
	}

	stats2, err := p.store.SyncLibraryCache(ctx, instanceID, items)
	if err != nil {
		return err
	}
	stats.Add(stats2)
	if err := p.store.TouchArrInstanceSync(ctx, instanceID); err != nil {
		return err
	}
	return nil
}

// ensureInstance returns the arr_instances id for conn, creating or updating the
// row so arr_library_cache's foreign key always resolves. The stored key is
// re-encrypted only when it actually differs, so a stable key does not rewrite
// the row on every pass.
func (p *Poller) ensureInstance(ctx context.Context, conn settings.ArrConnection, apiKey string) (int64, error) {
	row, ok, err := p.store.ArrInstanceByKind(ctx, conn.Kind)
	if err != nil {
		return 0, err
	}
	if !ok {
		enc, err := p.secrets.EncryptString(apiKey)
		if err != nil {
			return 0, fmt.Errorf("arrsync: encrypt %s API key: %w", conn.Kind, err)
		}
		return p.store.SaveArrInstance(ctx, store.ArrInstanceSyncRow{
			Name:        displayName(conn.Kind),
			Kind:        conn.Kind,
			BaseURL:     conn.BaseURL,
			APIKeyEnc:   enc,
			APIKeyLast4: secrets.Last4(apiKey),
			Enabled:     true,
		})
	}

	changed := false
	if strings.TrimSpace(row.Name) == "" {
		row.Name = displayName(conn.Kind)
		changed = true
	}
	if row.BaseURL != conn.BaseURL {
		row.BaseURL = conn.BaseURL
		changed = true
	}
	if !row.Enabled {
		// settings arr.connections is the operator's switch; a cached row
		// must follow it so M5-4 rescans are not skipped as "disabled".
		row.Enabled = true
		changed = true
	}
	if plain, derr := p.secrets.DecryptString(row.APIKeyEnc); derr != nil || plain != apiKey {
		enc, eerr := p.secrets.EncryptString(apiKey)
		if eerr != nil {
			return 0, fmt.Errorf("arrsync: encrypt %s API key: %w", conn.Kind, eerr)
		}
		row.APIKeyEnc = enc
		row.APIKeyLast4 = secrets.Last4(apiKey)
		changed = true
	}
	if !changed {
		return row.ID, nil
	}
	return p.store.SaveArrInstance(ctx, row)
}

// listItems lists one instance and turns it into cache items.
func (p *Poller) listItems(ctx context.Context, conn settings.ArrConnection, apiKey string, rule gate.Rule) ([]store.LibraryCacheItem, error) {
	switch conn.Kind {
	case "radarr":
		client, err := p.radarr(conn.BaseURL, apiKey)
		if err != nil {
			return nil, fmt.Errorf("arrsync: %w", err)
		}
		movies, err := client.Movies(ctx)
		if err != nil {
			return nil, fmt.Errorf("arrsync: list radarr movies: %w", err)
		}
		items := make([]store.LibraryCacheItem, 0, len(movies))
		for _, movie := range movies {
			target := ""
			if movie.HasFile && movie.MovieFile != nil {
				target = movie.MovieFile.Path
			}
			status, matches := classifyTarget(rule, target)
			items = append(items, store.LibraryCacheItem{
				MediaType:            "movie",
				ExternalItemID:       int64(movie.ID),
				TMDBID:               int64(movie.TmdbID),
				Title:                movie.Title,
				OriginalLanguageISO2: probe.ISO639_1(movie.OriginalLanguage.Name),
				Year:                 movie.Year,
				MediaFolderPath:      movie.Path,
				TargetFilePath:       target,
				MatchesTargetTags:    matches,
				EligibilityStatus:    status,
			})
		}
		return items, nil
	case "sonarr":
		client, err := p.sonarr(conn.BaseURL, apiKey)
		if err != nil {
			return nil, fmt.Errorf("arrsync: %w", err)
		}
		series, err := client.Series(ctx)
		if err != nil {
			return nil, fmt.Errorf("arrsync: list sonarr series: %w", err)
		}
		items := make([]store.LibraryCacheItem, 0, len(series))
		for _, show := range series {
			episodes, err := client.Episodes(ctx, show.ID)
			if err != nil {
				// Never delete a series' cache on a partial listing: abort the
				// whole instance so its snapshot stays the previous one.
				return nil, fmt.Errorf("arrsync: list sonarr episodes for series %d: %w", show.ID, err)
			}
			lang := probe.ISO639_1(show.OriginalLanguage.Name)
			for _, episode := range episodes {
				target := ""
				if episode.HasFile && episode.EpisodeFile != nil {
					target = episode.EpisodeFile.Path
				}
				status, matches := classifyTarget(rule, target)
				season, number := episode.SeasonNumber, episode.EpisodeNumber
				items = append(items, store.LibraryCacheItem{
					MediaType:            "episode",
					ExternalItemID:       int64(episode.ID),
					SeriesID:             int64(show.ID),
					TVDBID:               int64(show.TvdbID),
					Title:                show.Title,
					OriginalLanguageISO2: lang,
					Year:                 show.Year,
					SeasonNumber:         &season,
					EpisodeNumber:        &number,
					EpisodeTitle:         episode.Title,
					MediaFolderPath:      show.Path,
					TargetFilePath:       target,
					MatchesTargetTags:    matches,
					EligibilityStatus:    status,
				})
			}
		}
		return items, nil
	default:
		return nil, fmt.Errorf("arrsync: unknown arr kind %q", conn.Kind)
	}
}

// connections reads settings arr.connections.
func (p *Poller) connections(ctx context.Context) ([]settings.ArrConnection, error) {
	raw, ok, err := p.settings.Get(ctx, settings.KeyArrConnections)
	if err != nil {
		return nil, fmt.Errorf("arrsync: read %s: %w", settings.KeyArrConnections, err)
	}
	if !ok {
		return nil, nil
	}
	var conns settings.ArrConnections
	if err := json.Unmarshal(raw, &conns); err != nil {
		return nil, fmt.Errorf("arrsync: decode %s: %w", settings.KeyArrConnections, err)
	}
	return conns.Instances, nil
}

// gateRule reads settings target.tag_filter, falling back to the seeded default
// when the section is absent.
func (p *Poller) gateRule(ctx context.Context) (gate.Rule, error) {
	raw, ok, err := p.settings.Get(ctx, settings.KeyTargetTagFilter)
	if err != nil {
		return gate.Rule{}, fmt.Errorf("arrsync: read %s: %w", settings.KeyTargetTagFilter, err)
	}
	if !ok {
		tf, _ := settings.Defaults("")[settings.KeyTargetTagFilter].(settings.TargetTagFilter)
		return gate.RuleFromTagFilter(tf), nil
	}
	var tf settings.TargetTagFilter
	if err := json.Unmarshal(raw, &tf); err != nil {
		return gate.Rule{}, fmt.Errorf("arrsync: decode %s: %w", settings.KeyTargetTagFilter, err)
	}
	return gate.RuleFromTagFilter(tf), nil
}

// Start launches the poll loop. The first pass runs immediately, then one per
// interval. It is idempotent and a no-op after Stop.
func (p *Poller) Start(ctx context.Context) {
	p.mu.Lock()
	if p.started || p.stopped {
		p.mu.Unlock()
		return
	}
	p.started = true
	ctx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.wg.Add(1)
	p.mu.Unlock()

	go func() {
		defer p.wg.Done()
		p.run(ctx)
	}()
}

func (p *Poller) run(ctx context.Context) {
	if p.interval <= 0 {
		// Defensive: New never returns a non-positive interval, but a 0
		// ticker would panic and kill the process.
		return
	}
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		p.syncPass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// syncPass runs one pass and swallows panics: the loop must outlive a broken
// pass. Errors are already logged per instance by Sync.
func (p *Poller) syncPass(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			p.logf("arrsync: pass panic: %v", r)
		}
	}()
	_, _ = p.Sync(ctx)
}

// Stop cancels the loop and waits for it to exit. Safe to call more than once
// and without Start.
func (p *Poller) Stop() {
	p.mu.Lock()
	p.stopped = true
	cancel := p.cancel
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	p.Wait()
}

// Wait blocks until the loop goroutine (if any) has exited.
func (p *Poller) Wait() { p.wg.Wait() }

// secretKey names the encrypted secret one instance kind stores its key under
// (same layout as the arr settings API).
func secretKey(kind string) string { return "arr." + kind + ".api_key" }

// displayName is the default arr_instances.name for a kind.
func displayName(kind string) string {
	if kind == "" {
		return ""
	}
	return strings.ToUpper(kind[:1]) + kind[1:]
}
