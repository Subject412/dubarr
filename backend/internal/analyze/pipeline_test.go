package analyze

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/reflang"
)

// pipelineConfig is the settled policy with one worker, so a scripted engine's
// call order is the checkpoint order and every routing assertion below is
// about the strategy rather than about the scheduler.
func pipelineConfig() Config {
	cfg := DefaultConfig()
	cfg.ParallelCheckpoints = 1
	return cfg
}

// enginesFor keys one scripted engine under waveform, the only matcher a
// release before #83 had. It is how the existing engine-only tests reach the
// pipeline's engines map.
func enginesFor(e Engine) map[TechniqueID]Engine {
	return map[TechniqueID]Engine{TechniqueWaveform: e}
}

// secondOpinion is [waveform on every checkpoint, then the given technique] with
// the shipped thresholds — the shape #82 exists to make possible.
func secondOpinion(id TechniqueID, when When) Strategy {
	return strategyOf(PresetCustom,
		alwaysEntry(TechniqueWaveform, WhenAlways),
		alwaysEntry(id, when),
	)
}

// stripAdditive removes the fields #82 adds to a report — strategy, notes, the
// winning technique and the attempts — so what is left can be compared with the
// 0.4.0 shape byte for byte.
func stripAdditive(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode report: %v (%s)", err, raw)
	}
	delete(doc, "strategy")
	delete(doc, "notes")
	var checkpoints []map[string]json.RawMessage
	if err := json.Unmarshal(doc["checkpoints"], &checkpoints); err != nil {
		t.Fatalf("decode checkpoints: %v (%s)", err, doc["checkpoints"])
	}
	for _, cp := range checkpoints {
		delete(cp, "technique")
		delete(cp, "attempts")
	}
	blob, err := json.Marshal(checkpoints)
	if err != nil {
		t.Fatalf("re-encode checkpoints: %v", err)
	}
	doc["checkpoints"] = blob
	return doc
}

// TestPipelineDefaultStrategyIsOutputNeutral is the merge gate for the slice.
//
// The same synthetic pair runs twice: once through the shipped strategy (the
// engines map, one waveform entry, one always step) and once through the 0.4.0
// path, reconstructed here by driving the per-checkpoint engine loop directly
// with a single engine and no routing at all. Every value the old report
// published must be equal element for element, and the pinned literals below
// must not move: this is the D4 promise that 0.5.0 changes nothing until the
// operator adds a technique.
func TestPipelineDefaultStrategyIsOutputNeutral(t *testing.T) {
	// Pinned pre-change values: the scripted answers, and what 0.4.0 made of
	// them. A change to any of these is a behaviour change, not a refactor.
	wantOffsets := []float64{1200, 1210, 1195, 1205, 1198, 1202}
	wantQualities := []float64{0.91, 0.92, 0.90, 0.93, 0.94, 0.95}
	const wantMaxDriftMS = 15.0

	req := request(t.TempDir())

	// (a) the shipped strategy, through the engines map: no positional engine
	// at all, so the map is the only way a technique can run.
	cfg := pipelineConfig()
	cfg.Strategy = DefaultStrategy()
	cfg.Engines = enginesFor(&scriptedEngine{offsets: wantOffsets, qualities: wantQualities})
	got, err := Analyze(context.Background(), nil, cfg, req)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if !reflect.DeepEqual(got.OffsetsMS, wantOffsets) {
		t.Fatalf("offsets_ms = %v, want the pinned 0.4.0 values %v", got.OffsetsMS, wantOffsets)
	}
	if !reflect.DeepEqual(got.Qualities, wantQualities) {
		t.Fatalf("qualities = %v, want the pinned 0.4.0 values %v", got.Qualities, wantQualities)
	}
	if got.Verdict != VerdictOK || got.Classification != ClassificationStable {
		t.Fatalf("verdict/classification = %q/%q, want ok/stable", got.Verdict, got.Classification)
	}
	if got.MaxDriftMS != wantMaxDriftMS {
		t.Fatalf("max_drift_ms = %v, want %v", got.MaxDriftMS, wantMaxDriftMS)
	}
	if got.PrecisionMS != 0 {
		t.Fatalf("precision_ms = %v, want 0: the scripted engine reports none", got.PrecisionMS)
	}
	if !reflect.DeepEqual(got.MatchZ, []float64{0, 0, 0, 0, 0, 0}) {
		t.Fatalf("match_z = %v, want no z recorded", got.MatchZ)
	}
	if !reflect.DeepEqual(got.Strategy, DefaultStrategy()) {
		t.Fatalf("report strategy = %+v, want the shipped default", got.Strategy)
	}
	if len(got.Notes) != 0 {
		t.Fatalf("report notes = %v, want none: nothing was skipped", got.Notes)
	}
	for i, cp := range got.Checkpoints {
		if cp.Technique != string(TechniqueWaveform) {
			t.Fatalf("checkpoint %d technique = %q, want %q", i, cp.Technique, TechniqueWaveform)
		}
		if len(cp.Attempts) != 1 || !cp.Attempts[0].Accepted {
			t.Fatalf("checkpoint %d attempts = %+v, want one accepted waveform attempt", i, cp.Attempts)
		}
	}

	// (b) the 0.4.0 path: one engine, no strategy, no routing. The per-window
	// loop is the body Analyze ran before #82.
	legacyCfg := pipelineConfig()
	plan := planFor(t, legacyCfg, req.DurationSec)
	eng := &scriptedEngine{offsets: wantOffsets, qualities: wantQualities}
	slicer := legacyCfg.slicer(req)
	legacyOffsets := make([]float64, len(plan))
	legacyQualities := make([]float64, len(plan))
	legacyMatchZ := make([]float64, len(plan))
	legacyPoints := make([]Checkpoint, len(plan))
	for i, w := range plan {
		res, err := runWindow(context.Background(), eng, legacyCfg, slicer, w)
		if err != nil {
			t.Fatalf("legacy checkpoint %d: %v", i, err)
		}
		legacyOffsets[i] = res.OffsetMS
		legacyQualities[i] = res.CorrelationQuality
		legacyMatchZ[i] = res.MatchZ
		legacyPoints[i] = Checkpoint{
			PositionPct:   w.PositionPct,
			SliceStartSec: w.FilmStartSec,
			SliceLenSec:   w.LenSec,
			OffsetMS:      res.OffsetMS,
			Quality:       res.CorrelationQuality,
			OffsetSubMS:   res.OffsetSubMS,
			MatchZ:        res.MatchZ,
			PrecisionMS:   res.PrecisionMS,
		}
	}
	fit := FitTimeline(plan, legacyOffsets, legacyQualities, legacyMatchZ, DefaultThresholds)
	verdict, classification, drift := classify(legacyOffsets, legacyQualities, legacyMatchZ,
		float64(legacyCfg.MaxOffsetSec)*1000, DefaultThresholds)
	targets, vault, policy, original := sameLangRefs()
	resolved := reflang.Resolve(targets, vault, policy, original)
	legacy := Report{
		Verdict:        verdict,
		OffsetsMS:      legacyOffsets,
		Qualities:      legacyQualities,
		MaxDriftMS:     drift,
		Classification: classification,
		Checkpoints:    legacyPoints,
		SyncMethod:     resolved.Pairs[0].Method,
		Reason:         resolved.Pairs[0].Reason,
		MatchZ:         legacyMatchZ,
		Thresholds:     DefaultThresholds,
		Fit:            &fit,
	}

	gotRaw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	legacyRaw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy report: %v", err)
	}
	if gotDoc, wantDoc := stripAdditive(t, gotRaw), stripAdditive(t, legacyRaw); !reflect.DeepEqual(gotDoc, wantDoc) {
		t.Fatalf("the shipped strategy's report differs from the 0.4.0 path beyond the additive fields:\n got %s\nwant %s",
			gotRaw, legacyRaw)
	}
}

// planFor rebuilds the window plan the analysis will use.
func planFor(t *testing.T, cfg Config, durationSec float64) []Window {
	t.Helper()
	timing := Timing{
		Target: SourceTiming{DurationSec: durationSec, OriginKnown: true},
		Donor:  SourceTiming{DurationSec: durationSec, OriginKnown: true},
	}
	plan, err := PlanWindows(timing, cfg.CheckpointPositionsPct, searchWindowSec(cfg))
	if err != nil {
		t.Fatalf("PlanWindows: %v", err)
	}
	return plan
}

// TestPipelineFallsBackOnOneCheckpointOnly is D4's per-checkpoint promise: the
// second technique runs only where the first was weak, and that one checkpoint
// answers from the fallback while every other checkpoint keeps the first
// answer.
func TestPipelineFallsBackOnOneCheckpointOnly(t *testing.T) {
	wf := &scriptedEngine{
		offsets:   []float64{1200, 1200, 1195, 1200, 1200, 1200},
		qualities: []float64{0.9, 0.9, 0.05, 0.9, 0.9, 0.9},
	}
	second := &scriptedEngine{offsets: []float64{1300}, qualities: []float64{0.8}}

	cfg := pipelineConfig()
	cfg.Strategy = secondOpinion(TechniqueMFCC, WhenBelowConfidence)
	cfg.Engines = map[TechniqueID]Engine{TechniqueWaveform: wf, TechniqueMFCC: second}

	rep, err := Analyze(context.Background(), nil, cfg, request(t.TempDir()))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	weak := rep.Checkpoints[2]
	if len(weak.Attempts) != 2 {
		t.Fatalf("checkpoint 2 attempts = %+v, want two (waveform, then the second opinion)", weak.Attempts)
	}
	if weak.Attempts[0].Technique != string(TechniqueWaveform) || weak.Attempts[0].Accepted {
		t.Fatalf("checkpoint 2 first attempt = %+v, want the refused waveform answer", weak.Attempts[0])
	}
	if weak.Attempts[1].Technique != string(TechniqueMFCC) || !weak.Attempts[1].Accepted {
		t.Fatalf("checkpoint 2 second attempt = %+v, want the accepted fallback", weak.Attempts[1])
	}
	if weak.Technique != string(TechniqueMFCC) || weak.OffsetMS != 1300 {
		t.Fatalf("checkpoint 2 = %q/%v, want mfcc/1300", weak.Technique, weak.OffsetMS)
	}

	for i, cp := range rep.Checkpoints {
		if i == 2 {
			continue
		}
		if len(cp.Attempts) != 1 {
			t.Fatalf("checkpoint %d attempts = %+v, want only the waveform answer", i, cp.Attempts)
		}
		if cp.Technique != string(TechniqueWaveform) || cp.OffsetMS != 1200 {
			t.Fatalf("checkpoint %d = %q/%v, want waveform/1200", i, cp.Technique, cp.OffsetMS)
		}
	}

	// The first technique runs on every checkpoint by definition; the second
	// opinion runs exactly where it was needed.
	if len(wf.calls) != 6 {
		t.Fatalf("waveform ran %d times, want 6", len(wf.calls))
	}
	if len(second.calls) != 1 {
		t.Fatalf("the fallback ran %d times, want 1", len(second.calls))
	}
}

// TestPipelineBelowConfidenceIsSkippedWhenConfident: a second opinion costs a
// second decode and correlation, so it must not run where the first answer was
// already confident.
func TestPipelineBelowConfidenceIsSkippedWhenConfident(t *testing.T) {
	wf := &scriptedEngine{offsets: []float64{1200, 1200, 1200, 1200, 1200, 1200}, qualities: []float64{0.9, 0.9, 0.9, 0.9, 0.9, 0.9}}
	// An engine with no scripted answers: if it is called at all the scripted
	// engine itself fails the test with "unexpected extra job".
	second := &scriptedEngine{}

	cfg := pipelineConfig()
	cfg.Strategy = secondOpinion(TechniqueMFCC, WhenBelowConfidence)
	cfg.Engines = map[TechniqueID]Engine{TechniqueWaveform: wf, TechniqueMFCC: second}

	rep, err := Analyze(context.Background(), nil, cfg, request(t.TempDir()))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(second.calls) != 0 {
		t.Fatalf("the second opinion ran %d times over confident checkpoints, want 0", len(second.calls))
	}
	for i, cp := range rep.Checkpoints {
		if len(cp.Attempts) != 1 {
			t.Fatalf("checkpoint %d attempts = %+v, want only the waveform answer", i, cp.Attempts)
		}
	}
}

// TestPipelineAboveDriftRunsOnOutliersOnly: `above_drift` is evaluated in the
// second phase, where the fitted line exists, so it must run on exactly the
// checkpoints that break the trend — and the re-fit must show the trend it
// repaired.
func TestPipelineAboveDriftRunsOnOutliersOnly(t *testing.T) {
	// Five flat checkpoints and one 2 s spike at index 2: over four times the
	// stable band (160 ms), so it is the one point over the threshold.
	wf := &scriptedEngine{
		offsets:   []float64{1200, 1200, 3200, 1200, 1200, 1200},
		qualities: []float64{0.9, 0.9, 0.9, 0.9, 0.9, 0.9},
	}
	// The repair answers 1190 with a healthier peak, so it wins on quality and
	// the checkpoint rejoins the trend.
	repair := &scriptedEngine{offsets: []float64{1190}, qualities: []float64{0.95}}

	cfg := pipelineConfig()
	cfg.Strategy = secondOpinion(TechniqueMFCC, WhenAboveDrift)
	cfg.Engines = map[TechniqueID]Engine{TechniqueWaveform: wf, TechniqueMFCC: repair}

	before := FitTimeline(planFor(t, cfg, 600), []float64{1200, 1200, 3200, 1200, 1200, 1200},
		[]float64{0.9, 0.9, 0.9, 0.9, 0.9, 0.9}, nil, DefaultThresholds)

	rep, err := Analyze(context.Background(), nil, cfg, request(t.TempDir()))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if len(repair.calls) != 1 {
		t.Fatalf("the above_drift step ran %d times, want 1 (on the single outlier)", len(repair.calls))
	}
	if got, want := repair.calls[0].SliceStartSec, rep.Checkpoints[2].SliceStartSec; got != want {
		t.Fatalf("the above_drift step ran on the window at %v, want the outlier at %v", got, want)
	}
	spike := rep.Checkpoints[2]
	if spike.Technique != string(TechniqueMFCC) || spike.OffsetMS != 1190 {
		t.Fatalf("the outlier = %q/%v, want mfcc/1190", spike.Technique, spike.OffsetMS)
	}
	if len(spike.Attempts) != 2 {
		t.Fatalf("the outlier attempts = %+v, want the phase-1 answer and the repair", spike.Attempts)
	}
	if spike.Attempts[0].Accepted || !spike.Attempts[1].Accepted {
		t.Fatalf("attempts = %+v, want the replaced answer cleared and the repair accepted", spike.Attempts)
	}
	if rep.OffsetsMS[2] != 1190 {
		t.Fatalf("offsets_ms[2] = %v, want 1190", rep.OffsetsMS[2])
	}
	if rep.Fit == nil {
		t.Fatal("report.fit = nil, want the trend")
	}
	if rep.Fit.MaxResidualMS >= DefaultThresholds.AboveDriftResidualMS {
		t.Fatalf("fit.max_residual_ms = %v, want below %v after the repair (was %v)",
			rep.Fit.MaxResidualMS, DefaultThresholds.AboveDriftResidualMS, before.MaxResidualMS)
	}
	if rep.Fit.MaxResidualMS >= before.MaxResidualMS {
		t.Fatalf("the re-fit's max residual = %v, want it to shrink from %v",
			rep.Fit.MaxResidualMS, before.MaxResidualMS)
	}
	if rep.Verdict != VerdictOK {
		t.Fatalf("verdict = %q, want ok once the outlier is repaired", rep.Verdict)
	}
}

// TestPipelineAllTechniquesFailIsTheLowestIndexError: when every technique on a
// checkpoint errors, the job fails with the lowest failing checkpoint, never
// with whichever goroutine lost the race — and a technique error never becomes
// a zero offset.
func TestPipelineAllTechniquesFailIsTheLowestIndexError(t *testing.T) {
	starts := checkpointStarts(DefaultConfig())
	const wantErr = "analyze: checkpoint 25%: checkpoint 25 exploded"

	for _, workers := range []int{1, 6} {
		cfg := parallelConfig(t, workers)
		cfg.Strategy = secondOpinion(TechniqueMFCC, WhenBelowConfidence)
		// Both techniques fail at checkpoints 1 and 3; checkpoint 3 fails
		// sooner, so returning the first error to arrive would name 40%.
		cfg.Engines = map[TechniqueID]Engine{
			TechniqueWaveform: &failingEngine{
				starts: starts,
				fail:   map[int]string{1: "checkpoint 25 exploded", 3: "checkpoint 40 exploded"},
				delay:  map[int]time.Duration{1: 8 * time.Millisecond, 3: time.Millisecond},
			},
			TechniqueMFCC: &failingEngine{
				starts: starts,
				fail:   map[int]string{1: "checkpoint 25 exploded", 3: "checkpoint 40 exploded"},
				delay:  map[int]time.Duration{1: 2 * time.Millisecond, 3: time.Millisecond},
			},
		}
		if _, err := Analyze(context.Background(), nil, cfg, request(t.TempDir())); err == nil {
			t.Fatalf("workers=%d: Analyze succeeded, want checkpoint 25%%", workers)
		} else if err.Error() != wantErr {
			t.Fatalf("workers=%d: error = %q, want %q", workers, err, wantErr)
		}
	}
}

// TestPipelineReportRecordsStrategy: a report is self-describing, because the
// strategy is what makes two reports comparable.
func TestPipelineReportRecordsStrategy(t *testing.T) {
	cfg := pipelineConfig()
	cfg.Strategy = strategyOf(PresetCustom,
		alwaysEntry(TechniqueWaveform, WhenAlways),
		Technique{ID: TechniqueMFCC, Enabled: true, When: WhenBelowConfidence, MinConfidence: 0.20, MinMatchZ: 8.0},
	)
	cfg.Engines = map[TechniqueID]Engine{
		TechniqueWaveform: &scriptedEngine{offsets: []float64{1200, 1200, 1200, 1200, 1200, 1200}},
		TechniqueMFCC:     &scriptedEngine{},
	}
	rep, err := Analyze(context.Background(), nil, cfg, request(t.TempDir()))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if !reflect.DeepEqual(rep.Strategy, cfg.Strategy) {
		t.Fatalf("report.strategy = %+v, want the configured %+v", rep.Strategy, cfg.Strategy)
	}
	if rep.Strategy.Preset != PresetCustom {
		t.Fatalf("report.strategy.preset = %q, want %q", rep.Strategy.Preset, PresetCustom)
	}
	if len(rep.Strategy.Techniques) != 2 || rep.Strategy.Techniques[1].MinMatchZ != 8.0 {
		t.Fatalf("report.strategy.techniques = %+v, want both entries with their thresholds", rep.Strategy.Techniques)
	}
}

// TestPipelinePrecisionIsTheCoarsestSelected: a report whose answers are half
// sample-accurate and half hop-quantised must not advertise the better number.
func TestPipelinePrecisionIsTheCoarsestSelected(t *testing.T) {
	wf := &scriptedEngine{
		offsets:    []float64{1200, 1200, 1195, 1200, 1200, 1200},
		qualities:  []float64{0.9, 0.9, 0.05, 0.9, 0.9, 0.9},
		precisions: []float64{0.125, 0.125, 0.125, 0.125, 0.125, 0.125},
	}
	coarse := &scriptedEngine{offsets: []float64{1300}, qualities: []float64{0.8}, precisions: []float64{10}}

	cfg := pipelineConfig()
	cfg.Strategy = secondOpinion(TechniqueMFCC, WhenBelowConfidence)
	cfg.Engines = map[TechniqueID]Engine{TechniqueWaveform: wf, TechniqueMFCC: coarse}

	rep, err := Analyze(context.Background(), nil, cfg, request(t.TempDir()))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.PrecisionMS != 10 {
		t.Fatalf("report.precision_ms = %v, want 10 (the coarsest selected answer)", rep.PrecisionMS)
	}
	if rep.Checkpoints[2].PrecisionMS != 10 {
		t.Fatalf("checkpoint 2 precision_ms = %v, want its own 10", rep.Checkpoints[2].PrecisionMS)
	}
	if rep.Checkpoints[0].PrecisionMS != 0.125 {
		t.Fatalf("checkpoint 0 precision_ms = %v, want the waveform's 0.125", rep.Checkpoints[0].PrecisionMS)
	}
}

// TestPipelineSkipsATechniqueWithNoEngine: a configured technique this build
// cannot run (mfcc before slice 04 registers its engine) is skipped with a
// report note — a settings row must never fail every job.
func TestPipelineSkipsATechniqueWithNoEngine(t *testing.T) {
	cfg := pipelineConfig()
	cfg.Strategy = secondOpinion(TechniqueMFCC, WhenBelowConfidence)
	cfg.Engines = enginesFor(&scriptedEngine{
		offsets:   []float64{1200, 1200, 1200, 1200, 1200, 1200},
		qualities: []float64{0.9, 0.9, 0.9, 0.9, 0.9, 0.9},
	})

	rep, err := Analyze(context.Background(), nil, cfg, request(t.TempDir()))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(rep.Notes) != 1 || rep.Notes[0] != `technique "mfcc" skipped: no engine registered` {
		t.Fatalf("report notes = %v, want one note naming the skipped technique", rep.Notes)
	}
	for i, cp := range rep.Checkpoints {
		if len(cp.Attempts) != 1 || cp.Attempts[0].Technique != string(TechniqueWaveform) {
			t.Fatalf("checkpoint %d attempts = %+v, want only the waveform answer", i, cp.Attempts)
		}
	}
	if rep.Verdict != VerdictOK {
		t.Fatalf("verdict = %q, want ok: a skipped technique changes nothing", rep.Verdict)
	}
}
