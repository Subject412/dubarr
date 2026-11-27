package analyze

import (
	"math"
	"testing"
)

// fitWindows builds the settled checkpoint plan for a film of the given length.
//
// FitTimeline measures against [Window.FilmStartSec] — the window's position in
// the film, which is also what the report publishes as slice_start_sec — so a
// fixture generates its offsets from the windows it hands in, never from the
// percentage the window was planned at.
func fitWindows(t *testing.T, durationSec float64) []Window {
	t.Helper()
	timing := Timing{
		Target: SourceTiming{DurationSec: durationSec, OriginKnown: true},
		Donor:  SourceTiming{DurationSec: durationSec, OriginKnown: true},
	}
	plan, err := PlanWindows(timing, DefaultCheckpointPositionsPct, 190)
	if err != nil {
		t.Fatalf("PlanWindows: %v", err)
	}
	if len(plan) != len(DefaultCheckpointPositionsPct) {
		t.Fatalf("plan length = %d, want %d", len(plan), len(DefaultCheckpointPositionsPct))
	}
	return plan
}

// rampOffsets is the offset a drift of slopeMSPerSec and interceptMS produces at
// every window's film position: offset(t) = intercept + slope*t.
func rampOffsets(windows []Window, slopeMSPerSec, interceptMS float64) []float64 {
	offsets := make([]float64, len(windows))
	for i, w := range windows {
		offsets[i] = interceptMS + slopeMSPerSec*w.FilmStartSec
	}
	return offsets
}

// TestFitTimelineConstant: a flat timeline with decode jitter is a constant
// shift, not a sloped line, and nothing to stretch.
func TestFitTimelineConstant(t *testing.T) {
	windows := fitWindows(t, 5400)
	offsets := []float64{1200, 1205, 1195, 1202, 1198, 1201}

	fit := FitTimeline(windows, offsets, fillQualities(len(offsets), 0.9), nil, DefaultThresholds)
	if fit.Shape != ShapeConstant {
		t.Fatalf("shape = %q (slope %v, R2 %v), want %q",
			fit.Shape, fit.SlopeMSPerSec, fit.R2, ShapeConstant)
	}
	if abs(fit.SlopeMSPerSec) >= 1 {
		t.Fatalf("slope = %v ms/s, want < 1 for a flat timeline", fit.SlopeMSPerSec)
	}
	if fit.R2 < DefaultThresholds.FitR2Min {
		t.Fatalf("R2 = %v, want >= %v", fit.R2, DefaultThresholds.FitR2Min)
	}
	if fit.TempoFactor != 0 {
		t.Fatalf("tempo factor = %v, want 0: a constant shift is not a drift", fit.TempoFactor)
	}
	if fit.Points != len(offsets) {
		t.Fatalf("points = %d, want %d", fit.Points, len(offsets))
	}
	if math.Abs(fit.InterceptMS-1200) > 20 {
		t.Fatalf("intercept = %v ms, want ~1200", fit.InterceptMS)
	}
}

// TestFitTimelineDriftRecoversTempoFactor is the 25.000 -> 23.976 fps case: the
// fit must return the slope the fixture was built from and the atempo factor
// that removes it. TempoApplied stays false — analyze never sets it, because it
// does not know whether an encode path exists.
func TestFitTimelineDriftRecoversTempoFactor(t *testing.T) {
	windows := fitWindows(t, 5400)
	offsets := rampOffsets(windows, 42.7, -340)

	fit := FitTimeline(windows, offsets, fillQualities(len(offsets), 0.9), nil, DefaultThresholds)
	if fit.Shape != ShapeDrift {
		t.Fatalf("shape = %q (slope %v, R2 %v), want %q", fit.Shape, fit.SlopeMSPerSec, fit.R2, ShapeDrift)
	}
	if math.Abs(fit.SlopeMSPerSec-42.7) >= 0.2 {
		t.Fatalf("slope = %v ms/s, want 42.7", fit.SlopeMSPerSec)
	}
	if math.Abs(fit.TempoFactor-1.0427) >= 2e-4 {
		t.Fatalf("tempo factor = %v, want 1.0427", fit.TempoFactor)
	}
	if fit.TempoApplied {
		t.Fatal("TempoApplied = true, want false: analyze must never claim the encode happened")
	}
	if math.Abs(fit.InterceptMS-(-340)) > 0.2 {
		t.Fatalf("intercept = %v ms, want -340", fit.InterceptMS)
	}
}

// TestFitTimelineCutAtTheSplit is the shape a drift must never be mistaken for:
// two flat halves 2.5 s apart. The split is known to lie between the 40 % and
// 55 % windows, so the boundary is the midpoint of that gap with half the gap
// as its uncertainty.
func TestFitTimelineCutAtTheSplit(t *testing.T) {
	windows := fitWindows(t, 5400)
	offsets := []float64{-1200, -1195, -1205, 1300, 1295, 1305}

	fit := FitTimeline(windows, offsets, fillQualities(len(offsets), 0.9), nil, DefaultThresholds)
	if fit.Shape != ShapeCut {
		t.Fatalf("shape = %q (slope %v, R2 %v), want %q", fit.Shape, fit.SlopeMSPerSec, fit.R2, ShapeCut)
	}
	if fit.TempoFactor != 0 {
		t.Fatalf("tempo factor = %v, want 0: a cut is not a drift", fit.TempoFactor)
	}
	low, high := windows[2].FilmStartSec, windows[3].FilmStartSec
	if fit.CutAtSec <= low || fit.CutAtSec >= high {
		t.Fatalf("cut at %v s, want strictly between the neighbouring windows %v and %v s",
			fit.CutAtSec, low, high)
	}
	if math.Abs(fit.CutUncertaintySec-(high-low)/2) > 1e-9 {
		t.Fatalf("cut uncertainty = %v s, want half the gap %v s", fit.CutUncertaintySec, (high-low)/2)
	}
}

// TestFitTimelineMixedWhenTheLineDoesNotHold: offsets with neither a line nor a
// single step behind them. The fixture is fixed, not random, so the verdict is
// reproducible.
func TestFitTimelineMixedWhenTheLineDoesNotHold(t *testing.T) {
	windows := fitWindows(t, 5400)
	offsets := []float64{912, -1503, 421, 1187, -640, 1902}

	fit := FitTimeline(windows, offsets, fillQualities(len(offsets), 0.9), nil, DefaultThresholds)
	if fit.Shape != ShapeMixed {
		t.Fatalf("shape = %q (slope %v, R2 %v), want %q", fit.Shape, fit.SlopeMSPerSec, fit.R2, ShapeMixed)
	}
	if fit.TempoFactor != 0 {
		t.Fatalf("tempo factor = %v, want 0 for a timeline no line explains", fit.TempoFactor)
	}
}

// TestFitTimelineInsufficientBelowThreePoints: two checkpoints still report the
// line they define, but the shape says the sample is too small to classify.
func TestFitTimelineInsufficientBelowThreePoints(t *testing.T) {
	windows := fitWindows(t, 5400)[:2]
	offsets := []float64{-340, -300}

	fit := FitTimeline(windows, offsets, fillQualities(len(offsets), 0.9), nil, DefaultThresholds)
	if fit.Shape != ShapeInsufficient {
		t.Fatalf("shape = %q, want %q", fit.Shape, ShapeInsufficient)
	}
	if fit.Points != 2 {
		t.Fatalf("points = %d, want 2", fit.Points)
	}
	if math.IsNaN(fit.SlopeMSPerSec) || math.IsInf(fit.SlopeMSPerSec, 0) {
		t.Fatalf("slope = %v, want a finite number even below three points", fit.SlopeMSPerSec)
	}
	if fit.TempoFactor != 0 {
		t.Fatalf("tempo factor = %v, want 0", fit.TempoFactor)
	}
}

// TestFitTimelineIgnoresAnUnconfidentPoint: one checkpoint the engine did not
// trust cannot bend the trend it is supposed to be measured against. It sits
// 3 s off the line and below the quality floor, so the fit must drop it and
// report the five that are left.
func TestFitTimelineIgnoresAnUnconfidentPoint(t *testing.T) {
	windows := fitWindows(t, 5400)
	offsets := rampOffsets(windows, 42.7, -340)
	offsets[4] += 3000
	qualities := fillQualities(len(offsets), 0.9)
	qualities[4] = 0.04 // below LowQualityThreshold, and no z to outvote it

	fit := FitTimeline(windows, offsets, qualities, nil, DefaultThresholds)
	if fit.Points != len(offsets)-1 {
		t.Fatalf("points = %d, want %d: the unconfident checkpoint must not be fitted", fit.Points, len(offsets)-1)
	}
	if math.Abs(fit.SlopeMSPerSec-42.7) >= 0.2 {
		t.Fatalf("slope = %v ms/s, want the drift's 42.7, not the outlier's", fit.SlopeMSPerSec)
	}
	if fit.Shape != ShapeDrift {
		t.Fatalf("shape = %q (slope %v, R2 %v), want %q", fit.Shape, fit.SlopeMSPerSec, fit.R2, ShapeDrift)
	}
}

// TestFitTimelineOutlierDoesNotMoveTheSlope reuses the fallback fixture's spike:
// one window 1.14 s off a flat timeline. A least-squares line would take the
// bait; the robust one must stay flat.
func TestFitTimelineOutlierDoesNotMoveTheSlope(t *testing.T) {
	windows := fitWindows(t, 5400)
	offsets := []float64{-340, -340, 800, -340, -340, -340}

	fit := FitTimeline(windows, offsets, fillQualities(len(offsets), 0.9), nil, DefaultThresholds)
	if abs(fit.SlopeMSPerSec) >= 1 {
		t.Fatalf("slope = %v ms/s, want < 1: one spike must not define the trend", fit.SlopeMSPerSec)
	}
	if fit.Shape == ShapeDrift {
		t.Fatalf("shape = %q, want anything but %q for a single spike", fit.Shape, ShapeDrift)
	}
}

// TestTempoFactorForRejectsOutOfRange pins the encoder's own range: atempo
// honours 0.5..2.0 (transcode.ValidateTempo), i.e. slopes of -500..+1000 ms/s.
// One step past either end must be refused rather than clamped.
func TestTempoFactorForRejectsOutOfRange(t *testing.T) {
	// The encoder honours 0.5..2.0 (transcode.ValidateTempo), i.e. slopes of
	// -500..+1000 ms/s. One step past either end must be refused, not clamped.
	for _, slope := range []float64{1200, -600, 0} {
		if factor, ok := tempoFactorFor(slope); ok || factor != 0 {
			t.Fatalf("tempoFactorFor(%v) = %v/%v, want 0/false (outside atempo's 0.5..2.0)",
				slope, factor, ok)
		}
	}
	for _, tc := range []struct {
		slope, factor float64
	}{
		{42.7, 1.0427},  // 25.000 -> 23.976 fps
		{-42.7, 0.9573}, // the same mismatch the other way
	} {
		got, ok := tempoFactorFor(tc.slope)
		if !ok {
			t.Fatalf("tempoFactorFor(%v) = %v/false, want %v/true", tc.slope, got, tc.factor)
		}
		if math.Abs(got-tc.factor) > 1e-9 {
			t.Fatalf("tempoFactorFor(%v) = %v, want %v", tc.slope, got, tc.factor)
		}
	}
}

// TestFitLineIsUnchangedByTheMove guards the relocation out of fallback.go: the
// exact tuples the fallback policy's cases exercise, asserted against fitLine
// directly. A changed body would move at least one of them.
func TestFitLineIsUnchangedByTheMove(t *testing.T) {
	// A constant timeline: slope zero, intercept the offset. Exact, not
	// approximate — five of the six values are identical.
	constant := []timelinePoint{
		{10, -340}, {20, -340}, {30, -340}, {40, -340}, {50, -340}, {60, -340},
	}
	if slope, intercept := fitLine(constant); slope != 0 || intercept != -340 {
		t.Fatalf("fitLine(constant) = %v/%v, want 0/-340", slope, intercept)
	}

	// The fallback's spike fixture: the median of pairwise slopes is 0 and the
	// spike carries the whole 1140 ms residual.
	spike := []timelinePoint{
		{10, -340}, {20, -340}, {30, 800}, {40, -340}, {50, -340}, {60, -340},
	}
	slope, intercept := fitLine(spike)
	if slope != 0 || intercept != -340 {
		t.Fatalf("fitLine(spike) = %v/%v, want 0/-340", slope, intercept)
	}
	if got := spike[2].y - (slope*spike[2].x + intercept); got != 1140 {
		t.Fatalf("spike residual = %v ms, want 1140", got)
	}

	// The fallback's 240 ms ramp over equally spaced windows: the median
	// pairwise slope is the ramp's, which the test derives from the points it
	// built instead of hard-coding a spacing. The windows are snapped to whole
	// samples, so the spacing is only equal to within a sample — hence the
	// relative tolerance rather than an exact compare.
	windows := fallbackWindows(t, len(DefaultCheckpointPositionsPct))
	ramp := []float64{-340, -292, -244, -196, -148, -100}
	points := make([]timelinePoint, len(ramp))
	for i := range ramp {
		points[i] = timelinePoint{x: windows[i].FilmStartSec, y: ramp[i]}
	}
	wantSlope := (ramp[1] - ramp[0]) / (points[1].x - points[0].x)
	wantIntercept := ramp[0] - wantSlope*points[0].x
	gotSlope, gotIntercept := fitLine(points)
	if math.Abs(gotSlope-wantSlope) > 1e-4*wantSlope || math.Abs(gotIntercept-wantIntercept) > 0.1 {
		t.Fatalf("fitLine(ramp) = %v/%v, want %v/%v", gotSlope, gotIntercept, wantSlope, wantIntercept)
	}

	// Two points define the line exactly.
	two := []timelinePoint{{10, 1000}, {20, 2000}}
	if slope, intercept := fitLine(two); math.Abs(slope-100) > 1e-12 || math.Abs(intercept) > 1e-12 {
		t.Fatalf("fitLine(two points) = %v/%v, want 100/0", slope, intercept)
	}
}
