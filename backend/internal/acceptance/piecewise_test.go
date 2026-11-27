package acceptance

// End-to-end, generated media (D10): the piecewise render on real
// ffmpeg/mkvmerge output, with no fixture from testfiles/ and nothing
// committed.
//
// Two defects survived review on #84 because nothing measured the *published
// track*: a plan that stopped at the last sampled window (so the render lost
// its tail) and a zero-offset mux over a plan that did not start at 0 (so the
// whole track was placed early). Both were fixed in the analysis and the
// renderer, and both are invisible in the report: `segments`, `verdict` and the
// mux's `--sync 0` all stay plausible while the audio is wrong. This test
// therefore measures the artifact, not the plan:
//
//  1. the published track's length equals the target track's length (the tail);
//  2. a marker tone in the first second lands where the target has it (the
//     head — the zero-offset shift);
//  3. a marker tone on the far side of the join lands where the target has it
//     (the step was actually corrected, per segment, not by one offset);
//  4. the video bitstream is untouched ([videoStreamHash], ADR-0004).
//
// The media is built at test time, in the test's own temp tree:
//
//	programme  = deterministic noise (a fixed PRNG seed), 48 kHz mono, 200 s,
//	             plus a 1000 Hz marker at 0.2 s and a 1500 Hz marker at 125 s;
//	target.mkv = testsrc2 video + the programme as an AC3 "eng" track;
//	dub.mka    = the programme with a 500 ms lead and a 2 s hole at 105 s
//	             (FLAC), i.e. one constant offset per run, 2000 ms apart.
//
// The markers sit well outside the analysis band (30-200 Hz, the
// lowpass_fallback pair this fixture resolves to), so they are evidence for the
// assertion and never an input to the offsets.
//
// Why 200 s and a 15 s step: the settings ranges the analyzer itself enforces
// (step_sec >= 15, window_sec >= 20, min_segment_sec >= 30) and the segmenter's
// eight-confident-checkpoints floor leave no smaller fixture that can carry two
// three-point sides. At 15 s the plan is 12 windows centred from 7.5 s to
// 172.5 s, and the join at 105 s falls exactly between the 97.5 s and 112.5 s
// centres. Each of those two windows straddles the join holding 17.5 s of one
// run and 2.5 s of the other, so each should report the offset of the run that
// fills it and the step between them should put the boundary at 105.0 s. That
// is the expectation the plan assertions below are written against; if the
// segmenter answers differently this test says so with the numbers.
//
// Skipping: like every acceptance test, this skips when the toolchain is
// absent (requireAcceptanceTools) and when the temp filesystem is too small
// (newHarness). It needs no real media, so it is not behind DUBARR_PARITY.
// Nothing here reads testfiles/ and nothing writes outside t.TempDir.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/mux"
	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/reflang"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
	syncengine "github.com/Subject412/dubarr/backend/internal/sync"
	"github.com/Subject412/dubarr/backend/internal/syncjob"
	"github.com/Subject412/dubarr/backend/internal/syncworker"
	"github.com/Subject412/dubarr/backend/internal/vault"
)

// The generated fixture. Positions are target-timeline seconds; the marker
// positions are the truth the published track is measured against.
//
// piecewiseCutSec is the scene the dub has and the target does not,
// piecewiseLeadSec the dub's whole-track delay and piecewiseGapSec the inserted
// scene: the two runs' offsets are lead and lead+gap apart, so the step to be
// corrected is exactly piecewiseGapSec.
const (
	piecewiseRate      = 48000
	piecewiseTargetSec = 200.0
	piecewiseCutSec    = 105.0
	piecewiseLeadSec   = 0.5
	piecewiseGapSec    = 2.0
	piecewiseToneSec   = 0.3
	piecewiseToneAmp   = 0.55
	piecewiseNoiseAmp  = 0.28
)

// The markers. piecewiseHeadAtSec is "the first second of the donor";
// piecewiseAfterAtSec is one sampling step past the join, so it has to be in the
// second segment whatever the segmenter's ±step/2 boundary estimate does — or
// the assertion would be measuring the boundary rather than the step.
const (
	piecewiseHeadFreqHz  = 1000
	piecewiseHeadAtSec   = 0.2
	piecewiseAfterFreqHz = 1500
	piecewiseAfterAtSec  = 125.0
)

// The tolerances the failure modes ask for. The head bound sits far above the
// detector's own ±1 ms resolution — it has to absorb container start_time
// quantisation and resampler smear — and far below the 500 ms shift it has to
// catch (the dub's lead), which on a plan that started at the first kept window
// would be 7.5 s. The join bound is deliberately looser than the head bound:
// the join is placed by a segmentation whose own uncertainty is ±step/2, and the
// assertion is about the step (2000 ms), not about that uncertainty.
const (
	piecewiseHeadTolerance = 50 * time.Millisecond
	piecewiseJoinTolerance = 100 * time.Millisecond
)

// Analysis sample rate the marker detector decodes at: a bin-aligned 8 kHz,
// because 1000 Hz and 1500 Hz are then exactly bins 20 and 30 of the 20 ms
// frame, so the two markers cannot leak into each other's bin.
const (
	piecewiseDecodeRate = 8000
	piecewiseFrameLen   = 160 // 20 ms at 8 kHz
	piecewiseHop        = 8   // 1 ms: the onset is stated to a millisecond
)

// TestPiecewiseEndToEndKeepsTailAndHead runs one real piecewise job over
// generated media and measures the published track.
func TestPiecewiseEndToEndKeepsTailAndHead(t *testing.T) {
	requireAcceptanceTools(t)
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	dir := filepath.Join(h.root, "generated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}

	// 1. The programme and the re-cut dub, as PCM. Both are written by this
	// test into its temp tree: nothing is committed and nothing is read from
	// testfiles/.
	programmeWAV := filepath.Join(dir, "programme.wav")
	dubWAV := filepath.Join(dir, "dub.wav")
	programme := piecewiseProgramme()
	piecewiseWriteWAV(t, programmeWAV, programme)
	piecewiseWriteWAV(t, dubWAV, piecewiseDub(programme))

	// 2. The target: a synthetic picture with the programme as its original
	// ("eng") track, encoded so the analysis has a real container to seek.
	target := filepath.Join(h.media, "Piecewise Programme (2026).mkv")
	runTool(t, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=160x120:rate=12:duration=%.3f", piecewiseTargetSec),
		"-i", programmeWAV,
		"-map", "0:v:0", "-map", "1:a:0",
		"-c:v", "mpeg4", "-b:v", "120k", "-pix_fmt", "yuv420p",
		"-c:a", "ac3", "-b:a", "192k",
		"-metadata:s:a:0", "language=eng",
		"-shortest", target)

	// 3. The donor dub: FLAC, so the markers the assertion reads are the exact
	// samples the fixture wrote and the published track is measured without a
	// codec's own transient shaping in the way.
	//
	// It is encoded into WORK_DIR/extract, not into this test's scratch dir:
	// the vault archives only a track staged inside work/extract (ADR-0002/
	// 0015), the same place a manual pick leaves one (stageDonorStream).
	stagingDir := filepath.Join(h.work, "extract", "manual")
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		t.Fatalf("mkdir staging %s: %v", stagingDir, err)
	}
	dubMedia := filepath.Join(stagingDir, "piecewise-dub.mka")
	runTool(t, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-i", dubWAV, "-c:a", "flac", dubMedia)

	preVideo := videoStreamHash(t, target)
	// The target's own length, decoded the same way the published track will
	// be: one measurement, applied to both sides.
	targetSamples := piecewiseDecodeTrack(t, target, piecewiseAudioStream(t, target, "eng"),
		filepath.Join(dir, "target-decoded.wav"))

	// 4. Vault + library rows, exactly as the extract path and the API leave
	// them, then one queued job the real worker drains.
	dubInfo, err := probe.New(probe.Dependencies{}).Probe(ctx, dubMedia)
	if err != nil {
		t.Fatalf("probe %s: %v", dubMedia, err)
	}
	if len(dubInfo.Tracks) != 1 {
		t.Fatalf("generated dub audio tracks = %d, want 1", len(dubInfo.Tracks))
	}
	dubTrack := dubInfo.Tracks[0]
	dub := h.archive(t, vault.Request{
		StagedPath: dubMedia, BaseName: "Piecewise Programme (2026)", Language: "pl",
		Variant: "dubbing",
		Media: store.VaultMediaRecord{MediaType: "movie", Title: "Piecewise Programme",
			Year: 2026, OriginalLanguageISO2: "en"},
		GroupUUID: "piecewise-group", Codec: dubTrack.Codec,
		Channels: channelsLabel(dubTrack), ChannelCount: dubTrack.Channels,
		DurationMS: dubInfo.DurationMS, SourceReleaseName: filepath.Base(dubMedia),
	})

	instanceID := h.insertInstance(t, "http://127.0.0.1:1", false)
	libraryID := h.insertLibraryRow(t, instanceID, "Piecewise Programme", 2026, "en", target)

	h.enablePiecewiseStrategy(t, analyze.PiecewisePolicy{
		Enabled: true,
		// The smallest step and window the analyzer's own ranges allow, and the
		// shortest segment: 12 checkpoints, a join that lands between two of
		// them, and two runs of 90 s and 60 s of sampled film.
		StepSec: 15, WindowSec: 20,
		SplitPenaltyMS: 250, MinSegmentSec: 30, MaxSegments: 12,
	})

	rec, err := h.st.CreateSyncJob(ctx, store.SyncJobCreate{
		VaultTrackID: dub.ID, TargetLibraryCacheID: libraryID,
		TargetVideoPath: target, TargetLangISO2: "pl",
	})
	if err != nil {
		t.Fatalf("CreateSyncJob: %v", err)
	}

	engine := syncengine.New(syncengine.WithTimeout(testJobTimeout))
	jobSvc := syncjob.RegisterRoutes(http.NewServeMux(), syncjob.Dependencies{
		DB: h.st.DB, Settings: h.ss, WorkDir: h.work, Engine: budgetEngine{inner: engine},
		RequireAdmin: func(next http.Handler) http.Handler { return next },
	})
	worker, err := syncworker.New(syncworker.Dependencies{
		Store: h.st, Settings: h.ss, Executor: jobSvc,
		Muxer: mux.New(mux.Dependencies{}), Rescanner: noRescan{},
		WorkDir: h.work, Interval: time.Millisecond, Logf: h.logs.printf,
	})
	if err != nil {
		t.Fatalf("syncworker.New: %v", err)
	}
	if processed, err := worker.ProcessOnce(ctx); err != nil || !processed {
		t.Fatalf("syncworker.ProcessOnce = %v/%v, want true/nil; logs:\n%s", processed, err, h.logs.String())
	}

	final, ok, err := h.st.SyncJobByID(ctx, rec.ID)
	if err != nil || !ok {
		t.Fatalf("SyncJobByID(%d) = %v/%v", rec.ID, ok, err)
	}
	// The worker's terminal status (syncworker's statusCompleted), not the job
	// service's intermediate "done": after ProcessOnce the row is final.
	if final.Status != "completed" {
		t.Fatalf("job status = %q (%s), want completed; checkpoints: %s; logs:\n%s",
			final.Status, final.ErrorMessage, final.CheckpointsJSON, h.logs.String())
	}

	var report analyze.Report
	if err := json.Unmarshal([]byte(final.CheckpointsJSON), &report); err != nil {
		t.Fatalf("decode persisted report: %v", err)
	}
	if report.SyncMethod != reflang.LowpassFallback {
		t.Fatalf("sync_method = %q, this fixture documents %q (a pl-only donor group against an eng target)",
			report.SyncMethod, reflang.LowpassFallback)
	}
	// The premise of every assertion below: the job really took the piecewise
	// path. A rejection or a one-segment plan would leave the mux's single
	// offset in place and the markers would prove nothing about segments.
	if report.Piecewise == nil {
		t.Fatalf("report carries no piecewise block; logs:\n%s", h.logs.String())
	}
	if report.Piecewise.RejectedReason != "" {
		t.Fatalf("piecewise rejected the timeline (%s); segments %+v, offsets %v; logs:\n%s",
			report.Piecewise.RejectedReason, report.Piecewise.Segments, report.OffsetsMS, h.logs.String())
	}
	if report.Verdict != analyze.VerdictOK {
		t.Fatalf("verdict = %q (%s), want %q; offsets %v", report.Verdict, report.Reason,
			analyze.VerdictOK, report.OffsetsMS)
	}
	if got := len(report.Piecewise.Segments); got != 2 {
		t.Fatalf("segments = %d, want 2 (one cut at %.0f s); %+v; boundaries %+v; offsets %v; logs:\n%s",
			got, piecewiseCutSec, report.Piecewise.Segments, report.Piecewise.Boundaries,
			report.OffsetsMS, h.logs.String())
	}
	// The boundary's own resolution is the sampling step (±step/2 reported as
	// UncertaintySec), so that is the band it is asserted in. The render's
	// accuracy is a separate, tighter claim, made on the markers below.
	step := report.Piecewise.StepSec
	if step <= 0 {
		step = 15
	}
	if len(report.Piecewise.Boundaries) != 1 {
		t.Fatalf("boundaries = %d, want 1; %+v", len(report.Piecewise.Boundaries), report.Piecewise.Boundaries)
	}
	boundary := report.Piecewise.Boundaries[0]
	if d := math.Abs(boundary.AtSec - piecewiseCutSec); d > step {
		t.Fatalf("boundary at %.3f s, want %.0f s ±%.0f s (one sampling step); step %v",
			boundary.AtSec, piecewiseCutSec, step, report.Piecewise.StepSec)
	}
	if report.Application == nil {
		t.Fatalf("report.application is absent, so the row does not say the cut was rendered")
	}
	if want := "re-encoded (piecewise, 2 segments)"; report.Application.Note != want {
		t.Fatalf("application.note = %q, want %q", report.Application.Note, want)
	}
	if report.Application.SegmentCount != 2 {
		t.Fatalf("application.segment_count = %d, want 2", report.Application.SegmentCount)
	}
	// The offset the mux was given: 0 by design, because the render already
	// placed every run. Asserted here because it is the mechanism the head
	// assertion below is a measurement of.
	if final.CalculatedOffsetMS != 0 {
		t.Fatalf("stored sync offset = %d ms, want 0 (a piecewise render is aligned by the render, not by mkvmerge)",
			final.CalculatedOffsetMS)
	}

	// 5. The published track: the appended Polish stream of the replaced target.
	published := strings.TrimSpace(final.OutputPath)
	if published == "" {
		published = target
	}
	pubIdx := piecewiseAudioStream(t, published, "pol")
	publishedSamples := piecewiseDecodeTrack(t, published, pubIdx,
		filepath.Join(dir, "published-decoded.wav"))

	// Assertion 1 — the tail. A plan that stopped at the last sampled window
	// would publish a track ~17.5 s short (the dense grid ends one window and
	// one margin before the film does); the bound is the one transcode itself
	// verifies against, so nothing inside this file is looser than the gate
	// that lets the file be published at all.
	verifyToleranceMS := int64(piecewiseTargetSec * 1000 / 50) // Verify: 2 %
	if verifyToleranceMS < 1000 {
		verifyToleranceMS = 1000
	}
	targetMS := piecewiseMillis(len(targetSamples))
	publishedMS := piecewiseMillis(len(publishedSamples))
	if d := publishedMS - targetMS; d > verifyToleranceMS || d < -verifyToleranceMS {
		t.Fatalf("published track is %d ms long, target is %d ms (Δ %+d ms, tolerance %d ms): the render did not keep the film's tail; segments %+v; logs:\n%s",
			publishedMS, targetMS, d, verifyToleranceMS, report.Piecewise.Segments, h.logs.String())
	}

	// Assertion 2 — the head. The dub is 500 ms late as a whole, so a render
	// that started at the dub's own time 0 (or a mux that applied the first
	// run's offset on top of the cut) would place this marker at 0.7 s.
	headAt, headOK := piecewiseToneOnset(publishedSamples, piecewiseHeadFreqHz)
	if !headOK {
		t.Fatalf("the %d Hz head marker is not in the published track at all", piecewiseHeadFreqHz)
	}
	headWant := piecewiseDuration(piecewiseHeadAtSec)
	if d := headAt - headWant; d > piecewiseHeadTolerance || d < -piecewiseHeadTolerance {
		t.Fatalf("head marker at %v, want %v (Δ %+v, tolerance %v): the published track is shifted at its start; segments %+v; logs:\n%s",
			headAt, headWant, d, piecewiseHeadTolerance, report.Piecewise.Segments, h.logs.String())
	}

	// Assertion 3 — the step. One median offset would leave this marker 2000 ms
	// late; only a per-segment cut places it where the target has it.
	joinAt, joinOK := piecewiseToneOnset(publishedSamples, piecewiseAfterFreqHz)
	if !joinOK {
		t.Fatalf("the %d Hz marker on the far side of the join is not in the published track at all", piecewiseAfterFreqHz)
	}
	joinWant := piecewiseDuration(piecewiseAfterAtSec)
	if d := joinAt - joinWant; d > piecewiseJoinTolerance || d < -piecewiseJoinTolerance {
		t.Fatalf("marker after the join at %v, want %v (Δ %+v, tolerance %v): the %.0f ms step was not corrected; segments %+v; logs:\n%s",
			joinAt, joinWant, d, piecewiseJoinTolerance, piecewiseGapSec*1000,
			report.Piecewise.Segments, h.logs.String())
	}

	// Assertion 4 — the picture. ADR-0004: the mux copies the video bitstream.
	if got := videoStreamHash(t, target); got != preVideo {
		t.Fatalf("video stream hash changed: %s → %s (the target video must never be transcoded)", preVideo, got)
	}

	table := make([]string, 0, len(report.Checkpoints))
	for i, cp := range report.Checkpoints {
		table = append(table, fmt.Sprintf("%6.1fs %+9.3f ms q=%.3f", cp.SliceStartSec+cp.SliceLenSec/2,
			report.OffsetsMS[i], report.Qualities[i]))
	}
	t.Logf("piecewise end-to-end (generated, %.0f s target, cut at %.0f s, %d checkpoints, step %.0f s):\n"+
		"  segments: %+v\n  boundary: %.3f s ± %.3f s, step %+.0f ms\n"+
		"  duration: published %d ms vs target %d ms (Δ %+d ms, tolerance %d ms)\n"+
		"  head marker: %v vs %v (Δ %+v, tolerance %v)\n"+
		"  marker after the join: %v vs %v (Δ %+v, tolerance %v)\n"+
		"  checkpoints:\n    %s",
		piecewiseTargetSec, piecewiseCutSec, len(report.Checkpoints), step,
		report.Piecewise.Segments, boundary.AtSec, boundary.UncertaintySec, boundary.StepMS,
		publishedMS, targetMS, publishedMS-targetMS, verifyToleranceMS,
		headAt, headWant, headAt-headWant, piecewiseHeadTolerance,
		joinAt, joinWant, joinAt-joinWant, piecewiseJoinTolerance,
		strings.Join(table, "\n    "))
}

// enablePiecewiseStrategy switches segmentation on through the validated
// settings store the API uses, so the job reads it exactly as a real install's
// job would (syncjob → settings.KeySyncStrategy → analyze.Config.Piecewise).
func (h *harness) enablePiecewiseStrategy(t *testing.T, policy analyze.PiecewisePolicy) {
	t.Helper()
	ctx := context.Background()
	section := settings.StrategyOptions(analyze.DefaultStrategy())
	if raw, ok, err := h.ss.Get(ctx, settings.KeySyncStrategy); err != nil {
		t.Fatalf("read sync.strategy: %v", err)
	} else if ok {
		if err := json.Unmarshal(raw, &section); err != nil {
			t.Fatalf("decode sync.strategy: %v", err)
		}
	}
	// An edited preset is "custom" by the settings layer's own rule; saying so
	// here keeps the stored label honest instead of being rewritten behind the
	// test's back.
	section.Preset = analyze.PresetCustom
	section.Piecewise = policy
	raw, err := json.Marshal(section)
	if err != nil {
		t.Fatalf("encode sync.strategy: %v", err)
	}
	if err := h.ss.Set(ctx, settings.KeySyncStrategy, raw); err != nil {
		t.Fatalf("set sync.strategy: %v", err)
	}
	// The assertion below fails the job for the wrong reason if the switch did
	// not survive the store, so it is checked once, here.
	stored, ok, err := h.ss.Get(ctx, settings.KeySyncStrategy)
	if err != nil || !ok {
		t.Fatalf("re-read sync.strategy = %v/%v", ok, err)
	}
	var back settings.StrategyOptions
	if err := json.Unmarshal(stored, &back); err != nil {
		t.Fatalf("decode stored sync.strategy: %v", err)
	}
	if !back.Piecewise.Enabled {
		t.Fatalf("sync.strategy.piecewise.enabled did not survive the store: %s", stored)
	}
}

// piecewiseProgramme is the film's audio: deterministic noise plus the two
// marker tones, at 48 kHz mono.
//
// Noise is the analysis' best case and the honest one here: the same programme
// on both sides correlates to a single sharp lag in the 30-200 Hz band the
// lowpass pair uses, so an offset the pipeline reports is the offset the
// fixture contains rather than an artefact of a musical signal.
func piecewiseProgramme() []int16 {
	out := make([]int16, int(piecewiseTargetSec*piecewiseRate))
	rng := rand.New(rand.NewSource(84))
	for i := range out {
		out[i] = int16((rng.Float64()*2 - 1) * piecewiseNoiseAmp * 32767)
	}
	piecewiseAddTone(out, piecewiseHeadAtSec, piecewiseHeadFreqHz)
	piecewiseAddTone(out, piecewiseAfterAtSec, piecewiseAfterFreqHz)
	return out
}

// piecewiseAddTone mixes one marker burst into samples.
func piecewiseAddTone(samples []int16, atSec float64, freq int) {
	start := int(atSec * piecewiseRate)
	length := int(piecewiseToneSec * piecewiseRate)
	for i := 0; i < length && start+i < len(samples); i++ {
		v := float64(samples[start+i])/32768 +
			piecewiseToneAmp*math.Sin(2*math.Pi*float64(freq)*float64(i)/piecewiseRate)
		samples[start+i] = int16(math.Max(-1, math.Min(1, v)) * 32767)
	}
}

// piecewiseDub is the re-cut dub: the whole programme delayed by the lead, with
// the inserted scene (silence, as the fixture states it) at the join. Target
// time t is at dub time t+lead before the join and t+lead+gap after it, so the
// two runs' offsets differ by exactly gap (2000 ms) and one median offset would
// leave the second half 2000 ms late.
func piecewiseDub(programme []int16) []int16 {
	lead := int(piecewiseLeadSec * piecewiseRate)
	cut := int(piecewiseCutSec * piecewiseRate)
	gap := int(piecewiseGapSec * piecewiseRate)
	out := make([]int16, lead+len(programme)+gap)
	copy(out[lead:], programme[:cut])
	copy(out[lead+cut+gap:], programme[cut:])
	return out
}

// piecewiseWriteWAV writes 16-bit mono PCM at the fixture rate.
func piecewiseWriteWAV(t *testing.T, path string, samples []int16) {
	t.Helper()
	data := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(data[2*i:], uint16(s))
	}
	var buf bytes.Buffer
	write := func(v any) {
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			t.Fatalf("encode wav %s: %v", path, err)
		}
	}
	// The canonical 44-byte PCM header, in WAV field order: chunk id/size/form,
	// fmt chunk size, format (PCM), channels, sample rate, byte rate, block
	// align, bits per sample, then the data chunk id and size.
	write([]byte("RIFF"))
	write(uint32(36 + len(data)))
	write([]byte("WAVE"))
	write([]byte("fmt "))
	write(uint32(16))
	write(uint16(1))
	write(uint16(1))
	write(uint32(piecewiseRate))
	write(uint32(piecewiseRate * 2))
	write(uint16(2))
	write(uint16(16))
	write([]byte("data"))
	write(uint32(len(data)))
	buf.Write(data)
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// piecewiseReadWAV returns the 16-bit mono samples of a PCM WAV, walking the
// chunks so an ffmpeg-written LIST chunk is skipped rather than misread.
func piecewiseReadWAV(t *testing.T, path string) []int16 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(raw) < 12 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		t.Fatalf("%s is not a RIFF/WAVE file", path)
	}
	for off := 12; off+8 <= len(raw); {
		id := string(raw[off : off+4])
		size := int(binary.LittleEndian.Uint32(raw[off+4 : off+8]))
		body := off + 8
		switch id {
		case "fmt ":
			if body+16 > len(raw) {
				t.Fatalf("%s: truncated fmt chunk", path)
			}
			channels := int(binary.LittleEndian.Uint16(raw[body+2 : body+4]))
			bits := int(binary.LittleEndian.Uint16(raw[body+14 : body+16]))
			if channels != 1 || bits != 16 {
				t.Fatalf("%s: %d channels / %d bits, the detector reads mono 16-bit", path, channels, bits)
			}
		case "data":
			end := body + size
			if end > len(raw) {
				end = len(raw)
			}
			n := (end - body) / 2
			out := make([]int16, n)
			for i := 0; i < n; i++ {
				out[i] = int16(binary.LittleEndian.Uint16(raw[body+2*i:]))
			}
			return out
		}
		off = body + size + size%2
	}
	t.Fatalf("%s: no data chunk", path)
	return nil
}

// piecewiseDecodeTrack decodes one stream of a container to mono 16-bit PCM at
// the detector's rate and returns the samples. The decoded length is also how
// the track's duration is measured, in both directions of the duration
// assertion — one method, so the two numbers are comparable.
func piecewiseDecodeTrack(t *testing.T, path string, stream int, out string) []int16 {
	t.Helper()
	runTool(t, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-i", path,
		"-map", "0:"+strconv.Itoa(stream),
		"-ac", "1", "-ar", strconv.Itoa(piecewiseDecodeRate),
		"-c:a", "pcm_s16le", out)
	return piecewiseReadWAV(t, out)
}

// piecewiseAudioStream returns the container index of the audio stream tagged
// lang (ISO 639-1 or 639-2). Without a match it falls back to the last audio
// stream, which is the one the mux appends.
func piecewiseAudioStream(t *testing.T, path, lang string) int {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a",
		"-show_entries", "stream=index:stream_tags=language", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe audio streams of %s: %v", path, err)
	}
	var doc struct {
		Streams []struct {
			Index int `json:"index"`
			Tags  struct {
				Language string `json:"language"`
			} `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode ffprobe streams of %s: %v", path, err)
	}
	if len(doc.Streams) == 0 {
		t.Fatalf("%s carries no audio stream", path)
	}
	for _, s := range doc.Streams {
		tag := strings.ToLower(strings.TrimSpace(s.Tags.Language))
		if lang == "pl" || lang == "pol" {
			if tag == "pl" || tag == "pol" {
				return s.Index
			}
			continue
		}
		if tag == "en" || tag == "eng" {
			return s.Index
		}
	}
	return doc.Streams[len(doc.Streams)-1].Index
}

// piecewiseToneOnset finds the start of a tone at freq in decoded mono PCM at
// the detector's rate: a Goertzel magnitude per 1 ms step over a 20 ms frame,
// and the first frame whose magnitude reaches half the marker's amplitude.
//
// The half-amplitude threshold is what makes this an onset rather than a
// window fill: for a bin-aligned tone the magnitude is proportional to the
// fraction of the frame the tone covers, so half amplitude is half coverage,
// i.e. a frame centred on the true onset. Noise puts ~0.04 of the marker's
// amplitude into one bin (0.28 RMS noise over a 20 ms frame), an order of
// magnitude below the threshold.
func piecewiseToneOnset(samples []int16, freq int) (time.Duration, bool) {
	if len(samples) < piecewiseFrameLen {
		return 0, false
	}
	coeff := 2 * math.Cos(2*math.Pi*float64(freq)/float64(piecewiseDecodeRate))
	threshold := piecewiseToneAmp / 2
	run := 0
	for start := 0; start+piecewiseFrameLen <= len(samples); start += piecewiseHop {
		s1, s2 := 0.0, 0.0
		for n := start; n < start+piecewiseFrameLen; n++ {
			s0 := float64(samples[n])/32768 + coeff*s1 - s2
			s2 = s1
			s1 = s0
		}
		power := s1*s1 + s2*s2 - coeff*s1*s2
		if 2*math.Sqrt(math.Max(power, 0))/piecewiseFrameLen < threshold {
			run = 0
			continue
		}
		run++
		// Three consecutive 1 ms frames: one stray frame cannot be a marker.
		if run == 3 {
			cross := start - 2*piecewiseHop
			frames := float64(cross+piecewiseFrameLen/2) / piecewiseDecodeRate
			return time.Duration(frames * float64(time.Second)), true
		}
	}
	return 0, false
}

// piecewiseMillis converts a decoded sample count to milliseconds.
func piecewiseMillis(samples int) int64 {
	return int64(math.Round(float64(samples) * 1000 / piecewiseDecodeRate))
}

// piecewiseDuration converts fixture seconds to a duration without going
// through a float64 nanosecond count.
func piecewiseDuration(sec float64) time.Duration {
	return time.Duration(math.Round(sec*1e6)) * time.Microsecond
}
