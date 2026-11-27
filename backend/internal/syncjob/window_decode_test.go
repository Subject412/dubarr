package syncjob

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/dsp/wav"
	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/reflang"
	"github.com/Subject412/dubarr/backend/internal/sync"
)

// shrekFilter and the fixture layout of the Shrek pair, read from the committed
// golden's own provenance rather than hard-coded here (see shrekInputs).
const shrekGoldenPath = "../dsp/testdata/golden/shrek.json"

// oneSampleMS is the parity bound the phase states for a windowed decode: one
// sample at 8 kHz. It is not a fudge factor — a window that is off by a whole
// sample would move the peak, and everything past one sample is a seek bug.
const oneSampleMS = 1000.0 / 8000.0

// TestDecodeArgsWholeTrackIsUnchanged locks the argv the whole-track path has
// always produced. Both decodes now share one builder, so this is the test that
// keeps the refactor behaviour-preserving: any flag reordering for the window
// case must leave this list byte-identical.
func TestDecodeArgsWholeTrackIsUnchanged(t *testing.T) {
	got := decodeArgs(decodeRequest{
		Input:  "/media/in.mkv",
		Stream: 2,
		Filter: "highpass=f=80,lowpass=f=3500",
		Output: "/work/out.wav",
	})
	want := []string{
		"-v", "error", "-nostdin", "-y",
		"-i", "/media/in.mkv",
		"-map", "0:2",
		"-ac", "1", "-ar", "8000",
		"-af", "highpass=f=80,lowpass=f=3500",
		"-c:a", "pcm_f32le",
		"/work/out.wav",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("whole-track argv changed:\n got %q\nwant %q", got, want)
	}
}

// TestWindowRequestPrerollClamp covers the head-of-film case: a window at 1 s
// cannot have 2 s of pre-roll, so the decode starts at 0, the file holds one
// second of pre-roll, and the length is that one second plus the window.
func TestWindowRequestPrerollClamp(t *testing.T) {
	w := analyze.Window{PositionPct: 5, FilmStartSec: 1, LenSec: 10, TargetSeekSec: 1, DonorSeekSec: 1}
	req := windowRequest("/media/in.mkv", 1, "highpass=f=80,lowpass=f=3500", w, SideTarget, WindowPrerollSec, "/work/win/5/t.wav")
	if req.SeekSec == nil || req.DurationSec == nil {
		t.Fatalf("a window decode set no seek/duration: %+v", req)
	}
	if *req.SeekSec != 0 {
		t.Fatalf("seek = %v, want the clamp at 0", *req.SeekSec)
	}
	if want := 1 + w.LenSec; *req.DurationSec != want {
		t.Fatalf("duration = %v, want %v (1 s of pre-roll present + 10 s window)", *req.DurationSec, want)
	}
	args := decodeArgs(req)
	if !containsSeq(args, "-ss", "0.000000") || !containsSeq(args, "-t", "11.000000") {
		t.Fatalf("argv = %q, want -ss 0.000000 and -t 11.000000", args)
	}
	// The gate then wants (preroll' + LenSec) * rate frames, i.e. 88000.
	if got, want := int64(math.Round(*req.DurationSec*windowSampleRate)), int64(88000); got != want {
		t.Fatalf("wanted frames = %d, want %d", got, want)
	}

	// Away from the head the full pre-roll is present: seek - preroll.
	w = analyze.Window{PositionPct: 50, FilmStartSec: 100, LenSec: 10, TargetSeekSec: 100.5, DonorSeekSec: 100}
	req = windowRequest("/media/in.mkv", 1, "f", w, SideTarget, WindowPrerollSec, "/work/t.wav")
	if *req.SeekSec != 98.5 || *req.DurationSec != 12 {
		t.Fatalf("seek/duration = %v/%v, want 98.5/12", *req.SeekSec, *req.DurationSec)
	}
	if !containsSeq(decodeArgs(req), "-ss", "98.500000") {
		t.Fatalf("seek does not precede -i with the container timestamp: %q", decodeArgs(req))
	}
	// The donor side reads its own planned seek, not the target's.
	req = windowRequest("/media/donor.mka", 0, "f", w, SideDonor, WindowPrerollSec, "/work/d.wav")
	if *req.SeekSec != 98 {
		t.Fatalf("donor seek = %v, want 98 (its own correction)", *req.SeekSec)
	}
}

// TestWindowFilePathNeverMatchesStagedGlob pins the naming rule: the mux stage
// globs {jobDir}/target-*.mka and fails when it finds two matches, so a window
// file must be neither under that name nor that extension.
func TestWindowFilePathNeverMatchesStagedGlob(t *testing.T) {
	jobDir := "/work/jobs/12"
	w := analyze.Window{PositionPct: 25}
	got := windowFilePath(jobDir, w, SideTarget)
	if want := filepath.Join(jobDir, "win", "25", "t.wav"); got != want {
		t.Fatalf("target window path = %q, want %q", got, want)
	}
	if want := filepath.Join(jobDir, "win", "25", "d.wav"); windowFilePath(jobDir, w, SideDonor) != want {
		t.Fatalf("donor window path = %q, want %q", windowFilePath(jobDir, w, SideDonor), want)
	}
	matches, err := filepath.Glob(filepath.Join(jobDir, "target-*.mka"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, m := range matches {
		if m == got {
			t.Fatalf("a window file matched the staged-track glob: %s", m)
		}
	}
}

// TestVerifyWindowShortFails is the frame gate's own test: 98% of the wanted
// frames, 63 frames and a wrong rate are all refused, and the exact count
// passes.
func TestVerifyWindowShortFails(t *testing.T) {
	dir := t.TempDir()
	const want = int64(96000) // 12 s at 8 kHz

	full := filepath.Join(dir, "full.wav")
	writeFloatWAV(t, full, int(want), windowSampleRate)
	if err := verifyWindow(full, want, windowSampleRate); err != nil {
		t.Fatalf("exact window rejected: %v", err)
	}

	short := filepath.Join(dir, "short.wav")
	writeFloatWAV(t, short, int(want*98/100), windowSampleRate)
	err := verifyWindow(short, want, windowSampleRate)
	var shortErr *ErrShortWindow
	if !errors.As(err, &shortErr) {
		t.Fatalf("98%% window error = %v, want *ErrShortWindow", err)
	}
	if shortErr.Frames != want*98/100 || shortErr.Want != want {
		t.Fatalf("error fields = %d/%d, want %d/%d", shortErr.Frames, shortErr.Want, want*98/100, want)
	}
	if msg := err.Error(); !strings.Contains(msg, "94080") || !strings.Contains(msg, "96000") {
		t.Fatalf("error does not name got/want frames: %q", msg)
	}

	tiny := filepath.Join(dir, "tiny.wav")
	writeFloatWAV(t, tiny, 63, windowSampleRate)
	if err := verifyWindow(tiny, 63, windowSampleRate); !errors.As(err, &shortErr) {
		t.Fatalf("63-frame window error = %v, want *ErrShortWindow", err)
	}

	// A file at the wrong rate is the same class of failure: resampling it
	// would hand the engine samples the plan never promised.
	ratey := filepath.Join(dir, "rate.wav")
	writeFloatWAV(t, ratey, int(want), 16000)
	err = verifyWindow(ratey, want, windowSampleRate)
	if !errors.As(err, &shortErr) {
		t.Fatalf("16 kHz window error = %v, want *ErrShortWindow", err)
	}
	if shortErr.Rate != 16000 || shortErr.WantRate != windowSampleRate {
		t.Fatalf("rate fields = %d/%d, want 16000/8000", shortErr.Rate, shortErr.WantRate)
	}
}

// TestDecodeWindowBoundsAndCleanup runs one real decode: it must hold exactly
// -t seconds, stay within the (preroll + window) budget, and leave nothing
// behind when it fails.
func TestDecodeWindowBoundsAndCleanup(t *testing.T) {
	requireTools(t, "ffmpeg")
	dir := t.TempDir()
	src := filepath.Join(dir, "src.wav")
	writeFloatWAV(t, src, 20*windowSampleRate, windowSampleRate)

	// seek 1 s, preroll 2 s: the clamp case, end to end.
	w := analyze.Window{PositionPct: 5, FilmStartSec: 1, LenSec: 10, TargetSeekSec: 1, DonorSeekSec: 1}
	out := windowFilePath(dir, w, SideTarget)
	if err := decodeWindow(context.Background(), "ffmpeg", src, 0, "anull", w, SideTarget, WindowPrerollSec, out); err != nil {
		t.Fatalf("decodeWindow: %v", err)
	}
	reader, err := wav.Open(out)
	if err != nil {
		t.Fatalf("open window: %v", err)
	}
	frames := reader.Frames()
	rate := reader.SampleRate()
	_ = reader.Close()
	if rate != windowSampleRate {
		t.Fatalf("window rate = %d, want %d", rate, windowSampleRate)
	}
	if want := int64((1 + w.LenSec) * windowSampleRate); frames != want {
		t.Fatalf("window frames = %d, want exactly %d", frames, want)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat window: %v", err)
	}
	// 8 kHz mono f32: at most (preroll + window) seconds of samples, plus a
	// WAV header that this budget does not try to account for.
	const headerSlack = 1024
	if budget := int64((WindowPrerollSec+w.LenSec)*windowSampleRate*4) + headerSlack; info.Size() > budget {
		t.Fatalf("window file is %d bytes, budget %d", info.Size(), budget)
	}

	// Failure path: a missing input must leave no file, and no half-written one.
	missing := windowFilePath(dir, w, SideDonor)
	err = decodeWindow(context.Background(), "ffmpeg", filepath.Join(dir, "absent.wav"), 0, "anull", w, SideDonor, WindowPrerollSec, missing)
	if err == nil {
		t.Fatal("decodeWindow succeeded on a missing input")
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("failed decode left %s behind (stat err %v)", missing, statErr)
	}
}

// TestWindowStartTimeCorrection is the negative control the whole phase rests
// on: a synthetic MKV whose video starts at 0 and whose audio starts at 0.5 s.
//
// The window at film 10 s is decoded twice — once at the plan's corrected seek
// (container 10.5 s) and once at the film position itself (10.0 s, i.e. what a
// plan without the start_time correction would ask for). The first lands on the
// reference audio and correlates at offset 0; the second reads audio half a
// second early and must report exactly -500 ms. Delete the correction and the
// second assertion is what goes red.
func TestWindowStartTimeCorrection(t *testing.T) {
	requireTools(t, "ffmpeg", "ffprobe", "mkvmerge")
	dir := t.TempDir()
	fixture, reference := writeStartTimeFixture(t, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	prober := probe.New(probe.Dependencies{})
	fixtureInfo, err := prober.Probe(ctx, fixture)
	if err != nil {
		t.Fatalf("probe fixture: %v", err)
	}
	referenceInfo, err := prober.Probe(ctx, reference)
	if err != nil {
		t.Fatalf("probe reference: %v", err)
	}
	plan, err := analyze.PlanWindows(analyze.Timing{
		Target: analyzeTiming(sourceTiming(*fixtureInfo, fixtureAudioStream)),
		Donor:  analyzeTiming(sourceTiming(*referenceInfo, 0)),
	}, []int{50}, 10)
	if err != nil {
		t.Fatalf("PlanWindows: %v", err)
	}
	corrected := plan[0]
	if corrected.FilmStartSec != 10 {
		t.Fatalf("plan start = %v, want 10 s", corrected.FilmStartSec)
	}
	if got := corrected.TargetSeekSec - corrected.FilmStartSec; math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("fixture does not carry the 0.5 s audio origin the control needs: correction = %v", got)
	}
	if corrected.DonorSeekSec != corrected.FilmStartSec {
		t.Fatalf("donor seek = %v, want the film position %v", corrected.DonorSeekSec, corrected.FilmStartSec)
	}

	// The same plan with the correction dropped: this is the "without it" arm.
	uncorrected := corrected
	uncorrected.TargetSeekSec = uncorrected.FilmStartSec

	// Both arms are asserted exactly, not within a sample: the fixture's audio is
	// PCM at the analysis rate, so the only difference between a window decode
	// and the reference is where it started, and a two-second pre-roll makes even
	// the filter state identical at the analysed sample. An offset of "roughly
	// 500 ms" would be the very ambiguity this control exists to remove.
	//
	// The uncorrected arm reads +500 ms, not -500. That sign is the engine's
	// convention (sync.Result.OffsetMS = target time - donor time, "positive means
	// the donor must be delayed"): the uncorrected target window holds the film's
	// 9.5 s audio while both sides are labelled 10 s, so at equal content
	// t_target - t_donor = +0.5 s. The magnitude is the start_time error the
	// correction removes; the sign is what the mux stage will act on.
	for _, arm := range []struct {
		name string
		w    analyze.Window
		want float64
	}{
		{"with-correction", corrected, 0},
		{"without-correction", uncorrected, 500},
	} {
		t.Run(arm.name, func(t *testing.T) {
			target := windowFilePath(filepath.Join(dir, arm.name), arm.w, SideTarget)
			donor := windowFilePath(filepath.Join(dir, arm.name), arm.w, SideDonor)
			// The reference WAV is already band-filtered, so re-cutting it must
			// not filter it again; the fixture is decoded with the production
			// filter, exactly as a real job would.
			if err := decodeWindow(ctx, "ffmpeg", fixture, fixtureAudioStream, analysisFilter(reflang.SameLangReference), arm.w, SideTarget, WindowPrerollSec, target); err != nil {
				t.Fatalf("decode target window: %v", err)
			}
			if err := decodeWindow(ctx, "ffmpeg", reference, 0, "anull", arm.w, SideDonor, WindowPrerollSec, donor); err != nil {
				t.Fatalf("decode donor window: %v", err)
			}
			result := runWindowEngine(t, target, donor, arm.w.LenSec)
			if result.CorrelationQuality < 0.9 {
				t.Fatalf("quality %.3f: the offset below would be a spurious peak, not the control",
					result.CorrelationQuality)
			}
			if result.OffsetMS != arm.want {
				t.Fatalf("offset = %.3f ms, want exactly %.3f ms", result.OffsetMS, arm.want)
			}
			t.Logf("%s: offset %.3f ms, quality %.3f", arm.name, result.OffsetMS, result.CorrelationQuality)
		})
	}
}

// TestWindowSeekParityOnRealMaterial proves the primitive against the recorded
// answer: the golden's checkpoint, decoded as a window from the real sources,
// must correlate to the same offset and quality the reference engine recorded
// from a full-decode slice. This is the one-entry version of U-WIN.8.
func TestWindowSeekParityOnRealMaterial(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	golden := readShrekGolden(t)
	if len(golden.Entries) == 0 {
		t.Fatal("the shrek golden records no entries")
	}
	target, donor, filter := shrekInputs(t)
	entry := golden.Entries[0]
	if _, err := os.Stat(target); err != nil {
		t.Skipf("testfiles/ not present (%v)", err)
	}
	if _, err := os.Stat(donor); err != nil {
		t.Fatalf("target fixture is present but the donor is not: %v", err)
	}

	// Both Shrek inputs report start_time 0 (the goldens were cut at the same
	// track time from a full decode), so the seek is the film position; the
	// correction itself is covered by TestWindowStartTimeCorrection.
	w := analyze.Window{
		PositionPct:   entry.PositionPct,
		FilmStartSec:  entry.SourceStartSec,
		LenSec:        10,
		TargetSeekSec: entry.SourceStartSec,
		DonorSeekSec:  entry.SourceStartSec,
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	targetWindow := windowFilePath(dir, w, SideTarget)
	donorWindow := windowFilePath(dir, w, SideDonor)
	if err := decodeWindow(ctx, "ffmpeg", target, goldenStream(t, "target"), filter, w, SideTarget, WindowPrerollSec, targetWindow); err != nil {
		t.Fatalf("decode target window: %v", err)
	}
	if err := decodeWindow(ctx, "ffmpeg", donor, goldenStream(t, "donor"), filter, w, SideDonor, WindowPrerollSec, donorWindow); err != nil {
		t.Fatalf("decode donor window: %v", err)
	}
	result := runWindowEngine(t, targetWindow, donorWindow, w.LenSec)
	if math.Abs(result.OffsetMS-entry.Result.OffsetMS) > oneSampleMS {
		t.Fatalf("%s offset = %.3f ms, golden %.3f ms", entry.ID, result.OffsetMS, entry.Result.OffsetMS)
	}
	if math.Abs(result.CorrelationQuality-entry.Result.CorrelationQuality) > 0.02 {
		t.Fatalf("%s quality = %.3f, golden %.3f", entry.ID, result.CorrelationQuality, entry.Result.CorrelationQuality)
	}
	t.Logf("%s: window offset %.3f ms / quality %.3f, golden %.3f ms / %.3f",
		entry.ID, result.OffsetMS, result.CorrelationQuality,
		entry.Result.OffsetMS, entry.Result.CorrelationQuality)
}

// ---------------------------------------------------------------------------
// helpers

// fixtureAudioStream is the audio track's index in the synthetic MKV: video is
// written first (0), audio second (1).
const fixtureAudioStream = 1

// requireTools skips (naming the tool) when a tool the test shells out to is
// absent, the way the rest of this package's media tests do.
func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
}

// runWindowEngine runs the in-process engine over the two window files, telling
// it to skip the pre-roll exactly as a windowed job does.
func runWindowEngine(t *testing.T, target, donor string, lenSec float64) sync.Result {
	t.Helper()
	engine := sync.New(sync.WithTimeout(2 * time.Minute))
	window := lenSec
	result, err := engine.Run(context.Background(), sync.Job{
		TargetPath:    target,
		DonorPath:     donor,
		SliceStartSec: WindowPrerollSec,
		SliceLenSec:   lenSec,
		WindowSec:     &window,
	})
	if err != nil {
		t.Fatalf("engine over decoded windows: %v", err)
	}
	return result
}

// analyzeTiming converts this package's probed timing (durations) into the
// planner's view (seconds). The probe projection itself stays production code
// (sourceTiming), so the control tests the same numbers a job would use.
func analyzeTiming(s SourceTiming) analyze.SourceTiming {
	return analyze.SourceTiming{
		DurationSec:       s.Duration.Seconds(),
		ContainerStartSec: s.ContainerStart.Seconds(),
		TrackStartSec:     s.TrackStart.Seconds(),
		OriginKnown:       s.Known,
	}
}

// containsSeq reports whether args holds seq as consecutive elements.
func containsSeq(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		ok := true
		for j, s := range seq {
			if args[i+j] != s {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// writeFloatWAV writes frames of 32-bit float silence at rate. The gate reads
// headers and counts, so silence is enough — and it keeps the file exactly
// frames*4 + 44 bytes long.
func writeFloatWAV(t *testing.T, path string, frames, rate int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var buf bytes.Buffer
	le := binary.LittleEndian
	dataLen := uint32(frames * 4)
	buf.WriteString("RIFF")
	binary.Write(&buf, le, 36+dataLen)
	buf.WriteString("WAVEfmt ")
	binary.Write(&buf, le, uint32(16))
	binary.Write(&buf, le, uint16(3)) // IEEE float
	binary.Write(&buf, le, uint16(1)) // mono
	binary.Write(&buf, le, uint32(rate))
	binary.Write(&buf, le, uint32(rate*4))
	binary.Write(&buf, le, uint16(4))
	binary.Write(&buf, le, uint16(32))
	buf.WriteString("data")
	binary.Write(&buf, le, dataLen)
	buf.Write(make([]byte, frames*4))
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeStartTimeFixture builds the negative control's material: an MKV whose
// video starts at container 0 and whose audio starts at 0.5 s, plus the full
// decode of its audio track, which is the reference a corrected window must
// land on.
//
// The audio is PCM at the analysis rate so the two decodes differ in nothing but
// where they started — a lossy codec's seek behaviour would blur the property
// under test. mkvmerge's `--sync 1:500` is what gives the audio track its 0.5 s
// first timestamp; the caller asserts the resulting start_time through
// [analyze.PlanWindows] before trusting the file.
func writeStartTimeFixture(t *testing.T, dir string) (fixture, reference string) {
	t.Helper()
	audio := filepath.Join(dir, "audio.wav")
	video := filepath.Join(dir, "video.mkv")
	fixture = filepath.Join(dir, "start-time-0.5.mkv")
	reference = filepath.Join(dir, "reference.wav")

	runTool(t, "ffmpeg",
		"-v", "error", "-y",
		"-f", "lavfi", "-i", "anoisesrc=d=30:c=pink:r=8000:a=0.5",
		"-ac", "1", "-c:a", "pcm_s16le", audio)
	runTool(t, "ffmpeg",
		"-v", "error", "-y",
		"-f", "lavfi", "-i", "color=c=black:s=160x120:r=8:d=30",
		"-c:v", "mpeg4", video)
	// Track ID 0 is the audio input's only track (mkvmerge numbers tracks per
	// input file); a positive delay shifts its first block to 0.5 s, which is
	// the container fact being tested.
	runTool(t, "mkvmerge", "-o", fixture, "--quiet",
		video, "--sync", "0:500", audio)

	// The reference: the audio track decoded in full, with the production
	// filter. A window decode re-applies that filter from its own start, and
	// after 2 s of pre-roll it reproduces these samples exactly (which is why
	// the parity bound is one sample).
	runTool(t, "ffmpeg",
		"-v", "error", "-y",
		"-i", fixture,
		"-map", fmt.Sprintf("0:%d", fixtureAudioStream),
		"-ac", "1", "-ar", "8000",
		"-af", analysisFilter(reflang.SameLangReference),
		"-c:a", "pcm_f32le", reference)
	if _, err := os.Stat(reference); err != nil {
		t.Fatalf("reference decode: %v", err)
	}
	return fixture, reference
}

// runTool runs one external tool, failing the test with its full command line
// and stderr — a fixture that could not be built must never look like a pass.
func runTool(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// The Shrek golden's shape, limited to what this test consumes: the recorded
// checkpoint and the provenance argv the capture tool used, which names the
// sources, their stream indices and the filter.
type shrekGolden struct {
	Provenance struct {
		Argv struct {
			Decode []struct {
				Source string   `json:"source"`
				Stream int      `json:"stream"`
				Argv   []string `json:"argv"`
			} `json:"decode"`
		} `json:"argv"`
	} `json:"provenance"`
	Entries []struct {
		ID             string  `json:"id"`
		PositionPct    int     `json:"position_pct"`
		SourceStartSec float64 `json:"source_start_sec"`
		Result         struct {
			OffsetMS           float64 `json:"offset_ms"`
			CorrelationQuality float64 `json:"correlation_quality"`
		} `json:"result"`
	} `json:"entries"`
}

// goldenDecode returns the golden's recorded decode of one side.
func goldenDecode(t *testing.T, side string) (source string, stream int, filter string) {
	t.Helper()
	golden := readShrekGolden(t)
	for _, d := range golden.Provenance.Argv.Decode {
		if len(d.Argv) == 0 || !strings.Contains(d.Argv[len(d.Argv)-1], "-"+side+".wav") {
			continue
		}
		for i := 0; i+1 < len(d.Argv); i++ {
			if d.Argv[i] == "-af" {
				filter = d.Argv[i+1]
			}
		}
		if filter == "" {
			t.Fatalf("golden decode of the %s records no -af", side)
		}
		return d.Source, d.Stream, filter
	}
	t.Fatalf("golden records no %s decode", side)
	return "", 0, ""
}

// shrekInputs resolves the pair's source paths against the repo root, which is
// three directories above this package.
func shrekInputs(t *testing.T) (target, donor, filter string) {
	t.Helper()
	targetSource, _, targetFilter := goldenDecode(t, "target")
	donorSource, _, donorFilter := goldenDecode(t, "donor")
	if targetFilter != donorFilter {
		t.Fatalf("golden filters differ: %q vs %q", targetFilter, donorFilter)
	}
	resolve := func(p string) string { return filepath.Join("..", "..", "..", p) }
	return resolve(targetSource), resolve(donorSource), targetFilter
}

// goldenStream resolves one side's stream index from the golden's argv.
func goldenStream(t *testing.T, side string) int {
	_, stream, _ := goldenDecode(t, side)
	return stream
}

// readShrekGolden parses the committed golden once per call: the file is small
// and only real-fixture tests read it.
func readShrekGolden(t *testing.T) shrekGolden {
	t.Helper()
	data, err := os.ReadFile(shrekGoldenPath)
	if err != nil {
		t.Fatalf("read shrek golden: %v", err)
	}
	var golden shrekGolden
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatalf("parse shrek golden: %v", err)
	}
	return golden
}
