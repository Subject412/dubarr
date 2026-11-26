package correlate

import (
	"context"
	"math"
	"testing"
)

// The synthetic self-check, ported from sync-engine/src/selftest.rs so that
// `dubarr-sync --selftest` — a flag no Go caller would ever pass — becomes a
// test that runs with the suite.
const (
	selftestSec         = 1.5
	selftestToleranceMS = 10.0
	selftestMinQuality  = float32(0.3)
	selftestSampleRate  = 8000
	selftestSeed        = 0xD0BA5EED
)

// lcg is the reference generator's 64-bit linear congruential generator.
type lcg struct {
	state uint64
}

// newLCG seeds the generator; the reference engine ORs in a one.
func newLCG(seed uint64) *lcg { return &lcg{state: seed | 1} }

// next returns the next sample in -1..1: the top 24 bits of the state, divided
// by 2^23 (selftest.rs:22-28).
func (g *lcg) next() float32 {
	g.state = g.state*6364136223846793005 + 1442695040888963407
	return float32(g.state>>40)/8388608.0 - 1.0
}

// syntheticProgramme builds the deterministic programme of selftest.rs:31-52:
// a heavily low-passed noise floor plus a 120 ms burst every 370 ms whose
// frequency steps per burst. It is the same generator and the same synthesis,
// in the same width (a float32 filter state, float64 phase arithmetic).
func syntheticProgramme(sampleRate uint32, seconds float64) []float32 {
	sr := float64(sampleRate)
	n := int(math.Round(seconds * sr))
	rng := newLCG(selftestSeed)

	lp := float32(0)
	out := make([]float32, n)
	for i := range out {
		white := rng.next()
		lp = 0.9*lp + 0.1*white

		t := float64(i) / sr
		phase := math.Mod(t, 0.37)
		value := lp * 1.8
		if phase < 0.12 {
			burst := uint64(math.Floor(t / 0.37))
			freq := 300.0 + float64(burst*137%1000)
			env := math.Sin(math.Pi * phase / 0.12)
			value += float32(0.7 * env * math.Sin(2*math.Pi*freq*t))
		}
		out[i] = clamp32(value, -0.95, 0.95)
	}
	return out
}

// clamp32 is f32::clamp for a well-ordered pair.
func clamp32(v, lo, hi float32) float32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// TestSelftestSynthetic is the reference engine's `--selftest`: a programme
// delayed by exactly 1500 ms must be recovered within 10 ms with a quality of
// at least 0.3.
func TestSelftestSynthetic(t *testing.T) {
	const rate = uint32(selftestSampleRate)
	base := syntheticProgramme(rate, 10.0)

	delay := int(math.Round(selftestSec * float64(rate)))
	target := make([]float32, delay+len(base))
	copy(target[delay:], base)

	// The same 4 s windows selftest.rs analyses, two seconds in on both sides.
	start := 2 * int(rate)
	length := 4 * int(rate)
	got, err := Analyze(context.Background(), target[start:start+length], base[start:start+length], rate, 2.5)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	offsetMS := float64(got.OffsetSamples) * 1000 / float64(rate)
	want := selftestSec * 1000
	if err := math.Abs(offsetMS - want); err > selftestToleranceMS {
		t.Fatalf("recovered %.3f ms, expected %.1f ms (error %.3f ms)", offsetMS, want, err)
	}
	if got.Quality < selftestMinQuality {
		t.Fatalf("correlation quality %.3f below %v (offset %.3f ms)", got.Quality, selftestMinQuality, offsetMS)
	}
	t.Logf("selftest: offset %.3f ms, quality %.3f", offsetMS, got.Quality)
}
