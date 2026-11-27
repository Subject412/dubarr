package analyze

import (
	"fmt"
	"math"
	"slices"
	"sort"
)

// This file holds the piecewise segmentation (#84, D5): where a re-cut dub's
// offset changes, and how sure the analysis is allowed to be about it.
//
// Everything here is policy plus one dynamic program that runs once per job on
// the answers the checkpoints already produced. Nothing in this file touches
// audio and nothing is on a checkpoint's hot path.

// A Segment is one run of the film during which the offset is constant.
// StartSec/EndSec are positions in the target timeline, not in the dub: the
// encoder shifts them by OffsetMS to find the source material. Slice 05
// produces these; the type is fixed here so [Application] never grows a second
// shape (D2b).
//
// The outer edges are the audio the segmentation has evidence for: the first
// segment starts at the first window it kept and the last one ends at the last
// kept window's end. A caller that must cover the whole target timeline extends
// them to its own duration; the analysis never claims film it did not sample.
type Segment struct {
	StartSec   float64 `json:"start_sec"`
	EndSec     float64 `json:"end_sec"`
	OffsetMS   float64 `json:"offset_ms"`
	Confidence float64 `json:"confidence"` // min confidence of its points
	Points     int     `json:"points"`
}

// A Boundary is where one segment ends and the next begins, in film time.
type Boundary struct {
	AtSec          float64 `json:"at_sec"`
	UncertaintySec float64 `json:"uncertainty_sec"` // half the sampling step
	StepMS         float64 `json:"step_ms"`         // offset change across it
}

// PiecewisePolicy is the sync.strategy.piecewise block (#84, D5).
//
// The zero value is the shipped policy: off, and — once [PiecewisePolicy.Normalised]
// has filled them — sampling every 45 s with a 60 s window. Off is the default
// because a piecewise job re-encodes the operator's dub copy, which is a cost
// nobody should pay for a film that needed one offset.
type PiecewisePolicy struct {
	// Enabled turns dense sampling and segmentation on for a job.
	Enabled bool `json:"enabled"`
	// StepSec is how far apart the dense checkpoints are.
	StepSec float64 `json:"step_sec"`
	// WindowSec is how much audio each dense checkpoint compares.
	WindowSec float64 `json:"window_sec"`
	// SplitPenaltyMS is how much better a split has to fit before another
	// segment is believed: the penalty is squared into the DP's chi-squared
	// cost, so a 250 ms penalty means 62500 ms² of residual has to be bought.
	SplitPenaltyMS float64 `json:"split_penalty_ms"`
	// MinSegmentSec is the shortest run that can be its own segment. A run
	// shorter than this is a measurement accident far more often than an edit.
	MinSegmentSec float64 `json:"min_segment_sec"`
	// MaxSegments caps the output. Real re-cuts have a handful of joins.
	MaxSegments int `json:"max_segments"`
}

// The ranges sync.strategy validation enforces, and the two structural floors
// the segmenter needs. They are here, next to the policy they describe, so the
// settings layer and the analyser cannot drift apart.
const (
	piecewiseStepMinSec       = 15
	piecewiseStepMaxSec       = 300
	piecewiseWindowMinSec     = 20
	piecewiseWindowMaxSec     = 190
	piecewiseSplitPenaltyMin  = 50
	piecewiseSplitPenaltyMax  = 5000
	piecewiseMinSegmentMinSec = 30
	piecewiseMinSegmentMaxSec = 3600
	piecewiseMaxSegmentsMin   = 2
	piecewiseMaxSegmentsMax   = 40

	// piecewiseMinPoints is the fewest confident checkpoints a segmentation may
	// be built from: fewer than eight points cannot distinguish a cut from
	// jitter with any honesty.
	piecewiseMinPoints = 8
	// piecewiseMinPointsPerSide is the smallest evidence a side of a boundary
	// may rest on.
	piecewiseMinPointsPerSide = 3
)

// The rejection messages the report carries. They are constants because a test
// and the queue row both name them; a reworded rejection is a UI change.
const (
	reasonTooFewPoints  = "too few confident checkpoints"
	boundaryUnconfident = "boundary at %.1f s is not confident enough (%s)"
)

// DefaultPiecewisePolicy is the documented default of the sync.strategy.piecewise
// block: off, sampling every 45 s with a 60 s window, a 250 ms split penalty, no
// segment shorter than two minutes and at most a dozen of them.
func DefaultPiecewisePolicy() PiecewisePolicy {
	return PiecewisePolicy{
		Enabled:        false,
		StepSec:        45,
		WindowSec:      60,
		SplitPenaltyMS: 250,
		MinSegmentSec:  120,
		MaxSegments:    12,
	}
}

// Normalised fills every unset numeric field with its documented default and
// leaves Enabled alone. A settings row written before the block existed — and a
// Config built by a caller that has never heard of it — therefore behaves as
// "off at the documented numbers" instead of as "off, and if you switch it on
// every number is zero".
func (p PiecewisePolicy) Normalised() PiecewisePolicy {
	d := DefaultPiecewisePolicy()
	if p.StepSec <= 0 {
		p.StepSec = d.StepSec
	}
	if p.WindowSec <= 0 {
		p.WindowSec = d.WindowSec
	}
	if p.SplitPenaltyMS <= 0 {
		p.SplitPenaltyMS = d.SplitPenaltyMS
	}
	if p.MinSegmentSec <= 0 {
		p.MinSegmentSec = d.MinSegmentSec
	}
	if p.MaxSegments <= 0 {
		p.MaxSegments = d.MaxSegments
	}
	return p
}

// IsZero reports whether this policy is "off at the shipped numbers".
//
// It is what makes the JSON tag on [Strategy.Piecewise] an `omitzero`: the
// report's strategy block stays byte-identical to slice 02's for the shipped
// default (D5's "off by default" is not a new key in every report), while a
// policy the operator has actually configured — or switched on — is written out
// so the report says what it ran.
func (p PiecewisePolicy) IsZero() bool {
	return !p.Enabled && p.Normalised() == DefaultPiecewisePolicy()
}

// validate enforces the sync.strategy table. The messages carry the field path
// but not the section key: the settings layer prefixes `sync.strategy: ` and
// the analyser prefixes `analyze: `, so one message serves both callers.
//
// The caller validates a Normalised policy: an absent block is legal and means
// the documented defaults.
func (p PiecewisePolicy) validate() error {
	if p.StepSec < piecewiseStepMinSec || p.StepSec > piecewiseStepMaxSec {
		return fmt.Errorf("piecewise.step_sec must be %d..%d", piecewiseStepMinSec, piecewiseStepMaxSec)
	}
	if p.WindowSec < piecewiseWindowMinSec || p.WindowSec > piecewiseWindowMaxSec {
		return fmt.Errorf("piecewise.window_sec must be %d..%d", piecewiseWindowMinSec, piecewiseWindowMaxSec)
	}
	// Adjacent windows have to overlap enough for a boundary to show up as a
	// disagreement between neighbours rather than as a gap.
	if p.StepSec < p.WindowSec/2 {
		return fmt.Errorf("piecewise.window_sec must be at most twice step_sec")
	}
	if p.SplitPenaltyMS < piecewiseSplitPenaltyMin || p.SplitPenaltyMS > piecewiseSplitPenaltyMax {
		return fmt.Errorf("piecewise.split_penalty_ms must be %d..%d", piecewiseSplitPenaltyMin, piecewiseSplitPenaltyMax)
	}
	if p.MinSegmentSec < piecewiseMinSegmentMinSec || p.MinSegmentSec > piecewiseMinSegmentMaxSec {
		return fmt.Errorf("piecewise.min_segment_sec must be %d..%d", piecewiseMinSegmentMinSec, piecewiseMinSegmentMaxSec)
	}
	if p.MaxSegments < piecewiseMaxSegmentsMin || p.MaxSegments > piecewiseMaxSegmentsMax {
		return fmt.Errorf("piecewise.max_segments must be %d..%d", piecewiseMaxSegmentsMin, piecewiseMaxSegmentsMax)
	}
	// A dense window still has to fit inside the ±max_offset_sec search bound,
	// otherwise the engine cannot reach an offset it is asked about and the
	// checkpoint answers with a boundary peak instead of "no match".
	if p.Enabled && p.WindowSec > float64(DefaultMaxOffsetSec-CheckWindowMarginSec) {
		return fmt.Errorf("piecewise.window_sec must be at most %d when piecewise is enabled (it must fit the max_offset_sec search bound)",
			DefaultMaxOffsetSec-CheckWindowMarginSec)
	}
	return nil
}

// Piecewise is the segmentation result. RejectedReason is empty on success.
//
// StepSec is the sampling step the plan actually used, which is the configured
// step unless [MaxDenseSamples] widened it; Boundary.UncertaintySec is derived
// from the same number, so a report on a ten-hour source does not claim a
// precision it did not measure.
type Piecewise struct {
	Segments         []Segment  `json:"segments"`
	Boundaries       []Boundary `json:"boundaries"`
	DominantOffsetMS float64    `json:"dominant_offset_ms"`
	StepSec          float64    `json:"step_sec"`
	RejectedReason   string     `json:"rejected_reason,omitempty"`

	// maxResidualMS is the largest distance between a point and its own
	// segment's median: the drift a successful piecewise job is allowed to
	// report instead of the step it removed. Unexported because it is derived
	// data, not part of the stored shape ([classify] reads it directly).
	maxResidualMS float64
}

// accepted reports whether the segmentation is a plan the job can use: nothing
// was rejected.
//
// A one-segment plan is accepted as what it is — the ordinary constant-offset
// job. The dense plan ran and found nothing to re-cut, which is the common
// answer on a clean film, so it must not read as a rejection: an operator who
// switched the feature on would otherwise see every clean film surfaced for
// review and fall back to a dominant offset that differs from the only offset
// there is.
func (p *Piecewise) accepted() bool {
	return p != nil && p.RejectedReason == ""
}

// cut reports whether the accepted plan actually found a boundary. Only a cut
// is classified `piecewise`: a single offset is judged exactly as it is with
// the feature off (see [classify]), so turning segmentation on never changes
// the verdict of a film that needed one offset.
func (p *Piecewise) cut() bool { return p.accepted() && len(p.Segments) > 1 }

// MinStepMS is the smallest offset change that counts as a boundary: two stable
// bands, 2 x th.StableSpreadMS (80 ms with the shipped table).
//
// It is derived from [Thresholds] rather than configured so that re-tuning the
// stable band tightens the segmentation with it; a jitter of ±10 ms can never
// manufacture a boundary at any sane setting.
func (th Thresholds) MinStepMS() float64 { return 2 * th.StableSpreadMS }

// pointRun is one candidate segment while the DP is being solved: a run of
// point indices (inclusive) and the median offset of that run. Named for the
// points rather than for the type it becomes so it cannot be mistaken for a
// checkpoint's run ([checkpointRun]), which is a different thing entirely.
type pointRun struct {
	first  int
	last   int
	median float64
}

// BuildSegments finds the piecewise-constant structure of the offsets.
//
// It is a chi-squared segmentation: the cost of a run of points is the sum of
// squared residuals around that run's median, and a split is accepted only when
// it lowers the cost by more than SplitPenaltyMS² per new segment. That is what
// keeps ordinary jitter (±10 ms) from producing fifteen "segments" on a clean
// film.
//
// The steps, in order:
//
//  1. a point whose quality is below th.LowQuality cannot testify and is
//     dropped; fewer than eight survivors is a rejection;
//  2. the DP picks the cheapest split points, each new segment paying
//     SplitPenaltyMS²;
//  3. more than MaxSegments candidates are merged cheapest-split-first;
//  4. a boundary survives only if both sides carry at least three points and
//     MinSegmentSec of film, the medians differ by at least th.MinStepMS(), and
//     the checkpoint nearest the join on each side is confident. Anything else
//     merges the two runs and records why;
//  5. a plan left with a single run is not a rejection: it is the ordinary
//     constant-offset job, reported as one segment and no boundaries.
//
// A boundary is never invented: every failure above merges rather than guesses.
// A *rejected* plan (steps 1 and 4) falls back to DominantOffsetMS (D5).
func BuildSegments(windows []Window, offsets, qualities []float64, p PiecewisePolicy, th Thresholds) Piecewise {
	p = p.Normalised()
	out := Piecewise{
		Segments:   []Segment{},
		Boundaries: []Boundary{},
		StepSec:    p.StepSec,
	}
	// The step the plan actually used wins over the configured one: a source
	// long enough to hit MaxDenseSamples was sampled coarser, and every
	// uncertainty the report states comes from this number.
	if step := windowSpacingSec(windows); step > 0 {
		out.StepSec = step
	}

	n := len(windows)
	if len(offsets) < n {
		n = len(offsets)
	}
	if n == 0 {
		out.RejectedReason = reasonTooFewPoints
		return out
	}

	kept := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if qualityOf(qualities, i) < th.LowQuality {
			continue
		}
		kept = append(kept, i)
	}
	// The single offset a one-segment plan *is*, and the offset a rejected one
	// falls back to: the median over every point that could testify, which is
	// the 0.4.0 fallback.
	out.DominantOffsetMS = medianOf(offsetsOf(offsets[:n], kept))
	if len(kept) < piecewiseMinPoints {
		out.RejectedReason = reasonTooFewPoints
		return out
	}

	x := make([]float64, len(kept)) // film position of the point, in target seconds
	y := make([]float64, len(kept)) // its offset, in milliseconds
	for k, i := range kept {
		x[k] = windowCentreSec(windows[i])
		y[k] = offsets[i]
	}

	runs := capRuns(cheapestRuns(segmentCosts(y), p.SplitPenaltyMS*p.SplitPenaltyMS), y, p.MaxSegments)
	runs, rejected := confidentRuns(runs, x, y, kept, qualities, p, th)
	if rejected != "" {
		out.RejectedReason = rejected
	}

	out.Boundaries = make([]Boundary, 0, len(runs))
	for k := 0; k+1 < len(runs); k++ {
		out.Boundaries = append(out.Boundaries, Boundary{
			AtSec:          (x[runs[k].last] + x[runs[k+1].first]) / 2,
			UncertaintySec: (x[runs[k+1].first] - x[runs[k].last]) / 2,
			StepMS:         runs[k+1].median - runs[k].median,
		})
	}

	out.Segments = make([]Segment, len(runs))
	for k, r := range runs {
		first, last := kept[r.first], kept[r.last]
		segment := Segment{
			StartSec: windows[first].FilmStartSec,
			EndSec:   windows[last].FilmStartSec + windows[last].LenSec,
			OffsetMS: r.median,
			Points:   r.last - r.first + 1,
		}
		if k > 0 {
			segment.StartSec = out.Boundaries[k-1].AtSec
		}
		if k < len(out.Boundaries) {
			segment.EndSec = out.Boundaries[k].AtSec
		}
		segment.Confidence = math.Inf(1)
		for i := r.first; i <= r.last; i++ {
			segment.Confidence = math.Min(segment.Confidence, qualityOf(qualities, kept[i]))
			out.maxResidualMS = math.Max(out.maxResidualMS, abs(y[i]-r.median))
		}
		out.Segments[k] = segment
	}
	return out
}

// segmentCosts[i][j] is the cost of describing points i..j as one constant
// offset: Σ (offset_k − median(i..j))², the chi-squared residual of the run.
//
// Only the count, the sum, the sum of squares and the median are needed —
// Σ(o−m)² = Σo² − 2mΣo + k·m² — so a run costs O(1) once its median is known.
// The medians come from a sorted window that grows one point at a time, which
// is what makes the whole table O(n² log n) instead of O(n³ log n).
func segmentCosts(y []float64) [][]float64 {
	n := len(y)
	cost := make([][]float64, n)
	sorted := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		cost[i] = make([]float64, n)
		sorted = sorted[:0]
		sum, sumSquares := 0.0, 0.0
		for j := i; j < n; j++ {
			v := y[j]
			at := sort.SearchFloat64s(sorted, v)
			sorted = append(sorted, 0)
			copy(sorted[at+1:], sorted[at:])
			sorted[at] = v
			sum += v
			sumSquares += v * v
			m := medianSorted(sorted)
			c := sumSquares - 2*m*sum + float64(j-i+1)*m*m
			// Cancellation can leave a few negative femto-units on a perfectly
			// flat run; a negative cost would make the DP prefer noise.
			cost[i][j] = math.Max(c, 0)
		}
	}
	return cost
}

// medianSorted is the median of an already sorted slice, averaging the two
// middle values for an even count (the rule [medianOf] applies).
func medianSorted(sorted []float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// cheapestRuns solves the split-penalty DP and reads the segment list back.
//
//	best[j] = min over i of ( best[i-1] + cost(i, j) + penalty )
//
// with best[0] = 0. Charging the penalty on every segment rather than on every
// *extra* one adds the same constant to every candidate, so the chosen splits —
// and the comparison that decides whether a split at all is worth it — are
// exactly "penalty per new segment".
func cheapestRuns(cost [][]float64, penalty float64) []pointRun {
	n := len(cost)
	best := make([]float64, n+1)
	from := make([]int, n+1)
	for j := 1; j <= n; j++ {
		best[j] = math.Inf(1)
		for i := 1; i <= j; i++ {
			c := best[i-1] + cost[i-1][j-1] + penalty
			// Strictly less: a tie keeps the earliest start, so the split is
			// deterministic and the first segment is the longer one.
			if c < best[j] {
				best[j] = c
				from[j] = i
			}
		}
	}
	runs := make([]pointRun, 0, 4)
	for j := n; j > 0; {
		i := from[j]
		runs = append(runs, pointRun{first: i - 1, last: j - 1})
		j = i - 1
	}
	slices.Reverse(runs)
	return runs
}

// capRuns drops the cheapest splits until at most max segments are left.
//
// "Cheapest" is the split whose removal costs the least residual, i.e. the one
// the penalty was least justified in buying. A cap that leaves a single segment
// is the ordinary constant-offset job, not a failure; a cap that still leaves
// several says nothing in the report.
func capRuns(runs []pointRun, y []float64, max int) []pointRun {
	if max < piecewiseMaxSegmentsMin {
		max = piecewiseMaxSegmentsMin
	}
	for len(runs) > max {
		drop, cheapest := -1, 0.0
		for k := 0; k+1 < len(runs); k++ {
			delta := runCost(y, runs[k].first, runs[k+1].last) -
				runCost(y, runs[k].first, runs[k].last) -
				runCost(y, runs[k+1].first, runs[k+1].last)
			if drop < 0 || delta < cheapest {
				drop, cheapest = k, delta
			}
		}
		if drop < 0 {
			break
		}
		merged := pointRun{first: runs[drop].first, last: runs[drop+1].last}
		runs = append(runs[:drop+1], runs[drop+2:]...)
		runs[drop] = merged
	}
	return runs
}

// runCost is the chi-squared cost of one run, computed directly: the table is
// only interesting for the whole prefix, and a merge re-costs a range that is
// already known to be contiguous.
func runCost(y []float64, first, last int) float64 {
	if first > last {
		return 0
	}
	m := medianOf(y[first : last+1])
	cost := 0.0
	for _, v := range y[first : last+1] {
		cost += (v - m) * (v - m)
	}
	return cost
}

// confidentRuns applies the boundary rules and merges every boundary they
// refuse, returning the surviving runs and the first rejection it had to state.
//
// The merge is not a guess in the other direction: it says "these two runs are
// one offset", which is exactly what the 0.4.0 fallback applies, and the job is
// surfaced for review with the reason.
func confidentRuns(runs []pointRun, x, y []float64, kept []int, qualities []float64, p PiecewisePolicy, th Thresholds) ([]pointRun, string) {
	for k := range runs {
		runs[k].median = medianOf(y[runs[k].first : runs[k].last+1])
	}
	minStep := th.MinStepMS()
	rejected := ""
	for k := 0; k+1 < len(runs); {
		a, b := runs[k], runs[k+1]
		at := (x[a.last] + x[b.first]) / 2
		var reason string
		switch {
		case a.last-a.first+1 < piecewiseMinPointsPerSide || b.last-b.first+1 < piecewiseMinPointsPerSide:
			reason = "one side has fewer than three checkpoints"
		case x[a.last]-x[a.first] < p.MinSegmentSec || x[b.last]-x[b.first] < p.MinSegmentSec:
			reason = fmt.Sprintf("one side is shorter than %g s of film", p.MinSegmentSec)
		case abs(b.median-a.median) < minStep:
			reason = fmt.Sprintf("the offset changes by less than %g ms", minStep)
		case qualityOf(qualities, kept[b.first]-1) < th.LowQuality || qualityOf(qualities, kept[b.first]) < th.LowQuality:
			// The checkpoints either side of the join in the *original* plan: a
			// point dropped as unreadable next to the join means the join
			// itself was never measured.
			reason = "the checkpoint next to the join is not confident"
		}
		if reason == "" {
			k++
			continue
		}
		if rejected == "" {
			rejected = fmt.Sprintf(boundaryUnconfident, at, reason)
		}
		merged := pointRun{first: a.first, last: b.last}
		merged.median = medianOf(y[merged.first : merged.last+1])
		runs = append(runs[:k+1], runs[k+2:]...)
		runs[k] = merged
	}
	return runs, rejected
}

// windowCentreSec is the film position a window's correlation is about: the
// middle of the audio it compared. Reported boundaries and segment edges are
// stated on the same timeline ([Window.FilmStartSec] plus half a window).
func windowCentreSec(w Window) float64 { return w.FilmStartSec + w.LenSec/2 }

// windowSpacingSec is the sampling step a plan was built at: the median
// distance between consecutive window centres, or 0 for a plan too short to
// have a spacing.
func windowSpacingSec(windows []Window) float64 {
	if len(windows) < 2 {
		return 0
	}
	steps := make([]float64, 0, len(windows)-1)
	for i := 1; i < len(windows); i++ {
		if d := windowCentreSec(windows[i]) - windowCentreSec(windows[i-1]); d > 0 {
			steps = append(steps, d)
		}
	}
	if len(steps) == 0 {
		return 0
	}
	return medianOf(steps)
}

// qualityOf is a point's confidence, 0 for a point the report never recorded:
// no evidence is not evidence of a good match.
func qualityOf(qualities []float64, i int) float64 {
	if i < 0 || i >= len(qualities) {
		return 0
	}
	return qualities[i]
}

// offsetsOf collects the offsets of the kept points, so the dominant offset is
// one median call over exactly the points that may testify.
func offsetsOf(offsets []float64, kept []int) []float64 {
	if len(kept) == 0 {
		return offsets
	}
	out := make([]float64, len(kept))
	for k, i := range kept {
		out[k] = offsets[i]
	}
	return out
}
