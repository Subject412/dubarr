package correlate

import (
	"context"
	"fmt"
	"math"
	"slices"

	"github.com/go-fft/fft"
)

// MinSliceSamples is the shortest slice the correlator analyses, in samples.
// It mirrors MIN_SLICE_SAMPLES (lib.rs:24): below it the correlation is noise.
const MinSliceSamples = 64

// lagCheckInterval is how often the lag scan looks at ctx. The scan can run for
// a second at nfft = 2^22, which is longer than the deadline wants to wait.
const lagCheckInterval = 4096

// scratchFree is how many nfft-sized buffers a [Scratch] keeps: the two padded
// inputs plus one spare, which is the working set of a single correlation.
const scratchFree = 3

// Analysis is one correlation's answer.
type Analysis struct {
	// OffsetSamples is `t_target - t_donor` in samples: a positive value means
	// the target's audio runs late relative to the donor.
	OffsetSamples int64

	// Quality is the peak's prominence over the median lag response, in
	// `0..=1`; see [peakProminence].
	Quality float32
}

// result is what one correlation produced, including the peak NCC that
// [Analysis] does not expose. Tests read it to assert the normalisation
// directly rather than through the prominence formula.
type result struct {
	offset  int64
	peak    float64
	quality float32
}

// InvalidJob reports an argument the correlator cannot use. It renders the
// reference engine's text, so a job's recorded error keeps its shape.
type InvalidJob struct {
	Msg string
}

// Error implements error as `invalid job: <msg>`.
func (e *InvalidJob) Error() string { return "invalid job: " + e.Msg }

// SilentSlice reports a slice that carries no signal (RMS is effectively zero)
// and so cannot be correlated.
type SilentSlice struct {
	// Which names the slice: "target" or "donor".
	Which string
}

// Error implements error as `<which> slice is silent (no signal to correlate)`.
func (e *SilentSlice) Error() string {
	return e.Which + " slice is silent (no signal to correlate)"
}

// SliceTooShort reports a slice shorter than [MinSliceSamples].
type SliceTooShort struct {
	Samples int
	Min     int
}

// Error implements error as `slice too short: <n> samples (minimum <min>)`.
func (e *SliceTooShort) Error() string {
	return fmt.Sprintf("slice too short: %d samples (minimum %d)", e.Samples, e.Min)
}

// NoValidLag reports that no lag inside the search window had both enough
// overlap and enough signal to score.
type NoValidLag struct{}

// Error implements error as `no valid correlation lag inside the search
// window`.
func (e *NoValidLag) Error() string {
	return "no valid correlation lag inside the search window"
}

// Scratch recycles the nfft-sized complex buffers of repeated correlations of
// one length. A job that walks six checkpoints of the same window reuses them
// instead of allocating two fresh 33.55 MB padded inputs per checkpoint at
// nfft = 2^22.
//
// It is not safe for concurrent use: give each worker its own. Buffers are
// dropped when a correlation of a different nfft asks for the pool, because a
// shorter or longer transform cannot use them.
type Scratch struct {
	nfft int
	free [][]complex64
}

// NewScratch returns an empty buffer pool.
func NewScratch() *Scratch { return &Scratch{} }

// Analyze correlates on the package-level [Analyze], using this pool's
// buffers for the two padded inputs.
func (s *Scratch) Analyze(ctx context.Context, target, donor []float32, sampleRate uint32, windowSec float64) (Analysis, error) {
	r, err := correlate(ctx, s, target, donor, sampleRate, windowSec)
	if err != nil {
		return Analysis{}, err
	}
	return Analysis{OffsetSamples: r.offset, Quality: r.quality}, nil
}

// Analyze correlates target against donor, both sampled at sampleRate, and
// returns the offset of the correlation peak and its prominence. windowSec
// bounds the absolute offset searched, in seconds — the reference engine passes
// the analysed window's length.
//
// A cancelled ctx returns an error wrapping ctx.Err() and never a partial
// Analysis.
//
// For a job that correlates several slices of the same length, use a [Scratch]
// instead: this function allocates one nfft-sized buffer per transform.
func Analyze(ctx context.Context, target, donor []float32, sampleRate uint32, windowSec float64) (Analysis, error) {
	r, err := correlate(ctx, nil, target, donor, sampleRate, windowSec)
	if err != nil {
		return Analysis{}, err
	}
	return Analysis{OffsetSamples: r.offset, Quality: r.quality}, nil
}

// correlate runs one correlation, taking and returning its padded inputs from
// s when s is not nil.
func correlate(ctx context.Context, s *Scratch, target, donor []float32, sampleRate uint32, windowSec float64) (result, error) {
	// Validation, in the reference engine's order and wording
	// (correlation.rs:68-97). The order is part of the contract: a caller with
	// two problems gets the error the Rust engine reported for the same input.
	if sampleRate == 0 {
		return result{}, &InvalidJob{Msg: "sample_rate must be greater than zero"}
	}
	if math.IsNaN(windowSec) || math.IsInf(windowSec, 0) || windowSec <= 0 {
		return result{}, &InvalidJob{Msg: "window_sec must be a positive number"}
	}
	nx, ny := len(target), len(donor)
	if nx < MinSliceSamples {
		return result{}, &SliceTooShort{Samples: nx, Min: MinSliceSamples}
	}
	if ny < MinSliceSamples {
		return result{}, &SliceTooShort{Samples: ny, Min: MinSliceSamples}
	}

	// The search bound and the overlap every scored lag needs
	// (correlation.rs:93-97, :130). math.Round and Rust's f64::round both round
	// half away from zero, so window_sec maps to the same sample count. The
	// comparison is done in float64 so a window far wider than the slice
	// saturates at shorter-1 instead of converting an out-of-range float.
	shorter := min(nx, ny)
	maxLag := shorter - 1
	if requested := math.Round(windowSec * float64(sampleRate)); requested < float64(maxLag) {
		maxLag = int(requested)
	}
	if maxLag == 0 {
		return result{}, &NoValidLag{}
	}

	// Nothing below this point is cheap, so a cancelled caller leaves without
	// allocating a buffer.
	if err := ctx.Err(); err != nil {
		return result{}, fmt.Errorf("correlate: %w", err)
	}

	x, err := prepare(target, "target")
	if err != nil {
		return result{}, err
	}
	y, err := prepare(donor, "donor")
	if err != nil {
		return result{}, err
	}

	nfft := nextPow2(nx + ny - 1)
	opts := fft.Options{NoFMA: true}

	// X and Y: the forward transforms of the prepared slices, zero-padded to
	// nfft. FFT32With copies its input into a new nfft buffer, so the padded
	// input is dead the moment the spectrum exists and goes straight back.
	xb := s.take(nfft)
	fillPadded(xb, x)
	X := fft.FFT32With(xb, opts)
	s.give(xb)
	xb = nil
	if err := ctx.Err(); err != nil {
		return result{}, fmt.Errorf("correlate: %w", err)
	}

	yb := s.take(nfft)
	fillPadded(yb, y)
	Y := fft.FFT32With(yb, opts)
	s.give(yb)
	yb = nil
	if err := ctx.Err(); err != nil {
		return result{}, fmt.Errorf("correlate: %w", err)
	}

	// X * conj(Y) in float32 (correlation.rs:118-121). The product replaces X,
	// so no third spectrum is allocated for it.
	for i := range X {
		xr, xi := real(X[i]), imag(X[i])
		yr, yi := real(Y[i]), imag(Y[i])
		X[i] = complex(xr*yr+xi*yi, xi*yr-xr*yi)
	}
	Y = nil

	// The inverse, and no scale after it. go-fft's IFFT32 is normalised by
	// nfft where rustfft's inverse is not, so the engine's `scale := 1/nfft`
	// (correlation.rs:123-126) is dropped rather than ported: with both, every
	// NCC would be ~1/nfft and Quality ~1e-7. TestAutocorrelationPeakIsOne is
	// the guard. (A reusable fft.Plan32 would let dst alias src and allocate
	// nothing, but NewPlan32 takes the fused kernel where one exists, whose
	// bits depend on the CPU — hence the ...With forms, as the port requires.)
	corr := fft.IFFT32With(X, opts)
	s.give(X)
	X = nil
	defer s.give(corr)
	if err := ctx.Err(); err != nil {
		return result{}, fmt.Errorf("correlate: %w", err)
	}

	px := prefixSquares(x)
	py := prefixSquares(y)
	// The prepared slices are not needed past their prefix energy.
	x, y = nil, nil
	minOverlap := max(16, shorter/20)

	// The lag scan, verbatim from correlation.rs:128-162: lag k>0 indexes the
	// correlation at k, lag k<0 at nfft-k, the overlap shrinks as |k| grows,
	// and a lag whose two energies do not multiply to something positive is
	// skipped rather than scored (so no NaN and no Inf ever reaches scores).
	// The comparison is strictly `>`, so a tie keeps the lowest lag.
	scores := make([]float64, 0, 2*maxLag+1)
	best, peak, found := int64(0), 0.0, false
	for i := 0; i <= 2*maxLag; i++ {
		if i%lagCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return result{}, fmt.Errorf("correlate: %w", err)
			}
		}
		lag := int64(i - maxLag)

		var count, xStart, yStart int
		if lag >= 0 {
			l := int(lag)
			count = min(nx, ny+l) - l
			xStart, yStart = l, 0
		} else {
			l := int(-lag)
			count = min(nx, ny-l)
			xStart, yStart = 0, l
		}
		if count < minOverlap {
			continue
		}

		index := int(lag)
		if lag < 0 {
			index = nfft + int(lag)
		}
		raw := float64(real(corr[index]))
		denom := math.Sqrt(energy(px, xStart, count) * energy(py, yStart, count))
		if denom <= 1e-12 {
			continue
		}
		ncc := raw / denom
		if ncc > 1 {
			ncc = 1
		} else if ncc < -1 {
			ncc = -1
		}
		scores = append(scores, ncc)
		if !found || ncc > peak {
			best, peak, found = lag, ncc, true
		}
	}

	if !found {
		return result{}, &NoValidLag{}
	}
	return result{offset: best, peak: peak, quality: peakProminence(peak, scores)}, nil
}

// prepare removes DC, scales to unit RMS and rejects a silent slice, in the
// reference engine's arithmetic (correlation.rs:23-40): the mean and the RMS
// are accumulated in float64 from the float32 samples, the reciprocal of the
// RMS is rounded once to float32, and the samples are scaled in float32.
func prepare(samples []float32, which string) ([]float32, error) {
	var sum float64
	for _, s := range samples {
		sum += float64(s)
	}
	mean := sum / float64(len(samples))

	centered := make([]float32, len(samples))
	for i, s := range samples {
		centered[i] = float32(float64(s) - mean)
	}

	var squares float64
	for _, s := range centered {
		squares += float64(s) * float64(s)
	}
	rms := math.Sqrt(squares / float64(len(centered)))
	if rms <= 1e-9 {
		return nil, &SilentSlice{Which: which}
	}

	scale := 1 / float32(rms)
	for i := range centered {
		centered[i] *= scale
	}
	return centered, nil
}

// prefixSquares returns the running sum of squares, so the energy of any
// `[start, start+length)` range is one subtraction.
func prefixSquares(samples []float32) []float64 {
	prefix := make([]float64, len(samples)+1)
	var acc float64
	for i, s := range samples {
		acc += float64(s) * float64(s)
		prefix[i+1] = acc
	}
	return prefix
}

// energy is the sum of squares over samples[start:start+length].
func energy(prefix []float64, start, length int) float64 {
	return prefix[start+length] - prefix[start]
}

// peakProminence scores the peak against the median lag response, in `0..=1`,
// as correlation.rs:188-202 does: `(peak - median) / (1 - median)`, clamped,
// and 0 when there is no headroom above the median.
//
// The median is a plain sorted pick. That is safe without a NaN-handling
// comparator — unlike the Rust version, which needs partial_cmp(...).unwrap_or
// — because the scan skips every lag whose `denom <= 1e-12` before dividing, so
// scores holds only finite values and slices.Sort is a total order over them.
func peakProminence(peak float64, scores []float64) float32 {
	if len(scores) == 0 {
		return 0
	}
	sorted := slices.Clone(scores)
	slices.Sort(sorted)
	median := sorted[len(sorted)/2]

	prominence := 0.0
	if headroom := 1 - median; headroom > 1e-12 {
		prominence = (peak - median) / headroom
	}
	return float32(min(max(prominence, 0), 1))
}

// nextPow2 returns the smallest power of two that is >= n, as Rust's
// usize::next_power_of_two does.
func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// fillPadded writes samples into buf as reals and zeroes everything past them,
// so the transform sees the zero padding a linear (not circular) correlation
// needs.
func fillPadded(buf []complex64, samples []float32) {
	for i, s := range samples {
		buf[i] = complex(s, 0)
	}
	clear(buf[len(samples):])
}

// take returns a complex buffer of n points, from the pool when one of that
// length is waiting. Its contents are left over from the previous use;
// [fillPadded] overwrites every element it needs.
func (s *Scratch) take(n int) []complex64 {
	if s == nil {
		return make([]complex64, n)
	}
	if s.nfft != n {
		s.nfft, s.free = n, nil
	}
	if last := len(s.free) - 1; last >= 0 {
		buf := s.free[last]
		s.free = s.free[:last]
		return buf
	}
	return make([]complex64, n)
}

// give returns a buffer to the pool. Buffers of another length, and a pool that
// is already holding its working set, are dropped.
func (s *Scratch) give(buf []complex64) {
	if s == nil || len(buf) != s.nfft || len(s.free) >= scratchFree {
		return
	}
	s.free = append(s.free, buf)
}
