package transcode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
}

func TestBuildArgsIsSinglePass(t *testing.T) {
	plan := Plan{
		Codec: CodecOpus, BitrateBPS: 160000, Channels: 2, ChannelLayout: "stereo",
		SampleRate: 48000, TempoFactor: 1.001,
	}
	args, err := BuildArgs(Request{Input: "/work/in.mka", Output: "/work/out.mka", Plan: plan, LangISO2: "pl", Title: "Polski Dubbing"})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-i /work/in.mka",
		"-c:a libopus",
		"-b:a 160000",
		"-ar 48000",
		"atempo=1.001000",
		"aformat=channel_layouts=stereo",
		"language=pl",
		"title=Polski Dubbing",
		"-f matroska /work/out.mka",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv %v missing %q", args, want)
		}
	}
	if strings.Count(joined, "-i ") != 1 || strings.Count(joined, "-c:a") != 1 {
		t.Fatalf("argv is not a single encode pass: %v", args)
	}
	// Tempo must precede the layout conversion so ffmpeg applies the stretch
	// before the downmix (documented precedence, Q20).
	if strings.Index(joined, "atempo=") > strings.Index(joined, "aformat=") {
		t.Fatalf("atempo must precede aformat: %v", args)
	}
}

func TestBuildArgsRejectsKeepAndUnknown(t *testing.T) {
	if _, err := BuildArgs(Request{Input: "in", Output: "out", Plan: Plan{Codec: CodecKeep}}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("keep plan error = %v, want ErrInvalidPlan", err)
	}
	if _, err := BuildArgs(Request{Input: "in", Output: "out", Plan: Plan{Codec: "mp3"}}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("unknown codec error = %v, want ErrInvalidPlan", err)
	}
}

func TestVerify(t *testing.T) {
	want := Expect{Codec: CodecOpus, Channels: 2, DurationMS: 60000}
	if err := Verify(StreamInfo{Codec: "opus", Channels: 2, DurationMS: 60000}, want); err != nil {
		t.Fatalf("Verify = %v, want nil", err)
	}
	if err := Verify(StreamInfo{Codec: "opus", Channels: 2, DurationMS: 61000}, want); err != nil {
		t.Fatalf("1 s drift within tolerance: %v", err)
	}
	cases := []struct {
		name string
		got  StreamInfo
	}{
		{"codec", StreamInfo{Codec: "aac", Channels: 2, DurationMS: 60000}},
		{"channels", StreamInfo{Codec: "opus", Channels: 6, DurationMS: 60000}},
		{"duration", StreamInfo{Codec: "opus", Channels: 2, DurationMS: 90000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Verify(tc.got, want); !errors.Is(err, ErrVerify) {
				t.Fatalf("Verify = %v, want ErrVerify", err)
			}
		})
	}
}

func TestEncoderAvailable(t *testing.T) {
	requireTools(t)
	tr := New(Dependencies{})
	for _, enc := range []string{"libopus", "aac", "flac", "pcm_s16le"} {
		ok, detail := tr.EncoderAvailable(context.Background(), enc)
		if !ok {
			t.Fatalf("encoder %s unavailable: %s", enc, detail)
		}
	}
	if ok, _ := tr.EncoderAvailable(context.Background(), "definitely_not_an_encoder"); ok {
		t.Fatal("bogus encoder reported available")
	}
	if ok, _ := tr.EncoderAvailable(context.Background(), ""); ok {
		t.Fatal("empty encoder reported available")
	}
}

func TestTranscodeOpusStereoFromPCM(t *testing.T) {
	requireTools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mka")
	// A 5.1 48 kHz PCM track, like a lossless vault track that a rule targets.
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=6:sample_rate=48000",
		"-af", "aformat=channel_layouts=5.1", "-c:a", "pcm_s16le", src).CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v: %s", err, out)
	}
	before := hashFile(t, src)
	out := filepath.Join(dir, "out.mka")

	plan := Plan{RuleIndex: 0, Codec: CodecOpus, BitrateBPS: 96000, Channels: 2, ChannelLayout: "stereo"}
	tr := New(Dependencies{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := tr.Transcode(ctx, Request{
		Input: src, Output: out, Plan: plan, SourceChannels: 6, SourceDurationMS: 6000,
		LangISO2: "pl", Title: "Polski Dubbing",
	})
	if err != nil {
		t.Fatalf("Transcode: %v", err)
	}
	if res.Codec != "opus" || res.Channels != 2 {
		t.Fatalf("result = %+v, want opus stereo", res)
	}
	// The input is never modified: transcode reads the Work Area copy only.
	if after := hashFile(t, src); after != before {
		t.Fatalf("input hash changed: %s -> %s", before, after)
	}

	// The encoded file itself carries the requested stream.
	tr2 := New(Dependencies{})
	got, err := tr2.probe(ctx, mustLookPath(t, "ffprobe"), out)
	if err != nil {
		t.Fatalf("probe output: %v", err)
	}
	if got.Codec != "opus" || got.Channels != 2 {
		t.Fatalf("probed output = %+v, want opus stereo", got)
	}
	if got.BitrateBPS == 0 {
		t.Fatalf("probed output has no bitrate: %+v", got)
	}
}

func TestTranscodeFailsWhenVerificationCannotRun(t *testing.T) {
	requireTools(t)
	dir := t.TempDir()
	plan := Plan{Codec: CodecOpus, BitrateBPS: 64000, Channels: 2, ChannelLayout: "stereo"}
	_, err := New(Dependencies{}).Transcode(context.Background(), Request{
		Input: filepath.Join(dir, "missing.mka"), Output: filepath.Join(dir, "out.mka"), Plan: plan,
	})
	if err == nil {
		t.Fatal("Transcode accepted a missing input")
	}
	if !errors.Is(err, ErrTool) {
		t.Fatalf("error = %v, want ErrTool", err)
	}
}

func hashFile(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func mustLookPath(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("LookPath %s: %v", name, err)
	}
	return path
}
