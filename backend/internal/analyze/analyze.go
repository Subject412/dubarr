// Package analyze runs multi-point checkpoint drift analysis for a staged
// dubbing-sync job (IDEA.md §5.2, PLAN.md M4-5).
//
// It slices both staged WAV sources at the configured checkpoint positions
// (sync.defaults.checkpoint_positions_pct), correlates each 150 s window
// through the M4-2 sync bridge, and aggregates a [Report]. The report answers
// three questions the mux stage (M5) needs:
//
//   - What is the offset at each point, and how consistent is it? A constant
//     shift keeps the spread across checkpoints below 40 ms (IDEA.md:385);
//     a larger spread means speed drift or a mid-movie edit.
//   - Does any point exceed sync.defaults.max_offset_sec? Then the verdict is
//     [VerdictOutOfRange] and M5 must fail loudly instead of muxing.
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

	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/reflang"
	"github.com/Subject412/dubarr/backend/internal/sync"
)

// Defaults from IDEA.md §5.2 and the settled sync.defaults values.
const (
	// DefaultCheckpointWindowSec is the settled 150 s analysis window.
	DefaultCheckpointWindowSec = 150
	// DefaultMaxOffsetSec is the settled ±180 s search/acceptance bound.
	DefaultMaxOffsetSec = 180
)

// DefaultCheckpointPositionsPct is the settled 6-point sampling of the
// timeline (IDEA.md:373).
var DefaultCheckpointPositionsPct = []int{10, 25, 40, 55, 70, 85}

// StableSpreadThresholdMS is the maximum checkpoint spread that still counts
// as a constant offset. IDEA.md:385: "Spread across valid checkpoints < 40 ms
// -> final offset is median, speed_mode = 'none'". No other threshold is
// specified, so the spec's 40 ms is used verbatim.
const StableSpreadThresholdMS = 40.0

// Verdicts reported to the sync job status and consumed by M5.
const (
	VerdictOK         = "ok"
	VerdictDrifting   = "drifting"
	VerdictOutOfRange = "out_of_range"
)

// Classifications explain the verdict. They mirror the verdict names so a
// consumer can render either field without a lookup table.
const (
	ClassificationStable     = "stable"
	ClassificationDrifting   = "drifting"
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
// The engine searches the full slice (WindowSec = slice length), which for the
// settled 150 s window is tighter than the ±180 s acceptance bound; MaxOffsetSec
// is therefore the acceptance threshold, not an engine tuning knob. Any offset
// beyond it produces [VerdictOutOfRange].
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

	window := float64(cfg.CheckpointWindowSec)
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
		result, err := engine.Run(ctx, job)
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

	verdict, classification, drift := classify(report.OffsetsMS, float64(cfg.MaxOffsetSec)*1000)
	report.Verdict = verdict
	report.Classification = classification
	report.MaxDriftMS = drift
	return report, nil
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
// acceptance bound fails loudly.
func classify(offsets []float64, maxOffsetMS float64) (verdict, classification string, maxDrift float64) {
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
	if maxDrift >= StableSpreadThresholdMS {
		return VerdictDrifting, ClassificationDrifting, maxDrift
	}
	return VerdictOK, ClassificationStable, maxDrift
}
