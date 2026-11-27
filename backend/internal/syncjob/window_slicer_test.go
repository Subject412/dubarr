package syncjob

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
	syncpkg "github.com/Subject412/dubarr/backend/internal/sync"
)

// The fixture durations mirror the real pair used by the source-timing work:
// the container states both, the target is half a second longer, and the
// analysed track starts half a second into the target container.
const (
	testTargetDur   = 5431772 * time.Millisecond
	testDonorDur    = 5431232 * time.Millisecond
	testTargetStart = 500 * time.Millisecond
	testDonorStart  = 20 * time.Millisecond
)

// TestModeDecision is the whole decision table: windows only when the setting
// asks for it, both inputs stated a duration, both stated an origin and the
// plan validates. Everything else is full, with a reason an operator can read.
func TestModeDecision(t *testing.T) {
	windows := analyze.DefaultConfig()
	windows.AnalysisSource = analyze.AnalysisSourceWindows
	full := analyze.DefaultConfig()
	full.AnalysisSource = analyze.AnalysisSourceFull

	known := SourceTimings{
		Target: SourceTiming{Duration: testTargetDur, Known: true},
		Donor:  SourceTiming{Duration: testDonorDur, Known: true},
	}
	noDonorDuration := known
	noDonorDuration.Donor.Duration = 0
	noTargetDuration := known
	noTargetDuration.Target.Duration = 0
	unknownDonorOrigin := known
	unknownDonorOrigin.Donor.Known = false
	unknownTargetOrigin := known
	unknownTargetOrigin.Target.Known = false
	// A file shorter than the window is not a planning failure: the planner
	// shrinks the window to the file (its own "file shorter than window" case),
	// so a 100 s film analysed with a 190 s window is simply the whole file.
	// reasonPlanInvalid therefore stays a defensive branch - the clamp cannot
	// produce an unplaceable window, which the planner's tests also record.
	shortFile := SourceTimings{
		Target: SourceTiming{Duration: 100 * time.Second, Known: true},
		Donor:  SourceTiming{Duration: 100 * time.Second, Known: true},
	}

	cases := []struct {
		name       string
		cfg        analyze.Config
		timings    SourceTimings
		wantSource string
		wantReason string
	}{
		{"windows and both timings", windows, known, analyze.AnalysisSourceWindows, ""},
		{"setting says full", full, known, analyze.AnalysisSourceFull, reasonSettingFull},
		{"donor duration unknown", windows, noDonorDuration, analyze.AnalysisSourceFull, reasonNoDuration},
		{"target duration unknown", windows, noTargetDuration, analyze.AnalysisSourceFull, reasonNoDuration},
		{"donor origin unknown", windows, unknownDonorOrigin, analyze.AnalysisSourceFull, reasonUnknownOrigin},
		{"target origin unknown", windows, unknownTargetOrigin, analyze.AnalysisSourceFull, reasonUnknownOrigin},
		{"no timing probed at all", windows, SourceTimings{}, analyze.AnalysisSourceFull, reasonNoDuration},
		{"plan does not fit", windows, shortFile, analyze.AnalysisSourceWindows, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode := decideAnalysisSource(tc.cfg, tc.timings)
			if mode.Source != tc.wantSource || mode.Reason != tc.wantReason {
				t.Fatalf("mode = %q/%q, want %q/%q", mode.Source, mode.Reason, tc.wantSource, tc.wantReason)
			}
			if mode.Source != analyze.AnalysisSourceWindows {
				if mode.DurationSec != 0 || mode.Checkpoints != 0 {
					t.Errorf("full mode carries duration %v and %d windows, want none",
						mode.DurationSec, mode.Checkpoints)
				}
				return
			}
			// The base is whichever source is shorter, for every case - not the
			// fixture's duration, which only applies to the full-length ones.
			want := math.Min(tc.timings.Target.Duration.Seconds(), tc.timings.Donor.Duration.Seconds())
			if mode.DurationSec != want {
				t.Errorf("duration base = %v s, want the shorter source %v s", mode.DurationSec, want)
			}
			if want := len(windows.CheckpointPositionsPct); mode.Checkpoints != want {
				t.Errorf("windows = %d, want %d", mode.Checkpoints, want)
			}
		})
	}
}

// TestFallbackOnTwoSuspects drives the whole fallback: two windows pinned to the
// engine's search edge cannot be repaired one at a time, so the job decodes both
// whole tracks, its report is the full-mode report, and the row says what
// happened.
func TestFallbackOnTwoSuspects(t *testing.T) {
	h := newHarness(t)
	// The windows at 25% and 70% sit on the boundary: |lag| == max_lag.
	script := flatScript(-340)
	script["25"] = []answer{{offsetMS: 190000, quality: 0.9}}
	script["70"] = []answer{{offsetMS: -190000, quality: 0.9}}
	h.engine.script = script

	jobID := h.newJob(t, analyze.AnalysisSourceWindows)
	report, err := h.execute(t, jobID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got := h.wholeDecodeCount(); got != 2 {
		t.Errorf("whole-track decodes = %d, want 2 (the fallback decodes both sides)", got)
	}
	equalCheckpoints(t, report, h.referenceReport(t, h.modeDuration()))

	rec := h.job(t, jobID)
	if rec.Status != statusCompleted {
		t.Fatalf("status = %q, want done", rec.Status)
	}
	if rec.AnalysisSource != analyze.AnalysisSourceFull || rec.AnalysisFallback != "2-suspect-windows" {
		t.Fatalf("analysis = %q/%q, want full/2-suspect-windows", rec.AnalysisSource, rec.AnalysisFallback)
	}
}

// TestRetryRecoversOneSuspect is the repair path: one spike is re-decoded once
// at the longer pre-roll, and when it lands in the band the job keeps the
// windowed source and records no fallback, states the retried value at that
// index and leaves the other five checkpoints byte-identical.
func TestRetryRecoversOneSuspect(t *testing.T) {
	h := newHarness(t)
	script := flatScript(-340)
	script["40"] = []answer{
		{offsetMS: 800, quality: 0.42},  // first pass: a lone spike
		{offsetMS: -341, quality: 0.93}, // retry: back on the line
	}
	h.engine.script = script

	firstPass := h.referenceReport(t, h.modeDuration())

	jobID := h.newJob(t, analyze.AnalysisSourceWindows)
	report, err := h.execute(t, jobID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := h.wholeDecodeCount(); got != 0 {
		t.Fatalf("whole-track decodes = %d, want 0 (the retry repaired the job)", got)
	}

	rec := h.job(t, jobID)
	if rec.AnalysisSource != analyze.AnalysisSourceWindows || rec.AnalysisFallback != "" {
		t.Fatalf("analysis = %q/%q, want windows with no fallback", rec.AnalysisSource, rec.AnalysisFallback)
	}
	if got := report.OffsetsMS[2]; got != -341 {
		t.Errorf("retried checkpoint offset = %v, want -341", got)
	}
	if got := report.Qualities[2]; got != 0.93 {
		t.Errorf("retried checkpoint quality = %v, want 0.93", got)
	}
	for i := range report.Checkpoints {
		if i == 2 {
			continue
		}
		if got, want := checkpointJSON(t, report.Checkpoints[i]), checkpointJSON(t, firstPass.Checkpoints[i]); got != want {
			t.Errorf("checkpoint %d changed by the retry\n got %s\nwant %s", i, got, want)
		}
	}
	// The re-decode used the longer pre-roll, on the flagged window only.
	retried := h.decodeCalls(WindowRetryPrerollSec)
	if len(retried) != 2 {
		t.Fatalf("decodes at %gs = %v, want both sides of one window", WindowRetryPrerollSec, retried)
	}
	for _, call := range retried {
		if !strings.HasPrefix(call, "40/") {
			t.Errorf("retry decoded %q, want only the window at 40%%", call)
		}
	}
}

// TestShortWindowFallsBack is the decode-failure path: a window whose frame
// gate fails is not a failed job, it is a fallback whose count says how many
// window decodes the job lost.
func TestShortWindowFallsBack(t *testing.T) {
	cases := []struct {
		name string
		// fails reports whether the decode at this pre-roll fails.
		fails func(preroll float64) bool
		want  string
	}{
		{"retry recovers the window", func(preroll float64) bool { return preroll == WindowPrerollSec }, "1-short-window"},
		{"retry fails too", func(preroll float64) bool { return true }, "2-short-window"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			// The other five checkpoints must still answer, or the "a window
			// failure must not fail the job" claim is tested against a harness
			// that fails everything for an unrelated reason.
			h.engine.script = flatScript(-340)
			h.decode.fail = func(w analyze.Window, side Side, preroll float64) error {
				if w.PositionPct != 40 || side != SideTarget || !tc.fails(preroll) {
					return nil
				}
				return &ErrShortWindow{Path: "t.wav", Frames: 100, Want: 4000}
			}
			jobID := h.newJob(t, analyze.AnalysisSourceWindows)
			report, err := h.execute(t, jobID)
			if err != nil {
				t.Fatalf("Execute: %v (a window failure must not fail the job)", err)
			}
			if report.Verdict == "" {
				t.Error("fallback report has no verdict")
			}
			rec := h.job(t, jobID)
			if rec.Status != statusCompleted {
				t.Fatalf("status = %q, want done", rec.Status)
			}
			if rec.AnalysisSource != analyze.AnalysisSourceFull || rec.AnalysisFallback != tc.want {
				t.Fatalf("analysis = %q/%q, want full/%s", rec.AnalysisSource, rec.AnalysisFallback, tc.want)
			}
			if got := h.wholeDecodeCount(); got != 2 {
				t.Errorf("whole-track decodes = %d, want 2", got)
			}
		})
	}
}

// TestDecodedSecondsCounter is the structural speed gate: a windowed job reads
// the windows it planned and nothing else, a full job reads both whole tracks.
func TestDecodedSecondsCounter(t *testing.T) {
	h := newHarness(t, withDurations(testTargetDur, testTargetDur))
	h.engine.script = flatScript(-340)

	windowed := h.newJob(t, analyze.AnalysisSourceWindows)
	run, err := h.runAnalysis(t, windowed)
	if err != nil {
		t.Fatalf("runAnalysis (windows): %v", err)
	}
	gate := 6 * 2 * (190 + WindowPrerollSec)
	if run.DecodedSec > gate {
		t.Errorf("windowed run decoded %v s, gate is %v s", run.DecodedSec, gate)
	}
	if run.DecodedSec != gate {
		t.Errorf("windowed run decoded %v s, want %v s (six windows, both sides)", run.DecodedSec, gate)
	}
	windowedSec := run.DecodedSec

	full := h.newJob(t, analyze.AnalysisSourceFull)
	run, err = h.runAnalysis(t, full)
	if err != nil {
		t.Fatalf("runAnalysis (full): %v", err)
	}
	if want := 2 * testTargetDur.Seconds(); run.DecodedSec != want {
		t.Errorf("full run decoded %v s, want %v s (both whole tracks)", run.DecodedSec, want)
	}
	if run.DecodedSec <= windowedSec {
		t.Errorf("full run decoded %v s, no more than the windowed run's %v s", run.DecodedSec, windowedSec)
	}
}

// TestLogLines pins the two operator lines: the mode decision always, the
// fallback when it fires, both naming the job.
func TestLogLines(t *testing.T) {
	h := newHarness(t)

	h.engine = h.newStub(flatScript(-340))
	cleanJob := h.newJob(t, analyze.AnalysisSourceWindows)
	if _, err := h.execute(t, cleanJob); err != nil {
		t.Fatalf("Execute (windows): %v", err)
	}

	falling := flatScript(-340)
	falling["25"] = []answer{{offsetMS: 190000, quality: 0.9}}
	falling["70"] = []answer{{offsetMS: -190000, quality: 0.9}}
	h.engine = h.newStub(falling)
	fallbackJob := h.newJob(t, analyze.AnalysisSourceWindows)
	if _, err := h.execute(t, fallbackJob); err != nil {
		t.Fatalf("Execute (fallback): %v", err)
	}

	logs := h.logs.String()
	for _, want := range []string{
		fmt.Sprintf("job %d analysis_source=windows windows=6 preroll=2s", cleanJob),
		fmt.Sprintf("job %d analysis_source=windows windows=6 preroll=2s", fallbackJob),
		fmt.Sprintf("job %d analysis fallback=full reason=2-suspect-windows", fallbackJob),
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log is missing %q\n%s", want, logs)
		}
	}
	if strings.Contains(logs, fmt.Sprintf("job %d analysis fallback", cleanJob)) {
		t.Errorf("job %d logged a fallback it did not take\n%s", cleanJob, logs)
	}
}

// --- harness ---------------------------------------------------------------

// harness is one fixture wired to stubs: no ffmpeg, no media, no ffprobe, and
// a scripted engine. Everything below those seams is the production path.
type harness struct {
	t          *testing.T
	f          *fixture
	logs       *bytes.Buffer
	engine     *stubEngine
	decode     *decodeStub
	targetDur  time.Duration
	donorDur   time.Duration
	targetPath string
	libID      int64
	plID       int64

	mu           sync.Mutex
	wholeDecodes int
}

type harnessOption func(*harness)

// withDurations sets the lengths the two containers report.
func withDurations(target, donor time.Duration) harnessOption {
	return func(h *harness) {
		h.targetDur, h.donorDur = target, donor
	}
}

func newHarness(t *testing.T, opts ...harnessOption) *harness {
	t.Helper()
	h := &harness{
		t:         t,
		f:         newFixture(t),
		logs:      &bytes.Buffer{},
		decode:    &decodeStub{},
		targetDur: testTargetDur,
		donorDur:  testDonorDur,
	}
	for _, opt := range opts {
		opt(h)
	}
	h.engine = h.newStub(nil)

	dir := t.TempDir()
	h.targetPath = filepath.Join(dir, "Movie.mkv")
	if err := os.WriteFile(h.targetPath, []byte("not really a matroska file"), 0o600); err != nil {
		t.Fatal(err)
	}
	plPath := filepath.Join(dir, "track.pl.mka")
	enPath := filepath.Join(dir, "track.en.mka")
	for _, path := range []string{plPath, enPath} {
		if err := os.WriteFile(path, []byte("not really an mka: "+filepath.Base(path)), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	mediaID := insertMedia(t, h.f.db)
	h.plID = insertTrack(t, h.f.db.DB, mediaID, "group-1", "pl", plPath)
	insertTrack(t, h.f.db.DB, mediaID, "group-1", "en", enPath)
	h.libID = insertLibrary(t, h.f.db.DB, h.targetPath, "eligible", "eng")

	h.f.svc.logger = slog.New(slog.NewTextHandler(h.logs, nil))
	// Not a real binary: every decode in this file is stubbed, so a job that
	// tried to run ffmpeg would fail loudly instead of quietly working.
	h.f.svc.ffmpegPath = "/nonexistent/ffmpeg"
	h.f.svc.probeTimings = h.probe
	h.f.svc.probeTarget = h.probe
	h.f.svc.windowDecode = h.decode.decode
	h.f.svc.wholeDecode = h.wholeDecode
	h.f.svc.engine = h.engine
	return h
}

// newStub builds an engine stub whose window positions match this fixture's
// plan, so a whole-file pass can be answered from the same script.
func (h *harness) newStub(script map[string][]answer) *stubEngine {
	positions := planPositions(math.Min(h.targetDur.Seconds(), h.donorDur.Seconds()),
		sliceLenSec(analyze.DefaultConfig()))
	return &stubEngine{script: script, positions: positions, seen: map[string]int{}}
}

// probe answers both probe seams from the harness's timings: the target is a
// video container whose analysed track starts half a second in, the staged
// donor is an audio file.
func (h *harness) probe(ctx context.Context, path string) (probe.MediaInfo, error) {
	if path == h.targetPath {
		return probe.MediaInfo{
			Path: path, Duration: h.targetDur, StartTimeKnown: true,
			Tracks: []probe.AudioTrack{{
				Index: 0, Channels: 2, Codec: "pcm_s16le", Language: "eng", LanguageISO1: "en",
				Duration: h.targetDur, StartTime: testTargetStart, StartTimeKnown: true,
			}},
		}, nil
	}
	return probe.MediaInfo{
		Path: path, Duration: h.donorDur, StartTime: testDonorStart, StartTimeKnown: true,
		Tracks: []probe.AudioTrack{{
			Index: 0, Channels: 2, Codec: "pcm_s16le", Language: "en", LanguageISO1: "en",
			Duration: h.donorDur, StartTime: testDonorStart, StartTimeKnown: true,
		}},
	}, nil
}

// wholeDecode stands in for the two whole-track decodes. It writes a header
// declaring the source's own length, because the setting-full path derives its
// duration from those headers — so the plan it analyses is the plan the
// container timings describe. No samples are written; the stub engine reads
// nothing.
func (h *harness) wholeDecode(ctx context.Context, ffmpeg, input string, stream int, filter, out string) error {
	h.mu.Lock()
	h.wholeDecodes++
	h.mu.Unlock()
	dur := h.targetDur
	if input != h.targetPath {
		dur = h.donorDur
	}
	return writeHeaderOnlyWAV(out, dur, windowSampleRate)
}

func (h *harness) wholeDecodeCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.wholeDecodes
}

// newJob records the analysis source in settings and posts one job.
func (h *harness) newJob(t *testing.T, source string) int64 {
	t.Helper()
	row := fmt.Sprintf(`{"analysis_source":%q}`, source)
	if _, err := h.f.db.DB.ExecContext(context.Background(),
		`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`,
		settings.KeySyncDefaults, row); err != nil {
		t.Fatalf("set %s: %v", row, err)
	}
	h.f.svc.engine = h.engine
	rec := postJob(t, h.f.mux, fmt.Sprintf(`{"vault_track_id":%d,"target_library_cache_id":%d}`,
		h.plID, h.libID))
	if rec.Code != 201 {
		t.Fatalf("create job = %d %s", rec.Code, rec.Body)
	}
	var job JobInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	return job.ID
}

func (h *harness) execute(t *testing.T, jobID int64) (analyze.Report, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return h.f.svc.Execute(ctx, jobID)
}

func (h *harness) runAnalysis(t *testing.T, jobID int64) (analysisRun, error) {
	t.Helper()
	return h.f.svc.runAnalysis(context.Background(), h.job(t, jobID))
}

func (h *harness) job(t *testing.T, jobID int64) store.SyncJobRecord {
	t.Helper()
	rec, ok, err := h.f.db.SyncJobByID(context.Background(), jobID)
	if err != nil || !ok {
		t.Fatalf("SyncJobByID = %v/%v", ok, err)
	}
	return rec
}

// modeDuration is the window plan's duration base for this fixture.
func (h *harness) modeDuration() float64 {
	return math.Min(h.targetDur.Seconds(), h.donorDur.Seconds())
}

// referenceReport runs the plan outside the job with no slicer: the whole-file
// analysis over the same duration, whose checkpoint fields are the plan's own.
// It is the byte-identical reference the retry must not disturb.
func (h *harness) referenceReport(t *testing.T, durationSec float64) analyze.Report {
	t.Helper()
	policy, cfg := h.f.svc.analysisPolicy(context.Background())
	cfg.Slicer = nil
	report, err := analyze.Analyze(context.Background(), h.engine, cfg, analyze.Request{
		TargetPath:  "/reference/target.wav",
		DonorPath:   "/reference/donor.wav",
		DurationSec: durationSec,
		TargetTracks: []probe.AudioTrack{
			{Index: 0, Channels: 2, Language: "eng", LanguageISO1: "en"},
		},
		VaultTracks: []probe.AudioTrack{
			{Index: 0, Channels: 2, Language: "pl", LanguageISO1: "pl"},
			{Index: 1, Channels: 2, Language: "en", LanguageISO1: "en"},
		},
		Policy:           policy,
		OriginalLanguage: "eng",
	})
	if err != nil {
		t.Fatalf("reference analyze: %v", err)
	}
	return report
}

// decodeCalls lists the windows decoded at one pre-roll, as "pct/side".
func (h *harness) decodeCalls(preroll float64) []string {
	return h.decode.at(preroll)
}

// --- stubs -----------------------------------------------------------------

// decodeStub stands in for the window decode and its frame gate.
type decodeStub struct {
	mu    sync.Mutex
	calls []decodeCall
	fail  func(w analyze.Window, side Side, preroll float64) error
}

type decodeCall struct {
	positionPct int
	side        Side
	preroll     float64
}

func (d *decodeStub) decode(ctx context.Context, ffmpeg, input string, stream int, filter string,
	w analyze.Window, side Side, preroll float64, out string) error {
	d.mu.Lock()
	d.calls = append(d.calls, decodeCall{positionPct: w.PositionPct, side: side, preroll: preroll})
	fail := d.fail
	d.mu.Unlock()
	if fail != nil {
		return fail(w, side, preroll)
	}
	return nil
}

func (d *decodeStub) at(preroll float64) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.calls))
	for _, c := range d.calls {
		if c.preroll == preroll {
			out = append(out, fmt.Sprintf("%d/%s", c.positionPct, c.side))
		}
	}
	return out
}

// answer is one scripted engine result.
type answer struct {
	offsetMS float64
	quality  float64
}

// stubEngine answers every engine call from a script keyed by the window the
// job is about, so a test decides what each window measured and what the retry
// of one window measures. A whole-file pass is answered with the window's
// first-pass value: it analyses the same plan, at the same film positions.
type stubEngine struct {
	mu        sync.Mutex
	script    map[string][]answer
	positions map[string]float64
	calls     int
	seen      map[string]int
}

func (e *stubEngine) Run(ctx context.Context, job syncpkg.Job) (syncpkg.Result, error) {
	key, whole := e.windowKey(job)
	e.mu.Lock()
	e.calls++
	nth := 1
	if !whole {
		e.seen[key]++
		nth = e.seen[key]
	}
	answers := e.script[key]
	e.mu.Unlock()
	if len(answers) == 0 {
		return syncpkg.Result{}, fmt.Errorf("stubEngine: no script for %q", key)
	}
	if nth > len(answers) {
		nth = len(answers)
	}
	a := answers[nth-1]
	return syncpkg.Result{
		OffsetMS:           a.offsetMS,
		CorrelationQuality: a.quality,
		SampleRate:         windowSampleRate,
		Analyzed:           syncpkg.Analyzed{Start: job.SliceStartSec, Len: job.SliceLenSec},
	}, nil
}

// windowKey names the window an engine job belongs to. A windowed job points at
// {jobDir}/win/{pct}/t.wav; a whole-file job points at the staged WAV and
// carries the plan's film start, so the position is recovered from there.
func (e *stubEngine) windowKey(job syncpkg.Job) (string, bool) {
	dir := filepath.Dir(job.TargetPath)
	if filepath.Base(filepath.Dir(dir)) == "win" {
		return filepath.Base(dir), false
	}
	for key, start := range e.positions {
		if math.Abs(start-job.SliceStartSec) < 0.5 {
			return key, true
		}
	}
	return "whole", true
}

// planPositions maps each checkpoint percentage to the film position the plan
// gives it: the same clamp analyze.PlanWindows applies, for a duration base.
func planPositions(durationSec, windowSec float64) map[string]float64 {
	positions := map[string]float64{}
	for _, pct := range analyze.DefaultCheckpointPositionsPct {
		start := float64(pct)/100*durationSec - windowSec/2
		if start < 0 {
			start = 0
		}
		if max := durationSec - windowSec; start > max {
			start = max
		}
		positions[fmt.Sprintf("%d", pct)] = start
	}
	return positions
}

// flatScript scripts one answer for every checkpoint.
func flatScript(offsetMS float64) map[string][]answer {
	script := map[string][]answer{}
	for _, pct := range analyze.DefaultCheckpointPositionsPct {
		script[fmt.Sprintf("%d", pct)] = []answer{{offsetMS: offsetMS, quality: 0.9}}
	}
	return script
}

// --- assertions ------------------------------------------------------------

func checkpointJSON(t *testing.T, checkpoint analyze.Checkpoint) string {
	t.Helper()
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatalf("marshal checkpoint %d: %v", checkpoint.PositionPct, err)
	}
	return string(raw)
}

// equalCheckpoints reports whether two reports say the same thing.
func equalCheckpoints(t *testing.T, got, want analyze.Report) {
	t.Helper()
	if len(got.Checkpoints) != len(want.Checkpoints) {
		t.Fatalf("checkpoints = %d, want %d", len(got.Checkpoints), len(want.Checkpoints))
	}
	for i := range want.Checkpoints {
		if gotJSON, wantJSON := checkpointJSON(t, got.Checkpoints[i]), checkpointJSON(t, want.Checkpoints[i]); gotJSON != wantJSON {
			t.Errorf("checkpoint %d\n got %s\nwant %s", i, gotJSON, wantJSON)
		}
	}
	if got.Verdict != want.Verdict || got.SyncMethod != want.SyncMethod {
		t.Errorf("verdict/method = %q/%q, want %q/%q", got.Verdict, got.SyncMethod, want.Verdict, want.SyncMethod)
	}
}

// writeHeaderOnlyWAV writes a RIFF header declaring durationSec of PCM16 audio
// and no samples at all: the analysis reads durations from the header when the
// mode has none to offer, and the stub engine opens nothing.
func writeHeaderOnlyWAV(path string, duration time.Duration, sampleRate int) error {
	const bits = 16
	byteRate := sampleRate * bits / 8
	dataSize := uint32(duration.Seconds() * float64(byteRate))

	var buf bytes.Buffer
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36))
	buf.WriteString("WAVEfmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // mono
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&buf, binary.LittleEndian, uint32(byteRate))
	binary.Write(&buf, binary.LittleEndian, uint16(bits/8)) // block align
	binary.Write(&buf, binary.LittleEndian, uint16(bits))
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, dataSize)

	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}
