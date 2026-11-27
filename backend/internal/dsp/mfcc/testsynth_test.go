package mfcc

import "math"

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
	// so the expected answer is exact rather than a rounding accident. It sits
	// on the target, which is what makes it the positive offset [synthPair]
	// builds.
	synthDelayMS = 250

	// The degradation chain's knobs, in the order the chain applies them. The
	// chain is tuned so that the waveform matcher's quality falls below
	// analyze.DefaultThresholds.LowQuality on this pair while the cepstral
	// matcher's stays above it: the low-pass removes the band the samples'
	// phases live in, the clip and the quantiser add the distortion a re-encode
	// adds, and the noise floor is the re-encode's own.
	degradedTaps  = 4     // moving-average low-pass length
	degradedDrive = 2.5   // pre-gain into tanh(3x)
	degradedBits  = 6     // quantiser depth
	degradedSNRDB = -10.0 // additive noise, relative to the clipped signal
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

// programme builds the deterministic "programme-like" signal: five sinusoids
// with independent envelope rates, a slow amplitude modulation, a percussive
// click every 0.5 s whose pitch and level vary from click to click (so the
// signal is not periodic at that rate) and a seeded noise floor.
//
// The tones sit between 1500 Hz and 3600 Hz — inside the band the degradation
// chain's 4-tap moving average stops at (its gain is 0.06–0.32 there) — so the
// fixture separates the two matchers honestly: a fixed linear filter costs the
// sample-domain matcher most of its energy, while the cepstra see only a
// constant tilt per coefficient, which the standardisation removes.
func programme() []float32 {
	freqs := [5]float64{1500, 1900, 2400, 3000, 3600}
	envHz := [5]float64{0.37, 0.71, 1.13, 1.71, 2.29}
	envPhase := [5]float64{0.11, 1.37, 2.51, 4.19, 5.53}

	rate := float64(synthRate)
	n := int(math.Round(synthSeconds * rate))
	rng := newSynthLCG(synthSeed)

	clickEvery := int(math.Round(0.5 * rate))
	clickDecay := math.Exp(-1 / (0.004 * rate))
	countdown := 0
	clickEnv, clickHz := 0.0, 1200.0

	out := make([]float32, n)
	for i := range out {
		t := float64(i) / rate
		if countdown <= 0 {
			clickEnv = 0.7 + 0.5*rng.next()
			clickHz = 900 + 800*(0.5+0.5*rng.next())
			countdown = clickEvery
		}
		countdown--

		var v float64
		for k := range freqs {
			env := 0.25 + 0.75*(0.5+0.5*math.Sin(2*math.Pi*envHz[k]*t+envPhase[k]))
			v += env * math.Sin(2*math.Pi*freqs[k]*t+envPhase[k])
		}
		v *= 0.6 + 0.4*(0.5+0.5*math.Sin(2*math.Pi*0.1*t))
		v += clickEnv * math.Sin(2*math.Pi*clickHz*t)
		clickEnv *= clickDecay
		v += 0.02 * rng.next()

		out[i] = clamp32(float32(0.22*v), -0.95, 0.95)
	}
	return out
}

// noiseProgramme builds a signal unrelated to programme(): coloured noise under
// its own slow envelope, drawn from a different seed. It is the "no match" side
// of the match-z test.
func noiseProgramme(seed uint64, seconds float64) []float32 {
	rate := float64(synthRate)
	n := int(math.Round(seconds * rate))
	rng := newSynthLCG(seed)

	out := make([]float32, n)
	var lp float64
	for i := range out {
		t := float64(i) / rate
		lp = 0.85*lp + 0.15*rng.next()
		env := 0.4 + 0.6*(0.5+0.5*math.Sin(2*math.Pi*0.23*t))
		out[i] = clamp32(float32(0.3*env*(0.4*lp+0.6*rng.next())), -0.95, 0.95)
	}
	return out
}

// chirp builds a slow linear sweep: its cepstral sequences drift monotonically,
// so a correlation sees a rising curve rather than an isolated peak. The
// boundary test needs that shape.
func chirp(seconds float64) []float32 {
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

// synthPair builds the fixture's three slices: the target is the programme
// synthDelayMS late relative to the donor, and the degraded donor is the donor
// through the degradation chain.
//
// The delay sits on the target on purpose. correlate's sign convention is
// offset = t_target − t_donor, so a late target is the positive answer — the
// same construction correlate's own TestKnownShift uses — and the fixture's
// +250 ms is the offset the tests and expected.json assert. A donor delayed by
// 250 ms instead would answer −2000 samples.
func synthPair() (target, donor, degraded []float32) {
	donor = programme()
	delay := int(math.Round(synthDelayMS / 1000 * float64(synthRate)))
	target = delaySamples(donor, delay)
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

// degrade runs donor.wav through the chain in Fixture design's order: 4-tap
// moving-average low-pass, tanh(3x) soft clipping, 6-bit quantisation, additive
// noise at degradedSNRDB.
func degrade(in []float32) []float32 {
	// Low-pass: a 4-tap moving average, zero-padded at the head.
	lp := make([]float32, len(in))
	for i := range in {
		var sum float32
		for k := 0; k < degradedTaps; k++ {
			if j := i - k; j >= 0 {
				sum += in[j]
			}
		}
		lp[i] = sum / degradedTaps
	}

	// Soft clipping with drive, so the peaks sit well past the knee.
	for i, v := range lp {
		lp[i] = float32(math.Tanh(3 * degradedDrive * float64(v)))
	}

	// 6-bit quantisation of the clipped full scale.
	levels := float64(int(1) << (degradedBits - 1))
	for i, v := range lp {
		lp[i] = float32(math.Round(float64(v)*levels) / levels)
	}

	// Additive white noise at degradedSNRDB relative to the clipped signal. The
	// generator's values are uniform in -1..1, so scaling by sqrt(3) turns the
	// target RMS into the sample scale.
	var squares float64
	for _, v := range lp {
		squares += float64(v) * float64(v)
	}
	rms := math.Sqrt(squares / float64(len(lp)))
	scale := rms / math.Pow(10, degradedSNRDB/20) * math.Sqrt(3)

	rng := newSynthLCG(synthSeed ^ 0x5eed)
	for i := range lp {
		lp[i] = clamp32(lp[i]+float32(scale*rng.next()), -1, 1)
	}
	return lp
}

// clamp32 bounds v to [lo, hi].
func clamp32(v, lo, hi float32) float32 { return min(max(v, lo), hi) }

// clamp64 bounds v to [lo, hi].
func clamp64(v, lo, hi float64) float64 { return min(max(v, lo), hi) }
