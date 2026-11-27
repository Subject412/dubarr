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
// The package performs no FFmpeg work itself and writes no files: the
// [Slicer] in [Config] produces the audio each checkpoint reads (the default
// whole-file slicer simply names the staged WAVs), and callers own the sync
// job lifecycle and the staging (M4-3 Copy-on-Work).
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

// Thresholds is the one table every verdict limit comes from. Re-tuning the
// engine is an edit here, not a hunt through [classify].
//
// The two 0.4.0 limits keep their constants — [StableSpreadThresholdMS] and
// [LowQualityThreshold] are referenced by the fallback policy and by the
// acceptance tests — and the table takes its values from them, so there is
// still exactly one definition of each number. The two z bands are new in 0.5.0
// and live only here; they are engine policy, echoed in every report, and
// deliberately not settings (a second source of truth for a number that must be
// re-tuned from evidence is worse than a Go literal).
type Thresholds struct {
	// StableSpreadMS is the maximum checkpoint spread that still counts as a
	// constant offset band.
	StableSpreadMS float64 `json:"stable_spread_ms"`
	// LowQuality is the correlation_quality floor below which a checkpoint is
	// not trusted: the 0.4.0 gate.
	LowQuality float64 `json:"low_quality"`
	// MatchZMaybe is the z below which a weak checkpoint "needs a human look".
	MatchZMaybe float64 `json:"match_z_maybe"`
	// MatchZGood is the z at or above which a match is "likely correct".
	MatchZGood float64 `json:"match_z_good"`
	// FitR2Min is the coefficient of determination a timeline needs before the
	// fit is allowed to call it a line (a drift or a constant shift) rather
	// than a mixed timeline.
	FitR2Min float64 `json:"fit_r2_min"`
	// DriftSlopeFloorMSPerSec is the slope below which a good line is a
	// constant shift, not a drift. 1 ms/s is 0.1 %: the six-point Theil–Sen
	// estimate's standard error is orders of magnitude below an FPS mismatch
	// (10 ms of noise over a 4050 s span is ~0.002 ms/s), so the floor only
	// ever suppresses noise, never a real ramp.
	DriftSlopeFloorMSPerSec float64 `json:"drift_slope_floor_ms_per_sec"`
	// AboveDriftResidualMS is the residual above which a checkpoint's
	// disagreement with the fitted trend is reported as a fact about the
	// material rather than as decode noise: four times the stable band.
	//
	// Nothing reads it in 0.5.0 slice 02; it is the table's slot for the
	// above_drift technique (slice 03), introduced here so the table stays the
	// one place limits live.
	AboveDriftResidualMS float64 `json:"above_drift_residual_ms"`
}

// DefaultThresholds is the table that judges every report, and the table a
// report echoes so an operator reading an old job can see which limits applied.
var DefaultThresholds = Thresholds{
	StableSpreadMS: StableSpreadThresholdMS,
	LowQuality:     LowQualityThreshold,
	MatchZMaybe:    5.0,
	MatchZGood:     10.0,

	FitR2Min:                0.98,
	DriftSlopeFloorMSPerSec: 1.0,
	AboveDriftResidualMS:    4 * StableSpreadThresholdMS,
}

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
//
// ClassificationPiecewise is the one 0.5.0 addition: a job whose re-cut was
// segmented and accepted is `ok`, and the classification says why the offsets
// were not one number. A consumer that switches on classification (the UI is
// the only one) must render it.
const (
	ClassificationStable     = "stable"
	ClassificationDrifting   = "drifting"
	ClassificationUnreliable = "unreliable"
	ClassificationOutOfRange = "out_of_range"
	ClassificationPiecewise  = "piecewise"
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
	// Slicer produces the audio each checkpoint's engine job reads. Nil
	// selects a [wholeFileSlicer] over Request's paths, which is the
	// whole-file behaviour. It must be safe for concurrent use (see
	// [Slicer]).
	Slicer Slicer `json:"-"`
	// AnalysisSource is [AnalysisSourceWindows], [AnalysisSourceFull], or
	// empty for full. It is not a report input: it decides whether the
	// duration must come from the request rather than from the staged WAVs
	// (see [ErrNoDuration]).
	AnalysisSource string `json:"-"`
	// Strategy is the technique pipeline. The zero value means
	// [DefaultStrategy], so a caller that has never heard of a strategy gets
	// 0.4.0's behaviour: one waveform entry on every checkpoint.
	Strategy Strategy `json:"strategy"`
	// Engines maps a technique onto the seam that runs it. A missing (or nil)
	// key means "this build cannot run that technique": [Analyze] skips it and
	// says so in the report, rather than failing every job until slice 04
	// registers its engine. [Analyze]'s own engine argument satisfies
	// — and, when unset here, fills in — the waveform entry.
	Engines map[TechniqueID]Engine `json:"-"`

	// Piecewise is the segmentation policy for this job (#84). Its zero value
	// is the shipped one: off. When it is left at the zero value entirely,
	// [Analyze] takes the policy from [Strategy.Piecewise] instead — the
	// settings path stores the block inside the section, and a job must not
	// silently ignore a switch the operator turned on.
	Piecewise PiecewisePolicy `json:"piecewise"`
}

// windowed reports whether cfg asks for windows cut at the planned positions
// instead of the whole decoded sources. Only the explicit windows value does;
// empty, full and anything unrecognised mean the whole-file path.
func (c Config) windowed() bool {
	return c.AnalysisSource == AnalysisSourceWindows
}

// slicer returns the slicer to run the plan against: cfg's own when set, else
// the whole-file identity over the request's staged paths.
func (c Config) slicer(req Request) Slicer {
	if c.Slicer != nil {
		return c.Slicer
	}
	return wholeFileSlicer{TargetPath: req.TargetPath, DonorPath: req.DonorPath}
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
	// DurationSec is the window plan's duration base: how long the shorter
	// analysis input really is, in seconds. It is required when
	// Config.AnalysisSource is windows; the whole-file path may leave it zero
	// and have it derived from the headers of the two staged WAVs, which only
	// exist after a full decode. Zero is never a valid duration for a window
	// plan — it would clamp every checkpoint to the start of the file.
	DurationSec float64
	// Reference resolution inputs, forwarded to reflang.Resolve.
	TargetTracks     []probe.AudioTrack
	VaultTracks      []probe.AudioTrack
	Policy           reflang.Languages
	OriginalLanguage string
}

// Attempt is one technique's answer on one checkpoint, whether or not it was
// good enough to be used.
//
// It is recorded for every technique that ran, so a report can explain why a
// checkpoint fell through to the next step and which answer won. Error is the
// engine's own failure, verbatim: an attempt that errored carries no offset, and
// a zero offset is never substituted for one.
type Attempt struct {
	Technique   string  `json:"technique"`
	OffsetMS    float64 `json:"offset_ms"`
	Quality     float64 `json:"quality"`
	MatchZ      float64 `json:"match_z"`
	PrecisionMS float64 `json:"precision_ms"`
	// OffsetSubMS is the interpolated sub-sample part of OffsetMS, carried
	// per attempt so the winning answer's own value reaches the checkpoint.
	OffsetSubMS float64 `json:"offset_sub_ms"`
	Accepted    bool    `json:"accepted"`
	Error       string  `json:"error,omitempty"`
}

// Checkpoint is one correlation point in the report.
//
// OffsetSubMS, MatchZ and PrecisionMS are the 0.5.0 diagnostics and are never
// omitempty: a new report always carries them, and 0 is the documented "not
// recorded" sentinel a 0.4.0 row decodes to. MatchZ in particular is 0 both for
// "scored no z" and for "written before 0.5.0", and both mean the same thing to
// the verdict: no z evidence.
//
// Technique names the winning entry and Attempts carries every technique that
// ran here (#82). Both are additive: a 0.4.0 row decodes with an empty
// technique and no attempts, which the UI renders as the old single-line row.
type Checkpoint struct {
	PositionPct   int       `json:"position_pct"`
	SliceStartSec float64   `json:"slice_start_sec"`
	SliceLenSec   float64   `json:"slice_len_sec"`
	OffsetMS      float64   `json:"offset_ms"`
	Quality       float64   `json:"correlation_quality"`
	OffsetSubMS   float64   `json:"offset_sub_ms"`
	MatchZ        float64   `json:"match_z"`
	PrecisionMS   float64   `json:"precision_ms"`
	Technique     string    `json:"technique"`
	Attempts      []Attempt `json:"attempts,omitempty"`
}

// Report is the persisted multi-point analysis (sync_jobs.checkpoints_json).
// OffsetsMS and Qualities are flat, checkpoint-ordered views for charting; the
// Checkpoints slice carries the window details.
//
// MatchZ is the same flat view of the per-checkpoint z, and it is not
// omitempty: `len(MatchZ) == len(OffsetsMS)` is how a consumer tells a 0.5.0
// report with z = 0 from a 0.4.0 report with no z at all. PrecisionMS is the
// coarsest precision among the checkpoints (the largest sample period, so a
// report never claims more resolution than its worst checkpoint), and
// Thresholds is the table that judged this report.
//
// Fit is the trend through the same offsets (#86) and is omitempty: a 0.4.0
// row was never fitted, so the key is absent and a reader can tell "never
// measured" from "measured as flat". Application records what was done to the
// dub copy before the mux and is likewise absent on a plain copy, so the row
// can say "re-encoded (tempo 0.95904)" instead of leaving the operator to infer
// it from an offset of 0.
type Report struct {
	Verdict        string         `json:"verdict"`
	OffsetsMS      []float64      `json:"offsets_ms"`
	Qualities      []float64      `json:"qualities"`
	MaxDriftMS     float64        `json:"max_drift_ms"`
	Classification string         `json:"classification"`
	Checkpoints    []Checkpoint   `json:"checkpoints"`
	SyncMethod     reflang.Method `json:"sync_method"`
	Reason         string         `json:"reason,omitempty"`
	MatchZ         []float64      `json:"match_z"`
	PrecisionMS    float64        `json:"precision_ms"`
	Thresholds     Thresholds     `json:"thresholds"`
	Fit            *LineFit       `json:"fit,omitempty"`
	Application    *Application   `json:"application,omitempty"`
	// Strategy is what this job was asked to run, so a report is
	// self-describing: two reports are comparable only when the same
	// techniques produced them.
	Strategy Strategy `json:"strategy"`
	// Piecewise is the segmentation result (#84) and is absent unless the
	// feature ran: a job with the shipped strategy has no key at all, which is
	// how a reader tells "piecewise was off" from "piecewise ran and found
	// nothing".
	Piecewise *Piecewise `json:"piecewise,omitempty"`
	// ReviewReason says, in one operator-facing sentence, why a job needs a
	// human look even though the verdict alone does not demand it — today only
	// "the cut was not confident, the dominant offset was applied" (D5). It is
	// distinct from Reason, which belongs to the reference-language resolver.
	ReviewReason string `json:"review_reason,omitempty"`
	// Notes explains a configured technique that did not run — this build has
	// no engine for it — so a strategy naming `mfcc` says why it was ignored
	// instead of quietly doing less. Empty (and absent) for the shipped
	// strategy, which always runs.
	Notes []string `json:"notes,omitempty"`
}

// Application records what was done to the dub copy before the mux, so the job
// row can say it in words instead of leaving the operator to infer it from an
// offset of 0.
//
// The segment fields are part of the shape from the start (D2b): slice 05
// populates them for a piecewise render and nothing here needs a second shape
// or an additive pass on a shipped field. Slice 02 leaves them zero and fills
// only Mode and Note for a tempo re-encode.
type Application struct {
	Mode string `json:"mode"` // "copy" | "re-encoded"
	Note string `json:"note"` // "copied untouched", "re-encoded (tempo 0.95904)"
	// SegmentCount and Segments describe a piecewise render: how many runs of
	// the film were cut and placed separately.
	SegmentCount int       `json:"segment_count,omitempty"`
	Segments     []Segment `json:"segments,omitempty"`
}

// Segment, and the rest of the piecewise shape, live in segment.go with the
// segmenter that produces them (D2b: one shape, populated by slice 05).

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
// max_offset_sec + CheckWindowMarginSec (190 s with the settled defaults). The
// geometry itself is [PlanWindows] and only [PlanWindows]: one start is
// clamped into the shorter input, both sources receive that same film
// position, and every boundary is snapped to a whole sample at
// [analysisSampleRate]. The engine's search is bounded by the slice length, so
// a slice that stopped at the 150 s checkpoint window could never report an
// offset near the ±180 s bound and [VerdictOutOfRange] would be unreachable.
// WindowSec is set to the planned length, so the engine searches the whole
// analysed slice.
//
// # Report position and engine offset
//
// Each checkpoint asks cfg's [Slicer] for the audio it must read and builds
// the engine job from that [Slice] — its two paths, its EngineStartSec and its
// LenSec. The report, in turn, states the plan's film position and length
// ([Window.FilmStartSec], [Window.LenSec]). The two are deliberately not the
// same number: in windowed mode the slice starts at a pre-roll inside a small
// window file, and reporting that as the checkpoint's position is what made
// every window look like it sat at the start of the film. A nil Slicer means a
// [wholeFileSlicer] over Request's paths, whose engine start is the film
// position because a fully decoded WAV begins at it.
//
// The engine's echo of what it analysed (sync.Analyzed.Len) is read for one
// purpose only: a checkpoint whose slice came back more than one sample shorter
// than planned fails with [ErrShortWindow], checkpoint-indexed, and no report
// is returned. It never contributes a position.
//
// # A windowed plan needs a duration
//
// When cfg.AnalysisSource is [AnalysisSourceWindows], Request.DurationSec must
// be positive and is used verbatim: the percentages are taken of it rather than
// of a staged WAV, which a windowed decode does not produce. A missing duration
// is [ErrNoDuration] before any slicer call, instead of six checkpoints clamped
// to 0 that would agree with each other and look stable. On the whole-file path
// a zero duration is still derived from the two staged WAV headers.
//
// Each checkpoint runs with its own deadline, [JobTimeout] (120 s for the
// settled slice, or [Config.JobTimeout] when set), derived from the shared
// context so one failure or a caller cancel reaches every worker. A
// sync.Runner built for the production chain must use the same value through
// sync.WithTimeout: the runner's own default is shorter than a 190 s
// correlation job.
func Analyze(ctx context.Context, engine Engine, cfg Config, req Request) (Report, error) {
	if engine == nil && len(cfg.Engines) == 0 {
		return Report{}, fmt.Errorf("analyze: nil engine")
	}
	if err := cfg.validate(); err != nil {
		return Report{}, err
	}
	// The strategy and the engines are resolved once, here, before a single
	// checkpoint runs: a zero strategy is the shipped one, the engine argument
	// is the waveform engine, and a technique this build cannot run is dropped
	// with a note instead of being discovered per checkpoint.
	strategy := cfg.Strategy.normalised()
	if err := strategy.Validate(); err != nil {
		return Report{}, fmt.Errorf("analyze: %w", err)
	}
	cfg.Strategy = strategy
	// One piecewise policy for the job. Config's own copy wins when it names
	// one; otherwise the strategy's — the settings path stores the block inside
	// the section, so a switch the operator turned on must not be ignored here.
	// The strategy itself is left exactly as the caller passed it: the report's
	// `strategy` block is a record of the request, and `piecewise` beside it is
	// the record of what the request did.
	if cfg.Piecewise == (PiecewisePolicy{}) {
		cfg.Piecewise = strategy.Piecewise
	}
	cfg.Piecewise = cfg.Piecewise.Normalised()
	if err := cfg.Piecewise.validate(); err != nil {
		return Report{}, fmt.Errorf("analyze: %w", err)
	}
	cfg.Engines = mergeEngines(cfg.Engines, engine)
	if len(cfg.entries()) == 0 {
		// Every enabled technique is unavailable: that is a composition-root
		// bug, not a settings row, so it is loud and immediate — six
		// checkpoints failing one by one would say much less.
		return Report{}, fmt.Errorf("analyze: no enabled technique has an engine")
	}
	// A windowed plan is a percentage of the probed duration, so a missing
	// one must fail before any slicer is asked for audio: the alternative is
	// six checkpoints clamped to 0 that look like a stable report.
	if cfg.windowed() && req.DurationSec <= 0 {
		return Report{}, fmt.Errorf("analyze: analysis_source %s: %w (got %v)",
			cfg.AnalysisSource, ErrNoDuration, req.DurationSec)
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
	targetDur, donorDur := duration, duration
	if duration <= 0 {
		var err error
		targetDur, err = wavDuration(req.TargetPath)
		if err != nil {
			return Report{}, fmt.Errorf("analyze: target duration: %w", err)
		}
		donorDur, err = wavDuration(req.DonorPath)
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

	// One plan for the whole job, computed once: the film positions and
	// lengths every checkpoint reports, and the correction that would be
	// applied to each source's seek. The plan is the same object both
	// analysis sources consume, which is what keeps their reports
	// comparable; where each source's audio actually starts inside the files
	// it reads is the slicer's business (see [Slicer]).
	//
	// #84 replaces the shipped six percentages with a uniform dense grid when
	// piecewise is on. Six checkpoints cannot locate a boundary; a grid cannot
	// miss one by more than a step. The shipped path is untouched, so a job
	// with piecewise off plans exactly what it planned before.
	timing := Timing{
		Target: SourceTiming{DurationSec: targetDur, OriginKnown: true},
		Donor:  SourceTiming{DurationSec: donorDur, OriginKnown: true},
	}
	var (
		plan []Window
		err  error
	)
	if cfg.Piecewise.Enabled {
		plan, err = PlanWindowsEvery(timing, cfg.Piecewise.StepSec, cfg.Piecewise.WindowSec)
	} else {
		plan, err = PlanWindows(timing, pcts, window)
	}
	if err != nil {
		return Report{}, err
	}

	report := Report{
		SyncMethod:  method,
		Reason:      resolved.Pairs[0].Reason,
		Checkpoints: make([]Checkpoint, len(plan)),
		OffsetsMS:   make([]float64, len(plan)),
		Qualities:   make([]float64, len(plan)),
		MatchZ:      make([]float64, len(plan)),
		Thresholds:  DefaultThresholds,
		Strategy:    cfg.Strategy,
	}
	for _, id := range cfg.skipped() {
		report.Notes = append(report.Notes,
			fmt.Sprintf("technique %q skipped: no engine registered", id))
	}

	slicer := cfg.slicer(req)
	runs, err := runCheckpoints(ctx, cfg, slicer, plan, workers)
	if err != nil {
		return Report{}, err
	}
	// Phase 2: the drift-driven second pass, which runs only when a strategy
	// names an `above_drift` technique (normally: never) and reads every
	// checkpoint's first answer to find the outliers. It is one sequential
	// pass, before the aggregation, so the report states the final answers.
	if err := applyAboveDrift(ctx, cfg, slicer, plan, runs); err != nil {
		return Report{}, err
	}

	// Aggregation happens once, here, single-threaded, in checkpoint order:
	// no part of the report is assembled inside a goroutine.
	for i, run := range runs {
		report.Checkpoints[i] = run.checkpoint
		report.OffsetsMS[i] = run.checkpoint.OffsetMS
		report.Qualities[i] = run.checkpoint.Quality
		report.MatchZ[i] = run.checkpoint.MatchZ
		// The coarsest answer wins: a report must never claim more
		// resolution than its worst checkpoint, and a checkpoint contributes
		// the precision of the technique that answered it.
		if run.checkpoint.PrecisionMS > report.PrecisionMS {
			report.PrecisionMS = run.checkpoint.PrecisionMS
		}
	}

	// The trend is fitted after the checkpoints are aggregated and before the
	// verdict is decided: it describes the same offsets the verdict is about,
	// and the worker reads both together to decide whether the job is muxable.
	fit := FitTimeline(plan, report.OffsetsMS, report.Qualities, report.MatchZ, DefaultThresholds)
	report.Fit = &fit

	// Segmentation runs after the pipeline (every checkpoint answered) and
	// after the fit (the report keeps the shape of the raw timeline, so a
	// reader still sees the cut even once the verdict says `ok`): the two are
	// different statements about the same evidence.
	var piecewise *Piecewise
	if cfg.Piecewise.Enabled {
		built := BuildSegments(plan, report.OffsetsMS, report.Qualities, cfg.Piecewise, DefaultThresholds)
		piecewise = &built
		report.Piecewise = piecewise
		if built.RejectedReason != "" {
			// A rejected plan is not a failure of the job (D5): the dominant
			// offset is applied and the row says why a human should look.
			report.ReviewReason = fmt.Sprintf("cut not confident — applied the dominant offset (%s)", built.RejectedReason)
		}
	}

	verdict, classification, drift := classify(report.OffsetsMS, report.Qualities, report.MatchZ,
		piecewise, float64(cfg.MaxOffsetSec)*1000, DefaultThresholds)
	report.Verdict = verdict
	report.Classification = classification
	report.MaxDriftMS = drift
	return report, nil
}

// checkpointRun is one checkpoint's slot in the dispatch. Exactly one worker
// writes a given slot and nothing else does, so the slice needs no lock: the
// join is the happens-before edge.
//
// The slot carries the whole [Checkpoint], attempts included: a technique runs
// in the worker that owns the checkpoint, so nothing is appended from a
// goroutine into shared state and the aggregation loop copies the attempts in
// checkpoint order after the join.
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

// runCheckpoints analyses one planned window per checkpoint position with at
// most workers of them in flight, and returns their slots. A non-nil error is
// the deterministic pick described on [Analyze]: the lowest checkpoint index
// that genuinely failed, else the lowest one a cancellation stopped.
//
// One slicer call per checkpoint, from the worker that owns that checkpoint:
// the Slicer contract makes that safe for any worker count, and the worker
// writes only its own slot. Every technique on that checkpoint then reads the
// same slice (see [evaluateCheckpoint]).
func runCheckpoints(ctx context.Context, cfg Config, slicer Slicer, plan []Window, workers int) ([]checkpointRun, error) {
	runs := make([]checkpointRun, len(plan))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// One buffered channel of checkpoint indices: the workers pull, so a
	// worker never blocks another one and finishing early costs nothing.
	queue := make(chan int, len(plan))
	for i := range plan {
		queue <- i
	}
	close(queue)

	// The internal sync package owns the name `sync` in this file, so the
	// join is a channel: every worker reports exactly once on its way out.
	// The Slicer is one shared instance and must be safe for that (see
	// [Slicer]); one call is made per checkpoint, from the worker that owns
	// it.
	done := make(chan struct{}, workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := range queue {
				// The window is data, planned once for the whole job: the
				// report takes its film position and the engine takes the
				// offset and length of the audio the slicer handed it, and
				// neither is derived from the other.
				window := plan[i]
				checkpoint, err := evaluateCheckpoint(runCtx, cfg, slicer, window)
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
						cause:    fmt.Errorf("analyze: checkpoint %d%%: %w", window.PositionPct, err),
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
				runs[i] = checkpointRun{checkpoint: checkpoint}
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
		return nil, fmt.Errorf("analyze: checkpoint %d%%: %w", plan[0].PositionPct, err)
	}
	return runs, nil
}

// runJob is one correlation over one already-cut slice: the engine call, its
// per-job timeout, and the echo check that the engine saw the audio the plan
// asked for. [evaluateCheckpoint] runs it once per technique on the same slice,
// so every technique on a checkpoint reads exactly the audio the plan cut.
func runJob(ctx context.Context, engine Engine, cfg Config, slice Slice, w Window) (sync.Result, error) {
	length := slice.LenSec
	job := sync.Job{
		TargetPath:    slice.TargetPath,
		DonorPath:     slice.DonorPath,
		SliceStartSec: slice.EngineStartSec,
		SliceLenSec:   slice.LenSec,
		WindowSec:     &length,
	}
	jobCtx, cancelJob := context.WithTimeout(ctx, JobTimeout(cfg))
	defer cancelJob()

	result, err := engine.Run(jobCtx, job)
	if err != nil {
		return sync.Result{}, err
	}
	// The echo is used for exactly one thing: did the engine see the audio
	// the plan asked for? One sample of slack absorbs rounding; anything
	// more is a slice that lost material, whose peak is not data.
	if result.Analyzed.Len < w.LenSec-1.0/analysisSampleRate {
		return sync.Result{}, fmt.Errorf("%w: analyzed.len %.6f s, planned %.6f s",
			ErrShortWindow, result.Analyzed.Len, w.LenSec)
	}
	return result, nil
}

// searchWindowSec is the analysed slice length. It is at least the settled
// checkpoint window, but widened to max_offset_sec + CheckWindowMarginSec so
// the engine's lag search reaches the full offset bound.
func searchWindowSec(cfg Config) float64 {
	return math.Max(float64(cfg.CheckpointWindowSec), float64(cfg.MaxOffsetSec+CheckWindowMarginSec))
}

// classify implements the monotonic sanity check: a constant shift has a small
// spread across checkpoints, drift has a large one, and any point beyond the
// acceptance bound fails loudly. A checkpoint too weak to trust outranks drift
// but not an out-of-range offset, giving the precedence
//
//	out_of_range > piecewise-ok > unreliable > drifting > ok
//
// The piecewise rule is slice 05's one addition and it sits *below*
// out_of_range on purpose: an out-of-range checkpoint is out of range whatever
// the segmentation says about the rest. When the plan was accepted, the verdict
// is `ok` and the classification says why (`piecewise`), and the drift reported
// is the residual left by the segmentation — each point against its own
// segment's median — not the step it removed, so a successful re-cut job reads
// as a few milliseconds rather than as 2.5 s.
//
// Every limit comes from th (see [Thresholds]); no literal survives here, so
// re-tuning is one edit to [DefaultThresholds].
//
// # The unreliable rule is a conjunction
//
// A checkpoint is unreliable when its quality is below th.LowQuality **and** its
// match z dissents (or no z was recorded, which is every 0.4.0 row):
//
//	qualities[i] < th.LowQuality && (no z at i || matchZ[i] < th.MatchZMaybe)
//
// D2 requires both that the verdicts start using the new number and that an
// item that was `ok` in 0.4.0 cannot silently reclassify. Under the conjunction
// that second requirement holds by construction: a checkpoint 0.4.0 called fine
// has quality >= th.LowQuality, so the old gate fails first and the new number
// can only confirm distrust the old one already had. A pure-z rule would
// reclassify 0.4.0 rows, which is exactly what D2 forbids.
//
// "no z recorded" means len(matchZ) <= i or matchZ[i] == 0: a zero z is the
// documented sentinel for "not measured", so a legacy report behaves exactly as
// it did in 0.4.0.
func classify(offsets, qualities, matchZ []float64, piecewise *Piecewise, maxOffsetMS float64, th Thresholds) (verdict, classification string, maxDrift float64) {
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
	if piecewise.accepted() {
		// The step the raw spread measures is what the segmentation explains
		// away, so the honest number is the residual it leaves behind.
		return VerdictOK, ClassificationPiecewise, piecewise.maxResidualMS
	}
	for i, q := range qualities {
		if q >= th.LowQuality {
			continue
		}
		if len(matchZ) <= i || matchZ[i] == 0 || matchZ[i] < th.MatchZMaybe {
			return VerdictUnreliable, ClassificationUnreliable, maxDrift
		}
	}
	if maxDrift >= th.StableSpreadMS {
		return VerdictDrifting, ClassificationDrifting, maxDrift
	}
	return VerdictOK, ClassificationStable, maxDrift
}
