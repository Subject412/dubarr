package correlate

import (
	"context"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-fft/fft"
)

// This file is U-DSP.5: a numeric guard on what a correlation costs, expressed
// as a ratio against a measurement this host makes for itself. An absolute
// millisecond budget cannot work here — it fails on a busy CI box, gets marked
// flaky and then gets deleted — so the gate compares the correlation with the
// same two dominant operations it is built from, timed on the same machine, in
// the same process, in the same run.
//
// What this guards: the correlation's cost relative to its own reference
// checkpoint — three unfused transforms of nfft points plus one median
// extraction over a lag-score-sized slice. Those are the irreducible parts;
// everything above 1.0x is prepare, the X*conj(Y) product, the prefix sums and
// the lag scan. Those are the parts a coder can accidentally make quadratic or
// allocate per element; the ratio moves the moment one of them doubles.
//
// Why the reference carries the median and is not just "three transforms": the
// transforms are vectorised and scale with CPU width (289 ms on the Ryzen dev
// laptop, 83 ms on the GitHub runner, ~3.5x), while the median is a
// memory-bound sort over ~2.9 M scores that barely scales (the whole
// correlation only went from 981 ms to 401 ms, ~2.4x, and the sort is more than
// half of it). A transform-only reference therefore shrinks faster than the
// numerator on a better host, and the ratio *rose* for identical code: 3.40
// here against 4.84 on CI. With both dominant terms on both sides the ratio is
// a property of the code, not of the machine. ../PERF.md records that evidence
// and the current calibration.
//
// The transforms are not the whole story: `peakProminence` clones and sorts the
// ~2.9 M lag scores (one `slices.Clone`, 23 MB, then pdqsort), which is what
// correlation.rs:192-194 does too — parity, not a regression, and the single
// largest term of a checkpoint, so it is also the term a regression will come
// from.
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

	// perfScores is how many lag scores one production checkpoint's median
	// sorts. The scan offers 2*maxLag+1 = 3 039 999 candidates, and drops the
	// lags whose overlap is below minOverlap = max(16, shorter/20) = 76 000,
	// i.e. |lag| > shorter-minOverlap = 1 444 000, leaving
	// 2*1 444 000+1 = 2 888 001 scores. The reference sorts a slice of exactly
	// that length, so its median is the same piece of work, not a stand-in.
	perfScores = 2*1_444_000 + 1

	// perfRuns is how many timed runs each side gets. Best-of-N, never the
	// mean: everything a shared machine adds — another tenant's container, a
	// scheduling hiccup, a GC cycle — makes a run slower, so the fastest run
	// is the closest estimate of this host's cost and the number a busy runner
	// does not fail on.
	perfRuns = 5

	// costRatioK is the gate: one correlation of a production checkpoint may
	// cost at most costRatioK times the reference checkpoint of the same nfft
	// and the same score count.
	//
	// Recalibrated after #95 replaced the median's clone-and-sort with an exact
	// selection. Measured three times on the host recorded in ../PERF.md
	// (2026-11-27): correlation 425/424/421 ms against a reference checkpoint of
	// 910/897/894 ms, ratio 0.47 every time. The reference still contains the
	// sort, which is the point: it is a fixed baseline, so the ratio falling
	// from ~1.0 to 0.47 is the saving, not a moved goalpost.
	//
	// K = 0.75 rather than 0.47 * 1.25 = 0.59, because this gate's own history
	// records a 42 % spread between this host and CI (3.40 against 4.84) for the
	// same code; 60 % headroom is the honest response to that, and it still
	// leaves the gate meaningful — putting the sort back lands production at
	// ~1.0 of the reference, so K = 0.75 fails a regression long before the
	// absolute time does.
	//
	// What that buys and what it does not: a correlation that got ~60 % more
	// expensive fails, and so does any return to sorting the ~2.9 M scores. A
	// regression confined to the lag scan alone would have to be large to trip
	// it, because the scan is a small part of the total. Raising K is a decision
	// that needs a fresh measurement in PERF.md, not a tweak to silence a red
	// run.
	//
	// The number is per checkpoint, not per job. U-DSP.7 runs several
	// checkpoints at once and they share the memory bus, so the ratio at N > 1
	// will be a little worse; that unit makes this gate N-aware.
	costRatioK = 0.75
)

var (
	perfSlicesOnce sync.Once
	perfTarget     []float32
	perfDonor      []float32

	perfScoresOnce sync.Once
	perfScoreSlice []float32

	// Sinks: the values the timed runs produce, kept reachable so the compiler
	// cannot delete the work.
	perfCorrelationSink Analysis
	perfTransformSink   []complex64
	perfMedianSink      float32
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

// runThreeTransforms is the first of the reference checkpoint's two dominant
// terms, and the work a correlation cannot avoid: two forward transforms of
// nfft points and one inverse, all unfused, as the shipping configuration runs
// them (fft.Options{NoFMA: true}, §3.2 of the port plan). The X*conj(Y) product
// is not in the reference; it is part of what the ratio measures.
func runThreeTransforms(bufs *transformBuffers) []complex64 {
	opts := fft.Options{NoFMA: true}
	x := fft.FFT32With(bufs.target, opts)
	y := fft.FFT32With(bufs.donor, opts)
	// Keep both spectra alive to the end, as a correlation does until the
	// product replaces one of them.
	perfTransformSink = y
	return fft.IFFT32With(x, opts)
}

// lagScores returns the reference median's input, built once per test binary:
// exactly as many values as a checkpoint's lag-score slice, all unsorted, all
// finite, spread over the `-1..1` band the normalised correlations live in.
//
// The values must not be zero and must not be sorted. Sorting already-sorted
// input is a different workload — pdqsort detects the run and does far less
// branchy swapping — and a zeroed slice is a different one again (every
// comparison is a tie in a run of identical values). This has to cost what
// sorting ~2.9 M arbitrary scores costs, or the reference stops being a floor.
func lagScores() []float32 {
	perfScoresOnce.Do(func() {
		perfScoreSlice = make([]float32, perfScores)
		seed := uint64(0x5C0BE5)
		for i := range perfScoreSlice {
			seed = seed*6364136223846793005 + 1442695040888963407
			perfScoreSlice[i] = float32(2*float64(int64(seed>>11))/float64(uint64(1)<<52) - 1)
		}
	})
	return perfScoreSlice
}

// runMedian is the second dominant term: one median extraction over the
// lag-score slice, in the same shape peakProminence gives the production scores
// — clone, sort, pick the middle. The clone is deliberate: peakProminence
// clones too, so a reference that sorted one buffer in place would price less
// than the work it stands for.
func runMedian(scores []float32) float32 {
	sorted := slices.Clone(scores)
	slices.Sort(sorted)
	perfMedianSink = sorted[len(sorted)/2]
	return perfMedianSink
}

// referenceCheckpoint is the reference workload: one checkpoint's irreducible
// work in a single call, so the two terms are timed together exactly as the
// correlation runs them.
type referenceCheckpoint struct {
	bufs   *transformBuffers
	scores []float32
}

func newReferenceCheckpoint() *referenceCheckpoint {
	return &referenceCheckpoint{bufs: newTransformBuffers(), scores: lagScores()}
}

// run is this host's floor for a checkpoint: the three transforms, then the
// median over a lag-score slice of the checkpoint's size.
func (r *referenceCheckpoint) run() []complex64 {
	out := runThreeTransforms(r.bufs)
	runMedian(r.scores)
	return out
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
// not cost more than costRatioK times this host's reference checkpoint — the
// same three unfused transforms plus one median over a lag-score slice of the
// same length — measured best-of-perfRuns.
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
	if scores := lagScores(); len(scores) != perfScores {
		t.Fatalf("reference scores are %d values, want %d", len(scores), perfScores)
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

	reference := newReferenceCheckpoint()
	// Warm-up for the reference side too: go-fft caches its plan32 twiddle
	// table per (n, fused) pair on first use, and the median's clone walks a
	// fresh 11.5 MB allocation. A job pays both once per worker.
	perfTransformSink = reference.run()

	referenceCost := bestOf(t, perfRuns, func() {
		perfTransformSink = reference.run()
	})

	if referenceCost <= 0 {
		t.Fatalf("reference measurement is %v; nothing was timed", referenceCost)
	}
	ratio := float64(correlation) / float64(referenceCost)

	t.Logf("190 s @ %d Hz, nfft = 2^%d, NoFMA: correlation %v (%.0f ms), "+
		"reference checkpoint %v (%.0f ms), ratio %.2f, gate K = %.1f",
		perfRate, int(math.Log2(perfNFFT)), correlation, millis(correlation),
		referenceCost, millis(referenceCost), ratio, costRatioK)

	if ratio > costRatioK {
		t.Fatalf("correlation costs %.2fx the host's reference checkpoint, gate is %.1fx "+
			"(correlation %v, reference %v): the per-checkpoint work grew, not the machine. "+
			"Check prepare, the X*conj(Y) product, the prefix sums, the lag scan and "+
			"peakProminence's clone-and-sort for a reintroduced per-lag allocation or a lost "+
			"early exit. Re-measure and record the number in internal/dsp/PERF.md before "+
			"changing costRatioK.",
			ratio, costRatioK, correlation, referenceCost)
	}
}

// millis renders a duration as milliseconds for the log line.
func millis(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
