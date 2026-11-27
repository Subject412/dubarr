package sync

import (
	"context"
	"encoding/binary"
	"errors"
	"go/parser"
	"go/token"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/dsp/correlate"
	"github.com/Subject412/dubarr/backend/internal/dsp/wav"
)

// testRate is the engine's production analysis rate.
const testRate uint32 = 8000

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func newTestRunner(t *testing.T) *Runner {
	t.Helper()
	return New(WithLogger(discardLogger()))
}

// noiseSignal is deterministic broadband noise in [-1, 1): a sharp correlation
// peak without any periodic ambiguity.
func noiseSignal(n int, seed uint64) []float32 {
	out := make([]float32, n)
	x := seed | 1
	for i := range out {
		x = x*6364136223846793005 + 1442695040888963407
		out[i] = float32(int64(x>>33)) / float32(1<<31)
	}
	return out
}

// advanced returns a donor that runs ahead of target by lead samples, i.e. the
// target timeline is later, so offset_ms = t_target - t_donor is positive.
func advanced(target []float32, lead int) []float32 {
	out := make([]float32, len(target))
	copy(out, target[lead:])
	return out
}

// writeWAV writes mono float32 PCM in a RIFF/WAVE container.
func writeWAV(t *testing.T, path string, rate uint32, samples []float32) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()

	const headerLen = 44
	dataLen := len(samples) * 4
	var h [headerLen]byte
	copy(h[0:4], "RIFF")
	binary.LittleEndian.PutUint32(h[4:8], uint32(36+dataLen))
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:20], 16)
	binary.LittleEndian.PutUint16(h[20:22], 3) // IEEE float
	binary.LittleEndian.PutUint16(h[22:24], 1) // mono
	binary.LittleEndian.PutUint32(h[24:28], rate)
	binary.LittleEndian.PutUint32(h[28:32], rate*4)
	binary.LittleEndian.PutUint16(h[32:34], 4)
	binary.LittleEndian.PutUint16(h[34:36], 32)
	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:44], uint32(dataLen))
	if _, err := f.Write(h[:]); err != nil {
		t.Fatalf("write header: %v", err)
	}
	buf := make([]byte, 0, 64*1024)
	for _, s := range samples {
		buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(s))
		if len(buf) >= 64*1024 {
			if _, err := f.Write(buf); err != nil {
				t.Fatalf("write samples: %v", err)
			}
			buf = buf[:0]
		}
	}
	if _, err := f.Write(buf); err != nil {
		t.Fatalf("write tail: %v", err)
	}
}

// fixtureJob writes a 2 s 8 kHz pair whose donor runs 2000 samples (250 ms)
// ahead and returns the job that analyses all of it.
func fixtureJob(t *testing.T) Job {
	t.Helper()
	const n = 2 * int(testRate)
	target := noiseSignal(n, 7)
	donor := advanced(target, 2000)
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target.wav")
	donorPath := filepath.Join(dir, "donor.wav")
	writeWAV(t, targetPath, testRate, target)
	writeWAV(t, donorPath, testRate, donor)
	window := 1.0
	return Job{
		TargetPath:    targetPath,
		DonorPath:     donorPath,
		SliceStartSec: 0,
		SliceLenSec:   2,
		WindowSec:     &window,
	}
}

// assertFixtureResult pins the answer every fixture-based test expects.
func assertFixtureResult(t *testing.T, res Result) {
	t.Helper()
	if res.OffsetMS != 250 {
		t.Fatalf("offset_ms = %v, want 250", res.OffsetMS)
	}
	if res.SampleRate != testRate {
		t.Fatalf("sample_rate = %d, want %d", res.SampleRate, testRate)
	}
	if res.CorrelationQuality < 0.9 {
		t.Fatalf("correlation_quality = %v, want >= 0.9", res.CorrelationQuality)
	}
	if res.Analyzed.Start != 0 || res.Analyzed.Len != 2 {
		t.Fatalf("analyzed = %+v, want start 0 len 2", res.Analyzed)
	}
}

func TestRunSuccess(t *testing.T) {
	r := newTestRunner(t)
	job := fixtureJob(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := r.Run(ctx, job)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertFixtureResult(t, res)
}

// TestResultCarriesMatchZAndPrecision proves the three new numbers survive the
// runner: the sub-sample offset is converted to milliseconds and rounded to
// three decimals like every other reported value, the z travels as it was
// scored, and the precision is the engine's own sample period.
func TestResultCarriesMatchZAndPrecision(t *testing.T) {
	r := newTestRunner(t)
	job := fixtureJob(t)
	r.correlate = func(context.Context, []float32, []float32, uint32, float64) (correlate.Analysis, error) {
		return correlate.Analysis{
			OffsetSamples:    2000,
			Quality:          0.9,
			OffsetSubSamples: 2000.3,
			MatchZ:           27.5,
			PrecisionMS:      0.125,
		}, nil
	}

	res, err := r.Run(context.Background(), job)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.OffsetMS != 250 {
		t.Fatalf("offset_ms = %v, want 250", res.OffsetMS)
	}
	if res.CorrelationQuality != 0.9 {
		t.Fatalf("correlation_quality = %v, want 0.9", res.CorrelationQuality)
	}
	if res.OffsetSubMS != 250.038 {
		t.Fatalf("offset_sub_ms = %v, want 250.038 (2000.3 samples at 8 kHz, 3 decimals)", res.OffsetSubMS)
	}
	if res.MatchZ != 27.5 {
		t.Fatalf("match_z = %v, want 27.5", res.MatchZ)
	}
	if res.PrecisionMS != 0.125 {
		t.Fatalf("precision_ms = %v, want 0.125", res.PrecisionMS)
	}
}

// TestRunnerUsesTheInjectedCorrelator proves the exported seam is the one the
// runner calls: the injected correlator receives exactly the slice the job asked
// for — both files, at the job's start and length, at the file's rate and with
// the job's window bound — and what it reports reaches the Result. That is what
// lets a second matcher (the mel-cepstral one) run behind the same validation,
// slice echo and deadline handling as the waveform matcher.
func TestRunnerUsesTheInjectedCorrelator(t *testing.T) {
	target := noiseSignal(16000, 11)
	donor := advanced(target, 2000)
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target.wav")
	donorPath := filepath.Join(dir, "donor.wav")
	writeWAV(t, targetPath, testRate, target)
	writeWAV(t, donorPath, testRate, donor)

	// The slice the job asks for, read back the way the runner reads it: 0.25 s
	// into each file, one second long (2000..10000 at 8 kHz).
	wantTarget, err := readWAVSamples(targetPath, 2000, 8000)
	if err != nil {
		t.Fatalf("read target slice: %v", err)
	}
	wantDonor, err := readWAVSamples(donorPath, 2000, 8000)
	if err != nil {
		t.Fatalf("read donor slice: %v", err)
	}

	window := 2.0
	var calls int
	var gotRate uint32
	var gotWindow float64
	var gotTarget, gotDonor []float32
	r := New(
		WithLogger(discardLogger()),
		WithCorrelator(func(_ context.Context, targetSlice, donorSlice []float32, rate uint32, windowSec float64) (correlate.Analysis, error) {
			calls++
			gotTarget, gotDonor, gotRate, gotWindow = targetSlice, donorSlice, rate, windowSec
			return correlate.Analysis{
				OffsetSamples:    2400,
				OffsetSubSamples: 2400,
				Quality:          0.6,
				MatchZ:           31.25,
				PrecisionMS:      10,
			}, nil
		}),
	)

	res, err := r.Run(context.Background(), Job{
		TargetPath:    targetPath,
		DonorPath:     donorPath,
		SliceStartSec: 0.25,
		SliceLenSec:   1,
		WindowSec:     &window,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 1 {
		t.Fatalf("correlator calls = %d, want 1", calls)
	}
	if gotRate != testRate {
		t.Fatalf("sample_rate = %d, want %d", gotRate, testRate)
	}
	if gotWindow != window {
		t.Fatalf("window_sec = %v, want %v", gotWindow, window)
	}
	// The paths are proved by their content: each recorded slice must equal the
	// samples that file holds at the job's start and length, so reading the
	// wrong file or the wrong window fails here.
	assertSameSamples(t, "target", gotTarget, wantTarget)
	assertSameSamples(t, "donor", gotDonor, wantDonor)

	// The injected answer is what comes out, untranslated: a hop-quantised
	// precision and the z the matcher scored, at the rate the job ran.
	if res.PrecisionMS != 10 {
		t.Fatalf("precision_ms = %v, want 10", res.PrecisionMS)
	}
	if res.MatchZ != 31.25 {
		t.Fatalf("match_z = %v, want 31.25", res.MatchZ)
	}
	if res.OffsetMS != 300 || res.OffsetSubMS != 300 {
		t.Fatalf("offset = %v/%v ms, want 300/300 (2400 samples at 8 kHz)", res.OffsetMS, res.OffsetSubMS)
	}
	if res.Analyzed.Start != 0.25 || res.Analyzed.Len != 1 {
		t.Fatalf("analyzed = %+v, want start 0.25 len 1", res.Analyzed)
	}
}

// readWAVSamples decodes frames from start of a WAV file, the way the runner
// reads the slices it hands the correlator.
func readWAVSamples(path string, start, frames int64) ([]float32, error) {
	reader, err := wav.Open(path)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return reader.ReadMono(start, frames)
}

// assertSameSamples fails on the first sample that differs, naming the slice.
func assertSameSamples(t *testing.T, which string, got, want []float32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s slice length = %d, want %d", which, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s slice[%d] = %v, want %v", which, i, got[i], want[i])
		}
	}
}

func TestRunMismatchedRates(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target.wav")
	donorPath := filepath.Join(dir, "donor.wav")
	writeWAV(t, targetPath, 8000, noiseSignal(8000, 1))
	writeWAV(t, donorPath, 16000, noiseSignal(16000, 2))

	_, err := r.Run(context.Background(), Job{
		TargetPath:    targetPath,
		DonorPath:     donorPath,
		SliceStartSec: 0,
		SliceLenSec:   0.1,
	})
	if !errors.Is(err, ErrAnalysis) {
		t.Fatalf("err = %v, want ErrAnalysis", err)
	}
	if kind, ok := KindOf(err); !ok || kind != KindAnalysis {
		t.Fatalf("KindOf = %q/%v, want %q", kind, ok, KindAnalysis)
	}
	var mismatch *SampleRateMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %#v, want *SampleRateMismatch", err)
	}
	if mismatch.Target != 8000 || mismatch.Donor != 16000 {
		t.Fatalf("mismatch = %+v, want target 8000 donor 16000", mismatch)
	}
}

func TestRunShortSlice(t *testing.T) {
	r := newTestRunner(t)
	job := fixtureJob(t)
	job.SliceLenSec = 0.0001 // 0.8 samples -> below the 64-sample minimum
	_, err := r.Run(context.Background(), job)
	if !errors.Is(err, ErrAnalysis) {
		t.Fatalf("err = %v, want ErrAnalysis", err)
	}
	var short *correlate.SliceTooShort
	if !errors.As(err, &short) {
		t.Fatalf("err = %#v, want *correlate.SliceTooShort", err)
	}
	if !strings.Contains(err.Error(), "slice too short") {
		t.Fatalf("err = %v, want it to name a short slice", err)
	}
}

func TestRunSilentSlice(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target.wav")
	donorPath := filepath.Join(dir, "donor.wav")
	silence := make([]float32, 2*int(testRate))
	writeWAV(t, targetPath, testRate, silence)
	writeWAV(t, donorPath, testRate, silence)

	_, err := r.Run(context.Background(), Job{
		TargetPath:    targetPath,
		DonorPath:     donorPath,
		SliceStartSec: 0,
		SliceLenSec:   2,
	})
	if !errors.Is(err, ErrAnalysis) {
		t.Fatalf("err = %v, want ErrAnalysis", err)
	}
	if !strings.Contains(err.Error(), "silent") {
		t.Fatalf("err = %v, want it to say the slice is silent", err)
	}
}

func TestRunOutOfRange(t *testing.T) {
	r := newTestRunner(t)
	job := fixtureJob(t)
	job.SliceStartSec = 10 // the fixture is 2 s long
	_, err := r.Run(context.Background(), job)
	if !errors.Is(err, ErrAnalysis) {
		t.Fatalf("err = %v, want ErrAnalysis", err)
	}
	var oor *SliceOutOfRange
	if !errors.As(err, &oor) {
		t.Fatalf("err = %#v, want *SliceOutOfRange", err)
	}
	if !strings.Contains(err.Error(), "beyond audio duration") {
		t.Fatalf("err = %v, want the engine's out-of-range wording", err)
	}
}

func TestInvalidJobLocal(t *testing.T) {
	// The paths do not exist: a job rejected before any file is opened must
	// fail as ErrInvalidJob, not ErrAnalysis.
	r := newTestRunner(t)
	missing := filepath.Join(t.TempDir(), "missing.wav")
	window := 0.0
	check := func(name string, job Job) {
		t.Run(name, func(t *testing.T) {
			if _, err := r.Run(context.Background(), job); !errors.Is(err, ErrInvalidJob) {
				t.Fatalf("err = %v, want ErrInvalidJob", err)
			}
		})
	}
	check("no target", Job{DonorPath: missing, SliceLenSec: 1})
	check("no donor", Job{TargetPath: missing, SliceLenSec: 1})
	check("negative slice", Job{TargetPath: missing, DonorPath: missing, SliceStartSec: -1, SliceLenSec: 1})
	check("zero slice len", Job{TargetPath: missing, DonorPath: missing, SliceLenSec: 0})
	check("nan slice start", Job{TargetPath: missing, DonorPath: missing, SliceStartSec: math.NaN(), SliceLenSec: 1})
	check("zero window", Job{TargetPath: missing, DonorPath: missing, SliceLenSec: 1, WindowSec: &window})
}

// TestRunTimeout proves the deadline survives without a process to kill, and
// that a timed-out job leaves nothing behind for the next one.
func TestRunTimeout(t *testing.T) {
	const timeout = 100 * time.Millisecond
	r := New(WithLogger(discardLogger()), WithTimeout(timeout))
	job := fixtureJob(t)

	// Block until the context expires, exactly like a slow correlation would.
	blocked := true
	r.correlate = func(ctx context.Context, target, donor []float32, rate uint32, window float64) (correlate.Analysis, error) {
		if blocked {
			blocked = false
			<-ctx.Done()
			return correlate.Analysis{}, ctx.Err()
		}
		return correlate.Analyze(ctx, target, donor, rate, window)
	}

	start := time.Now()
	_, err := r.Run(context.Background(), job)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if kind, ok := KindOf(err); !ok || kind != KindTimeout {
		t.Fatalf("KindOf = %q/%v, want %q", kind, ok, KindTimeout)
	}
	if elapsed > 2*timeout {
		t.Fatalf("timeout took %s, want within %s", elapsed, 2*timeout)
	}

	// No leaked state: the same Runner runs the same job normally.
	res, err := r.Run(context.Background(), job)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	assertFixtureResult(t, res)
}

// TestPanicBecomesTypedError is the in-process panic boundary: an fft panic
// (check1D/checkLen/checkAlloc panic by contract) must fail the job, not the
// process, and must leave the Runner usable.
func TestPanicBecomesTypedError(t *testing.T) {
	r := newTestRunner(t)
	job := fixtureJob(t)

	panicked := false
	r.correlate = func(ctx context.Context, target, donor []float32, rate uint32, window float64) (correlate.Analysis, error) {
		if !panicked {
			panicked = true
			panic("fft: checkAlloc: refusing a 2^22 plan")
		}
		return correlate.Analyze(ctx, target, donor, rate, window)
	}

	_, err := r.Run(context.Background(), job)
	if !errors.Is(err, ErrExecution) {
		t.Fatalf("err = %v, want ErrExecution", err)
	}
	if kind, ok := KindOf(err); !ok || kind != KindExecution {
		t.Fatalf("KindOf = %q/%v, want %q", kind, ok, KindExecution)
	}
	var typed *Error
	if !errors.As(err, &typed) {
		t.Fatalf("err = %#v, want *Error", err)
	}
	if typed.Stderr == "" {
		t.Fatal("Stderr is empty, want a one-line stack")
	}
	if !strings.Contains(err.Error(), "dsp: fft: checkAlloc") {
		t.Fatalf("err = %v, want it to name the panic", err)
	}
	if strings.ContainsAny(err.Error(), "\n\r\t") {
		t.Fatalf("err message is not one line: %q", err.Error())
	}

	res, err := r.Run(context.Background(), job)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	assertFixtureResult(t, res)
}

// TestCancelPropagates covers the caller cancelling the parent context: there is
// no process to kill, so the correlation must observe the cancellation itself.
func TestCancelPropagates(t *testing.T) {
	r := newTestRunner(t)
	job := fixtureJob(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	r.correlate = func(ctx context.Context, _, _ []float32, _ uint32, _ float64) (correlate.Analysis, error) {
		close(entered)
		<-ctx.Done()
		return correlate.Analysis{}, ctx.Err()
	}
	go func() {
		<-entered
		cancel()
	}()

	start := time.Now()
	_, err := r.Run(ctx, job)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancel took %s, want within the sub-second overrun bound", elapsed)
	}
	if !errors.Is(err, ErrExecution) {
		t.Fatalf("err = %v, want ErrExecution", err)
	}
	if kind, ok := KindOf(err); !ok || kind != KindExecution {
		t.Fatalf("KindOf = %q/%v, want %q", kind, ok, KindExecution)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
}

// TestNoExecImported holds the subprocess bridge gone: no os/exec import and no
// process_*.go left in the package. It parses imports instead of grepping bytes,
// so the literal below cannot match itself.
func TestNoExecImported(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "process_") && strings.HasSuffix(name, ".go") {
			t.Errorf("%s still exists, want the subprocess bridge deleted", name)
		}
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range file.Imports {
			if imp.Path.Value == `"os/exec"` {
				t.Errorf("%s imports os/exec, want an in-process engine", name)
			}
		}
	}
}
