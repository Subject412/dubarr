package analyze

import (
	"math"
	"slices"
)

// Why this file exists: a windowed analysis decodes one window per source
// instead of two whole tracks, so a window that decoded the wrong audio
// invents an offset the aggregation must not trust. The obvious rule —
// "flag whatever is far from the median" — flags exactly the two patterns
// that are legitimate and common:
//
//   - FPS drift: a dub mastered at a slightly different frame rate moves the
//     offset monotonically across the film (tens of ms end to end).
//   - A mid-movie cut: the donor has an insert or an edit, so the offset
//     steps once and stays put.
//
// Both are real properties of the material, and both are verdicts the mux
// stage already reasons about ([VerdictDrifting]); neither is a decode
// failure. So the suspect rule is shape-based: the timeline is compared with
// a straight line, and only a deviation the line cannot explain — a lone
// spike, a peak pinned to the engine's search edge, or a collapse in
// correlation quality — makes a window untrustworthy.
//
// Drift is deliberately *not* reported here. Fitting a slope and writing it
// into the report is issue #86; this file only decides which points to
// re-decode.
//
// The thresholds are the package's existing ones — [StableSpreadThresholdMS]
// and [LowQualityThreshold] — and nothing new: a policy that cannot be
// explained with the numbers classify already uses is a policy nobody can
// check.

// Suspect reasons, in the order [Suspects] gives them precedence.
const (
	// SuspectBoundaryPeak means the window's peak sits on the edge of the
	// engine's search range (|offset| == max_lag), where a peak is not a
	// match but the end of the search. A whole-file decode reproduces it,
	// and [VerdictOutOfRange] is then the honest answer: a fallback must
	// never turn a boundary peak into an apparently good offset.
	SuspectBoundaryPeak = "boundary-peak"
	// SuspectLowQuality means the correlation itself was too weak to trust
	// (quality below [LowQualityThreshold]), which is how an offset beyond
	// the searchable window surfaces: a degraded peak, not no peak.
	SuspectLowQuality = "low-quality"
	// SuspectResidual means the window's offset departs from the timeline's
	// line by more than the whole stable-spread band, while both its
	// neighbours sit on the line — a lone spike, not a step.
	SuspectResidual = "residual"
)

// msPerSample is one sample at [analysisSampleRate] in milliseconds. It is
// the slack the boundary-peak test allows, because the engine reports whole
// samples and a peak at the last lags rounds to the bound.
const msPerSample = 1000.0 / analysisSampleRate

// Suspect is one window the policy does not trust: the window's index, why it
// was flagged, and the residual it carried against the timeline's line.
type Suspect struct {
	Index int
	// Reason is one of [SuspectResidual], [SuspectBoundaryPeak] or
	// [SuspectLowQuality].
	Reason string
	// ResidualMS is offset minus the fitted line at this window's
	// [Window.FilmStartSec], in milliseconds. It is reported for every
	// reason, not only [SuspectResidual]: it is the number that says how
	// far out the window was. A window that sits on the line — a boundary
	// peak or a weak correlation in an otherwise flat timeline — carries
	// a small one, because its trouble is not its position.
	ResidualMS float64
}

// Suspects classifies windows as trustworthy or not, purely by shape.
//
// offsetsMS is the engine's answer per window (offset_ms = t_target −
// t_donor), so it is compared with the window's *absolute*
// [Window.FilmStartSec]: the question is whether position and offset are
// related the way a constant shift or a drift relates them. maxLagMS is the
// engine's search bound in milliseconds (the analysed window length,
// window_sec*1000) and is used only by the boundary-peak test.
//
// The rules, in order:
//
//  1. Fit one straight line through the timeline and take each window's
//     residual against it.
//  2. Every residual inside [StableSpreadThresholdMS] ⇒ nothing to fix: a
//     constant shift or a linear ramp is the material, not the decode.
//  3. A one-step timeline — for some split, each side flat about its own
//     mean while the two means differ by more than the band ⇒ nothing to
//     fix: a mid-movie cut.
//  4. Otherwise a window is suspect when its residual exceeds four times the
//     band **and both neighbours sit on the line** (a lone spike; a step has
//     a whole side off the line, so it is never flagged this way), or when
//     its offset is at the search boundary, or when its quality is below
//     [LowQualityThreshold].
//
// Rules 2 and 3 excuse the *residual* test only. A boundary peak and a
// collapsed quality are facts about one window's correlation, not about the
// shape of the timeline, and a flat timeline can still contain one — a
// low-quality window in a perfectly constant pair is exactly the checkpoint a
// windowed run must re-decode. Short-circuiting them away would make the
// policy blind to the two failures it exists to catch, so they are evaluated
// whatever the shape says.
//
// Suspects are returned in ascending index order, so the result does not
// depend on anything but the numbers handed in.
func Suspects(windows []Window, offsetsMS, qualities []float64, maxLagMS float64) []Suspect {
	n := len(windows)
	if n == 0 || len(offsetsMS) < n {
		// Nothing to judge: no windows, or a caller that lost offsets. An
		// empty result means "trust the timeline", which is the only safe
		// reading of "no data" here — the job is then verified by the
		// report's own verdict, not by a guess.
		return nil
	}

	residuals := residualsAgainstLine(windows, offsetsMS)
	// The shape excuses the residual test (rules 2 and 3): a constant shift,
	// a drift and a single cut are all lines or steps, and all legitimate.
	shapeExplainsTimeline := withinBand(residuals) || oneStep(offsetsMS)

	suspects := make([]Suspect, 0, n)
	for i := 0; i < n; i++ {
		reason := ""
		switch {
		case maxLagMS > 0 && abs(offsetsMS[i]) >= maxLagMS-msPerSample:
			reason = SuspectBoundaryPeak
		case i < len(qualities) && qualities[i] < LowQualityThreshold:
			reason = SuspectLowQuality
		case !shapeExplainsTimeline && loneSpike(residuals, i):
			reason = SuspectResidual
		}
		if reason != "" {
			suspects = append(suspects, Suspect{Index: i, Reason: reason, ResidualMS: residuals[i]})
		}
	}
	return suspects
}

// RequiresFullDecode reports whether the suspect set is too large to repair
// window by window: exactly one suspect is re-decoded once with the longer
// pre-roll, two or more mean the whole job is decoded in full instead. The
// rule counts windows, so it is independent of the order the checkpoints
// finished in.
//
// A single suspect whose retry is rejected by [Accepts], or whose retry
// fails (a decode error, [ErrShortWindow]), also escalates to a full decode:
// the caller decides that with Accepts' answer, and this function only
// carries the count half of the rule.
func RequiresFullDecode(suspects []Suspect) bool {
	return len(suspects) >= 2
}

// Accepts judges one re-decoded window against the reference built from the
// windows that were never suspect, which is the call that turns "re-decode
// once" into either a repaired report or a whole-file decode.
//
// The reference is fitted again here, without the value under test in it: a
// fit contaminated by the window being judged cannot judge it. (The same
// reason the timeline fit is a robust line — see fitLine.) When fewer than
// three clean windows are left there is no line to speak of, so the reference
// is the median of their offsets, which is what a short timeline can honestly
// say.
//
// The tolerance is [StableSpreadThresholdMS]: a retried offset must land in
// the band the report already calls one constant shift. With no clean window
// at all there is no reference, and nothing can be accepted.
func Accepts(reference []Window, referenceOffsets []float64, w Window, offsetMS float64) bool {
	n := len(reference)
	if len(referenceOffsets) < n {
		n = len(referenceOffsets)
	}
	if n == 0 {
		return false
	}
	points := make([]timelinePoint, 0, n)
	for i := 0; i < n; i++ {
		points = append(points, timelinePoint{x: reference[i].FilmStartSec, y: referenceOffsets[i]})
	}

	var expected float64
	if len(points) < 3 {
		ys := make([]float64, len(points))
		for i, p := range points {
			ys[i] = p.y
		}
		expected = medianOf(ys)
	} else {
		slope, intercept := fitLine(points)
		expected = slope*w.FilmStartSec + intercept
	}
	return abs(offsetMS-expected) < StableSpreadThresholdMS
}

// timelinePoint is one (film position, measured offset) pair: x in seconds of
// the movie, y in milliseconds of offset.
type timelinePoint struct{ x, y float64 }

// residualsAgainstLine returns each window's offset minus the timeline's line
// at that window's film position, in milliseconds.
func residualsAgainstLine(windows []Window, offsetsMS []float64) []float64 {
	points := make([]timelinePoint, len(windows))
	for i := range windows {
		points[i] = timelinePoint{x: windows[i].FilmStartSec, y: offsetsMS[i]}
	}
	slope, intercept := fitLine(points)
	residuals := make([]float64, len(points))
	for i, p := range points {
		residuals[i] = p.y - (slope*p.x + intercept)
	}
	return residuals
}

// fitLine returns the straight line the timeline is measured against: the
// median of the slopes between every pair of windows, and the median of the
// intercepts that slope implies.
//
// It is not a least-squares line, and it must not become one. A least-squares
// fit is dragged by the very point the rule is looking for: on six windows
// holding one spike (-340, -340, +800, -340, -340, -340) it lands at slope
// -32.6 and leaves residuals of 239..934 ms on the *clean* windows, so the
// spike's neighbours are as far off as the spike and the "both neighbours on
// the line" test can never pass — the rule would be dead for the shape it
// exists to catch. The median of pairwise slopes (Theil–Sen) is the same line
// for a constant shift and for a true drift — residuals 0 on both — and is
// moved by at most a minority of windows, so the spike shows up as a spike.
//
// Fewer than two points, or every window at the same film position, leaves no
// slope to take: the line is then horizontal at the median offset, which is
// the honest answer when position carries no information.
func fitLine(points []timelinePoint) (slope, intercept float64) {
	if len(points) == 0 {
		return 0, 0
	}
	slopes := make([]float64, 0, len(points)*(len(points)-1)/2)
	for i := 0; i < len(points); i++ {
		for j := i + 1; j < len(points); j++ {
			if dx := points[j].x - points[i].x; dx != 0 {
				slopes = append(slopes, (points[j].y-points[i].y)/dx)
			}
		}
	}
	if len(slopes) > 0 {
		slope = medianOf(slopes)
	}
	intercepts := make([]float64, len(points))
	for i, p := range points {
		intercepts[i] = p.y - slope*p.x
	}
	return slope, medianOf(intercepts)
}

// withinBand reports whether every residual is inside
// [StableSpreadThresholdMS] — rule 2: the timeline is one straight line, so
// whatever it says (a constant shift, an FPS ramp) is the material.
func withinBand(residuals []float64) bool {
	for _, r := range residuals {
		if abs(r) >= StableSpreadThresholdMS {
			return false
		}
	}
	return true
}

// oneStep reports whether the timeline is one cut — rule 3: some split where
// each side is flat about its own mean and the two means are further apart
// than the stable band. The within-group flatness is what separates a step
// from a spike: a spike's group mean moves and leaves the spike as far from
// its own group as it was from the line.
func oneStep(offsets []float64) bool {
	for k := 1; k < len(offsets); k++ {
		low, lowFlat := flatMean(offsets[:k])
		if !lowFlat {
			continue
		}
		high, highFlat := flatMean(offsets[k:])
		if !highFlat {
			continue
		}
		if abs(low-high) > StableSpreadThresholdMS {
			return true
		}
	}
	return false
}

// flatMean returns the mean of one side of a candidate split and whether
// every value sits within [StableSpreadThresholdMS] of it.
func flatMean(values []float64) (mean float64, flat bool) {
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	mean = sum / float64(len(values))
	for _, v := range values {
		if abs(v-mean) >= StableSpreadThresholdMS {
			return mean, false
		}
	}
	return mean, true
}

// loneSpike reports whether window i departs from the line by more than four
// times the stable band while both of its neighbours sit on it. The neighbour
// test is what makes this a spike and not a step, and it is why an end window
// is never flagged by shape: an end has one neighbour, so a departure there
// cannot be told apart from a step's edge.
func loneSpike(residuals []float64, i int) bool {
	if i == 0 || i == len(residuals)-1 {
		return false
	}
	if abs(residuals[i]) <= 4*StableSpreadThresholdMS {
		return false
	}
	return abs(residuals[i-1]) < StableSpreadThresholdMS && abs(residuals[i+1]) < StableSpreadThresholdMS
}

// medianOf returns the median of values, averaging the two middle values for
// an even count. It sorts a copy: callers reuse their slices.
func medianOf(values []float64) float64 {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	if len(sorted) == 0 {
		return 0
	}
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// abs is math.Abs, kept local so the millisecond arithmetic below reads as
// the rules it implements.
func abs(v float64) float64 { return math.Abs(v) }
