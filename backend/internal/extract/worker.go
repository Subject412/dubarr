package extract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// DefaultInterval is the idle poll cadence between extraction passes. The
// worker also drains every ready row back-to-back before sleeping.
const DefaultInterval = 2 * time.Second

// DurationTolerance is how far the extracted track's duration may differ from
// the donor's before validation rejects it. Lossless extraction of one stream
// can trim container padding by a frame or two; anything larger means the
// wrong stream or a damaged donor.
const DurationTolerance = 2 * time.Second

// ErrNoStagingRoot reports that WORK_DIR could not be resolved (no setting and
// no DATA_DIR to fall back on).
var ErrNoStagingRoot = errors.New("extract: work directory is not configured")

// JobStore is the import_queue surface the worker needs. *store.Store
// satisfies it.
type JobStore interface {
	NextImportExtraction(ctx context.Context) (store.ImportExtractionJob, bool, error)
	ClaimImportExtraction(ctx context.Context, id int64) (bool, error)
	FinishImportExtraction(ctx context.Context, id int64, status, errorMessage string) error
}

// SettingsReader reads the storage/language sections. *settings.Store
// satisfies it.
type SettingsReader interface {
	Get(ctx context.Context, key string) (json.RawMessage, bool, error)
}

// Dependencies configures a Worker.
type Dependencies struct {
	// Store persists queue transitions. Required.
	Store JobStore
	// Settings supplies storage.paths and languages.policy. Optional: without
	// it, WORK_DIR falls back to DATA_DIR/work and the default policy is used.
	Settings SettingsReader
	// DataDir is the fallback base for WORK_DIR when storage.paths.work_dir is
	// unset.
	DataDir string
	// Prober inspects donor and extracted files. Nil builds a default Prober.
	Prober *probe.Prober
	// MKVMergePath / MKVExtractPath pin the mkvtoolnix binaries; empty resolves
	// them on PATH per run.
	MKVMergePath   string
	MKVExtractPath string
	// Interval is the idle cadence; 0 selects DefaultInterval, negative is an
	// error.
	Interval time.Duration
	// Logf receives worker diagnostics. Nil uses the standard logger.
	Logf func(format string, args ...any)
}

// Worker consumes extracting rows serially. Lifecycle mirrors the watch
// scanner: Start launches one loop, Stop waits for it.
type Worker struct {
	store      JobStore
	settings   SettingsReader
	dataDir    string
	prober     *probe.Prober
	mkvmerge   string
	mkvextract string
	interval   time.Duration
	logf       func(string, ...any)

	// run executes mkvtoolnix commands; overridable for unit tests.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)

	mu      sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New validates deps and builds a Worker. Binaries are resolved lazily per
// job, so a missing mkvtoolnix degrades to per-row failures instead of
// preventing startup.
func New(deps Dependencies) (*Worker, error) {
	if deps.Store == nil {
		return nil, errors.New("extract: job store is required")
	}
	if deps.Interval < 0 {
		return nil, fmt.Errorf("extract: interval must not be negative, got %v", deps.Interval)
	}
	interval := deps.Interval
	if interval == 0 {
		interval = DefaultInterval
	}
	prober := deps.Prober
	if prober == nil {
		prober = probe.New(probe.Dependencies{})
	}
	logf := deps.Logf
	if logf == nil {
		logf = log.Printf
	}
	return &Worker{
		store:      deps.Store,
		settings:   deps.Settings,
		dataDir:    deps.DataDir,
		prober:     prober,
		mkvmerge:   deps.MKVMergePath,
		mkvextract: deps.MKVExtractPath,
		interval:   interval,
		logf:       logf,
		run:        execRun,
	}, nil
}

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return out, err
}

// Start reaps staging directories left from before boot, then launches the
// serialized processing loop. Start is idempotent and a no-op after Stop.
func (w *Worker) Start(ctx context.Context) {
	w.mu.Lock()
	if w.started || w.stopped {
		w.mu.Unlock()
		return
	}
	w.started = true
	ctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.mu.Unlock()

	// Reap synchronously: any staging directory older than this boot belongs to
	// a crashed/aborted run and must not be mistaken for current work.
	if root, err := w.stagingRoot(ctx); err != nil {
		w.logf("extract: startup reap skipped: %v", err)
	} else if err := reapStale(root, time.Now()); err != nil {
		w.logf("extract: startup reap: %v", err)
	}

	w.mu.Lock()
	w.wg.Add(1)
	w.mu.Unlock()

	go func() {
		defer w.wg.Done()
		w.runLoop(ctx)
	}()
}

// Stop cancels the loop and waits for the in-flight job to wind down. Stop is
// permanent; Start afterwards is a no-op.
func (w *Worker) Stop() {
	w.mu.Lock()
	w.stopped = true
	cancel := w.cancel
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	w.wg.Wait()
}

// Interval reports the effective idle cadence.
func (w *Worker) Interval() time.Duration { return w.interval }

func (w *Worker) runLoop(ctx context.Context) {
	// Drain once immediately so an approval before Start is not delayed.
	w.drain(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.drain(ctx)
		}
	}
}

// drain processes every currently ready row, one at a time, until the queue is
// empty or the context is cancelled. A row failure is recorded on the row and
// never stops the loop.
func (w *Worker) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		processed, err := w.ProcessOnce(ctx)
		if err != nil {
			w.logf("extract: pass: %v", err)
			return
		}
		if !processed {
			return
		}
	}
}

// ProcessOnce claims and processes at most one extracting row. processed is
// false when there was nothing (claimable) to do. A job-level failure is
// recorded on the row and reported as processed with a nil error; only store
// failures are returned.
func (w *Worker) ProcessOnce(ctx context.Context) (processed bool, err error) {
	job, ok, err := w.store.NextImportExtraction(ctx)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	claimed, err := w.store.ClaimImportExtraction(ctx, job.ID)
	if err != nil {
		return false, err
	}
	if !claimed {
		// Another actor finished the row between the read and the claim.
		return true, nil
	}

	// A panic in extraction must not kill the loop; record it as a failure.
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("extract: job %d panic: %v", job.ID, r)
			}
		}()
		err = w.processJob(ctx, job)
	}()

	status, message := "completed", ""
	if err != nil {
		status, message = "failed", errorSummary(err)
		w.logf("extract: job %d failed: %v", job.ID, err)
	}
	if ferr := w.store.FinishImportExtraction(ctx, job.ID, status, message); ferr != nil {
		w.logf("extract: job %d: record %s: %v", job.ID, status, ferr)
		return true, ferr
	}
	return true, nil
}

// errorSummary trims an error to a single storable line.
func errorSummary(err error) string {
	msg := strings.Join(strings.Fields(err.Error()), " ")
	const limit = 1000
	if len(msg) > limit {
		msg = msg[:limit]
	}
	return msg
}

// stagingRoot resolves {WORK_DIR}/extract, creating it when needed.
func (w *Worker) stagingRoot(ctx context.Context) (string, error) {
	workDir, err := w.workDir(ctx)
	if err != nil {
		return "", err
	}
	root := filepath.Join(workDir, "extract")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("extract: create staging root %s: %w", root, err)
	}
	return root, nil
}

// workDir reads storage.paths.work_dir, falling back to DATA_DIR/work.
func (w *Worker) workDir(ctx context.Context) (string, error) {
	if w.settings != nil {
		raw, ok, err := w.settings.Get(ctx, settings.KeyStoragePaths)
		if err == nil && ok {
			var paths settings.StoragePaths
			if json.Unmarshal(raw, &paths) == nil && strings.TrimSpace(paths.WorkDir) != "" {
				return paths.WorkDir, nil
			}
		}
	}
	if strings.TrimSpace(w.dataDir) != "" {
		return filepath.Join(w.dataDir, "work"), nil
	}
	return "", ErrNoStagingRoot
}

// reapStale removes every entry under stagingRoot whose modification time is
// before boot. Entries are removed wholesale; failures are collected so one
// locked directory does not block the rest.
func reapStale(stagingRoot string, boot time.Time) error {
	entries, err := os.ReadDir(stagingRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("extract: read staging root: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !info.ModTime().Before(boot) {
			continue
		}
		path := filepath.Join(stagingRoot, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}
