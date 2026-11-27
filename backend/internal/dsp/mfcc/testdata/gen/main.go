//go:build ignore

// Command gen builds the MFCC matcher's fixture and records its gates.
//
// It is a developer tool, never compiled by `go build ./...`: the
// `//go:build ignore` tag excludes it from every package build, and it lives
// under `testdata/`, which the go tool does not descend into. Run it with an
// explicit file argument, as the capture tool is run:
//
//	cd backend && go run ./internal/dsp/mfcc/testdata/gen/main.go
//
// It writes target.wav, donor.wav and donor-degraded.wav next to this file (all
// three are gitignored — the WAVs are reproducible, the numbers are what is
// committed), measures them through both matchers and rewrites expected.json:
// the tolerance gates below plus the 32-bit FNV-1a hash of the target's samples,
// which TestSyntheticBuilderMatchesCommittedHash uses to keep this copy of the
// synthesis identical to the test package's.
//
// The synthesis here is a deliberate second copy of
// internal/dsp/mfcc/testsynth_test.go: the committed generator must run without
// a test binary, and the test package must run without this tool. Change one and
// you must change the other; the hash test is what says so.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"

	"github.com/go-fft/fft"

	"github.com/Subject412/dubarr/backend/internal/dsp/correlate"
	"github.com/Subject412/dubarr/backend/internal/dsp/mfcc"
)

const (
	rate    = uint32(8000)
	seconds = 120.0
	seed    = uint64(0x5eedbeef)
	delayMS = 250
	hopMS   = 10.0
)

const (
	// The gates written to expected.json.
	mfccErrMS                  = 20.0
	degradedMFCCErrMS          = 30.0
	degradedWaveformQualityMax = 0.10
	degradedMFCCMinConfidence  = 0.10
)

// The fixture's texture: a bank of band-limited noise streams, each under its
// own aperiodic gain contour. See testsynth_test.go — the copy is the contract.
const (
	bandCount      = 8
	bandResonance  = 0.85
	programmeLevel = 0.16
)

var (
	bandCentres = [bandCount]float64{250, 520, 820, 1180, 1600, 2050, 2580, 3250}
	driftHz     = [bandCount]float64{6.1, 9.7, 13.3, 4.9, 17.1, 7.9, 21.7, 11.3}
)

// The codec emulation's knobs, in the order the chain applies them.
const (
	degradedLowPass = 0.6
	degradedDrive   = 2.5

	codecFFT     = 1024
	codecHop     = codecFFT / 2
	codecMagBits = 4
	codecRangeDB = 48.0

	degradedBits = 6
)

// lcg is the fixture's random source: the same generator as the test package's.
type lcg struct{ state uint64 }

func newLCG(s uint64) *lcg { return &lcg{state: s | 1} }

func (g *lcg) next() float64 {
	g.state = g.state*6364136223846793005 + 1442695040888963407
	return float64(g.state>>40)/8388608.0 - 1.0
}

func clamp(v, lo, hi float64) float64 { return min(max(v, lo), hi) }

// clamp32 is the test package's clamp32, so the two builders round identically.
func clamp32(v, lo, hi float32) float32 { return min(max(v, lo), hi) }

// sumSquares is the test package's.
func sumSquares(values []float32) float64 {
	var total float64
	for _, v := range values {
		total += float64(v) * float64(v)
	}
	return total
}

// scaleToProgrammeLevel is the test package's.
func scaleToProgrammeLevel(samples []float32, sumsq float64) []float32 {
	if rms := math.Sqrt(sumsq / float64(len(samples))); rms > 1e-12 {
		scale := float32(programmeLevel / rms)
		for i := range samples {
			samples[i] = clamp32(samples[i]*scale, -0.95, 0.95)
		}
	}
	return samples
}

// bandNoise is the test package's bandNoise, sample for sample.
func bandNoise(secs float64, s uint64) []float32 {
	rateF := float64(rate)
	n := int(math.Round(secs * rateF))
	rng := newLCG(s)

	var cosW, pole [bandCount]float64
	for b, f := range bandCentres {
		cosW[b] = math.Cos(2 * math.Pi * f / rateF)
		pole[b] = math.Exp(-2 * math.Pi * driftHz[b] / rateF)
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
			deviation := math.Sqrt((1 - pole[b]) / (1 + pole[b]))
			unit := clamp(0.5+0.8*walk[b]/deviation, 0, 1)
			v += (0.12 + 0.95*unit) * y
		}
		out[i] = float32(v)
	}
	return scaleToProgrammeLevel(out, sumSquares(out))
}

// programmeAt is the test package's programmeAt, sample for sample.
func programmeAt(secs float64) []float32 { return bandNoise(secs, seed) }

// pairAtLag is the test package's pairAtLag: two slices of one source, lag
// samples apart, cut rather than zero-padded so neither carries a silent head.
func pairAtLag(lag int, secs float64) (target, donor []float32) {
	source := programmeAt(secs + float64(lag)/float64(rate))
	n := int(math.Round(secs * float64(rate)))
	return source[:n], source[lag : lag+n]
}

// degrade is the test package's chain: band-limit, clip, STFT round trip, PCM.
func degrade(in []float32) []float32 {
	lp := make([]float32, len(in))
	var state float64
	for i, v := range in {
		state += degradedLowPass * (float64(v) - state)
		lp[i] = float32(state)
	}
	for i, v := range lp {
		lp[i] = float32(math.Tanh(3 * degradedDrive * float64(v)))
	}

	codec := codecRoundTrip(lp)

	levels := float64(int(1) << (degradedBits - 1))
	for i, v := range codec {
		codec[i] = clamp32(float32(math.Round(float64(v)*levels)/levels), -1, 1)
	}
	return codec
}

// codecRoundTrip is the test package's lossy-codec emulation.
func codecRoundTrip(in []float32) []float32 {
	nfft, hop := codecFFT, codecHop
	window := hannWindow(nfft)
	opts := fft.Options{NoFMA: true}
	buf := make([]complex64, nfft)

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

	step := codecRangeDB * math.Ln10 / 20 / float64(int(1)<<codecMagBits)
	floorLevel := -float64(int(1)<<codecMagBits - 1)
	quantise := func(magnitude float64) float64 {
		level := math.Round(math.Log(magnitude/reference) / step)
		return reference * math.Exp(max(level, floorLevel)*step)
	}

	out := make([]float64, len(in)+nfft)
	norm := make([]float64, len(in)+nfft)
	rng := newLCG(seed ^ 0x5eed)

	for start := 0; start < len(in); start += hop {
		windowedFrame(buf, in, start, window)
		spectrum := fft.FFT32With(buf, opts)

		dc := float64(real(spectrum[0]))
		spectrum[0] = complex(float32(math.Copysign(quantise(math.Abs(dc)), dc)), 0)
		for k := 1; k < nfft/2; k++ {
			magnitude := quantise(math.Hypot(float64(real(spectrum[k])), float64(imag(spectrum[k]))))
			phase := math.Pi * rng.next()
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

// windowedFrame is the test package's, sample for sample.
func windowedFrame(buf []complex64, samples []float32, start int, window []float32) {
	for i := range buf {
		if j := start + i; j < len(samples) {
			buf[i] = complex(samples[j]*window[i], 0)
		} else {
			buf[i] = 0
		}
	}
}

// hannWindow is the test package's window.
func hannWindow(n int) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1)))
	}
	return out
}

// writeWav writes mono PCM s16 and returns the payload it wrote, which is what
// the hash covers.
func writeWav(path string, samples []float32) ([]byte, error) {
	payload := make([]byte, 2*len(samples))
	for i, s := range samples {
		v := int16(math.Round(clamp(float64(s)*32767, -32768, 32767)))
		binary.LittleEndian.PutUint16(payload[2*i:], uint16(v))
	}

	header := make([]byte, 44)
	copy(header[0:], "RIFF")
	binary.LittleEndian.PutUint32(header[4:], uint32(36+len(payload)))
	copy(header[8:], "WAVE")
	copy(header[12:], "fmt ")
	binary.LittleEndian.PutUint32(header[16:], 16)
	binary.LittleEndian.PutUint16(header[20:], 1) // PCM
	binary.LittleEndian.PutUint16(header[22:], 1) // mono
	binary.LittleEndian.PutUint32(header[24:], rate)
	binary.LittleEndian.PutUint32(header[28:], rate*2)
	binary.LittleEndian.PutUint16(header[32:], 2)
	binary.LittleEndian.PutUint16(header[34:], 16)
	copy(header[36:], "data")
	binary.LittleEndian.PutUint32(header[40:], uint32(len(payload)))

	data := append(header, payload...)
	return payload, os.WriteFile(path, data, 0o644)
}

// pcmHash is FNV-1a 32 over the little-endian int16 payload.
func pcmHash(payload []byte) uint32 {
	h := fnv.New32a()
	h.Write(payload)
	return h.Sum32()
}

func main() {
	dir := "internal/dsp/mfcc/testdata"

	// The target is the earlier slice, so the answer is +delayMS by correlate's
	// sign convention (offset = t_target − t_donor); the two slices swapped
	// would answer the same magnitude, negated.
	delay := int(math.Round(float64(delayMS) / 1000 * float64(rate)))
	target, donor := pairAtLag(delay, seconds)
	degraded := degrade(donor)

	payload, err := writeWav(filepath.Join(dir, "target.wav"), target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen: target: %v\n", err)
		os.Exit(1)
	}
	if _, err := writeWav(filepath.Join(dir, "donor.wav"), donor); err != nil {
		fmt.Fprintf(os.Stderr, "gen: donor: %v\n", err)
		os.Exit(1)
	}
	if _, err := writeWav(filepath.Join(dir, "donor-degraded.wav"), degraded); err != nil {
		fmt.Fprintf(os.Stderr, "gen: donor-degraded: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	clean, err := mfcc.Analyze(ctx, target, donor, rate, 1.0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen: clean mfcc: %v\n", err)
		os.Exit(1)
	}
	damaged, err := mfcc.Analyze(ctx, target, degraded, rate, 1.0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen: degraded mfcc: %v\n", err)
		os.Exit(1)
	}
	wave, err := correlate.Analyze(ctx, target, degraded, rate, 1.0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen: degraded waveform: %v\n", err)
		os.Exit(1)
	}

	offsetMS := float64(clean.OffsetSamples) * 1000 / float64(rate)
	damagedMS := float64(damaged.OffsetSamples) * 1000 / float64(rate)
	waveMS := float64(wave.OffsetSamples) * 1000 / float64(rate)

	fmt.Printf("clean     : offset %.3f ms, quality %v, match_z %v, precision %v ms\n",
		offsetMS, clean.Quality, clean.MatchZ, clean.PrecisionMS)
	fmt.Printf("degraded  : mfcc offset %.3f ms, quality %v, match_z %v\n",
		damagedMS, damaged.Quality, damaged.MatchZ)
	fmt.Printf("degraded  : waveform offset %.3f ms, quality %v, match_z %v\n",
		waveMS, wave.Quality, wave.MatchZ)

	if math.Abs(offsetMS-delayMS) > mfccErrMS {
		fmt.Fprintf(os.Stderr, "gen: clean offset %.3f ms is outside %v ± %v ms\n",
			offsetMS, delayMS, mfccErrMS)
		os.Exit(1)
	}
	if math.Abs(damagedMS-delayMS) > degradedMFCCErrMS {
		fmt.Fprintf(os.Stderr, "gen: degraded mfcc offset %.3f ms is outside %v ± %v ms\n",
			damagedMS, delayMS, degradedMFCCErrMS)
		os.Exit(1)
	}
	if float64(wave.Quality) >= degradedWaveformQualityMax {
		fmt.Fprintf(os.Stderr, "gen: the degradation chain is too gentle: waveform quality %v >= %v; "+
			"tighten the codec round trip (in this file and testsynth_test.go), do not relax the test\n",
			wave.Quality, degradedWaveformQualityMax)
		os.Exit(1)
	}
	if float64(damaged.Quality) < degradedMFCCMinConfidence {
		fmt.Fprintf(os.Stderr, "gen: mfcc quality %v < %v on the degraded pair\n",
			damaged.Quality, degradedMFCCMinConfidence)
		os.Exit(1)
	}

	expected := map[string]any{
		"offset_ms":                     delayMS,
		"precision_ms":                  hopMS,
		"mfcc_max_error_ms":             mfccErrMS,
		"degraded_waveform_quality_max": degradedWaveformQualityMax,
		"degraded_mfcc_min_confidence":  degradedMFCCMinConfidence,
		"degraded_mfcc_max_error_ms":    degradedMFCCErrMS,
		"sample_hash":                   fmt.Sprintf("0x%08x", pcmHash(payload)),
	}
	encoded, err := json.MarshalIndent(expected, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen: encoding expected.json: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(dir, "expected.json"), append(encoded, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "gen: writing expected.json: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (sample_hash 0x%08x)\n", filepath.Join(dir, "expected.json"), pcmHash(payload))
}
