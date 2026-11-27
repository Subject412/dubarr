package transcode

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// A Segment tells the encoder which part of the dub to take and where it
// belongs on the target timeline (#84). StartSec/EndSec are positions in the
// TARGET timeline, not in the dub: the encoder shifts them by OffsetMS to find
// the source material.
//
// A re-cut dub has one offset per run of the film, so the track cannot be moved
// by a single mkvmerge `--sync` value. Instead each run is cut out of the dub
// copy, given exactly its own length (adelay/apad/atrim) and the pieces are
// concatenated in film order — in ONE encode, because a per-segment encode would
// be a second generation of loss for every join (ADR-0010).
//
// concat stacks the pieces end to end and knows nothing about absolute time: a
// piece lands at the sum of the lengths before it, not at the position it names.
// StartSec/EndSec are therefore a *plan*, and a plan is only honest when it
// tiles the target timeline from 0 with no holes and no overlap — which is what
// [BuildSegmentedArgs] refuses to render any other way.
type Segment struct {
	StartSec float64 `json:"start_sec"`
	EndSec   float64 `json:"end_sec"`
	OffsetMS float64 `json:"offset_ms"`
}

// lengthSec is the output length this segment must occupy.
func (s Segment) lengthSec() float64 { return s.EndSec - s.StartSec }

// SegmentsTotalMS is the output duration a segmented encode must produce when
// the caller cannot say how long the target track is (see
// [Request.TargetDurationMS] for the case where it can).
//
// It sums each segment's length rounded to the millisecond. That rounding is the
// reporting unit, not the graph's: the filtergraph writes `atrim=duration` in
// seconds with six decimals, so what it adds up to is the unrounded total, and
// the difference here is at most half a millisecond per segment — far inside the
// one-second floor [Verify] allows either way.
func SegmentsTotalMS(segs []Segment) int64 {
	var total int64
	for _, s := range segs {
		if s.lengthSec() <= 0 {
			continue
		}
		total += int64(math.Round(s.lengthSec() * 1000))
	}
	return total
}

// expectedSegmentTotalMS is the length a segmented render must produce: the
// target track's own length when the caller knows it, otherwise the plan's own
// total. Both the tiling check and the verification read it, so a plan cannot be
// judged tiled against one number and verified against another.
func expectedSegmentTotalMS(req Request) int64 {
	if req.TargetDurationMS > 0 {
		return req.TargetDurationMS
	}
	return SegmentsTotalMS(req.Segments)
}

// segmentTilingToleranceMS is how far an edge may sit from its exact tiling
// position. Every position here is snapped to a whole 8000 Hz analysis sample
// (0.125 ms) and the totals are whole milliseconds, so a real plan is exact; the
// tolerance only absorbs that rounding. The shifts this check refuses are
// steps — tens of seconds — so nothing real hides inside a millisecond.
const segmentTilingToleranceMS = 1.0

// checkSegmentsTile refuses a plan that is not one contiguous run of the target
// timeline, from 0 to the expected total.
//
// The filtergraph cannot enforce this and cannot even see it: concat stacks the
// branches in plan order whatever position each one names. A plan whose first
// segment starts at 45 s therefore publishes a track 45 s early, and a plan with
// a hole pulls everything after the hole earlier by the length of the hole.
// Both used to be silent — the worker even recorded a sync offset of 0 — so they
// are refused here instead. Segment lengths are checked by the caller's own
// per-segment pass; this is about where the pieces sit.
func checkSegmentsTile(segs []Segment, expectedMS int64) error {
	if len(segs) == 0 {
		return fmt.Errorf("%w: a segmented render needs at least one segment", ErrInvalidPlan)
	}
	for i, seg := range segs {
		if i == 0 {
			if startMS := math.Round(seg.StartSec * 1000); math.Abs(startMS) > segmentTilingToleranceMS {
				return fmt.Errorf("%w: the first segment starts at %v s, want 0: "+
					"concat places it at the track's start, so any other position shifts the whole track",
					ErrInvalidPlan, seg.StartSec)
			}
			continue
		}
		prev := segs[i-1]
		gapMS := math.Round(seg.StartSec*1000) - math.Round(prev.EndSec*1000)
		if math.Abs(gapMS) > segmentTilingToleranceMS {
			return fmt.Errorf("%w: segment %d starts at %v s but segment %d ends at %v s (%v ms apart): "+
				"the render would drop or repeat that much of the film",
				ErrInvalidPlan, i+1, seg.StartSec, i, prev.EndSec, gapMS)
		}
	}
	last := segs[len(segs)-1]
	if expectedMS > 0 {
		if gapMS := math.Round(last.EndSec*1000) - float64(expectedMS); math.Abs(gapMS) > segmentTilingToleranceMS {
			return fmt.Errorf("%w: the last segment ends at %v s, want %d ms: the render would stop short of the film",
				ErrInvalidPlan, last.EndSec, expectedMS)
		}
	}
	return nil
}

// BuildSegmentedArgs renders the one ffmpeg invocation that cuts the dub copy
// per segment, gives every run exactly the length of its span of film, and
// concatenates the pieces in plan order. Exported for the argv test.
//
// concat stacks the pieces end to end: branch i lands at the sum of the lengths
// before it, not at the position its Segment names. The render is placed
// correctly only because the plan tiles the target timeline — every piece
// exactly as long as its span, starting at 0, with no hole and the last edge on
// the target's end — which [checkSegmentsTile] enforces before a single filter is
// written.
//
// Per segment the filtergraph trims the dub at
// [StartSec - OffsetMS/1000, EndSec - OffsetMS/1000), resets the timestamps,
// converts to the plan's layout and rate with the same aformat/aresample pair
// BuildArgs uses (concat refuses branches whose formats differ), delays the run
// by any lead silence it needs, pads it to its length and trims it back to that
// length. adelay+apad+atrim=duration are what make each piece exactly
// EndSec-StartSec long, so the stack cannot drift and the total equals
// [SegmentsTotalMS].
//
// A segmented render is always an encode (a copy cannot be cut), so the plan
// must request one. The codec comes from the matched rule, or FLAC when no rule
// matched — lossless, so enabling a timing feature cannot degrade the dub.
func BuildSegmentedArgs(req Request) ([]string, error) {
	p := req.Plan
	if !p.Encode() {
		return nil, fmt.Errorf("%w: a segmented render always encodes, plan requests no encode", ErrInvalidPlan)
	}
	encoder := p.Encoder()
	if encoder == "" {
		return nil, fmt.Errorf("%w: unknown codec %q", ErrInvalidPlan, p.Codec)
	}
	if err := ValidateTempo(p.TempoFactor); err != nil {
		return nil, err
	}
	// A cut and a stretch are different corrections of the same timeline: the
	// graph below stacks each run's span of film back to back, and an atempo in
	// front of that would scale every piece — and so every position — by the
	// factor. Refuse rather than encode one and claim the other.
	if f := p.TempoFactor; f != 0 && f != 1 {
		return nil, fmt.Errorf("%w: a segmented render cannot carry a tempo factor (%v)", ErrInvalidPlan, f)
	}
	if len(req.Segments) == 0 {
		return nil, fmt.Errorf("%w: a segmented render needs at least one segment", ErrInvalidPlan)
	}
	// The plan must tile the target timeline. concat places the pieces back to
	// back, so a gap, an overlap or a first run that does not start at 0 would
	// all be published as though the stack were the timeline — and the mux that
	// follows writes a sync offset of 0, so nothing downstream would notice.
	if err := checkSegmentsTile(req.Segments, expectedSegmentTotalMS(req)); err != nil {
		return nil, err
	}

	srcDur := float64(req.SourceDurationMS) / 1000
	branches := make([]string, 0, len(req.Segments))
	labels := make([]string, 0, len(req.Segments))
	for i, seg := range req.Segments {
		length := seg.lengthSec()
		if math.IsNaN(length) || math.IsInf(length, 0) || length <= 0 {
			return nil, fmt.Errorf("%w: segment %d is %v s long (start %v s, end %v s)",
				ErrInvalidPlan, i+1, length, seg.StartSec, seg.EndSec)
		}
		// The segment's positions are target-timeline seconds; the dub is
		// shifted by this run's offset, so the source window is the same span
		// moved back by offset/1000.
		srcStart := seg.StartSec - seg.OffsetMS/1000
		srcEnd := seg.EndSec - seg.OffsetMS/1000
		// A negative start is audio the dub does not have: lead silence.
		leadMS := int64(0)
		if srcStart < 0 {
			leadMS = int64(math.Round(-srcStart * 1000))
			srcStart = 0
		}
		// Clamp the window to the dub when its duration is known. A window past
		// the end (or a run that moved entirely before the start) collapses to
		// an empty trim; apad below still produces the segment's full length as
		// silence, which is what "the dub has no audio here" sounds like.
		if srcDur > 0 {
			if srcStart > srcDur {
				srcStart = srcDur
			}
			if srcEnd > srcDur {
				srcEnd = srcDur
			}
		}
		if srcEnd < srcStart {
			srcEnd = srcStart
		}

		filters := []string{
			"atrim=start=" + seconds(srcStart) + ":end=" + seconds(srcEnd),
			"asetpts=PTS-STARTPTS",
		}
		format := "aformat=sample_fmts=fltp"
		if p.ChannelLayout != "" {
			format += ":channel_layouts=" + p.ChannelLayout
		}
		filters = append(filters, format)
		if p.SampleRate > 0 {
			filters = append(filters, "aresample="+strconv.Itoa(p.SampleRate))
		}
		filters = append(filters,
			fmt.Sprintf("adelay=delays=%d:all=1", leadMS),
			"apad=whole_dur="+seconds(length),
			"atrim=duration="+seconds(length),
		)
		label := "seg" + strconv.Itoa(i)
		labels = append(labels, "["+label+"]")
		branches = append(branches, "[0:a]"+strings.Join(filters, ",")+"["+label+"]")
	}
	graph := strings.Join(branches, ";") + ";" + strings.Join(labels, "") +
		fmt.Sprintf("concat=n=%d:v=0:a=1[out]", len(req.Segments))

	// One input, one encoder and one mapping: the concat output is the whole
	// track, and no second generation of loss is introduced (ADR-0010).
	args := []string{
		"-hide_banner", "-nostdin", "-y",
		"-i", req.Input,
		"-filter_complex", graph,
		"-map", "[out]", "-vn", "-sn", "-dn",
		"-map_metadata", "0",
		"-c:a", encoder,
	}
	if p.BitrateBPS > 0 {
		args = append(args, "-b:a", strconv.Itoa(p.BitrateBPS))
	}
	if p.SampleRate > 0 {
		args = append(args, "-ar", strconv.Itoa(p.SampleRate))
	}
	if lang := strings.ToLower(strings.TrimSpace(req.LangISO2)); lang != "" {
		args = append(args, "-metadata:s:a:0", "language="+lang)
	}
	if title := strings.TrimSpace(req.Title); title != "" {
		args = append(args, "-metadata:s:a:0", "title="+title)
	}
	args = append(args, "-f", "matroska", req.Output)
	return args, nil
}

// seconds formats a timeline position for a filter argument. A fixed six
// decimals is microsecond resolution — far finer than any checkpoint can
// locate a boundary — and keeps the argv exactly reproducible in a test.
func seconds(v float64) string {
	if v < 0 {
		v = 0
	}
	return strconv.FormatFloat(v, 'f', 6, 64)
}
