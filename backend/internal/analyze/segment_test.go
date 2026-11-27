package analyze

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// This file pins the two halves of #84 that decide *what* a piecewise job
// measures: the dense checkpoint grid (#86's six percentages cannot locate a
// boundary) and the split-penalty segmenter that reads the grid's answers.

// filmTiming is a Timing whose two inputs are both durationSec long, which is
// the case the plans are unit-tested on: max_offset_sec is not part of planning.
func filmTiming(durationSec float64) Timing {
	return Timing{
		Target: SourceTiming{DurationSec: durationSec, OriginKnown: true},
		Donor:  SourceTiming{DurationSec: durationSec, OriginKnown: true},
	}
}

// denseGrid is the window list a dense plan of n checkpoints produces: windows
// stepSec apart, each windowSec long, on the target timeline. The segmenter only
// reads FilmStartSec and LenSec, so this is the same data PlanWindowsEvery
// returns without the sample snapping.
func denseGrid(n int, stepSec, windowSec float64) []Window {
	windows := make([]Window, n)
	for i := range windows {
		windows[i] = Window{
			PositionPct:  i,
			FilmStartSec: float64(i) * stepSec,
			LenSec:       windowSec,
		}
	}
	return windows
}

// flatQualities is n confident checkpoints: every point testifies.
func flatQualities(n int, q float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = q
	}
	return out
}

// pseudoJitter is a deterministic pseudo-random value in ±amplitude: a sine
// hash rather than a real generator, so a failure is reproducible and the test
// needs no seed to report.
func pseudoJitter(i int, amplitude float64) float64 {
	x := math.Sin(float64(i)*12.9898+7.13) * 43758.5453
	return (x - math.Floor(x) - 0.5) * 2 * amplitude
}

// TestPlanWindowsEveryCoversTheTimeline pins the dense grid's geometry on a
// 90-minute film: 119 centres, one step apart, none of them reading into the
// last ten seconds.
func TestPlanWindowsEveryCoversTheTimeline(t *testing.T) {
	const (
		dur      = 5400.0
		step     = 45.0
		window   = 60.0
		lastEdge = dur - CheckWindowMarginSec
	)
	plan, err := PlanWindowsEvery(filmTiming(dur), step, window)
	if err != nil {
		t.Fatalf("PlanWindowsEvery: %v", err)
	}
	if want := 119; len(plan) != want {
		t.Fatalf("windows = %d, want %d (a 5400 s film every 45 s)", len(plan), want)
	}

	// The first window is centred on step/2 = 22.5 s, so the clamp pulls it back
	// to the start of the file: it opens at 0 and is a whole window long.
	if plan[0].FilmStartSec != 0 || plan[0].LenSec != window {
		t.Fatalf("first window = %v+%v, want 0+%v (clamped to the start)", plan[0].FilmStartSec, plan[0].LenSec, window)
	}
	for i := 1; i < len(plan); i++ {
		// Centre i is at step/2 + i*step, so the window opens half a window
		// before it.
		wantStart := step/2 + float64(i)*step - window/2
		if diff := plan[i].FilmStartSec - wantStart; math.Abs(diff) > 1e-9 {
			t.Fatalf("window %d starts at %v, want %v: the grid is not one step apart", i, plan[i].FilmStartSec, wantStart)
		}
		if plan[i].LenSec != window {
			t.Fatalf("window %d length = %v, want %v", i, plan[i].LenSec, window)
		}
	}

	last := plan[len(plan)-1]
	lastCentre := last.FilmStartSec + last.LenSec/2
	if end := last.FilmStartSec + last.LenSec; end > lastEdge+1e-9 {
		t.Fatalf("last window ends at %v, want <= %v (it must not read into the tail margin)", end, lastEdge)
	}
	// A uniform grid cannot land exactly on the bound: the last centre is
	// within one step of it, never further.
	if lastCentre < lastEdge-window/2-step {
		t.Fatalf("last centre = %v, want >= %v: the grid stopped more than one step early", lastCentre, lastEdge-window/2-step)
	}
}

// TestPlanWindowsEveryWidensPastTheCap is the cost guard: a ten-hour source
// degrades in resolution instead of allocating an unbounded plan, and the step
// it really used is what the report will state.
func TestPlanWindowsEveryWidensPastTheCap(t *testing.T) {
	const (
		dur  = 36000.0
		step = 45.0
	)
	if got, want := DenseStepSec(dur, step), dur/MaxDenseSamples; math.Abs(got-want) > 1e-9 {
		t.Fatalf("DenseStepSec(%v, %v) = %v, want %v", dur, step, got, want)
	}
	plan, err := PlanWindowsEvery(filmTiming(dur), step, 60)
	if err != nil {
		t.Fatalf("PlanWindowsEvery: %v", err)
	}
	if len(plan) > MaxDenseSamples {
		t.Fatalf("windows = %d, want at most %d", len(plan), MaxDenseSamples)
	}
	if len(plan) != MaxDenseSamples {
		t.Fatalf("windows = %d, want the cap %d: the widened step should fill it", len(plan), MaxDenseSamples)
	}
	if got, want := windowSpacingSec(plan), dur/MaxDenseSamples; math.Abs(got-want) > 1e-9 {
		t.Fatalf("effective step = %v, want %v: the report must state the widened step", got, want)
	}
}

// TestPlanWindowsEveryKeepsTheShopPedPlanAlone guards the shipped path: the
// dense planner is a new function, and the six-point plan is untouched.
func TestPlanWindowsEveryKeepsTheShippedPlanAlone(t *testing.T) {
	timing := filmTiming(5400)
	shipped, err := PlanWindows(timing, DefaultCheckpointPositionsPct, DefaultCheckpointWindowSec)
	if err != nil {
		t.Fatalf("PlanWindows: %v", err)
	}
	if len(shipped) != len(DefaultCheckpointPositionsPct) {
		t.Fatalf("shipped plan = %d windows, want %d", len(shipped), len(DefaultCheckpointPositionsPct))
	}
	if shipped[0].PositionPct != DefaultCheckpointPositionsPct[0] || shipped[0].FilmStartSec != 5400*0.10-75 {
		t.Fatalf("first shipped window = %+v, want the 10%% checkpoint at %v", shipped[0], 5400*0.10-75)
	}
}

// TestBuildSegmentsFindsOneCut is the whole feature in one fixture: 160 points,
// flat at 0 up to the halfway mark and at +2500 ms after it. The boundary must
// be recovered to within one step, and the step and the fallback offset stated.
func TestBuildSegmentsFindsOneCut(t *testing.T) {
	const (
		points = 160
		step   = 45.0
		cut    = 2500.0
	)
	windows := denseGrid(points, step, 60)
	offsets := make([]float64, points)
	for i := points / 2; i < points; i++ {
		offsets[i] = cut
	}
	pw := BuildSegments(windows, offsets, flatQualities(points, 0.9), DefaultPiecewisePolicy(), DefaultThresholds)
	if pw.RejectedReason != "" {
		t.Fatalf("rejected: %q (segments %+v)", pw.RejectedReason, pw.Segments)
	}
	if len(pw.Segments) != 2 || len(pw.Boundaries) != 1 {
		t.Fatalf("segments/boundaries = %d/%d, want 2/1: %+v", len(pw.Segments), len(pw.Boundaries), pw.Segments)
	}
	// The halfway mark is 50 % of 160 points 45 s apart: 3600 s of film.
	if got := math.Abs(pw.Boundaries[0].AtSec - 3600); got > step {
		t.Fatalf("boundary at %v s, want ~3600 (±%v)", pw.Boundaries[0].AtSec, step)
	}
	if got := pw.Boundaries[0].StepMS; math.Abs(got-cut) > 1e-9 {
		t.Fatalf("boundary step = %v ms, want %v", got, cut)
	}
	if got := pw.Boundaries[0].UncertaintySec; math.Abs(got-step/2) > 1e-9 {
		t.Fatalf("boundary uncertainty = %v s, want %v (half a step)", got, step/2)
	}
	if got := math.Abs(pw.DominantOffsetMS - 1250); got > 1e-9 {
		t.Fatalf("dominant offset = %v, want the median of the whole timeline (1250): "+
			"the fallback is a median, not the larger segment's offset", pw.DominantOffsetMS)
	}
	if got, want := pw.Segments[0].OffsetMS, 0.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("first segment offset = %v, want %v", got, want)
	}
	if got := pw.Segments[1].OffsetMS; math.Abs(got-cut) > 1e-9 {
		t.Fatalf("second segment offset = %v, want %v", got, cut)
	}
	if got := pw.Segments[0].Points + pw.Segments[1].Points; got != points {
		t.Fatalf("points covered = %d, want %d", got, points)
	}
	if got, want := pw.StepSec, step; math.Abs(got-want) > 1e-9 {
		t.Fatalf("step_sec = %v, want %v", got, want)
	}
}

// TestBuildSegmentsTilesTheFilm is the first half of defect #84's render path:
// an accepted plan must cover the whole film. The opening checkpoint of this
// fixture is a quiet/fade-in window whose quality is below the floor, so it
// cannot testify and the first *kept* point is index 1 — 45 s into the film.
//
// BuildSegments stops at the evidence it has (it is never handed the film's
// length), so the tiling is the caller's finalisation step: analyze extends an
// accepted plan to 0 and to the film's duration before the report is stored.
// Without it the worker's concat would place the first kept window at time 0
// (a 45 s shift) and the published track would stop at the last kept window's
// end (a lost tail).
func TestBuildSegmentsTilesTheFilm(t *testing.T) {
	const (
		points   = 40
		step     = 45.0
		window   = 60.0
		duration = 1830.0 // the film, a little past the last planned window
		cut      = 2500.0
	)
	windows := denseGrid(points, step, window)
	offsets := make([]float64, points)
	for i := points / 2; i < points; i++ {
		offsets[i] = cut
	}
	qualities := flatQualities(points, 0.9)
	// The opening cannot testify: below thresholds.LowQuality.
	qualities[0] = 0.05

	pw := BuildSegments(windows, offsets, qualities, DefaultPiecewisePolicy(), DefaultThresholds)
	if pw.RejectedReason != "" {
		t.Fatalf("rejected: %q (segments %+v)", pw.RejectedReason, pw.Segments)
	}
	if len(pw.Segments) != 2 {
		t.Fatalf("segments = %d, want 2: %+v", len(pw.Segments), pw.Segments)
	}
	// The defect, pinned: the segmenter alone starts at the first kept window
	// and ends at the last one, which is not the film.
	if got := pw.Segments[0].StartSec; got != windows[1].FilmStartSec {
		t.Fatalf("untiled first start = %v s, want the first kept window's %v s: "+
			"the fixture must drop window 0 for the tiling to be what is tested",
			got, windows[1].FilmStartSec)
	}
	lastKept := windows[len(windows)-1]
	if got, want := pw.Segments[1].EndSec, lastKept.FilmStartSec+lastKept.LenSec; got != want {
		t.Fatalf("untiled last end = %v s, want the last kept window's end %v s", got, want)
	}

	pw.tileToFilm(duration)

	if got := pw.Segments[0].StartSec; got != 0 {
		t.Fatalf("first start = %v s, want 0: concat places the first run at the track's start, "+
			"so any other value shifts the whole published track", got)
	}
	if got := pw.Segments[len(pw.Segments)-1].EndSec; got != duration {
		t.Fatalf("last end = %v s, want the film's %v s: a shorter plan loses the tail", got, duration)
	}
	for k := 1; k < len(pw.Segments); k++ {
		if got, want := pw.Segments[k].StartSec, pw.Segments[k-1].EndSec; got != want {
			t.Fatalf("segment %d starts at %v s but segment %d ends at %v s: the run would repeat or drop audio",
				k, got, k-1, want)
		}
	}
	// Tiling moves the outer edges only: the evidence behind each run (its
	// offset, confidence and point count) is untouched.
	if got, want := pw.Segments[0].Points, points/2-1; got != want {
		t.Fatalf("first segment points = %d, want %d: the dropped opening must not be re-counted", got, want)
	}
	if got, want := pw.Segments[1].Points, points/2; got != want {
		t.Fatalf("second segment points = %d, want %d", got, want)
	}
	if got := pw.Segments[1].OffsetMS; math.Abs(got-cut) > 1e-9 {
		t.Fatalf("second segment offset = %v ms, want %v: tiling must not move an offset", got, cut)
	}
	if got, want := pw.Segments[0].EndSec, pw.Segments[1].StartSec; got != want {
		t.Fatalf("the join moved: %v s vs %v s", got, want)
	}
}

// TestBuildSegmentsRejectedPlanIsNotTiled: tiling is the finalisation of an
// *accepted* plan. A rejected segmentation is never published — the job falls
// back to the dominant offset and fails for review — so it must not grow edges
// that claim film the analysis refused to stand behind.
func TestBuildSegmentsRejectedPlanIsNotTiled(t *testing.T) {
	const points = 160
	offsets := make([]float64, points)
	qualities := flatQualities(points, 0.9)
	for i := points / 2; i < points; i++ {
		offsets[i] = 2500
	}
	qualities[points/2-1] = 0.05
	qualities[points/2] = 0.05

	pw := BuildSegments(denseGrid(points, 45, 60), offsets, qualities, DefaultPiecewisePolicy(), DefaultThresholds)
	if pw.accepted() {
		t.Fatalf("fixture is not rejected: %+v", pw.Segments)
	}
	before := append([]Segment(nil), pw.Segments...)
	pw.tileToFilm(8000)
	if len(pw.Segments) != len(before) {
		t.Fatalf("segments changed: %d -> %d", len(before), len(pw.Segments))
	}
	for k := range before {
		if pw.Segments[k] != before[k] {
			t.Fatalf("segment %d changed from %+v to %+v: a rejected plan must not be tiled",
				k, before[k], pw.Segments[k])
		}
	}
}

// TestBuildSegmentsKeepsACleanTimelineAtOneSegment: ordinary jitter is not a
// cut, and the one-segment answer is *accepted* — it is the ordinary
// constant-offset job, not a rejected plan. An operator who switches
// segmentation on must not see every clean film surfaced for review.
func TestBuildSegmentsKeepsACleanTimelineAtOneSegment(t *testing.T) {
	const points = 160
	offsets := make([]float64, points)
	for i := range offsets {
		offsets[i] = 1200 + pseudoJitter(i, 10)
	}
	pw := BuildSegments(denseGrid(points, 45, 60), offsets, flatQualities(points, 0.9), DefaultPiecewisePolicy(), DefaultThresholds)
	if len(pw.Segments) != 1 || len(pw.Boundaries) != 0 {
		t.Fatalf("segments/boundaries = %d/%d, want 1/0: ±10 ms of jitter is not a cut", len(pw.Segments), len(pw.Boundaries))
	}
	if !pw.accepted() {
		t.Fatalf("the one-segment plan was not accepted: rejected_reason = %q", pw.RejectedReason)
	}
	if pw.RejectedReason != "" {
		t.Fatalf("rejected_reason = %q, want empty: one plateau is the ordinary constant-offset job", pw.RejectedReason)
	}
	if pw.cut() {
		t.Fatal("cut() is true for a single segment: nothing was split, so nothing may be classified piecewise")
	}
}

// TestBuildSegmentsPenaltySuppressesNoiseSplits is the reason the penalty
// exists, stated as a comparison on the same noisy timeline: the DP splinters it
// at a 1 ms penalty and leaves it whole at the shipped 250 ms.
//
// The comparison is made on the DP rather than on the finished segmentation
// because the two gates answer different questions: the penalty decides whether
// a split is worth buying, the min-step rule decides whether the bought step is
// real. A ±40 ms jitter can never clear the 80 ms min step, so BuildSegments
// would report one segment either way and the comparison would prove nothing.
func TestBuildSegmentsPenaltySuppressesNoiseSplits(t *testing.T) {
	const points = 160
	offsets := make([]float64, points)
	for i := range offsets {
		offsets[i] = 700 + pseudoJitter(i, 40)
	}
	windows := denseGrid(points, 45, 60)
	qualities := flatQualities(points, 0.9)

	suppressed := BuildSegments(windows, offsets, qualities, DefaultPiecewisePolicy(), DefaultThresholds)
	if len(suppressed.Segments) != 1 {
		t.Fatalf("default penalty produced %d segments: the split penalty is not suppressing noise (%+v)",
			len(suppressed.Segments), suppressed.Segments)
	}

	cost := segmentCosts(offsets)
	shipped := len(cheapestRuns(cost, DefaultPiecewisePolicy().SplitPenaltyMS*DefaultPiecewisePolicy().SplitPenaltyMS))
	if shipped != 1 {
		t.Fatalf("the DP bought %d runs at the shipped 250 ms penalty, want 1", shipped)
	}
	eager := len(cheapestRuns(cost, 1*1))
	if eager <= shipped {
		t.Fatalf("the DP bought %d runs at a 1 ms penalty and %d at 250 ms: "+
			"the penalty is not what suppresses the splits", eager, shipped)
	}
}

// TestBuildSegmentsRejectsAnUnconfidentBoundary is D5's rule: the two
// checkpoints either side of the join are unreadable, so the join was never
// measured. The step is real but the plan is refused, nothing is guessed, and
// the fallback offset is still the median of what did testify.
func TestBuildSegmentsRejectsAnUnconfidentBoundary(t *testing.T) {
	const points = 160
	offsets := make([]float64, points)
	qualities := flatQualities(points, 0.9)
	for i := points / 2; i < points; i++ {
		offsets[i] = 2500
	}
	// The join lies between these two points, and both of them are noise.
	qualities[points/2-1] = 0.05
	qualities[points/2] = 0.05

	pw := BuildSegments(denseGrid(points, 45, 60), offsets, qualities, DefaultPiecewisePolicy(), DefaultThresholds)
	if len(pw.Segments) != 1 {
		t.Fatalf("segments = %d, want 1: an unconfident boundary must merge, not split (%+v)",
			len(pw.Segments), pw.Segments)
	}
	if pw.RejectedReason == "" {
		t.Fatal("rejected_reason is empty: the operator would never learn why the cut was not applied")
	}
	// The reason names the join it refused, to within the resolution the plan
	// was built at: dropping the two unreadable points leaves the neighbouring
	// centres a step either side of 3600.
	var at float64
	if _, err := fmt.Sscanf(pw.RejectedReason, "boundary at %f s", &at); err != nil {
		t.Fatalf("rejected_reason = %q, want it to open with the boundary's time: %v", pw.RejectedReason, err)
	}
	if got := math.Abs(at - 3600); got > 45 {
		t.Fatalf("rejected_reason names %v s, want the join at ~3600 s", at)
	}
	if !strings.Contains(pw.RejectedReason, "not confident") {
		t.Fatalf("rejected_reason = %q, want it to say the boundary is not confident", pw.RejectedReason)
	}
	if got := math.Abs(pw.DominantOffsetMS - 1250); got > 1e-9 {
		t.Fatalf("dominant offset = %v, want the median of the surviving points (1250)", pw.DominantOffsetMS)
	}
}

// TestBuildSegmentsMergesShortRuns: a three-point island is a measurement
// accident far more often than an edit, so MinSegmentSec merges it away.
func TestBuildSegmentsMergesShortRuns(t *testing.T) {
	const points = 30
	offsets := make([]float64, points)
	for i := 14; i <= 16; i++ {
		offsets[i] = 2500
	}
	pw := BuildSegments(denseGrid(points, 45, 60), offsets, flatQualities(points, 0.9), DefaultPiecewisePolicy(), DefaultThresholds)
	if len(pw.Segments) != 1 {
		t.Fatalf("segments = %d, want 1: a %v s island is shorter than min_segment_sec (%v s)",
			len(pw.Segments), 2*45.0, DefaultPiecewisePolicy().MinSegmentSec)
	}
	if pw.Segments[0].Points != points {
		t.Fatalf("merged segment covers %d points, want %d", pw.Segments[0].Points, points)
	}
}

// TestBuildSegmentsDropsNothingWhenQualityIsHigh: the quality gate only removes
// points that cannot testify.
func TestBuildSegmentsDropsNothingWhenQualityIsHigh(t *testing.T) {
	const points = 160
	offsets := make([]float64, points)
	for i := points / 2; i < points; i++ {
		offsets[i] = 2500
	}
	pw := BuildSegments(denseGrid(points, 45, 60), offsets, flatQualities(points, 0.9), DefaultPiecewisePolicy(), DefaultThresholds)
	covered := 0
	for _, segment := range pw.Segments {
		covered += segment.Points
	}
	if covered != points {
		t.Fatalf("segments cover %d points, want all %d: nothing was below the quality floor", covered, points)
	}
}

// TestBuildSegmentsCapsSegments: a jagged timeline that wants five segments is
// cut down to MaxSegments by dropping the cheapest splits, not by refusing.
func TestBuildSegmentsCapsSegments(t *testing.T) {
	const plateaus = 5
	const perPlateau = 40
	points := plateaus * perPlateau
	offsets := make([]float64, points)
	for i := range offsets {
		offsets[i] = float64(i/perPlateau) * 500
	}
	policy := DefaultPiecewisePolicy()
	policy.MaxSegments = 3
	pw := BuildSegments(denseGrid(points, 45, 60), offsets, flatQualities(points, 0.9), policy, DefaultThresholds)
	if len(pw.Segments) != 3 {
		t.Fatalf("segments = %d, want the cap 3", len(pw.Segments))
	}
	if len(pw.Boundaries) != 2 {
		t.Fatalf("boundaries = %d, want 2", len(pw.Boundaries))
	}
	if pw.RejectedReason != "" {
		t.Fatalf("rejected_reason = %q, want empty: a cap that still leaves three segments is not a rejection",
			pw.RejectedReason)
	}
}

// TestBuildSegmentsRespectsMinStepMS: 2 x StableSpreadMS is the smallest real
// step, so 50 ms is jitter and 100 ms is a cut.
func TestBuildSegmentsRespectsMinStepMS(t *testing.T) {
	const points = 160
	if got := DefaultThresholds.MinStepMS(); math.Abs(got-80) > 1e-9 {
		t.Fatalf("MinStepMS() = %v, want 80 (2 x the 40 ms stable band)", got)
	}
	for _, tc := range []struct {
		step float64
		want int
	}{
		{step: 50, want: 1},
		{step: 100, want: 2},
	} {
		offsets := make([]float64, points)
		for i := points / 2; i < points; i++ {
			offsets[i] = tc.step
		}
		pw := BuildSegments(denseGrid(points, 45, 60), offsets, flatQualities(points, 0.9), DefaultPiecewisePolicy(), DefaultThresholds)
		if len(pw.Segments) != tc.want {
			t.Fatalf("a %v ms step produced %d segments, want %d (%q)",
				tc.step, len(pw.Segments), tc.want, pw.RejectedReason)
		}
	}
}

// TestBuildSegmentsNeedsEnoughPoints: six confident checkpoints are the shipped
// plan, and the shipped plan cannot be segmented.
func TestBuildSegmentsNeedsEnoughPoints(t *testing.T) {
	const points = 6
	offsets := make([]float64, points)
	for i := range offsets {
		offsets[i] = 400
	}
	pw := BuildSegments(denseGrid(points, 300, 60), offsets, flatQualities(points, 0.9), DefaultPiecewisePolicy(), DefaultThresholds)
	if pw.RejectedReason != reasonTooFewPoints {
		t.Fatalf("rejected_reason = %q, want %q", pw.RejectedReason, reasonTooFewPoints)
	}
	if len(pw.Segments) != 0 {
		t.Fatalf("segments = %d, want none: there is no evidence to describe", len(pw.Segments))
	}
	if pw.DominantOffsetMS != 400 {
		t.Fatalf("dominant offset = %v, want the median of the points that did testify (400)", pw.DominantOffsetMS)
	}
}

// TestPiecewisePolicyDefaults pins the shipped block and the normalisation that
// makes an absent one legal: off, and the documented numbers behind it.
func TestPiecewisePolicyDefaults(t *testing.T) {
	shipped := DefaultPiecewisePolicy()
	if shipped.Enabled {
		t.Fatal("the shipped piecewise policy is enabled: D5 makes segmentation opt-in")
	}
	want := PiecewisePolicy{StepSec: 45, WindowSec: 60, SplitPenaltyMS: 250, MinSegmentSec: 120, MaxSegments: 12}
	if shipped.Normalised() != want {
		t.Fatalf("DefaultPiecewisePolicy() = %+v, want %+v", shipped, want)
	}
	// The zero value means the same thing: off at the documented numbers, which
	// is why an absent settings block needs no migration and no flag (D1).
	if zero := (PiecewisePolicy{}); zero.Normalised() != want {
		t.Fatalf("zero policy normalises to %+v, want %+v", zero.Normalised(), want)
	}
	if !(PiecewisePolicy{}).IsZero() || !shipped.IsZero() {
		t.Fatal("the shipped policy is not its own zero value: every report would grow a piecewise key")
	}
	enabled := shipped
	enabled.Enabled = true
	if enabled.IsZero() {
		t.Fatal("an enabled policy is reported as zero: the report would not say what it ran")
	}
	tuned := shipped
	tuned.StepSec = 30
	if tuned.IsZero() {
		t.Fatal("a tuned policy is reported as zero: the report would not say what it ran")
	}
}

// TestPiecewisePolicyValidation pins the sync.strategy ranges, message and
// all: the settings layer prefixes them with the section key.
func TestPiecewisePolicyValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*PiecewisePolicy)
		wantErr string
	}{
		{"shipped", func(*PiecewisePolicy) {}, ""},
		{"enabled at the shipped numbers", func(p *PiecewisePolicy) { p.Enabled = true }, ""},
		{"step too small", func(p *PiecewisePolicy) { p.StepSec = 5 }, "piecewise.step_sec must be 15..300"},
		{"step too large", func(p *PiecewisePolicy) { p.StepSec = 301 }, "piecewise.step_sec must be 15..300"},
		{"window too small", func(p *PiecewisePolicy) { p.WindowSec = 19 }, "piecewise.window_sec must be 20..190"},
		{"window too large", func(p *PiecewisePolicy) { p.WindowSec = 200 }, "piecewise.window_sec must be 20..190"},
		{"window wider than two steps", func(p *PiecewisePolicy) { p.StepSec = 20; p.WindowSec = 60 }, "piecewise.window_sec must be at most twice step_sec"},
		{"penalty too small", func(p *PiecewisePolicy) { p.SplitPenaltyMS = 10 }, "piecewise.split_penalty_ms must be 50..5000"},
		{"segment too short", func(p *PiecewisePolicy) { p.MinSegmentSec = 10 }, "piecewise.min_segment_sec must be 30..3600"},
		{"too few segments", func(p *PiecewisePolicy) { p.MaxSegments = 1 }, "piecewise.max_segments must be 2..40"},
		{"window past the search bound", func(p *PiecewisePolicy) { p.Enabled = true; p.StepSec = 90; p.WindowSec = 180 }, "when piecewise is enabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := DefaultPiecewisePolicy()
			tc.mutate(&policy)
			err := policy.validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("validate() = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("validate() = nil, want %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("validate() = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
