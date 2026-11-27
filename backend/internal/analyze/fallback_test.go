package analyze

import (
	"math"
	"testing"
)

// The policy's own numbers, so the cases read as the table in the plan: the
// settled 190 s analysis window is the engine's search bound, and a peak
// reported at that bound is what "boundary" means.
const (
	testMaxLagMS      = 190 * 1000.0
	testAcceptBoundMS = DefaultMaxOffsetSec * 1000.0
	testQuality       = 0.55 // inside the real 0.14..0.55 band
)

// fallbackWindows returns the six settled checkpoint windows of the donor
// fixture length: unequal in nothing but position, which is all the policy
// uses. Building them through PlanWindows rather than by hand keeps the test
// on the same windows a job analyses.
func fallbackWindows(t *testing.T, n int) []Window {
	t.Helper()
	timing := Timing{
		Target: SourceTiming{DurationSec: 5431.232, OriginKnown: true},
		Donor:  SourceTiming{DurationSec: 5431.232, OriginKnown: true},
	}
	pcts := DefaultCheckpointPositionsPct
	if n > len(pcts) {
		t.Fatalf("fallbackWindows(%d): the settled plan has %d checkpoints", n, len(pcts))
	}
	pcts = pcts[:n]
	plan, err := PlanWindows(timing, pcts, 190)
	if err != nil {
		t.Fatalf("PlanWindows: %v", err)
	}
	if len(plan) != n {
		t.Fatalf("plan length = %d, want %d", len(plan), n)
	}
	return plan
}

// fillQualities returns n copies of q.
func fillQualities(n int, q float64) []float64 {
	qualities := make([]float64, n)
	for i := range qualities {
		qualities[i] = q
	}
	return qualities
}

// suspectSummary is one expected row of a [Suspects] result, flattened so a
// table case can state it inline.
type suspectSummary struct {
	index  int
	reason string
}

func summarise(suspects []Suspect) []suspectSummary {
	if len(suspects) == 0 {
		return nil
	}
	out := make([]suspectSummary, len(suspects))
	for i, s := range suspects {
		out[i] = suspectSummary{index: s.Index, reason: s.Reason}
	}
	return out
}

func sameSuspects(got []Suspect, want []suspectSummary) bool {
	summary := summarise(got)
	if len(summary) != len(want) {
		return false
	}
	for i := range want {
		if summary[i] != want[i] {
			return false
		}
	}
	return true
}

// TestFallbackSuspects pins the shape rule against the patterns the policy
// exists for.
//
// The two legitimate shapes (drift, cut) are the point of the whole file: a
// rule based on distance from the median flags them, and this one must not.
// The spike rows are the ones a least-squares line could not see — see
// fitLine — so they are asserted here rather than only in prose.
func TestFallbackSuspects(t *testing.T) {
	cases := []struct {
		name      string
		offsets   []float64
		qualities []float64
		want      []suspectSummary
	}{
		{
			name:    "constant offset",
			offsets: []float64{-340, -340, -340, -340, -340, -340},
		},
		{
			name:    "mid-movie cut, both sides flat",
			offsets: []float64{-340, -340, -340, -260, -260, -260},
		},
		{
			name:    "mid-movie cut, 1 s step",
			offsets: []float64{-340, -340, -340, 660, 660, 660},
		},
		{
			name:    "linear ramp",
			offsets: []float64{-340, -332, -324, -316, -308, -300},
		},
		{
			name:    "one spike",
			offsets: []float64{-340, -340, 800, -340, -340, -340},
			want:    []suspectSummary{{2, SuspectResidual}},
		},
		{
			name:    "two spikes",
			offsets: []float64{-340, 800, -340, -340, 800, -340},
			want:    []suspectSummary{{1, SuspectResidual}, {4, SuspectResidual}},
		},
		{
			name:    "spike at the first window is a step edge, not a spike",
			offsets: []float64{800, -340, -340, -340, -340, -340},
		},
		{
			name:    "boundary peak",
			offsets: []float64{-340, -340, -testMaxLagMS, -340, -340, -340},
			want:    []suspectSummary{{2, SuspectBoundaryPeak}},
		},
		{
			name:      "boundary peak outranks its own weak quality",
			offsets:   []float64{-340, -340, -testMaxLagMS, -340, -340, -340},
			qualities: []float64{testQuality, testQuality, 0.04, testQuality, testQuality, testQuality},
			want:      []suspectSummary{{2, SuspectBoundaryPeak}},
		},
		{
			name:      "low quality in a perfectly flat timeline",
			offsets:   []float64{-340, -340, -340, -340, -340, -340},
			qualities: []float64{testQuality, testQuality, 0.04, testQuality, testQuality, testQuality},
			want:      []suspectSummary{{2, SuspectLowQuality}},
		},
		{
			name:      "quality at the floor is trusted",
			offsets:   []float64{-340, -340, -340, -340, -340, -340},
			qualities: []float64{testQuality, testQuality, LowQualityThreshold, testQuality, testQuality, testQuality},
		},
		{
			name:      "quality just below the floor is not",
			offsets:   []float64{-340, -340, -340, -340, -340, -340},
			qualities: []float64{testQuality, testQuality, math.Nextafter(LowQualityThreshold, 0), testQuality, testQuality, testQuality},
			want:      []suspectSummary{{2, SuspectLowQuality}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			windows := fallbackWindows(t, len(tc.offsets))
			qualities := tc.qualities
			if qualities == nil {
				qualities = fillQualities(len(tc.offsets), testQuality)
			}
			got := Suspects(windows, tc.offsets, qualities, testMaxLagMS)
			if !sameSuspects(got, tc.want) {
				t.Fatalf("Suspects(%v) = %+v, want %+v", tc.offsets, summarise(got), tc.want)
			}
		})
	}
}

// TestFpsDriftIsNotASuspect is the acceptance case the policy is designed
// around: an FPS difference moves the offset monotonically across the film,
// which looks like a wandering outlier to a distance-from-the-median rule.
// It is not a decode failure — [classify] calls it drifting and the mux stage
// reasons about it — so no window may be re-decoded or discarded because of
// it.
func TestFpsDriftIsNotASuspect(t *testing.T) {
	windows := fallbackWindows(t, len(DefaultCheckpointPositionsPct))
	cases := []struct {
		name    string
		offsets []float64
	}{
		// The settled drift shape: 40 ms end to end, which is exactly the
		// band classify calls "drifting" rather than "stable".
		{"40 ms ramp", []float64{-340, -332, -324, -316, -308, -300}},
		// A much steeper ramp: still one straight line, still not a decode
		// failure, and still nothing to retry.
		{"240 ms ramp", []float64{-340, -292, -244, -196, -148, -100}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Suspects(windows, tc.offsets, fillQualities(len(tc.offsets), testQuality), testMaxLagMS)
			if len(got) != 0 {
				t.Fatalf("Suspects(ramp %v) = %+v, want none: drift is a verdict, not a decode failure",
					tc.offsets, got)
			}
			// The shape really is drift: assert the verdict the policy is
			// preserving, so a "fix" that flattened the ramp cannot pass here.
			verdict, _, maxDrift := classify(tc.offsets, fillQualities(len(tc.offsets), testQuality), testAcceptBoundMS)
			if verdict != VerdictDrifting {
				t.Fatalf("classify(%v) = %q (max drift %v ms), want %q", tc.offsets, verdict, maxDrift, VerdictDrifting)
			}
		})
	}
}

// TestMidMovieCutIsNotASuspect is the other acceptance case: the donor has an
// insert, so the offset steps once by more than the stable band and stays
// there. A spike rule with no neighbour test flags both windows beside the
// step; this policy must flag none of them, and must leave the drifting
// verdict to [classify].
func TestMidMovieCutIsNotASuspect(t *testing.T) {
	offsets := []float64{-340, -340, -340, -260, -260, -260}
	windows := fallbackWindows(t, len(offsets))
	qualities := fillQualities(len(offsets), testQuality)

	if got := Suspects(windows, offsets, qualities, testMaxLagMS); len(got) != 0 {
		t.Fatalf("Suspects(cut %v) = %+v, want none: a cut is a verdict, not a decode failure", offsets, got)
	}
	verdict, _, maxDrift := classify(offsets, qualities, testAcceptBoundMS)
	if verdict != VerdictDrifting {
		t.Fatalf("classify(cut %v) = %q (max drift %v ms), want %q", offsets, verdict, maxDrift, VerdictDrifting)
	}
}

// TestFallbackOneSpikeIsRetried is the acceptance case for the
// residual reason end to end at the policy level: one spike, one suspect, and
// the retried window accepted only if it lands back on the timeline the clean
// windows describe. The reference deliberately excludes the suspect — a fit
// containing the value under test would accept it.
func TestFallbackOneSpikeIsRetried(t *testing.T) {
	offsets := []float64{-340, -340, 800, -340, -340, -340}
	windows := fallbackWindows(t, len(offsets))

	suspects := Suspects(windows, offsets, fillQualities(len(offsets), testQuality), testMaxLagMS)
	if len(suspects) != 1 || suspects[0].Index != 2 || suspects[0].Reason != SuspectResidual {
		t.Fatalf("Suspects(spike) = %+v, want exactly one residual suspect at index 2", suspects)
	}
	if math.Abs(suspects[0].ResidualMS-1140) > 1e-9 {
		t.Fatalf("residual = %v ms, want 1140", suspects[0].ResidualMS)
	}
	if RequiresFullDecode(suspects) {
		t.Fatal("one suspect must be re-decoded once, not escalated to a full decode")
	}

	// The reference: the five windows that were never suspect.
	clean := make([]Window, 0, len(windows)-1)
	cleanOffsets := make([]float64, 0, len(windows)-1)
	for i := range windows {
		if i == suspects[0].Index {
			continue
		}
		clean = append(clean, windows[i])
		cleanOffsets = append(cleanOffsets, offsets[i])
	}

	if !Accepts(clean, cleanOffsets, windows[2], -341) {
		t.Fatal("Accepts(-341) = false, want true: a retried spike that lands on the line is a repaired window")
	}
	if Accepts(clean, cleanOffsets, windows[2], 700) {
		t.Fatal("Accepts(+700) = true, want false: the retry reproduced the spike")
	}
	// A value just outside the band is rejected too: the tolerance is the
	// stable band, not an arbitrary slack.
	if Accepts(clean, cleanOffsets, windows[2], -340+StableSpreadThresholdMS) {
		t.Fatal("Accepts(at the band edge) = true, want false")
	}
}

// TestFallbackBoundaryPeakIsNotMasked pins the case where a
// retry cannot help: the peak is on the edge of the search, so re-decoding
// with a longer pre-roll finds the same edge. The job must escalate to a full
// decode — and that full decode must reproduce the peak and keep the
// out_of_range verdict, because a fallback that quietly produced a plausible
// offset would be worse than failing.
func TestFallbackBoundaryPeakIsNotMasked(t *testing.T) {
	offsets := []float64{-340, -340, -testMaxLagMS, -340, -340, -340}
	windows := fallbackWindows(t, len(offsets))
	qualities := fillQualities(len(offsets), testQuality)

	suspects := Suspects(windows, offsets, qualities, testMaxLagMS)
	if len(suspects) != 1 || suspects[0].Index != 2 || suspects[0].Reason != SuspectBoundaryPeak {
		t.Fatalf("Suspects(boundary peak) = %+v, want exactly one boundary-peak suspect at index 2", suspects)
	}

	clean := make([]Window, 0, len(windows)-1)
	cleanOffsets := make([]float64, 0, len(offsets)-1)
	for i := range windows {
		if i == 2 {
			continue
		}
		clean = append(clean, windows[i])
		cleanOffsets = append(cleanOffsets, offsets[i])
	}

	// The retried window still peaks at the bound: the retry is not a
	// repaired window, so the caller decodes in full.
	retried := -testMaxLagMS
	if Accepts(clean, cleanOffsets, windows[2], retried) {
		t.Fatal("Accepts(boundary peak) = true, want false: a boundary peak is not a match")
	}
	// Re-running the policy on the retried timeline agrees, so a caller that
	// re-classifies instead of accepting still escalates.
	suspectsAfterRetry := Suspects(windows, offsets, qualities, testMaxLagMS)
	if len(suspectsAfterRetry) != 1 || suspectsAfterRetry[0].Reason != SuspectBoundaryPeak {
		t.Fatalf("Suspects after retry = %+v, want the same boundary peak", suspectsAfterRetry)
	}

	// And the full decode reproduces the peak: the report must still say
	// out_of_range, not a drifting timeline that happens to include 190 s.
	verdict, classification, _ := classify(offsets, qualities, testAcceptBoundMS)
	if verdict != VerdictOutOfRange || classification != ClassificationOutOfRange {
		t.Fatalf("classify(boundary peak) = %q/%q, want %q/%q: the fallback must not mask it",
			verdict, classification, VerdictOutOfRange, ClassificationOutOfRange)
	}
}

// TestFallbackTwoSpikesMeanFullDecode is the escalation rule's other half,
// asserted at
// the policy level so it needs no ffmpeg: two suspect windows are not worth
// two retries, so the whole job is decoded in full.
func TestFallbackTwoSpikesMeanFullDecode(t *testing.T) {
	offsets := []float64{-340, 800, -340, -340, 800, -340}
	windows := fallbackWindows(t, len(offsets))

	suspects := Suspects(windows, offsets, fillQualities(len(offsets), testQuality), testMaxLagMS)
	if len(suspects) != 2 {
		t.Fatalf("Suspects(two spikes) = %+v, want 2", suspects)
	}
	if !RequiresFullDecode(suspects) {
		t.Fatal("RequiresFullDecode(2 suspects) = false, want true")
	}
	if RequiresFullDecode(nil) {
		t.Fatal("RequiresFullDecode(no suspects) = true, want false")
	}
}

// TestFallbackMedianReferenceOnShortTimeline covers the short-timeline
// fallback: with only two clean windows there is no line to speak of, so the
// reference is the median of their offsets. The second half uses two windows
// whose median is not either value, which is what makes the assertion about
// the median rather than about "either of the clean offsets".
func TestFallbackMedianReferenceOnShortTimeline(t *testing.T) {
	offsets := []float64{-340, 800, -340}
	windows := fallbackWindows(t, len(offsets))

	suspects := Suspects(windows, offsets, fillQualities(len(offsets), testQuality), testMaxLagMS)
	if len(suspects) != 1 || suspects[0].Index != 1 || suspects[0].Reason != SuspectResidual {
		t.Fatalf("Suspects(3-point spike) = %+v, want one residual suspect in the middle", suspects)
	}

	clean := []Window{windows[0], windows[2]}
	cleanOffsets := []float64{offsets[0], offsets[2]}

	median := medianOf(cleanOffsets)
	if !Accepts(clean, cleanOffsets, windows[1], -341) {
		t.Fatal("Accepts(-341) = false, want true: within the band of the clean median")
	}
	if math.Abs(-341-median) >= StableSpreadThresholdMS {
		t.Fatalf("accepted value is %v ms from the clean median %v", math.Abs(-341-median), median)
	}
	if Accepts(clean, cleanOffsets, windows[1], 700) {
		t.Fatal("Accepts(+700) = true, want false")
	}

	// Two clean windows that straddle the candidate: the reference is their
	// median (-320), so -350 is inside the band and -270 is outside it.
	asymmetric := []float64{-340, -300}
	if !Accepts(clean, asymmetric, windows[1], -350) {
		t.Fatal("Accepts(-350) = false against a -320 median, want true")
	}
	if Accepts(clean, asymmetric, windows[1], -270) {
		t.Fatal("Accepts(-270) = true against a -320 median, want false")
	}
}

// TestFallbackNeedsData pins the "nothing to judge" shapes so a caller never
// gets a suspect it cannot act on, and so a short offsets slice cannot panic.
func TestFallbackNeedsData(t *testing.T) {
	windows := fallbackWindows(t, len(DefaultCheckpointPositionsPct))
	if got := Suspects(nil, nil, nil, testMaxLagMS); got != nil {
		t.Fatalf("Suspects(nil) = %+v, want nil", got)
	}
	if got := Suspects(windows, []float64{-340}, fillQualities(len(windows), testQuality), testMaxLagMS); got != nil {
		t.Fatalf("Suspects(short offsets) = %+v, want nil", got)
	}
	if Accepts(nil, nil, windows[0], -340) {
		t.Fatal("Accepts with no reference window = true, want false")
	}
}
