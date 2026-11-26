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
	"fmt"
	"math"
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

// Analyze resolves the reference strategy, runs one correlation per checkpoint
// sequentially, and aggregates the drift report.
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
// settled slice, or [Config.JobTimeout] when set). A sync.Runner built for the
// production chain must use the same value through sync.WithTimeout: the
// runner's own default is shorter than a 190 s correlation job.
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
	report := Report{
		SyncMethod:  method,
		Reason:      resolved.Pairs[0].Reason,
		Checkpoints: make([]Checkpoint, 0, len(cfg.CheckpointPositionsPct)),
		OffsetsMS:   make([]float64, 0, len(cfg.CheckpointPositionsPct)),
		Qualities:   make([]float64, 0, len(cfg.CheckpointPositionsPct)),
	}

	for _, pct := range cfg.CheckpointPositionsPct {
		start, length := sliceWindow(duration, pct, window)
		search := length
		job := sync.Job{
			Target:        sync.PathSource(req.TargetPath),
			Donor:         sync.PathSource(req.DonorPath),
			SliceStartSec: start,
			SliceLenSec:   length,
			WindowSec:     &search,
		}
		jobCtx, cancel := context.WithTimeout(ctx, JobTimeout(cfg))
		result, err := engine.Run(jobCtx, job)
		cancel()
		if err != nil {
			return Report{}, fmt.Errorf("analyze: checkpoint %d%%: %w", pct, err)
		}
		report.Checkpoints = append(report.Checkpoints, Checkpoint{
			PositionPct:   pct,
			SliceStartSec: start,
			SliceLenSec:   length,
			OffsetMS:      result.OffsetMS,
			Quality:       result.CorrelationQuality,
		})
		report.OffsetsMS = append(report.OffsetsMS, result.OffsetMS)
		report.Qualities = append(report.Qualities, result.CorrelationQuality)
	}

	verdict, classification, drift := classify(report.OffsetsMS, report.Qualities, float64(cfg.MaxOffsetSec)*1000)
	report.Verdict = verdict
	report.Classification = classification
	report.MaxDriftMS = drift
	return report, nil
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
