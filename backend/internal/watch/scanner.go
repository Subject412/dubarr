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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultInterval is the settled watch-folder cadence (PLAN.md M2-3,
// settings.KeyWatchOptions default scan_interval_sec = 10).
const DefaultInterval = 10 * time.Second

// ErrScanInProgress is returned when Scan is called while a previous scan is
// still running. The overlapping call is skipped, not queued.
var ErrScanInProgress = errors.New("watch: scan already in progress")

// mediaExtensions is the set of donor video containers the scanner enqueues.
// IDEA.md names .mkv explicitly; the remaining common containers are accepted
// so a donor drop is never silently ignored. Matching is case-insensitive.
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
// be enqueued (IDEA.md §3.5). Lower-case comparison.
var partialSuffixes = []string{".!qb", ".part", ".tmp", ".crdownload", ".tdarr"}

// CandidateStore persists a discovered donor file. *store.Store satisfies it.
// created reports whether the file was new to the queue.
type CandidateStore interface {
	RecordImportCandidate(ctx context.Context, sourcePath, filename string, size int64) (created bool, err error)
}

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

// Scanner periodically walks the configured roots for new media files.
type Scanner struct {
	store    CandidateStore
	roots    []string
	interval time.Duration

	// running is the debounce latch: Scan holds it for its whole duration.
	running sync.Mutex
	busy    bool

	// Lifecycle state, created by Start and released by Stop/Wait.
	startOnce sync.Once
	stopOnce  sync.Once
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// New builds a Scanner for the given roots. interval <= 0 selects
// DefaultInterval; a negative interval is an error. Every root must be an
// absolute, existing directory; it is resolved with filepath.EvalSymlinks so
// confinement checks compare canonical paths.
func New(store CandidateStore, roots []string, interval time.Duration) (*Scanner, error) {
	if store == nil {
		return nil, errors.New("watch: candidate store is required")
	}
	if len(roots) == 0 {
		return nil, errors.New("watch: at least one media root is required")
	}
	if interval < 0 {
		return nil, fmt.Errorf("watch: interval must not be negative, got %v", interval)
	}
	if interval == 0 {
		interval = DefaultInterval
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

	return &Scanner{store: store, roots: canonical, interval: interval}, nil
}

// Interval reports the effective scan cadence.
func (s *Scanner) Interval() time.Duration { return s.interval }

// Roots reports the canonicalized media roots.
func (s *Scanner) Roots() []string { return append([]string(nil), s.roots...) }

// Scan performs one pass over every root. It never returns ErrScanInProgress
// for the first caller; overlapping callers are skipped immediately. Per-file
// failures are collected in Result.Errors and also returned aggregated, so the
// caller can log them without losing the successful candidates.
func (s *Scanner) Scan(ctx context.Context) (Result, error) {
	if !s.acquire() {
		return Result{}, ErrScanInProgress
	}
	defer s.release()

	var res Result
	for _, root := range s.roots {
		if err := s.scanRoot(ctx, root, &res); err != nil {
			res.Errors = append(res.Errors, err)
		}
		if ctx.Err() != nil {
			break
		}
	}
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

func (s *Scanner) scanRoot(ctx context.Context, root string, res *Result) error {
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
			res.Errors = append(res.Errors, fmt.Errorf("watch: %q escapes media root %q", path, root))
			return nil
		}
		info, err := os.Stat(resolved)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("watch: stat %q: %w", resolved, err))
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		res.Seen++
		created, err := s.store.RecordImportCandidate(ctx, resolved, info.Name(), info.Size())
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("watch: record %q: %w", resolved, err))
			return nil
		}
		if created {
			res.Added++
		}
		return nil
	})
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
// cancelled or Stop is called. Start is idempotent.
func (s *Scanner) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(ctx)
		s.cancel = cancel
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.run(ctx)
		}()
	})
}

func (s *Scanner) run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		// Errors are surfaced through Result; the loop keeps running.
		_, _ = s.Scan(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Stop cancels the scan loop and waits for it to exit. Safe to call more than
// once and without Start.
func (s *Scanner) Stop() {
	s.stopOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
	s.Wait()
}

// Wait blocks until the loop goroutine (if any) has exited.
func (s *Scanner) Wait() {
	s.wg.Wait()
}
