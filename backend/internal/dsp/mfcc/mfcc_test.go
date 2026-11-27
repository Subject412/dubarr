package mfcc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/dsp/correlate"
)

// The verdict thresholds below mirror analyze.DefaultThresholds. They are local
// constants because analyze imports sync, which imports correlate: a test in
// this package cannot import analyze without an import cycle. Slice 01 and
// correlate's golden test mirror the same table the same way; if
// analyze.DefaultThresholds changes, these three move with it.
//
// There is deliberately no MatchZGood here: this matcher is not z-gated, so the
// "almost certainly right" band judges nothing of its own (see the ruling in
// docs/plans/0.5.0/04-mfcc-matcher.md and analyze.Technique.ZGate).
const (
	thresholdLowQuality  = 0.10 // analyze.DefaultThresholds.LowQuality (LowQualityThreshold)
	thresholdMatchZMaybe = 5.0  // analyze.DefaultThresholds.MatchZMaybe
)

// expectedFile is testdata/expected.json: the fixture's gates, written by
// testdata/gen and read here so a regeneration changes the assertions in one
// place.
type expectedFile struct {
	OffsetMS                   float64 `json:"offset_ms"`
	PrecisionMS                float64 `json:"precision_ms"`
	MFCCMaxErrorMS             float64 `json:"mfcc_max_error_ms"`
	DegradedWaveformQualityMax float64 `json:"degraded_waveform_quality_max"`
	DegradedMFCCMinConfidence  float64 `json:"degraded_mfcc_min_confidence"`
	DegradedMFCCMaxErrorMS     float64 `json:"degraded_mfcc_max_error_ms"`
	SampleHash                 string  `json:"sample_hash"`
}

// loadExpected reads testdata/expected.json and fails the test if it is
// missing or malformed: the fixture's gates are part of the contract.
func loadExpected(t *testing.T) expectedFile {
	t.Helper()
	data, err := os.ReadFile("testdata/expected.json")
	if err != nil {
		t.Fatalf("reading testdata/expected.json: %v", err)
	}
	var exp expectedFile
	if err := json.Unmarshal(data, &exp); err != nil {
		t.Fatalf("parsing testdata/expected.json: %v", err)
	}
	return exp
}

// mustAnalyze runs the package entry point and fails the test on error.
func mustAnalyze(t *testing.T, target, donor []float32, sampleRate uint32, windowSec float64) correlate.Analysis {
	t.Helper()
	got, err := Analyze(context.Background(), target, donor, sampleRate, windowSec)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	return got
}

// offsetMillis is the reported offset in milliseconds, for messages.
func offsetMillis(samples int64, sampleRate uint32) float64 {
	return float64(samples) * 1000 / float64(sampleRate)
}

// TestFramesCoverTheSliceAtTenMillisecondHops pins the frame geometry: a 10 s
// slice at 8 kHz yields (80000−200)/80 + 1 = 998 frames of 200 samples, each
// carrying Cepstra coefficients.
func TestFramesCoverTheSliceAtTenMillisecondHops(t *testing.T) {
	const rate = uint32(8000)
	if got := frameSamples(rate); got != 200 {
		t.Errorf("frame length = %d samples, want 200 (25 ms at 8 kHz)", got)
	}
	if got := hopSamples(rate); got != 80 {
		t.Errorf("hop = %d samples, want 80 (10 ms at 8 kHz)", got)
	}

	coeffs, energy := frames(programme()[:10*int(rate)], rate)
	if want := 998; len(coeffs) != want {
		t.Fatalf("%d frames, want %d ((80000-200)/80 + 1)", len(coeffs), want)
	}
	if len(energy) != len(coeffs) {
		t.Fatalf("%d energies for %d frames", len(energy), len(coeffs))
	}
	for i, row := range coeffs {
		if len(row) != Cepstra {
			t.Fatalf("frame %d carries %d coefficients, want %d", i, len(row), Cepstra)
		}
	}
}

// TestMFCCRecoversAKnownShift recovers a 250 ms delay — a whole number of hops
// (25) — exactly, and reports the hop as the precision.
func TestMFCCRecoversAKnownShift(t *testing.T) {
	const rate = uint32(8000)
	target, donor, _ := synthPair()

	got := mustAnalyze(t, target, donor, rate, 1.0)
	exp := loadExpected(t)
	if got.OffsetSamples != 2000 {
		t.Fatalf("offset = %d samples (%v ms), want 2000 (250 ms)",
			got.OffsetSamples, offsetMillis(got.OffsetSamples, rate))
	}
	// No sub-hop interpolation: the answer is the hop itself, not a guess.
	if got.OffsetSubSamples != float64(got.OffsetSamples) {
		t.Errorf("OffsetSubSamples = %v, want %v (no sub-hop parabola)",
			got.OffsetSubSamples, float64(got.OffsetSamples))
	}
	if got.PrecisionMS != exp.PrecisionMS {
		t.Errorf("PrecisionMS = %v, want %v (the hop, not one sample)", got.PrecisionMS, exp.PrecisionMS)
	}
}

// TestMFCCQuantisesToTheHop proves the answer is a whole number of hops and is
// never the raw sample delay: a 255 ms delay reports one of the two hops it
// straddles, never 2040 samples.
//
// 255 ms is 2040 samples, exactly half a hop past 25 hops (2000) and half a hop
// before 26 (2080), so which of the two wins is a tie broken by the data. The
// test therefore pins what the contract actually promises — a hop multiple
// within one hop of the delay — instead of the tie's coin flip.
func TestMFCCQuantisesToTheHop(t *testing.T) {
	const rate = uint32(8000)
	hop := int64(hopSamples(rate))
	donor := programme()
	target := delaySamples(donor, 2040) // 255 ms on the target, so the raw answer is +2040

	got := mustAnalyze(t, target, donor, rate, 1.0)
	if got.OffsetSamples == 2040 {
		t.Fatalf("offset = 2040 samples: a sample-accurate answer the matcher cannot have")
	}
	if got.OffsetSamples%hop != 0 {
		t.Fatalf("offset = %d samples is not a multiple of the %d-sample hop", got.OffsetSamples, hop)
	}
	if want := int64(2000); got.OffsetSamples != want && got.OffsetSamples != want+hop {
		t.Fatalf("offset = %d samples (%v ms), want %d or %d (the nearest hops)",
			got.OffsetSamples, offsetMillis(got.OffsetSamples, rate), want, want+hop)
	}
	if got.PrecisionMS != 10 {
		t.Errorf("PrecisionMS = %v, want 10", got.PrecisionMS)
	}
}

// TestMFCCSurvivesClippingAndLowPass is #83's acceptance in one test: on the
// degraded fixture — a low-bitrate re-encode emulated as the codec's band-limit,
// a loudness push past the encoder's knee, an STFT round trip that keeps the band
// energies and regenerates the fine structure as noise, and a 6-bit output — the
// mel-cepstral matcher still clears LowQuality with an offset within 30 ms, while
// the waveform matcher on the same input does not.
//
// The chain is tuned in the fixture builders — this test's synthesis and
// testdata/gen carry the same copy — and the assertion is never relaxed; if the
// chain does not clear it, the chain is tightened and expected.json's git history
// records that. Additive noise is deliberately not part of it: it costs both
// matchers the same SNR-bounded amount, so it can never isolate the cepstral
// advantage, which lives on envelope-preserving processing.
func TestMFCCSurvivesClippingAndLowPass(t *testing.T) {
	const rate = uint32(8000)
	exp := loadExpected(t)

	target, _, degraded := synthPair()

	got := mustAnalyze(t, target, degraded, rate, 1.0)
	gotMS := offsetMillis(got.OffsetSamples, rate)
	if exp.DegradedWaveformQualityMax != thresholdLowQuality {
		t.Fatalf("expected.json's degraded_waveform_quality_max = %v, want "+
			"analyze.DefaultThresholds.LowQuality = %v",
			exp.DegradedWaveformQualityMax, thresholdLowQuality)
	}
	if float64(got.Quality) < exp.DegradedMFCCMinConfidence {
		t.Errorf("mfcc quality = %v, want >= %v on the degraded pair",
			got.Quality, exp.DegradedMFCCMinConfidence)
	}
	if math.Abs(gotMS-exp.OffsetMS) > exp.DegradedMFCCMaxErrorMS {
		t.Errorf("mfcc offset = %v ms, want %v ± %v ms",
			gotMS, exp.OffsetMS, exp.DegradedMFCCMaxErrorMS)
	}

	wave, err := correlate.Analyze(context.Background(), target, degraded, rate, 1.0)
	if err != nil {
		t.Fatalf("waveform correlate on the degraded pair: %v", err)
	}
	if float64(wave.Quality) >= exp.DegradedWaveformQualityMax {
		t.Errorf("waveform quality = %v on the degraded pair, want < %v (mfcc %v at %v ms); "+
			"the fixture no longer isolates what mfcc survives — tighten the degradation chain "+
			"in the builders and regenerate expected.json, do not relax this assertion",
			wave.Quality, exp.DegradedWaveformQualityMax, got.Quality, gotMS)
	}
	t.Logf("degraded: mfcc quality %v at %v ms, waveform quality %v at %v ms",
		got.Quality, gotMS, wave.Quality, offsetMillis(wave.OffsetSamples, rate))
}

// TestMFCCRejectsSilence mirrors correlate's SilentSlice, naming the slice.
func TestMFCCRejectsSilence(t *testing.T) {
	const rate = uint32(8000)
	_, err := Analyze(context.Background(), make([]float32, 4000), make([]float32, 4000), rate, 1.0)
	var silent *correlate.SilentSlice
	if !errors.As(err, &silent) {
		t.Fatalf("error = %v, want *correlate.SilentSlice", err)
	}
	if silent.Which != "target" {
		t.Errorf("Which = %q, want %q (the target is validated first)", silent.Which, "target")
	}
}

// TestMFCCRejectsAShortSlice mirrors correlate's SliceTooShort, including the
// minimum it reports.
func TestMFCCRejectsAShortSlice(t *testing.T) {
	const rate = uint32(8000)
	short := make([]float32, 63)
	_, err := Analyze(context.Background(), short, short, rate, 1.0)
	var tooShort *correlate.SliceTooShort
	if !errors.As(err, &tooShort) {
		t.Fatalf("error = %v, want *correlate.SliceTooShort", err)
	}
	if tooShort.Min != correlate.MinSliceSamples {
		t.Errorf("Min = %d, want correlate.MinSliceSamples = %d", tooShort.Min, correlate.MinSliceSamples)
	}
	if tooShort.Samples != 63 {
		t.Errorf("Samples = %d, want 63", tooShort.Samples)
	}
}

// TestMFCCRejectsABadWindow asserts the argument errors are the waveform
// matcher's, word for word: the runner maps them by kind, and a report written
// with one matcher must read the same as one written with the other.
func TestMFCCRejectsABadWindow(t *testing.T) {
	samples := programme()[:16000]
	cases := []struct {
		name      string
		rate      uint32
		windowSec float64
		target    []float32
		donor     []float32
	}{
		{name: "zero window", rate: 8000, windowSec: 0, target: samples, donor: samples},
		{name: "negative window", rate: 8000, windowSec: -1, target: samples, donor: samples},
		{name: "NaN window", rate: 8000, windowSec: math.NaN(), target: samples, donor: samples},
		{name: "infinite window", rate: 8000, windowSec: math.Inf(1), target: samples, donor: samples},
		{name: "zero rate", rate: 0, windowSec: 1, target: samples, donor: samples},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Analyze(context.Background(), tc.target, tc.donor, tc.rate, tc.windowSec)
			var invalid *correlate.InvalidJob
			if !errors.As(err, &invalid) {
				t.Fatalf("error = %v, want *correlate.InvalidJob", err)
			}
			_, wantErr := correlate.Analyze(context.Background(), tc.target, tc.donor, tc.rate, tc.windowSec)
			if wantErr == nil {
				t.Fatalf("correlate accepted the same arguments; the case is wrong")
			}
			if got, want := err.Error(), wantErr.Error(); got != want {
				t.Errorf("error = %q, want correlate's wording %q", got, want)
			}
		})
	}
}

// TestMFCCPinsABoundaryPeakToTheSearchEdge: a real shift larger than windowSec
// lands the peak on the search edge, and the exhaustion is not a match — the
// z stays under MatchZMaybe so the strategy pipeline cannot read the window's
// boundary as evidence.
//
// The pair is [boundaryPair]: two slices cut out of one swept source, with no
// silent head, whose true lag sits two hops past the 1 s bound. The sweep's
// cepstral sequences drift monotonically, so the correlation rises towards the
// edge nearest the true delay instead of peaking inside the window, which makes
// the edge assertion a property of the fixture rather than of the noise.
func TestMFCCPinsABoundaryPeakToTheSearchEdge(t *testing.T) {
	const rate = uint32(8000)
	target, donor := boundaryPair()

	got := mustAnalyze(t, target, donor, rate, 1.0)
	hop := int64(hopSamples(rate))
	maxLagSamples := int64(math.Round(1.0 * float64(rate))) // windowSec * rate
	wantEdge := maxLagSamples / hop * hop
	if got.OffsetSamples != wantEdge {
		t.Errorf("offset = %d samples (%v ms), want the search edge %d samples (%v ms)",
			got.OffsetSamples, offsetMillis(got.OffsetSamples, rate), wantEdge, offsetMillis(wantEdge, rate))
	}
	if z := float64(got.MatchZ); z >= thresholdMatchZMaybe {
		t.Errorf("match_z = %v with the peak pinned to the search edge, want < %v: exhaustion is not a match",
			z, thresholdMatchZMaybe)
	}
}

// TestMFCCIsCancellable: a cancelled context returns an error wrapping ctx.Err()
// and never a partial Analysis.
func TestMFCCIsCancellable(t *testing.T) {
	samples := programme()[:16000]
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := Analyze(ctx, samples, samples, 8000, 1.0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want an error wrapping context.Canceled", err)
	}
	if got != (correlate.Analysis{}) {
		t.Errorf("Analysis = %+v, want the zero value (never a partial answer)", got)
	}
}

// TestSyntheticBuilderMatchesCommittedHash keeps the two copies of the
// synthesis honest: the builder in this package and testdata/gen must produce
// the same target samples, or the committed numbers describe audio nobody
// renders.
//
// The generator records the 32-bit FNV-1a hash of the target's little-endian
// int16 payload. Until it has been run once, expected.json carries an empty
// hash and this test skips rather than guessing a value.
func TestSyntheticBuilderMatchesCommittedHash(t *testing.T) {
	exp := loadExpected(t)
	if exp.SampleHash == "" {
		t.Skip("expected.json records no sample_hash yet; run " +
			"`cd backend && go run ./internal/dsp/mfcc/testdata/gen/main.go` and commit the number it prints")
	}

	want, err := parseHexHash(exp.SampleHash)
	if err != nil {
		t.Fatalf("expected.json sample_hash %q: %v", exp.SampleHash, err)
	}
	target, _, _ := synthPair()
	got := pcmHash(target)
	if got != want {
		t.Fatalf("target sample hash = 0x%08x, expected.json records 0x%08x; "+
			"the builders in this package and testdata/gen have diverged", got, want)
	}
}

// parseHexHash reads expected.json's "0x…" hash form.
func parseHexHash(s string) (uint32, error) {
	trimmed := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "0x")
	raw, err := hex.DecodeString(trimmed)
	if err != nil {
		return 0, err
	}
	if len(raw) != 4 {
		return 0, errors.New("want four bytes")
	}
	return uint32(raw[0])<<24 | uint32(raw[1])<<16 | uint32(raw[2])<<8 | uint32(raw[3]), nil
}

// pcmHash is the FNV-1a 32 hash of the samples as the tester writes them:
// little-endian int16, the payload of target.wav. testdata/gen computes the
// same bytes the same way.
func pcmHash(samples []float32) uint32 {
	h := fnv.New32a()
	var b [2]byte
	for _, s := range samples {
		v := int16(math.Round(clamp64(float64(s)*32767, -32768, 32767)))
		b[0] = byte(uint16(v))
		b[1] = byte(uint16(v) >> 8)
		h.Write(b[:])
	}
	return h.Sum32()
}
