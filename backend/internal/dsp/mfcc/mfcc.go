package mfcc

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/go-fft/fft"

	"github.com/Subject412/dubarr/backend/internal/dsp/correlate"
)

// Analysis parameters. They are constants, not settings: a knob that changes
// the meaning of every stored offset is not an option (D11 says technical knobs
// live behind defaults that are correct for everybody).
const (
	// FrameMS is the analysis frame's length in milliseconds: 200 samples at
	// 8 kHz, transformed at nfft = nextPow2(frame) = 256.
	FrameMS = 25

	// HopMS is the frame step in milliseconds, and it is also the matcher's
	// measurement precision: 80 samples and 10.0 ms at 8 kHz.
	HopMS = 10

	// MelBands is how many triangular filters span the band, 0 Hz to
	// min(FMaxHz, rate/2).
	MelBands = 26

	// Cepstra is how many DCT-II coefficients are kept per frame. c0 is kept
	// because it is the frame's log-energy contour, the cheapest and most
	// compression-robust cue once standardised.
	Cepstra = 13

	// Preemphasis is the one-tap pre-emphasis coefficient: y[n] = x[n] − 0.97·x[n−1].
	Preemphasis = 0.97

	// FMaxHz is the top of the analysed band, clamped to Nyquist at low rates.
	FMaxHz = 4000
)

const (
	// logFloor floors a band energy before the logarithm, so a silent band is a
	// very small number rather than −Inf.
	logFloor = 1e-12

	// energyFloor is the denominator below which a lag carries no evidence: the
	// same cut correlate applies to its NCC.
	energyFloor = 1e-12

	// lagCheckInterval is how often the lag scan looks at ctx, in hops.
	lagCheckInterval = 64
)

// Analysis contract, mirrored from correlate: [Analyze] returns a
// [correlate.Analysis] whose fields mean what the waveform matcher's mean,
// except that the offset is quantised to the hop and that MatchZ is not a gate.
//
// Analyze correlates the mel-cepstral representation of two slices and returns
// the answer in the shared shape.
//
// OffsetSamples is quantised to the hop: the answer is the lag, in hops, of the
// best match, times the hop's sample count. OffsetSubSamples is that value (no
// fine interpolation: a sub-hop number computed from a 10 ms grid would be a
// guess dressed as precision). PrecisionMS is the hop in milliseconds, which is
// what the report and the UI must show.
//
// MatchZ and Quality are computed exactly as the waveform matcher computes them
// — (peak−mean)/std and (peak−median)/(1−median) over the matcher's own lag
// curve — but the formulas agreeing does not make the numbers comparable. This
// matcher scores a few thousand heavily overlapping hop lags where the sample
// correlator scores ~8·10⁴ near-independent ones, so the spread is larger
// relative to the peak: a correct answer here scores z 5.3 where the waveform
// scores 21-28 on the same committed pair. Quality is therefore what decides
// this matcher's answers (analyze.Technique.ZGate is false for it) and MatchZ is
// reported as a diagnostic.
//
// The errors are correlate's, value for value: *correlate.InvalidJob,
// *correlate.SilentSlice, *correlate.SliceTooShort and *correlate.NoValidLag,
// raised in the same order and with the same wording, so the engine seam can
// swap one matcher for the other without an adapter.
//
// A cancelled ctx returns an error wrapping ctx.Err() and never a partial
// Analysis.
func Analyze(ctx context.Context, target, donor []float32, sampleRate uint32, windowSec float64) (correlate.Analysis, error) {
	return analyze(ctx, target, donor, sampleRate, windowSec)
}

// frames converts one slice into a [frames][Cepstra] matrix plus its per-frame
// total energy (the denominator of the NCC).
//
// The pipeline is the plan's, in its order: pre-emphasis, Hamming-windowed
// frames at the hop, one real FFT per frame, the mel filterbank, the logarithm,
// the DCT-II keeping [Cepstra] coefficients, then a per-coefficient
// standardisation over the whole slice. Standardising is what makes the matcher
// survive a re-encode: a low-bitrate copy attenuates the high band, which shifts
// each coefficient by a constant, and a constant is exactly what the mean
// subtraction removes. The energy is computed after standardisation, so the NCC
// measures the shape's agreement and not the level's.
//
// A slice shorter than one frame yields no frames: there is nothing to
// correlate, and the caller turns that into correlate's NoValidLag.
func frames(samples []float32, sampleRate uint32) ([][]float32, []float64) {
	length, hop := frameSamples(sampleRate), hopSamples(sampleRate)
	if length < 2 || len(samples) < length {
		return nil, nil
	}

	count := (len(samples)-length)/hop + 1
	nfft := nextPow2(length)
	bank := filterbank(sampleRate, nfft)
	window := hamming(length)
	emphasised := preEmphasis(samples, Preemphasis)

	coeffs := make([][]float32, count)
	buffer := make([]float64, MelBands)
	frame := make([]float32, nfft)
	for f := 0; f < count; f++ {
		start := f * hop
		for i := 0; i < length; i++ {
			frame[i] = emphasised[start+i] * window[i]
		}
		for i := length; i < nfft; i++ {
			frame[i] = 0
		}

		power := powerSpectrum(frame, nfft)
		for b, weights := range bank {
			var band float64
			for k, w := range weights {
				band += float64(w) * power[k]
			}
			if band < logFloor {
				band = logFloor
			}
			buffer[b] = math.Log(band)
		}

		kept := dctII(buffer, Cepstra)
		row := make([]float32, len(kept))
		for i, v := range kept {
			row[i] = float32(v)
		}
		coeffs[f] = row
	}

	standardise(coeffs)

	energy := make([]float64, count)
	for f, row := range coeffs {
		var total float64
		for _, v := range row {
			total += float64(v) * float64(v)
		}
		energy[f] = total
	}
	return coeffs, energy
}

// standardise centres and scales every coefficient stream over the whole slice,
// so each of the [Cepstra] coefficients carries equal weight in the summed
// correlation. A stream that does not vary at all becomes all zeros rather than
// dividing by zero.
func standardise(coeffs [][]float32) {
	if len(coeffs) == 0 {
		return
	}
	width := len(coeffs[0])
	n := float64(len(coeffs))
	for c := 0; c < width; c++ {
		var sum, sumSquares float64
		for _, row := range coeffs {
			v := float64(row[c])
			sum += v
			sumSquares += v * v
		}
		mean := sum / n
		std := math.Sqrt(max(sumSquares/n-mean*mean, 0))
		if std < 1e-9 {
			for _, row := range coeffs {
				row[c] = 0
			}
			continue
		}
		offset, scale := float32(mean), float32(1/std)
		for _, row := range coeffs {
			row[c] = (row[c] - offset) * scale
		}
	}
}

// preEmphasis applies y[n] = x[n] − coefficient·x[n−1], with x[−1] = 0.
func preEmphasis(samples []float32, coefficient float64) []float32 {
	out := make([]float32, len(samples))
	if len(samples) == 0 {
		return out
	}
	out[0] = samples[0]
	coef := float32(coefficient)
	for i := 1; i < len(samples); i++ {
		out[i] = samples[i] - coef*samples[i-1]
	}
	return out
}

// hamming returns the analysis window of length n: 0.54 − 0.46·cos(2πi/(n−1)).
func hamming(n int) []float32 {
	out := make([]float32, n)
	if n == 1 {
		out[0] = 1
		return out
	}
	for i := range out {
		out[i] = float32(0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/float64(n-1)))
	}
	return out
}

// frameSamples is the frame length in samples: round(rate·FrameMS/1000).
func frameSamples(sampleRate uint32) int {
	return int(math.Round(float64(sampleRate) * FrameMS / 1000))
}

// hopSamples is the hop in samples: round(rate·HopMS/1000), floored at one so a
// very low analysis rate still steps.
func hopSamples(sampleRate uint32) int {
	hop := int(math.Round(float64(sampleRate) * HopMS / 1000))
	if hop < 1 {
		hop = 1
	}
	return hop
}

// analyze runs one correlation. The two transform buffers it needs are
// allocated here: nothing outside this package analysed the same coefficient
// length twice in a row, so a pool had no reuse site to justify it.
func analyze(ctx context.Context, target, donor []float32, sampleRate uint32, windowSec float64) (correlate.Analysis, error) {
	// Validation, in correlate's order and wording. The order is part of the
	// contract: a caller with two problems gets the error the waveform matcher
	// would have reported for the same input.
	if sampleRate == 0 {
		return correlate.Analysis{}, &correlate.InvalidJob{Msg: "sample_rate must be greater than zero"}
	}
	if math.IsNaN(windowSec) || math.IsInf(windowSec, 0) || windowSec <= 0 {
		return correlate.Analysis{}, &correlate.InvalidJob{Msg: "window_sec must be a positive number"}
	}
	nx, ny := len(target), len(donor)
	if nx < correlate.MinSliceSamples {
		return correlate.Analysis{}, &correlate.SliceTooShort{Samples: nx, Min: correlate.MinSliceSamples}
	}
	if ny < correlate.MinSliceSamples {
		return correlate.Analysis{}, &correlate.SliceTooShort{Samples: ny, Min: correlate.MinSliceSamples}
	}

	// The search bound in samples (as correlate computes it), then in hops: a
	// lag is only ever a whole number of hops, so the bound is floored onto the
	// grid the answer lives on. A window narrower than one hop has no lag to
	// score.
	hop := hopSamples(sampleRate)
	shorter := min(nx, ny)
	maxLagSamples := shorter - 1
	if requested := math.Round(windowSec * float64(sampleRate)); requested < float64(maxLagSamples) {
		maxLagSamples = int(requested)
	}
	maxLagHops := maxLagSamples / hop
	if maxLagHops == 0 {
		return correlate.Analysis{}, &correlate.NoValidLag{}
	}

	// Nothing below this point is cheap, so a cancelled caller leaves without
	// transforming anything.
	if err := ctx.Err(); err != nil {
		return correlate.Analysis{}, fmt.Errorf("mfcc: %w", err)
	}

	// Silence is rejected on the samples, in the same arithmetic and with the
	// same wording as correlate's prepare: target first.
	if err := checkSignal(target, "target"); err != nil {
		return correlate.Analysis{}, err
	}
	if err := checkSignal(donor, "donor"); err != nil {
		return correlate.Analysis{}, err
	}

	x, ex := frames(target, sampleRate)
	y, ey := frames(donor, sampleRate)
	if len(x) < 2 || len(y) < 2 {
		// Fewer than two scored frames means there is no lag curve at all.
		return correlate.Analysis{}, &correlate.NoValidLag{}
	}
	if err := ctx.Err(); err != nil {
		return correlate.Analysis{}, fmt.Errorf("mfcc: %w", err)
	}

	// S(k) = Σ_c Σ_n X[n][c]·Y[n−k][c]: the [Cepstra] linear correlations,
	// summed, each computed with one padded RFFT pair and one inverse at
	// nfft = nextPow2(len(x)+len(y)−1). The products replace X in place, so the
	// buffers stay at two.
	nfft := nextPow2(len(x) + len(y) - 1)
	opts := fft.Options{NoFMA: true}
	sum := make([]float64, nfft)

	xb := make([]complex64, nfft)
	yb := make([]complex64, nfft)

	width := len(x[0])
	for c := 0; c < width; c++ {
		if err := ctx.Err(); err != nil {
			return correlate.Analysis{}, fmt.Errorf("mfcc: %w", err)
		}
		fillCoefficient(xb, x, c)
		fillCoefficient(yb, y, c)
		X := fft.FFT32With(xb, opts)
		Y := fft.FFT32With(yb, opts)
		for i := range X {
			xr, xi := real(X[i]), imag(X[i])
			yr, yi := real(Y[i]), imag(Y[i])
			X[i] = complex(xr*yr+xi*yi, xi*yr-xr*yi)
		}
		correlation := fft.IFFT32With(X, opts)
		for i, v := range correlation {
			sum[i] += float64(real(v))
		}
	}

	// ex and ey already hold Σ_c X[n][c]², one energy per frame, so the
	// prefix of them is a prefix of sums — squaring here a second time would
	// weight every frame by its own energy and leave the NCC measuring the
	// loudness contour's fourth moment instead of the coefficient agreement.
	px := prefixSums(ex)
	py := prefixSums(ey)
	// The overlap rule and its floor: 5 % of the shorter slice, never fewer than
	// 16 frames, as correlate keeps 16 samples.
	minOverlap := max(16, min(len(x), len(y))/20)

	// The lag scan: lag k ≥ 0 indexes sum at k, k < 0 at nfft+k, the overlap
	// shrinks as |k| grows, and a lag whose energies do not multiply to
	// something positive is skipped rather than scored. The comparison is
	// strictly `>`, so a tie keeps the lowest lag — the same rule, in hops,
	// that correlate applies in samples.
	scores := make([]float64, 0, 2*maxLagHops+1)
	bestHop, peak, found := 0, 0.0, false
	var sumScores, sumSquares float64

	for k := -maxLagHops; k <= maxLagHops; k++ {
		if k%lagCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return correlate.Analysis{}, fmt.Errorf("mfcc: %w", err)
			}
		}

		var count, xStart, yStart int
		if k >= 0 {
			count = min(len(x), len(y)+k) - k
			xStart, yStart = k, 0
		} else {
			l := -k
			count = min(len(x), len(y)-l)
			xStart, yStart = 0, l
		}
		if count < minOverlap {
			continue
		}

		index := k
		if k < 0 {
			index = nfft + k
		}
		denom := math.Sqrt(segmentSum(px, xStart, count) * segmentSum(py, yStart, count))
		if denom <= energyFloor {
			continue
		}
		ncc := sum[index] / denom
		ncc = min(max(ncc, -1), 1)

		scores = append(scores, ncc)
		sumScores += ncc
		sumSquares += ncc * ncc
		if !found || ncc > peak {
			bestHop, peak, found = k, ncc, true
		}
	}

	if !found {
		return correlate.Analysis{}, &correlate.NoValidLag{}
	}

	// The answer is a hop count, reported as samples: bestHop·hop. There is no
	// sub-hop interpolation, so OffsetSubSamples is OffsetSamples exactly.
	offset := int64(bestHop) * int64(hop)
	return correlate.Analysis{
		OffsetSamples:    offset,
		Quality:          peakProminence(peak, scores),
		OffsetSubSamples: float64(offset),
		MatchZ:           float32(matchZ(peak, len(scores), sumScores, sumSquares)),
		PrecisionMS:      1000 * float64(hop) / float64(sampleRate),
	}, nil
}

// checkSignal rejects a slice whose RMS is effectively zero, in correlate's
// arithmetic: the mean and the RMS are accumulated in float64 from the float32
// samples, and the threshold is the same 1e-9.
func checkSignal(samples []float32, which string) error {
	var sum float64
	for _, v := range samples {
		sum += float64(v)
	}
	mean := sum / float64(len(samples))

	var squares float64
	for _, v := range samples {
		d := float64(v) - mean
		squares += d * d
	}
	if math.Sqrt(squares/float64(len(samples))) <= 1e-9 {
		return &correlate.SilentSlice{Which: which}
	}
	return nil
}

// fillCoefficient writes coefficient c of every frame into buf as reals and
// zeroes the padding a linear (not circular) correlation needs.
func fillCoefficient(buf []complex64, coeffs [][]float32, c int) {
	for n, row := range coeffs {
		buf[n] = complex(row[c], 0)
	}
	clear(buf[len(coeffs):])
}

// prefixSums returns the running sum, so the total of any
// `[start, start+length)` range is one subtraction. Its input is a per-frame
// energy (already a sum of squares), not the coefficients themselves.
func prefixSums(values []float64) []float64 {
	prefix := make([]float64, len(values)+1)
	var acc float64
	for i, v := range values {
		acc += v
		prefix[i+1] = acc
	}
	return prefix
}

// segmentSum is the total over values[start:start+length].
func segmentSum(prefix []float64, start, length int) float64 {
	return prefix[start+length] - prefix[start]
}

// peakProminence scores the peak against the median lag response, in `0..=1`,
// exactly as correlate's does: `(peak - median) / (1 - median)`, clamped, and 0
// when there is no headroom above the median. The median is the upper middle of
// the scored lags, the value correlate's order-statistic pick returns.
//
// The lag curve here is at most a few thousand values (one per hop inside the
// window), so a sorted copy costs nothing and leaves the caller's slice intact.
func peakProminence(peak float64, scores []float64) float32 {
	if len(scores) == 0 {
		return 0
	}
	sorted := append([]float64(nil), scores...)
	sort.Float64s(sorted)
	median := sorted[len(sorted)/2]

	prominence := 0.0
	if headroom := 1 - median; headroom > 1e-12 {
		prominence = (peak - median) / headroom
	}
	return float32(min(max(prominence, 0), 1))
}

// matchZ scores the peak against the whole lag distribution:
// (peak−mean)/std over the lags the scan scored. It is correlate's formula,
// constant for constant, so the two matchers' z values live on one scale:
// sample variance `(sumSq − sum²/n)/(n−1)`, and 0 for fewer than two scored
// lags or a standard deviation below 1e-9 (the documented "no z" sentinel).
func matchZ(peak float64, n int, sum, sumSq float64) float64 {
	if n < 2 {
		return 0
	}
	mean := sum / float64(n)
	variance := (sumSq - sum*sum/float64(n)) / float64(n-1)
	std := math.Sqrt(max(variance, 0))
	if std < 1e-9 {
		return 0
	}
	return (peak - mean) / std
}

// nextPow2 returns the smallest power of two that is >= n, as correlate's does.
func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}
