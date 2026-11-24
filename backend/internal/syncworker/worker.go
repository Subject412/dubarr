// Package syncworker is the M5-3 background consumer for sync jobs: one drain
// goroutine picks queued sync_mux_jobs rows by id ascending and runs the
// Execute → Mux → RescanAfterMux chain.
//
// The worker never analyses, muxes or rescans itself. It sequences the
// existing seams: [syncjob.Service.Execute] (M4-6) writes the DriftReport,
// internal/mux publishes a muxable verdict into the target, and
// internal/arrrescan notifies Radarr/Sonarr best-effort (M5-4).
//
// Failure policy:
//
//   - Execute failure, a non-muxable verdict (drifting, unreliable,
//     out_of_range), a mux failure: the job is marked failed with the first
//     line of the reason, staging under {WORK_DIR}/jobs/{id}/ is removed and
//     no mux is attempted.
//   - A rescan failure is logged and never changes a good mux result
//     (M5-4 contract: best-effort, non-fatal).
//   - Staging is removed once the job finishes, on success and on failure
//     (issue #23): Execute keeps it for the mux stage, the worker - which owns
//     the mux - deletes it when the job reaches a terminal status. Directories
//     left by an interrupted run are reaped when the worker starts.
//
// A panic in any seam is recovered and recorded as the job's failure so one
// bad job cannot kill the drain loop.
package syncworker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/arrrescan"
	"github.com/Subject412/dubarr/backend/internal/gate"
	"github.com/Subject412/dubarr/backend/internal/mux"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
	"github.com/Subject412/dubarr/backend/internal/transcode"
	"github.com/Subject412/dubarr/backend/internal/workdir"
)

// DefaultInterval is the idle poll cadence between queue passes.
const DefaultInterval = 2 * time.Second

// Typed constructor failures. A missing seam is a composition-root bug, so New
// fails closed instead of silently skipping a stage.
var (
	ErrNoWorkDir   = errors.New("syncworker: work directory is not configured")
	ErrNoStore     = errors.New("syncworker: job store is required")
	ErrNoExecutor  = errors.New("syncworker: sync job executor is required")
	ErrNoMuxer     = errors.New("syncworker: muxer is required")
	ErrNoRescanner = errors.New("syncworker: arr rescanner is required")
	// ErrUnmuxableVerdict reports a report the sync analysis itself rejected
	// (drifting, unreliable, out_of_range). Mux is never attempted.
	ErrUnmuxableVerdict = errors.New("syncworker: sync report is not muxable")
	// ErrEncoderUnavailable reports that an enabled Transcode Rule needs an
	// ffmpeg encoder this host does not provide (ADR-0010). The job fails
	// before any analysis, staging or mux work starts.
	ErrEncoderUnavailable = errors.New("syncworker: required audio encoder is unavailable")
)

// Executor is the M4-6 analysis seam. *syncjob.Service satisfies it.
type Executor interface {
	Execute(ctx context.Context, jobID int64) (analyze.Report, error)
}

// Muxer is the M5-1 muxing seam. *mux.Muxer satisfies it.
type Muxer interface {
	Mux(ctx context.Context, job mux.Job) (mux.Result, error)
}

// Rescanner is the M5-4 rescan seam. *arrrescan.Rescanner satisfies it.
type Rescanner interface {
	RescanAfterMux(ctx context.Context, row arrrescan.LibraryCacheRow) error
}

// Transcoder is the ADR-0010 opt-in audio encode seam. *transcode.Transcoder
// satisfies it; tests inject a fake to assert the plan and the failure paths.
type Transcoder interface {
	// EncoderAvailable is the pre-job check: a missing libopus must surface
	// as ErrEncoderUnavailable before any work happens.
	EncoderAvailable(ctx context.Context, encoder string) (bool, string)
	// Transcode performs one ffmpeg pass on the Work Area copy and verifies
	// the result.
	Transcode(ctx context.Context, req transcode.Request) (transcode.Result, error)
}

// Dependencies configure a Worker.
type Dependencies struct {
	// Store persists sync job state and reads the vault/library rows. Required.
	Store *store.Store
	// Settings supplies storage.paths, target.tag_filter, mux.options and
	// security.options. Optional: without it the gate is non-enforcing and the
	// default track title is used.
	Settings *settings.Store
	// Executor runs the M4-6 analysis. Required.
	Executor Executor
	// Muxer publishes a successful analysis. Required.
	Muxer Muxer
	// Rescanner notifies the arr instance after a publish. Required.
	Rescanner Rescanner
	// Transcoder applies opt-in Transcode Rules (ADR-0010). Optional: nil
	// builds the ffmpeg-backed transcoder, so the composition root needs no
	// extra wiring and a host without ffmpeg fails only the affected job.
	Transcoder Transcoder
	// WorkDir is storage.paths.work_dir (the Work Area root). Required.
	WorkDir string
	// Interval is the idle cadence; 0 selects DefaultInterval.
	Interval time.Duration
	// Logf receives worker diagnostics. Nil uses log.Printf.
	Logf func(format string, args ...any)
}

// Worker consumes queued sync jobs serially.
type Worker struct {
	store      *store.Store
	settings   *settings.Store
	executor   Executor
	muxer      Muxer
	rescanner  Rescanner
	transcoder Transcoder
	workDir    string
	interval   time.Duration
	logf       func(string, ...any)

	mu      sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New validates deps and builds a Worker.
func New(deps Dependencies) (*Worker, error) {
	if strings.TrimSpace(deps.WorkDir) == "" {
		return nil, ErrNoWorkDir
	}
	if deps.Store == nil || deps.Store.DB == nil {
		return nil, ErrNoStore
	}
	if deps.Executor == nil {
		return nil, ErrNoExecutor
	}
	if deps.Muxer == nil {
		return nil, ErrNoMuxer
	}
	if deps.Rescanner == nil {
		return nil, ErrNoRescanner
	}
	if deps.Interval < 0 {
		return nil, fmt.Errorf("syncworker: interval must not be negative, got %v", deps.Interval)
	}
	interval := deps.Interval
	if interval == 0 {
		interval = DefaultInterval
	}
	logf := deps.Logf
	if logf == nil {
		logf = log.Printf
	}
	transcoder := deps.Transcoder
	if transcoder == nil {
		transcoder = transcode.New(transcode.Dependencies{})
	}
	return &Worker{
		store:      deps.Store,
		settings:   deps.Settings,
		executor:   deps.Executor,
		muxer:      deps.Muxer,
		rescanner:  deps.Rescanner,
		transcoder: transcoder,
		workDir:    deps.WorkDir,
		interval:   interval,
		logf:       logf,
	}, nil
}

// Start launches the drain loop. Start is idempotent and a no-op after Stop.
//
// Before draining, Start reaps every {WORK_DIR}/jobs/{id}/ directory left over
// from a previous run (issue #23): staging only has meaning for the job that
// created it, so anything already on disk at boot belongs to a crashed or
// killed run.
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

	w.reapStale(time.Now())

	w.mu.Lock()
	w.wg.Add(1)
	w.mu.Unlock()

	go func() {
		defer w.wg.Done()
		w.runLoop(ctx)
	}()
}

// reapStale removes sync staging directories older than boot. Failures are
// logged: a leftover directory must never keep the worker from draining.
func (w *Worker) reapStale(boot time.Time) {
	stager, err := workdir.New(w.workDir, w.store)
	if err != nil {
		w.logf("syncworker: startup reap skipped: %v", err)
		return
	}
	if err := stager.ReapStale(boot); err != nil {
		w.logf("syncworker: startup reap: %v", err)
	}
}

// Stop cancels the loop and waits for the in-flight job to finish. Stop is
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
	// Drain once immediately so a job queued before Start is not delayed.
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

// drain processes queued jobs back-to-back until the queue is empty or the
// context is cancelled. A job failure is recorded on the row and never stops
// the loop.
func (w *Worker) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		processed, err := w.ProcessOnce(ctx)
		if err != nil {
			w.logf("syncworker: pass: %v", err)
			return
		}
		if !processed {
			return
		}
	}
}

// ProcessOnce picks and processes at most one queued job (lowest id first).
// processed is false when the queue is empty. A job-level failure is recorded
// on the row and reported as processed with a nil error; only store failures
// while recording the outcome are returned.
func (w *Worker) ProcessOnce(ctx context.Context) (processed bool, err error) {
	if ctx.Err() != nil {
		return false, nil
	}
	id, ok, err := w.nextQueued(ctx)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	// Issue #23: whatever happens to the job, its staging directory is
	// removed once the worker is done with it - completed (after mux) or
	// failed (Execute's own failure path is idempotent with this).
	defer w.cleanup(id)

	// A panic in any stage must fail only this job.
	var jobErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				jobErr = fmt.Errorf("syncworker: job %d panic: %v", id, r)
			}
		}()
		jobErr = w.processJob(ctx, id)
	}()

	if jobErr != nil {
		w.logf("syncworker: job %d failed: %v", id, jobErr)
		// Execute may already have recorded its own failure; overwriting with
		// the same first line is idempotent.
		if ferr := w.store.SetSyncJobStatus(context.WithoutCancel(ctx), id, statusFailed, errorSummary(jobErr)); ferr != nil {
			return true, ferr
		}
	}
	return true, nil
}

const (
	statusMuxing    = "muxing"
	statusCompleted = "completed"
	statusFailed    = "failed"
)

// processJob runs the Execute → Mux → Rescan chain for one queued job. Status
// transitions are left here so ProcessOnce can map any error onto a failure.
func (w *Worker) processJob(ctx context.Context, id int64) error {
	rec, ok, err := w.store.SyncJobByID(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("syncworker: job %d not found", id)
	}

	// ADR-0010: Transcode Rules are matched against PRE-TEMPO vault-track
	// metadata and the required encoder is verified BEFORE the job does any
	// work, so a missing libopus fails with a clear error instead of midway
	// through an encode (Q19).
	plan, track, err := w.transcodePlan(ctx, rec)
	if err != nil {
		return err
	}
	if plan.Encode() {
		if available, detail := w.transcoder.EncoderAvailable(ctx, plan.Encoder()); !available {
			return fmt.Errorf("%w: rule %d needs %s for audio track %d: %s",
				ErrEncoderUnavailable, plan.RuleIndex+1, plan.Encoder(), rec.TargetTrackID, detail)
		}
	}

	report, err := w.executor.Execute(ctx, id)
	if err != nil {
		return err
	}
	if !muxableVerdict(report.Verdict) {
		return fmt.Errorf("%w: verdict %q (%s)", ErrUnmuxableVerdict, report.Verdict, strings.TrimSpace(report.Reason))
	}

	// Execute persisted the report and offset; re-read for the canonical row.
	if rec, ok, err = w.store.SyncJobByID(ctx, id); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("syncworker: job %d disappeared", id)
	}

	lib, ok, err := w.store.LibraryCacheJobTargetByID(ctx, rec.TargetLibraryCacheID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("syncworker: library target %d not found", rec.TargetLibraryCacheID)
	}
	targetPath := strings.TrimSpace(rec.TargetVideoPath)
	if targetPath == "" {
		targetPath = lib.TargetFilePath
	}

	audio, err := w.stagedAudio(id)
	if err != nil {
		return err
	}
	if plan.Encode() {
		if audio, err = w.transcodeStaged(ctx, id, audio, plan, track); err != nil {
			return err
		}
	}
	offset := medianOffsetMS(report.OffsetsMS)
	rule, err := w.gateRule(ctx)
	if err != nil {
		return err
	}
	candidates, err := w.candidates(ctx, id, rec)
	if err != nil {
		return err
	}

	if err := w.store.SetSyncJobStatus(ctx, id, statusMuxing, ""); err != nil {
		return err
	}

	opts, optsOK := w.muxOptions(ctx)
	mode, outputDir, err := w.outputDestination(ctx, opts)
	if err != nil {
		return err
	}
	res, err := w.muxer.Mux(ctx, mux.Job{
		ID:             id,
		WorkDir:        w.workDir,
		TargetPath:     targetPath,
		TargetLangISO2: rec.TargetLangISO2,
		AudioPath:      audio,
		SyncOffsetMS:   offset,
		TrackTitle:     w.trackTitle(opts),
		KeepBak:        keepBak(opts, optsOK),
		OutputMode:     mode,
		OutputDir:      outputDir,
		Gate:           rule,
		Arr:            gate.ArrMetadata{Found: true, Tags: lib.ArrTagLabels, RootFolder: lib.RootFolderPath},
		Force:          w.forceOverride(ctx),
		Report:         report,
		Candidates:     candidates,
	})
	if err != nil {
		return err
	}

	// ADR-0011: record where the file landed so the API/UI can show it. This
	// write must outlive a shutdown cancellation (the mux already published);
	// a failure is logged and never undoes the publish.
	if err := w.store.SetSyncJobOutputPath(context.WithoutCancel(ctx), id, res.OutputPath); err != nil {
		w.logf("syncworker: job %d output path: %v", id, err)
	}

	// M5-4: rescan is best-effort and never undoes a published mux. Separate
	// mode leaves the library Target untouched, so there is nothing for the
	// arr to rescan (ADR-0011).
	if mode != settings.MuxOutputSeparate {
		if row, err := w.rescanRow(ctx, rec.TargetLibraryCacheID); err != nil {
			w.logf("syncworker: job %d rescan skipped: %v", id, err)
		} else if err := w.rescanner.RescanAfterMux(ctx, row); err != nil {
			w.logf("syncworker: job %d rescan: %v", id, err)
		}
	}

	// The mux already published, so the terminal status write must outlive a
	// shutdown cancellation: a SIGTERM during the up-to-30s rescan window would
	// otherwise fail this write and leave ProcessOnce marking a published job
	// failed. Only this last write is detached; the rescan above stays
	// cancellable and best-effort.
	return w.store.FinishSyncJobAnalysis(context.WithoutCancel(ctx), id, statusCompleted, string(report.SyncMethod), offset, report, "")
}

// muxableVerdict accepts exactly the two verdicts M5-1 may publish. The
// classification word "stable" is tolerated because analyze emits it alongside
// verdict ok in older reports.
func muxableVerdict(verdict string) bool {
	switch strings.ToLower(strings.TrimSpace(verdict)) {
	case analyze.VerdictOK, analyze.ClassificationStable:
		return true
	default:
		return false
	}
}

func (w *Worker) nextQueued(ctx context.Context) (int64, bool, error) {
	var id int64
	err := w.store.DB.QueryRowContext(ctx,
		`SELECT id FROM sync_mux_jobs WHERE status = 'queued' ORDER BY id ASC LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("syncworker: read queued jobs: %w", err)
	}
	return id, true, nil
}

// stagedAudio finds the single target .mka Execute left under
// {WORK_DIR}/jobs/{id}/. The directory is kept on Execute success (M5-1
// contract), so a missing file means the analysis did not stage its work.
func (w *Worker) stagedAudio(jobID int64) (string, error) {
	dir := filepath.Join(w.workDir, "jobs", fmt.Sprintf("%d", jobID))
	matches, err := filepath.Glob(filepath.Join(dir, "target-*.mka"))
	if err != nil {
		return "", fmt.Errorf("syncworker: list staging %s: %w", dir, err)
	}
	sort.Strings(matches)
	if len(matches) != 1 {
		return "", fmt.Errorf("syncworker: expected one staged .mka under %s, found %d", dir, len(matches))
	}
	info, err := os.Stat(matches[0])
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("syncworker: staged audio %s is not a regular file", matches[0])
	}
	return matches[0], nil
}

// candidates lists the active queued sync jobs for this target plus the job
// itself, so mux's ambiguity check fails loudly when two tracks are queued for
// the same target language. Completed and failed jobs are history, not queue.
func (w *Worker) candidates(ctx context.Context, jobID int64, rec store.SyncJobRecord) ([]mux.Candidate, error) {
	rows, err := w.store.DB.QueryContext(ctx, `
		SELECT j.target_track_id, COALESCE(t.lang_iso2, ''), COALESCE(t.variant, '')
		FROM sync_mux_jobs j
		JOIN audio_tracks t ON t.id = j.target_track_id
		WHERE j.target_video_path = ? AND j.id <> ? AND j.status IN ('queued', 'analyzing')
		ORDER BY j.id`, rec.TargetVideoPath, jobID)
	if err != nil {
		return nil, fmt.Errorf("syncworker: read queued candidates: %w", err)
	}
	defer rows.Close()
	candidates := []mux.Candidate{}
	seen := map[int64]bool{}
	for rows.Next() {
		var c mux.Candidate
		if err := rows.Scan(&c.TrackID, &c.LangISO2, &c.Variant); err != nil {
			return nil, fmt.Errorf("syncworker: scan candidate: %w", err)
		}
		c.TargetFilename = rec.TargetVideoPath
		if seen[c.TrackID] {
			continue
		}
		seen[c.TrackID] = true
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("syncworker: iterate candidates: %w", err)
	}
	if !seen[rec.TargetTrackID] {
		track, ok, err := w.store.VaultTrackByID(ctx, rec.TargetTrackID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("syncworker: vault track %d not found", rec.TargetTrackID)
		}
		candidates = append(candidates, mux.Candidate{
			TrackID:        track.ID,
			LangISO2:       track.LangISO2,
			Variant:        track.Variant,
			TargetFilename: rec.TargetVideoPath,
		})
	}
	return candidates, nil
}

// rescanRow reads the arr_library_cache columns RescanAfterMux needs. It is a
// direct query because the store projection does not carry the arr ids.
func (w *Worker) rescanRow(ctx context.Context, id int64) (arrrescan.LibraryCacheRow, error) {
	var row arrrescan.LibraryCacheRow
	err := w.store.DB.QueryRowContext(ctx, `
		SELECT arr_instance_id, media_type, external_item_id, COALESCE(series_id, 0)
		FROM arr_library_cache WHERE id = ?`, id).
		Scan(&row.ArrInstanceID, &row.MediaType, &row.ExternalItemID, &row.SeriesID)
	if errors.Is(err, sql.ErrNoRows) {
		return arrrescan.LibraryCacheRow{}, fmt.Errorf("syncworker: library target %d not found", id)
	}
	if err != nil {
		return arrrescan.LibraryCacheRow{}, fmt.Errorf("syncworker: read library target %d: %w", id, err)
	}
	return row, nil
}

func (w *Worker) gateRule(ctx context.Context) (gate.Rule, error) {
	if w.settings == nil {
		return gate.Rule{}, nil
	}
	raw, ok, err := w.settings.Get(ctx, settings.KeyTargetTagFilter)
	if err != nil {
		return gate.Rule{}, fmt.Errorf("syncworker: read target.tag_filter: %w", err)
	}
	if !ok {
		return gate.Rule{}, nil
	}
	var tf settings.TargetTagFilter
	if err := json.Unmarshal(raw, &tf); err != nil {
		return gate.Rule{}, fmt.Errorf("syncworker: decode target.tag_filter: %w", err)
	}
	return gate.RuleFromTagFilter(tf), nil
}

// forceOverride maps the appliance-wide security switch onto mux.Force. The
// job row does not persist a per-job force flag (M4-6 create only gates the
// request), so the global opt-in is the only durable caller-owned switch.
func (w *Worker) forceOverride(ctx context.Context) bool {
	if w.settings == nil {
		return false
	}
	raw, ok, err := w.settings.Get(ctx, settings.KeySecurityOptions)
	if err != nil || !ok {
		return false
	}
	var sec settings.SecurityOptions
	if json.Unmarshal(raw, &sec) != nil {
		return false
	}
	return sec.AllowForceTagOverride
}

func (w *Worker) trackTitle(opts settings.MuxOptions) string {
	return strings.TrimSpace(opts.TrackTitleInMKV)
}

// muxOptions reads settings mux.options once per job. ok=false means the
// settings are unavailable, and the caller falls back to the shipped defaults
// (empty track title, backups kept).
func (w *Worker) muxOptions(ctx context.Context) (settings.MuxOptions, bool) {
	if w.settings == nil {
		return settings.MuxOptions{}, false
	}
	raw, ok, err := w.settings.Get(ctx, settings.KeyMuxOptions)
	if err != nil || !ok {
		return settings.MuxOptions{}, false
	}
	var opts settings.MuxOptions
	if json.Unmarshal(raw, &opts) != nil {
		return settings.MuxOptions{}, false
	}
	return opts, true
}

// audioOptions reads settings audio.options once per job. ok=false means the
// section is absent (a legacy database), which means no rules at all.
func (w *Worker) audioOptions(ctx context.Context) (settings.AudioOptions, bool, error) {
	if w.settings == nil {
		return settings.AudioOptions{}, false, nil
	}
	raw, ok, err := w.settings.Get(ctx, settings.KeyAudioOptions)
	if err != nil {
		return settings.AudioOptions{}, false, fmt.Errorf("syncworker: read audio.options: %w", err)
	}
	if !ok {
		return settings.AudioOptions{}, false, nil
	}
	var opts settings.AudioOptions
	if err := json.Unmarshal(raw, &opts); err != nil {
		return settings.AudioOptions{}, false, fmt.Errorf("syncworker: decode audio.options: %w", err)
	}
	return opts, true, nil
}

// transcodePlan resolves the first matching Transcode Rule against the vault
// track's PRE-TEMPO metadata (Q19). It always returns the vault track row so
// the encode step can verify against its channels/duration. A missing or empty
// rule list is not an error: the job keeps the historical copy behaviour.
func (w *Worker) transcodePlan(ctx context.Context, rec store.SyncJobRecord) (transcode.Plan, store.VaultTrackRecord, error) {
	track, ok, err := w.store.VaultTrackByID(ctx, rec.TargetTrackID)
	if err != nil {
		return transcode.Plan{}, store.VaultTrackRecord{}, err
	}
	if !ok {
		return transcode.Plan{}, store.VaultTrackRecord{}, fmt.Errorf("syncworker: vault track %d not found", rec.TargetTrackID)
	}
	opts, found, err := w.audioOptions(ctx)
	if err != nil {
		return transcode.Plan{}, store.VaultTrackRecord{}, err
	}
	if !found || len(opts.Rules) == 0 {
		return transcode.Plan{}, track, nil
	}
	plan, matched := transcode.Match(opts.Rules, transcode.TrackMeta{
		Codec:         track.Codec,
		ChannelCount:  track.ChannelCount,
		ChannelLayout: track.Channels,
		BitrateBPS:    track.BitrateBPS,
		SampleRate:    track.SampleRate,
		LangISO2:      track.LangISO2,
		Variant:       track.Variant,
	})
	if !matched {
		return transcode.Plan{}, track, nil
	}
	return plan, track, nil
}

// transcodeStaged applies a matched rule to the Copy-on-Work .mka Execute
// staged. The vault file is never opened here (ADR-0002); the output lives in
// the same job directory so mux's confinement check still accepts it.
//
// Timing and transcoding are one encode (Q20): the mux path's offset is an
// mkvmerge `--sync` flag, not a re-encode, and any tempo factor must be folded
// into this same plan (Plan.WithTempo) rather than applied as a second pass.
// Language and title survive because mux writes them onto the appended track
// (mux.options.track_title_in_mkv), and the encode carries the language tag.
func (w *Worker) transcodeStaged(ctx context.Context, jobID int64, audio string, plan transcode.Plan, track store.VaultTrackRecord) (string, error) {
	out := filepath.Join(w.workDir, "jobs", strconv.FormatInt(jobID, 10), "audio.transcoded.mka")
	res, err := w.transcoder.Transcode(ctx, transcode.Request{
		Input:            audio,
		Output:           out,
		Plan:             plan,
		SourceChannels:   track.ChannelCount,
		SourceDurationMS: track.DurationMS,
		LangISO2:         track.LangISO2,
	})
	if err != nil {
		return "", fmt.Errorf("syncworker: job %d transcode (rule %d): %w", jobID, plan.RuleIndex+1, err)
	}
	w.logf("syncworker: job %d transcoded rule %d to %s (%d ch, %d bps)", jobID, plan.RuleIndex+1, res.Codec, res.Channels, res.BitrateBPS)
	return res.Path, nil
}

// keepBak maps mux.options.keep_bak onto mux.Job.KeepBak: only an explicit
// false becomes a pointer, so the muxer's default (keep) survives a missing or
// unreadable settings row.
func keepBak(opts settings.MuxOptions, ok bool) *bool {
	if ok && !opts.KeepBak {
		discard := false
		return &discard
	}
	return nil
}

// outputDestination resolves mux.options.output_mode and re-validates
// storage.paths.output_dir in the worker (ADR-0011). The settings API already
// checks the shape, but a row can be edited directly in SQLite, and the
// EvalSymlinks-resolved vault guard is only meaningful here.
//
// replace mode returns the empty output dir; Mux then publishes in place.
func (w *Worker) outputDestination(ctx context.Context, opts settings.MuxOptions) (string, string, error) {
	mode := strings.TrimSpace(opts.OutputMode)
	if mode == "" {
		mode = settings.MuxOutputReplace
	}
	if mode != settings.MuxOutputSeparate {
		// An unknown mode is rejected by the muxer itself.
		return mode, "", nil
	}
	if w.settings == nil {
		return "", "", fmt.Errorf("%w: storage.paths is unavailable", mux.ErrOutputDir)
	}
	raw, ok, err := w.settings.Get(ctx, settings.KeyStoragePaths)
	if err != nil {
		return "", "", fmt.Errorf("syncworker: read storage.paths: %w", err)
	}
	if !ok {
		return "", "", fmt.Errorf("%w: storage.paths is not configured", mux.ErrOutputDir)
	}
	var paths settings.StoragePaths
	if err := json.Unmarshal(raw, &paths); err != nil {
		return "", "", fmt.Errorf("%w: decode storage.paths: %v", mux.ErrOutputDir, err)
	}
	if strings.TrimSpace(paths.OutputDir) == "" {
		return "", "", fmt.Errorf("%w: separate output mode requires storage.paths.output_dir", mux.ErrOutputDir)
	}
	resolved, err := resolveOutputRoot(paths.OutputDir)
	if err != nil {
		return "", "", err
	}
	// ADR-0002: never write into the Audio Vault. Compare canonical paths when
	// the vault exists, and fall back to the cleaned lexical paths otherwise.
	if strings.TrimSpace(paths.VaultDir) != "" {
		vault := filepath.Clean(paths.VaultDir)
		if abs, err := filepath.Abs(vault); err == nil {
			vault = abs
		}
		if canonical, err := filepath.EvalSymlinks(vault); err == nil {
			vault = canonical
		}
		if withinDir(vault, resolved) {
			return "", "", fmt.Errorf("%w: %s is the vault folder or inside it", mux.ErrOutputDir, resolved)
		}
	}
	return settings.MuxOutputSeparate, resolved, nil
}

// resolveOutputRoot canonicalizes and checks the separate-mode destination root.
func resolveOutputRoot(dir string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(dir))
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", mux.ErrOutputDir, dir, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", mux.ErrOutputDir, dir, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", mux.ErrOutputDir, dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %s is not a directory", mux.ErrOutputDir, resolved)
	}
	return resolved, nil
}

// withinDir reports whether target is root or below it.
func withinDir(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// cleanup removes {WORK_DIR}/jobs/{id}/ once a job finishes. Cleanup errors are
// logged; the job outcome is already recorded.
func (w *Worker) cleanup(jobID int64) {
	stager, err := workdir.New(w.workDir, w.store)
	if err != nil {
		w.logf("syncworker: job %d cleanup: %v", jobID, err)
		return
	}
	if err := stager.Cleanup(jobID); err != nil {
		w.logf("syncworker: job %d cleanup: %v", jobID, err)
	}
}

// medianOffsetMS mirrors syncjob's persisted offset: the median checkpoint
// offset, rounded. An empty report yields 0 (the muxer resolves the verdict).
func medianOffsetMS(offsets []float64) int64 {
	if len(offsets) == 0 {
		return 0
	}
	sorted := append([]float64(nil), offsets...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	median := sorted[mid]
	if len(sorted)%2 == 0 {
		median = (sorted[mid-1] + sorted[mid]) / 2
	}
	return int64(math.Round(median))
}

// errorSummary trims an error to one storable line.
func errorSummary(err error) string {
	msg := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	msg = strings.Join(strings.Fields(msg), " ")
	const limit = 1000
	if len(msg) > limit {
		msg = msg[:limit]
	}
	return msg
}
