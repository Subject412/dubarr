package correlate

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/go-fft/fft"
)

// This file is U-DSP.5: a numeric guard on what a correlation costs, expressed
// as a ratio against a measurement this host makes for itself. An absolute
// millisecond budget cannot work here — it fails on a busy CI box, gets marked
// flaky and then gets deleted — so the gate compares the correlation with the
// three transforms it is built from, timed on the same machine, in the same
// process, in the same run.
//
// What this guards: the correlation's cost relative to its own transform floor.
// The three transforms are the irreducible part; everything above 1.0x is
// prepare, the X*conj(Y) product, the prefix sums, the lag scan and the median
// in peakProminence. Those are the parts a coder can accidentally make
// quadratic, allocate per element, or sort twice; the ratio moves the moment one
// of them doubles.
//
// The ratio is not the 1.5-2.0 the port plan guessed. Measured here it is
// 3.3-3.4, and a CPU profile says why: the three transforms cost ~295 ms per
// checkpoint while `peakProminence` spends ~800 ms sorting the ~2.9 M lag
// scores to take their median (one `slices.Clone`, 23 MB, then pdqsort). That
// sort is what correlation.rs:192-194 does too, so it is parity, not a
// regression — the plan's estimate simply did not price an O(n log n) median
// over three million lags. ../PERF.md records the breakdown; the number to
// watch is that the sort is the single largest term, so it is also the term a
// regression will come from.
//
// What this does NOT guard is correctness, and it cannot: removing the overlap
// guard (`count < minOverlap`) makes the scan *cheaper* and still passes here;
// adding a `1/nfft` multiply costs one extra pass and still passes here. Cost is
// cost whatever the answer is. TestAutocorrelationPeakIsOne guards the
// normalisation and TestGoldenExcerpts guards the numbers; this file only says
// the work did not get bigger.

const (
	// perfRate and perfWindowSec are one production checkpoint: analyze's
	// searchWindowSec is max(150, 180+10) = 190 s at 8 kHz, so 1 520 000
	// samples a side, nx+ny-1 = 3 039 999, and nfft = 2^22.
	perfRate      = 8000
	perfWindowSec = 190
	perfSamples   = perfRate * perfWindowSec
	perfNFFT      = 1 << 22

	// perfRuns is how many timed runs each side gets. Best-of-N, never the
	// mean: everything a shared machine adds — another tenant's container, a
	// scheduling hiccup, a GC cycle — makes a run slower, so the fastest run
	// is the closest estimate of this host's cost and the number a busy runner
	// does not fail on.
	perfRuns = 5

	// costRatioK is the gate: one correlation of a production checkpoint may
	// cost at most costRatioK times three transforms of the same nfft.
	//
	// Calibrated once on the host recorded in ../PERF.md (AMD Ryzen 5 2500U,
	// 2026-11-27): three runs of this test measured 3.33, 3.37 and 3.42, so the
	// observation taken is the worst of them, 3.42, and
	// K = ceil(3.42 * 1.25 * 10) / 10 = 4.3. The 25 % headroom is for the
	// difference a runner can show that is *not* a regression: the median sort
	// and the transform kernel respond differently to a smaller L3 and to a
	// shared memory bus, and the ratio is only as portable as that difference is
	// small.
	//
	// What that headroom buys and what it does not: at 4.3 a correlation that
	// got ~25 % more expensive fails, and so does a second clone-and-sort of the
	// scores (that alone is ~1.7x here). A regression confined to the lag scan
	// alone would have to be ~3x to trip it, because the scan is a small part of
	// the total — the sort is the largest term. Raising K is a decision that
	// needs a fresh measurement in PERF.md, not a tweak to silence a red run.
	//
	// The number is per checkpoint, not per job. U-DSP.7 runs several
	// checkpoints at once and they share the memory bus, so the ratio at N > 1
	// will be a little worse; that unit makes this gate N-aware.
	costRatioK = 4.3
)

var (
	perfSlicesOnce sync.Once
	perfTarget     []float32
	perfDonor      []float32

	// Sinks: the values the timed runs produce, kept reachable so the compiler
	// cannot delete the work.
	perfCorrelationSink Analysis
	perfTransformSink   []complex64
)

// productionSlices returns one checkpoint's pair, built once per test binary.
// The programme is deterministic, so every run measures the same arithmetic:
// the same samples, the same peak at the same lag, the same scan.
func productionSlices() (target, donor []float32) {
	perfSlicesOnce.Do(func() {
		perfTarget, perfDonor = makeProductionSlices()
	})
	return perfTarget, perfDonor
}

// makeProductionSlices synthesises 190 s of programme material at 8 kHz: a few
// partials plus a little noise, which is broadband enough that the correlation
// has one clean peak and the transform has realistic non-zero input. The donor
// is the same programme 250 ms late, so the answer is offset +2000 samples.
//
// The content only has to be non-silent and non-degenerate; what this file
// measures is work, not accuracy.
func makeProductionSlices() (target, donor []float32) {
	const delay = 2000 // samples, 250 ms

	programme := make([]float32, perfSamples+delay)
	seed := uint64(0xD0BA5EED)
	for i := range programme {
		seed = seed*6364136223846793005 + 1442695040888963407
		noise := float64(int64(seed>>11))/float64(uint64(1)<<52) - 0.5

		t := float64(i) / perfRate
		v := 0.45*math.Sin(2*math.Pi*220*t) +
			0.25*math.Sin(2*math.Pi*660*t) +
			0.15*math.Sin(2*math.Pi*1760*t) +
			0.05*noise
		programme[i] = float32(v)
	}

	target = make([]float32, perfSamples)
	donor = make([]float32, perfSamples)
	copy(target, programme[:perfSamples])
	copy(donor, programme[delay:delay+perfSamples])
	return target, donor
}

// transformBuffers is the reference benchmark's input: two nfft-sized spectra
// buffers holding exactly what a correlation would hand the transform, allocated
// once so the reference times transforms and not allocation.
type transformBuffers struct {
	target, donor []complex64
}

func newTransformBuffers() *transformBuffers {
	target, donor := productionSlices()
	bufs := &transformBuffers{
		target: make([]complex64, perfNFFT),
		donor:  make([]complex64, perfNFFT),
	}
	fillPadded(bufs.target, target)
	fillPadded(bufs.donor, donor)
	return bufs
}

// runThreeTransforms is this host's floor for the work a correlation cannot
// avoid: two forward transforms of nfft points and one inverse, all unfused, as
// the shipping configuration runs them (fft.Options{NoFMA: true}, §3.2 of the
// port plan). The X*conj(Y) product is not in the reference; it is part of what
// the ratio measures.
func runThreeTransforms(bufs *transformBuffers) []complex64 {
	opts := fft.Options{NoFMA: true}
	x := fft.FFT32With(bufs.target, opts)
	y := fft.FFT32With(bufs.donor, opts)
	// Keep both spectra alive to the end, as a correlation does until the
	// product replaces one of them.
	perfTransformSink = y
	return fft.IFFT32With(x, opts)
}

// runCorrelation is one production checkpoint through the shipping path: a
// [Scratch] shared across checkpoints of the same nfft, as a job's worker uses
// it, so the padded inputs are recycled rather than allocated per run.
func runCorrelation(scratch *Scratch) (Analysis, error) {
	target, donor := productionSlices()
	return scratch.Analyze(context.Background(), target, donor, perfRate, perfWindowSec)
}

// bestOf returns the fastest of n timed runs of f.
func bestOf(t *testing.T, n int, f func()) time.Duration {
	t.Helper()
	best := time.Duration(math.MaxInt64)
	for i := 0; i < n; i++ {
		start := time.Now()
		f()
		if took := time.Since(start); took < best {
			best = took
		}
	}
	return best
}

// TestCorrelationCostRatio is the numeric gate: one production checkpoint may
// not cost more than costRatioK times three unfused transforms of the same
// nfft, measured best-of-perfRuns on this host.
//
// It never skips. It is a few seconds of work, it needs no media and no
// toolchain, and it is the only performance number in the normal `go test` run
// — a gate that can skip is a gate that silently stops existing.
func TestCorrelationCostRatio(t *testing.T) {
	target, donor := productionSlices()
	if len(target) != perfSamples || len(donor) != perfSamples {
		t.Fatalf("production slices are %d and %d samples, want %d each",
			len(target), len(donor), perfSamples)
	}

	// One pool for both the warm-up and the timed runs, as a job reuses it.
	scratch := NewScratch()
	ctx := context.Background()

	// Warm-up: the pool is empty on the first correlation, so the first run
	// allocates the padded inputs a job allocates once per worker. Timing that
	// as the checkpoint's cost would charge the gate for start-up, and the
	// reference has no equivalent.
	if _, err := scratch.Analyze(ctx, target, donor, perfRate, perfWindowSec); err != nil {
		t.Fatalf("warm-up correlation: %v", err)
	}

	correlation := bestOf(t, perfRuns, func() {
		got, err := scratch.Analyze(ctx, target, donor, perfRate, perfWindowSec)
		if err != nil {
			t.Fatalf("correlation: %v", err)
		}
		perfCorrelationSink = got
	})

	bufs := newTransformBuffers()
	// Warm-up for the transform side too: go-fft caches its plan32 twiddle
	// table per (n, fused) pair on first use, and a job pays that once.
	perfTransformSink = runThreeTransforms(bufs)

	reference := bestOf(t, perfRuns, func() {
		perfTransformSink = runThreeTransforms(bufs)
	})

	if reference <= 0 {
		t.Fatalf("reference measurement is %v; nothing was timed", reference)
	}
	ratio := float64(correlation) / float64(reference)

	t.Logf("190 s @ %d Hz, nfft = 2^%d, NoFMA: correlation %v (%.0f ms), "+
		"three transforms %v (%.0f ms), ratio %.2f, gate K = %.1f",
		perfRate, int(math.Log2(perfNFFT)), correlation, millis(correlation),
		reference, millis(reference), ratio, costRatioK)

	if ratio > costRatioK {
		t.Fatalf("correlation costs %.2fx the host's three-transform floor, gate is %.1fx "+
			"(correlation %v, transforms %v): the per-checkpoint work grew, not the machine. "+
			"Check prepare, the X*conj(Y) product, the prefix sums and the lag scan for a "+
			"reintroduced per-lag allocation or a lost early exit. Re-measure and record the "+
			"number in internal/dsp/PERF.md before changing costRatioK.",
			ratio, costRatioK, correlation, reference)
	}
}

// millis renders a duration as milliseconds for the log line.
func millis(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
