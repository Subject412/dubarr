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
// Tags and root folders ride the same pass, so an arr tag edit becomes visible
// to eligibility within one interval — at most 300 s (ADR-0009).
const DefaultInterval = 300 * time.Second

// ErrMissingAPIKey reports an enabled instance whose API key is not in the
// secret store. The pass skips that instance and keeps its cache.
var ErrMissingAPIKey = errors.New("arrsync: arr instance has no API key")

// ErrSyncBusy reports that a pass (scheduled or manual) already owns the
// poller. A second pass is never queued: the caller gets this sentinel instead.
var ErrSyncBusy = errors.New("arrsync: a sync pass is already running")

// ErrPollerStopped rejects a trigger that arrives before Start or after Stop:
// shutdown wins, and there is no cancellable loop context to run a pass on.
var ErrPollerStopped = errors.New("arrsync: poller is stopped")

// maxSkipRetryDelay bounds how long a skipped scheduled tick waits before it
// re-attempts. A skipped tick used to wait a full interval, so an admin who
// repeatedly refreshed one kind could defer the other kind's pass forever.
const maxSkipRetryDelay = 30 * time.Second

// syncFailureWriteTimeout bounds the detached health UPDATE: the write runs
// after a cancelled pass, so it needs its own short deadline instead of an
// unbounded context.
const syncFailureWriteTimeout = 5 * time.Second

// tagSource is the /api/v3/tag read both products provide.
type tagSource interface {
	Tags(ctx context.Context) ([]arr.Tag, error)
}

// rootFolderSource is the /api/v3/rootfolder read both products provide.
type rootFolderSource interface {
	RootFolders(ctx context.Context) ([]arr.RootFolder, error)
}

// RadarrSource is the Radarr read surface the poller needs; *arr.RadarrClient
// satisfies it.
type RadarrSource interface {
	tagSource
	rootFolderSource
	Movies(ctx context.Context) ([]arr.Movie, error)
}

// SonarrSource is the Sonarr read surface the poller needs; *arr.SonarrClient
// satisfies it.
type SonarrSource interface {
	tagSource
	rootFolderSource
	Series(ctx context.Context) ([]arr.Series, error)
	Episodes(ctx context.Context, seriesID int) ([]arr.Episode, error)
}

// LibraryStore is the persistence seam; *store.Store satisfies it.
type LibraryStore interface {
	ArrInstanceByKind(ctx context.Context, kind string) (store.ArrInstanceSyncRow, bool, error)
	SaveArrInstance(ctx context.Context, row store.ArrInstanceSyncRow) (int64, error)
	TouchArrInstanceSync(ctx context.Context, id int64) error
	// RecordArrInstanceSyncFailure stores why the last attempt failed,
	// leaving the last successful pass and the cached snapshot alone. The
	// cause must already be scrubbed of credentials (the arr clients do
	// that); the store redacts and caps it again before persisting.
	RecordArrInstanceSyncFailure(ctx context.Context, id int64, cause string) error
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
	// runCtx is the loop's context (nil before Start). A manual pass runs on
	// it, not on the caller's request context, so Stop cancels the pass and
	// Wait drains it.
	runCtx context.Context
	// inFlight is the single pass slot shared by the loop and Trigger.
	inFlight bool
	wg       sync.WaitGroup
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

// HasEnabledInstance reports whether arr.connections holds at least one enabled
// instance with a base URL, i.e. whether a pass has anything to do. It lets the
// library API tell "no instance configured" from "poller running, nothing
// enabled". An unreadable section reports true so the caller surfaces the real
// error instead of claiming no instance exists.
func (p *Poller) HasEnabledInstance(ctx context.Context) bool {
	conns, err := p.connections(ctx)
	if err != nil {
		return true
	}
	for _, conn := range conns {
		if conn.Enabled && strings.TrimSpace(conn.BaseURL) != "" {
			return true
		}
	}
	return false
}

// Sync performs one pass over every enabled connection. It returns the
// aggregated stats and a joined error; a failing instance never stops the rest
// and never touches its own previous cache rows.
func (p *Poller) Sync(ctx context.Context) (Stats, error) {
	return p.syncKind(ctx, "")
}

// syncKind is Sync restricted to one instance kind ("" = all). Both the
// scheduled loop and a manual Trigger go through it, so a manual pass records
// health exactly like a scheduled one.
func (p *Poller) syncKind(ctx context.Context, kind string) (Stats, error) {
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
		if kind != "" && conn.Kind != kind {
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
// one broken instance cannot abort the pass, and any failure is recorded as
// sync health so the Library screen can show why the last attempt failed
// without losing the previous snapshot.
func (p *Poller) syncInstance(ctx context.Context, rule gate.Rule, conn settings.ArrConnection, stats *Stats) (err error) {
	// Resolve the instance id BEFORE anything that can fail: the most common
	// failure (a missing or unreadable API key) happens before ensureInstance
	// runs, and without an id the health write below would be skipped,
	// leaving a stale green row forever. A missing row or a failed lookup is
	// not itself a pass error — ensureInstance still reports the real cause —
	// but it is logged so a silent skip is visible.
	var instanceID int64
	if row, ok, lerr := p.store.ArrInstanceByKind(ctx, conn.Kind); lerr != nil {
		p.logf("arrsync: %s: look up arr instance for health: %v", conn.Kind, lerr)
	} else if !ok {
		p.logf("arrsync: %s: no arr_instances row yet, a failure cannot be recorded", conn.Kind)
	} else {
		instanceID = row.ID
	}
	// Declared before the recover, so it runs after it and sees the final
	// error (including a recovered panic). Health is written on a context
	// detached from cancellation, because the pass may already be aborted,
	// but bounded: a shutting-down process must not hang on the UPDATE.
	defer func() {
		if err == nil || instanceID <= 0 {
			return
		}
		// A graceful shutdown cancels ctx mid-pass. That is not a sync
		// failure, so health stays untouched instead of turning the
		// instance red until the next pass. A genuine per-request timeout
		// (context.DeadlineExceeded) is a different sentinel and is still
		// recorded as a failure.
		if errors.Is(err, context.Canceled) {
			p.logf("arrsync: %s: pass cancelled, health left unchanged: %v", conn.Kind, err)
			return
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncFailureWriteTimeout)
		defer cancel()
		if ferr := p.store.RecordArrInstanceSyncFailure(writeCtx, instanceID, err.Error()); ferr != nil {
			p.logf("arrsync: %s: record sync failure: %v", conn.Kind, ferr)
		}
	}()
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

	instanceID, err = p.ensureInstance(ctx, conn, apiKey)
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
		// A failed tag/root-folder read fails the instance so the previous
		// snapshot stays; caching empty metadata would silently change
		// eligibility once the gate consumes it (ADR-0009).
		labels, roots, err := p.arrMetadata(ctx, client, conn.Kind)
		if err != nil {
			return nil, err
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
			arrIDs, arrLabels := resolveArrTags(movie.Tags, labels)
			root := rootFolderFor(movie.Path, roots)
			status, matches := classifyTarget(rule, target, gate.ArrMetadata{
				Found:      true,
				Tags:       arrLabels,
				RootFolder: root,
			})
			items = append(items, store.LibraryCacheItem{
				MediaType:            "movie",
				ExternalItemID:       int64(movie.ID),
				TMDBID:               int64(movie.TmdbID),
				Title:                movie.Title,
				OriginalTitle:        movie.OriginalTitle,
				OriginalLanguageISO2: probe.ISO639_1(movie.OriginalLanguage.Name),
				Year:                 movie.Year,
				MediaFolderPath:      movie.Path,
				RootFolderPath:       root,
				ArrTagIDs:            arrIDs,
				ArrTagLabels:         arrLabels,
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
		labels, roots, err := p.arrMetadata(ctx, client, conn.Kind)
		if err != nil {
			return nil, err
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
			// Sonarr tags live on the series; episodes inherit them.
			arrIDs, arrLabels := resolveArrTags(show.Tags, labels)
			root := rootFolderFor(show.Path, roots)
			for _, episode := range episodes {
				target := ""
				if episode.HasFile && episode.EpisodeFile != nil {
					target = episode.EpisodeFile.Path
				}
				status, matches := classifyTarget(rule, target, gate.ArrMetadata{
					Found:      true,
					Tags:       arrLabels,
					RootFolder: root,
				})
				season, number := episode.SeasonNumber, episode.EpisodeNumber
				items = append(items, store.LibraryCacheItem{
					MediaType:            "episode",
					ExternalItemID:       int64(episode.ID),
					SeriesID:             int64(show.ID),
					TVDBID:               int64(show.TvdbID),
					Title:                show.Title,
					OriginalTitle:        show.OriginalTitle,
					OriginalLanguageISO2: lang,
					Year:                 show.Year,
					SeasonNumber:         &season,
					EpisodeNumber:        &number,
					EpisodeTitle:         episode.Title,
					MediaFolderPath:      show.Path,
					RootFolderPath:       root,
					ArrTagIDs:            arrIDs,
					ArrTagLabels:         arrLabels,
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

// arrMetadataSource is what one pass needs beyond the library listing.
type arrMetadataSource interface {
	tagSource
	rootFolderSource
}

// arrMetadata reads the instance's tag list and root folders for one pass. Tags
// are resolved to labels here, next to the library listing, so a label rename
// or deletion is reflected by the same sync that refreshes the items.
func (p *Poller) arrMetadata(ctx context.Context, src arrMetadataSource, kind string) (map[int]string, []arr.RootFolder, error) {
	tags, err := src.Tags(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("arrsync: list %s tags: %w", kind, err)
	}
	roots, err := src.RootFolders(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("arrsync: list %s root folders: %w", kind, err)
	}
	return tagLabels(tags), roots, nil
}

// tagLabels indexes one instance's tags by id.
func tagLabels(tags []arr.Tag) map[int]string {
	out := make(map[int]string, len(tags))
	for _, tag := range tags {
		out[int(tag.ID)] = tag.Label
	}
	return out
}

// resolveArrTags turns arr tag ids into a paired id/label snapshot. Ids the
// instance no longer lists are dropped: keeping an unresolvable id would record
// a tag that no longer exists, and ADR-0009 matches by label.
func resolveArrTags(ids []int, labels map[int]string) ([]int64, []string) {
	tagIDs := make([]int64, 0, len(ids))
	tagLabels := make([]string, 0, len(ids))
	for _, id := range ids {
		label, ok := labels[id]
		if !ok {
			continue
		}
		tagIDs = append(tagIDs, int64(id))
		tagLabels = append(tagLabels, label)
	}
	return tagIDs, tagLabels
}

// rootFolderFor picks the longest configured root folder containing
// mediaFolder (canonical arr path, ADR-0009). Paths are compared as arr
// reports them, so a root with a trailing slash still matches.
func rootFolderFor(mediaFolder string, roots []arr.RootFolder) string {
	best := ""
	for _, root := range roots {
		path := strings.TrimRight(root.Path, "/")
		if path == "" {
			continue
		}
		if mediaFolder == path || strings.HasPrefix(mediaFolder, path+"/") {
			if len(path) > len(best) {
				best = path
			}
		}
	}
	return best
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
	p.runCtx = ctx
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

	// retry carries a skipped pass back to the loop after a short delay
	// instead of making it wait a whole interval. It is buffered and the
	// timer below is reused, so repeated skips never stack up.
	retry := make(chan struct{}, 1)
	var retryTimer *time.Timer
	defer func() {
		if retryTimer != nil {
			retryTimer.Stop()
		}
	}()
	retryIn := func(delay time.Duration) {
		if retryTimer == nil {
			retryTimer = time.AfterFunc(delay, func() {
				// A timer that fires while Stop is running must not keep
				// the pass alive: the loop has already left.
				if ctx.Err() != nil {
					return
				}
				select {
				case retry <- struct{}{}:
				default:
				}
			})
			return
		}
		retryTimer.Reset(delay)
	}
	clearRetry := func() {
		if retryTimer != nil {
			retryTimer.Stop()
		}
		// A retry that already fired must not spin the loop into one extra
		// immediate pass.
		select {
		case <-retry:
		default:
		}
	}

	for {
		if p.syncPass(ctx) {
			clearRetry()
		} else {
			retryIn(min(p.interval, maxSkipRetryDelay))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-retry:
		}
	}
}

// syncPass runs one pass, swallowing panics: the loop must outlive a broken
// pass. Errors are already logged per instance by Sync.
//
// A pass is exclusive. When a manual Trigger already owns the slot this tick
// is skipped rather than queued: the caller re-attempts shortly instead of
// waiting the full interval, so one busy kind cannot starve the others, and
// the two passes can never race on the same cache rows. The return value
// reports whether the pass actually ran.
func (p *Poller) syncPass(ctx context.Context) bool {
	if !p.beginPass() {
		p.logf("arrsync: debug: skipping scheduled pass, a sync pass is already running")
		return false
	}
	defer p.endPass()
	defer func() {
		if r := recover(); r != nil {
			p.logf("arrsync: pass panic: %v", r)
		}
	}()
	_, _ = p.Sync(ctx)
	return true
}

// beginPass takes the poller's single pass slot. false means a pass already
// owns it, so the caller skips instead of queueing.
func (p *Poller) beginPass() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inFlight {
		return false
	}
	p.inFlight = true
	return true
}

// endPass releases the slot. It must run exactly once per successful
// beginPass or per Trigger.
func (p *Poller) endPass() {
	p.mu.Lock()
	p.inFlight = false
	p.mu.Unlock()
}

// Trigger starts one manual pass over a single instance kind ("" = every
// enabled connection) and returns immediately; the pass runs in a detached
// goroutine on the poller's own context, so Stop cancels it and Wait drains it.
//
// The slot is shared with the scheduled loop: a manual pass makes the loop
// skip its tick, and a Trigger while any pass runs reports ErrSyncBusy instead
// of queueing. A poller that has not been started (or has been stopped)
// refuses with ErrPollerStopped: there is no cancellable loop context, so the
// pass could never be drained.
func (p *Poller) Trigger(ctx context.Context, kind string) error {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return ErrPollerStopped
	}
	if p.inFlight {
		p.mu.Unlock()
		return ErrSyncBusy
	}
	runCtx := p.runCtx
	if runCtx == nil {
		// Not started yet (or never Start-ed): a pass on a background
		// context could not be cancelled by Stop, so refuse instead.
		p.mu.Unlock()
		return ErrPollerStopped
	}
	p.inFlight = true
	// Add under the same lock Stop takes before Wait, so a trigger racing a
	// shutdown is either drained or refused — never a stray goroutine.
	p.wg.Add(1)
	p.mu.Unlock()

	go func() {
		defer p.wg.Done()
		defer p.endPass()
		defer func() {
			if r := recover(); r != nil {
				p.logf("arrsync: manual pass panic: %v", r)
			}
		}()
		if _, err := p.syncKind(runCtx, kind); err != nil {
			p.logf("arrsync: manual pass (%s): %v", kindOrAll(kind), err)
		}
	}()
	return nil
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

// kindOrAll labels a manual pass in logs.
func kindOrAll(kind string) string {
	if kind == "" {
		return "all"
	}
	return kind
}
