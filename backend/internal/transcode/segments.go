package transcode

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// A Segment tells the encoder which part of the dub to take and where to put
// it (#84). StartSec/EndSec are positions in the TARGET timeline, not in the
// dub: the encoder shifts them by OffsetMS to find the source material.
//
// A re-cut dub has one offset per run of the film, so the track cannot be
// moved by a single mkvmerge `--sync` value. Instead each run is cut out of the
// dub copy, padded to its absolute position and concatenated — in ONE encode,
// because a per-segment encode would be a second generation of loss for every
// join (ADR-0010).
type Segment struct {
	StartSec float64 `json:"start_sec"`
	EndSec   float64 `json:"end_sec"`
	OffsetMS float64 `json:"offset_ms"`
}

// lengthSec is the output length this segment must occupy.
func (s Segment) lengthSec() float64 { return s.EndSec - s.StartSec }

// SegmentsTotalMS is the output duration a segmented encode must produce.
//
// It sums the per-segment lengths as the filtergraph writes them (each one
// rounded to the millisecond by `atrim=duration`), not the unrounded total, so
// the number the verification compares against is exactly what the graph adds
// up to.
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

// BuildSegmentedArgs renders the one ffmpeg invocation that cuts the dub copy
// per segment, places every run at its absolute position and concatenates the
// result. Exported for the argv test.
//
// Per segment the filtergraph trims the dub at
// [StartSec - OffsetMS/1000, EndSec - OffsetMS/1000), resets the timestamps,
// converts to the plan's layout and rate with the same aformat/aresample pair
// BuildArgs uses (concat refuses branches whose formats differ), delays the run
// by any lead silence it needs, pads it to its length and trims it back to that
// length. adelay+apad+atrim=duration are what make each piece exactly
// EndSec-StartSec long, so concat cannot drift and the total equals
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
	// graph below places every run at an absolute position, and an atempo in
	// front of that would move every one of them. Refuse rather than encode one
	// and claim the other.
	if f := p.TempoFactor; f != 0 && f != 1 {
		return nil, fmt.Errorf("%w: a segmented render cannot carry a tempo factor (%v)", ErrInvalidPlan, f)
	}
	if len(req.Segments) == 0 {
		return nil, fmt.Errorf("%w: a segmented render needs at least one segment", ErrInvalidPlan)
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
