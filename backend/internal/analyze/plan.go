package analyze

import (
	"errors"
	"fmt"
	"math"
)

// analysisSampleRate is the rate both analysis inputs are decoded at
// (`-ac 1 -ar 8000` in syncjob's decode) and the rate every planned boundary
// is snapped to. It is one constant because three independent conversions of
// the same seconds to samples — the planner's, the decoder's and the engine's
// (`sync.Job`'s SliceStartSec x rate) — otherwise disagree by up to 62.5 µs
// each, and the windowed parity bound is a single sample: they must round the
// same numbers, not three copies of them.
const analysisSampleRate = 8000

// ErrWindowPlan is what [PlanWindows] returns instead of a window it cannot
// place: an unusable duration or window, or a window whose snapped end reads
// past the audio the plan promised it fits in. Callers treat it as "analyse
// the whole file instead", never as "carry on with a shortened window".
var ErrWindowPlan = errors.New("analyze: cannot plan analysis windows")

// UnknownOrigin names the analysis input whose timeline origin the container
// did not state. A zero value ([UnknownOriginNone]) means both origins were
// reported and the seek corrections can be trusted.
//
// It exists because an unknown origin cannot be represented as a number: a
// correction of 0 is a real, correct value for an audio-only file, so writing
// 0 for "the container said nothing" would turn an unverifiable seek into a
// plausible one. The plan therefore records the gap and the caller falls back
// to a whole-file decode (U-WIN.7), where the correction does not arise.
type UnknownOrigin string

const (
	// UnknownOriginNone means both inputs stated their origins.
	UnknownOriginNone UnknownOrigin = ""
	// UnknownOriginTarget means only the target's origin is unknown.
	UnknownOriginTarget UnknownOrigin = "target"
	// UnknownOriginDonor means only the donor's origin is unknown.
	UnknownOriginDonor UnknownOrigin = "donor"
	// UnknownOriginBoth means neither input stated its origin.
	UnknownOriginBoth UnknownOrigin = "target+donor"
)

// SourceTiming is the analyze-side view of one analysis input's timing as
// probed before any decode (syncjob.SourceTiming, U-WIN.1): how long the track
// really is, and where its timeline starts.
//
// The origins are two facts, not one. ContainerStartSec is the container's
// timeline origin (`format.start_time`), which is what a `-ss` seek addresses;
// TrackStartSec is the analysed stream's own origin, which is where a full
// decode's frame 0 sits. On an audio-only file they coincide; inside a muxed
// MKV whose reference stream starts late they do not.
type SourceTiming struct {
	// DurationSec is the length of the track this input contributes, in
	// seconds. Zero (or negative) means unknown and makes the plan
	// impossible; it is never treated as "an empty file".
	DurationSec float64
	// ContainerStartSec and TrackStartSec are the two timeline origins, in
	// seconds. They are only meaningful when OriginKnown is true.
	ContainerStartSec float64
	TrackStartSec     float64
	// OriginKnown reports that the container stated both origins. False is
	// "no fact", never "zero": see [UnknownOrigin].
	OriginKnown bool
}

// Timing is the window plan's input: the duration and the two seek
// corrections of the pair being analysed.
//
// The durations are kept per source even though only their minimum is used,
// because that minimum is the single duration base for both sources (DURATION
// BASE): the donor is the shorter source in the fixtures, and a percentage of
// the target's length alone would push the last window past the donor's EOF.
type Timing struct {
	Target SourceTiming
	Donor  SourceTiming
}

// DurationSec is the plan's duration base: the shorter of the two analysis
// inputs, in seconds. Percentages are taken of this one number, so a window
// never claims more audio than either source has.
func (t Timing) DurationSec() float64 {
	return math.Min(t.Target.DurationSec, t.Donor.DurationSec)
}

// unknownOrigin reports which input, if any, left its timeline origin
// unstated.
func (t Timing) unknownOrigin() UnknownOrigin {
	switch {
	case t.Target.OriginKnown && t.Donor.OriginKnown:
		return UnknownOriginNone
	case !t.Target.OriginKnown && !t.Donor.OriginKnown:
		return UnknownOriginBoth
	case !t.Target.OriginKnown:
		return UnknownOriginTarget
	default:
		return UnknownOriginDonor
	}
}

// seekCorrection is how far this input's container origin differs from the
// analysed track's own origin, in seconds: the amount a container seek must be
// shifted so the decoded audio is the track's audio at the film position.
//
// An unknown origin returns 0, and that 0 is never used silently: the plan
// carries [UnknownOrigin] for it, and the caller must not execute a plan whose
// correction is unknown. There is deliberately no per-source clamping here —
// the correction is metadata, not a fit to the file's length.
func (s SourceTiming) seekCorrection() float64 {
	if !s.OriginKnown {
		return 0
	}
	return s.TrackStartSec - s.ContainerStartSec
}

// Window is one planned checkpoint. FilmStartSec is the target-timeline
// position reported as slice_start_sec. The Seek fields are where the decoder
// must start reading each file to land on that film position; they differ from
// it only by a container/track start_time correction.
//
// FilmStartSec and the two Seek fields are separate values for a reason
// (TWO-NUMBERS): one is what the window claims about the movie, the others are
// where a decoder must point. Reporting a seek as the film position shifts
// every checkpoint by the start_time correction; seeking to the film position
// inside a muxed container reads the wrong audio by the same amount.
type Window struct {
	PositionPct   int
	FilmStartSec  float64
	LenSec        float64
	TargetSeekSec float64
	DonorSeekSec  float64
	// UnknownOrigin names the input whose timeline origin was not stated, so
	// the caller cannot mistake the uncorrected seeks above for facts. Empty
	// when both origins were reported.
	UnknownOrigin UnknownOrigin
}

// PlanWindows turns a duration, the checkpoint percentages and a window
// length into the exact list of windows a job analyses.
//
// The clamp is joint (PAIR CLAMP): one start is computed from the duration
// base, clamped into it, and both sources then receive that same film position
// — only the seek correction differs per source, and it is metadata rather
// than a per-source clamp. A design that shifts one source to fit its own EOF
// puts that shift straight into offset_ms, which is finding 3.
//
// Every boundary is snapped to a whole sample at [analysisSampleRate]: the
// start (half away from zero) becomes FilmStartSec, the length is at least one
// sample. Without the snapping, three roundings of the same seconds disagree
// by up to 62.5 µs and a one-sample parity bound is unreachable for the wrong
// reason.
//
// A plan that would read past the duration base — more than one sample of
// slack, i.e. beyond what snapping can explain — is refused with
// [ErrWindowPlan] rather than shortened: a short window is not data (INV-WIN).
func PlanWindows(dur Timing, pcts []int, window float64) ([]Window, error) {
	base := dur.DurationSec()
	if math.IsNaN(base) || math.IsInf(base, 0) || base <= 0 {
		return nil, fmt.Errorf("%w: duration %v is not a positive finite number", ErrWindowPlan, base)
	}
	if math.IsNaN(window) || math.IsInf(window, 0) || window <= 0 {
		return nil, fmt.Errorf("%w: window %v is not a positive finite number", ErrWindowPlan, window)
	}
	if len(pcts) == 0 {
		return nil, fmt.Errorf("%w: no checkpoint positions", ErrWindowPlan)
	}

	origin := dur.unknownOrigin()
	durSamples := base * analysisSampleRate
	windows := make([]Window, len(pcts))
	for i, pct := range pcts {
		start, length := clampWindow(base, float64(pct), window)
		startSample, lenSamples := sampleCount(start), sampleCount(length)
		if lenSamples < 1 {
			lenSamples = 1
		}
		if windowEscapes(startSample, lenSamples, durSamples) {
			return nil, fmt.Errorf("%w: checkpoint %d%%: samples %d+%d exceed %.3f",
				ErrWindowPlan, pct, startSample, lenSamples, durSamples)
		}
		film := float64(startSample) / analysisSampleRate
		windows[i] = Window{
			PositionPct:   pct,
			FilmStartSec:  film,
			LenSec:        float64(lenSamples) / analysisSampleRate,
			TargetSeekSec: film + dur.Target.seekCorrection(),
			DonorSeekSec:  film + dur.Donor.seekCorrection(),
			UnknownOrigin: origin,
		}
	}
	return windows, nil
}

// clampWindow places one window centred on pct per cent of dur and clamped into
// the analysis input, in this exact order: centre it, floor the start at 0, pull
// the start back so start+window fits, floor it at 0 again, then take the length
// as the smaller of the window and the audio left. The order matters: flooring
// before fitting a file shorter than the window is what turns that case into
// "start 0, length dur" instead of a negative start.
//
// pct is fractional, not a whole percentage: the dense plan places its windows
// on a time grid ([PlanWindowsEvery]) and hands the equivalent percentage in, so
// the two planners share one clamp instead of two copies of these four rules.
func clampWindow(dur, pct, window float64) (start, length float64) {
	start = dur*pct/100 - window/2
	if start < 0 {
		start = 0
	}
	if start+window > dur {
		start = dur - window
	}
	if start < 0 {
		start = 0
	}
	length = math.Min(window, dur-start)
	return start, length
}

// MaxDenseSamples caps the dense checkpoint grid: with the shipped 45 s step a
// two-hour film is 160 checkpoints, and beyond this many the step is widened
// rather than the pool flooded. 400 points is also the bound the segmenter's
// O(n²) tables are sized for (a few megabytes, milliseconds).
const MaxDenseSamples = 400

// DenseStepSec is the sampling step [PlanWindowsEvery] will actually use:
// stepSec, widened to dur/MaxDenseSamples when that is coarser.
//
// The widening is a resolution trade, not a failure: a ten-hour source cannot be
// sampled every 45 s within the cap, so it degrades to 90 s and the report says
// so ([Piecewise.StepSec]) instead of allocating an unbounded plan.
func DenseStepSec(durationSec, stepSec float64) float64 {
	if stepSec <= 0 || math.IsNaN(stepSec) || math.IsInf(stepSec, 0) {
		stepSec = DefaultPiecewisePolicy().StepSec
	}
	if durationSec <= 0 || math.IsNaN(durationSec) || math.IsInf(durationSec, 0) {
		return stepSec
	}
	if widened := durationSec / MaxDenseSamples; widened > stepSec {
		return widened
	}
	return stepSec
}

// PlanWindowsEvery builds a uniform checkpoint grid: one window every stepSec,
// centred in its slot, for as long as a whole window still fits inside the
// timeline (the same margin, clamping and sample snapping [PlanWindows] applies).
//
// The grid deliberately stops short of the end: a window must fit inside the
// max_offset_sec search bound, so the distance from the last planned window's
// end to the film's end is the margin at best and a whole step past it at worst
// (310 s at a 300 s step). That is a limit on what the analysis *samples*, not
// on what it can describe: an accepted segmentation is tiled to the film's own
// duration ([Piecewise.tileToFilm]) before it is reported, so the tail is
// covered by the last run's offset instead of being dropped from the plan.
//
// The grid is what turns a re-cut into a measurable discontinuity: six
// percentage checkpoints cannot locate a boundary, a grid 45 s apart locates it
// to within about ±step/2. Positions are still reported as percentages so an
// existing reader of [Checkpoint.PositionPct] has a number to show; on a dense
// plan that number is an approximation of the true film position, and
// [Window.FilmStartSec] is the fact.
//
// The step is widened past [MaxDenseSamples] (see [DenseStepSec]); a caller that
// needs to report the effective step asks for it by name rather than reading it
// back out of the plan.
func PlanWindowsEvery(dur Timing, stepSec, windowSec float64) ([]Window, error) {
	base := dur.DurationSec()
	if math.IsNaN(base) || math.IsInf(base, 0) || base <= 0 {
		return nil, fmt.Errorf("%w: duration %v is not a positive finite number", ErrWindowPlan, base)
	}
	if math.IsNaN(windowSec) || math.IsInf(windowSec, 0) || windowSec <= 0 {
		return nil, fmt.Errorf("%w: window %v is not a positive finite number", ErrWindowPlan, windowSec)
	}
	if math.IsNaN(stepSec) || math.IsInf(stepSec, 0) || stepSec <= 0 {
		return nil, fmt.Errorf("%w: step %v is not a positive finite number", ErrWindowPlan, stepSec)
	}

	origin := dur.unknownOrigin()
	durSamples := base * analysisSampleRate
	step := DenseStepSec(base, stepSec)
	windows := make([]Window, 0, int(base/step)+1)
	for centre := step / 2; centre+windowSec/2 <= base-CheckWindowMarginSec; centre += step {
		start, length := clampWindow(base, centre*100/base, windowSec)
		startSample, lenSamples := sampleCount(start), sampleCount(length)
		if lenSamples < 1 {
			lenSamples = 1
		}
		if windowEscapes(startSample, lenSamples, durSamples) {
			return nil, fmt.Errorf("%w: dense window at %.3f s: samples %d+%d exceed %.3f",
				ErrWindowPlan, centre, startSample, lenSamples, durSamples)
		}
		film := float64(startSample) / analysisSampleRate
		pct := int(math.Round(centre / base * 100))
		if pct < 1 {
			pct = 1
		}
		if pct > 99 {
			pct = 99
		}
		windows = append(windows, Window{
			PositionPct:   pct,
			FilmStartSec:  film,
			LenSec:        float64(lenSamples) / analysisSampleRate,
			TargetSeekSec: film + dur.Target.seekCorrection(),
			DonorSeekSec:  film + dur.Donor.seekCorrection(),
			UnknownOrigin: origin,
		})
	}
	if len(windows) == 0 {
		return nil, fmt.Errorf("%w: no dense window fits %.3f s of audio", ErrWindowPlan, base)
	}
	return windows, nil
}

// sampleCount converts seconds to a whole number of samples at
// [analysisSampleRate], half away from zero — the rounding the engine applies
// to the boundary it is handed.
func sampleCount(sec float64) int64 {
	return int64(math.Round(sec * analysisSampleRate))
}

// windowEscapes reports whether a snapped window reads past the audio it
// claims to fit in: one sample of slack is a rounding edge, more than that is
// a plan that leaves either file.
func windowEscapes(startSample, lenSamples int64, durSamples float64) bool {
	return float64(startSample+lenSamples) > durSamples+1
}
