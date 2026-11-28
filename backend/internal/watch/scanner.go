// Package watch scans the configured media roots for newly arrived donor
// video files and registers them in the approval queue.
//
// The scanner is deliberately conservative about paths: every root is
// canonicalized with filepath.EvalSymlinks at construction time, and every
// discovered file is re-resolved and must remain inside one of those roots
// (AGENTS.md path rule). A file that escapes its root is reported as an error
// and skipped rather than recorded.
package watch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultInterval is the fallback watch-folder cadence for a stored interval of
// exactly 0 (settings validation rejects 0, so a fresh install runs at
// settings.KeyWatchOptions' default) and the number the UI advertises. It is
// deliberately equal to settings.Defaults()[settings.KeyWatchOptions]
// .ScanIntervalSec so the fallback and the seeded default can never disagree;
// TestDefaultIntervalMatchesSettingsDefault pins the pair.
const DefaultInterval = 60 * time.Second

// DefaultStabilityWindow is the settled stability window (settings
// watch.options.stability_check_interval_sec default = 4, validated >= 4).
const DefaultStabilityWindow = 4 * time.Second

// OptionsProvider supplies the live watch cadence and stability window. The
// run loop re-reads it once per iteration, so a settings save takes effect
// without a restart. Implementations are called from the scan-loop goroutine
// and must be safe for that.
//
// An error means "unknown right now": the scanner keeps the last value it
// read instead of stopping or reverting to a default.
type OptionsProvider interface {
	ScanInterval() (time.Duration, error)
	StabilityWindow() (time.Duration, error)
}

// scanTicker abstracts time.Ticker so tests can drive the run loop by hand
// instead of waiting on wall-clock periods.
type scanTicker interface {
	Chan() <-chan time.Time
	Reset(time.Duration)
	Stop()
}

// realTicker is the production scanTicker.
type realTicker struct{ ticker *time.Ticker }

func newRealTicker(d time.Duration) scanTicker { return realTicker{ticker: time.NewTicker(d)} }

func (t realTicker) Chan() <-chan time.Time { return t.ticker.C }
func (t realTicker) Reset(d time.Duration)  { t.ticker.Reset(d) }
func (t realTicker) Stop()                  { t.ticker.Stop() }

// ErrScanInProgress is returned when Scan is called while a previous scan is
// still running. The overlapping call is skipped, not queued.
var ErrScanInProgress = errors.New("watch: scan already in progress")

// mediaExtensions is the set of donor video containers the scanner enqueues.
// The watch contract names .mkv explicitly; the remaining common containers
// are accepted so a donor drop is never silently ignored. Matching is
// case-insensitive.
var mediaExtensions = map[string]bool{
	".mkv":  true,
	".mp4":  true,
	".m4v":  true,
	".avi":  true,
	".m2ts": true,
	".ts":   true,
	".mov":  true,
	".wmv":  true,
	".webm": true,
	".mpg":  true,
	".mpeg": true,
	".flv":  true,
}

// partialSuffixes are in-progress download/processing artefacts that must not
// be enqueued (the watch contract). Lower-case comparison.
var partialSuffixes = []string{".!qb", ".part", ".tmp", ".crdownload", ".tdarr"}

// CandidateStore persists a discovered donor file. *store.Store satisfies it.
// created reports whether the file was new to the queue.
type CandidateStore interface {
	RecordImportCandidate(ctx context.Context, sourcePath, filename string, size int64) (created bool, err error)
}

// CandidateHook is invoked once for every newly recorded candidate. It lets
// the composition root classify the file (Processing Gate) without making the
// scanner depend on the gate or the store's gate columns.
//
// The hook runs after the row is committed, so a hook failure never drops the
// candidate; the error is collected in Result.Errors and returned aggregated
// from Scan, exactly like a per-file record error. A panic is recovered and
// treated the same way, so a bad hook cannot kill the scan loop.
type CandidateHook func(ctx context.Context, sourcePath, filename string, size int64) error

// Result summarises one scan pass.
type Result struct {
	// Seen is the number of media files that passed extension/root checks.
	Seen int
	// Added is the number of candidates newly recorded in the store.
	Added int
	// Errors holds non-fatal per-file/per-root failures. A scan with errors
	// still processes every other file.
	Errors []error
}

// observation is one candidate's last size/mtime reading, held in memory and
// keyed by scanned path. since is when that exact size/mtime pair was first
// seen. There is deliberately no "already recorded" flag: the store's UNIQUE
// source_file_path row is the single source of truth for "queued", so a
// settled file is re-offered on every pass and a deleted queue row is
// re-created instead of stranding the file.
type observation struct {
	size    int64
	modTime time.Time
	since   time.Time
}

// Scanner periodically walks the configured roots for new media files.
type Scanner struct {
	store    CandidateStore
	roots    []string
	provider OptionsProvider
	// interval and stability are the last values the loop read from the
	// provider, guarded by mu. now is the clock used for stability
	// observations (time.Now in production); newTicker builds the loop's
	// ticker (time.NewTicker in production). Tests replace the last two before
	// Start.
	interval  time.Duration
	stability time.Duration
	now       func() time.Time
	newTicker func(time.Duration) scanTicker
	// observations holds the in-flight stability readings, guarded by obsMu.
	// Entries are pruned for paths that disappear from the roots.
	observations map[string]observation
	// hook is the optional post-record callback, guarded by mu.
	hook CandidateHook
	// logf/debugf are the optional log seams, guarded by mu: info for the one
	// line per scan, debug for the per-path detail. Nil keeps the scanner
	// silent, which is the pre-U1.6 behaviour of a bare New.
	logf   func(format string, args ...any)
	debugf func(format string, args ...any)

	// running is the debounce latch: Scan holds it for its whole duration.
	running sync.Mutex
	busy    bool

	// Lifecycle state, guarded by mu. started records that the loop goroutine
	// was launched; stopped makes Stop permanent (Start after Stop is a no-op).
	// wg.Add happens under mu so a concurrent Stop's cancel+Wait never sees a
	// zero counter that is about to be incremented.
	mu      sync.Mutex
	obsMu   sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New builds a Scanner for the given roots. The provider is read for the
// initial cadence and stability window and again once per loop iteration, so
// settings changes apply live. An interval of exactly 0 selects DefaultInterval;
// a negative interval is an error and never falls back, so a bad provider value
// cannot quietly pick a cadence. A non-positive stability window selects
// DefaultStabilityWindow, so a zero window can never silently disable the
// stability gate.
// Every root must be an absolute, existing directory; it is resolved with
// filepath.EvalSymlinks so confinement checks compare canonical paths.
func New(store CandidateStore, roots []string, provider OptionsProvider) (*Scanner, error) {
	if store == nil {
		return nil, errors.New("watch: candidate store is required")
	}
	if provider == nil {
		return nil, errors.New("watch: options provider is required")
	}
	if len(roots) == 0 {
		return nil, errors.New("watch: at least one media root is required")
	}
	interval, err := provider.ScanInterval()
	if err != nil {
		return nil, fmt.Errorf("watch: read scan interval: %w", err)
	}
	if interval < 0 {
		return nil, fmt.Errorf("watch: interval must not be negative, got %v", interval)
	}
	if interval == 0 {
		interval = DefaultInterval
	}
	stability, err := provider.StabilityWindow()
	if err != nil {
		return nil, fmt.Errorf("watch: read stability window: %w", err)
	}
	// A non-positive window can never mean "gate off": that would make the
	// feature disappear silently if a provider returned a bad value. Fall back
	// to the settled default instead (the settings provider validates >= 4 s,
	// so this only fires for a buggy or hand-built provider).
	if stability <= 0 {
		stability = DefaultStabilityWindow
	}

	canonical := make([]string, 0, len(roots))
	seen := make(map[string]bool, len(roots))
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("watch: media root %q must be absolute", root)
		}
		resolved, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			return nil, fmt.Errorf("watch: resolve media root %q: %w", root, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("watch: stat media root %q: %w", root, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("watch: media root %q is not a directory", root)
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		canonical = append(canonical, resolved)
	}

	return &Scanner{
		store:        store,
		roots:        canonical,
		provider:     provider,
		interval:     interval,
		stability:    stability,
		now:          time.Now,
		newTicker:    newRealTicker,
		observations: make(map[string]observation),
	}, nil
}

// Interval reports the current scan cadence. It is the value the loop last
// read from the provider, so it follows settings changes live.
func (s *Scanner) Interval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interval
}

// stabilityWindow reports the stability window the loop last read.
func (s *Scanner) stabilityWindow() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stability
}

func (s *Scanner) setInterval(d time.Duration) {
	s.mu.Lock()
	s.interval = d
	s.mu.Unlock()
}

func (s *Scanner) setStabilityWindow(d time.Duration) {
	s.mu.Lock()
	s.stability = d
	s.mu.Unlock()
}

// SetCandidateHook installs fn as the post-record callback. Passing nil clears
// it. Set it before Start; a hook installed later takes effect on the next
// recorded candidate. Existing behaviour without a hook is unchanged.
func (s *Scanner) SetCandidateHook(fn CandidateHook) {
	s.mu.Lock()
	s.hook = fn
	s.mu.Unlock()
}

// SetLogging installs the scan log seams: logf gets one info line per scan
// (files seen / candidates added / skipped), debugf gets one line per scanned
// path with the reason it was skipped. Both are optional and nil keeps the
// scanner silent; the levels come from the seam, so a debug line disappears at
// the info threshold without the scanner knowing about levels.
func (s *Scanner) SetLogging(logf, debugf func(format string, args ...any)) {
	s.mu.Lock()
	s.logf, s.debugf = logf, debugf
	s.mu.Unlock()
}

// info emits one scan-level line through the installed seam.
func (s *Scanner) info(format string, args ...any) {
	s.mu.Lock()
	fn := s.logf
	s.mu.Unlock()
	if fn != nil {
		fn(format, args...)
	}
}

// debug emits one per-path line through the installed seam.
func (s *Scanner) debug(format string, args ...any) {
	s.mu.Lock()
	fn := s.debugf
	s.mu.Unlock()
	if fn != nil {
		fn(format, args...)
	}
}

// invokeHook runs the configured post-record callback, if any. A panic in the
// hook is recovered and returned as an error: the hook runs in the scan loop
// goroutine, so letting it unwind would kill the loop permanently.
func (s *Scanner) invokeHook(ctx context.Context, sourcePath, filename string, size int64) (err error) {
	s.mu.Lock()
	hook := s.hook
	s.mu.Unlock()
	if hook == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("watch: candidate hook panic: %v", r)
		}
	}()
	return hook(ctx, sourcePath, filename, size)
}

// Roots reports the canonicalized media roots.
func (s *Scanner) Roots() []string { return append([]string(nil), s.roots...) }

// Scan performs one pass over every root. It never returns ErrScanInProgress
// for the first caller; overlapping callers are skipped immediately. Per-file
// failures are collected in Result.Errors and also returned aggregated, so the
// caller can log them without losing the successful candidates.
//
// A file is only recorded once its size and mtime have stayed unchanged for
// the stability window, so a file still being copied is not enqueued mid-copy.
func (s *Scanner) Scan(ctx context.Context) (Result, error) {
	if !s.acquire() {
		return Result{}, ErrScanInProgress
	}
	defer s.release()

	var res Result
	present := make(map[string]struct{})
	complete := true
	for _, root := range s.roots {
		if err := s.scanRoot(ctx, root, &res, present); err != nil {
			res.Errors = append(res.Errors, err)
		}
		if ctx.Err() != nil {
			complete = false
			break
		}
	}
	// Prune stability observations for files that left the roots, so the map
	// stays bounded by what is actually on disk. A cancelled pass may not
	// have seen every root, so it must not prune the roots it never walked.
	if complete {
		s.pruneObservations(present)
	}
	// One info line per scan: what it saw and what it did with it. "Skipped"
	// is every seen file that did not become a new candidate this pass —
	// still settling, already queued, or a record/hook error.
	s.info("watch: scan finished: %d file(s) seen, %d candidate(s) added, %d skipped, %d error(s)",
		res.Seen, res.Added, res.Seen-res.Added, len(res.Errors))
	return res, errors.Join(res.Errors...)
}

func (s *Scanner) acquire() bool {
	s.running.Lock()
	defer s.running.Unlock()
	if s.busy {
		return false
	}
	s.busy = true
	return true
}

func (s *Scanner) release() {
	s.running.Lock()
	s.busy = false
	s.running.Unlock()
}

func (s *Scanner) scanRoot(ctx context.Context, root string, res *Result, present map[string]struct{}) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Vanished or unreadable entry: record and keep walking the rest.
			res.Errors = append(res.Errors, fmt.Errorf("watch: walk %q: %w", path, walkErr))
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		if !isMediaFile(d.Name()) {
			return nil
		}

		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("watch: resolve %q: %w", path, err))
			return nil
		}
		if !confined(resolved, root) {
			s.debug("watch: skipped %s: it escapes media root %s", path, root)
			res.Errors = append(res.Errors, fmt.Errorf("watch: %q escapes media root %q", path, root))
			return nil
		}
		info, err := os.Stat(resolved)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("watch: stat %q: %w", resolved, err))
			return nil
		}
		if !info.Mode().IsRegular() {
			s.debug("watch: skipped %s: not a regular file", resolved)
			return nil
		}

		res.Seen++
		present[resolved] = struct{}{}
		s.debug("watch: scanned %s (%d bytes, mtime %s)", resolved, info.Size(), info.ModTime().UTC().Format(time.RFC3339))
		if !s.observe(resolved, info) {
			// Still being written: keep the reading and retry next pass.
			s.debug("watch: skipped %s: size/mtime not stable for %v yet", resolved, s.stabilityWindow())
			return nil
		}
		created, err := s.store.RecordImportCandidate(ctx, resolved, info.Name(), info.Size())
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("watch: record %q: %w", resolved, err))
			return nil
		}
		// The store is the single source of truth for "queued": it inserts with
		// INSERT OR IGNORE against a UNIQUE source_file_path, so re-offering a
		// settled file every pass is a no-op while the row exists and re-creates
		// it when the row was deleted. No local latch, nothing to get stale.
		if created {
			res.Added++
			s.debug("watch: queued %s as a new import candidate", resolved)
			if err := s.invokeHook(ctx, resolved, info.Name(), info.Size()); err != nil {
				res.Errors = append(res.Errors, fmt.Errorf("watch: post-record hook for %q: %w", resolved, err))
			}
		} else {
			s.debug("watch: skipped %s: already queued", resolved)
		}
		return nil
	})
}

// observe records this pass's size/mtime reading for path and reports whether
// the file may be offered to the store. A file is offered once the same size
// and mtime have been observed for at least the stability window; a changed
// reading restarts the window. Whether the file is already queued is the
// store's business, not this map's.
func (s *Scanner) observe(path string, info fs.FileInfo) bool {
	window := s.stabilityWindow()

	s.obsMu.Lock()
	defer s.obsMu.Unlock()
	now := s.now()
	prev, ok := s.observations[path]
	if !ok || prev.size != info.Size() || !prev.modTime.Equal(info.ModTime()) {
		s.observations[path] = observation{size: info.Size(), modTime: info.ModTime(), since: now}
		return false
	}
	return now.Sub(prev.since) >= window
}

// pruneObservations drops readings for paths no longer under any scan root.
func (s *Scanner) pruneObservations(present map[string]struct{}) {
	s.obsMu.Lock()
	defer s.obsMu.Unlock()
	for path := range s.observations {
		if _, ok := present[path]; !ok {
			delete(s.observations, path)
		}
	}
}

// confined reports whether path is root itself or nested under it. Both are
// canonical paths.
func confined(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func isMediaFile(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, ".") {
		return false
	}
	for _, suffix := range partialSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return false
		}
	}
	return mediaExtensions[strings.ToLower(filepath.Ext(lower))]
}

// Start launches the scan loop in a goroutine and returns immediately. The
// first scan runs at once, then one per interval. The loop stops when ctx is
// cancelled or Stop is called. Start is idempotent and a no-op once Stop has
// been called or after the first Start (one loop per Scanner).
func (s *Scanner) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		s.run(ctx)
	}()
}

// run scans once immediately and then once per cadence. The cadence and the
// stability window are re-read from the provider every iteration, so a saved
// settings change applies without a restart; the ticker is rebuilt only when
// the cadence actually moves. A read error keeps the last known value — a
// settings hiccup must not change cadence or drop the stability gate.
func (s *Scanner) run(ctx context.Context) {
	interval := s.Interval()
	ticker := s.newTicker(interval)
	defer ticker.Stop()

	for {
		// Hook and record failures are non-fatal: Scan collects them in
		// Result.Errors and the loop keeps running, so log each one instead
		// of dropping the detail on the floor.
		res, _ := s.Scan(ctx)
		for _, err := range res.Errors {
			log.Printf("WARN watch: scan error: %v", err)
		}

		if next, err := s.provider.ScanInterval(); err != nil {
			log.Printf("WARN watch: read scan interval: %v (keeping %v)", err, interval)
		} else if next > 0 && next != interval {
			interval = next
			s.setInterval(next)
			ticker.Reset(next)
		}
		if window, err := s.provider.StabilityWindow(); err != nil {
			log.Printf("WARN watch: read stability window: %v", err)
		} else {
			// Same rule as New: a non-positive window is a bad provider
			// value, never "turn the gate off".
			if window <= 0 {
				window = DefaultStabilityWindow
			}
			s.setStabilityWindow(window)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.Chan():
		}
	}
}

// Stop cancels the scan loop and waits for it to exit. Safe to call more than
// once and without Start: stopping before Start is remembered (a later Start is
// a no-op), and a second Stop still waits for the loop to finish. Stop never
// returns while the goroutine is still running.
func (s *Scanner) Stop() {
	s.mu.Lock()
	s.stopped = true
	cancel := s.cancel
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	s.Wait()
}

// Wait blocks until the loop goroutine (if any) has exited.
func (s *Scanner) Wait() {
	s.wg.Wait()
}
