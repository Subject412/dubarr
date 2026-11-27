package analyze

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/reflang"
	"github.com/Subject412/dubarr/backend/internal/sync"
)

// Wall-clock budgets for the integration tests that drive the real engine.
const (
	// testJobTimeout is the explicit per-checkpoint engine budget handed to
	// both the runner (sync.WithTimeout) and [Config.JobTimeout].
	//
	// It is deliberately much larger than production's derived budget
	// ([DefaultJobTimeout] floors the 190 s slice at 120 s): that budget is a
	// production policy covered by TestJobTimeoutPolicy, and reusing it here
	// would make a loaded machine indistinguishable from a broken
	// correlation — the job would starve and the cap would fire instead of
	// the offset assertion.
	testJobTimeout = 10 * time.Minute
	// testAnalysisBudget bounds one whole Analyze call. Six checkpoints may
	// each use testJobTimeout, so this is only a hang guard, never a
	// performance assertion.
	testAnalysisBudget = 30 * time.Minute
)

// testJobConfig is DefaultConfig with the integration-test budget, so a real
// engine run is never capped by production's wall-clock policy.
func testJobConfig() Config {
	cfg := DefaultConfig()
	cfg.JobTimeout = testJobTimeout
	return cfg
}

// requireEngine returns the in-process engine. It needs no external toolchain:
// the DSP code is compiled into the test binary.
func requireEngine(t *testing.T) Engine {
	t.Helper()
	// The runner's own timeout must match [Config.JobTimeout], otherwise it
	// caps the job below the budget the analysis asks for.
	return sync.New(sync.WithTimeout(testJobTimeout))
}

// echoResult is a successful engine answer that echoes the slice the job asked
// for, as the real engine does. The analyser reads the echo to detect a short
// slice, so a stub that leaves Analyzed.Len at zero is claiming it analysed
// nothing.
func echoResult(job sync.Job, offset, quality float64) sync.Result {
	return sync.Result{
		OffsetMS:           offset,
		CorrelationQuality: quality,
		SampleRate:         8000,
		Analyzed:           sync.Analyzed{Start: job.SliceStartSec, Len: job.SliceLenSec},
	}
}

// scriptedEngine is the aggregation seam: it returns canned offsets per job so
// the classification logic is unit-tested without the Rust binary.
//
// The answers are consumed in call order, which is why the routing tests pin
// one worker: with a single worker the call order is the checkpoint order, so
// a scripted answer per call is a scripted answer per checkpoint.
//
// matchZ and precisions are the 0.5.0 diagnostics; an absent script leaves them
// at 0, which is the documented "this engine does not report one" sentinel and
// the shape every pre-change test was written against.
type scriptedEngine struct {
	offsets    []float64
	qualities  []float64
	matchZ     []float64
	precisions []float64
	calls      []sync.Job
	err        error
}

func (e *scriptedEngine) Run(_ context.Context, job sync.Job) (sync.Result, error) {
	e.calls = append(e.calls, job)
	if e.err != nil {
		return sync.Result{}, e.err
	}
	i := len(e.calls) - 1
	if i >= len(e.offsets) {
		return sync.Result{}, errors.New("scripted engine: unexpected extra job")
	}
	q := 0.9
	if i < len(e.qualities) {
		q = e.qualities[i]
	}
	result := echoResult(job, e.offsets[i], q)
	if i < len(e.matchZ) {
		result.MatchZ = e.matchZ[i]
	}
	if i < len(e.precisions) {
		result.PrecisionMS = e.precisions[i]
	}
	return result, nil
}

func sameLangRefs() ([]probe.AudioTrack, []probe.AudioTrack, reflang.Languages, string) {
	target := []probe.AudioTrack{
		{Index: 1, Language: "pol", LanguageISO1: "pl", LanguageISO2B: "pol", Channels: 2},
		{Index: 2, Language: "eng", LanguageISO1: "en", LanguageISO2B: "eng", Channels: 2},
	}
	vault := []probe.AudioTrack{
		{Index: 0, Language: "eng", LanguageISO1: "en", LanguageISO2B: "eng", Channels: 2},
	}
	return target, vault, reflang.Languages{Targets: []string{"pl"}}, "eng"
}

func lowpassRefs() ([]probe.AudioTrack, []probe.AudioTrack, reflang.Languages, string) {
	target := []probe.AudioTrack{
		{Index: 1, Language: "pol", LanguageISO1: "pl", LanguageISO2B: "pol", Channels: 2},
	}
	vault := []probe.AudioTrack{
		{Index: 0, Language: "pol", LanguageISO1: "pl", LanguageISO2B: "pol", Channels: 2},
	}
	return target, vault, reflang.Languages{Targets: []string{"pl"}}, ""
}

func request(path string) Request {
	t, v, p, orig := sameLangRefs()
	return Request{
		TargetPath:       filepath.Join(path, "target.wav"),
		DonorPath:        filepath.Join(path, "donor.wav"),
		DurationSec:      600,
		TargetTracks:     t,
		VaultTracks:      v,
		Policy:           p,
		OriginalLanguage: orig,
	}
}

func TestAnalyzeStable(t *testing.T) {
	eng := &scriptedEngine{offsets: []float64{1200, 1210, 1195, 1205, 1198, 1202}, qualities: []float64{0.91, 0.92, 0.9, 0.93, 0.94, 0.95}}
	cfg := DefaultConfig()
	rep, err := Analyze(context.Background(), eng, cfg, request(t.TempDir()))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.Verdict != VerdictOK {
		t.Fatalf("verdict = %q, want %q", rep.Verdict, VerdictOK)
	}
	if rep.Classification != ClassificationStable {
		t.Fatalf("classification = %q, want %q", rep.Classification, ClassificationStable)
	}
	if rep.SyncMethod != reflang.SameLangReference {
		t.Fatalf("sync_method = %q, want %q", rep.SyncMethod, reflang.SameLangReference)
	}
	if got, want := rep.MaxDriftMS, 15.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("max_drift_ms = %v, want %v", got, want)
	}
	if len(rep.OffsetsMS) != 6 || len(rep.Qualities) != 6 || len(rep.Checkpoints) != 6 {
		t.Fatalf("report lengths = %d/%d/%d, want 6", len(rep.OffsetsMS), len(rep.Qualities), len(rep.Checkpoints))
	}
	// slice_start = pct*duration - window/2, clamped into the file and then
	// snapped to a whole 8 kHz sample. The analysed slice is widened to
	// max_offset_sec + margin (190 s). Both values below land on an exact
	// sample, so the snapped plan still prints 0 and 235: the plan is
	// sample-exact, not merely rounded twice.
	const sliceLen = 190.0
	wantFirst := 0.0
	if got := rep.Checkpoints[0].SliceStartSec; math.Abs(got-wantFirst) > 1e-9 {
		t.Fatalf("first slice_start = %v, want %v", got, wantFirst)
	}
	want55 := 235.0 // 0.55*600 - sliceLen/2, exactly 1 880 000 samples
	if got := rep.Checkpoints[3].SliceStartSec; math.Abs(got-want55) > 1e-9 {
		t.Fatalf("55%% slice_start = %v, want %v", got, want55)
	}
	if len(eng.calls) != 6 {
		t.Fatalf("engine calls = %d, want 6", len(eng.calls))
	}
	if got, want := eng.calls[0].SliceLenSec, sliceLen; got != want {
		t.Fatalf("slice_len = %v, want %v", got, want)
	}
	// The engine search window must cover the full max_offset_sec bound, not
	// just the 150 s checkpoint window.
	if eng.calls[0].WindowSec == nil {
		t.Fatal("window_sec = nil, want the analysed slice length")
	}
	if got, want := *eng.calls[0].WindowSec, sliceLen; got != want {
		t.Fatalf("window_sec = %v, want %v", got, want)
	}
	if got, want := *eng.calls[0].WindowSec, float64(DefaultMaxOffsetSec); got < want {
		t.Fatalf("window_sec = %v, want >= max_offset_sec %v", got, want)
	}
}

// The former TestSliceWindow lives on as TestPlanWindows in plan_test.go: the
// window geometry is now [PlanWindows] data, not a helper inside Analyze.

func TestAnalyzeDrifting(t *testing.T) {
	eng := &scriptedEngine{offsets: []float64{500, 500, 500, 2000, 2000, 2000}}
	rep, err := Analyze(context.Background(), eng, DefaultConfig(), request(t.TempDir()))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.Verdict != VerdictDrifting || rep.Classification != ClassificationDrifting {
		t.Fatalf("verdict/classification = %q/%q, want drifting/drifting", rep.Verdict, rep.Classification)
	}
	if got, want := rep.MaxDriftMS, 1500.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("max_drift_ms = %v, want %v", got, want)
	}
}

func TestAnalyzeOutOfRange(t *testing.T) {
	eng := &scriptedEngine{offsets: []float64{200000, 200100, 199900, 200050, 200000, 200010}}
	cfg := DefaultConfig()
	cfg.MaxOffsetSec = 180
	rep, err := Analyze(context.Background(), eng, cfg, request(t.TempDir()))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.Verdict != VerdictOutOfRange || rep.Classification != ClassificationOutOfRange {
		t.Fatalf("verdict/classification = %q/%q, want out_of_range", rep.Verdict, rep.Classification)
	}
}

// TestAnalyzeVerdictPrecedence drives the aggregation seam through the
// precedence matrix out_of_range > unreliable > drifting > ok.
func TestAnalyzeVerdictPrecedence(t *testing.T) {
	const hi = 0.9
	stable := []float64{1200, 1210, 1195, 1205, 1198, 1202}
	drifting := []float64{500, 500, 500, 2000, 2000, 2000}
	beyond := []float64{200000, 200100, 199900, 200050, 200000, 200010}

	cases := []struct {
		name           string
		offsets        []float64
		qualities      []float64
		wantVerdict    string
		wantClass      string
		wantMaxDriftMS float64
	}{
		{
			name:        "ok",
			offsets:     stable,
			qualities:   []float64{hi, hi, hi, hi, hi, hi},
			wantVerdict: VerdictOK, wantClass: ClassificationStable,
			wantMaxDriftMS: 15,
		},
		{
			name:        "drifting",
			offsets:     drifting,
			qualities:   []float64{hi, hi, hi, hi, hi, hi},
			wantVerdict: VerdictDrifting, wantClass: ClassificationDrifting,
			wantMaxDriftMS: 1500,
		},
		{
			name:        "single weak checkpoint is unreliable",
			offsets:     stable,
			qualities:   []float64{hi, hi, 0.05, hi, hi, hi},
			wantVerdict: VerdictUnreliable, wantClass: ClassificationUnreliable,
			wantMaxDriftMS: 15,
		},
		{
			name:        "unreliable outranks drifting",
			offsets:     drifting,
			qualities:   []float64{hi, hi, 0.02, hi, hi, hi},
			wantVerdict: VerdictUnreliable, wantClass: ClassificationUnreliable,
			wantMaxDriftMS: 1500,
		},
		{
			name:        "out_of_range outranks unreliable",
			offsets:     beyond,
			qualities:   []float64{0.05, 0.05, 0.05, 0.05, 0.05, 0.05},
			wantVerdict: VerdictOutOfRange, wantClass: ClassificationOutOfRange,
			wantMaxDriftMS: 200,
		},
		{
			name:        "quality at threshold is trusted",
			offsets:     stable,
			qualities:   []float64{hi, hi, LowQualityThreshold, hi, hi, hi},
			wantVerdict: VerdictOK, wantClass: ClassificationStable,
			wantMaxDriftMS: 15,
		},
		{
			name:        "quality just below threshold is unreliable",
			offsets:     stable,
			qualities:   []float64{hi, hi, math.Nextafter(LowQualityThreshold, 0), hi, hi, hi},
			wantVerdict: VerdictUnreliable, wantClass: ClassificationUnreliable,
			wantMaxDriftMS: 15,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := &scriptedEngine{offsets: tc.offsets, qualities: tc.qualities}
			rep, err := Analyze(context.Background(), eng, DefaultConfig(), request(t.TempDir()))
			if err != nil {
				t.Fatalf("Analyze: %v", err)
			}
			if rep.Verdict != tc.wantVerdict || rep.Classification != tc.wantClass {
				t.Fatalf("verdict/classification = %q/%q, want %q/%q",
					rep.Verdict, rep.Classification, tc.wantVerdict, tc.wantClass)
			}
			if math.Abs(rep.MaxDriftMS-tc.wantMaxDriftMS) > 1e-9 {
				t.Fatalf("max_drift_ms = %v, want %v", rep.MaxDriftMS, tc.wantMaxDriftMS)
			}
			if len(rep.Qualities) != len(tc.qualities) {
				t.Fatalf("qualities length = %d, want %d", len(rep.Qualities), len(tc.qualities))
			}
		})
	}
}

func TestAnalyzeLowpass(t *testing.T) {
	eng := &scriptedEngine{offsets: []float64{100, 100, 100, 100, 100, 100}}
	req := request(t.TempDir())
	req.TargetTracks, req.VaultTracks, req.Policy, req.OriginalLanguage = lowpassRefs()
	rep, err := Analyze(context.Background(), eng, DefaultConfig(), req)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.SyncMethod != reflang.LowpassFallback {
		t.Fatalf("sync_method = %q, want %q", rep.SyncMethod, reflang.LowpassFallback)
	}
}

func TestAnalyzeNoReference(t *testing.T) {
	eng := &scriptedEngine{offsets: []float64{0}}
	req := request(t.TempDir())
	req.TargetTracks = nil
	if _, err := Analyze(context.Background(), eng, DefaultConfig(), req); err == nil {
		t.Fatal("Analyze with no reference pairs: want error")
	}
	if len(eng.calls) != 0 {
		t.Fatalf("engine ran %d jobs before rejecting, want 0", len(eng.calls))
	}
}

func TestAnalyzeEngineError(t *testing.T) {
	eng := &scriptedEngine{err: sync.ErrAnalysis}
	_, err := Analyze(context.Background(), eng, DefaultConfig(), request(t.TempDir()))
	if !errors.Is(err, sync.ErrAnalysis) {
		t.Fatalf("Analyze error = %v, want sync.ErrAnalysis", err)
	}
}

func TestConfigValidate(t *testing.T) {
	cases := []Config{
		{CheckpointPositionsPct: []int{0}, CheckpointWindowSec: 150, MaxOffsetSec: 180},
		{CheckpointPositionsPct: []int{101}, CheckpointWindowSec: 150, MaxOffsetSec: 180},
		{CheckpointPositionsPct: []int{10}, CheckpointWindowSec: 0, MaxOffsetSec: 180},
		{CheckpointPositionsPct: []int{10}, CheckpointWindowSec: 150, MaxOffsetSec: 0},
	}
	for i, c := range cases {
		if err := c.validate(); err == nil {
			t.Fatalf("case %d: validate(%+v) = nil, want error", i, c)
		}
	}
	if err := DefaultConfig().validate(); err != nil {
		t.Fatalf("DefaultConfig().validate: %v", err)
	}
}

// TestJobTimeoutPolicy pins the production per-checkpoint budget
// ([DefaultJobTimeout]) and the explicit override [JobTimeout] honours.
//
// Integration tests pass their own, much larger [Config.JobTimeout]: the
// derived production budget must not double as the thing that turns a loaded
// machine into a red suite, so its behaviour is asserted here instead.
//
// Every expectation below is a literal, deliberately not derived from
// MinJobTimeoutMS/JobTimeoutBaseMS/JobTimeoutPer10SecMS: this test is the
// specification of the policy, so it must fail when the policy changes instead
// of moving along with the constant it is supposed to guard.
func TestJobTimeoutPolicy(t *testing.T) {
	// want = 30 s base + 1 s per 10 s of slice, floored at 120 s.
	cases := []struct {
		name     string
		sliceLen float64
		want     time.Duration
	}{
		// 30 s + 19 s = 49 s, so the 120 s floor wins for the settled slice.
		{"settled 190 s slice sits on the floor", 190, 120 * time.Second},
		// 30 s + 6 s = 36 s, well below the floor: the floor still wins.
		{"short 60 s slice sits on the floor", 60, 120 * time.Second},
		// 30 s + 90 s = 120 s, exactly at the floor boundary.
		{"900 s slice lands exactly on the floor", 900, 120 * time.Second},
		// 30 s + 120 s = 150 s, clearly above the floor.
		{"1200 s slice scales past the floor", 1200, 150 * time.Second},
		// 30 s + 200 s = 230 s, deep past the floor.
		{"2000 s slice scales past the floor", 2000, 230 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DefaultJobTimeout(tc.sliceLen); got != tc.want {
				t.Fatalf("DefaultJobTimeout(%v) = %v, want %v", tc.sliceLen, got, tc.want)
			}
		})
	}

	cfg := DefaultConfig()
	// DefaultConfig analyses a 190 s slice (max_offset_sec + margin), whose
	// 49 s derivation is floored: the effective budget is exactly 120 s.
	if got, want := JobTimeout(cfg), 120*time.Second; got != want {
		t.Fatalf("JobTimeout(DefaultConfig()) = %v, want %v", got, want)
	}
	cfg.JobTimeout = testJobTimeout
	if got := JobTimeout(cfg); got != testJobTimeout {
		t.Fatalf("JobTimeout(explicit %v) = %v, want the explicit value", testJobTimeout, got)
	}
}

func TestReportJSONShape(t *testing.T) {
	rep := Report{
		Verdict:        VerdictOK,
		OffsetsMS:      []float64{1, 2},
		Qualities:      []float64{0.5, 0.6},
		MaxDriftMS:     1,
		Classification: ClassificationStable,
		SyncMethod:     reflang.SameLangReference,
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"verdict", "offsets_ms", "qualities", "max_drift_ms", "classification", "sync_method"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("report JSON missing %q: %s", key, raw)
		}
	}
}

// TestVerdictUnreliableNeedsBothScores pins the conjunction D2 asks for: the
// old quality floor must fail *and* the new z must dissent. A weak quality with
// a strong z is the old gate alone; a healthy quality with a weak z is the
// future gate alone. Neither is `unreliable` on its own.
func TestVerdictUnreliableNeedsBothScores(t *testing.T) {
	offsets := []float64{1200, 1210, 1195}
	qualities := []float64{0.9, 0.05, 0.9}

	verdict, classification, _ := classify(offsets, qualities, []float64{30, 3, 30}, testAcceptBoundMS, DefaultThresholds)
	if verdict != VerdictUnreliable || classification != ClassificationUnreliable {
		t.Fatalf("weak quality with a dissenting z = %q/%q, want unreliable/unreliable", verdict, classification)
	}

	verdict, classification, _ = classify(offsets, qualities, []float64{30, 12, 30}, testAcceptBoundMS, DefaultThresholds)
	if verdict != VerdictOK || classification != ClassificationStable {
		t.Fatalf("weak quality with an agreeing z = %q/%q, want ok/stable (the conjunction must not invent distrust)", verdict, classification)
	}
}

// TestVerdictUnreliableOnALegacyReport is the compatibility guard: a 0.4.0
// report carries no z at all, and a weak quality must classify exactly as it
// did then.
func TestVerdictUnreliableOnALegacyReport(t *testing.T) {
	verdict, classification, drift := classify(
		[]float64{1200, 1210, 1195},
		[]float64{0.9, 0.05, 0.9},
		nil,
		testAcceptBoundMS,
		DefaultThresholds,
	)
	if verdict != VerdictUnreliable || classification != ClassificationUnreliable {
		t.Fatalf("legacy report verdict = %q/%q, want unreliable/unreliable", verdict, classification)
	}
	if drift != 15 {
		t.Fatalf("max drift = %v, want 15", drift)
	}
}

// TestVerdictCannotReclassifyA04OKCheckpoint is D2's hard requirement, one
// checkpoint at a time: a quality 0.4.0 called fine (at or above the floor)
// keeps its verdict whatever the z says — including the absent z (0) and a z
// just under the "needs a human look" floor.
func TestVerdictCannotReclassifyA04OKCheckpoint(t *testing.T) {
	stable := []float64{1200, 1210, 1195, 1205, 1198, 1202}
	drifting := []float64{500, 500, 500, 2000, 2000, 2000}

	cases := []struct {
		name    string
		quality float64
		z       float64
		offsets []float64
		want    string
	}{
		{name: "at the floor, no z", quality: 0.10, z: 0, offsets: stable, want: VerdictOK},
		{name: "just above the floor, z 1", quality: 0.11, z: 1, offsets: stable, want: VerdictOK},
		{name: "healthy quality, z just under the floor", quality: 0.5, z: 4.9, offsets: stable, want: VerdictOK},
		{name: "at the floor, drifting spread", quality: 0.10, z: 0, offsets: drifting, want: VerdictDrifting},
		{name: "just above the floor, z 1, drifting spread", quality: 0.11, z: 1, offsets: drifting, want: VerdictDrifting},
		{name: "healthy quality, z just under the floor, drifting spread", quality: 0.5, z: 4.9, offsets: drifting, want: VerdictDrifting},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			qualities := make([]float64, len(tc.offsets))
			matchZ := make([]float64, len(tc.offsets))
			for i := range qualities {
				qualities[i] = tc.quality
				matchZ[i] = tc.z
			}
			verdict, _, _ := classify(tc.offsets, qualities, matchZ, testAcceptBoundMS, DefaultThresholds)
			if verdict == VerdictUnreliable {
				t.Fatalf("verdict = %q for quality %v, z %v: a 0.4.0 ok checkpoint reclassified (want %q)",
					verdict, tc.quality, tc.z, tc.want)
			}
			if verdict != tc.want {
				t.Fatalf("verdict = %q, want %q", verdict, tc.want)
			}
		})
	}
}

// TestThresholdsTableIsTheOnlySource proves classify reads its limits from the
// table and from nowhere else: a hand-built table with an absurd band and a
// zero floor must change the answer, which no surviving literal could do.
func TestThresholdsTableIsTheOnlySource(t *testing.T) {
	if DefaultThresholds.StableSpreadMS != StableSpreadThresholdMS {
		t.Fatalf("DefaultThresholds.StableSpreadMS = %v, want the existing constant %v",
			DefaultThresholds.StableSpreadMS, StableSpreadThresholdMS)
	}
	if DefaultThresholds.LowQuality != LowQualityThreshold {
		t.Fatalf("DefaultThresholds.LowQuality = %v, want the existing constant %v",
			DefaultThresholds.LowQuality, LowQualityThreshold)
	}
	if DefaultThresholds.MatchZMaybe != 5.0 || DefaultThresholds.MatchZGood != 10.0 {
		t.Fatalf("DefaultThresholds z bands = %v/%v, want 5/10",
			DefaultThresholds.MatchZMaybe, DefaultThresholds.MatchZGood)
	}

	// A 5 s spread that the 40 ms band would call drifting: the table's band
	// is the only thing that decides.
	spread := []float64{0, 2500, 5000}
	qualities := []float64{0.9, 0.9, 0.9}
	wide := DefaultThresholds
	wide.StableSpreadMS = 1e9
	if verdict, _, _ := classify(spread, qualities, nil, testAcceptBoundMS, wide); verdict != VerdictOK {
		t.Fatalf("verdict = %q with a 1e9 ms band, want ok", verdict)
	}
	if verdict, _, _ := classify(spread, qualities, nil, testAcceptBoundMS, DefaultThresholds); verdict != VerdictDrifting {
		t.Fatalf("verdict = %q with the default band, want drifting", verdict)
	}

	// A zero floor cannot be reached, so nothing is unreliable however weak
	// the correlation and however absent the z.
	weak := DefaultThresholds
	weak.LowQuality = 0
	if verdict, _, _ := classify(spread, []float64{0.01, 0.01, 0.01}, nil, testAcceptBoundMS, weak); verdict == VerdictUnreliable {
		t.Fatalf("verdict = %q with LowQuality 0, want anything but unreliable", verdict)
	}
}

// TestReportJSONShapeCarriesMatchZ extends TestReportJSONShape: the new keys
// travel with every report and the existing ones still do.
func TestReportJSONShapeCarriesMatchZ(t *testing.T) {
	rep := Report{
		Verdict:        VerdictOK,
		OffsetsMS:      []float64{1, 2},
		Qualities:      []float64{0.5, 0.6},
		MaxDriftMS:     1,
		Classification: ClassificationStable,
		Checkpoints: []Checkpoint{
			{PositionPct: 10, OffsetMS: 1, Quality: 0.5, OffsetSubMS: 1.3, MatchZ: 27.5, PrecisionMS: 0.125},
			{PositionPct: 25, OffsetMS: 2, Quality: 0.6, OffsetSubMS: 2.1, MatchZ: 31.0, PrecisionMS: 0.125},
		},
		SyncMethod:  reflang.SameLangReference,
		MatchZ:      []float64{27.5, 31},
		PrecisionMS: 0.125,
		Thresholds:  DefaultThresholds,
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"verdict", "offsets_ms", "qualities", "max_drift_ms", "classification", "sync_method",
		"match_z", "precision_ms", "thresholds",
	} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("report JSON missing %q: %s", key, raw)
		}
	}
	// The per-checkpoint keys are not omitempty either: 0 is the documented
	// "not recorded" sentinel, not an absent field.
	var checkpoints []map[string]json.RawMessage
	if err := json.Unmarshal(fields["checkpoints"], &checkpoints); err != nil {
		t.Fatalf("unmarshal checkpoints: %v", err)
	}
	for _, key := range []string{"offset_sub_ms", "match_z", "precision_ms"} {
		if _, ok := checkpoints[0][key]; !ok {
			t.Fatalf("checkpoint JSON missing %q: %s", key, fields["checkpoints"])
		}
	}
	// A zero z is a real value, not an omission.
	legacy := Report{Verdict: VerdictOK, OffsetsMS: []float64{1}, Qualities: []float64{0.5}, MatchZ: []float64{0}}
	zeroRaw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal zero-z report: %v", err)
	}
	var zeroFields map[string]json.RawMessage
	if err := json.Unmarshal(zeroRaw, &zeroFields); err != nil {
		t.Fatalf("unmarshal zero-z report: %v", err)
	}
	if string(zeroFields["match_z"]) == "" {
		t.Fatalf("report JSON dropped a zero match_z: %s", zeroRaw)
	}
}

// TestReportCarriesFit: the report publishes the trend next to the spread.
//
// The fixture is the drifting one above, and it is a *step*: the offsets jump
// once and stay put, so the fit must call it a cut — a step is not a drift and
// must never be made muxable by the tempo path. The second half of the test is
// a real linear ramp, which is what a drift looks like.
func TestReportCarriesFit(t *testing.T) {
	eng := &scriptedEngine{offsets: []float64{500, 500, 500, 2000, 2000, 2000}}
	rep, err := Analyze(context.Background(), eng, DefaultConfig(), request(t.TempDir()))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.Fit == nil {
		t.Fatal("report.fit = nil, want the trend")
	}
	if rep.Fit.Shape != ShapeCut {
		t.Fatalf("fit.shape = %q (slope %v, R2 %v), want %q for a one-step timeline",
			rep.Fit.Shape, rep.Fit.SlopeMSPerSec, rep.Fit.R2, ShapeCut)
	}
	if rep.Fit.SlopeMSPerSec == 0 {
		t.Fatal("fit.slope_ms_per_sec = 0, want the non-zero slope a step produces")
	}
	if rep.Fit.Points != len(rep.OffsetsMS) {
		t.Fatalf("fit.points = %d, want %d", rep.Fit.Points, len(rep.OffsetsMS))
	}

	// A genuine ramp at 40.96 ms/s (25.000 -> 23.976 fps): the fit reports the
	// drift and the factor that would remove it, and analyze leaves
	// TempoApplied alone — only the worker knows whether an encode can run.
	timing := Timing{
		Target: SourceTiming{DurationSec: 600, OriginKnown: true},
		Donor:  SourceTiming{DurationSec: 600, OriginKnown: true},
	}
	windows, err := PlanWindows(timing, DefaultCheckpointPositionsPct, searchWindowSec(DefaultConfig()))
	if err != nil {
		t.Fatalf("PlanWindows: %v", err)
	}
	ramp := make([]float64, len(windows))
	for i, w := range windows {
		ramp[i] = -340 + 40.96*w.FilmStartSec
	}
	rep, err = Analyze(context.Background(), &scriptedEngine{offsets: ramp}, DefaultConfig(), request(t.TempDir()))
	if err != nil {
		t.Fatalf("Analyze(ramp): %v", err)
	}
	if rep.Verdict != VerdictDrifting {
		t.Fatalf("verdict = %q, want %q", rep.Verdict, VerdictDrifting)
	}
	if rep.Fit == nil || rep.Fit.Shape != ShapeDrift {
		t.Fatalf("fit = %+v, want shape %q", rep.Fit, ShapeDrift)
	}
	if math.Abs(rep.Fit.SlopeMSPerSec-40.96) > 0.2 {
		t.Fatalf("fit.slope_ms_per_sec = %v, want 40.96", rep.Fit.SlopeMSPerSec)
	}
	if math.Abs(rep.Fit.TempoFactor-0.95904) > 2e-4 {
		t.Fatalf("fit.tempo_factor = %v, want 0.95904", rep.Fit.TempoFactor)
	}
	if rep.Fit.TempoFactor >= 1 {
		t.Fatalf("fit.tempo_factor = %v for a positive slope, want < 1: the dub runs fast and is slowed",
			rep.Fit.TempoFactor)
	}
	if rep.Fit.TempoApplied {
		t.Fatal("fit.tempo_applied = true, want false: analyze never claims the encode happened")
	}
}

func TestReportJSONShapeCarriesFit(t *testing.T) {
	rep := Report{
		Verdict:        VerdictOK,
		OffsetsMS:      []float64{1, 2, 3},
		Qualities:      []float64{0.5, 0.6, 0.7},
		Classification: ClassificationStable,
		SyncMethod:     reflang.SameLangReference,
		Fit: &LineFit{
			SlopeMSPerSec: 40.96, InterceptMS: -340, ResidualRMSMS: 1.2, MaxResidualMS: 3.4,
			R2: 0.999, Points: 3, Shape: ShapeDrift, TempoFactor: 0.95904,
		},
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := fields["fit"]; !ok {
		t.Fatalf("report JSON missing fit: %s", raw)
	}
	var fit map[string]json.RawMessage
	if err := json.Unmarshal(fields["fit"], &fit); err != nil {
		t.Fatalf("unmarshal fit: %v", err)
	}
	for _, key := range []string{
		"slope_ms_per_sec", "intercept_ms", "residual_rms_ms", "max_residual_ms",
		"r2", "points", "shape", "tempo_factor", "tempo_applied",
	} {
		if _, ok := fit[key]; !ok {
			t.Fatalf("fit JSON missing %q: %s", key, fields["fit"])
		}
	}

	// A 0.4.0 row has no trend at all: the key must be absent, not zeroed, so
	// the UI can tell "never measured" from "measured as flat".
	legacy := Report{Verdict: VerdictOK, OffsetsMS: []float64{1}, Qualities: []float64{0.5}}
	legacyRaw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	var legacyFields map[string]json.RawMessage
	if err := json.Unmarshal(legacyRaw, &legacyFields); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if _, ok := legacyFields["fit"]; ok {
		t.Fatalf("legacy report JSON carries fit: %s", legacyRaw)
	}
	if _, ok := legacyFields["application"]; ok {
		t.Fatalf("legacy report JSON carries application: %s", legacyRaw)
	}
}

// TestIntegrationConstantOffsetPrecision runs the existing constant-offset pair
// through the real engine and adds the precision the report now states: the
// staged WAVs are written at 1 kHz, the runner analyses them at the file's rate,
// so one sample is 1 ms. The offsets themselves are unchanged.
func TestIntegrationConstantOffsetPrecision(t *testing.T) {
	eng := requireEngine(t)
	dir := t.TempDir()
	n := durationS * sampleRate
	target := noiseSignal(n, 11)
	writeWAV(t, filepath.Join(dir, "target.wav"), target)
	writeWAV(t, filepath.Join(dir, "donor.wav"), shiftedDonor(target, 1200))

	req := request(dir)
	req.DurationSec = 0 // derive from the WAV headers
	ctx, cancel := context.WithTimeout(context.Background(), testAnalysisBudget)
	defer cancel()
	rep, err := Analyze(ctx, eng, testJobConfig(), req)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.Verdict != VerdictOK {
		t.Fatalf("verdict = %q (offsets %v, drift %v), want ok", rep.Verdict, rep.OffsetsMS, rep.MaxDriftMS)
	}
	for i, off := range rep.OffsetsMS {
		if math.Abs(off-1200) > 5 {
			t.Fatalf("checkpoint %d offset = %v, want ~1200", i, off)
		}
	}
	if rep.PrecisionMS != 1.0 {
		t.Fatalf("precision_ms = %v, want 1.0 (one sample of the 1 kHz fixture)", rep.PrecisionMS)
	}
	if len(rep.MatchZ) != len(rep.OffsetsMS) {
		t.Fatalf("match_z has %d entries for %d checkpoints, want one each", len(rep.MatchZ), len(rep.OffsetsMS))
	}
	for i, z := range rep.MatchZ {
		if !(z > 0) || math.IsNaN(z) || math.IsInf(z, 0) {
			t.Fatalf("checkpoint %d match_z = %v on a clean pair, want a finite positive number", i, z)
		}
	}
}

// Integration: synthesised WAV pairs through the real Rust engine.

const (
	sampleRate = 1000
	durationS  = 600
)

// noiseSignal mirrors internal/sync's deterministic broadband noise.
func noiseSignal(n int, seed uint64) []float32 {
	out := make([]float32, n)
	x := seed | 1
	for i := range out {
		x = x*6364136223846793005 + 1442695040888963407
		out[i] = float32(int64(x>>33)) / float32(1<<31)
	}
	return out
}

// shiftedDonor returns donor[t] = target[t+lead], i.e. the donor runs lead
// samples ahead, so offset_ms = t_target - t_donor is positive.
func shiftedDonor(target []float32, lead int) []float32 {
	out := make([]float32, len(target))
	copy(out, target[lead:])
	return out
}

// piecewiseDonor shifts by leadA before split and leadB from split on. Test
// checkpoint windows straddle neither side of split.
func piecewiseDonor(target []float32, leadA, leadB, split int) []float32 {
	out := make([]float32, len(target))
	for i := range out {
		lead := leadA
		if i >= split {
			lead = leadB
		}
		if i+lead < len(target) {
			out[i] = target[i+lead]
		}
	}
	return out
}

// writeWAV writes mono float32 PCM in a RIFF/WAVE container.
func writeWAV(t *testing.T, path string, samples []float32) {
	t.Helper()
	writeWAVRate(t, path, samples, sampleRate)
}

// writeWAVRate is writeWAV with an explicit sample rate.
func writeWAVRate(tb testing.TB, path string, samples []float32, rate uint32) {
	tb.Helper()
	f, err := os.Create(path)
	if err != nil {
		tb.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()

	const headerLen = 44
	dataLen := len(samples) * 4
	var h [headerLen]byte
	copy(h[0:4], "RIFF")
	binary.LittleEndian.PutUint32(h[4:8], uint32(36+dataLen))
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:20], 16)
	binary.LittleEndian.PutUint16(h[20:22], 3) // IEEE float
	binary.LittleEndian.PutUint16(h[22:24], 1) // mono
	binary.LittleEndian.PutUint32(h[24:28], rate)
	binary.LittleEndian.PutUint32(h[28:32], rate*4)
	binary.LittleEndian.PutUint16(h[32:34], 4)
	binary.LittleEndian.PutUint16(h[34:36], 32)
	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:44], uint32(dataLen))
	if _, err := f.Write(h[:]); err != nil {
		tb.Fatalf("write header: %v", err)
	}
	buf := make([]byte, 0, 64*1024)
	for _, s := range samples {
		buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(s))
		if len(buf) >= 64*1024 {
			if _, err := f.Write(buf); err != nil {
				tb.Fatalf("write samples: %v", err)
			}
			buf = buf[:0]
		}
	}
	if _, err := f.Write(buf); err != nil {
		tb.Fatalf("write tail: %v", err)
	}
}

func TestIntegrationConstantOffset(t *testing.T) {
	eng := requireEngine(t)
	dir := t.TempDir()
	n := durationS * sampleRate
	target := noiseSignal(n, 11)
	writeWAV(t, filepath.Join(dir, "target.wav"), target)
	writeWAV(t, filepath.Join(dir, "donor.wav"), shiftedDonor(target, 1200))

	req := request(dir)
	req.DurationSec = 0 // derive from the WAV headers
	ctx, cancel := context.WithTimeout(context.Background(), testAnalysisBudget)
	defer cancel()
	rep, err := Analyze(ctx, eng, testJobConfig(), req)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.Verdict != VerdictOK {
		t.Fatalf("verdict = %q (offsets %v, drift %v), want ok", rep.Verdict, rep.OffsetsMS, rep.MaxDriftMS)
	}
	for i, off := range rep.OffsetsMS {
		if math.Abs(off-1200) > 5 {
			t.Fatalf("checkpoint %d offset = %v, want ~1200", i, off)
		}
	}
	if rep.MaxDriftMS > 40 {
		t.Fatalf("max drift = %v, want <= 40", rep.MaxDriftMS)
	}
}

// TestIntegrationPiecewiseDrift is the re-cut fixture run against the real
// engine: one 2.5 s step at 712 s. Piecewise is disabled here, which is the
// shipped default (05-piecewise-sync.md), so the verdict stays `drifting`.
func TestIntegrationPiecewiseDrift(t *testing.T) {
	assertPiecewiseDrift(t)
}

// TestIntegrationPiecewiseDriftKeepsDriftingVerdict is the named assertion the
// #86 plan states (docs/plans/0.5.0/02-line-fit-drift.md): a step is a
// `drifting` verdict whose trend is a `cut`, never a `drift` — otherwise the
// worker would try to tempo-correct a re-cut and make it muxable.
func TestIntegrationPiecewiseDriftKeepsDriftingVerdict(t *testing.T) {
	assertPiecewiseDrift(t)
}

// assertPiecewiseDrift runs the step fixture once per named test: a `drifting`
// verdict paired with a `cut` shape is the whole point of both.
func assertPiecewiseDrift(t *testing.T) {
	t.Helper()
	eng := requireEngine(t)
	dir := t.TempDir()
	const dur = 1500
	n := dur * sampleRate
	target := noiseSignal(n, 13)
	// 190 s windows centred on 10/25/40/55/70/85 %: 55-245, 280-470, 505-695,
	// 730-920, 955-1145, 1180-1370. A split at 712 s leaves every window
	// entirely on one side.
	donor := piecewiseDonor(target, 500, 2000, 712*sampleRate)
	writeWAV(t, filepath.Join(dir, "target.wav"), target)
	writeWAV(t, filepath.Join(dir, "donor.wav"), donor)

	req := request(dir)
	req.DurationSec = dur
	ctx, cancel := context.WithTimeout(context.Background(), testAnalysisBudget)
	defer cancel()
	rep, err := Analyze(ctx, eng, testJobConfig(), req)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.Verdict != VerdictDrifting || rep.Classification != ClassificationDrifting {
		t.Fatalf("verdict/classification = %q/%q (offsets %v), want drifting", rep.Verdict, rep.Classification, rep.OffsetsMS)
	}
	if rep.MaxDriftMS < 1000 {
		t.Fatalf("max drift = %v, want >= 1000", rep.MaxDriftMS)
	}
	// #86: a re-cut is a step, not a drift. The trend must say so — a job like
	// this stays unmuxable and waits for piecewise segmentation (slice 05).
	// The test keeps its name and its `drifting` verdict because piecewise is
	// off here, which is the shipped default (05-piecewise-sync.md).
	if rep.Fit == nil {
		t.Fatal("report.fit = nil, want the trend")
	}
	if rep.Fit.Shape != ShapeCut {
		t.Fatalf("fit.shape = %q (slope %v, R2 %v), want %q: a step is not a drift",
			rep.Fit.Shape, rep.Fit.SlopeMSPerSec, rep.Fit.R2, ShapeCut)
	}
	if rep.Fit.TempoFactor != 0 {
		t.Fatalf("fit.tempo_factor = %v, want 0: a cut cannot be tempo-corrected", rep.Fit.TempoFactor)
	}
}

func TestIntegrationOutOfRange(t *testing.T) {
	eng := requireEngine(t)
	dir := t.TempDir()
	n := durationS * sampleRate
	target := noiseSignal(n, 17)
	writeWAV(t, filepath.Join(dir, "target.wav"), target)
	// A 5 s shift is visible inside every 150 s slice but exceeds the
	// configured 2 s acceptance bound.
	writeWAV(t, filepath.Join(dir, "donor.wav"), shiftedDonor(target, 5*sampleRate))

	cfg := testJobConfig()
	cfg.MaxOffsetSec = 2
	req := request(dir)
	req.DurationSec = 0
	ctx, cancel := context.WithTimeout(context.Background(), testAnalysisBudget)
	defer cancel()
	rep, err := Analyze(ctx, eng, cfg, req)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.Verdict != VerdictOutOfRange {
		t.Fatalf("verdict = %q (offsets %v), want out_of_range", rep.Verdict, rep.OffsetsMS)
	}
}

// recordingEngine wraps the real engine and keeps the jobs it was handed, so
// integration tests can assert the window the analysis asked for.
type recordingEngine struct {
	inner Engine
	calls []sync.Job
}

func (r *recordingEngine) Run(ctx context.Context, job sync.Job) (sync.Result, error) {
	r.calls = append(r.calls, job)
	return r.inner.Run(ctx, job)
}

// TestIntegrationOffsetWithinSearchWindow places the donor 165 s ahead: beyond
// the 150 s checkpoint window but inside the 180 s max_offset_sec bound. The
// engine can only report it because the analysed slice is widened to
// max_offset_sec + margin; with a 150 s slice the lag cap hides it.
func TestIntegrationOffsetWithinSearchWindow(t *testing.T) {
	eng := &recordingEngine{inner: requireEngine(t)}
	dir := t.TempDir()
	const (
		rate   = 8000
		dur    = 600
		leadMS = 165000
	)
	target := noiseSignal(dur*rate, 19)
	writeWAVRate(t, filepath.Join(dir, "target.wav"), target, rate)
	writeWAVRate(t, filepath.Join(dir, "donor.wav"), shiftedDonor(target, leadMS*rate/1000), rate)

	req := request(dir)
	req.DurationSec = dur
	ctx, cancel := context.WithTimeout(context.Background(), testAnalysisBudget)
	defer cancel()
	rep, err := Analyze(ctx, eng, testJobConfig(), req)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	want := float64(DefaultMaxOffsetSec + CheckWindowMarginSec) // 190 s slice
	for i, job := range eng.calls {
		if job.SliceLenSec != want {
			t.Fatalf("checkpoint %d slice_len = %v, want %v", i, job.SliceLenSec, want)
		}
		if job.WindowSec == nil || *job.WindowSec != want {
			t.Fatalf("checkpoint %d window_sec = %v, want %v", i, job.WindowSec, want)
		}
	}
	for i, off := range rep.OffsetsMS {
		if math.Abs(off-leadMS) > 50 {
			t.Fatalf("checkpoint %d offset = %v, want ~%v", i, off, leadMS)
		}
	}
	// 165 s is inside the settled 180 s acceptance bound.
	if rep.Verdict != VerdictOK || rep.Classification != ClassificationStable {
		t.Fatalf("verdict/classification = %q/%q (offsets %v), want ok/stable", rep.Verdict, rep.Classification, rep.OffsetsMS)
	}
}

// Parallel checkpoints (U-DSP.7).
//
// The invariant under test is the report, not the speed: it must be
// byte-identical for any worker count, and the error must be the lowest failing
// checkpoint whatever order the goroutines finished in.

// withMemAvailable substitutes the effective-memory-limit seam for one test.
// The package's tests are sequential (none calls t.Parallel), so a plain
// variable swap is enough; restore happens on cleanup even after a fatal
// assertion.
func withMemAvailable(t *testing.T, available uint64, ok bool) {
	t.Helper()
	prev := memAvailable
	memAvailable = func() (uint64, bool) { return available, ok }
	t.Cleanup(func() { memAvailable = prev })
}

// parallelConfig is the settled config with n workers and a budget that lets n
// of them through, so a test that is not about the memory ceiling cannot be
// silently reduced by this host's free RAM.
func parallelConfig(t *testing.T, n int) Config {
	t.Helper()
	withMemAvailable(t, 2*checkpointBytes(MaxParallelCheckpoints), true)
	cfg := DefaultConfig()
	cfg.ParallelCheckpoints = n
	return cfg
}

// checkpointStarts maps a checkpoint's planned film start to its index, so a
// stub engine can answer per checkpoint instead of per call order — which is
// what makes a completion-order bug visible.
func checkpointStarts(cfg Config) map[float64]int {
	timing := Timing{
		Target: SourceTiming{DurationSec: 600, OriginKnown: true},
		Donor:  SourceTiming{DurationSec: 600, OriginKnown: true},
	}
	plan, err := PlanWindows(timing, cfg.CheckpointPositionsPct, searchWindowSec(cfg))
	if err != nil {
		panic(err)
	}
	starts := make(map[float64]int, len(plan))
	for i, w := range plan {
		starts[w.FilmStartSec] = i
	}
	return starts
}

// reverseOrderEngine answers per checkpoint but finishes in reverse: the last
// checkpoint returns first. A report assembled in completion order would be
// reversed, so n=1 equality is a real assertion, not a tautology.
type reverseOrderEngine struct {
	starts  map[float64]int
	offsets []float64
}

func (e *reverseOrderEngine) Run(ctx context.Context, job sync.Job) (sync.Result, error) {
	idx, ok := e.starts[job.SliceStartSec]
	if !ok {
		return sync.Result{}, fmt.Errorf("unstubbed slice start %v", job.SliceStartSec)
	}
	if d := time.Duration(len(e.offsets)-1-idx) * 5 * time.Millisecond; d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return sync.Result{}, ctx.Err()
		}
	}
	return echoResult(job, e.offsets[idx], 0.9+float64(idx)/100), nil
}

// TestParallelReportIsOrderIndependent is the invariant: the marshalled report
// for n workers equals the n=1 report byte for byte, while the stub completes
// the checkpoints in the opposite order.
func TestParallelReportIsOrderIndependent(t *testing.T) {
	base := DefaultConfig()
	starts := checkpointStarts(base)
	offsets := []float64{1200, 1203, 1201, 1204, 1202, 1205}

	var want []byte
	for _, n := range []int{1, 2, 4, 6} {
		cfg := parallelConfig(t, n)
		eng := &reverseOrderEngine{starts: starts, offsets: offsets}
		rep, err := Analyze(context.Background(), eng, cfg, request(t.TempDir()))
		if err != nil {
			t.Fatalf("n=%d: Analyze: %v", n, err)
		}
		raw, err := json.Marshal(rep)
		if err != nil {
			t.Fatalf("n=%d: marshal: %v", n, err)
		}
		if n == 1 {
			want = raw
			if len(rep.Checkpoints) != len(offsets) {
				t.Fatalf("checkpoints = %d, want %d", len(rep.Checkpoints), len(offsets))
			}
			for i, cp := range rep.Checkpoints {
				if cp.PositionPct != base.CheckpointPositionsPct[i] || cp.OffsetMS != offsets[i] {
					t.Fatalf("checkpoint %d = %+v, want position %d offset %v",
						i, cp, base.CheckpointPositionsPct[i], offsets[i])
				}
			}
			continue
		}
		if !bytes.Equal(raw, want) {
			t.Fatalf("n=%d report differs from n=1:\n got %s\nwant %s", n, raw, want)
		}
	}
}

// countingEngine tracks how many Run calls are in flight at once. With
// together > 0 it also holds every call until that many are in flight
// together, so the observed maximum is exact rather than a timing guess.
type countingEngine struct {
	together int32
	hold     time.Duration
	open     chan struct{}
	opened   atomic.Bool
	inFlight atomic.Int32
	max      atomic.Int32
}

func newCountingEngine(together int32, hold time.Duration) *countingEngine {
	e := &countingEngine{together: together, hold: hold}
	if together > 0 {
		e.open = make(chan struct{})
	}
	return e
}

func (e *countingEngine) Run(ctx context.Context, job sync.Job) (sync.Result, error) {
	cur := e.inFlight.Add(1)
	defer e.inFlight.Add(-1)
	for {
		max := e.max.Load()
		if cur <= max || e.max.CompareAndSwap(max, cur) {
			break
		}
	}
	if e.together > 0 {
		if cur >= e.together && e.opened.CompareAndSwap(false, true) {
			close(e.open)
		}
		select {
		case <-e.open:
		case <-ctx.Done():
			return sync.Result{}, ctx.Err()
		case <-time.After(10 * time.Second):
			return sync.Result{}, fmt.Errorf("%d checkpoints never ran together, want %d", e.max.Load(), e.together)
		}
		return echoResult(job, 1200, 0.9), nil
	}
	if e.hold > 0 {
		select {
		case <-time.After(e.hold):
		case <-ctx.Done():
			return sync.Result{}, ctx.Err()
		}
	}
	return echoResult(job, 1200, 0.9), nil
}

// TestParallelConcurrencyCap proves the workers are really concurrent and never
// exceed the clamp: maxInFlight is exactly min(n, checkpoints), never more.
func TestParallelConcurrencyCap(t *testing.T) {
	for _, n := range []int{1, 3, 6} {
		cfg := parallelConfig(t, n)
		want := int32(min(n, len(cfg.CheckpointPositionsPct)))
		eng := newCountingEngine(want, 0)
		if _, err := Analyze(context.Background(), eng, cfg, request(t.TempDir())); err != nil {
			t.Fatalf("n=%d: Analyze: %v", n, err)
		}
		if got := eng.max.Load(); got != want {
			t.Fatalf("n=%d: max in flight = %d, want %d", n, got, want)
		}
	}
}

// failingEngine fails the checkpoints in fail, with a per-checkpoint delay, and
// deliberately ignores the context: a checkpoint's own failure must be
// observable even after a sibling has already cancelled the job.
type failingEngine struct {
	starts map[float64]int
	fail   map[int]string
	delay  map[int]time.Duration
}

func (e *failingEngine) Run(_ context.Context, job sync.Job) (sync.Result, error) {
	idx, ok := e.starts[job.SliceStartSec]
	if !ok {
		return sync.Result{}, fmt.Errorf("unstubbed slice start %v", job.SliceStartSec)
	}
	time.Sleep(e.delay[idx])
	if msg, bad := e.fail[idx]; bad {
		return sync.Result{}, errors.New(msg)
	}
	return echoResult(job, 1200, 0.9), nil
}

// TestParallelErrorIsLowestIndex pins the deterministic first error: the higher
// checkpoint fails FASTER, so returning whichever error arrived first would name
// checkpoint 55%; every worker count must name checkpoint 25% instead.
func TestParallelErrorIsLowestIndex(t *testing.T) {
	starts := checkpointStarts(DefaultConfig())
	const wantErr = "analyze: checkpoint 25%: checkpoint 25 exploded"

	for _, n := range []int{1, 2, 3, 6} {
		cfg := parallelConfig(t, n)
		// Repeated so a scheduler-dependent implementation has twenty chances
		// to lose the race differently.
		for attempt := 0; attempt < 20; attempt++ {
			eng := &failingEngine{
				starts: starts,
				fail:   map[int]string{1: "checkpoint 25 exploded", 4: "checkpoint 55 exploded"},
				delay:  map[int]time.Duration{1: 8 * time.Millisecond, 4: time.Millisecond},
			}
			_, err := Analyze(context.Background(), eng, cfg, request(t.TempDir()))
			if err == nil {
				t.Fatalf("n=%d attempt %d: Analyze succeeded, want checkpoint 25%%", n, attempt)
			}
			if err.Error() != wantErr {
				t.Fatalf("n=%d attempt %d: error = %q, want %q", n, attempt, err, wantErr)
			}
		}
	}
}

// blockEngine parks every call until its context is done, as the real engine's
// cooperative cancellation does.
type blockEngine struct{ started chan struct{} }

func (e *blockEngine) Run(ctx context.Context, _ sync.Job) (sync.Result, error) {
	select {
	case e.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return sync.Result{}, ctx.Err()
}

// TestParallelCancelStopsAll cancels mid-run: every worker must return, Analyze
// must report one typed error, and no goroutine may be left behind.
func TestParallelCancelStopsAll(t *testing.T) {
	before := runtime.NumGoroutine()

	cfg := parallelConfig(t, 4)
	eng := &blockEngine{started: make(chan struct{}, 1)}
	req := request(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	finished := make(chan error, 1)
	go func() {
		_, err := Analyze(ctx, eng, cfg, req)
		finished <- err
	}()

	select {
	case <-eng.started:
	case <-time.After(10 * time.Second):
		t.Fatal("no checkpoint reached the engine")
	}
	cancel()

	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Analyze error = %v, want a wrapped context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Analyze did not return after the cancel: a worker is still parked")
	}

	// Every worker exits before Analyze returns; the poll only absorbs the
	// runtime's own bookkeeping.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines before/after = %d/%d, want no leak", before, after)
	}
}

// TestMemoryCeilingReducesWorkers gives the seam a budget that fits exactly
// three workers: n is reduced from 4, the reduction is logged once, and the
// report is still the n=1 report.
func TestMemoryCeilingReducesWorkers(t *testing.T) {
	withMemAvailable(t, 2*checkpointBytes(3), true)

	// The package's tests are sequential, and the reduction is logged from
	// this test's own goroutine, so an unlocked buffer is enough.
	var logged bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	req := request(t.TempDir())
	single := DefaultConfig()
	single.ParallelCheckpoints = 1
	eng := newCountingEngine(0, 0)
	want, err := Analyze(context.Background(), eng, single, req)
	if err != nil {
		t.Fatalf("n=1: Analyze: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal n=1: %v", err)
	}

	cfg := DefaultConfig()
	cfg.ParallelCheckpoints = 4
	reduced := newCountingEngine(0, 20*time.Millisecond)
	rep, err := Analyze(context.Background(), reduced, cfg, req)
	if err != nil {
		t.Fatalf("reduced: Analyze: %v", err)
	}
	repJSON, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal reduced: %v", err)
	}
	if !bytes.Equal(repJSON, wantJSON) {
		t.Fatalf("reduced report differs from n=1:\n got %s\nwant %s", repJSON, wantJSON)
	}

	const line = "analyze: parallel checkpoints reduced 4 → 3 (memory budget)"
	text := logged.String()
	if !strings.Contains(text, line) {
		t.Fatalf("log = %q, want %q", text, line)
	}
	if n := strings.Count(text, "\n"); n != 1 {
		t.Fatalf("log lines = %d, want exactly 1: %q", n, text)
	}
	if got := reduced.max.Load(); got != 3 {
		t.Fatalf("max in flight = %d, want the reduced 3", got)
	}
}

// TestParallelWorkerClamp guards the clamp order independently of the engine:
// the setting is 0/negative → 1, capped at MaxParallelCheckpoints and at the
// checkpoint count, and only then reduced by memory.
func TestParallelWorkerClamp(t *testing.T) {
	withMemAvailable(t, 1<<62, true)
	cases := []struct {
		name      string
		workers   int
		positions []int
		want      int
	}{
		{"zero means one", 0, DefaultCheckpointPositionsPct, 1},
		{"negative means one", -3, DefaultCheckpointPositionsPct, 1},
		{"configured value stands", 4, DefaultCheckpointPositionsPct, 4},
		{"the whole list", 6, DefaultCheckpointPositionsPct, 6},
		{"capped at the hard bound", 99, []int{10, 20, 30, 40, 50, 60, 70, 80, 90}, MaxParallelCheckpoints},
		{"capped at the checkpoint count", 8, []int{10, 20, 30}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.ParallelCheckpoints = tc.workers
			cfg.CheckpointPositionsPct = tc.positions
			if got := parallelWorkers(cfg); got != tc.want {
				t.Fatalf("parallelWorkers(%d workers, %d checkpoints) = %d, want %d",
					tc.workers, len(tc.positions), got, tc.want)
			}
		})
	}
}

// The effective memory limit.
//
// The guard exists for a container on a larger host, and MemAvailable there
// describes the host: reading only /proc/meminfo would permit every worker on
// a 4 GB container in a 128 GB TrueNAS box and let the OOM killer decide.
// These tests pin the resolution order — cgroup v2, then cgroup v1, then
// MemAvailable, and the smaller of the cgroup limit and MemAvailable.

// withCgroupRoot points the production reader at a temporary cgroup tree.
func withCgroupRoot(t *testing.T, root string) {
	t.Helper()
	prev := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = prev })
}

// writeCgroupLimit creates one cgroup memory-limit file under root, with
// parents. An empty content means "the file does not exist".
func writeCgroupLimit(t *testing.T, root, rel, content string) {
	t.Helper()
	if content == "" {
		return
	}
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func TestEffectiveMemLimit(t *testing.T) {
	// A 4 GiB container limit, and what MemAvailable reports on a large host.
	const container = uint64(4) << 30
	const host = uint64(120) << 30
	// What cgroup v1 writes when the group has no limit: page-aligned 2^63,
	// i.e. "unlimited", not 8 EiB of RAM.
	const v1Unlimited = "9223372036854771712\n"
	cases := []struct {
		name      string
		v2        string
		v1        string
		meminfo   uint64
		meminfoOK bool
		want      uint64
		wantOK    bool
	}{
		{"v2 limit beats a large MemAvailable", "4294967296\n", "", host, true, container, true},
		{"v2 max means unlimited and falls through", "max\n", "", host, true, host, true},
		{"v1 limit", "", "4294967296\n", host, true, container, true},
		{"v1 near-2^63 means unlimited", "", v1Unlimited, host, true, host, true},
		{"no cgroup file uses MemAvailable", "", "", host, true, host, true},
		{"nothing readable is unknown", "", "", 0, false, 0, false},
		{"MemAvailable wins when it is the smaller ceiling", "17179869184\n", "", container, true, container, true},
		{"v2 wins over v1", "2147483648\n", "4294967296\n", host, true, 2 << 30, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeCgroupLimit(t, root, "memory.max", tc.v2)
			writeCgroupLimit(t, root, "memory/memory.limit_in_bytes", tc.v1)
			meminfo := func() (uint64, bool) { return tc.meminfo, tc.meminfoOK }
			got, ok := effectiveMemLimit(root, meminfo)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("effectiveMemLimit = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestReadEffectiveMemLimitReadsCgroupRoot checks the production wiring: the
// seam's default reader consults cgroupRoot, so a 2 GiB memory.max caps the
// budget whatever this host's MemAvailable happens to be.
func TestReadEffectiveMemLimitReadsCgroupRoot(t *testing.T) {
	root := t.TempDir()
	writeCgroupLimit(t, root, "memory.max", "2147483648\n")
	withCgroupRoot(t, root)

	got, ok := readEffectiveMemLimit()
	if !ok {
		t.Fatal("readEffectiveMemLimit reported no limit with a 2 GiB memory.max present")
	}
	if got > 2<<30 {
		t.Fatalf("readEffectiveMemLimit = %d, want at most the 2 GiB cgroup limit", got)
	}
}

// The cost model.

// TestCheckpointCostModel pins total(n) = 200 MiB + n x 400 MiB, the arithmetic
// written out in the constants' doc comment.
func TestCheckpointCostModel(t *testing.T) {
	cases := map[int]uint64{0: 0, 1: 600 << 20, 2: 1000 << 20, 3: 1400 << 20, 4: 1800 << 20, 8: 3400 << 20}
	for n, want := range cases {
		if got := checkpointBytes(n); got != want {
			t.Errorf("checkpointBytes(%d) = %d MiB, want %d MiB", n, got>>20, want>>20)
		}
	}
}

// TestWorkerClampForContainerSizes pins the worker counts quoted in
// [parallelWorkers] and in internal/dsp/PERF.md, so the documentation cannot
// drift from the model. The setting is the full 8 in every case, so what is
// measured is the ceiling, not the setting. The checkpoint count is a ceiling
// of its own — the settled policy has six — which is why the last two rows use
// a nine-checkpoint config to see the memory ceiling alone.
func TestWorkerClampForContainerSizes(t *testing.T) {
	const GiB = uint64(1) << 30
	nine := []int{10, 20, 30, 40, 50, 60, 70, 80, 90}
	cases := []struct {
		name      string
		container uint64
		positions []int // nil keeps the settled six
		want      int
	}{
		{"1 GiB, settled six", 1 * GiB, nil, 1},
		{"2 GiB, settled six", 2 * GiB, nil, 2},
		{"4 GiB, settled six", 4 * GiB, nil, 4},
		{"6 GiB, settled six: budget allows 7, the six checkpoints cap it", 6 * GiB, nil, 6},
		{"6 GiB, nine checkpoints", 6 * GiB, nine, 7},
		{"16 GiB, settled six", 16 * GiB, nil, 6},
		{"16 GiB, nine checkpoints: the hard bound", 16 * GiB, nine, MaxParallelCheckpoints},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withMemAvailable(t, tc.container, true)
			cfg := DefaultConfig()
			cfg.ParallelCheckpoints = MaxParallelCheckpoints
			if tc.positions != nil {
				cfg.CheckpointPositionsPct = tc.positions
			}
			if got := parallelWorkers(cfg); got != tc.want {
				t.Fatalf("container %d GiB: parallelWorkers = %d, want %d",
					tc.container/GiB, got, tc.want)
			}
		})
	}
}

// TestContainerBytesForFourWorkers pins the second documented figure: 3600 MiB
// is the smallest container whose half-budget holds four workers, and one byte
// less drops to three.
func TestContainerBytesForFourWorkers(t *testing.T) {
	const need = 3600 << 20
	if got := 2 * checkpointBytes(4); got != need {
		t.Fatalf("2 x checkpointBytes(4) = %d MiB, want the documented 3600 MiB", got>>20)
	}
	cfg := DefaultConfig()
	cfg.ParallelCheckpoints = 4

	withMemAvailable(t, need, true)
	if got := parallelWorkers(cfg); got != 4 {
		t.Fatalf("container 3600 MiB: parallelWorkers = %d, want 4", got)
	}
	withMemAvailable(t, need-1, true)
	if got := parallelWorkers(cfg); got != 3 {
		t.Fatalf("container 3600 MiB - 1: parallelWorkers = %d, want 3", got)
	}
}
