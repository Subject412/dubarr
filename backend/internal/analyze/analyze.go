// Package analyze runs multi-point checkpoint drift analysis for a staged
// dubbing-sync job (PLAN.md M4-5).
//
// It slices both staged WAV sources at the configured checkpoint positions
// (sync.defaults.checkpoint_positions_pct), correlates each window through the
// M4-2 sync bridge, and aggregates a [Report]. Each slice is widened past
// checkpoint_window_sec to cover max_offset_sec (see [Analyze]). The report
// answers three questions the mux stage (M5) needs:
//
//   - What is the offset at each point, and how consistent is it? A constant
//     shift keeps the spread across checkpoints below 40 ms;
//     a larger spread means speed drift or a mid-movie edit.
//   - Does any point exceed sync.defaults.max_offset_sec? Then the verdict is
//     [VerdictOutOfRange] and M5 must fail loudly instead of muxing.
//   - Is any point's correlation weak enough to distrust? A checkpoint whose
//     correlation_quality falls below [LowQualityThreshold] makes the verdict
//     [VerdictUnreliable]: offsets beyond the searchable window surface as
//     boundary peaks with degraded quality, so a weak peak usually means the
//     reported offset is not a real match.
//   - Was the reference pair resolved as same_lang_reference or
//     lowpass_fallback? This comes from internal/reflang and travels in the
//     report so the accuracy of a low-pass match is visible downstream.
//
// The package performs no FFmpeg work and never writes files: callers stage
// the WAV windows (M4-3 Copy-on-Work) and own the sync job lifecycle.
package analyze

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/reflang"
	"github.com/Subject412/dubarr/backend/internal/sync"
)

// Defaults from the settled sync.defaults values.
const (
	// DefaultCheckpointWindowSec is the settled 150 s analysis window.
	DefaultCheckpointWindowSec = 150
	// DefaultMaxOffsetSec is the settled ±180 s search/acceptance bound; the
	// bound itself lives in sync.defaults.max_offset_sec, which also widens
	// the analysed slice the engine searches.
	DefaultMaxOffsetSec = 180
	// CheckWindowMarginSec widens the analysed slice beyond max_offset_sec so
	// the engine can actually reach the bound: the engine caps |lag| at the
	// slice length and only scores lags that keep at least 1/20 of the slice
	// overlapping, so a slice equal to max_offset_sec + 10 s still scores a
	// peak at the full bound (10 s of overlap vs the 9.5 s minimum).
	CheckWindowMarginSec = 10
)

// DefaultCheckpointPositionsPct is the settled 6-point sampling of the
// timeline.
var DefaultCheckpointPositionsPct = []int{10, 25, 40, 55, 70, 85}

// StableSpreadThresholdMS is the maximum checkpoint spread that still counts
// as a constant offset: "Spread across valid checkpoints < 40 ms -> final
// offset is median, speed_mode = 'none'". No other threshold is specified,
// so 40 ms is used verbatim.
const StableSpreadThresholdMS = 40.0

// LowQualityThreshold is the correlation_quality floor below which a
// checkpoint is not trusted. correlation_quality is now the peak's prominence
// over the median lag response ((peak-median)/(1-median)), not the raw NCC
// coefficient: measured on the acceptance fixtures it is 0.14..0.55 for real
// same-language podcast/dub material (Shrek, 2160p remux vs 1080p WEB-DL) and
// 0.02 for unrelated white noise, while the synthetic clean goldens sit at
// 0.80..1.00. 0.10 is chosen to sit clearly above the noise band while leaving
// headroom below the weakest genuine checkpoint; the former 0.25 was tuned for
// the raw coefficient and rejected valid real-world matches. A low-quality
// checkpoint is still treated as a non-match rather than a real offset and
// yields [VerdictUnreliable].
const LowQualityThreshold = 0.10

// Per-job engine timeout. One correlation job scans the whole widened slice
// (190 s with the settled defaults), which is far longer than sync.Runner's
// 30 s default: the engine must decode and correlate every lag at the source
// rate. The budget therefore scales with the slice instead of using one fixed
// value.
const (
	// JobTimeoutBaseMS is the fixed setup/startup allowance per job.
	JobTimeoutBaseMS = 30_000
	// JobTimeoutPer10SecMS adds one second per ten seconds of analysed slice.
	JobTimeoutPer10SecMS = 1_000
	// MinJobTimeoutMS floors the derived budget so short slices and debug
	// builds (unoptimised correlation) still fit comfortably.
	MinJobTimeoutMS = 120_000
)

// DefaultJobTimeout returns the per-checkpoint engine budget for a slice of
// sliceLenSec: JobTimeoutBaseMS + one second per ten seconds, floored at
// MinJobTimeoutMS. With the settled 190 s slice this is 120 s.
func DefaultJobTimeout(sliceLenSec float64) time.Duration {
	ms := JobTimeoutBaseMS + int(math.Ceil(sliceLenSec/10))*JobTimeoutPer10SecMS
	if ms < MinJobTimeoutMS {
		ms = MinJobTimeoutMS
	}
	return time.Duration(ms) * time.Millisecond
}

// JobTimeout is the effective per-checkpoint budget for cfg: the explicit
// [Config.JobTimeout] when set, otherwise [DefaultJobTimeout] for cfg's slice.
func JobTimeout(cfg Config) time.Duration {
	if cfg.JobTimeout > 0 {
		return cfg.JobTimeout
	}
	return DefaultJobTimeout(searchWindowSec(cfg))
}

// Parallelism ceiling and the measured cost model that the memory guard uses.
const (
	// MaxParallelCheckpoints is the hard upper bound on concurrent checkpoint
	// workers, whatever the setting says (the setting itself is 1..8,
	// U-DSP.8). At the modelled cost below, 8 workers is 3400 MiB, so on the
	// 4 GB deployment target the runtime ceiling — not this bound — is what
	// reduces the setting.
	MaxParallelCheckpoints = 8

	// CheckpointFixedBytes and CheckpointBytesPerWorker model one analysis'
	// resident cost as
	//
	//	total(n) = CheckpointFixedBytes + n x CheckpointBytesPerWorker
	//
	// Measured with /proc/self/status VmHWM taken before and after a
	// 6-checkpoint analysis of 190 s / 8 kHz slices through the real engine
	// (BenchmarkParallelCheckpoints; the run is recorded in
	// internal/dsp/PERF.md): the analysis grew the process by 511-516 MiB at
	// n=1 and by 1367-1609 MiB at n=4.
	//
	// A flat per-worker figure cannot describe both points: the n=1
	// measurement bundles the process's own footprint into its single
	// worker, so charging it per worker over-charges small n and
	// under-charges large n — the wrong direction for an OOM guard. Fitting
	// the two terms to the central values, 513 MiB at n=1 and 1488 MiB at
	// n=4:
	//
	//	perWorker = (1488 - 513) / 3 = 325 MiB
	//	base      = 513 - 325       = 188 MiB
	//
	// rounded up to 400 MiB and 200 MiB so the model is never optimistic:
	// it charges 600 MiB at n=1 (measured 511-516) and 1800 MiB at n=4
	// (measured 1367-1609). The base is the process's own footprint plus the
	// collector headroom that a single worker would otherwise be charged
	// for; the per-worker term is what each additional concurrent
	// correlation adds on top.
	CheckpointFixedBytes     = 200 << 20
	CheckpointBytesPerWorker = 400 << 20
)

// checkpointBytes is the modelled resident cost of n concurrent checkpoint
// workers: [CheckpointFixedBytes] once, plus [CheckpointBytesPerWorker] each.
func checkpointBytes(n int) uint64 {
	if n <= 0 {
		return 0
	}
	return CheckpointFixedBytes + uint64(n)*CheckpointBytesPerWorker
}

// cgroupRoot is the cgroup mount point the container's memory limit is read
// from. A variable, not a constant, so a test can point it at a temporary
// directory holding either candidate file.
var cgroupRoot = "/sys/fs/cgroup"

// cgroupUnlimited is the threshold above which a cgroup memory limit means "no
// limit": cgroup v1 writes a page-aligned sentinel just below 2^63, and no real
// container is 4 EiB.
const cgroupUnlimited = uint64(1) << 62

// memAvailable reports the *effective* memory limit in bytes: the container's
// cgroup limit when there is one, otherwise the kernel's MemAvailable. It is a
// variable, not a call, so a test can substitute a budget.
//
// It is deliberately not a bare /proc/meminfo read. dubarr ships in a
// container on a larger host, and MemAvailable there describes the host: a
// 4 GB container on a 128 GB TrueNAS box would report ~120 GB, the guard would
// permit every worker, and the container would be OOM-killed — the exact
// failure the guard exists to prevent.
var memAvailable = readEffectiveMemLimit

// readEffectiveMemLimit returns min(cgroup memory limit, MemAvailable), or
// whichever of the two exists, or false when there is neither.
func readEffectiveMemLimit() (uint64, bool) {
	return effectiveMemLimit(cgroupRoot, readMeminfoAvailable)
}

// effectiveMemLimit resolves the budget from root and meminfo, first source
// wins in this order: cgroup v2 /sys/fs/cgroup/memory.max, cgroup v1
// /sys/fs/cgroup/memory/memory.limit_in_bytes, then MemAvailable. When both a
// cgroup limit and MemAvailable are known the smaller wins, because inside a
// container both are real ceilings: the cgroup can be smaller than the host's
// free memory (the deployment that matters), and the host can be smaller than
// a generous cgroup.
func effectiveMemLimit(root string, meminfo func() (uint64, bool)) (uint64, bool) {
	limit, limited := cgroupMemoryLimit(root)
	available, ok := meminfo()
	switch {
	case limited && ok:
		return min(limit, available), true
	case limited:
		return limit, true
	case ok:
		return available, true
	}
	return 0, false
}

// cgroupMemoryLimit reads the container's memory limit, unified hierarchy
// first. A missing file, an unparseable one, `max`, or a v1 near-2^63 sentinel
// all fall through to the next source.
func cgroupMemoryLimit(root string) (uint64, bool) {
	for _, rel := range []string{"memory.max", "memory/memory.limit_in_bytes"} {
		if n, ok := cgroupLimitBytes(filepath.Join(root, rel)); ok {
			return n, true
		}
	}
	return 0, false
}

// cgroupLimitBytes parses one cgroup memory-limit file. "max", a missing or
// empty file, a zero limit and anything at or above [cgroupUnlimited] mean
// "no limit here", not "zero bytes", so the caller moves on to the next
// source instead of pinning the worker count to the floor.
func cgroupLimitBytes(path string) (uint64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "max" {
		return 0, false
	}
	n, err := strconv.ParseUint(text, 10, 64)
	if err != nil || n == 0 || n >= cgroupUnlimited {
		return 0, false
	}
	return n, true
}

// readMeminfoAvailable parses MemAvailable from /proc/meminfo.
func readMeminfoAvailable() (uint64, bool) {
	return procFieldBytes("/proc/meminfo", "MemAvailable")
}

// procFieldBytes reads a "Key: <n> kB" value from a /proc file as bytes.
func procFieldBytes(path, key string) (uint64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	prefix := key + ":"
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}

// parallelWorkers decides how many checkpoints this job may analyse at once.
//
// The order is fixed. First the setting is clamped to
// [1, min([MaxParallelCheckpoints], len(cfg.CheckpointPositionsPct))]; zero and
// negatives mean one. Then the memory ceiling reduces it until the modelled
// footprint of n workers — [checkpointBytes], i.e.
// [CheckpointFixedBytes] + n x [CheckpointBytesPerWorker] — fits in half of the
// effective memory limit, never below one: one worker is always allowed,
// because refusing to analyse at all is worse than exceeding the budget. When
// the limit is unknown (no cgroup limit and no /proc) the configured value
// stands.
//
// The limit is the *effective* one, not /proc/meminfo's MemAvailable: inside a
// container MemAvailable describes the host, so on a 4 GB container in a large
// TrueNAS box it would permit every worker and let the OOM killer decide. See
// [memAvailable] for the resolution order.
//
// What that yields, at 200 MiB + 400 MiB per worker and a budget of half the
// limit (8 workers models 3400 MiB, so below a 6.6 GiB limit it is the budget,
// not the setting, that decides):
//
//	container 1 GiB  → budget  512 MiB → 1 worker  (the floor)
//	container 2 GiB  → budget 1024 MiB → 2 workers (1000 MiB)
//	container 4 GiB  → budget 2048 MiB → 4 workers (1800 MiB; 5 would be 2200)
//	container 6 GiB  → budget 3072 MiB → 7 workers (3000 MiB) by budget
//	container 16 GiB → budget 8192 MiB → 8 workers (3400 MiB), the hard bound
//
// The checkpoint count caps all of that: the settled policy has six, so from 6
// GiB up a job with six checkpoints runs six workers whatever the budget would
// allow.
//
// Four workers need a container of 2 x 1800 MiB = 3600 MiB (~3.5 GiB), so a
// 4 GB container is that plus ~0.5 GiB of slack. The figures are reported, not
// tuned.
//
// A reduction is logged once, because it is the only case where the operator's
// setting is silently overridden.
func parallelWorkers(cfg Config) (n int) {
	n = cfg.ParallelCheckpoints
	if n < 1 {
		n = 1
	}
	if n > MaxParallelCheckpoints {
		n = MaxParallelCheckpoints
	}
	if n > len(cfg.CheckpointPositionsPct) {
		n = len(cfg.CheckpointPositionsPct)
	}
	if n <= 1 {
		return n
	}
	limit, ok := memAvailable()
	if !ok || limit == 0 {
		return n
	}
	configured := n
	budget := limit / 2
	for n > 1 && checkpointBytes(n) > budget {
		n--
	}
	if n < configured {
		slog.Default().Info(fmt.Sprintf("analyze: parallel checkpoints reduced %d → %d (memory budget)", configured, n))
	}
	return n
}

// Verdicts reported to the sync job status and consumed by M5.
const (
	VerdictOK         = "ok"
	VerdictDrifting   = "drifting"
	VerdictUnreliable = "unreliable"
	VerdictOutOfRange = "out_of_range"
)

// Classifications explain the verdict. They mirror the verdict names so a
// consumer can render either field without a lookup table.
const (
	ClassificationStable     = "stable"
	ClassificationDrifting   = "drifting"
	ClassificationUnreliable = "unreliable"
	ClassificationOutOfRange = "out_of_range"
)

// Engine is the one-job correlation seam. *sync.Runner satisfies it.
type Engine interface {
	Run(ctx context.Context, job sync.Job) (sync.Result, error)
}

// Config is the checkpoint policy, normally built from sync.defaults.
type Config struct {
	CheckpointPositionsPct []int `json:"checkpoint_positions_pct"`
	CheckpointWindowSec    int   `json:"checkpoint_window_sec"`
	MaxOffsetSec           int   `json:"max_offset_sec"`
	// ParallelCheckpoints is how many checkpoints one job analyses at once.
	// Zero means one. It is clamped to
	// [1, min([MaxParallelCheckpoints], len(CheckpointPositionsPct))] and then
	// reduced further by the memory ceiling (see [Analyze]); it never changes
	// the report.
	ParallelCheckpoints int `json:"-"`
	// JobTimeout bounds one engine job per checkpoint. Zero selects
	// [DefaultJobTimeout] for the analysed slice; callers that construct a
	// sync.Runner should pass the same value via sync.WithTimeout.
	JobTimeout time.Duration `json:"-"`
}

// DefaultConfig returns the settled sync.defaults checkpoint policy.
func DefaultConfig() Config {
	return Config{
		CheckpointPositionsPct: append([]int(nil), DefaultCheckpointPositionsPct...),
		CheckpointWindowSec:    DefaultCheckpointWindowSec,
		MaxOffsetSec:           DefaultMaxOffsetSec,
	}
}

func (c Config) validate() error {
	if len(c.CheckpointPositionsPct) == 0 {
		return fmt.Errorf("analyze: checkpoint_positions_pct is empty")
	}
	for _, pct := range c.CheckpointPositionsPct {
		if pct <= 0 || pct >= 100 {
			return fmt.Errorf("analyze: checkpoint position %d%% out of range (0,100)", pct)
		}
	}
	if c.CheckpointWindowSec <= 0 {
		return fmt.Errorf("analyze: checkpoint_window_sec must be positive")
	}
	if c.MaxOffsetSec <= 0 {
		return fmt.Errorf("analyze: max_offset_sec must be positive")
	}
	return nil
}

// Request is one staged job: the WORK_DIR copies plus the reference metadata
// that decides same-lang vs low-pass.
type Request struct {
	// TargetPath and DonorPath are WAV files staged by M4-3.
	TargetPath string
	DonorPath  string
	// DurationSec is the shorter source's duration. Zero means "read the WAV
	// headers of both staged files".
	DurationSec float64
	// Reference resolution inputs, forwarded to reflang.Resolve.
	TargetTracks     []probe.AudioTrack
	VaultTracks      []probe.AudioTrack
	Policy           reflang.Languages
	OriginalLanguage string
}

// Checkpoint is one correlation point in the report.
type Checkpoint struct {
	PositionPct   int     `json:"position_pct"`
	SliceStartSec float64 `json:"slice_start_sec"`
	SliceLenSec   float64 `json:"slice_len_sec"`
	OffsetMS      float64 `json:"offset_ms"`
	Quality       float64 `json:"correlation_quality"`
}

// Report is the persisted multi-point analysis (sync_jobs.checkpoints_json).
// OffsetsMS and Qualities are flat, checkpoint-ordered views for charting; the
// Checkpoints slice carries the window details.
type Report struct {
	Verdict        string         `json:"verdict"`
	OffsetsMS      []float64      `json:"offsets_ms"`
	Qualities      []float64      `json:"qualities"`
	MaxDriftMS     float64        `json:"max_drift_ms"`
	Classification string         `json:"classification"`
	Checkpoints    []Checkpoint   `json:"checkpoints"`
	SyncMethod     reflang.Method `json:"sync_method"`
	Reason         string         `json:"reason,omitempty"`
}

// Analyze resolves the reference strategy, runs one correlation per checkpoint,
// and aggregates the drift report.
//
// # One job, N checkpoints at a time
//
// A job analyses up to [Config.ParallelCheckpoints] checkpoints concurrently.
// That is concurrency *inside* one job and nothing more: the queue still runs
// one job at a time (syncworker's single ProcessOnce loop), so this function
// adds no job-level concurrency. It also adds no decoders — the staged decode
// stays exactly two sequential ffmpeg runs per job (syncjob), so "N concurrent
// checkpoints" means N concurrent reads and correlations of the already-staged
// WAVs, never N ffmpeg processes. *sync.Runner holds no per-job state, so one
// runner serves every worker.
//
// # The report is the same for any N
//
// Each worker writes only its own slot, indexed by checkpoint position, and the
// report is assembled from those slots in checkpoint order after the join.
// Nothing is appended, so [Report.Checkpoints], [Report.OffsetsMS] and
// [Report.Qualities] are byte-identical for any N, and aggregation ([classify])
// runs once, single-threaded, after the join.
//
// # Errors and the memory ceiling are deterministic too
//
// A failing checkpoint cancels the shared context so the rest stop early, the
// join waits for every worker, and the error returned is the lowest checkpoint
// index that failed — never whichever goroutine lost the race. A failure that
// is only the consequence of that cancellation is not a failure of its own
// checkpoint and never outranks a genuine one. Errors stay wrapped exactly as
// they were sequentially: "analyze: checkpoint %d%%: %w".
//
// Before dispatch the worker count is clamped (see [parallelWorkers]) and then
// reduced until the modelled footprint of n workers fits in half of the
// effective memory limit — the container's cgroup limit when there is one,
// otherwise /proc/meminfo's MemAvailable (Linux only, ADR-0016). Reading the
// cgroup limit matters because MemAvailable describes the host, not the
// container: on the 4 GB container in a larger TrueNAS host the clamp yields 4
// of the 8 workers (see [parallelWorkers] for the arithmetic). The reduction
// never goes below one worker and never changes the report.
//
// Each checkpoint window is centred on the settled pct position, but its
// length is searchWindowSec: the larger of checkpoint_window_sec and
// max_offset_sec + CheckWindowMarginSec (190 s with the settled defaults).
// The engine's search is bounded by the slice length, so a slice that stopped
// at the 150 s checkpoint window could never report an offset near the ±180 s
// bound and [VerdictOutOfRange] would be unreachable. WindowSec is set to the
// actual slice length, so the engine searches the whole analysed slice.
//
// Each checkpoint runs with its own deadline, [JobTimeout] (120 s for the
// settled slice, or [Config.JobTimeout] when set), derived from the shared
// context so one failure or a caller cancel reaches every worker. A
// sync.Runner built for the production chain must use the same value through
// sync.WithTimeout: the runner's own default is shorter than a 190 s
// correlation job.
func Analyze(ctx context.Context, engine Engine, cfg Config, req Request) (Report, error) {
	if engine == nil {
		return Report{}, fmt.Errorf("analyze: nil engine")
	}
	if err := cfg.validate(); err != nil {
		return Report{}, err
	}
	resolved := reflang.Resolve(req.TargetTracks, req.VaultTracks, req.Policy, req.OriginalLanguage)
	if len(resolved.Pairs) == 0 {
		reason := resolved.Reason
		if reason == "" {
			reason = "no reference pair"
		}
		return Report{}, fmt.Errorf("analyze: no reference strategy: %s", reason)
	}
	method := resolved.Pairs[0].Method

	duration := req.DurationSec
	if duration <= 0 {
		targetDur, err := wavDuration(req.TargetPath)
		if err != nil {
			return Report{}, fmt.Errorf("analyze: target duration: %w", err)
		}
		donorDur, err := wavDuration(req.DonorPath)
		if err != nil {
			return Report{}, fmt.Errorf("analyze: donor duration: %w", err)
		}
		duration = math.Min(targetDur, donorDur)
	}
	if duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) {
		return Report{}, fmt.Errorf("analyze: invalid duration %v", duration)
	}

	window := searchWindowSec(cfg)
	workers := parallelWorkers(cfg)
	pcts := cfg.CheckpointPositionsPct

	report := Report{
		SyncMethod:  method,
		Reason:      resolved.Pairs[0].Reason,
		Checkpoints: make([]Checkpoint, len(pcts)),
		OffsetsMS:   make([]float64, len(pcts)),
		Qualities:   make([]float64, len(pcts)),
	}

	runs, err := runCheckpoints(ctx, engine, cfg, req, duration, window, workers)
	if err != nil {
		return Report{}, err
	}

	// Aggregation happens once, here, single-threaded, in checkpoint order:
	// no part of the report is assembled inside a goroutine.
	for i, run := range runs {
		report.Checkpoints[i] = run.checkpoint
		report.OffsetsMS[i] = run.checkpoint.OffsetMS
		report.Qualities[i] = run.checkpoint.Quality
	}

	verdict, classification, drift := classify(report.OffsetsMS, report.Qualities, float64(cfg.MaxOffsetSec)*1000)
	report.Verdict = verdict
	report.Classification = classification
	report.MaxDriftMS = drift
	return report, nil
}

// checkpointRun is one checkpoint's slot in the dispatch. Exactly one worker
// writes a given slot and nothing else does, so the slice needs no lock: the
// join is the happens-before edge.
type checkpointRun struct {
	checkpoint Checkpoint
	// cause is the worker's failure, already wrapped as
	// "analyze: checkpoint %d%%: %w".
	cause error
	// casualty marks a cause that is only the consequence of the shared
	// context being cancelled by a sibling's failure (or by the caller), not
	// this checkpoint's own verdict.
	casualty bool
}

// runCheckpoints analyses len(cfg.CheckpointPositionsPct) checkpoints with at
// most workers of them in flight, and returns their slots. A non-nil error is
// the deterministic pick described on [Analyze]: the lowest checkpoint index
// that genuinely failed, else the lowest one a cancellation stopped.
func runCheckpoints(ctx context.Context, engine Engine, cfg Config, req Request, duration, window float64, workers int) ([]checkpointRun, error) {
	pcts := cfg.CheckpointPositionsPct
	runs := make([]checkpointRun, len(pcts))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// One buffered channel of checkpoint indices: the workers pull, so a
	// worker never blocks another one and finishing early costs nothing.
	queue := make(chan int, len(pcts))
	for i := range pcts {
		queue <- i
	}
	close(queue)

	// The internal sync package owns the name `sync` in this file, so the
	// join is a channel: every worker reports exactly once on its way out.
	done := make(chan struct{}, workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := range queue {
				pct := pcts[i]
				start, length := sliceWindow(duration, pct, window)
				search := length
				job := sync.Job{
					TargetPath:    req.TargetPath,
					DonorPath:     req.DonorPath,
					SliceStartSec: start,
					SliceLenSec:   length,
					WindowSec:     &search,
				}
				jobCtx, cancelJob := context.WithTimeout(runCtx, JobTimeout(cfg))
				result, err := engine.Run(jobCtx, job)
				cancelJob()
				if err != nil {
					// A checkpoint cancelled by a sibling's failure says
					// nothing about its own audio, so it must never outrank
					// a genuine failure recorded at a higher index. The
					// dispatch deliberately continues in that case: the
					// engine is contractually fast to give up on a cancelled
					// context, and running it is what lets every checkpoint
					// report its own failure.
					casualty := errors.Is(err, context.Canceled) && runCtx.Err() != nil
					runs[i] = checkpointRun{
						cause:    fmt.Errorf("analyze: checkpoint %d%%: %w", pct, err),
						casualty: casualty,
					}
					if !casualty {
						// First failure stops the rest. The index is
						// recorded, so *which* one stopped the job does not
						// matter.
						cancel()
					}
					return
				}
				runs[i] = checkpointRun{checkpoint: Checkpoint{
					PositionPct:   pct,
					SliceStartSec: start,
					SliceLenSec:   length,
					OffsetMS:      result.OffsetMS,
					Quality:       result.CorrelationQuality,
				}}
			}
		}()
	}
	for w := 0; w < workers; w++ {
		<-done
	}

	// Two deterministic passes over the slots, both in checkpoint order: a
	// genuine failure wins over a casualty of any index, and among each kind
	// the lowest index wins. Neither depends on which goroutine finished when.
	for _, run := range runs {
		if run.cause != nil && !run.casualty {
			return nil, run.cause
		}
	}
	for _, run := range runs {
		if run.cause != nil {
			return nil, run.cause
		}
	}
	// Nothing failed but the caller's context did: the analysis is not a
	// result, so report it the way a cancelled checkpoint would have been.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("analyze: checkpoint %d%%: %w", pcts[0], err)
	}
	return runs, nil
}

// searchWindowSec is the analysed slice length. It is at least the settled
// checkpoint window, but widened to max_offset_sec + CheckWindowMarginSec so
// the engine's lag search reaches the full offset bound.
func searchWindowSec(cfg Config) float64 {
	return math.Max(float64(cfg.CheckpointWindowSec), float64(cfg.MaxOffsetSec+CheckWindowMarginSec))
}

// sliceWindow places the checkpoint window at pct of duration, centred on the
// checkpoint and clamped into the file. For files shorter than the window the
// whole file is analysed.
func sliceWindow(duration float64, pct int, window float64) (start, length float64) {
	start = duration*float64(pct)/100 - window/2
	if start < 0 {
		start = 0
	}
	if start+window > duration {
		start = duration - window
	}
	if start < 0 {
		start = 0
	}
	length = math.Min(window, duration-start)
	return start, length
}

// classify implements the monotonic sanity check: a constant shift has a small
// spread across checkpoints, drift has a large one, and any point beyond the
// acceptance bound fails loudly. A checkpoint too weak to trust (quality below
// [LowQualityThreshold]) outranks drift but not an out-of-range offset, giving
// the precedence out_of_range > unreliable > drifting > ok.
func classify(offsets, qualities []float64, maxOffsetMS float64) (verdict, classification string, maxDrift float64) {
	if len(offsets) == 0 {
		return VerdictOutOfRange, ClassificationOutOfRange, 0
	}
	low, high := offsets[0], offsets[0]
	for _, off := range offsets[1:] {
		if off < low {
			low = off
		}
		if off > high {
			high = off
		}
	}
	maxDrift = high - low
	for _, off := range offsets {
		if math.Abs(off) > maxOffsetMS {
			return VerdictOutOfRange, ClassificationOutOfRange, maxDrift
		}
	}
	for _, q := range qualities {
		if q < LowQualityThreshold {
			return VerdictUnreliable, ClassificationUnreliable, maxDrift
		}
	}
	if maxDrift >= StableSpreadThresholdMS {
		return VerdictDrifting, ClassificationDrifting, maxDrift
	}
	return VerdictOK, ClassificationStable, maxDrift
}
