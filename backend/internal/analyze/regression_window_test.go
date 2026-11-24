package analyze

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/sync"
)

// Issue #22 regression: the engine now seeks to the analysed window inside the
// WAV files instead of decoding them whole. Checkpoint offsets must not move:
// the ranged path has to reproduce the former "slice a fully decoded array"
// result within ±1 ms, on a long synthetic input, at the production checkpoint
// positions.
func TestRangedReadsMatchWholeFileSlices(t *testing.T) {
	eng := requireEngine(t)
	const (
		rate   = 8000
		dur    = 600
		leadMS = 1500
	)
	target := noiseSignal(dur*rate, 0x9BAD_C0DE)
	lead := leadMS * rate / 1000
	donor := shiftedDonor(target, lead)

	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target.wav")
	donorPath := filepath.Join(dir, "donor.wav")
	writeWAVRate(t, targetPath, target, rate)
	writeWAVRate(t, donorPath, donor, rate)

	req := request(dir)
	req.TargetPath = targetPath
	req.DonorPath = donorPath
	req.DurationSec = dur

	report, err := Analyze(context.Background(), eng, DefaultConfig(), req)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(report.Checkpoints) != len(DefaultConfig().CheckpointPositionsPct) {
		t.Fatalf("checkpoints = %d, want %d", len(report.Checkpoints), len(DefaultConfig().CheckpointPositionsPct))
	}

	for _, cp := range report.Checkpoints {
		// Pinned numbers from the former implementation: an integer-sample
		// lead must be recovered exactly on every window.
		if math.Abs(cp.OffsetMS-float64(leadMS)) > 1 {
			t.Fatalf("checkpoint %d%%: offset = %v ms, pinned %d ms ±1", cp.PositionPct, cp.OffsetMS, leadMS)
		}

		// Former behaviour, expressed at the API level: analyse the same
		// window sliced out of the fully decoded signals, fed as inline PCM
		// starting at 0 s. The correlation inputs are therefore identical.
		start := int(math.Round(cp.SliceStartSec * float64(rate)))
		length := int(math.Round(cp.SliceLenSec * float64(rate)))
		window := cp.SliceLenSec
		res, err := eng.Run(context.Background(), sync.Job{
			Target:        sync.PCMSource(rate, target[start:start+length]),
			Donor:         sync.PCMSource(rate, donor[start:start+length]),
			SliceStartSec: 0,
			SliceLenSec:   cp.SliceLenSec,
			WindowSec:     &window,
		})
		if err != nil {
			t.Fatalf("checkpoint %d%%: whole-file reference: %v", cp.PositionPct, err)
		}
		if math.Abs(res.OffsetMS-cp.OffsetMS) > 1 {
			t.Fatalf("checkpoint %d%%: ranged offset %v ms vs whole-file %v ms (>1 ms)",
				cp.PositionPct, cp.OffsetMS, res.OffsetMS)
		}
		if math.Abs(res.CorrelationQuality-cp.Quality) > 1e-3 {
			t.Fatalf("checkpoint %d%%: quality %v vs whole-file %v",
				cp.PositionPct, cp.Quality, res.CorrelationQuality)
		}
	}
}
