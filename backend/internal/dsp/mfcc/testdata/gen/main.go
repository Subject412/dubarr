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

const (
	// The degradation chain, in the order it applies. Tighten these — not the
	// tests' assertions — if the waveform matcher ever clears the floor above.
	taps  = 4
	drive = 2.5
	bits  = 6
	snrDB = -10.0
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

// programme is the test package's programme(), sample for sample.
func programme() []float32 {
	freqs := [5]float64{1500, 1900, 2400, 3000, 3600}
	envHz := [5]float64{0.37, 0.71, 1.13, 1.71, 2.29}
	envPhase := [5]float64{0.11, 1.37, 2.51, 4.19, 5.53}

	rateF := float64(rate)
	n := int(math.Round(seconds * rateF))
	rng := newLCG(seed)

	clickEvery := int(math.Round(0.5 * rateF))
	clickDecay := math.Exp(-1 / (0.004 * rateF))
	countdown := 0
	clickEnv, clickHz := 0.0, 1200.0

	out := make([]float32, n)
	for i := range out {
		t := float64(i) / rateF
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

// delay shifts samples later by samples of delay, padding the head with zeros.
func delay(in []float32, samples int) []float32 {
	out := make([]float32, len(in))
	for i := range out {
		if j := i - samples; j >= 0 {
			out[i] = in[j]
		}
	}
	return out
}

// degrade is the test package's chain: low-pass, clip, quantise, noise.
func degrade(in []float32) []float32 {
	lp := make([]float32, len(in))
	for i := range in {
		var sum float32
		for k := 0; k < taps; k++ {
			if j := i - k; j >= 0 {
				sum += in[j]
			}
		}
		lp[i] = sum / taps
	}
	for i, v := range lp {
		lp[i] = float32(math.Tanh(3 * drive * float64(v)))
	}
	levels := float64(int(1) << (bits - 1))
	for i, v := range lp {
		lp[i] = float32(math.Round(float64(v)*levels) / levels)
	}
	var squares float64
	for _, v := range lp {
		squares += float64(v) * float64(v)
	}
	rms := math.Sqrt(squares / float64(len(lp)))
	scale := rms / math.Pow(10, snrDB/20) * math.Sqrt(3)
	rng := newLCG(seed ^ 0x5eed)
	for i := range lp {
		lp[i] = clamp32(lp[i]+float32(scale*rng.next()), -1, 1)
	}
	return lp
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

	// The target carries the delay, so the answer is +delayMS by correlate's
	// sign convention (offset = t_target − t_donor); a delayed donor would
	// answer the same magnitude, negated.
	donor := programme()
	shift := int(math.Round(float64(delayMS) / 1000 * float64(rate)))
	target := delay(donor, shift)
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
			"tighten taps/drive/bits/snrDB (in this file and testsynth_test.go), do not relax the test\n",
			wave.Quality, degradedWaveformQualityMax)
		os.Exit(1)
	}
	if float64(damaged.Quality) < degradedMFCCMinConfidence {
		fmt.Fprintf(os.Stderr, "gen: mfcc quality %v < %v on the degraded pair\n",
			damaged.Quality, degradedMFCCMinConfidence)
		os.Exit(1)
	}

	expected := map[string]any{
		"offset_ms":                    delayMS,
		"precision_ms":                 hopMS,
		"mfcc_max_error_ms":            mfccErrMS,
		"degraded_waveform_quality_max": degradedWaveformQualityMax,
		"degraded_mfcc_min_confidence": degradedMFCCMinConfidence,
		"degraded_mfcc_max_error_ms":   degradedMFCCErrMS,
		"sample_hash":                  fmt.Sprintf("0x%08x", pcmHash(payload)),
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
