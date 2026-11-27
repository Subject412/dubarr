package correlate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
)

// whiteProgramme returns n samples of the reference generator's white noise,
// in -1..1. A broadband signal has a narrow autocorrelation peak, so a lag test
// reads the recovered offset instead of a smear.
func whiteProgramme(n int, seed uint64) []float32 {
	rng := newLCG(seed)
	out := make([]float32, n)
	for i := range out {
		out[i] = rng.next()
	}
	return out
}

// TestAutocorrelationPeakIsOne is the INV-NORM guard (§3.1): a slice
// correlated with itself peaks at lag 0 with an NCC of 1.
//
// The reference engine's inverse transform is unnormalised and it multiplies
// the result by 1/nfft; go-fft's IFFT32 is already normalised by nfft, so the
// port must apply no scale at all. A port that kept the Rust multiply lands at
// 1/nfft — 2.4e-7 at nfft = 2^22 — which is six orders of magnitude below the
// floor here, and drives every quality to ~1e-7 downstream.
func TestAutocorrelationPeakIsOne(t *testing.T) {
	programme := syntheticProgramme(8000, 4.0)

	raw, err := correlate(context.Background(), nil, programme, programme, 8000, 1.0)
	if err != nil {
		t.Fatalf("correlate: %v", err)
	}
	if raw.offset != 0 {
		t.Fatalf("autocorrelation peak at lag %d, want 0", raw.offset)
	}
	if raw.peak < 0.99 {
		t.Fatalf("lag-0 NCC = %.9f, want >= 0.99 (a double-normalised port lands near 1/nfft)", raw.peak)
	}

	got, err := Analyze(context.Background(), programme, programme, 8000, 1.0)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if got.OffsetSamples != 0 || got.Quality < 0.99 {
		t.Fatalf("Analysis = {offset %d, quality %.9f}, want {offset 0, quality >= 0.99}",
			got.OffsetSamples, got.Quality)
	}
}

// TestKnownShift recovers a delay applied to a synthetic programme, through the
// public entry point and with a window wide enough to hold it.
func TestKnownShift(t *testing.T) {
	const (
		rate  = uint32(8000)
		shift = 1200
	)
	base := whiteProgramme(32000, 0xBEEF)
	target := make([]float32, shift+len(base))
	copy(target[shift:], base)

	got, err := Analyze(context.Background(), target[:len(base)], base, rate, 1.0)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if got.OffsetSamples != shift {
		t.Fatalf("offset = %d samples, want %d", got.OffsetSamples, shift)
	}
	if got.Quality < 0.9 {
		t.Fatalf("quality = %.6f on an exact copy, want >= 0.9", got.Quality)
	}
}

// TestPeakProminence pins the formula against hand-computed answers
// (correlation.rs:188-202).
func TestPeakProminence(t *testing.T) {
	cases := []struct {
		name   string
		peak   float64
		scores []float64
		want   float32
	}{
		{
			// median = 0.1, so (0.9 - 0.1) / (1 - 0.1) = 0.888...
			name:   "peak over median",
			peak:   0.9,
			scores: []float64{0.1, 0.1, 0.9},
			want:   0.8888889,
		},
		{
			name:   "peak equals median",
			peak:   0.5,
			scores: []float64{0.5, 0.5, 0.5},
			want:   0,
		},
		{
			// median = -1, so (0 - (-1)) / (1 - (-1)) = 0.5
			name:   "negative median",
			peak:   0,
			scores: []float64{-1, -1, -1, 0},
			want:   0.5,
		},
		{
			// No headroom above the median: the reference engine returns 0
			// rather than dividing by zero.
			name:   "no headroom",
			peak:   1,
			scores: []float64{1, 1, 1},
			want:   0,
		},
		{
			name:   "clamped at one",
			peak:   2,
			scores: []float64{0, 0, 2},
			want:   1,
		},
		{
			name:   "no scores",
			peak:   1,
			scores: nil,
			want:   0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := peakProminence(tc.peak, tc.scores)
			if diff := math.Abs(float64(got) - float64(tc.want)); diff > 1e-6 {
				t.Fatalf("peakProminence(%v, %v) = %v, want %v", tc.peak, tc.scores, got, tc.want)
			}
		})
	}
}

// TestValidation pins the validation order and wording of
// correlation.rs:68-97: a caller with two problems gets the error the reference
// engine reported for the same input.
func TestValidation(t *testing.T) {
	signal := whiteProgramme(128, 7)
	short := signal[:63]

	cases := []struct {
		name   string
		target []float32
		donor  []float32
		rate   uint32
		window float64
		want   string
	}{
		{
			name:   "rate zero wins over the other problems",
			target: short,
			donor:  short,
			rate:   0,
			window: 0,
			want:   "invalid job: sample_rate must be greater than zero",
		},
		{
			name:   "window zero",
			target: signal,
			donor:  signal,
			rate:   8000,
			window: 0,
			want:   "invalid job: window_sec must be a positive number",
		},
		{
			name:   "window NaN",
			target: signal,
			donor:  signal,
			rate:   8000,
			window: math.NaN(),
			want:   "invalid job: window_sec must be a positive number",
		},
		{
			name:   "window negative",
			target: signal,
			donor:  signal,
			rate:   8000,
			window: -5,
			want:   "invalid job: window_sec must be a positive number",
		},
		{
			name:   "target too short",
			target: short,
			donor:  signal,
			rate:   8000,
			window: 1,
			want:   "slice too short: 63 samples (minimum 64)",
		},
		{
			name:   "donor too short",
			target: signal,
			donor:  short,
			rate:   8000,
			window: 1,
			want:   "slice too short: 63 samples (minimum 64)",
		},
		{
			// 1e-9 s * 8000 Hz rounds to no lags at all.
			name:   "window rounds to zero lags",
			target: signal,
			donor:  signal,
			rate:   8000,
			window: 1e-9,
			want:   "no valid correlation lag inside the search window",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Analyze(context.Background(), tc.target, tc.donor, tc.rate, tc.window)
			if err == nil {
				t.Fatalf("Analyze succeeded with %+v, want error %q", got, tc.want)
			}
			if err.Error() != tc.want {
				t.Fatalf("error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

// TestSilentSlice covers the two ways a slice can carry no signal: digital
// silence, and a constant that is removed with its DC.
func TestSilentSlice(t *testing.T) {
	signal := whiteProgramme(4096, 9)
	silence := make([]float32, 4096)
	constant := make([]float32, 4096)
	for i := range constant {
		constant[i] = 0.5
	}

	cases := []struct {
		name          string
		target, donor []float32
		which         string
	}{
		{name: "target silent", target: silence, donor: signal, which: "target"},
		{name: "donor silent", target: signal, donor: silence, which: "donor"},
		{name: "target is pure DC", target: constant, donor: signal, which: "target"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Analyze(context.Background(), tc.target, tc.donor, 8000, 1.0)
			var silent *SilentSlice
			if !errors.As(err, &silent) {
				t.Fatalf("error = %v, want *SilentSlice", err)
			}
			if silent.Which != tc.which {
				t.Fatalf("SilentSlice.Which = %q, want %q", silent.Which, tc.which)
			}
			want := tc.which + " slice is silent (no signal to correlate)"
			if err.Error() != want {
				t.Fatalf("error = %q, want %q", err.Error(), want)
			}
		})
	}
}

// TestCancelledContext asserts a cancelled context stops the correlation and
// reports the context error instead of a bogus offset. The checks sit before
// the first transform, after each transform and every 4096 lags, so a caller
// that has already given up pays nothing: the returned Analysis is the zero
// value either way.
func TestCancelledContext(t *testing.T) {
	target := whiteProgramme(400_000, 11)
	donor := whiteProgramme(400_000, 12)

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		started := time.Now()
		got, err := Analyze(ctx, target, donor, 8000, 1.0)
		elapsed := time.Since(started)

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if got != (Analysis{}) {
			t.Fatalf("Analysis = %+v with a cancelled context, want the zero value", got)
		}
		// A correlation of these slices moves megabytes and takes far longer
		// than this; the bound only catches a hang.
		if elapsed > time.Second {
			t.Fatalf("cancelled Analyze took %v, want well under 1s", elapsed)
		}
	})

	t.Run("deadline exceeded", func(t *testing.T) {
		ctx, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer stop()

		got, err := Analyze(ctx, target, donor, 8000, 1.0)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
		if got != (Analysis{}) {
			t.Fatalf("Analysis = %+v past its deadline, want the zero value", got)
		}
	})
}

// TestInterpolatedOffsetMovesOffTheSampleGrid proves the reported sub-sample
// offset is diagnostics on top of an unchanged integer answer: a pair delayed
// by a whole number of samples *plus a fraction* keeps the 0.4.0 argmax (the
// integer lag of the global maximum) and reports a fractional offset within
// half a sample of it.
//
// The donor is a linear interpolation between two neighbouring target samples,
// so the true delay sits at lead+0.35 samples: the peak of the cross-correlation
// is off the grid, which is exactly what the parabolic fit is for.
func TestInterpolatedOffsetMovesOffTheSampleGrid(t *testing.T) {
	const (
		rate = uint32(8000)
		lead = 1200
		frac = 0.35
		n    = 8000
	)
	target := whiteProgramme(n, 0xD00D)
	donor := make([]float32, n)
	for i := range donor {
		if i+lead+1 >= n {
			continue
		}
		donor[i] = float32((1-frac)*float64(target[i+lead]) + frac*float64(target[i+lead+1]))
	}

	got, err := Analyze(context.Background(), target, donor, rate, 0.75)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	// 0.4.0's answer: the integer lag of the global maximum, never moved.
	if got.OffsetSamples != lead {
		t.Fatalf("offset = %d samples, want the unchanged integer peak at %d", got.OffsetSamples, lead)
	}
	delta := got.OffsetSubSamples - float64(got.OffsetSamples)
	if delta == 0 {
		t.Fatalf("offset_sub_samples = %v equals the integer offset %d: the peak stayed on the grid",
			got.OffsetSubSamples, got.OffsetSamples)
	}
	if math.Abs(delta) > 0.5 {
		t.Fatalf("offset_sub_samples = %v lies %v samples off the peak: the fit cannot pass its neighbour",
			got.OffsetSubSamples, delta)
	}
	if math.Abs(delta) < 0.05 {
		t.Fatalf("offset_sub_samples = %v is %v samples off the grid for a 0.35-sample delay: want a real fraction",
			got.OffsetSubSamples, delta)
	}
}

// TestAnalysisPrecisionIsTheAnalysisSample states the measurement precision
// explicitly: one sample of the rate the correlation ran at.
func TestAnalysisPrecisionIsTheAnalysisSample(t *testing.T) {
	programme := whiteProgramme(4000, 0x7E57)

	cases := []struct {
		rate uint32
		want float64
	}{
		{rate: 8000, want: 0.125},
		{rate: 1000, want: 1.0},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%dHz", tc.rate), func(t *testing.T) {
			got, err := Analyze(context.Background(), programme, programme, tc.rate, 0.1)
			if err != nil {
				t.Fatalf("Analyze: %v", err)
			}
			if got.PrecisionMS != tc.want {
				t.Fatalf("precision_ms = %v at %d Hz, want %v", got.PrecisionMS, tc.rate, tc.want)
			}
		})
	}
}

// TestScratchReuseKeepsNumbersIdentical proves the buffer pool cannot move a
// number: repeated runs on one Scratch, and a run of a different nfft on the
// same pool, agree exactly with a fresh run.
//
// The comparison is over the whole [Analysis], so the sub-sample offset, the
// match z and the precision are covered by the same guard.
func TestScratchReuseKeepsNumbersIdentical(t *testing.T) {
	ctx := context.Background()
	target := whiteProgramme(20000, 3)
	donor := whiteProgramme(20000, 4)

	fresh, err := Analyze(ctx, target, donor, 8000, 1.0)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	scratch := NewScratch()
	for run := 0; run < 3; run++ {
		got, err := scratch.Analyze(ctx, target, donor, 8000, 1.0)
		if err != nil {
			t.Fatalf("run %d: Analyze: %v", run, err)
		}
		if got != fresh {
			t.Fatalf("run %d on a reused Scratch = %+v, fresh run = %+v", run, got, fresh)
		}
	}

	// A different length drops the pool's buffers instead of handing a stale
	// length to the transform.
	other := whiteProgramme(9000, 5)
	want, err := Analyze(ctx, other, other, 8000, 1.0)
	if err != nil {
		t.Fatalf("Analyze (other length): %v", err)
	}
	got, err := scratch.Analyze(ctx, other, other, 8000, 1.0)
	if err != nil {
		t.Fatalf("Analyze (other length) on a reused Scratch: %v", err)
	}
	if got != want {
		t.Fatalf("other length on a reused Scratch = %+v, fresh run = %+v", got, want)
	}
}
