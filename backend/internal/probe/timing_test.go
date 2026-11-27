package probe

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// shrekTargetProbeJSON is the shape a captured `ffprobe -show_streams
// -show_format -of json` document has for the real Shrek remux, trimmed to the
// fields probe reads: Matroska reports `duration: "N/A"` on every stream, so
// the format duration is the only number the container provides.
const shrekTargetProbeJSON = `{
  "streams": [
    {"index":0,"codec_name":"av1","codec_type":"video","start_time":"0.000000","duration":"N/A"},
    {"index":1,"codec_name":"opus","codec_type":"audio","channels":8,"channel_layout":"7.1",
     "bit_rate":"1875000","sample_rate":"48000","start_time":"0.000000","duration":"N/A",
     "tags":{"language":"eng","title":"English Opus 7.1"},"disposition":{"default":1,"dub":0}}
  ],
  "format":{"format_name":"matroska,webm","duration":"5431.772000","start_time":"0.000000"}
}`

// probeDoc serves one canned ffprobe document through a real file, so the
// stat + cache path is exercised exactly as in production.
func probeDoc(t *testing.T, doc string) *MediaInfo {
	t.Helper()
	p := New(Dependencies{Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte(doc), nil
	}})
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := p.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return info
}

// TestTrackDurationFallsBackToFormat is the finding-2 shape: every stream says
// N/A, the container says 5431.772 s, and the analysed track must not come back
// as "unknown" just because its own field is empty.
func TestTrackDurationFallsBackToFormat(t *testing.T) {
	info := probeDoc(t, shrekTargetProbeJSON)
	if len(info.Tracks) != 1 || info.Tracks[0].Index != 1 {
		t.Fatalf("tracks = %+v, want the audio stream at index 1", info.Tracks)
	}
	// AudioTrack.Duration keeps its meaning: the stream field only.
	if got := info.Tracks[0].Duration; got != 0 {
		t.Errorf("AudioTrack.Duration = %v, want 0 for a N/A stream field", got)
	}
	got, ok := info.TrackDuration(1)
	if !ok {
		t.Fatal("TrackDuration ok = false, want true from the format duration")
	}
	if want := 5431772 * time.Millisecond; got != want {
		t.Errorf("TrackDuration = %v, want %v", got, want)
	}
}

// TestTrackDurationStreamWins: a stream duration that disagrees with the
// container's wins outright — no averaging, no preference for the longer value.
func TestTrackDurationStreamWins(t *testing.T) {
	const doc = `{
  "streams": [
    {"index":0,"codec_name":"aac","codec_type":"audio","channels":2,"duration":"100.500000","start_time":"0.000000"},
    {"index":1,"codec_name":"ac3","codec_type":"audio","channels":6,"duration":"98.250000","start_time":"1.400000"},
    {"index":2,"codec_name":"h264","codec_type":"video"}
  ],
  "format":{"format_name":"matroska,webm","duration":"123.456000","start_time":"0.000000"}
}`
	info := probeDoc(t, doc)
	cases := []struct {
		index int
		want  time.Duration
	}{
		{0, 100500 * time.Millisecond},
		{1, 98250 * time.Millisecond},
		// No stream at index 2 (it is video, so probe skips it): the container
		// duration is the only number left, and it is still a real answer.
		{2, 123456 * time.Millisecond},
	}
	for _, c := range cases {
		got, ok := info.TrackDuration(c.index)
		if !ok || got != c.want {
			t.Errorf("TrackDuration(%d) = %v/%v, want %v/true", c.index, got, ok, c.want)
		}
	}
}

// TestTrackDurationUnknownWhenNothingReported: no stream duration and no
// format duration is "unknown", never 0-as-a-fact.
func TestTrackDurationUnknownWhenNothingReported(t *testing.T) {
	const doc = `{
  "streams": [
    {"index":0,"codec_name":"aac","codec_type":"audio","channels":2,"duration":"N/A"}
  ],
  "format":{"format_name":"matroska,webm","duration":"N/A"}
}`
	info := probeDoc(t, doc)
	got, ok := info.TrackDuration(0)
	if ok {
		t.Fatalf("TrackDuration ok = true (%v), want false when nothing is reported", got)
	}
	if got != 0 {
		t.Errorf("TrackDuration = %v, want the zero value alongside ok=false", got)
	}
}

// TestStartTimeNonZeroIsRead is the start_time half of U-WIN.1: a container
// whose reference stream starts 0.5 s late must report that, not 0.
func TestStartTimeNonZeroIsRead(t *testing.T) {
	const doc = `{
  "streams": [
    {"index":0,"codec_name":"aac","codec_type":"audio","channels":2,"start_time":"0.500000","duration":"N/A"}
  ],
  "format":{"format_name":"matroska,webm","duration":"10.000000","start_time":"0.021000"}
}`
	info := probeDoc(t, doc)
	if !info.StartTimeKnown || info.StartTime != 21*time.Millisecond {
		t.Errorf("container start = %v/%v, want 21ms/true", info.StartTime, info.StartTimeKnown)
	}
	track := info.Tracks[0]
	if !track.StartTimeKnown || track.StartTime != 500*time.Millisecond {
		t.Errorf("track start = %v/%v, want 500ms/true", track.StartTime, track.StartTimeKnown)
	}
}

// TestStartTimeUnknownIsNotZero: absent, N/A and unparseable all mean unknown.
// The bool is what keeps "the container says 0" distinguishable from "the
// container says nothing", because both carry the value 0.
func TestStartTimeUnknownIsNotZero(t *testing.T) {
	docs := map[string]string{
		"absent": `{
		  "streams":[{"index":0,"codec_name":"aac","codec_type":"audio","channels":2,"duration":"10.0"}],
		  "format":{"format_name":"matroska,webm","duration":"10.000000"}
		}`,
		"N/A": `{
		  "streams":[{"index":0,"codec_name":"aac","codec_type":"audio","channels":2,"duration":"10.0","start_time":"N/A"}],
		  "format":{"format_name":"matroska,webm","duration":"10.000000","start_time":"N/A"}
		}`,
		"unparseable": `{
		  "streams":[{"index":0,"codec_name":"aac","codec_type":"audio","channels":2,"duration":"10.0","start_time":"bogus"}],
		  "format":{"format_name":"matroska,webm","duration":"10.000000","start_time":"bogus"}
		}`,
	}
	for name, doc := range docs {
		info := probeDoc(t, doc)
		if info.StartTimeKnown || info.StartTime != 0 {
			t.Errorf("%s: container start = %v/%v, want 0/false", name, info.StartTime, info.StartTimeKnown)
		}
		if info.Tracks[0].StartTimeKnown || info.Tracks[0].StartTime != 0 {
			t.Errorf("%s: track start = %v/%v, want 0/false",
				name, info.Tracks[0].StartTime, info.Tracks[0].StartTimeKnown)
		}
	}
}

// TestStartTimeZeroIsKnown: a container that states 0 stated something.
func TestStartTimeZeroIsKnown(t *testing.T) {
	info := probeDoc(t, shrekTargetProbeJSON)
	if !info.StartTimeKnown || info.StartTime != 0 {
		t.Errorf("container start = %v/%v, want 0/true (ffprobe reports 0.000000)",
			info.StartTime, info.StartTimeKnown)
	}
	if !info.Tracks[0].StartTimeKnown || info.Tracks[0].StartTime != 0 {
		t.Errorf("track start = %v/%v, want 0/true", info.Tracks[0].StartTime, info.Tracks[0].StartTimeKnown)
	}
}

// TestShrekSourceTimings measures the real fixtures: the two analysis inputs
// the next unit must window. The donor's track is the shorter one, which is why
// percentages must be taken over min(target, donor).
func TestShrekSourceTimings(t *testing.T) {
	const (
		targetName = "Shrek (2001) - [ArcA310][Remux-2160p][HDR10][Opus 7.1][AV1][imdb-tt0126029]-FraMeSToR.mkv"
		donorName  = "Shrek.2001.1080p.SKST.WEB-DL.MULTi.DDP5.1.H.264-KIFR.mkv"
		// Tolerances the acceptance contract states, in seconds.
		tolerance = 0.05
	)
	root := filepath.Join("..", "..", "..", "testfiles")
	targetPath := filepath.Join(root, targetName)
	donorPath := filepath.Join(root, donorName)
	if _, err := os.Stat(targetPath); err != nil {
		t.Skipf("media fixtures unavailable (%v); this test needs testfiles/ at the repository root", err)
	}
	if _, err := os.Stat(donorPath); err != nil {
		t.Skipf("media fixtures unavailable (%v); this test needs testfiles/ at the repository root", err)
	}

	p := New(Dependencies{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	targetInfo, err := p.Probe(ctx, targetPath)
	if err != nil {
		t.Fatalf("probe target: %v", err)
	}
	// The reference audio of the remux is stream 1 (stream 0 is the video).
	targetDur, ok := targetInfo.TrackDuration(1)
	if !ok {
		t.Fatalf("target TrackDuration(1) unknown; tracks = %+v", targetInfo.Tracks)
	}
	if got := targetDur.Seconds(); math.Abs(got-5431.772) > tolerance {
		t.Errorf("target duration = %.3f s, want 5431.772 s ±%.2f", got, tolerance)
	}
	if !targetInfo.StartTimeKnown {
		t.Errorf("target container start_time unknown, want the ffprobe value (0.000000)")
	}
	if !targetInfo.Tracks[0].StartTimeKnown {
		t.Errorf("target track start_time unknown, want the ffprobe value (0.000000)")
	}

	donorInfo, err := p.Probe(ctx, donorPath)
	if err != nil {
		t.Fatalf("probe donor: %v", err)
	}
	donorDur, ok := donorInfo.TrackDuration(donorInfo.Tracks[0].Index)
	if !ok {
		t.Fatalf("donor TrackDuration unknown; tracks = %+v", donorInfo.Tracks)
	}
	if got := donorDur.Seconds(); math.Abs(got-5431.232) > tolerance {
		t.Errorf("donor duration = %.3f s, want 5431.232 s ±%.2f", got, tolerance)
	}

	// DURATION BASE: the donor is the shorter source, so the planner's base is
	// the donor's length; taking the target's would push the last window past
	// the donor's EOF.
	if min := math.Min(targetDur.Seconds(), donorDur.Seconds()); min != donorDur.Seconds() {
		t.Errorf("min(target, donor) = %.3f, want the donor's %.3f", min, donorDur.Seconds())
	}
}
