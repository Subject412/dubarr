package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// builtBinary is the dubarr-sync executable compiled once by TestMain.
var builtBinary string

// buildErr records why TestMain could not produce a binary; integration tests
// skip (with a clear reason) instead of failing for a missing Rust toolchain.
var buildErr error

// TestMain builds the real Rust engine into a temporary directory with
// `cargo build --offline`. The Go bridge is only meaningful against the real
// binary, so this is the single integration fixture for the package.
func TestMain(m *testing.M) {
	os.Exit(runWithBuiltBinary(m))
}

func runWithBuiltBinary(m *testing.M) int {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		buildErr = err
		return m.Run()
	}
	manifest := filepath.Join(root, "sync-engine", "Cargo.toml")
	if _, err := os.Stat(manifest); err != nil {
		buildErr = fmt.Errorf("sync-engine source missing: %w", err)
		return m.Run()
	}
	cargo, err := exec.LookPath("cargo")
	if err != nil {
		buildErr = fmt.Errorf("cargo not on PATH: %w", err)
		return m.Run()
	}
	dir, err := os.MkdirTemp("", "dubarr-sync-build-")
	if err != nil {
		buildErr = err
		return m.Run()
	}
	defer os.RemoveAll(dir)

	cmd := exec.Command(cargo, "build", "--offline", "--manifest-path", manifest, "--target-dir", dir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		buildErr = fmt.Errorf("cargo build --offline: %w", err)
		return m.Run()
	}
	builtBinary = filepath.Join(dir, "debug", BinaryName)
	if _, err := os.Stat(builtBinary); err != nil {
		buildErr = fmt.Errorf("built binary missing: %w", err)
	}
	return m.Run()
}

// requireBinary returns the compiled engine or skips the test.
func requireBinary(t *testing.T) string {
	t.Helper()
	if buildErr != nil {
		t.Skipf("dubarr-sync build unavailable: %v", buildErr)
	}
	if builtBinary == "" {
		t.Skip("dubarr-sync binary not built")
	}
	return builtBinary
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func newTestRunner(t *testing.T) *Runner {
	t.Helper()
	r, err := New(requireBinary(t), WithLogger(discardLogger()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
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

func TestJSONContract(t *testing.T) {
	window := 1.5
	job := Job{
		Target:        PathSource("/media/target.mka"),
		Donor:         PCMSource(48000, []float32{0.5, -0.5}),
		SliceStartSec: 12.25,
		SliceLenSec:   10,
		WindowSec:     &window,
	}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"target":{"path":"/media/target.mka"},"donor":{"pcm":{"sample_rate":48000,"samples":[0.5,-0.5]}},"slice_start_sec":12.25,"slice_len_sec":10,"window_sec":1.5}`
	if string(raw) != want {
		t.Fatalf("job JSON mismatch\n got: %s\nwant: %s", raw, want)
	}

	// window_sec is Optional in Rust: an unset pointer must be omitted, never
	// serialised as null.
	job.WindowSec = nil
	raw, err = json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(raw); strings.Contains(got, "window_sec") {
		t.Fatalf("window_sec must be omitted when nil: %s", got)
	}

	var result Result
	fixture := `{"offset_ms":250.0,"correlation_quality":0.987,"sample_rate":8000,"analyzed":{"start":0.0,"len":2.0}}`
	if err := json.Unmarshal([]byte(fixture), &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	want2 := Result{OffsetMS: 250, CorrelationQuality: 0.987, SampleRate: 8000, Analyzed: Analyzed{Start: 0, Len: 2}}
	if result != want2 {
		t.Fatalf("result decode: got %+v want %+v", result, want2)
	}
}

func TestRunSuccess(t *testing.T) {
	r := newTestRunner(t)
	// The construction handshake must have parsed the clap --version banner.
	if r.Version() == "" {
		t.Fatalf("version handshake did not report a version (path %s)", r.Path())
	}
	const sampleRate = 8000
	const n = 2 * sampleRate
	const delaySamples = 2000 // 250 ms

	target := noiseSignal(n, 7)
	donor := advanced(target, delaySamples)
	window := 1.0

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := r.Run(ctx, Job{
		Target:        PCMSource(sampleRate, target),
		Donor:         PCMSource(sampleRate, donor),
		SliceStartSec: 0,
		SliceLenSec:   float64(n) / sampleRate,
		WindowSec:     &window,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantMS := float64(delaySamples) * 1000 / sampleRate
	if math.Abs(res.OffsetMS-wantMS) > 1 {
		t.Fatalf("offset_ms = %v, want %v", res.OffsetMS, wantMS)
	}
	if res.SampleRate != sampleRate {
		t.Fatalf("sample_rate = %d, want %d", res.SampleRate, sampleRate)
	}
	if res.CorrelationQuality < 0.9 {
		t.Fatalf("correlation_quality = %v, want >= 0.9", res.CorrelationQuality)
	}
	if math.Abs(res.Analyzed.Len-2) > 1e-9 {
		t.Fatalf("analyzed.len = %v, want 2", res.Analyzed.Len)
	}
}

func TestRunMismatchedRates(t *testing.T) {
	r := newTestRunner(t)
	_, err := r.Run(context.Background(), Job{
		Target:        PCMSource(8000, noiseSignal(1000, 1)),
		Donor:         PCMSource(16000, noiseSignal(1000, 2)),
		SliceStartSec: 0,
		SliceLenSec:   0.1,
	})
	if !errors.Is(err, ErrAnalysis) {
		t.Fatalf("err = %v, want ErrAnalysis", err)
	}
	kind, ok := KindOf(err)
	if !ok || kind != KindAnalysis {
		t.Fatalf("KindOf = %q/%v, want %q", kind, ok, KindAnalysis)
	}
	var typed *Error
	if !errors.As(err, &typed) || typed.ExitCode != 3 {
		t.Fatalf("expected *Error with exit 3, got %#v", err)
	}
}

func TestRunShortSlice(t *testing.T) {
	r := newTestRunner(t)
	_, err := r.Run(context.Background(), Job{
		Target:        PCMSource(8000, noiseSignal(1000, 3)),
		Donor:         PCMSource(8000, noiseSignal(1000, 4)),
		SliceStartSec: 0,
		SliceLenSec:   0.0001, // 0.8 samples -> below MIN_SLICE_SAMPLES
	})
	if !errors.Is(err, ErrAnalysis) {
		t.Fatalf("err = %v, want ErrAnalysis", err)
	}
}

func TestRunUsageError(t *testing.T) {
	r := newTestRunner(t)
	_, err := r.Run(context.Background(), Job{
		Target:        PCMSource(8000, noiseSignal(1000, 5)),
		Donor:         PCMSource(8000, noiseSignal(1000, 6)),
		SliceStartSec: 0,
		SliceLenSec:   -1, // rejected by the engine as InvalidJob -> exit 2
	})
	// The bridge's own validation catches this first; both map to a client
	// usage error.
	if !errors.Is(err, ErrInvalidJob) && !errors.Is(err, ErrUsage) {
		t.Fatalf("err = %v, want ErrInvalidJob or ErrUsage", err)
	}
}

// fakeBinary writes an executable shell script that ignores stdin.
func fakeBinary(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dubarr-sync")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

func TestRunTimeout(t *testing.T) {
	path := fakeBinary(t, "sleep 5")
	r, err := New(path, WithLogger(discardLogger()), WithTimeout(100*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	start := time.Now()
	_, err = r.Run(context.Background(), Job{
		Target:        PCMSource(8000, noiseSignal(1000, 7)),
		Donor:         PCMSource(8000, noiseSignal(1000, 8)),
		SliceStartSec: 0,
		SliceLenSec:   0.1,
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout took %s, want ~100ms", elapsed)
	}
}

func TestRunMalformedOutput(t *testing.T) {
	for name, script := range map[string]string{
		"garbage": "cat >/dev/null; echo 'not json'",
		"empty":   "cat >/dev/null; true",
		"no_rate": "cat >/dev/null; echo '{\"offset_ms\":1}'",
	} {
		t.Run(name, func(t *testing.T) {
			r, err := New(fakeBinary(t, script), WithLogger(discardLogger()))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = r.Run(context.Background(), Job{
				Target:        PCMSource(8000, noiseSignal(1000, 9)),
				Donor:         PCMSource(8000, noiseSignal(1000, 10)),
				SliceStartSec: 0,
				SliceLenSec:   0.1,
			})
			if !errors.Is(err, ErrMalformedOutput) {
				t.Fatalf("err = %v, want ErrMalformedOutput", err)
			}
		})
	}
}

func TestNewMissingBinary(t *testing.T) {
	_, err := New(filepath.Join(t.TempDir(), "does-not-exist"), WithLogger(discardLogger()))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestRunInvalidJobLocal(t *testing.T) {
	// No binary needed: local validation must reject before spawning.
	r, err := New(fakeBinary(t, "exit 99"), WithLogger(discardLogger()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := map[string]Job{
		"no target":      {Donor: PCMSource(8000, []float32{0.1}), SliceLenSec: 1},
		"both sources":   {Target: Source{Path: "x", PCM: &PCM{SampleRate: 8000, Samples: []float32{0.1}}}, Donor: PCMSource(8000, []float32{0.1}), SliceLenSec: 1},
		"zero rate":      {Target: PCMSource(0, []float32{0.1}), Donor: PCMSource(8000, []float32{0.1}), SliceLenSec: 1},
		"nan sample":     {Target: PCMSource(8000, []float32{float32(math.NaN())}), Donor: PCMSource(8000, []float32{0.1}), SliceLenSec: 1},
		"negative slice": {Target: PCMSource(8000, []float32{0.1}), Donor: PCMSource(8000, []float32{0.1}), SliceStartSec: -1, SliceLenSec: 1},
		"zero slice len": {Target: PCMSource(8000, []float32{0.1}), Donor: PCMSource(8000, []float32{0.1}), SliceLenSec: 0},
	}
	for name, job := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := r.Run(context.Background(), job); !errors.Is(err, ErrInvalidJob) {
				t.Fatalf("err = %v, want ErrInvalidJob", err)
			}
		})
	}
}
