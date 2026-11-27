package analyze

import (
	"errors"
	"math"
	"testing"
)

// settledWindowSec is the analysed window length with the settled defaults
// (max_offset_sec 180 + CheckWindowMarginSec 10).
const settledWindowSec = 190.0

// planFor plans one checkpoint of a pair whose two inputs are both dur long.
func planFor(t *testing.T, dur float64, pct int, window float64) Window {
	t.Helper()
	timing := Timing{
		Target: SourceTiming{DurationSec: dur, OriginKnown: true},
		Donor:  SourceTiming{DurationSec: dur, OriginKnown: true},
	}
	plan, err := PlanWindows(timing, []int{pct}, window)
	if err != nil {
		t.Fatalf("PlanWindows(dur %v, pct %d, window %v): %v", dur, pct, window, err)
	}
	if len(plan) != 1 {
		t.Fatalf("plan length = %d, want 1", len(plan))
	}
	return plan[0]
}

// TestPlanWindows is the former TestSliceWindow, ported to the planner and
// extended with the cases the plan has to get right on its own: a duration
// that is not a whole number of seconds, a start+window that overshoots the
// duration by exactly one sample (the clamp absorbs it, the escape guard must
// not fire), and the escape guard itself.
//
// Every case also asserts the invariant the planner exists to keep: the window
// starts inside the file, is at least one sample long, and does not end past
// the duration by more than the one sample snapping can explain.
func TestPlanWindows(t *testing.T) {
	cases := []struct {
		name   string
		dur    float64
		pct    int
		window float64
		start  float64
		length float64
	}{
		{"head clamp", 600, 10, 190, 0, 190},
		{"centred", 600, 55, 190, 235, 190},
		{"tail clamp", 600, 85, 190, 410, 190},
		{"file shorter than window", 100, 50, 190, 0, 100},
		{"file shorter at head", 120, 10, 190, 0, 120},
		{"file equals window", 190, 50, 190, 0, 190},
		{"file just short of window", 189, 99, 190, 0, 189},
		{"window below file", 600, 1, 150, 0, 150},
		// The real fixture length, which is not a whole number of seconds:
		// the start snaps to the nearest sample instead of being carried as
		// 2077.4928 s that no decoder can address.
		{"duration not a whole number of seconds", 5431.232, 40, 190, 2077.49275, 190},
		{"fractional duration, tail clamp", 5431.232, 99, 190, 5241.232, 190},
		// start+window lands exactly one sample past dur, so the tail clamp
		// pulls the start back to dur-window: clamped, NOT an error, because
		// one sample is what snapping is allowed to move a boundary.
		{"start+window one sample past dur", 9499.9875, 99, 190, 9309.9875, 190},
		// Snapping alone takes the end one sample past the raw duration: still
		// inside the guard's slack, and the plan says so.
		{"snapped end one sample past dur", 190.00015, 85, 190, 0.000125, 190},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := planFor(t, tc.dur, tc.pct, tc.window)
			if w.PositionPct != tc.pct {
				t.Fatalf("position = %d, want %d", w.PositionPct, tc.pct)
			}
			if math.Abs(w.FilmStartSec-tc.start) > 1e-12 || math.Abs(w.LenSec-tc.length) > 1e-12 {
				t.Fatalf("PlanWindows(%v, %d, %v) = (%v, %v), want (%v, %v)",
					tc.dur, tc.pct, tc.window, w.FilmStartSec, w.LenSec, tc.start, tc.length)
			}
			if w.FilmStartSec < 0 || w.LenSec <= 0 {
				t.Fatalf("window (%v, %v) is not inside the file", w.FilmStartSec, w.LenSec)
			}
			if w.LenSec*analysisSampleRate < 1 {
				t.Fatalf("length %v is under one sample", w.LenSec)
			}
			if end := w.FilmStartSec + w.LenSec; end > tc.dur+1.0/analysisSampleRate+1e-9 {
				t.Fatalf("window ends at %v, past dur %v by more than snapping", end, tc.dur)
			}
		})
	}

	// The clamp above cannot produce an escaping window: the tail clamp puts
	// the start at dur-window, and snapping moves each boundary by at most
	// half a sample, so start+len lands at most exactly one sample past dur —
	// which the guard tolerates by design, as the two cases above show. A
	// genuinely escaping plan is what a per-source clamp reintroduces, so the
	// guard is exercised directly here rather than through an input the clamp
	// cannot reach.
	t.Run("escaping plan is refused", func(t *testing.T) {
		if windowEscapes(0, 1520001, 1520000) {
			t.Fatal("one sample of slack is a rounding edge, not an escape")
		}
		if !windowEscapes(0, 1520002, 1520000) {
			t.Fatal("a plan two samples past the duration must be refused")
		}
	})
}

// TestPlanWindowsRejectsUnusableInput pins the "cannot plan" shapes: the
// planner is pure data in, pure data out, so it validates its own input rather
// than trusting that Config.validate ran first.
func TestPlanWindowsRejectsUnusableInput(t *testing.T) {
	ok := SourceTiming{DurationSec: 600, OriginKnown: true}
	cases := []struct {
		name   string
		timing Timing
		pcts   []int
		window float64
	}{
		{"no duration", Timing{}, []int{10}, 190},
		{"donor duration missing", Timing{Target: ok}, []int{10}, 190},
		{"negative duration", Timing{Target: ok, Donor: SourceTiming{DurationSec: -1, OriginKnown: true}}, []int{10}, 190},
		{"NaN duration", Timing{Target: SourceTiming{DurationSec: math.NaN(), OriginKnown: true}, Donor: ok}, []int{10}, 190},
		{"zero window", Timing{Target: ok, Donor: ok}, []int{10}, 0},
		{"negative window", Timing{Target: ok, Donor: ok}, []int{10}, -190},
		{"infinite window", Timing{Target: ok, Donor: ok}, []int{10}, math.Inf(1)},
		{"no positions", Timing{Target: ok, Donor: ok}, nil, 190},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := PlanWindows(tc.timing, tc.pcts, tc.window)
			if !errors.Is(err, ErrWindowPlan) {
				t.Fatalf("PlanWindows = (%v, %v), want ErrWindowPlan", plan, err)
			}
			if plan != nil {
				t.Fatalf("PlanWindows returned %d windows alongside the error", len(plan))
			}
		})
	}
}

// TestPlanWindowsDistinctOnRealDuration is the finding-1 regression, at the
// planner. The old code derived the duration from the 190 s window WAV that a
// windowed decode never produces, so all six percentages clamped to 0 and the
// job analysed the same audio six times: six identical slice_start_sec values,
// max_drift 0, verdict ok. With the real duration (5431.232 s, the donor of
// the Shrek pair) the six positions must be pairwise distinct and ascending,
// and every one must fit inside the shorter input.
func TestPlanWindowsDistinctOnRealDuration(t *testing.T) {
	const dur = 5431.232
	timing := Timing{
		Target: SourceTiming{DurationSec: 5431.772, OriginKnown: true}, // the 2160p remux
		Donor:  SourceTiming{DurationSec: dur, OriginKnown: true},      // the staged .mka, 0.54 s shorter
	}
	plan, err := PlanWindows(timing, DefaultCheckpointPositionsPct, settledWindowSec)
	if err != nil {
		t.Fatalf("PlanWindows: %v", err)
	}
	if len(plan) != len(DefaultCheckpointPositionsPct) {
		t.Fatalf("plan length = %d, want %d", len(plan), len(DefaultCheckpointPositionsPct))
	}

	for i, w := range plan {
		if w.PositionPct != DefaultCheckpointPositionsPct[i] {
			t.Fatalf("plan[%d] position = %d, want %d", i, w.PositionPct, DefaultCheckpointPositionsPct[i])
		}
		if end := w.FilmStartSec + w.LenSec; end > dur+1.0/analysisSampleRate {
			t.Fatalf("checkpoint %d%% ends at %v, past the shorter input %v", w.PositionPct, end, dur)
		}
		for j := 0; j < i; j++ {
			if plan[j].FilmStartSec == w.FilmStartSec {
				t.Fatalf("checkpoints %d%% and %d%% both start at %v: the finding-1 signature",
					plan[j].PositionPct, w.PositionPct, w.FilmStartSec)
			}
			if plan[j].FilmStartSec > w.FilmStartSec {
				t.Fatalf("checkpoints are not ascending: %d%% at %v before %d%% at %v",
					plan[j].PositionPct, plan[j].FilmStartSec, w.PositionPct, w.FilmStartSec)
			}
		}
	}

	// Snapped to a whole sample, the first and the last of the settled six.
	if got, want := plan[0].FilmStartSec, 448.12325; got != want {
		t.Fatalf("first FilmStartSec = %v, want %v", got, want)
	}
	if got, want := plan[len(plan)-1].FilmStartSec, 4521.54725; got != want {
		t.Fatalf("last FilmStartSec = %v, want %v", got, want)
	}
}

// TestPlanWindowsCollapseWhenTheDurationIsTheWindow is the companion of
// [TestPlanWindowsDistinctOnRealDuration], and the reason that test means
// something: hand the planner the *window length* in place of the film
// duration — exactly what deriving the duration from a 190 s window WAV did —
// and all six checkpoints collapse onto the same window. If anyone reintroduces
// a derived-from-WAV duration, this test still passes and the distinctness test
// above goes red, which is the point.
func TestPlanWindowsCollapseWhenTheDurationIsTheWindow(t *testing.T) {
	wrong := Timing{
		Target: SourceTiming{DurationSec: settledWindowSec, OriginKnown: true},
		Donor:  SourceTiming{DurationSec: settledWindowSec, OriginKnown: true},
	}
	plan, err := PlanWindows(wrong, DefaultCheckpointPositionsPct, settledWindowSec)
	if err != nil {
		t.Fatalf("PlanWindows: %v", err)
	}
	if len(plan) != len(DefaultCheckpointPositionsPct) {
		t.Fatalf("plan length = %d, want %d", len(plan), len(DefaultCheckpointPositionsPct))
	}
	for i, w := range plan[1:] {
		if w.FilmStartSec != plan[0].FilmStartSec || w.LenSec != plan[0].LenSec {
			t.Fatalf("checkpoint %d%% = (%v, %v), want the identical (%v, %v) the old bug produced",
				w.PositionPct, w.FilmStartSec, w.LenSec, plan[0].FilmStartSec, plan[0].LenSec)
		}
		if w.FilmStartSec != 0 || w.LenSec != settledWindowSec {
			t.Fatalf("checkpoint %d%% = (%v, %v), want the whole 190 s file", i+1, w.FilmStartSec, w.LenSec)
		}
	}
}

// TestWindowIsSampleExact is the point of the snapping: the planner, the
// decoder and the engine all convert seconds to samples, so every number the
// plan publishes must already be a whole number of samples at the analysis
// rate. Anything else makes the one-sample parity bound unreachable for the
// wrong reason — three roundings of the same seconds disagreeing by 62.5 µs.
func TestWindowIsSampleExact(t *testing.T) {
	// Corrections on sample boundaries, so "seek is a whole sample" is a
	// statement about the planner and not about the fixture.
	timing := Timing{
		Target: SourceTiming{
			DurationSec:       5431.772,
			ContainerStartSec: 0.021, // 168 samples
			TrackStartSec:     0.5,   // 4 000 samples
			OriginKnown:       true,
		},
		Donor: SourceTiming{
			DurationSec:       5431.232,
			ContainerStartSec: 0,
			TrackStartSec:     0,
			OriginKnown:       true,
		},
	}
	durations := []Timing{timing, {
		Target: SourceTiming{DurationSec: 600, OriginKnown: true},
		Donor:  SourceTiming{DurationSec: 189, OriginKnown: true},
	}}

	for _, dur := range durations {
		plan, err := PlanWindows(dur, DefaultCheckpointPositionsPct, settledWindowSec)
		if err != nil {
			t.Fatalf("PlanWindows: %v", err)
		}
		for _, w := range plan {
			for _, field := range []struct {
				name string
				sec  float64
			}{
				{"FilmStartSec", w.FilmStartSec},
				{"LenSec", w.LenSec},
				{"TargetSeekSec", w.TargetSeekSec},
				{"DonorSeekSec", w.DonorSeekSec},
			} {
				// Compare in seconds against the nearest whole sample: the
				// tolerance is 1 ns, which is 8e-6 of a sample and six orders
				// of magnitude below the 62.5 µs this snapping removes. It is
				// not tighter because seek = film + correction adds two
				// numbers that are not exactly representable in binary.
				samples := math.Round(field.sec * analysisSampleRate)
				if math.Abs(field.sec-samples/analysisSampleRate) > 1e-9 {
					t.Fatalf("checkpoint %d%%: %s = %v s = %.9f samples, want a whole sample",
						w.PositionPct, field.name, field.sec, field.sec*analysisSampleRate)
				}
			}
		}
	}
}

// TestSeekCorrection pins the seek formula: seek = film + (trackStart -
// containerStart), computed from the same FilmStartSec for both sources, with
// no independent clamping. A source whose origin the container did not state is
// recorded as [UnknownOrigin] for exactly that source instead of being given a
// silent correction of 0.
func TestSeekCorrection(t *testing.T) {
	const dur = 5431.232
	cases := []struct {
		name         string
		target       SourceTiming
		donor        SourceTiming
		wantTarget   float64 // added to the film start
		wantDonor    float64
		wantUnknown  UnknownOrigin
		wantUnusable bool
	}{
		{
			name:   "audio only: both origins coincide with the container",
			target: SourceTiming{DurationSec: dur, OriginKnown: true},
			donor:  SourceTiming{DurationSec: dur, OriginKnown: true},
			// seek == film start, so the whole-file and windowed reads agree.
			wantUnknown: UnknownOriginNone,
		},
		{
			name: "muxed target whose reference stream starts at 0.5 s",
			target: SourceTiming{
				DurationSec: 5431.772, ContainerStartSec: 0, TrackStartSec: 0.5, OriginKnown: true,
			},
			donor:       SourceTiming{DurationSec: dur, OriginKnown: true},
			wantTarget:  0.5,
			wantUnknown: UnknownOriginNone,
		},
		{
			name:   "target start unknown",
			target: SourceTiming{DurationSec: 5431.772},
			donor: SourceTiming{
				DurationSec: dur, ContainerStartSec: 0, TrackStartSec: 0.5, OriginKnown: true,
			},
			wantDonor:   0.5,
			wantUnknown: UnknownOriginTarget,
		},
		{
			name:        "donor start unknown",
			target:      SourceTiming{DurationSec: 5431.772, OriginKnown: true},
			donor:       SourceTiming{DurationSec: dur},
			wantUnknown: UnknownOriginDonor,
		},
		{
			name:        "both starts unknown",
			target:      SourceTiming{DurationSec: 5431.772},
			donor:       SourceTiming{DurationSec: dur},
			wantUnknown: UnknownOriginBoth,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := PlanWindows(Timing{Target: tc.target, Donor: tc.donor}, []int{55}, settledWindowSec)
			if err != nil {
				t.Fatalf("PlanWindows: %v", err)
			}
			w := plan[0]
			if w.UnknownOrigin != tc.wantUnknown {
				t.Fatalf("UnknownOrigin = %q, want %q", w.UnknownOrigin, tc.wantUnknown)
			}
			if got, want := w.TargetSeekSec, w.FilmStartSec+tc.wantTarget; math.Abs(got-want) > 1e-12 {
				t.Fatalf("target seek = %v, want film %v + %v", got, w.FilmStartSec, tc.wantTarget)
			}
			if got, want := w.DonorSeekSec, w.FilmStartSec+tc.wantDonor; math.Abs(got-want) > 1e-12 {
				t.Fatalf("donor seek = %v, want film %v + %v", got, w.FilmStartSec, tc.wantDonor)
			}
			// The identity that makes the correction metadata rather than a
			// clamp: each seek is the film start plus its own correction, and
			// an unstated origin contributes no correction at all.
			if got := w.DonorSeekSec - w.FilmStartSec; math.Abs(got-tc.wantDonor) > 1e-12 {
				t.Fatalf("donor correction = %v, want %v", got, tc.wantDonor)
			}
		})
	}
}

// TestPairClampIsJoint is the finding-3 negative control.
//
// Joint clamp first: the duration base is min(target, donor), so a pair whose
// target is 0.54 s longer than the donor plans the *same* windows as the donor
// alone — no window is shifted to fit either file's own EOF.
//
// Then the control. pairEngine is an engine stub that does what the engine
// does with the start it is given: it reads each file from that position and
// reports targetTime - donorTime. Feeding it the plan's two seeks makes a
// deliberately donor-shifted pair's offset move by exactly the shift; feeding
// it one start for both files — the conflation finding 3 is about — makes the
// shift vanish. The invariant, not a comment, is what keeps the shift where it
// belongs: in the seek, out of offset_ms.
func TestPairClampIsJoint(t *testing.T) {
	const (
		dur          = 5431.232
		trueOffsetMS = 1200.0 // what the two files' audio is really offset by
		donorShift   = 0.5    // the donor's stream starts 0.5 s into its container
	)
	same := Timing{
		Target: SourceTiming{DurationSec: dur, OriginKnown: true},
		Donor:  SourceTiming{DurationSec: dur, OriginKnown: true},
	}
	longerTarget := Timing{
		Target: SourceTiming{DurationSec: 5431.772, OriginKnown: true},
		Donor:  SourceTiming{DurationSec: dur, OriginKnown: true},
	}
	shiftedDonor := same
	shiftedDonor.Donor.TrackStartSec = donorShift

	plan, planLong, planShifted := mustPlan(t, same), mustPlan(t, longerTarget), mustPlan(t, shiftedDonor)
	for i := range plan {
		if planLong[i].FilmStartSec != plan[i].FilmStartSec || planLong[i].LenSec != plan[i].LenSec {
			t.Fatalf("checkpoint %d%%: a longer target moved the window to (%v, %v), want (%v, %v)",
				plan[i].PositionPct, planLong[i].FilmStartSec, planLong[i].LenSec,
				plan[i].FilmStartSec, plan[i].LenSec)
		}
		// The one per-source difference that legitimately exists: the seek.
		if got := planShifted[i].DonorSeekSec - planShifted[i].FilmStartSec; math.Abs(got-donorShift) > 1e-12 {
			t.Fatalf("checkpoint %d%%: donor correction = %v, want %v", plan[i].PositionPct, got, donorShift)
		}
		if planShifted[i].TargetSeekSec != planShifted[i].FilmStartSec {
			t.Fatalf("checkpoint %d%%: target seek = %v, want the film start %v",
				plan[i].PositionPct, planShifted[i].TargetSeekSec, planShifted[i].FilmStartSec)
		}
	}

	joint := pairEngine{trueOffsetMS: trueOffsetMS}
	if got, want := joint.run(planShifted[3])-joint.run(plan[3]), -donorShift*1000; math.Abs(got-want) > 1e-12 {
		t.Fatalf("offset_ms moved by %v ms with per-source seeks, want exactly %v ms", got, want)
	}
	conflated := pairEngine{trueOffsetMS: trueOffsetMS, singleStart: true}
	if got := conflated.run(planShifted[3]) - conflated.run(plan[3]); got != 0 {
		t.Fatalf("offset_ms moved by %v ms with one start for both files, want 0: "+
			"that is the shift a per-source clamp would hide", got)
	}
}

// mustPlan plans the settled six windows or fails the test.
func mustPlan(t *testing.T, timing Timing) []Window {
	t.Helper()
	plan, err := PlanWindows(timing, DefaultCheckpointPositionsPct, settledWindowSec)
	if err != nil {
		t.Fatalf("PlanWindows: %v", err)
	}
	return plan
}

// pairEngine models the engine's answer over one planned window: the two files
// carry the same programme, offset by trueOffsetMS, and each is read from the
// start it is given, so the reported offset is targetTime - donorTime.
//
// With singleStart it applies ONE start to both files, which is what a job
// carrying a single SliceStartSec does — and what makes a per-source shift
// invisible in the result.
type pairEngine struct {
	trueOffsetMS float64
	singleStart  bool
}

func (e pairEngine) run(w Window) float64 {
	targetStart, donorStart := w.TargetSeekSec, w.DonorSeekSec
	if e.singleStart {
		donorStart = targetStart
	}
	return e.trueOffsetMS + (targetStart-donorStart)*1000
}
