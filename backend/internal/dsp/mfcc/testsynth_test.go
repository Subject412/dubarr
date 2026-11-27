package mfcc

import (
	"math"

	"github.com/go-fft/fft"
)

// This file is the fixture builder for the tests in this package: a
// deterministic, programme-like signal and the degradation chain that #83 uses
// to show what the mel-cepstral matcher survives and the waveform matcher does
// not.
//
// It is deliberately duplicated in testdata/gen/main.go, which is the committed
// generator an operator runs to inspect or regenerate the media. The two copies
// must build identical audio;
// TestSyntheticBuilderMatchesCommittedHash compares this builder's samples with
// the hash the generator recorded.
const (
	// synthRate is the analysis rate every fixture uses: the engine's own.
	synthRate = uint32(8000)

	// synthSeconds is the fixture's duration.
	synthSeconds = 120.0

	// synthSeed is the constant seed the whole fixture is derived from, so the
	// files are reproducible byte for byte.
	synthSeed = uint64(0x5eedbeef)

	// synthDelayMS is the pair's delay: a whole number of hops (25 at 8 kHz),
	// so the expected answer is exact rather than a rounding accident. The
	// target is the earlier of the two slices, which is what makes it the
	// positive offset [synthPair] builds.
	synthDelayMS = 250
)

// The fixture's texture. Programme is broadband and never stationary: the bank
// below spreads band-limited noise across the analysed band and drifts each
// stream's gain, so the spectral envelope moves on the frame scale the way real
// programme does.
//
// The shape matters more than the sound, and so does the drift being aperiodic.
// A pair of steady tones gives the lag curve a delta peak plus a one-hop
// shoulder — the cepstra of a stationary spectrum barely move from frame to
// frame — and sinusoidal gain drift is nearly as bad: a sinusoid correlates with
// itself at dozens of lags, which lifts the whole curve and caps the match z
// near 9 for either matcher. Broadband material with a random, fast-changing
// envelope puts a sharp peak on a noise-like curve instead, which is the material
// a confidence number is meant to be measured on.
const (
	// bandCount is how many noise streams the bank sums.
	bandCount = 8

	// bandResonance is the two-pole resonators' pole radius: 0.85 at 8 kHz is a
	// band about 380 Hz wide — wide enough to fill its part of the spectrum with
	// noise, narrow enough that a 25 ms frame's band energy still fluctuates.
	bandResonance = 0.85

	// programmeLevel is the RMS every variant is scaled to, so the fixtures have
	// one level whatever their gains do.
	programmeLevel = 0.16
)

var (
	// bandCentres spread the bank from 250 Hz to 3250 Hz, the band the mel
	// filterbank actually covers at 8 kHz.
	bandCentres = [bandCount]float64{250, 520, 820, 1180, 1600, 2050, 2580, 3250}

	// driftHz is each stream's gain contour: a one-pole smoothed random walk at
	// that corner frequency, so the envelopes are aperiodic and decorrelate over
	// a few tens of milliseconds instead of repeating.
	driftHz = [bandCount]float64{6.1, 9.7, 13.3, 4.9, 17.1, 7.9, 21.7, 11.3}
)

// The codec emulation's knobs, in the order the chain applies them.
const (
	// degradedLowPass is the codec's band-limit: a one-pole coefficient that
	// rolls the top octave off, as an encoder's anti-alias filter does. It is
	// fixed, so its per-band log-energy shift is a constant the cepstral
	// standardisation removes.
	degradedLowPass = 0.6

	// degradedDrive is the loudness push into the encoder's knee.
	degradedDrive = 2.5

	// The STFT round trip: 1024-point Hann at 50 % overlap, magnitudes
	// quantised in the log domain to codecMagBits bits over codecRangeDB dB.
	codecFFT     = 1024
	codecHop     = codecFFT / 2
	codecMagBits = 4
	codecRangeDB = 48.0

	// degradedBits is the depth the codec writes its output at.
	degradedBits = 6
)

// synthLCG is the fixture's random source: a 64-bit linear congruential
// generator, the same shape correlate's self-test uses, so no fixture depends on
// the standard library's generator changing.
type synthLCG struct{ state uint64 }

// newSynthLCG seeds the generator; the seed is forced odd so state 0 is not a
// fixed point.
func newSynthLCG(seed uint64) *synthLCG { return &synthLCG{state: seed | 1} }

// next returns the next value in -1..1.
func (g *synthLCG) next() float64 {
	g.state = g.state*6364136223846793005 + 1442695040888963407
	return float64(g.state>>40)/8388608.0 - 1.0
}

// bandNoise sums the bank: white noise through bandCount two-pole resonators at
// fixed centres, each weighted by its own aperiodic gain contour. The sum is
// scaled to programmeLevel RMS and clamped, so the fixture has one level whatever
// the contours do.
func bandNoise(seconds float64, seed uint64) []float32 {
	rate := float64(synthRate)
	n := int(math.Round(seconds * rate))
	rng := newSynthLCG(seed)

	var cosW, pole [bandCount]float64
	for b, f := range bandCentres {
		cosW[b] = math.Cos(2 * math.Pi * f / rate)
		pole[b] = math.Exp(-2 * math.Pi * driftHz[b] / rate)
	}

	out := make([]float32, n)
	var y1, y2, walk [bandCount]float64
	for i := range out {
		carrier := rng.next()
		envelope := rng.next()
		var v float64
		for b := 0; b < bandCount; b++ {
			y := 2*bandResonance*cosW[b]*y1[b] - bandResonance*bandResonance*y2[b] + (1-bandResonance)*carrier
			y2[b], y1[b] = y1[b], y

			walk[b] = pole[b]*walk[b] + (1-pole[b])*envelope
			// The walk's own standard deviation is sqrt((1-p)/(1+p)); scaling by
			// its inverse puts every stream's contour on one scale, and a gain
			// that saturates now and then keeps the envelope from being a
			// near-constant tilt.
			deviation := math.Sqrt((1 - pole[b]) / (1 + pole[b]))
			unit := clamp64(0.5+0.8*walk[b]/deviation, 0, 1)
			v += (0.12 + 0.95*unit) * y
		}
		out[i] = float32(v)
	}
	return scaleToProgrammeLevel(out, sumSquares(out))
}

// programme builds the fixture's clean signal.
func programme() []float32 { return programmeAt(synthSeconds) }

// programmeAt builds the same texture over an arbitrary span, which is what a
// pair cut out of one source needs: the source has to be longer than a slice by
// the lag.
func programmeAt(seconds float64) []float32 { return bandNoise(seconds, synthSeed) }

// chirpProgramme builds a linear sweep from 300 Hz to 3400 Hz across seconds. Its
// cepstral sequences drift monotonically and deterministically, with no
// frame-to-frame noise, so the lag curve is a smooth hill whose argmax is a
// property of the fixture rather than of the sampling.
func chirpProgramme(seconds float64) []float32 {
	rate := float64(synthRate)
	n := int(math.Round(seconds * rate))

	const (
		f0 = 300.0
		f1 = 3400.0
	)
	out := make([]float32, n)
	var phase float64
	for i := range out {
		t := float64(i) / rate
		f := f0 + (f1-f0)*t/seconds
		phase += 2 * math.Pi * f / rate
		out[i] = float32(0.6 * math.Sin(phase))
	}
	return out
}

// boundaryPair builds the two slices
// [TestMFCCPinsABoundaryPeakToTheSearchEdge] correlates: one swept source cut
// into two 4 s slices whose true lag sits two hops past the 1 s search bound.
//
// Both slices are cut out of the source instead of padded, so neither carries a
// silent head: a 1 s zero head is a step in the coefficient streams that the
// per-slice standardisation turns into the dominant feature of the lag curve,
// which is an artefact of the fixture rather than of the matcher.
//
// The source is [chirpProgramme] on purpose. Its cepstra are a smooth monotone
// function of time, so the correlation is a smooth monotone function of how far
// the two slices are out of alignment, and the maximum inside the window is the
// lag that is least out of alignment — the edge. A noise-like texture has no
// correlation left two hops out and would answer whichever lag the noise favours.
//
// The true lag is int(synthRate) + two hops = 8160 samples (102 hops), so the
// scan stops two hops short of it, at the bound.
func boundaryPair() (target, donor []float32) {
	slice := 4 * int(synthRate)
	lag := int(synthRate) + 2*hopSamples(synthRate)

	source := chirpProgramme(float64(lag+slice) / float64(synthRate))
	return source[:slice], source[lag : lag+slice]
}

// pairAtLag cuts two equal-length slices of one programme source whose content
// is lag samples apart: the target is the earlier slice, so the offset the
// matcher reports is +lag.
//
// Cutting rather than zero-padding the delay matters. A silent head is a step at
// the start of the target's coefficient streams, and once the slice is
// standardised that step carries most of the frame energy, which makes the NCC
// denominator — and with it the whole lag curve — a function of where the head
// falls in the overlap. That is a fixture artefact the old pair put in the way of
// the confidence numbers.
func pairAtLag(lag int, seconds float64) (target, donor []float32) {
	source := programmeAt(seconds + float64(lag)/float64(synthRate))
	n := int(math.Round(seconds * float64(synthRate)))
	return source[:n], source[lag : lag+n]
}

// synthPair builds the fixture's three slices: the target holds the same
// programme as the donor, starting synthDelayMS/1000 earlier, and the degraded
// donor is the donor through the degradation chain.
//
// The earlier slice is the target on purpose. correlate's sign convention is
// offset = t_target − t_donor, so a target whose content comes first is the
// positive answer — the same construction correlate's own TestKnownShift uses —
// and the fixture's +250 ms is the offset the tests and expected.json assert. The
// two slices swapped would answer −2000 samples.
func synthPair() (target, donor, degraded []float32) {
	// The constant must be made a float before dividing: 250/1000 in untyped
	// integer arithmetic is 0, which silently builds a zero-delay pair.
	delay := int(math.Round(float64(synthDelayMS) / 1000 * float64(synthRate)))
	target, donor = pairAtLag(delay, synthSeconds)
	return target, donor, degrade(donor)
}

// delaySamples shifts samples later by delay samples, padding the head with
// zeros and trimming the tail, so the result has the donor's length.
func delaySamples(samples []float32, delay int) []float32 {
	out := make([]float32, len(samples))
	for i := range out {
		if j := i - delay; j >= 0 {
			out[i] = samples[j]
		}
	}
	return out
}

// scaleToProgrammeLevel scales samples to programmeLevel RMS and clamps them,
// given their sum of squares, so every variant of the fixture has one level.
func scaleToProgrammeLevel(samples []float32, sumsq float64) []float32 {
	if rms := math.Sqrt(sumsq / float64(len(samples))); rms > 1e-12 {
		scale := float32(programmeLevel / rms)
		for i := range samples {
			samples[i] = clamp32(samples[i]*scale, -0.95, 0.95)
		}
	}
	return samples
}

// sumSquares is the total of values squared.
func sumSquares(values []float32) float64 {
	var total float64
	for _, v := range values {
		total += float64(v) * float64(v)
	}
	return total
}

// degrade runs the donor through what a low-bitrate re-encode does to a track:
// the codec's band-limit, a loudness push past the encoder's knee, one STFT
// round trip at coarse resolution, and a 6-bit output.
//
// The round trip is the part that matters, and it models the process this
// fixture is for. A perceptual codec at a few tens of kbit/s stops carrying the
// waveform: it transmits band energies and regenerates the fine structure as
// noise (perceptual noise substitution in AAC, the same idea as a speech codec's
// noise excitation). The mel envelope — what [frames] measures — survives that,
// because it is exactly what the encoder kept; the sample-level detail the
// waveform matcher's NCC lives on does not. Additive white noise is a different
// process: it costs both matchers the same SNR-bounded amount, so tightening it
// separates nothing, and it was modelling the wrong thing here.
func degrade(in []float32) []float32 {
	// The codec's band-limit: a one-pole low-pass.
	lp := make([]float32, len(in))
	var state float64
	for i, v := range in {
		state += degradedLowPass * (float64(v) - state)
		lp[i] = float32(state)
	}

	// The encoder's knee: tanh(3·drive·x) clips the peaks the loudness push
	// takes past it.
	for i, v := range lp {
		lp[i] = float32(math.Tanh(3 * degradedDrive * float64(v)))
	}

	codec := codecRoundTrip(lp)

	// The depth the codec writes: 6-bit PCM.
	levels := float64(int(1) << (degradedBits - 1))
	for i, v := range codec {
		codec[i] = clamp32(float32(math.Round(float64(v)*levels)/levels), -1, 1)
	}
	return codec
}

// codecRoundTrip is the lossy-codec emulation: an STFT (Hann, 50 % overlap)
// whose magnitudes are quantised coarsely in the log domain and whose phases are
// not transmitted at all, followed by an inverse STFT.
//
// Phase is quantised onto one cell — the whole circle — which is the coarsest
// quantiser there is: the decoder draws a phase inside the cell at random. That
// is the noise-substitution path of a real codec, and it is the step that
// separates the two matchers here: every bin keeps its magnitude, so each band's
// energy (and therefore the mel envelope) is preserved, while the samples are
// rebuilt from a different fine structure and the waveform NCC collapses.
//
// Magnitudes are quantised against one full-scale reference for the whole slice,
// not per frame, so a quiet frame keeps its level instead of being renormalised
// to full scale.
func codecRoundTrip(in []float32) []float32 {
	nfft, hop := codecFFT, codecHop
	window := hannWindow(nfft)
	opts := fft.Options{NoFMA: true}
	buf := make([]complex64, nfft)

	// Pass 1: the encoder's full scale.
	var reference float64
	for start := 0; start < len(in); start += hop {
		windowedFrame(buf, in, start, window)
		for k, bin := range fft.FFT32With(buf, opts) {
			if k > nfft/2 {
				break
			}
			reference = max(reference, math.Hypot(float64(real(bin)), float64(imag(bin))))
		}
	}
	if reference <= 0 {
		return make([]float32, len(in))
	}

	// A codecMagBits-bit quantiser over codecRangeDB decibels: 3 dB steps at 4
	// bits, and the bottom level is the codec's noise floor.
	step := codecRangeDB * math.Ln10 / 20 / float64(int(1)<<codecMagBits)
	floorLevel := -float64(int(1)<<codecMagBits - 1)
	quantise := func(magnitude float64) float64 {
		level := math.Round(math.Log(magnitude/reference) / step)
		return reference * math.Exp(max(level, floorLevel)*step)
	}

	// Pass 2: quantise, regenerate the phase, overlap-add.
	out := make([]float64, len(in)+nfft)
	norm := make([]float64, len(in)+nfft)
	rng := newSynthLCG(synthSeed ^ 0x5eed)

	for start := 0; start < len(in); start += hop {
		windowedFrame(buf, in, start, window)
		spectrum := fft.FFT32With(buf, opts)

		// DC and Nyquist are real; the rest are mirrored so the frame stays a
		// real signal.
		dc := float64(real(spectrum[0]))
		spectrum[0] = complex(float32(math.Copysign(quantise(math.Abs(dc)), dc)), 0)
		for k := 1; k < nfft/2; k++ {
			magnitude := quantise(math.Hypot(float64(real(spectrum[k])), float64(imag(spectrum[k]))))
			phase := math.Pi * rng.next() // the cell is the whole circle: -π..π
			bin := complex(float32(magnitude*math.Cos(phase)), float32(magnitude*math.Sin(phase)))
			spectrum[k] = bin
			spectrum[nfft-k] = complex(real(bin), -imag(bin))
		}
		spectrum[nfft/2] = complex(float32(quantise(math.Abs(float64(real(spectrum[nfft/2]))))), 0)

		frame := fft.IFFT32With(spectrum, opts)
		for i := 0; i < nfft; i++ {
			out[start+i] += float64(window[i]) * float64(real(frame[i]))
			norm[start+i] += float64(window[i]) * float64(window[i])
		}
	}

	round := make([]float32, len(in))
	for i := range round {
		if n := norm[i]; n > 1e-9 {
			round[i] = float32(out[i] / n)
		}
	}
	return round
}

// windowedFrame copies samples[start:start+nfft] into buf under the analysis
// window, zero-padding past the end of the slice.
func windowedFrame(buf []complex64, samples []float32, start int, window []float32) {
	for i := range buf {
		if j := start + i; j < len(samples) {
			buf[i] = complex(samples[j]*window[i], 0)
		} else {
			buf[i] = 0
		}
	}
}

// hannWindow returns the codec's analysis and synthesis window:
// 0.5 − 0.5·cos(2πi/(n−1)).
func hannWindow(n int) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1)))
	}
	return out
}

// clamp32 bounds v to [lo, hi].
func clamp32(v, lo, hi float32) float32 { return min(max(v, lo), hi) }

// clamp64 bounds v to [lo, hi].
func clamp64(v, lo, hi float64) float64 { return min(max(v, lo), hi) }
