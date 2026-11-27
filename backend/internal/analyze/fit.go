package analyze

import (
	"math"
	"slices"
)

// This file holds the timeline fit: the robust straight line the drift report
// publishes (#86) and the shape rule that tells an FPS mismatch from a re-cut.
//
// The estimator itself — timelinePoint, fitLine, medianOf, abs — moved here
// verbatim from fallback.go, which had grown the very same Theil–Sen line for
// its suspect rule. Same estimator, same reason: one bad checkpoint must not
// define the trend it is being judged against.

// Fit shapes: what the line through the checkpoint offsets says about the
// material. [LineFit.Shape] carries exactly one of them.
const (
	// ShapeConstant is a flat line: one offset for the whole film.
	ShapeConstant = "constant"
	// ShapeDrift is a sloped line with small residuals: an FPS mismatch, and
	// the one shape a single tempo encode can correct.
	ShapeDrift = "drift"
	// ShapeCut is one step with both sides flat: a re-cut. Correcting it needs
	// per-segment work (slice 05), never a tempo stretch.
	ShapeCut = "cut"
	// ShapeMixed is neither a line nor a single step.
	ShapeMixed = "mixed"
	// ShapeInsufficient means fewer than three usable checkpoints: the line is
	// still reported, but it is not a classification.
	ShapeInsufficient = "insufficient"
)

// LineFit is the trend through the checkpoint offsets, reported alongside the
// spread. Slope is in milliseconds of offset per second of film, measured on
// the TARGET timeline ([Window.FilmStartSec]): a 25.000 fps dub against a
// 23.976 fps target gives +40.96 ms/s, and the factor that removes it is
// 0.95904 — below 1, because the dub runs fast and has to be slowed.
//
// TempoFactor is the atempo factor that removes the slope, set only for a
// [ShapeDrift] the encoder can represent. TempoApplied is deliberately not
// set here: analyze does not know whether the operator's encode path exists,
// so the worker — which does — marks it once it has folded the factor into the
// job's single encode.
type LineFit struct {
	SlopeMSPerSec     float64 `json:"slope_ms_per_sec"`
	InterceptMS       float64 `json:"intercept_ms"`
	ResidualRMSMS     float64 `json:"residual_rms_ms"`
	MaxResidualMS     float64 `json:"max_residual_ms"`
	R2                float64 `json:"r2"`
	Points            int     `json:"points"`
	Shape             string  `json:"shape"`
	CutAtSec          float64 `json:"cut_at_sec,omitempty"`
	CutUncertaintySec float64 `json:"cut_uncertainty_sec,omitempty"`
	TempoFactor       float64 `json:"tempo_factor,omitempty"`
	TempoApplied      bool    `json:"tempo_applied"`
}

// FitTimeline fits the robust line through (film position, offset) and
// classifies it.
//
// It uses every checkpoint whose confidence clears the table when at least
// three do, and every checkpoint otherwise, so a genuinely unreadable
// checkpoint cannot bend the trend it is supposed to be measured against. The
// confidence test is [classify]'s unreliable rule, inverted — the same
// numbers, read the same way, so a checkpoint the verdict distrusts is never
// one the trend leans on.
//
// Classification order, first match wins, every limit from th:
//
//	insufficient  Points < 3 (slope and intercept are still reported)
//	drift         R2 >= th.FitR2Min and |slope| >= th.DriftSlopeFloorMSPerSec
//	cut           oneStep: one step, both sides flat
//	constant      R2 >= th.FitR2Min
//	mixed         anything else
//
// The drift rule comes before the cut rule on purpose: a drift has a high R²
// *and* a slope above the floor, while a step's robust line has a low R² (and,
// for the settled six-point plans, a slope below the floor) — the two are
// never the same timeline.
func FitTimeline(windows []Window, offsetsMS, qualities, matchZ []float64, th Thresholds) LineFit {
	n := len(windows)
	if len(offsetsMS) < n {
		n = len(offsetsMS)
	}
	fit := LineFit{}
	if n == 0 {
		fit.Shape = ShapeInsufficient
		return fit
	}

	// The points the trend may lean on. Fewer than three trusted checkpoints
	// is no sample at all, so every checkpoint is used then: an unusable fit
	// reported with all the evidence beats one built from a single window.
	keep := trustedCheckpoints(n, qualities, matchZ, th)
	if len(keep) < 3 {
		keep = make([]int, n)
		for i := range keep {
			keep[i] = i
		}
	}
	fit.Points = len(keep)

	points := make([]timelinePoint, len(keep))
	offsets := make([]float64, len(keep))
	for k, i := range keep {
		points[k] = timelinePoint{x: windows[i].FilmStartSec, y: offsetsMS[i]}
		offsets[k] = offsetsMS[i]
	}

	slope, intercept := fitLine(points)
	fit.SlopeMSPerSec = slope
	fit.InterceptMS = intercept

	// Residuals against the line, plus the two sums the coefficient of
	// determination needs. The residual sums of squares are taken about the
	// fitted line; the total is taken about zero — see rSquared.
	var ssRes, ssTot float64
	for _, p := range points {
		r := p.y - (slope*p.x + intercept)
		fit.MaxResidualMS = math.Max(fit.MaxResidualMS, abs(r))
		ssRes += r * r
		ssTot += p.y * p.y
	}
	fit.ResidualRMSMS = math.Sqrt(ssRes / float64(len(points)))
	fit.R2 = rSquared(ssRes, ssTot)

	switch {
	case fit.Points < 3:
		fit.Shape = ShapeInsufficient
	case fit.R2 >= th.FitR2Min && abs(slope) >= th.DriftSlopeFloorMSPerSec:
		fit.Shape = ShapeDrift
		if factor, ok := tempoFactorFor(slope); ok {
			fit.TempoFactor = factor
		}
	case oneStep(offsets):
		fit.Shape = ShapeCut
		fit.CutAtSec, fit.CutUncertaintySec = cutBoundary(points, offsets)
	case fit.R2 >= th.FitR2Min:
		fit.Shape = ShapeConstant
	default:
		fit.Shape = ShapeMixed
	}
	return fit
}

// trustedCheckpoints returns the indices of the checkpoints [classify] would
// trust: quality at or above the floor, or a recorded z that clears the "needs
// a human look" band. A missing quality or z is no evidence of distrust.
func trustedCheckpoints(n int, qualities, matchZ []float64, th Thresholds) []int {
	keep := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if checkpointDistrusted(i, qualities, matchZ, th) {
			continue
		}
		keep = append(keep, i)
	}
	return keep
}

// checkpointDistrusted is one checkpoint of classify's unreliable rule: a
// quality below the floor that no z outvotes.
func checkpointDistrusted(i int, qualities, matchZ []float64, th Thresholds) bool {
	if i >= len(qualities) || qualities[i] >= th.LowQuality {
		return false
	}
	return len(matchZ) <= i || matchZ[i] == 0 || matchZ[i] < th.MatchZMaybe
}

// rSquared is the coefficient of determination of the fitted line against the
// zero-offset baseline, not against the mean offset.
//
// The textbook (centred) R² asks "does this line beat predicting the mean?",
// which is the wrong question for this report: on a flat timeline the whole
// variation is the ±5 ms decode jitter around a 1.2 s offset, so a line that
// captures the offset exactly and only misses the jitter scores near 0 — and
// every constant timeline would classify as mixed. What the shape rule needs to
// know is whether the line explains the offsets themselves ("this track needs
// shift = intercept + slope·t"), so the total sum of squares is taken about
// zero, the physical null hypothesis "the dub needs no shift at all". The two
// readings agree where it matters: a real drift is ~1, a step is well below
// the floor.
//
// A robust line can be worse than the baseline; the result is reported as 0
// rather than as a negative number nobody can read.
func rSquared(ssRes, ssTot float64) float64 {
	if ssTot <= 0 {
		if ssRes <= 0 {
			return 1
		}
		return 0
	}
	r2 := 1 - ssRes/ssTot
	if r2 < 0 {
		return 0
	}
	return r2
}

// cutBoundary locates the step [oneStep] reported and states it in film
// seconds: the midpoint of the gap between the last window on the low side and
// the first on the high side, with half that gap as its uncertainty.
//
// oneStep only answers yes or no, and its body is the fallback policy's — it
// reports no index — so the split is found again here with the same
// flat-side rule. The two agree by construction; a timeline oneStep rejects
// simply yields no boundary.
func cutBoundary(points []timelinePoint, offsets []float64) (atSec, uncertaintySec float64) {
	k := stepSplit(offsets)
	if k <= 0 || k >= len(points) {
		return 0, 0
	}
	low, high := points[k-1].x, points[k].x
	if high < low {
		low, high = high, low
	}
	return (low + high) / 2, (high - low) / 2
}

// stepSplit is oneStep's scan returning the split it accepted: the lowest k
// where both sides are flat about their own mean and the two means differ by
// more than the stable band. 0 means no such split.
func stepSplit(offsets []float64) int {
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
			return k
		}
	}
	return 0
}

// The atempo factor range the single-pass encoder honours. It mirrors
// transcode.ValidateTempo (0.5..2.0) rather than importing it: the analysis
// package reasons about the engine, not about the encoder's Go package, and a
// factor outside this range is simply one this release cannot apply.
const (
	tempoFactorFloor   = 0.5
	tempoFactorCeiling = 2.0
)

// tempoFactorFor turns a slope in ms/s into the atempo factor that removes it:
// factor = 1 - slope/1000.
//
// The sign follows the offset definition: OffsetMS = target - donor, taken on
// the target timeline, so a POSITIVE slope means the donor falls further behind
// every second — it runs fast (a 25.000 fps dub against a 23.976 fps target) and
// must be SLOWED, i.e. a factor below 1. 1 + slope/1000 would stretch it and
// double the drift; transcode.Verify cannot tell the two apart, because its
// expected duration is duration/f either way.
//
// The 0.5..2.0 window is the range the single-pass encoder honours
// (transcode.ValidateTempo), which here means slopes of -1000..+500 ms/s.
// Returns (0,false) outside it and for a zero slope, which is no correction at
// all.
func tempoFactorFor(slopeMSPerSec float64) (float64, bool) {
	factor := 1 - slopeMSPerSec/1000
	if factor == 1 || factor < tempoFactorFloor || factor > tempoFactorCeiling {
		return 0, false
	}
	return factor, true
}

// timelinePoint is one (film position, measured offset) pair: x in seconds of
// the movie, y in milliseconds of offset.
type timelinePoint struct{ x, y float64 }

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

// abs is math.Abs, kept local so the millisecond arithmetic in this file reads
// as the rules it implements.
func abs(v float64) float64 { return math.Abs(v) }
