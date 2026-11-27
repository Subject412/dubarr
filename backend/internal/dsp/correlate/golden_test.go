package correlate

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/dsp/wav"
)

// The committed goldens of U-DSP.1, replayed through this package. This is the
// numeric gate that cannot skip: it needs no media, no toolchain and no
// network, only the excerpts under internal/dsp/testdata.
const (
	testdataDir = "../testdata"

	// goldenQualityTolerance is the parity budget for the prominence score.
	// Offsets are asserted exactly — the peak either moves or it does not —
	// but quality is a ratio of float32 sums and is allowed to drift.
	goldenQualityTolerance = 0.02

	// goldenCleanQuality is the recorded prominence at or above which an entry
	// belongs to the clean excerpt set: the goldens whose content really
	// matches, as opposed to the Minions checkpoints that sit on spurious
	// peaks. It is a property of the committed capture, not a verdict limit.
	goldenCleanQuality = 0.5

	// goldenMatchZGood mirrors analyze.DefaultThresholds.MatchZGood. It is a
	// local constant because analyze imports sync, which imports this package:
	// a test here cannot import analyze without an import cycle.
	goldenMatchZGood = 10.0
)

// goldenFile is one committed capture: the reference engine's own output for a
// set of WAV pairs.
type goldenFile struct {
	Case    string        `json:"case"`
	Tier    string        `json:"tier"`
	Entries []goldenEntry `json:"entries"`
}

// goldenEntry is one recorded correlation.
type goldenEntry struct {
	ID        string `json:"id"`
	Tier      string `json:"tier"`
	TargetWav string `json:"target_wav"`
	DonorWav  string `json:"donor_wav"`
	Payload   string `json:"payload"`
	Result    struct {
		OffsetMS           float64 `json:"offset_ms"`
		CorrelationQuality float64 `json:"correlation_quality"`
		SampleRate         uint32  `json:"sample_rate"`
	} `json:"result"`
}

// goldenPayload is the job the capture tool sent the reference engine.
type goldenPayload struct {
	SliceStartSec float64 `json:"slice_start_sec"`
	SliceLenSec   float64 `json:"slice_len_sec"`
	WindowSec     float64 `json:"window_sec"`
}

// TestGoldenExcerpts replays every committed golden entry: the same frames the
// reference engine analysed, through this package.
//
// The rules, from the unit contract: the offset must be exactly the recorded
// one (converted back to samples, losslessly), the quality must be within
// goldenQualityTolerance of the recorded one, and quality must lie in 0..1. No
// absolute quality floor is asserted — the Minions 40 % checkpoint is 0.113 and
// its 10 %/55 % neighbours sit on spurious peaks, which is the reference
// engine's real behaviour. Parity means reproducing it, not correcting it.
func TestGoldenExcerpts(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(testdataDir, "golden", "*.json"))
	if err != nil {
		t.Fatalf("globbing the goldens: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("no goldens under %s; the numeric gate must not pass by finding nothing",
			filepath.Join(testdataDir, "golden"))
	}

	// One pool for the whole run, as a job shares one across checkpoints.
	scratch := NewScratch()
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		var golden goldenFile
		if err := json.Unmarshal(data, &golden); err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		if len(golden.Entries) == 0 {
			t.Fatalf("%s records no entries", path)
		}
		for _, entry := range golden.Entries {
			t.Run(golden.Case+"/"+entry.ID, func(t *testing.T) {
				checkGolden(t, scratch, entry)
			})
		}
	}
}

// checkGolden correlates one entry's excerpts and compares the answer with the
// recorded one.
func checkGolden(t *testing.T, scratch *Scratch, entry goldenEntry) {
	t.Helper()

	got := replayGolden(t, scratch, entry)
	rate := entry.Result.SampleRate

	wantSamples, ok := offsetSamples(entry.Result.OffsetMS, rate)
	if !ok {
		t.Fatalf("%s: golden offset_ms %v is not a whole number of samples at %d Hz; "+
			"the capture records multiples of 1/8 ms, so this golden cannot be replayed",
			entry.ID, entry.Result.OffsetMS, rate)
	}
	gotMS := samplesToMillis(got.OffsetSamples, rate)
	if got.OffsetSamples != wantSamples {
		t.Errorf("%s: offset = %d samples (%v ms), golden = %d samples (%v ms); quality %v, golden %v",
			entry.ID, got.OffsetSamples, gotMS, wantSamples, entry.Result.OffsetMS,
			got.Quality, entry.Result.CorrelationQuality)
	}

	delta := math.Abs(float64(got.Quality) - entry.Result.CorrelationQuality)
	if delta > goldenQualityTolerance {
		t.Errorf("%s: quality = %v, golden = %v (|delta| = %v > %v); offset %d samples (%v ms), golden offset %v ms",
			entry.ID, got.Quality, entry.Result.CorrelationQuality, delta, goldenQualityTolerance,
			got.OffsetSamples, gotMS, entry.Result.OffsetMS)
	}
	if got.Quality < 0 || got.Quality > 1 {
		t.Errorf("%s: quality = %v is outside 0..1", entry.ID, got.Quality)
	}

	// `go test -run TestGoldenExcerpts -v` prints the whole parity table.
	t.Logf("%s: offset %d samples (%v ms, golden %v ms), quality %v (golden %v)",
		entry.ID, got.OffsetSamples, gotMS, entry.Result.OffsetMS,
		got.Quality, entry.Result.CorrelationQuality)
}

// replayGolden reads one entry's excerpts exactly as the capture asked for them
// and runs them through the correlator, returning the whole [Analysis].
func replayGolden(t *testing.T, scratch *Scratch, entry goldenEntry) Analysis {
	t.Helper()

	var job goldenPayload
	if err := json.Unmarshal([]byte(entry.Payload), &job); err != nil {
		t.Fatalf("%s: parsing the recorded payload: %v", entry.ID, err)
	}
	rate := entry.Result.SampleRate
	if rate == 0 {
		t.Fatalf("%s: the golden records no sample rate", entry.ID)
	}

	target, err := readExcerpt(entry.TargetWav, job.SliceStartSec, job.SliceLenSec, rate)
	if err != nil {
		t.Fatalf("%s: target: %v", entry.ID, err)
	}
	donor, err := readExcerpt(entry.DonorWav, job.SliceStartSec, job.SliceLenSec, rate)
	if err != nil {
		t.Fatalf("%s: donor: %v", entry.ID, err)
	}

	got, err := scratch.Analyze(context.Background(), target, donor, rate, job.WindowSec)
	if err != nil {
		t.Fatalf("%s: analyze: %v", entry.ID, err)
	}
	return got
}

// TestGoldenExcerptsCarryAPlausibleMatchZ guards the lag-curve accumulator
// against a sign or overflow error without pinning numbers the float32 sums may
// move: every committed entry reports a finite, positive z, and the clean
// excerpt set (recorded prominence >= goldenCleanQuality) clears the "likely
// correct" band. The goldens themselves are never regenerated — TestGoldenExcerpts
// stays the 0.4.0 parity record.
func TestGoldenExcerptsCarryAPlausibleMatchZ(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(testdataDir, "golden", "*.json"))
	if err != nil {
		t.Fatalf("globbing the goldens: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("no goldens under %s; this guard must not pass by finding nothing",
			filepath.Join(testdataDir, "golden"))
	}

	scratch := NewScratch()
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		var golden goldenFile
		if err := json.Unmarshal(data, &golden); err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		if len(golden.Entries) == 0 {
			t.Fatalf("%s records no entries", path)
		}
		for _, entry := range golden.Entries {
			t.Run(golden.Case+"/"+entry.ID, func(t *testing.T) {
				got := replayGolden(t, scratch, entry)
				z := float64(got.MatchZ)
				if math.IsNaN(z) || math.IsInf(z, 0) {
					t.Fatalf("%s: match_z = %v, want a finite number", entry.ID, z)
				}
				if z <= 0 {
					t.Fatalf("%s: match_z = %v, want > 0 (the accumulator never saw the peak)", entry.ID, z)
				}
				if entry.Result.CorrelationQuality >= goldenCleanQuality && z < goldenMatchZGood {
					t.Fatalf("%s: match_z = %v on a clean excerpt (quality %v), want >= %v",
						entry.ID, z, entry.Result.CorrelationQuality, goldenMatchZGood)
				}
			})
		}
	}
}

// readExcerpt reads the frames the recorded job asked for, out of the committed
// excerpt. The golden stores repository-relative paths; the file itself lives
// next to this package's testdata.
func readExcerpt(recordedPath string, startSec, lenSec float64, rate uint32) ([]float32, error) {
	if recordedPath == "" {
		return nil, fmt.Errorf("the golden records no WAV path")
	}
	path := filepath.Join(testdataDir, "wav", filepath.Base(recordedPath))

	reader, err := wav.Open(path)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	if got := reader.SampleRate(); got != rate {
		return nil, fmt.Errorf("%s: sample rate %d Hz, the golden records %d Hz", path, got, rate)
	}

	start := int64(math.Round(startSec * float64(rate)))
	frames := int64(math.Round(lenSec * float64(rate)))
	return reader.ReadMono(start, frames)
}

// offsetSamples converts a recorded offset in milliseconds back to samples.
//
// It refuses a value that is not a whole number of samples instead of rounding
// silently: the capture writes `round_millis(samples*1000/rate)`, so at 8 kHz
// every correct golden is a multiple of 0.125 ms and the round trip is exact. A
// golden that is not means the capture or the sample rate is wrong, and a test
// that quietly rounded it would hide that.
func offsetSamples(offsetMS float64, rate uint32) (int64, bool) {
	exact := offsetMS * float64(rate) / 1000
	rounded := math.Round(exact)
	if math.Abs(exact-rounded) > 1e-6 {
		return 0, false
	}
	return int64(rounded), true
}

// samplesToMillis is the engine's own conversion, unrounded, for messages.
func samplesToMillis(samples int64, rate uint32) float64 {
	return float64(samples) * 1000 / float64(rate)
}
