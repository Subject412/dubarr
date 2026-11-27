package analyze

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	engine "github.com/Subject412/dubarr/backend/internal/sync"
)

// recordingSlicer is a whole-file slicer that remembers every call: it hands
// back the same two paths and the same in-file start the default
// [wholeFileSlicer] would, so a report built through it must equal one built
// through the default. It is mutex-guarded because the interface promises
// concurrent safety and the parallel test exercises that.
type recordingSlicer struct {
	target, donor string

	mu       sync.Mutex
	windows  []Window
	slices   []Slice
	released []Window
}

func (s *recordingSlicer) Slice(_ context.Context, w Window) (Slice, error) {
	slice := Slice{
		TargetPath:     s.target,
		DonorPath:      s.donor,
		EngineStartSec: w.FilmStartSec,
		LenSec:         w.LenSec,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.windows = append(s.windows, w)
	s.slices = append(s.slices, slice)
	return slice, nil
}

func (s *recordingSlicer) Release(w Window) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = append(s.released, w)
}

func (s *recordingSlicer) counts() (slices, releases int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.slices), len(s.released)
}

// fixedStartSlicer is a windowed decode's shape: every checkpoint's audio
// starts at the same small pre-roll inside its own small file, so the engine
// start has nothing to do with the film position the report states.
type fixedStartSlicer struct {
	start         float64
	target, donor string

	mu       sync.Mutex
	calls    int
	releases int
}

func (s *fixedStartSlicer) Slice(_ context.Context, w Window) (Slice, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return Slice{
		TargetPath:     s.target,
		DonorPath:      s.donor,
		EngineStartSec: s.start,
		LenSec:         w.LenSec,
	}, nil
}

func (s *fixedStartSlicer) Release(Window) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases++
}

func (s *fixedStartSlicer) count() (calls, releases int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.releases
}

// countingSlicer counts calls, so a test can assert the slicer was never
// reached.
type countingSlicer struct {
	mu    sync.Mutex
	calls int
}

func (s *countingSlicer) Slice(_ context.Context, _ Window) (Slice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return Slice{}, errors.New("countingSlicer: not expected to be called")
}

func (c *countingSlicer) Release(Window) {}

func (s *countingSlicer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// echoEngine answers per checkpoint with a canned offset and an echo of the
// slice it was given. shortBy makes it claim it analysed that much less than
// planned; echoStart is what it reports as analyzed.start, which the report
// must ignore.
type echoEngine struct {
	offset    float64
	quality   float64
	echoStart float64
	shortBy   float64

	mu   sync.Mutex
	jobs []engine.Job
}

func (e *echoEngine) Run(_ context.Context, job engine.Job) (engine.Result, error) {
	e.mu.Lock()
	e.jobs = append(e.jobs, job)
	e.mu.Unlock()
	length := job.SliceLenSec
	if job.WindowSec != nil {
		length = *job.WindowSec
	}
	return engine.Result{
		OffsetMS:           e.offset,
		CorrelationQuality: e.quality,
		SampleRate:         8000,
		Analyzed:           engine.Analyzed{Start: e.echoStart, Len: math.Max(0, length-e.shortBy)},
	}, nil
}

func (e *echoEngine) recorded() []engine.Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]engine.Job(nil), e.jobs...)
}

// TestSlicerModeEquivalence: the report is a function of the plan, not of how
// the engine's audio was produced. A Slicer handing back the same files and
// the same in-file start as the default whole-file path must produce a report
// that marshals identically — offsets, qualities, max_drift_ms and verdict.
func TestSlicerModeEquivalence(t *testing.T) {
	dir := t.TempDir()
	req := request(dir)
	offsets := []float64{1200, 1210, 1195, 1205, 1198, 1202}
	qualities := []float64{0.91, 0.92, 0.9, 0.93, 0.94, 0.95}

	cfg := DefaultConfig()
	// One worker keeps scriptedEngine's call-order answers deterministic;
	// only the slicer differs between the two runs.
	cfg.ParallelCheckpoints = 1

	whole, err := Analyze(context.Background(), &scriptedEngine{offsets: offsets, qualities: qualities}, cfg, req)
	if err != nil {
		t.Fatalf("whole-file Analyze: %v", err)
	}

	rec := &recordingSlicer{target: req.TargetPath, donor: req.DonorPath}
	cfg.Slicer = rec
	windowed, err := Analyze(context.Background(), &scriptedEngine{offsets: offsets, qualities: qualities}, cfg, req)
	if err != nil {
		t.Fatalf("sliced Analyze: %v", err)
	}

	wholeJSON, err := json.Marshal(whole)
	if err != nil {
		t.Fatalf("marshal whole-file report: %v", err)
	}
	windowedJSON, err := json.Marshal(windowed)
	if err != nil {
		t.Fatalf("marshal sliced report: %v", err)
	}
	if !bytes.Equal(wholeJSON, windowedJSON) {
		t.Fatalf("report depends on the slicer:\nwhole  %s\nsliced %s", wholeJSON, windowedJSON)
	}

	slices, releases := rec.counts()
	if slices != len(cfg.CheckpointPositionsPct) {
		t.Fatalf("slicer calls = %d, want %d", slices, len(cfg.CheckpointPositionsPct))
	}
	// Release is called for every slice the slicer handed out.
	if releases != slices {
		t.Fatalf("releases = %d, want %d (one per slice)", releases, slices)
	}
	// The stub must have been given the same in-file start as the default.
	for i, s := range rec.slices {
		if s.TargetPath != req.TargetPath || s.DonorPath != req.DonorPath {
			t.Fatalf("slice %d paths = %q/%q, want the staged paths", i, s.TargetPath, s.DonorPath)
		}
		if s.EngineStartSec != windowed.Checkpoints[i].SliceStartSec {
			t.Fatalf("slice %d engine start = %v, want the film start %v",
				i, s.EngineStartSec, windowed.Checkpoints[i].SliceStartSec)
		}
		if s.LenSec != windowed.Checkpoints[i].SliceLenSec {
			t.Fatalf("slice %d len = %v, want %v", i, s.LenSec, windowed.Checkpoints[i].SliceLenSec)
		}
	}
}

// TestReportStatesFilmWhileEngineReadsSlice is finding 1's contract, at the
// execution layer: for one and the same checkpoint the engine is told to read
// at 2.0 s inside its window file while the report places the checkpoint at
// 448.12325 s of the film. The engine's own echo (analyzed.start) must not
// leak into the report.
func TestReportStatesFilmWhileEngineReadsSlice(t *testing.T) {
	dir := t.TempDir()
	req := request(dir)
	// 10% of 5431.2325 minus half the 190 s window is exactly 448.12325 s,
	// which is a whole number of 8 kHz samples: the plan can state it
	// without rounding.
	req.DurationSec = 5431.2325

	cfg := DefaultConfig()
	cfg.ParallelCheckpoints = 1
	cfg.AnalysisSource = AnalysisSourceWindows
	slicer := &fixedStartSlicer{start: 2.0, target: req.TargetPath, donor: req.DonorPath}
	cfg.Slicer = slicer

	const echoStart = 9999.5
	eng := &echoEngine{offset: 1200, quality: 0.95, echoStart: echoStart}
	rep, err := Analyze(context.Background(), eng, cfg, req)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	const wantFilm = 448.12325
	if got := rep.Checkpoints[0].SliceStartSec; got != wantFilm {
		t.Fatalf("report checkpoint 10%% slice_start = %v, want the film position %v", got, wantFilm)
	}
	if rep.Checkpoints[0].PositionPct != 10 {
		t.Fatalf("first checkpoint pct = %d, want 10", rep.Checkpoints[0].PositionPct)
	}

	jobs := eng.recorded()
	if len(jobs) != len(cfg.CheckpointPositionsPct) {
		t.Fatalf("engine jobs = %d, want %d", len(jobs), len(cfg.CheckpointPositionsPct))
	}
	for i, job := range jobs {
		if job.SliceStartSec != 2.0 {
			t.Fatalf("engine job %d slice_start = %v, want the pre-roll 2.0", i, job.SliceStartSec)
		}
		if job.SliceLenSec != rep.Checkpoints[i].SliceLenSec {
			t.Fatalf("engine job %d slice_len = %v, want the planned %v",
				i, job.SliceLenSec, rep.Checkpoints[i].SliceLenSec)
		}
		if job.WindowSec == nil || *job.WindowSec != rep.Checkpoints[i].SliceLenSec {
			t.Fatalf("engine job %d window_sec = %v, want the planned length", i, job.WindowSec)
		}
	}
	// The engine echoed a fantasy start; the report is unaffected.
	if rep.Checkpoints[0].SliceStartSec == echoStart {
		t.Fatalf("report took analyzed.start (%v) as the checkpoint position", echoStart)
	}
	calls, releases := slicer.count()
	if calls != len(jobs) || releases != calls {
		t.Fatalf("slicer calls/releases = %d/%d, want %d/%d", calls, releases, len(jobs), len(jobs))
	}
}

// TestNoDurationRefusedBeforeSlicer: a windowed plan without a duration is a
// loud failure, and it happens before the slicer is asked for anything. The
// staged paths do not exist on disk, so a full-decode duration read would also
// fail here — the test pins that the guard, not the file system, answers.
func TestNoDurationRefusedBeforeSlicer(t *testing.T) {
	for _, duration := range []float64{0, -1} {
		dir := t.TempDir()
		req := request(dir)
		req.DurationSec = duration

		cfg := DefaultConfig()
		cfg.AnalysisSource = AnalysisSourceWindows
		slicer := &countingSlicer{}
		cfg.Slicer = slicer

		_, err := Analyze(context.Background(), &echoEngine{}, cfg, req)
		if !errors.Is(err, ErrNoDuration) {
			t.Fatalf("duration %v: Analyze error = %v, want ErrNoDuration", duration, err)
		}
		if got := slicer.count(); got != 0 {
			t.Fatalf("duration %v: slicer called %d times before refusing, want 0", duration, got)
		}
	}
}

// TestShortWindowTripwire: the engine's echo is read for one purpose. A slice
// that came back a whole second short is a failed checkpoint, named by
// position, and no report is returned.
func TestShortWindowTripwire(t *testing.T) {
	dir := t.TempDir()
	req := request(dir)

	cfg := DefaultConfig()
	cfg.ParallelCheckpoints = 1
	eng := &echoEngine{offset: 1200, quality: 0.95, shortBy: 1.0}

	rep, err := Analyze(context.Background(), eng, cfg, req)
	if !errors.Is(err, ErrShortWindow) {
		t.Fatalf("Analyze error = %v, want ErrShortWindow", err)
	}
	if !strings.Contains(err.Error(), "checkpoint 10%") {
		t.Fatalf("error = %q, want it to name the failing checkpoint 10%%", err)
	}
	if rep.Verdict != "" || len(rep.Checkpoints) != 0 || len(rep.OffsetsMS) != 0 {
		t.Fatalf("report returned alongside ErrShortWindow: %+v", rep)
	}
}

// TestEchoToleranceIsOneSample: the tripwire is not an equality test. A slice
// one sample short of the plan is rounding, not lost audio.
func TestEchoToleranceIsOneSample(t *testing.T) {
	dir := t.TempDir()
	req := request(dir)

	cfg := DefaultConfig()
	cfg.ParallelCheckpoints = 1
	eng := &echoEngine{offset: 1200, quality: 0.95, shortBy: 1.0 / analysisSampleRate}

	if _, err := Analyze(context.Background(), eng, cfg, req); err != nil {
		t.Fatalf("Analyze with a one-sample-short echo: %v", err)
	}
}

// TestSixDistinctSliceStartSec is finding 1's acceptance through the whole
// facade: a 5431.232 s film yields six different checkpoint positions, not the
// six identical zeros the window-derived duration produced.
func TestSixDistinctSliceStartSec(t *testing.T) {
	dir := t.TempDir()
	req := request(dir)
	req.DurationSec = 5431.232

	cfg := DefaultConfig()
	cfg.ParallelCheckpoints = 1
	rep, err := Analyze(context.Background(), &scriptedEngine{offsets: []float64{1200, 1201, 1202, 1203, 1204, 1205}}, cfg, req)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(rep.Checkpoints) != 6 {
		t.Fatalf("checkpoints = %d, want 6", len(rep.Checkpoints))
	}

	seen := make(map[float64]int, len(rep.Checkpoints))
	for i, cp := range rep.Checkpoints {
		if cp.SliceStartSec <= 0 {
			t.Fatalf("checkpoint %d%% slice_start = %v, want a position inside the film", cp.PositionPct, cp.SliceStartSec)
		}
		if cp.SliceStartSec+cp.SliceLenSec > req.DurationSec+1.0/analysisSampleRate {
			t.Fatalf("checkpoint %d%% reads past the film: %v+%v > %v",
				cp.PositionPct, cp.SliceStartSec, cp.SliceLenSec, req.DurationSec)
		}
		if prev, dup := seen[cp.SliceStartSec]; dup {
			t.Fatalf("checkpoints %d%% and %d%% both start at %v", prev, cp.PositionPct, cp.SliceStartSec)
		}
		seen[cp.SliceStartSec] = i
	}
	if len(seen) != 6 {
		t.Fatalf("distinct slice_start_sec values = %d, want 6 (%v)", len(seen), rep.Checkpoints)
	}
	// 10% of 5431.232 minus half the 190 s window, snapped to a sample.
	if got, want := rep.Checkpoints[0].SliceStartSec, 448.12325; got != want {
		t.Fatalf("first slice_start = %v, want %v", got, want)
	}
}

// fileBoundsEngine behaves like the engine's slice contract: it refuses a start
// at or past the end of the file it was handed (sync.SliceOutOfRange) and a
// window that reads past it. Everything it is given in this test must fit,
// which is the assertion that no absolute film position reaches the engine.
type fileBoundsEngine struct {
	target, donor string

	mu   sync.Mutex
	jobs []engine.Job
}

func (e *fileBoundsEngine) Run(_ context.Context, job engine.Job) (engine.Result, error) {
	duration, err := wavDuration(job.TargetPath)
	if err != nil {
		return engine.Result{}, err
	}
	if job.SliceStartSec >= duration || job.SliceStartSec+job.SliceLenSec > duration+1.0/analysisSampleRate {
		return engine.Result{}, fmt.Errorf("slice out of range: start %v len %v, file %v",
			job.SliceStartSec, job.SliceLenSec, duration)
	}
	e.mu.Lock()
	e.jobs = append(e.jobs, job)
	e.mu.Unlock()
	return engine.Result{
		OffsetMS:           1200,
		CorrelationQuality: 0.95,
		SampleRate:         8000,
		Analyzed:           engine.Analyzed{Start: job.SliceStartSec, Len: job.SliceLenSec},
	}, nil
}

// TestWholeFileSlicerKeepsTheEngineInsideTheAudio: 200 s WAVs, the real
// wholeFileSlicer (nil Config.Slicer) and a plan whose windows sit at a
// fraction of the film. Every job the engine receives is inside the file — a
// window that started at 400 s would be refused here, as the engine refuses
// it, but it can never be produced: the plan, not the engine, owns the
// geometry, and a window that reads past the duration base is [ErrWindowPlan].
func TestWholeFileSlicerKeepsTheEngineInsideTheAudio(t *testing.T) {
	dir := t.TempDir()
	const rate, dur = 8000, 200
	targetPath := dir + "/target.wav"
	donorPath := dir + "/donor.wav"
	writeWAVRate(t, targetPath, noiseSignal(dur*rate, 0x77A1), rate)
	writeWAVRate(t, donorPath, noiseSignal(dur*rate, 0x77A2), rate)

	req := request(dir)
	req.DurationSec = dur
	eng := &fileBoundsEngine{target: targetPath, donor: donorPath}
	cfg := DefaultConfig()
	cfg.ParallelCheckpoints = 1

	if _, err := Analyze(context.Background(), eng, cfg, req); err != nil {
		t.Fatalf("Analyze over %d s WAVs: %v", dur, err)
	}

	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.jobs) != len(cfg.CheckpointPositionsPct) {
		t.Fatalf("engine jobs = %d, want %d", len(eng.jobs), len(cfg.CheckpointPositionsPct))
	}
	for i, job := range eng.jobs {
		if job.SliceStartSec >= dur {
			t.Fatalf("job %d: start %v s is an absolute position past the %d s file", i, job.SliceStartSec, dur)
		}
		if job.SliceStartSec == 400 {
			t.Fatalf("job %d: the absolute 400 s start reached the engine", i)
		}
	}

	// A window starting at 400 s in a 200 s input reads past the audio by far
	// more than snapping can explain; this is the plan's own refusal, and it
	// runs before any slicer or engine is involved.
	if !windowEscapes(sampleCount(400), sampleCount(190), float64(dur)*analysisSampleRate) {
		t.Fatal("the plan would accept a 400 s start inside 200 s of audio")
	}
	// And the public planner refuses what it cannot place, with its own typed
	// error — never by handing the impossible geometry to the engine.
	timing := Timing{
		Target: SourceTiming{DurationSec: dur, OriginKnown: true},
		Donor:  SourceTiming{DurationSec: dur, OriginKnown: true},
	}
	if _, err := PlanWindows(timing, []int{10}, 0); !errors.Is(err, ErrWindowPlan) {
		t.Fatalf("PlanWindows with an unusable window = %v, want ErrWindowPlan", err)
	}
}

// TestSlicerCalledOncePerCheckpointUnderParallelism: one Slice call and one
// Release per checkpoint, whatever the worker count, and the start handed out
// is the planned film position of that checkpoint — the interface's
// concurrent-safety contract, exercised under -race.
func TestSlicerCalledOncePerCheckpointUnderParallelism(t *testing.T) {
	dir := t.TempDir()
	req := request(dir)
	cfg := parallelConfig(t, 6)
	starts := checkpointStarts(cfg)
	rec := &recordingSlicer{target: req.TargetPath, donor: req.DonorPath}
	cfg.Slicer = rec

	// One offset per checkpoint, keyed by the film position the slicer was
	// handed, so completion order cannot change the report.
	eng := &startKeyedEngine{starts: starts}
	if _, err := Analyze(context.Background(), eng, cfg, req); err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	slices, releases := rec.counts()
	if slices != 6 || releases != 6 {
		t.Fatalf("slicer calls/releases = %d/%d, want 6/6", slices, releases)
	}
	seen := make(map[float64]int, 6)
	for _, w := range rec.windows {
		seen[w.FilmStartSec]++
	}
	if len(seen) != 6 {
		t.Fatalf("distinct windows handed to the slicer = %d, want 6 (%v)", len(seen), seen)
	}
	for start, n := range seen {
		if n != 1 {
			t.Fatalf("window %v sliced %d times, want once", start, n)
		}
		if _, ok := starts[start]; !ok {
			t.Fatalf("window start %v was never planned", start)
		}
	}
}

// startKeyedEngine answers per planned film position, so a report assembled in
// completion order would differ.
type startKeyedEngine struct {
	starts map[float64]int
}

func (e *startKeyedEngine) Run(_ context.Context, job engine.Job) (engine.Result, error) {
	idx, ok := e.starts[job.SliceStartSec]
	if !ok {
		return engine.Result{}, fmt.Errorf("unstubbed slice start %v", job.SliceStartSec)
	}
	return engine.Result{
		OffsetMS:           1200 + float64(idx),
		CorrelationQuality: 0.9,
		SampleRate:         8000,
		Analyzed:           engine.Analyzed{Start: job.SliceStartSec, Len: job.SliceLenSec},
	}, nil
}
