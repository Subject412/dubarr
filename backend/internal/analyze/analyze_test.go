package analyze

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/reflang"
	"github.com/Subject412/dubarr/backend/internal/sync"
)

// builtBinary is the dubarr-sync executable compiled once by TestMain, using
// the same build-once pattern as internal/sync.
var builtBinary string
var buildErr error

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
	dir, err := os.MkdirTemp("", "dubarr-analyze-build-")
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
	builtBinary = filepath.Join(dir, "debug", sync.BinaryName)
	if _, err := os.Stat(builtBinary); err != nil {
		buildErr = fmt.Errorf("built binary missing: %w", err)
	}
	return m.Run()
}

func requireEngine(t *testing.T) Engine {
	t.Helper()
	if buildErr != nil {
		t.Skipf("dubarr-sync build unavailable: %v", buildErr)
	}
	if builtBinary == "" {
		t.Skip("dubarr-sync binary not built")
	}
	r, err := sync.New(builtBinary, sync.WithTimeout(JobTimeout(DefaultConfig())))
	if err != nil {
		t.Fatalf("sync.New: %v", err)
	}
	return r
}

// scriptedEngine is the aggregation seam: it returns canned offsets per job so
// the classification logic is unit-tested without the Rust binary.
type scriptedEngine struct {
	offsets   []float64
	qualities []float64
	calls     []sync.Job
	err       error
}

func (e *scriptedEngine) Run(_ context.Context, job sync.Job) (sync.Result, error) {
	e.calls = append(e.calls, job)
	if e.err != nil {
		return sync.Result{}, e.err
	}
	i := len(e.calls) - 1
	if i >= len(e.offsets) {
		return sync.Result{}, errors.New("scripted engine: unexpected extra job")
	}
	q := 0.9
	if i < len(e.qualities) {
		q = e.qualities[i]
	}
	return sync.Result{OffsetMS: e.offsets[i], CorrelationQuality: q, SampleRate: 8000}, nil
}

func sameLangRefs() ([]probe.AudioTrack, []probe.AudioTrack, reflang.Languages, string) {
	target := []probe.AudioTrack{
		{Index: 1, Language: "pol", LanguageISO1: "pl", LanguageISO2B: "pol", Channels: 2},
		{Index: 2, Language: "eng", LanguageISO1: "en", LanguageISO2B: "eng", Channels: 2},
	}
	vault := []probe.AudioTrack{
		{Index: 0, Language: "eng", LanguageISO1: "en", LanguageISO2B: "eng", Channels: 2},
	}
	return target, vault, reflang.Languages{Targets: []string{"pl"}}, "eng"
}

func lowpassRefs() ([]probe.AudioTrack, []probe.AudioTrack, reflang.Languages, string) {
	target := []probe.AudioTrack{
		{Index: 1, Language: "pol", LanguageISO1: "pl", LanguageISO2B: "pol", Channels: 2},
	}
	vault := []probe.AudioTrack{
		{Index: 0, Language: "pol", LanguageISO1: "pl", LanguageISO2B: "pol", Channels: 2},
	}
	return target, vault, reflang.Languages{Targets: []string{"pl"}}, ""
}

func request(path string) Request {
	t, v, p, orig := sameLangRefs()
	return Request{
		TargetPath:       filepath.Join(path, "target.wav"),
		DonorPath:        filepath.Join(path, "donor.wav"),
		DurationSec:      600,
		TargetTracks:     t,
		VaultTracks:      v,
		Policy:           p,
		OriginalLanguage: orig,
	}
}

func TestAnalyzeStable(t *testing.T) {
	eng := &scriptedEngine{offsets: []float64{1200, 1210, 1195, 1205, 1198, 1202}, qualities: []float64{0.91, 0.92, 0.9, 0.93, 0.94, 0.95}}
	cfg := DefaultConfig()
	rep, err := Analyze(context.Background(), eng, cfg, request(t.TempDir()))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.Verdict != VerdictOK {
		t.Fatalf("verdict = %q, want %q", rep.Verdict, VerdictOK)
	}
	if rep.Classification != ClassificationStable {
		t.Fatalf("classification = %q, want %q", rep.Classification, ClassificationStable)
	}
	if rep.SyncMethod != reflang.SameLangReference {
		t.Fatalf("sync_method = %q, want %q", rep.SyncMethod, reflang.SameLangReference)
	}
	if got, want := rep.MaxDriftMS, 15.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("max_drift_ms = %v, want %v", got, want)
	}
	if len(rep.OffsetsMS) != 6 || len(rep.Qualities) != 6 || len(rep.Checkpoints) != 6 {
		t.Fatalf("report lengths = %d/%d/%d, want 6", len(rep.OffsetsMS), len(rep.Qualities), len(rep.Checkpoints))
	}
	// slice_start = pct*duration - window/2, clamped into the file. The
	// analysed slice is widened to max_offset_sec + margin (190 s).
	const sliceLen = 190.0
	wantFirst := 0.0
	if got := rep.Checkpoints[0].SliceStartSec; math.Abs(got-wantFirst) > 1e-9 {
		t.Fatalf("first slice_start = %v, want %v", got, wantFirst)
	}
	want55 := 0.55*600 - sliceLen/2
	if got := rep.Checkpoints[3].SliceStartSec; math.Abs(got-want55) > 1e-9 {
		t.Fatalf("55%% slice_start = %v, want %v", got, want55)
	}
	if len(eng.calls) != 6 {
		t.Fatalf("engine calls = %d, want 6", len(eng.calls))
	}
	if got, want := eng.calls[0].SliceLenSec, sliceLen; got != want {
		t.Fatalf("slice_len = %v, want %v", got, want)
	}
	// The engine search window must cover the full max_offset_sec bound, not
	// just the 150 s checkpoint window (IDEA.md §5.1).
	if eng.calls[0].WindowSec == nil {
		t.Fatal("window_sec = nil, want the analysed slice length")
	}
	if got, want := *eng.calls[0].WindowSec, sliceLen; got != want {
		t.Fatalf("window_sec = %v, want %v", got, want)
	}
	if got, want := *eng.calls[0].WindowSec, float64(DefaultMaxOffsetSec); got < want {
		t.Fatalf("window_sec = %v, want >= max_offset_sec %v", got, want)
	}
}

func TestSliceWindow(t *testing.T) {
	cases := []struct {
		name     string
		duration float64
		pct      int
		window   float64
		start    float64
		length   float64
	}{
		{"head clamp", 600, 10, 190, 0, 190},
		{"centred", 600, 55, 190, 235, 190},
		{"tail clamp", 600, 85, 190, 410, 190},
		{"file shorter than window", 100, 50, 190, 0, 100},
		{"file shorter at head", 120, 10, 190, 0, 120},
		{"file equals window", 190, 50, 190, 0, 190},
		{"file just short of window", 189, 99, 190, 0, 189},
		{"window below file", 600, 1, 150, 0, 150},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, length := sliceWindow(tc.duration, tc.pct, tc.window)
			if math.Abs(start-tc.start) > 1e-9 || math.Abs(length-tc.length) > 1e-9 {
				t.Fatalf("sliceWindow(%v, %d, %v) = (%v, %v), want (%v, %v)",
					tc.duration, tc.pct, tc.window, start, length, tc.start, tc.length)
			}
			if start < 0 || length <= 0 || start+length > tc.duration+1e-9 {
				t.Fatalf("window (%v, %v) escapes [0, %v]", start, length, tc.duration)
			}
		})
	}
}

func TestAnalyzeDrifting(t *testing.T) {
	eng := &scriptedEngine{offsets: []float64{500, 500, 500, 2000, 2000, 2000}}
	rep, err := Analyze(context.Background(), eng, DefaultConfig(), request(t.TempDir()))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.Verdict != VerdictDrifting || rep.Classification != ClassificationDrifting {
		t.Fatalf("verdict/classification = %q/%q, want drifting/drifting", rep.Verdict, rep.Classification)
	}
	if got, want := rep.MaxDriftMS, 1500.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("max_drift_ms = %v, want %v", got, want)
	}
}

func TestAnalyzeOutOfRange(t *testing.T) {
	eng := &scriptedEngine{offsets: []float64{200000, 200100, 199900, 200050, 200000, 200010}}
	cfg := DefaultConfig()
	cfg.MaxOffsetSec = 180
	rep, err := Analyze(context.Background(), eng, cfg, request(t.TempDir()))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.Verdict != VerdictOutOfRange || rep.Classification != ClassificationOutOfRange {
		t.Fatalf("verdict/classification = %q/%q, want out_of_range", rep.Verdict, rep.Classification)
	}
}

// TestAnalyzeVerdictPrecedence drives the aggregation seam through the
// precedence matrix out_of_range > unreliable > drifting > ok.
func TestAnalyzeVerdictPrecedence(t *testing.T) {
	const hi = 0.9
	stable := []float64{1200, 1210, 1195, 1205, 1198, 1202}
	drifting := []float64{500, 500, 500, 2000, 2000, 2000}
	beyond := []float64{200000, 200100, 199900, 200050, 200000, 200010}

	cases := []struct {
		name           string
		offsets        []float64
		qualities      []float64
		wantVerdict    string
		wantClass      string
		wantMaxDriftMS float64
	}{
		{
			name:        "ok",
			offsets:     stable,
			qualities:   []float64{hi, hi, hi, hi, hi, hi},
			wantVerdict: VerdictOK, wantClass: ClassificationStable,
			wantMaxDriftMS: 15,
		},
		{
			name:        "drifting",
			offsets:     drifting,
			qualities:   []float64{hi, hi, hi, hi, hi, hi},
			wantVerdict: VerdictDrifting, wantClass: ClassificationDrifting,
			wantMaxDriftMS: 1500,
		},
		{
			name:        "single weak checkpoint is unreliable",
			offsets:     stable,
			qualities:   []float64{hi, hi, 0.05, hi, hi, hi},
			wantVerdict: VerdictUnreliable, wantClass: ClassificationUnreliable,
			wantMaxDriftMS: 15,
		},
		{
			name:        "unreliable outranks drifting",
			offsets:     drifting,
			qualities:   []float64{hi, hi, 0.02, hi, hi, hi},
			wantVerdict: VerdictUnreliable, wantClass: ClassificationUnreliable,
			wantMaxDriftMS: 1500,
		},
		{
			name:        "out_of_range outranks unreliable",
			offsets:     beyond,
			qualities:   []float64{0.05, 0.05, 0.05, 0.05, 0.05, 0.05},
			wantVerdict: VerdictOutOfRange, wantClass: ClassificationOutOfRange,
			wantMaxDriftMS: 200,
		},
		{
			name:        "quality at threshold is trusted",
			offsets:     stable,
			qualities:   []float64{hi, hi, LowQualityThreshold, hi, hi, hi},
			wantVerdict: VerdictOK, wantClass: ClassificationStable,
			wantMaxDriftMS: 15,
		},
		{
			name:        "quality just below threshold is unreliable",
			offsets:     stable,
			qualities:   []float64{hi, hi, math.Nextafter(LowQualityThreshold, 0), hi, hi, hi},
			wantVerdict: VerdictUnreliable, wantClass: ClassificationUnreliable,
			wantMaxDriftMS: 15,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := &scriptedEngine{offsets: tc.offsets, qualities: tc.qualities}
			rep, err := Analyze(context.Background(), eng, DefaultConfig(), request(t.TempDir()))
			if err != nil {
				t.Fatalf("Analyze: %v", err)
			}
			if rep.Verdict != tc.wantVerdict || rep.Classification != tc.wantClass {
				t.Fatalf("verdict/classification = %q/%q, want %q/%q",
					rep.Verdict, rep.Classification, tc.wantVerdict, tc.wantClass)
			}
			if math.Abs(rep.MaxDriftMS-tc.wantMaxDriftMS) > 1e-9 {
				t.Fatalf("max_drift_ms = %v, want %v", rep.MaxDriftMS, tc.wantMaxDriftMS)
			}
			if len(rep.Qualities) != len(tc.qualities) {
				t.Fatalf("qualities length = %d, want %d", len(rep.Qualities), len(tc.qualities))
			}
		})
	}
}

func TestAnalyzeLowpass(t *testing.T) {
	eng := &scriptedEngine{offsets: []float64{100, 100, 100, 100, 100, 100}}
	req := request(t.TempDir())
	req.TargetTracks, req.VaultTracks, req.Policy, req.OriginalLanguage = lowpassRefs()
	rep, err := Analyze(context.Background(), eng, DefaultConfig(), req)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.SyncMethod != reflang.LowpassFallback {
		t.Fatalf("sync_method = %q, want %q", rep.SyncMethod, reflang.LowpassFallback)
	}
}

func TestAnalyzeNoReference(t *testing.T) {
	eng := &scriptedEngine{offsets: []float64{0}}
	req := request(t.TempDir())
	req.TargetTracks = nil
	if _, err := Analyze(context.Background(), eng, DefaultConfig(), req); err == nil {
		t.Fatal("Analyze with no reference pairs: want error")
	}
	if len(eng.calls) != 0 {
		t.Fatalf("engine ran %d jobs before rejecting, want 0", len(eng.calls))
	}
}

func TestAnalyzeEngineError(t *testing.T) {
	eng := &scriptedEngine{err: sync.ErrAnalysis}
	_, err := Analyze(context.Background(), eng, DefaultConfig(), request(t.TempDir()))
	if !errors.Is(err, sync.ErrAnalysis) {
		t.Fatalf("Analyze error = %v, want sync.ErrAnalysis", err)
	}
}

func TestConfigValidate(t *testing.T) {
	cases := []Config{
		{CheckpointPositionsPct: []int{0}, CheckpointWindowSec: 150, MaxOffsetSec: 180},
		{CheckpointPositionsPct: []int{101}, CheckpointWindowSec: 150, MaxOffsetSec: 180},
		{CheckpointPositionsPct: []int{10}, CheckpointWindowSec: 0, MaxOffsetSec: 180},
		{CheckpointPositionsPct: []int{10}, CheckpointWindowSec: 150, MaxOffsetSec: 0},
	}
	for i, c := range cases {
		if err := c.validate(); err == nil {
			t.Fatalf("case %d: validate(%+v) = nil, want error", i, c)
		}
	}
	if err := DefaultConfig().validate(); err != nil {
		t.Fatalf("DefaultConfig().validate: %v", err)
	}
}

func TestReportJSONShape(t *testing.T) {
	rep := Report{
		Verdict:        VerdictOK,
		OffsetsMS:      []float64{1, 2},
		Qualities:      []float64{0.5, 0.6},
		MaxDriftMS:     1,
		Classification: ClassificationStable,
		SyncMethod:     reflang.SameLangReference,
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"verdict", "offsets_ms", "qualities", "max_drift_ms", "classification", "sync_method"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("report JSON missing %q: %s", key, raw)
		}
	}
}

// Integration: synthesised WAV pairs through the real Rust engine.

const (
	sampleRate = 1000
	durationS  = 600
)

// noiseSignal mirrors internal/sync's deterministic broadband noise.
func noiseSignal(n int, seed uint64) []float32 {
	out := make([]float32, n)
	x := seed | 1
	for i := range out {
		x = x*6364136223846793005 + 1442695040888963407
		out[i] = float32(int64(x>>33)) / float32(1<<31)
	}
	return out
}

// shiftedDonor returns donor[t] = target[t+lead], i.e. the donor runs lead
// samples ahead, so offset_ms = t_target - t_donor is positive.
func shiftedDonor(target []float32, lead int) []float32 {
	out := make([]float32, len(target))
	copy(out, target[lead:])
	return out
}

// piecewiseDonor shifts by leadA before split and leadB from split on. Test
// checkpoint windows straddle neither side of split.
func piecewiseDonor(target []float32, leadA, leadB, split int) []float32 {
	out := make([]float32, len(target))
	for i := range out {
		lead := leadA
		if i >= split {
			lead = leadB
		}
		if i+lead < len(target) {
			out[i] = target[i+lead]
		}
	}
	return out
}

// writeWAV writes mono float32 PCM in a RIFF/WAVE container.
func writeWAV(t *testing.T, path string, samples []float32) {
	t.Helper()
	writeWAVRate(t, path, samples, sampleRate)
}

// writeWAVRate is writeWAV with an explicit sample rate.
func writeWAVRate(t *testing.T, path string, samples []float32, rate uint32) {
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

func TestIntegrationConstantOffset(t *testing.T) {
	eng := requireEngine(t)
	dir := t.TempDir()
	n := durationS * sampleRate
	target := noiseSignal(n, 11)
	writeWAV(t, filepath.Join(dir, "target.wav"), target)
	writeWAV(t, filepath.Join(dir, "donor.wav"), shiftedDonor(target, 1200))

	req := request(dir)
	req.DurationSec = 0 // derive from the WAV headers
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rep, err := Analyze(ctx, eng, DefaultConfig(), req)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.Verdict != VerdictOK {
		t.Fatalf("verdict = %q (offsets %v, drift %v), want ok", rep.Verdict, rep.OffsetsMS, rep.MaxDriftMS)
	}
	for i, off := range rep.OffsetsMS {
		if math.Abs(off-1200) > 5 {
			t.Fatalf("checkpoint %d offset = %v, want ~1200", i, off)
		}
	}
	if rep.MaxDriftMS > 40 {
		t.Fatalf("max drift = %v, want <= 40", rep.MaxDriftMS)
	}
}

func TestIntegrationPiecewiseDrift(t *testing.T) {
	eng := requireEngine(t)
	dir := t.TempDir()
	const dur = 1500
	n := dur * sampleRate
	target := noiseSignal(n, 13)
	// 190 s windows centred on 10/25/40/55/70/85 %: 55-245, 280-470, 505-695,
	// 730-920, 955-1145, 1180-1370. A split at 712 s leaves every window
	// entirely on one side.
	donor := piecewiseDonor(target, 500, 2000, 712*sampleRate)
	writeWAV(t, filepath.Join(dir, "target.wav"), target)
	writeWAV(t, filepath.Join(dir, "donor.wav"), donor)

	req := request(dir)
	req.DurationSec = dur
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rep, err := Analyze(ctx, eng, DefaultConfig(), req)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.Verdict != VerdictDrifting || rep.Classification != ClassificationDrifting {
		t.Fatalf("verdict/classification = %q/%q (offsets %v), want drifting", rep.Verdict, rep.Classification, rep.OffsetsMS)
	}
	if rep.MaxDriftMS < 1000 {
		t.Fatalf("max drift = %v, want >= 1000", rep.MaxDriftMS)
	}
}

func TestIntegrationOutOfRange(t *testing.T) {
	eng := requireEngine(t)
	dir := t.TempDir()
	n := durationS * sampleRate
	target := noiseSignal(n, 17)
	writeWAV(t, filepath.Join(dir, "target.wav"), target)
	// A 5 s shift is visible inside every 150 s slice but exceeds the
	// configured 2 s acceptance bound.
	writeWAV(t, filepath.Join(dir, "donor.wav"), shiftedDonor(target, 5*sampleRate))

	cfg := DefaultConfig()
	cfg.MaxOffsetSec = 2
	req := request(dir)
	req.DurationSec = 0
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rep, err := Analyze(ctx, eng, cfg, req)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.Verdict != VerdictOutOfRange {
		t.Fatalf("verdict = %q (offsets %v), want out_of_range", rep.Verdict, rep.OffsetsMS)
	}
}

// recordingEngine wraps the real engine and keeps the jobs it was handed, so
// integration tests can assert the window the analysis asked for.
type recordingEngine struct {
	inner Engine
	calls []sync.Job
}

func (r *recordingEngine) Run(ctx context.Context, job sync.Job) (sync.Result, error) {
	r.calls = append(r.calls, job)
	return r.inner.Run(ctx, job)
}

// TestIntegrationOffsetWithinSearchWindow places the donor 165 s ahead: beyond
// the 150 s checkpoint window but inside the 180 s max_offset_sec bound. The
// engine can only report it because the analysed slice is widened to
// max_offset_sec + margin; with a 150 s slice the lag cap hides it.
func TestIntegrationOffsetWithinSearchWindow(t *testing.T) {
	eng := &recordingEngine{inner: requireEngine(t)}
	dir := t.TempDir()
	const (
		rate   = 8000
		dur    = 600
		leadMS = 165000
	)
	target := noiseSignal(dur*rate, 19)
	writeWAVRate(t, filepath.Join(dir, "target.wav"), target, rate)
	writeWAVRate(t, filepath.Join(dir, "donor.wav"), shiftedDonor(target, leadMS*rate/1000), rate)

	req := request(dir)
	req.DurationSec = dur
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rep, err := Analyze(ctx, eng, DefaultConfig(), req)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	want := float64(DefaultMaxOffsetSec + CheckWindowMarginSec) // 190 s slice
	for i, job := range eng.calls {
		if job.SliceLenSec != want {
			t.Fatalf("checkpoint %d slice_len = %v, want %v", i, job.SliceLenSec, want)
		}
		if job.WindowSec == nil || *job.WindowSec != want {
			t.Fatalf("checkpoint %d window_sec = %v, want %v", i, job.WindowSec, want)
		}
	}
	for i, off := range rep.OffsetsMS {
		if math.Abs(off-leadMS) > 50 {
			t.Fatalf("checkpoint %d offset = %v, want ~%v", i, off, leadMS)
		}
	}
	// 165 s is inside the settled 180 s acceptance bound.
	if rep.Verdict != VerdictOK || rep.Classification != ClassificationStable {
		t.Fatalf("verdict/classification = %q/%q (offsets %v), want ok/stable", rep.Verdict, rep.Classification, rep.OffsetsMS)
	}
}
