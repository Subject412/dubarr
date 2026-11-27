package transcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// segmentedPair is the two-segment render the argv tests pin: a 20-minute cut at
// 1500 s, the second run carrying the 2 s insert the cut removed.
func segmentedPair() []Segment {
	return []Segment{
		{StartSec: 300, EndSec: 1500, OffsetMS: 800},
		{StartSec: 1500, EndSec: 2400, OffsetMS: 2800},
	}
}

// TestBuildSegmentedArgsRendersTrimDelayAndConcat pins the one-pass filtergraph:
// one atrim/adelay/apad chain per segment, one concat, one mapping.
func TestBuildSegmentedArgsRendersTrimDelayAndConcat(t *testing.T) {
	plan := Plan{Codec: CodecFLAC, ChannelLayout: "stereo", SampleRate: 48000}
	args, err := BuildSegmentedArgs(Request{
		Input:    "/work/dub.mka",
		Output:   "/work/audio.transcoded.mka",
		Plan:     plan,
		Segments: segmentedPair(),
	})
	if err != nil {
		t.Fatalf("BuildSegmentedArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	// Positions are target-timeline seconds shifted back by the segment's own
	// offset: 300 - 0.8 = 299.2 in the dub, 1500 - 2.8 = 1497.2 for the run
	// after the insert.
	for _, want := range []string{
		"atrim=start=299.200000:end=1499.200000",
		"atrim=start=1497.200000:end=2397.200000",
		"asetpts=PTS-STARTPTS",
		"aformat=sample_fmts=fltp:channel_layouts=stereo",
		"aresample=48000",
		"adelay=delays=0:all=1",
		"apad=whole_dur=1200.000000",
		"apad=whole_dur=900.000000",
		"atrim=duration=1200.000000",
		"atrim=duration=900.000000",
		"concat=n=2:v=0:a=1[out]",
		"-map [out]",
		"-c:a flac",
		"-ar 48000",
		"-f matroska /work/audio.transcoded.mka",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv %v missing %q", args, want)
		}
	}
	if got := strings.Count(joined, "atrim=start="); got != 2 {
		t.Fatalf("argv has %d atrim=start= branches, want 2: %v", got, args)
	}
	if got := strings.Count(joined, "adelay="); got != 2 {
		t.Fatalf("argv has %d adelay branches, want 2: %v", got, args)
	}
	if got := strings.Count(joined, "concat="); got != 1 {
		t.Fatalf("argv has %d concat filters, want exactly 1: %v", got, args)
	}
	if got := strings.Count(joined, "-map [out]"); got != 1 {
		t.Fatalf("argv maps the concat output %d times, want 1: %v", got, args)
	}
	// One input, one encode: the whole point of the slice (ADR-0010).
	if got := strings.Count(joined, "-i "); got != 1 {
		t.Fatalf("argv has %d inputs, want 1: %v", got, args)
	}
	if got := strings.Count(joined, "-c:a"); got != 1 {
		t.Fatalf("argv has %d audio encoders, want 1: %v", got, args)
	}
}

// TestBuildSegmentedArgsClampsAtTheEdges: a segment the dub does not cover
// becomes silence of exactly the right length — lead silence for a negative
// source start, trailing silence for a window past the dub's end.
func TestBuildSegmentedArgsClampsAtTheEdges(t *testing.T) {
	plan := Plan{Codec: CodecFLAC, ChannelLayout: "stereo", SampleRate: 48000}
	args, err := BuildSegmentedArgs(Request{
		Input:            "/work/dub.mka",
		Output:           "/work/out.mka",
		Plan:             plan,
		SourceDurationMS: 120000,
		Segments: []Segment{
			// 2.5 s of the dub does not exist yet: the film starts before it.
			{StartSec: 0, EndSec: 60, OffsetMS: 2500},
			// The dub ends at 120 s; this run needs 40 s past it.
			{StartSec: 100, EndSec: 160, OffsetMS: 0},
		},
	})
	if err != nil {
		t.Fatalf("BuildSegmentedArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "atrim=start=0.000000:end=57.500000") {
		t.Fatalf("negative source start not clamped to the dub's first sample: %v", args)
	}
	if !strings.Contains(joined, "adelay=delays=2500:all=1") {
		t.Fatalf("negative source start did not become 2500 ms of lead silence: %v", args)
	}
	// The second run ends 40 s past the dub: the trim stops at the duration and
	// apad produces the rest of the segment as silence.
	if !strings.Contains(joined, "atrim=start=100.000000:end=120.000000") {
		t.Fatalf("window past the dub's end not clamped to the duration: %v", args)
	}
	if !strings.Contains(joined, "apad=whole_dur=60.000000") {
		t.Fatalf("window past the dub's end not padded to its segment length: %v", args)
	}
}

func TestSegmentsTotalMSIsTheSum(t *testing.T) {
	if got, want := SegmentsTotalMS(segmentedPair()), int64(2100); got != want {
		t.Fatalf("SegmentsTotalMS = %d, want %d", got, want)
	}
	// A zero-length segment contributes nothing rather than shifting the
	// expected duration.
	if got := SegmentsTotalMS([]Segment{{StartSec: 10, EndSec: 10}, {StartSec: 0, EndSec: 1}}); got != 1000 {
		t.Fatalf("SegmentsTotalMS with an empty segment = %d, want 1000", got)
	}
	if got := SegmentsTotalMS(nil); got != 0 {
		t.Fatalf("SegmentsTotalMS(nil) = %d, want 0", got)
	}
}

// fakeTool stands in for ffmpeg and ffprobe: it records every invocation,
// reports a probed duration per path, and creates the file ffmpeg was asked for
// so the existence check passes without running anything.
type fakeTool struct {
	calls []string
	// probeMS is the duration every path without an override reports.
	probeMS int64
	// byPath overrides the duration for one path, so a test can make the dub
	// longer than the cut track rendered from it.
	byPath map[string]int64
}

func (f *fakeTool) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if strings.HasSuffix(name, "ffprobe") {
		ms := f.probeMS
		if len(args) > 0 {
			if override, ok := f.byPath[args[len(args)-1]]; ok {
				ms = override
			}
		}
		return []byte(fmt.Sprintf(
			`{"streams":[{"codec_name":"flac","codec_type":"audio","channels":2,"duration":"%.3f"}],"format":{}}`,
			float64(ms)/1000)), nil
	}
	// ffmpeg: the output path is the last argv element.
	if len(args) == 0 {
		return nil, errors.New("ffmpeg called without arguments")
	}
	if err := os.WriteFile(args[len(args)-1], []byte("encoded"), 0o600); err != nil {
		return nil, err
	}
	return nil, nil
}

func (f *fakeTool) ffmpegCalls() []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "/fake/ffmpeg ") {
			out = append(out, c)
		}
	}
	return out
}

func segmentedRequest() Request {
	return Request{
		Input:   "/work/dub.mka",
		Output:  "/tmp/out.mka",
		Plan:    Plan{RuleIndex: NoRule, Codec: CodecFLAC, ChannelLayout: "stereo", SampleRate: 48000},
		Segments: []Segment{
			{StartSec: 0, EndSec: 3000, OffsetMS: 0},
			{StartSec: 3000, EndSec: 6000, OffsetMS: 0},
		},
	}
}

// TestSegmentedEncodeIsOnePass: three segments, one ffmpeg invocation.
func TestSegmentedEncodeIsOnePass(t *testing.T) {
	tool := &fakeTool{probeMS: 9000}
	tr := New(Dependencies{FFmpegPath: "/fake/ffmpeg", FFprobePath: "/fake/ffprobe", Run: tool.run})

	req := segmentedRequest()
	req.Output = t.TempDir() + "/out.mka"
	req.Segments = []Segment{
		{StartSec: 0, EndSec: 3000, OffsetMS: 0},
		{StartSec: 3000, EndSec: 6000, OffsetMS: 500},
		{StartSec: 6000, EndSec: 9000, OffsetMS: 1200},
	}
	if _, err := tr.Transcode(context.Background(), req); err != nil {
		t.Fatalf("Transcode: %v", err)
	}
	calls := tool.ffmpegCalls()
	if len(calls) != 1 {
		t.Fatalf("ffmpeg invocations = %d, want exactly 1 for 3 segments: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "concat=n=3:v=0:a=1[out]") {
		t.Fatalf("the single invocation does not concatenate all 3 segments: %s", calls[0])
	}
}

// TestSegmentedEncodeVerifiesTheSegmentsTotal proves the verification compares
// the output against what the segments add up to, not against the input's
// duration: the dub is 20 s and the cut track it renders is 6 s.
func TestSegmentedEncodeVerifiesTheSegmentsTotal(t *testing.T) {
	req := segmentedRequest()
	req.Output = t.TempDir() + "/out.mka"
	// The input is probed as 20 s on purpose: using it as the expected duration
	// would reject the correct 6 s render by 14 s.
	tool := &fakeTool{
		probeMS: 6000,
		byPath:  map[string]int64{req.Input: 20000},
	}
	tr := New(Dependencies{FFmpegPath: "/fake/ffmpeg", FFprobePath: "/fake/ffprobe", Run: tool.run})

	res, err := tr.Transcode(context.Background(), req)
	if err != nil {
		t.Fatalf("Transcode rejected a render that matches SegmentsTotalMS: %v", err)
	}
	if res.DurationMS != 6000 {
		t.Fatalf("result duration = %d ms, want the probed 6000", res.DurationMS)
	}
}

func TestSegmentedEncodeFailsWhenTheRenderIsShort(t *testing.T) {
	req := segmentedRequest()
	req.Output = t.TempDir() + "/out.mka"
	// 1.5 s short of the 6 s the segments ask for, and far short of the 20 s dub.
	tool := &fakeTool{
		probeMS: 4500,
		byPath:  map[string]int64{req.Input: 20000},
	}
	tr := New(Dependencies{FFmpegPath: "/fake/ffmpeg", FFprobePath: "/fake/ffprobe", Run: tool.run})

	if _, err := tr.Transcode(context.Background(), req); !errors.Is(err, ErrVerify) {
		t.Fatalf("Transcode = %v, want ErrVerify for a render short of SegmentsTotalMS", err)
	}
}

// TestBuildSegmentedArgsRejectsUnusableRequests: a segmented render is always an
// encode, always has at least one segment, and never carries a tempo factor
// (the graph places absolute positions; a stretch would move them).
func TestBuildSegmentedArgsRejectsUnusableRequests(t *testing.T) {
	base := Request{Input: "/work/in.mka", Output: "/work/out.mka", Segments: segmentedPair()}
	cases := []struct {
		name string
		req  Request
	}{
		{"keep plan", func() Request { r := base; r.Plan = Plan{Codec: CodecKeep}; return r }()},
		{"no segments", func() Request { r := base; r.Plan = Plan{Codec: CodecFLAC}; r.Segments = nil; return r }()},
		{"empty segment", func() Request {
			r := base
			r.Plan = Plan{Codec: CodecFLAC}
			r.Segments = []Segment{{StartSec: 10, EndSec: 10}}
			return r
		}()},
		{"tempo factor", func() Request {
			r := base
			r.Plan = Plan{Codec: CodecFLAC, TempoFactor: 0.95904}
			return r
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildSegmentedArgs(tc.req); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("BuildSegmentedArgs = %v, want ErrInvalidPlan", err)
			}
		})
	}
}
