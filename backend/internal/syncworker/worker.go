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
//   - Execute failure, a non-muxable verdict (an unreliable or out-of-range
//     report, or a drift this worker cannot correct — see muxableReport), a mux
//     failure: the job is marked failed with the first line of the reason,
//     staging under {WORK_DIR}/jobs/{id}/ is removed and no mux is attempted.
//   - A mux refusal is terminal, never retried: [mux.ErrUnsupportedContainer]
//     (a target or destination that is not Matroska) and the other mux
//     sentinels surface unchanged from Mux, so the job is marked failed once
//     with the sentinel's own message and no second attempt is made.
//   - A target the media manager replaced mid-mux ([mux.ErrTargetChanged],
//     ADR-0018) is not a failure: nothing was published, so the job returns to
//     queued with one log line and is skipped for the rest of that pass. The
//     drain keeps running the jobs behind it and ends only when no queued job
//     is left unvisited; a later pass picks the requeued row up against the new
//     file. Attempt caps and backoff are a later unit.
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
	// errRequeued is an internal signal, never surfaced to a caller: the job
	// was put back on the queue (a target that changed mid-mux, ADR-0018)
	// instead of failing. The drain pass records the id as visited and moves on
	// to the next queued job rather than re-running the same row.
	errRequeued = errors.New("syncworker: job requeued")
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
	// as ErrEncoderUnavailable before any work happens. A drift correction's
	// encoder is checked after Execute instead, because its factor does not
	// exist before the report does; that check still precedes the encode.
	EncoderAvailable(ctx context.Context, encoder string) (bool, string)
	// Transcode performs one ffmpeg pass on the copy in the temp folder and
	// verifies the result.
	Transcode(ctx context.Context, req transcode.Request) (transcode.Result, error)
}

// Dependencies configure a Worker.
type Dependencies struct {
	// Store persists sync job state and reads the vault/library rows. Required.
	Store *store.Store
	// Settings supplies storage.paths, target.tag_filter, mux.options and
	// security.options. Optional: without it the gate is non-enforcing and the
	// mux runs in replace mode.
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
	// WorkDir is storage.paths.work_dir (the temp folder root). Required.
	WorkDir string
	// Interval is the idle cadence; 0 selects DefaultInterval.
	Interval time.Duration
	// Logf receives worker decisions at info: job claimed, mux published.
	// Nil uses log.Printf.
	Logf func(format string, args ...any)
	// Errorf receives a job failure at error, with the job id and the stage it
	// failed at. Nil falls back to Logf so a bare worker still reports it.
	Errorf func(format string, args ...any)
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
	errorf     func(string, ...any)

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
	errorf := deps.Errorf
	if errorf == nil {
		errorf = logf
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
		errorf:     errorf,
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
// the loop. A job that went back on the queue (a target that changed mid-mux)
// keeps its low id, so the pass remembers every id it visited and excludes
// them from the next selection: the requeued row cannot be picked up again in
// the same pass, and the jobs behind it still run instead of starving until a
// whole interval has passed.
func (w *Worker) drain(ctx context.Context) {
	visited := make(map[int64]bool)
	for {
		if ctx.Err() != nil {
			return
		}
		processed, err := w.processOnce(ctx, visited)
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
// while recording the outcome are returned. A job returned to the queue (a
// target that changed mid-mux) is handled — requeued and logged once — and
// still reported as processed, because this call did run a job; a one-shot
// caller simply finds the row queued again.
func (w *Worker) ProcessOnce(ctx context.Context) (processed bool, err error) {
	return w.processOnce(ctx, nil)
}

// processOnce is ProcessOnce restricted to the queued jobs a drain pass has
// not handled yet: every id it handles is recorded in visited (nil outside a
// pass), so a requeued row with the same low id is not selected twice.
func (w *Worker) processOnce(ctx context.Context, visited map[int64]bool) (bool, error) {
	if ctx.Err() != nil {
		return false, nil
	}
	id, ok, err := w.nextQueued(ctx, visited)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if visited != nil {
		visited[id] = true
	}
	w.logf("syncworker: job %d claimed", id)
	// Issue #23: whatever happens to the job, its staging directory is
	// removed once the worker is done with it - completed (after mux), failed
	// (Execute's own failure path is idempotent with this) or requeued (a
	// changed target; the next attempt re-stages from scratch).
	defer w.cleanup(id)

	// A panic in any stage must fail only this job.
	var jobErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				jobErr = atStage("panic", fmt.Errorf("syncworker: job %d panic: %v", id, r))
			}
		}()
		jobErr = w.processJob(ctx, id)
	}()

	if jobErr != nil {
		// The job was put back on the queue, not failed: its status and its one
		// log line are already written, and the id is in visited, so this pass
		// moves on to the next queued job instead of re-running the same row
		// (ADR-0018).
		if errors.Is(jobErr, errRequeued) {
			return true, nil
		}
		w.errorf("syncworker: job %d failed at %s: %v", id, failureStage(jobErr), jobErr)
		// Execute may already have recorded its own failure; overwriting with
		// the same first line is idempotent.
		if ferr := w.store.SetSyncJobStatus(context.WithoutCancel(ctx), id, statusFailed, errorSummary(jobErr)); ferr != nil {
			return true, ferr
		}
	}
	return true, nil
}

const (
	statusQueued    = "queued"
	statusMuxing    = "muxing"
	statusCompleted = "completed"
	statusFailed    = "failed"
)

// processJob runs the Execute → Mux → Rescan chain for one queued job. Status
// transitions are left here so ProcessOnce can map any error onto a failure.
func (w *Worker) processJob(ctx context.Context, id int64) error {
	rec, ok, err := w.store.SyncJobByID(ctx, id)
	if err != nil {
		return atStage("load", err)
	}
	if !ok {
		return atStage("load", fmt.Errorf("syncworker: job %d not found", id))
	}

	// Claim-time re-check (#48): the selection (nextQueued) and this load are not
	// one transaction, and a held row is only ever promoted by a projection that
	// ran on some earlier snapshot. Before spending an analysis on it, prove the
	// row is still queued and its target still passes the gate; a job that turned
	// ineligible under us goes back to held instead of muxing into a file the
	// upstream pipeline may still replace. A forced job is never re-held — the
	// operator's override is persisted on the row and outranks the gate.
	if rec.Status != statusQueued {
		w.logf("syncworker: debug: job %d is %q, skipping", id, rec.Status)
		return nil
	}
	reason, err := w.reholdReason(ctx, rec)
	if err != nil {
		return atStage("gate", err)
	}
	if reason != "" {
		// HoldSyncJob only accepts a claimed row (analyzing or muxing), so claim
		// it first and demote it in the same breath: the job never reaches
		// Execute, and a projection that releases it later starts from held.
		if err := w.store.SetSyncJobStatus(ctx, id, store.SyncJobAnalyzing, ""); err != nil {
			return atStage("claim", err)
		}
		held, err := w.store.HoldSyncJob(ctx, id, reason)
		if err != nil {
			return atStage("hold", err)
		}
		if held {
			w.logf("syncworker: job %d re-held (%s): its target is no longer eligible", id, reason)
		}
		return nil
	}

	// ADR-0010: Transcode Rules are matched against PRE-TEMPO vault-track
	// metadata and the required encoder is verified BEFORE the job does any
	// work, so a missing libopus fails with a clear error instead of midway
	// through an encode (Q19).
	plan, track, err := w.transcodePlan(ctx, rec)
	if err != nil {
		return atStage("plan", err)
	}
	if plan.Encode() {
		if available, detail := w.transcoder.EncoderAvailable(ctx, plan.Encoder()); !available {
			return atStage("encoder", fmt.Errorf("%w: rule %d needs %s for audio track %d: %s",
				ErrEncoderUnavailable, plan.RuleIndex+1, plan.Encoder(), rec.TargetTrackID, detail))
		}
	}

	report, err := w.executor.Execute(ctx, id)
	if err != nil {
		return atStage("analyze", err)
	}
	// #86: a clean frame-rate drift is corrected by this worker, folded into the
	// one encode the plan already describes (a matched rule's, or a tempo-only
	// FLAC one). The decision is taken before the muxability gate because the
	// gate must see whether the drift was actually compensated — and only the
	// worker knows whether an encode path exists; analyze never sets
	// TempoApplied itself.
	//
	// #84 sits first and is mutually exclusive with it by construction: a
	// piecewise render requires an accepted segmentation, the tempo path requires
	// a drifting verdict, and analyze never emits both for the same report. If
	// they ever did, the cut wins and Application records only that.
	//
	// A *rejected* segmentation is never guessed at (D5): no cut, no tempo — a
	// drift that came from a re-cut is not a frame-rate drift — just the dominant
	// single offset, with report.review_reason surfaced on the failure below.
	var segments []transcode.Segment
	switch {
	case piecewiseRender(report):
		segments = toTranscodeSegments(report.Piecewise.Segments)
		// A cut track is always an encode: the matched rule's codec when there
		// is one, FLAC otherwise. Lossless, because the operator switched this
		// on for timing, not to spend quality (ADR-0010's boundary).
		if !plan.Encode() {
			plan = transcode.Plan{RuleIndex: transcode.NoRule, Codec: transcode.CodecFLAC}
			// The rule path's encoder check ran before Execute. This one could
			// not: the FLAC plan did not exist until the report did.
			if available, detail := w.transcoder.EncoderAvailable(ctx, plan.Encoder()); !available {
				return atStage("encoder", fmt.Errorf("%w: piecewise render needs %s for audio track %d: %s",
					ErrEncoderUnavailable, plan.Encoder(), rec.TargetTrackID, detail))
			}
		}
		// #129: a dub shorter than the film leaves the film's end silent, and
		// the render pads that tail (see analyze.Segment's tiling note). Say so
		// in milliseconds instead of letting a 20-minute dub under a 22-minute
		// film read as a clean success.
		//
		// The sign is the renderer's own convention, not a guess:
		// BuildSegmentedArgs trims a run at [StartSec - OffsetMS/1000,
		// EndSec - OffsetMS/1000) of the dub (backend/internal/transcode/
		// segments.go), so the source window is the film span moved back by the
		// offset and a film position t is dub position t - offset/1000. The
		// dub's last sample therefore sits at film time dubSec + offset/1000,
		// and the tail is what the last run's end leaves beyond it. Offsets are
		// per-segment, so it is the LAST run's offset that places the dub's end;
		// segment 0's offset belongs to a different part of the film.
		//
		// What this measures is the film's silent tail after the cut render: it
		// is NOT the residual drift (that is Fit) and NOT the mux offset (0 for
		// a tiled plan).
		var tailMS int64
		if track.DurationMS > 0 && len(report.Piecewise.Segments) > 0 {
			last := report.Piecewise.Segments[0]
			for _, seg := range report.Piecewise.Segments[1:] {
				if seg.EndSec > last.EndSec {
					last = seg
				}
			}
			dubSec := float64(track.DurationMS) / 1000
			tailSec := last.EndSec - (dubSec + last.OffsetMS/1000)
			tailMS = int64(math.Round(tailSec * 1000))
			// Under a second is padding noise and a negative tail means the
			// dub covers the film: neither is a finding, so the field stays
			// zero and omitempty drops the key.
			if tailMS < 1000 {
				tailMS = 0
			}
		}
		report.Application = &analyze.Application{
			Mode:          "piecewise",
			Note:          fmt.Sprintf("re-encoded (piecewise, %d segments)", len(segments)),
			SegmentCount:  len(segments),
			Segments:      report.Piecewise.Segments,
			TailSilenceMS: tailMS,
		}
	case piecewiseRejected(report):
		// Deliberately no cut and no tempo: the offset chosen below becomes the
		// plan's dominant single offset, and the gate refuses the job so the
		// operator sees report.review_reason (D5).
	case tempoEligible(report):
		report.Fit.TempoApplied = true
		plan = plan.WithTempo(report.Fit.TempoFactor)
		if !plan.Encode() {
			// No rule matched, so there is no codec to fold the factor into:
			// materialise a lossless one. FLAC is the only lossless encoder
			// this build always carries, so a correction nobody configured
			// cannot cost the dub any quality.
			plan = transcode.Plan{RuleIndex: transcode.NoRule, Codec: transcode.CodecFLAC}.WithTempo(report.Fit.TempoFactor)
		}
		// The rule path's encoder check ran before Execute (ADR-0010). This one
		// cannot: the factor does not exist until the report does.
		if available, detail := w.transcoder.EncoderAvailable(ctx, plan.Encoder()); !available {
			return atStage("encoder", fmt.Errorf("%w: tempo %.4f needs %s for audio track %d: %s",
				ErrEncoderUnavailable, report.Fit.TempoFactor, plan.Encoder(), rec.TargetTrackID, detail))
		}
		// The note names the factor to five decimals: the factors this slice
		// produces are 5-decimal numbers (0.95904 for the 25.000 -> 23.976
		// drift), and %.4f would print 0.9590 — a different number from the
		// one the encode was given.
		report.Application = &analyze.Application{
			Mode: "re-encoded",
			Note: fmt.Sprintf("re-encoded (tempo %.5f)", report.Fit.TempoFactor),
		}
	}
	if !muxableReport(report) {
		reason := strings.TrimSpace(report.Reason)
		if review := strings.TrimSpace(report.ReviewReason); review != "" {
			reason = strings.TrimSpace(reason + " — " + review)
		}
		return atStage("verdict", fmt.Errorf("%w: verdict %q (%s)", ErrUnmuxableVerdict, report.Verdict, reason))
	}

	// Execute persisted the report and offset; re-read for the canonical row.
	if rec, ok, err = w.store.SyncJobByID(ctx, id); err != nil {
		return atStage("reload", err)
	} else if !ok {
		return atStage("reload", fmt.Errorf("syncworker: job %d disappeared", id))
	}

	lib, ok, err := w.store.LibraryCacheJobTargetByID(ctx, rec.TargetLibraryCacheID)
	if err != nil {
		return atStage("target", err)
	}
	if !ok {
		return atStage("target", fmt.Errorf("syncworker: library target %d not found", rec.TargetLibraryCacheID))
	}
	targetPath := strings.TrimSpace(rec.TargetVideoPath)
	if targetPath == "" {
		targetPath = lib.TargetFilePath
	}

	audio, err := w.stagedAudio(id)
	if err != nil {
		return atStage("staging", err)
	}
	if plan.Encode() {
		if audio, err = w.transcodeStaged(ctx, id, audio, plan, segments, track); err != nil {
			return atStage("transcode", err)
		}
	}
	// #86: mkvmerge --sync shifts the whole track by one constant, so the
	// number to give it is the offset the tempo-corrected track still has at
	// t = 0 — the intercept, scaled by the factor the encode applied. The fit
	// measures the intercept on the UNCORRECTED timeline; the encode has
	// already stretched it by TempoFactor, so the residual constant is
	// intercept/TempoFactor, not the raw intercept (a 30 s intercept at
	// 5 ms/s would otherwise leave ~150 ms, well outside the 40 ms stable
	// band). The median is only the right constant for a timeline with no
	// trend: on a linear ramp it leaves half the drift in place. A report
	// without a fit (a 0.4.0 row) keeps the median.
	//
	// #84: a piecewise render needs no shift at all, and that is a claim about
	// the plan rather than about mkvmerge. concat stacks the runs in plan order,
	// so the track is aligned only because analyze tiles an accepted plan from 0
	// to the film's end: the first run really does land at time 0 and the last
	// one on the film's last frame. transcode refuses any other shape, which is
	// what keeps this 0 honest — with a plan that started at the first kept
	// window, concat would still place it at 0 and the published track would be
	// early by that much. The stored 0 plus application.mode is the signature of
	// that mechanism. A rejected segmentation keeps the median, and uses the
	// plan's dominant offset when analyze reported one: that is the 0.4.0
	// fallback D5 promises.
	offset := medianOffsetMS(report.OffsetsMS)
	switch {
	case len(segments) > 0:
		offset = 0
	case report.Fit != nil && report.Fit.TempoApplied && report.Fit.TempoFactor > 0:
		offset = int64(math.Round(report.Fit.InterceptMS / report.Fit.TempoFactor))
	case piecewiseRejected(report):
		offset = int64(math.Round(report.Piecewise.DominantOffsetMS))
	}
	rule, err := w.gateRule(ctx)
	if err != nil {
		return atStage("gate", err)
	}
	candidates, err := w.candidates(ctx, id, rec)
	if err != nil {
		return atStage("candidates", err)
	}

	if err := w.store.SetSyncJobStatus(ctx, id, statusMuxing, ""); err != nil {
		return atStage("status", err)
	}

	opts, _ := w.muxOptions(ctx)
	mode, outputDir, err := w.outputDestination(ctx, opts)
	if err != nil {
		return atStage("output", err)
	}
	res, err := w.muxer.Mux(ctx, mux.Job{
		ID:             id,
		WorkDir:        w.workDir,
		TargetPath:     targetPath,
		TargetLangISO2: rec.TargetLangISO2,
		AudioPath:      audio,
		SyncOffsetMS:   offset,
		OutputMode:     mode,
		OutputDir:      outputDir,
		Gate:           rule,
		Arr:            gate.ArrMetadata{Found: true, Tags: lib.ArrTagLabels, RootFolder: lib.RootFolderPath},
		Force:          rec.ForceIgnoreTagFilter || w.forceOverride(ctx),
		Report:         report,
		Candidates:     candidates,
	})
	if err != nil {
		// ADR-0018: the media manager replaced the target while the mux ran, so
		// Mux published nothing (no temp, no .bak, target untouched). That is
		// not a failure of this job — the upgrade has to be analysed against
		// the new file — so the row goes back to queued, with one log line, and
		// the pass skips it for the rest of its run instead of re-running it at
		// once.
		if errors.Is(err, mux.ErrTargetChanged) {
			// The requeue must outlive a shutdown cancellation: leaving the row
			// in "muxing" would strand it, since nothing reaps that status.
			if rerr := w.store.SetSyncJobStatus(context.WithoutCancel(ctx), id, statusQueued, ""); rerr != nil {
				return rerr
			}
			w.logf("syncworker: job %d target changed during the mux; requeued to run again against the new file", id)
			return errRequeued
		}
		// A mux sentinel — a non-Matroska container (mux.ErrUnsupportedContainer)
		// included — is terminal: it is returned unwrapped and ProcessOnce
		// records it as the job's failed status once, with no retry.
		return atStage("mux", err)
	}

	// ADR-0011: record where the file landed so the API/UI can show it. This
	// write must outlive a shutdown cancellation (the mux already published);
	// a failure is logged and never undoes the publish.
	if err := w.store.SetSyncJobOutputPath(context.WithoutCancel(ctx), id, res.OutputPath); err != nil {
		w.logf("syncworker: job %d output path: %v", id, err)
	}
	w.logf("syncworker: job %d mux published: %s (mode=%s, offset=%d ms)", id, res.OutputPath, mode, offset)

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
	//
	// The analysis source and its fallback travel from the row Execute wrote
	// (re-read above): the worker's own finish must not erase how the report
	// was produced.
	return atStage("finish", w.store.FinishSyncJobAnalysis(context.WithoutCancel(ctx), id, statusCompleted,
		string(report.SyncMethod), offset, report, "", rec.AnalysisSource, rec.AnalysisFallback))
}

// muxableVerdict accepts exactly the verdicts this worker may publish without a
// correction of its own. The classification word "stable" is tolerated because
// analyze emits it alongside verdict ok in older reports. A drifting verdict is
// not accepted here: [muxableReport] decides that case, because it depends on
// whether the drift was actually compensated.
func muxableVerdict(verdict string) bool {
	switch strings.ToLower(strings.TrimSpace(verdict)) {
	case analyze.VerdictOK, analyze.ClassificationStable:
		return true
	default:
		return false
	}
}

// muxableReport is the gate the job must clear before anything is encoded or
// muxed: the pre-#86 rules, plus one new case.
//
// A drifting verdict is publishable when the trend is a clean drift the encoder
// can represent and this worker has folded the correction into the job's single
// encode (Fit.TempoApplied). The mux then runs at intercept/TempoFactor and the
// row says so in report.application. Everything else keeps today's behaviour:
// a cut needs per-segment work, an unreliable or out-of-range report needs a
// human, and a drift whose factor the worker refused to apply must not be
// published with half the correction missing.
//
// #84 adds one refusal and no acceptance. An accepted segmentation whose cut
// was rendered is already muxable through muxableVerdict(ok) — the classification
// is `piecewise`, the verdict is `ok` — so nothing here needs to wave it
// through. A *rejected* one is refused even when the verdict looks acceptable:
// the analysis found a boundary it could not stand behind, and publishing the
// dominant offset as if the timeline were flat is exactly the guess D5 says
// must not happen. The operator sees review_reason instead.
func muxableReport(report analyze.Report) bool {
	if piecewiseRejected(report) {
		return false
	}
	if muxableVerdict(report.Verdict) {
		return true
	}
	if !strings.EqualFold(strings.TrimSpace(report.Verdict), analyze.VerdictDrifting) {
		return false
	}
	return report.Fit != nil && report.Fit.Shape == analyze.ShapeDrift && report.Fit.TempoApplied
}

// piecewiseCut reports whether the report carries a segmentation the job could
// cut on: the feature ran, nothing was rejected, and more than one run survived.
//
// A one-segment plan is an accepted plan and not a cut — the dense plan looked
// and found nothing to re-cut, which is the ordinary constant-offset job, and
// re-encoding it would cost the operator a lossless generation for no reason.
func piecewiseCut(report analyze.Report) bool {
	p := report.Piecewise
	return p != nil && p.RejectedReason == "" && len(p.Segments) > 1
}

// piecewiseRejected reports whether the segmenter refused a boundary it was not
// confident about. Such a report is never cut, never tempo-corrected and never
// published; it falls back to the dominant single offset, for review.
func piecewiseRejected(report analyze.Report) bool {
	p := report.Piecewise
	return p != nil && strings.TrimSpace(p.RejectedReason) != ""
}

// piecewiseRender is the one case this worker cuts and renders: an accepted
// segmentation with a real boundary, on the ok verdict the segmenter produced
// for it. Any other verdict means the report needs a human, so no encode is
// spent on it.
func piecewiseRender(report analyze.Report) bool {
	return piecewiseCut(report) && strings.EqualFold(strings.TrimSpace(report.Verdict), analyze.VerdictOK)
}

// toTranscodeSegments maps the report's segments onto the encoder's: the report
// keeps the evidence (confidence, points), the encoder needs only the window and
// the offset that places it.
func toTranscodeSegments(segs []analyze.Segment) []transcode.Segment {
	out := make([]transcode.Segment, len(segs))
	for i, s := range segs {
		out[i] = transcode.Segment{StartSec: s.StartSec, EndSec: s.EndSec, OffsetMS: s.OffsetMS}
	}
	return out
}

// tempoEligible reports whether the report describes a drift this worker can
// correct in one encode: a drifting verdict, a clean drift shape, a factor the
// encoder's range accepts.
//
// The verdict is part of the test, not just the shape. A cut must never be
// stretched, and an out_of_range or unreliable report also carries a fit:
// stretching a timeline whose offsets are already outside the checkpoint window,
// or whose matches the engine did not trust, would be guessing. An ok report is
// copied untouched exactly as before — it needs no correction.
func tempoEligible(report analyze.Report) bool {
	if report.Fit == nil || report.Fit.Shape != analyze.ShapeDrift {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(report.Verdict), analyze.VerdictDrifting) {
		return false
	}
	if report.Fit.TempoFactor == 0 {
		return false
	}
	// A report is data, and a factor outside the single encode's range is one
	// this release cannot honour — refuse rather than clamp.
	return transcode.ValidateTempo(report.Fit.TempoFactor) == nil
}

// nextQueued returns the lowest-id queued job that this pass has not visited
// yet. The exclusion is the whole point: a requeued row keeps its id and would
// otherwise be selected first on every iteration, starving everything behind
// it for the rest of the pass.
func (w *Worker) nextQueued(ctx context.Context, visited map[int64]bool) (int64, bool, error) {
	rows, err := w.store.DB.QueryContext(ctx,
		`SELECT id FROM sync_mux_jobs WHERE status = 'queued' ORDER BY id ASC`)
	if err != nil {
		return 0, false, fmt.Errorf("syncworker: read queued jobs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, false, fmt.Errorf("syncworker: read queued jobs: %w", err)
		}
		if !visited[id] {
			return id, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("syncworker: read queued jobs: %w", err)
	}
	return 0, false, nil
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

// reholdReason answers "would the gate refuse this job's target right now?" and
// returns the closed-vocabulary reason when it would, "" when the job may run.
//
// A forced job is never re-held: force_ignore_tag_filter is the operator's
// persisted "start it now" and outranks the gate everywhere (birth, projection,
// claim). A vanished target is missing_target rather than a tag verdict, so the
// UI can say the file is gone instead of asking for tags that cannot help.
func (w *Worker) reholdReason(ctx context.Context, rec store.SyncJobRecord) (string, error) {
	if rec.ForceIgnoreTagFilter {
		return "", nil
	}
	lib, hasLib, err := w.store.LibraryCacheJobTargetByID(ctx, rec.TargetLibraryCacheID)
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(rec.TargetVideoPath)
	if target == "" && hasLib {
		target = strings.TrimSpace(lib.TargetFilePath)
	}
	if target == "" {
		return store.HeldReasonMissingTarget, nil
	}
	if info, err := os.Stat(target); err != nil || !info.Mode().IsRegular() {
		return store.HeldReasonMissingTarget, nil
	}
	rule, err := w.gateRule(ctx)
	if err != nil {
		return "", err
	}
	if !rule.EligiblePath(target, gate.ArrMetadata{
		Found:      hasLib,
		Tags:       lib.ArrTagLabels,
		RootFolder: lib.RootFolderPath,
	}) {
		return store.HeldReasonWaitingForTags, nil
	}
	return "", nil
}

// forceOverride maps the appliance-wide security switch onto mux.Force. The
// per-job flag (migration 0008) is the operator's recorded override for one
// approval; this is the appliance-wide opt-in that also covers manual Studio
// jobs, which carry no approval to record it on. The mux is called with the OR
// of the two.
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

// muxOptions reads settings mux.options once per job. ok=false means the
// settings are unavailable, and the caller falls back to the shipped defaults
// (replace mode).
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
// Language and title survive because mux writes both onto the appended track —
// the language tag and that language's own name (U10) — so the encode does not
// have to carry them.
//
// segs, when non-empty, is the #84 piecewise render: the same one pass cuts,
// pads and concatenates the segments, which is why a piecewise job's log line
// names the segments instead of a rule the operator may not have written.
func (w *Worker) transcodeStaged(ctx context.Context, jobID int64, audio string, plan transcode.Plan, segs []transcode.Segment, track store.VaultTrackRecord) (string, error) {
	out := filepath.Join(w.workDir, "jobs", strconv.FormatInt(jobID, 10), "audio.transcoded.mka")
	label := encodeLabel(plan)
	if len(segs) > 0 {
		label = fmt.Sprintf("piecewise, %d segments", len(segs))
	}
	res, err := w.transcoder.Transcode(ctx, transcode.Request{
		Input:            audio,
		Output:           out,
		Plan:             plan,
		Segments:         segs,
		SourceChannels:   track.ChannelCount,
		SourceDurationMS: track.DurationMS,
		LangISO2:         track.LangISO2,
	})
	if err != nil {
		return "", fmt.Errorf("syncworker: job %d transcode (%s): %w", jobID, label, err)
	}
	w.logf("syncworker: job %d transcoded %s to %s (%d ch, %d bps)", jobID, label, res.Codec, res.Channels, res.BitrateBPS)
	return res.Path, nil
}

// encodeLabel names the encode a plan asks for, in job logs and errors: "rule N"
// when a rule produced it, "tempo-only" when the plan exists solely to correct a
// drift (RuleIndex == transcode.NoRule), plus the factor when a drift
// correction rides along with a rule. Without it, a tempo-only plan would log
// as "rule 0" — a rule the operator never wrote.
func encodeLabel(plan transcode.Plan) string {
	label := "tempo-only"
	if plan.RuleIndex != transcode.NoRule {
		label = fmt.Sprintf("rule %d", plan.RuleIndex+1)
	}
	if plan.TempoFactor != 0 && plan.TempoFactor != 1 {
		label += fmt.Sprintf(" + tempo %.4f", plan.TempoFactor)
	}
	return label
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

// stageError names the pipeline stage a job failure came from, so the failure
// line can say where the job stopped instead of only what broke. Unwrap keeps
// the mux sentinels and errRequeued reachable through errors.Is.
type stageError struct {
	stage string
	err   error
}

func (e *stageError) Error() string { return e.err.Error() }
func (e *stageError) Unwrap() error { return e.err }

// atStage tags err with the stage it failed at. A nil error stays nil.
func atStage(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &stageError{stage: stage, err: err}
}

// failureStage reports the stage a job error was tagged with ("unknown" for an
// error that never passed through atStage).
func failureStage(err error) string {
	var se *stageError
	if errors.As(err, &se) {
		return se.stage
	}
	return "unknown"
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
