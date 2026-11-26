//go:build syncmeasure

// Measurement harness for issue #22/#23 evidence (measurement protocol
// §1–§3, work order §8.1). It drives the same acceptance fixture and the same
// production seams as TestShrekEndToEnd, but invokes every stage individually
// so each step's wall time, child CPU (user/sys) and child peak RSS
// (getrusage maxrss) plus the {WORK_DIR}/jobs/{id} disk footprint can be
// recorded. It is behind the `syncmeasure` build tag so normal `go test ./...`
// never pays for it.
//
// Emitted lines (all `KEY|field=value…`, see protocol §3):
//
//	RUNMETA|…              provenance: commit/cache policy/tool versions/host
//	PT|…                   parent (test process) 1 Hz timeline
//	CHILD|<step>|…         child 1 Hz timeline (steps executed by this file)
//	STEP|<step>|…          one row per stage: wall + cpu + maxrss
//	DISK|… / MUX|…         existing staging-footprint and mux argv lines
//	SYNCOFFSET|…           the --sync value handed to mkvmerge
//	PUBLISH|…              published-file validation
//	REPORT|…               analyze.Report (verdict/offsets/quality/checkpoints)
//	TOTAL|…                movie_wall_ms (monotonic, no double count), job_wall_ms
//	SUMMARY|…              legacy summed fields + parent peaks/CPU + child CPU
//
// Linux-only by design (ADR-0006): /proc and getrusage are Linux/POSIX
// interfaces, and the whole file is already gated behind `syncmeasure`.
//
// Run:
//
//	cd backend && SYNC_MEASURE_COMMIT=<sha> SYNC_MEASURE_CACHE_POLICY=warm \
//	  go test -tags syncmeasure -run TestMeasureSyncResources \
//	  -v -count=1 -timeout 60m ./internal/acceptance
package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/extract"
	"github.com/Subject412/dubarr/backend/internal/gate"
	"github.com/Subject412/dubarr/backend/internal/mux"
	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/reflang"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
	syncengine "github.com/Subject412/dubarr/backend/internal/sync"
	"github.com/Subject412/dubarr/backend/internal/workdir"
)

// measureRow is one recorded stage. MaxRSS keeps its historical meaning (child
// peak RSS in KiB, 0 when unknown) so old logs still parse.
type measureRow struct {
	Step        string
	TStart      time.Duration
	Wall        time.Duration
	CPUUserMS   int64
	CPUSysMS    int64
	MaxRSS      int64 // KiB from getrusage(RUSAGE_CHILDREN of the child); 0 unknown
	ChildUserMS int64 // indirect-child CPU (M7), see record
	ChildSysMS  int64
	OK          bool
	Detail      string
}

// seamCall is one tool invocation made by a production package through
// recorder.runSeam (probe/mux). Its rusage is exact, but the pid is not
// reachable from inside the adapter, so it contributes one exact CHILD| point
// instead of a sampled series.
type seamCall struct {
	step      string
	tRelStart time.Duration
	userMS    int64
	sysMS     int64
	maxRSSKB  int64
}

type recorder struct {
	t *testing.T

	origin time.Time
	mu     sync.Mutex
	rows   []measureRow
	seam   []seamCall
	step   string

	interval     time.Duration
	childSamples bool

	prevChildren childUsage

	parent          *parentSampler
	parentRSS       int64
	parentHWm       int64
	parentCPUUserMS int64
	parentCPUSysMS  int64
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	interval := time.Second
	if raw := strings.TrimSpace(os.Getenv("SYNC_MEASURE_INTERVAL_S")); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 {
			interval = time.Duration(v * float64(time.Second))
		}
	}
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	childSamples := true
	if raw := strings.TrimSpace(os.Getenv("SYNC_MEASURE_CHILD_SAMPLES")); raw != "" {
		childSamples = raw != "0"
	}
	return &recorder{
		t:            t,
		origin:       time.Now(),
		interval:     interval,
		childSamples: childSamples,
		prevChildren: readChildrenUsage(),
	}
}

func (r *recorder) setStep(step string) {
	r.mu.Lock()
	r.step = step
	r.mu.Unlock()
}

// record stores one stage. cpuUserMS/cpuSysMS/maxRSS come from the step's
// directly measured child (runCommand, or the seam calls routed through
// probe/mux). Seam calls made by the active step are folded in here so the
// step still yields exactly one STEP| row. The RUSAGE_CHILDREN delta is then
// reduced by the directly measured CPU: what remains is the CPU of children
// spawned *indirectly* by the step (M7 — extract_vault has no Run seam at all,
// analyze_total owns the six engine spawns).
func (r *recorder) record(step string, wall time.Duration, cpuUserMS, cpuSysMS, maxRSS int64, ok bool, detail string) {
	tEnd := time.Since(r.origin)
	tStart := tEnd - wall

	r.mu.Lock()
	seams := r.seam
	r.seam = nil
	r.mu.Unlock()

	for _, c := range seams {
		cpuUserMS += c.userMS
		cpuSysMS += c.sysMS
		if c.maxRSSKB > maxRSS {
			maxRSS = c.maxRSSKB
		}
	}

	indirectUser, indirectSys := r.childUsageDelta(cpuUserMS, cpuSysMS)

	row := measureRow{
		Step:        step,
		TStart:      tStart,
		Wall:        wall,
		CPUUserMS:   cpuUserMS,
		CPUSysMS:    cpuSysMS,
		MaxRSS:      maxRSS,
		ChildUserMS: indirectUser,
		ChildSysMS:  indirectSys,
		OK:          ok,
		Detail:      detail,
	}
	r.mu.Lock()
	r.rows = append(r.rows, row)
	r.mu.Unlock()

	// One exact point per seam-invoked tool: no sampling interval can resolve
	// a ~200 ms ffprobe, but its rusage is exact.
	for _, c := range seams {
		r.t.Logf("CHILD|%s|t_rel_s=%s|rss_kb=%d|cpu_user_ms=%d|cpu_sys_ms=%d",
			step, f3(c.tRelStart.Seconds()), c.maxRSSKB, c.userMS, c.sysMS)
	}

	line := fmt.Sprintf("STEP|%s|t_rel_s=%s|wall_ms=%d|cpu_user_ms=%d|cpu_sys_ms=%d|maxrss_kb=%d|child_cpu_user_ms=%d|child_cpu_sys_ms=%d|ok=%t",
		step, f3(tStart.Seconds()), wall.Milliseconds(), cpuUserMS, cpuSysMS, maxRSS, indirectUser, indirectSys, ok)
	if detail != "" {
		line += "|" + detail
	}
	r.t.Log(line)
}

// childUsageDelta snapshots RUSAGE_CHILDREN and returns the per-step delta of
// children reaped since the previous call, reduced by the CPU already measured
// directly for this step. What remains is the cost of children spawned
// *indirectly* (M7): extract_vault's ffmpeg has no Run seam at all, and
// analyze_total owns the six engine spawns. ru_maxrss is never used here — it
// is the max over every child ever reaped, not a per-step peak.
func (r *recorder) childUsageDelta(directUserMS, directSysMS int64) (userMS, sysMS int64) {
	now := readChildrenUsage()
	userMS = now.userMS - r.prevChildren.userMS - directUserMS
	sysMS = now.sysMS - r.prevChildren.sysMS - directSysMS
	r.prevChildren = now
	if userMS < 0 {
		userMS = 0
	}
	if sysMS < 0 {
		sysMS = 0
	}
	return userMS, sysMS
}

// runSeam adapts recorder to the production seam signature
// `func(ctx, name string, args ...string) ([]byte, error)` used by
// probe.Dependencies.Run and mux.Dependencies.Run. It records the child's
// exact rusage for the step set with setStep so ffprobe/mkvmerge are measured
// instead of reporting maxrss_kb=0.
//
// The two production defaults differ and both are mirrored here so the
// measurement does not change behaviour: mux's execRun uses CombinedOutput and
// accepts mkvmerge's warning exit 1 when it produced output; probe uses
// cmd.Output (stdout only) and treats any non-zero exit as failure.
func (r *recorder) runSeam(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	start := time.Now()
	runErr := cmd.Run()
	wall := time.Since(start)

	var usage seamCall
	usage.tRelStart = time.Since(r.origin) - wall
	if cmd.ProcessState != nil {
		if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
			usage.userMS = timevalMS(ru.Utime)
			usage.sysMS = timevalMS(ru.Stime)
			usage.maxRSSKB = ru.Maxrss
		}
	}
	r.mu.Lock()
	usage.step = r.step
	if r.step != "" {
		r.seam = append(r.seam, usage)
	}
	r.mu.Unlock()

	result := out.Bytes()
	if strings.Contains(filepath.Base(name), "mkvmerge") {
		result = append(append([]byte(nil), out.Bytes()...), errOut.Bytes()...)
		if runErr != nil {
			var exitErr *exec.ExitError
			if errors.As(runErr, &exitErr) && exitErr.ExitCode() == 1 && len(bytes.TrimSpace(result)) > 0 {
				runErr = nil
			}
		}
	}
	if runErr != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), runErr,
			strings.TrimSpace(string(result)))
	}
	return result, nil
}

// runCommand executes one external tool and records its wall time, the child's
// CPU (M2) and the child's peak RSS (M3; Linux: ru_maxrss in KiB, whole child
// lifetime). While it runs, a 1 Hz sampler emits the CHILD| series (M4).
func (r *recorder) runCommand(ctx context.Context, step, name string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	start := time.Now()
	if err := cmd.Start(); err != nil {
		r.record(step, time.Since(start), 0, 0, 0, false,
			"error="+strings.ReplaceAll(err.Error(), "|", " "))
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	stopSampling := r.sampleChild(step, cmd.Process.Pid)
	err := cmd.Wait()
	stopSampling()
	wall := time.Since(start)

	var cpuUserMS, cpuSysMS, maxRSS int64
	if cmd.ProcessState != nil {
		if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
			cpuUserMS = timevalMS(ru.Utime)
			cpuSysMS = timevalMS(ru.Stime)
			maxRSS = ru.Maxrss
		}
	}
	detail := ""
	if err != nil {
		detail = "error=" + strings.ReplaceAll(strings.TrimSpace(errOut.String()), "|", " ")
	}
	r.record(step, wall, cpuUserMS, cpuSysMS, maxRSS, err == nil, detail)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, errOut.String())
	}
	return out.Bytes(), nil
}

// sampleChild starts the 1 Hz /proc/<pid> poller for a just-started child and
// returns the function that stops it. Gated by SYNC_MEASURE_CHILD_SAMPLES
// (default enabled).
func (r *recorder) sampleChild(step string, pid int) func() {
	if !r.childSamples || pid <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		statPath := "/proc/" + strconv.Itoa(pid) + "/stat"
		statusPath := "/proc/" + strconv.Itoa(pid) + "/status"
		emit := func() {
			userMS, sysMS, ok := readStatCPUMs(statPath)
			if !ok {
				return // child reaped (or unreadable): stop emitting
			}
			kv := readStatusKV(statusPath)
			r.t.Logf("CHILD|%s|t_rel_s=%s|rss_kb=%d|cpu_user_ms=%d|cpu_sys_ms=%d",
				step, f3(time.Since(r.origin).Seconds()), kv["VmRSS"], userMS, sysMS)
		}
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		emit()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				emit()
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// parentSampler is the 1 Hz timeline of the test process itself (M5/M6).
type parentSampler struct {
	stop chan struct{}
	done chan struct{}
}

func (r *recorder) startParentSampler() {
	if r.parent != nil {
		return
	}
	sp := &parentSampler{stop: make(chan struct{}), done: make(chan struct{})}
	r.parent = sp
	go func() {
		defer close(sp.done)
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		r.emitParentSample()
		for {
			select {
			case <-sp.stop:
				return
			case <-ticker.C:
				r.emitParentSample()
			}
		}
	}()
}

func (r *recorder) stopParentSampler() {
	if r.parent == nil {
		return
	}
	close(r.parent.stop)
	<-r.parent.done
	r.parent = nil
	r.emitParentSample()
}

func (r *recorder) emitParentSample() {
	userMS, sysMS, ok := readStatCPUMs("/proc/self/stat")
	kv := readStatusKV("/proc/self/status")
	rss, hwm := kv["VmRSS"], kv["VmHWM"]

	r.mu.Lock()
	if rss > r.parentRSS {
		r.parentRSS = rss
	}
	if hwm > r.parentHWm {
		r.parentHWm = hwm
	}
	if ok {
		r.parentCPUUserMS, r.parentCPUSysMS = userMS, sysMS
	}
	r.mu.Unlock()

	r.t.Logf("PT|t_rel_s=%s|vmrss_kb=%d|vmhwm_kb=%d|cpu_user_ms=%d|cpu_sys_ms=%d",
		f3(time.Since(r.origin).Seconds()), rss, hwm, userMS, sysMS)
}

// childUsage is a RUSAGE_CHILDREN snapshot. ru_maxrss is deliberately unused:
// it is the max over *all* reaped children, never a per-step peak (protocol
// §1, M7 caveat).
type childUsage struct {
	userMS int64
	sysMS  int64
}

func readChildrenUsage() childUsage {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_CHILDREN, &ru); err != nil {
		return childUsage{}
	}
	return childUsage{userMS: timevalMS(ru.Utime), sysMS: timevalMS(ru.Stime)}
}

func timevalMS(tv syscall.Timeval) int64 {
	return int64(tv.Sec)*1000 + int64(tv.Usec)/1000
}

// clkTck resolves CLK_TCK once (Linux userspace convention: 100).
var clkTckOnce sync.Once

func clkTck() int64 {
	tck := int64(100)
	clkTckOnce.Do(func() {
		out, err := exec.Command("getconf", "CLK_TCK").Output()
		if err == nil {
			if v, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); perr == nil && v > 0 {
				tck = v
			}
		}
	})
	return tck
}

// readStatCPUMs returns utime/stime (own CPU, clock ticks → ms) from a
// /proc/<pid>/stat file. comm may contain spaces and parentheses, hence the
// split on the last ')'.
func readStatCPUMs(path string) (userMS, sysMS int64, ok bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	s := string(raw)
	idx := strings.LastIndexByte(s, ')')
	if idx < 0 || idx+2 > len(s) {
		return 0, 0, false
	}
	fields := strings.Fields(s[idx+2:])
	// fields[0] is field 3 (state); utime = field 14, stime = field 15.
	if len(fields) < 13 {
		return 0, 0, false
	}
	u, err1 := strconv.ParseInt(fields[11], 10, 64)
	sy, err2 := strconv.ParseInt(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	tck := clkTck()
	return u * 1000 / tck, sy * 1000 / tck, true
}

// readStatusKV parses `Key: <int> kB` lines of a /proc status file.
func readStatusKV(path string) map[string]int64 {
	out := make(map[string]int64)
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		if v, perr := strconv.ParseInt(fields[0], 10, 64); perr == nil {
			out[key] = v
		}
	}
	return out
}

// measuredEngine is the production JSON-over-stdio contract with timing and RSS
// capture, so each of the six engine spawns is measured individually.
type measuredEngine struct {
	bin string
	rec *recorder
	n   int
}

func (e *measuredEngine) Run(ctx context.Context, job syncengine.Job) (syncengine.Result, error) {
	payload, err := json.Marshal(job)
	if err != nil {
		return syncengine.Result{}, err
	}
	e.n++
	step := fmt.Sprintf("engine_spawn_%d", e.n)
	out, err := e.rec.runCommand(ctx, step, e.bin, payload)
	if err != nil {
		return syncengine.Result{}, err
	}
	var result syncengine.Result
	if err := json.Unmarshal(bytes.TrimSpace(out), &result); err != nil {
		return syncengine.Result{}, fmt.Errorf("decode engine result: %w", err)
	}
	return result, nil
}

// decodeAudio mirrors syncjob.decodeAudio exactly (same ffmpeg argv), measured.
func (r *recorder) decodeAudio(ctx context.Context, step, ffmpeg, input string, stream int, filter, output string) error {
	args := []string{
		"-v", "error", "-nostdin", "-y",
		"-i", input,
		"-map", "0:" + strconv.Itoa(stream),
		"-ac", "1", "-ar", "8000",
		"-af", filter,
		"-c:a", "pcm_f32le",
		output,
	}
	_, err := r.runCommand(ctx, step, ffmpeg, nil, args...)
	return err
}

type dirUsage struct {
	files int
	bytes int64
	names []string
}

func measureDir(t *testing.T, dir string) dirUsage {
	t.Helper()
	var u dirUsage
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		u.files++
		u.bytes += info.Size()
		u.names = append(u.names, fmt.Sprintf("%s=%d", filepath.Base(path), info.Size()))
		return nil
	})
	if err != nil {
		t.Fatalf("measure %s: %v", dir, err)
	}
	sort.Strings(u.names)
	return u
}

func analysisFilter(method reflang.Method) string {
	if method == reflang.LowpassFallback {
		return "highpass=f=30,lowpass=f=200"
	}
	return "highpass=f=80,lowpass=f=3500"
}

func trackAudioTrack(rec store.VaultTrackRecord, index int) probe.AudioTrack {
	return probe.AudioTrack{
		Index:         index,
		Channels:      rec.ChannelCount,
		Language:      rec.LangISO2,
		LanguageISO1:  rec.LangISO2,
		LanguageISO2B: rec.LangISO3,
		Disposition: probe.Disposition{
			Comment: strings.EqualFold(strings.TrimSpace(rec.Variant), "commentary"),
		},
	}
}

func medianOffsetMS(offsets []float64) float64 {
	if len(offsets) == 0 {
		return 0
	}
	sorted := append([]float64(nil), offsets...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return (sorted[mid-1] + sorted[mid]) / 2
	}
	return sorted[mid]
}

// ---- formatting helpers -------------------------------------------------

func f3(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }
func f4(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }

// sanitiseValue keeps a field value a single token: `|` (would fake a field
// separator) becomes a space, runs of whitespace become `_`.
func sanitiseValue(s string) string {
	s = strings.ReplaceAll(s, "|", " ")
	return strings.Join(strings.Fields(s), "_")
}

// firstLine runs a command and returns its first output line as one token.
func firstLine(name string, args ...string) string {
	out, err := exec.Command(name, args...).CombinedOutput()
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if err != nil && line == "" {
		return "unknown"
	}
	return sanitiseValue(line)
}

// toolVersion returns one whitespace-separated field of a tool's version line.
func toolVersion(name string, field int, args ...string) string {
	out, err := exec.Command(name, args...).CombinedOutput()
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	fields := strings.Fields(line)
	if err != nil || field >= len(fields) || field < 0 {
		return "unknown"
	}
	return sanitiseValue(fields[field])
}

func checkpointTriples(cps []analyze.Checkpoint) string {
	if len(cps) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(cps))
	for _, cp := range cps {
		parts = append(parts, fmt.Sprintf("%d:%s:%s:%s:%s",
			cp.PositionPct, f3(cp.SliceStartSec), f3(cp.SliceLenSec), f3(cp.OffsetMS), f4(cp.Quality)))
	}
	return strings.Join(parts, ";")
}

// emitRunMeta prints provenance for the run (protocol §3, §6.2). Values are
// single tokens; every one of them is needed to decide whether two runs are
// comparable at all.
func emitRunMeta(t *testing.T, rec *recorder, fixtureTarget, fixtureDonor string) {
	t.Helper()
	commit := strings.TrimSpace(os.Getenv("SYNC_MEASURE_COMMIT"))
	if commit == "" {
		commit = "unknown"
	}
	cachePolicy := strings.TrimSpace(os.Getenv("SYNC_MEASURE_CACHE_POLICY"))
	if cachePolicy == "" {
		cachePolicy = "unknown"
	}
	nproc := firstLine("nproc")
	if nproc == "" || nproc == "unknown" {
		nproc = strconv.Itoa(runtime.NumCPU())
	}
	mem := readStatusKV("/proc/meminfo")
	load1 := "unknown"
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		if fields := strings.Fields(string(raw)); len(fields) > 0 {
			load1 = sanitiseValue(fields[0])
		}
	}
	childSamples := 0
	if rec.childSamples {
		childSamples = 1
	}
	t.Logf("RUNMETA|commit=%s|go=%s|rustc=%s|ffmpeg=%s|mkvmerge=%s|ffprobe=%s|kernel=%s|nproc=%s|mem_total_mb=%d|mem_available_mb=%d|page_cached_mb=%d|clk_tck=%d|interval_s=%s|child_samples=%d|load1=%s|cache_policy=%s|fixture_target=%s|fixture_donor=%s",
		sanitiseValue(commit),
		sanitiseValue(runtime.Version()),
		toolVersion("rustc", 1, "--version"),
		toolVersion("ffmpeg", 2, "-version"),
		toolVersion("mkvmerge", 1, "--version"),
		toolVersion("ffprobe", 2, "-version"),
		firstLine("uname", "-r"),
		nproc,
		mem["MemTotal"]/1024,
		mem["MemAvailable"]/1024,
		mem["Cached"]/1024,
		clkTck(),
		f3(rec.interval.Seconds()),
		childSamples,
		load1,
		sanitiseValue(cachePolicy),
		sanitiseValue(filepath.Base(fixtureTarget)),
		sanitiseValue(filepath.Base(fixtureDonor)),
	)
}

// TestMeasureSyncResources runs the Shrek same-language fixture once through
// every sync-pipeline stage and prints one STEP| line per invocation plus the
// RUNMETA/PT/CHILD/DISK/SYNCOFFSET/PUBLISH/REPORT/SUMMARY/TOTAL lines.
func TestMeasureSyncResources(t *testing.T) {
	movieStart := time.Now() // entry to the test body: before fixture staging
	requireAcceptanceTools(t)
	h := newHarness(t)
	rec := newRecorder(t)
	rec.startParentSampler()
	defer rec.stopParentSampler()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	defer cancel()

	target := h.copyFixture(t, shrekTargetName, h.media)
	donor := h.copyFixture(t, shrekDonorName, h.donors)
	emitRunMeta(t, rec, target, donor)

	instanceID := h.insertInstance(t, "http://127.0.0.1:1", false)
	libraryID := h.insertLibraryRow(t, instanceID, "Shrek", 2001, "en", target)

	created, err := h.st.RecordImportCandidate(ctx, donor, filepath.Base(donor), fileSize(t, donor))
	if err != nil || !created {
		t.Fatalf("RecordImportCandidate = %v/%v", created, err)
	}
	h.approveImport(t, 1, libraryID, donor)

	// Extraction → vault (not part of the sync job; reported for completeness).
	// It has no Run seam, so its child CPU is the RUSAGE_CHILDREN delta (M7).
	ew, err := extract.New(extract.Dependencies{
		Store: h.st, Settings: h.ss, DataDir: h.data,
		VaultHook: h.vaultHook(t), Logf: h.logs.printf,
	})
	if err != nil {
		t.Fatalf("extract.New: %v", err)
	}
	start := time.Now()
	processed, err := ew.ProcessOnce(ctx)
	rec.record("extract_vault", time.Since(start), 0, 0, 0, err == nil && processed, "")
	if err != nil || !processed {
		t.Fatalf("extract.ProcessOnce = %v/%v", processed, err)
	}

	pol := h.trackByLang(t, "pl")
	eng := h.trackByLang(t, "en")
	recRow, err := h.st.CreateSyncJob(ctx, store.SyncJobCreate{
		VaultTrackID: pol.ID, TargetLibraryCacheID: libraryID,
		TargetVideoPath: target, TargetLangISO2: "pl",
	})
	if err != nil {
		t.Fatalf("CreateSyncJob: %v", err)
	}

	jobDir := filepath.Join(h.work, "jobs", strconv.FormatInt(recRow.ID, 10))

	// ---- Stage (Copy-on-Work) ----
	stager, err := workdir.New(h.work, h.st)
	if err != nil {
		t.Fatalf("workdir.New: %v", err)
	}
	start = time.Now()
	staging, err := stager.Stage(ctx, workdir.Job{
		ID: recRow.ID, TargetTrackID: pol.ID, ReferenceTrackIDs: []int64{eng.ID},
	})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	rec.record("stage_copy", time.Since(start), 0, 0, 0, true, "")

	// ---- Probe (ffprobe routed through the recorder seam) ----
	rec.setStep("probe_target")
	prober := probe.New(probe.Dependencies{Run: rec.runSeam})
	start = time.Now()
	info, err := prober.Probe(ctx, target)
	rec.record("probe_target", time.Since(start), 0, 0, 0, err == nil, "")
	rec.setStep("")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}

	// ---- Reference resolution (same as syncjob.analysisPolicy) ----
	policy := reflang.Languages{}
	if raw, ok, err := h.ss.Get(ctx, settings.KeyLanguagesPolicy); err == nil && ok {
		var lp settings.LanguagesPolicy
		if json.Unmarshal(raw, &lp) == nil {
			policy = reflang.Languages{
				Targets:                 append([]string(nil), lp.TargetImportLanguages...),
				References:              append([]string(nil), lp.ExtraReferenceLanguages...),
				PreferOriginalLanguage:  lp.PreferOriginalLanguage,
				OriginalLanguageFromArr: lp.OriginalLanguageFromArr,
			}
		}
	}
	cfg := analyze.DefaultConfig()
	if raw, ok, err := h.ss.Get(ctx, settings.KeySyncDefaults); err == nil && ok {
		var sd settings.SyncDefaults
		if json.Unmarshal(raw, &sd) == nil {
			if len(sd.CheckpointPositionsPct) > 0 {
				cfg.CheckpointPositionsPct = append([]int(nil), sd.CheckpointPositionsPct...)
			}
			if sd.CheckpointWindowSec > 0 {
				cfg.CheckpointWindowSec = sd.CheckpointWindowSec
			}
			if sd.MaxOffsetSec > 0 {
				cfg.MaxOffsetSec = sd.MaxOffsetSec
			}
		}
	}
	vaultTracks := []probe.AudioTrack{trackAudioTrack(store.VaultTrackRecord{
		ID: pol.ID, LangISO2: pol.LangISO2, ChannelCount: 2,
	}, 0)}
	polRec, ok, err := h.st.VaultTrackByID(ctx, pol.ID)
	if err != nil || !ok {
		t.Fatalf("VaultTrackByID(%d) = %v/%v", pol.ID, ok, err)
	}
	siblings, err := h.st.SiblingVaultTracks(ctx, polRec.VaultMediaID, polRec.GroupUUID, pol.ID)
	if err != nil {
		t.Fatalf("SiblingVaultTracks: %v", err)
	}
	for _, sib := range siblings {
		vaultTracks = append(vaultTracks, trackAudioTrack(sib, len(vaultTracks)))
	}
	resolved := reflang.Resolve(info.Tracks, vaultTracks, policy, "en")
	if len(resolved.Pairs) == 0 {
		t.Fatalf("no reference pair: %s", resolved.Reason)
	}
	pair := resolved.Pairs[0]
	donorPath := staging.Target.Path
	if pair.Vault.Index > 0 && pair.Vault.Index <= len(staging.References) {
		donorPath = staging.References[pair.Vault.Index-1].Path
	}

	// ---- Two ffmpeg decodes into the whole-movie WAVs ----
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatalf("ffmpeg: %v", err)
	}
	targetWAV := filepath.Join(jobDir, "target.wav")
	donorWAV := filepath.Join(jobDir, "donor.wav")
	filter := analysisFilter(pair.Method)
	stream := pair.Target.Index
	if err := rec.decodeAudio(ctx, "ffmpeg_decode_target", ffmpeg, target, stream, filter, targetWAV); err != nil {
		t.Fatalf("decode target: %v", err)
	}
	if err := rec.decodeAudio(ctx, "ffmpeg_decode_donor", ffmpeg, donorPath, 0, filter, donorWAV); err != nil {
		t.Fatalf("decode donor: %v", err)
	}

	engine, err := syncengine.New(syncBinary)
	if err != nil {
		t.Fatalf("sync.New: %v", err)
	}
	_ = engine

	// ---- Six engine spawns driven by the production analyzer ----
	req := analyze.Request{
		TargetPath:       targetWAV,
		DonorPath:        donorWAV,
		DurationSec:      0,
		TargetTracks:     info.Tracks,
		VaultTracks:      vaultTracks,
		Policy:           policy,
		OriginalLanguage: "en",
	}
	analyzeStart := time.Now()
	report, err := analyze.Analyze(ctx, &measuredEngine{bin: syncBinary, rec: rec}, cfg, req)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	rec.record("analyze_total", time.Since(analyzeStart), 0, 0, 0, true,
		fmt.Sprintf("offsets_ms=%v", report.OffsetsMS))

	t.Logf("REPORT|verdict=%s|classification=%s|max_drift_ms=%s|offsets_ms=%v|qualities=%v|checkpoints=%s",
		sanitiseValue(report.Verdict), sanitiseValue(report.Classification),
		f3(report.MaxDriftMS), report.OffsetsMS, report.Qualities,
		checkpointTriples(report.Checkpoints))

	// ---- Disk footprint of the staging directory before mux ----
	preMux := measureDir(t, jobDir)
	t.Logf("DISK|after_analysis|dir=%s|files=%d|bytes=%d|entries=%s",
		jobDir, preMux.files, preMux.bytes, strings.Join(preMux.names, ","))

	// ---- Mux ----
	raw, err := json.Marshal(settings.TargetTagFilter{
		RequiredFilenameTags: []string{"[ArcA310]"}, TagMatchMode: "ANY", EnforceTagFilter: true,
	})
	if err != nil {
		t.Fatalf("marshal tag filter: %v", err)
	}
	if err := h.ss.Set(ctx, settings.KeyTargetTagFilter, raw); err != nil {
		t.Fatalf("set tag filter: %v", err)
	}
	rule := gate.RuleFromTagFilter(settings.TargetTagFilter{
		RequiredFilenameTags: []string{"[ArcA310]"}, TagMatchMode: "ANY", EnforceTagFilter: true,
	})
	audio := filepath.Join(jobDir, "target-"+strconv.FormatInt(pol.ID, 10)+"-"+filepath.Base(pol.Path))

	// The only alignment control mkvmerge receives; §4.2 compares this integer.
	medianRaw := medianOffsetMS(report.OffsetsMS)
	syncOffsetMS := int64(math.Round(medianRaw))
	t.Logf("SYNCOFFSET|written_ms=%d|median_raw_ms=%s", syncOffsetMS, f3(medianRaw))

	// mkvmerge and mux's internal ffprobe validation both route through the
	// seam, so this step reports real cpu/maxrss instead of zero.
	rec.setStep("mux_mkvmerge")
	start = time.Now()
	res, err := mux.New(mux.Dependencies{Run: rec.runSeam}).Mux(ctx, mux.Job{
		ID:             recRow.ID,
		WorkDir:        h.work,
		TargetPath:     target,
		TargetLangISO2: "pl",
		AudioPath:      audio,
		SyncOffsetMS:   syncOffsetMS,
		TrackTitle:     "Polski Dubbing",
		OutputMode:     mux.OutputModeReplace,
		Gate:           rule,
		Report:         report,
	})
	muxEnd := time.Now()
	rec.record("mux_mkvmerge", muxEnd.Sub(start), 0, 0, 0, err == nil, "")
	rec.setStep("")
	if err != nil {
		t.Fatalf("mux: %v", err)
	}
	t.Logf("MUX|output=%s|workcopy=%s", res.OutputPath, res.WorkCopy)

	// ---- Publish validation (fresh prober: the pre-mux cache entry is stale) --
	afterTracks := 0
	durationSec := 0.0
	ffprobeOK := false
	if pubInfo, perr := probe.New(probe.Dependencies{}).Probe(ctx, res.OutputPath); perr == nil {
		ffprobeOK = true
		afterTracks = len(pubInfo.Tracks)
		durationSec = float64(pubInfo.DurationMS) / 1000
	}
	t.Logf("PUBLISH|%s|audio_tracks_before=%d|audio_tracks_after=%d|lang=%s|title=%s|duration_sec=%s|ffprobe_ok=%t",
		res.OutputPath, len(info.Tracks), afterTracks,
		sanitiseValue(res.Language), sanitiseValue(res.TrackTitle), f3(durationSec), ffprobeOK)

	// Optional preservation of the published file for the tester's Layer B
	// gold check: t.TempDir removes the test tree on return, so an env-gated
	// hardlink (same filesystem, zero copy) keeps the exact published bytes.
	// Unset in normal runs, so this never changes measured behaviour.
	if dir := strings.TrimSpace(os.Getenv("SYNC_MEASURE_PUBLISH_DIR")); dir != "" {
		dest := filepath.Join(dir, sanitiseValue(strings.TrimSpace(os.Getenv("SYNC_MEASURE_COMMIT")))+"_published.mkv")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Logf("PUBLISHCOPY|error=%s", sanitiseValue(err.Error()))
		} else if err := os.Link(res.OutputPath, dest); err != nil {
			t.Logf("PUBLISHCOPY|error=%s", sanitiseValue(err.Error()))
		} else {
			t.Logf("PUBLISHCOPY|path=%s", dest)
		}
	}
	movieEnd := time.Now()

	// On the branch (#23) Mux removes {WORK_DIR}/jobs/{id} itself, so the
	// post-mux footprint is legitimately absent there; measure only when the
	// directory survived (baseline/main keeps it until cleanup_staging).
	postMux := dirUsage{}
	if _, statErr := os.Stat(jobDir); statErr == nil {
		postMux = measureDir(t, jobDir)
		t.Logf("DISK|after_mux|dir=%s|files=%d|bytes=%d|entries=%s",
			jobDir, postMux.files, postMux.bytes, strings.Join(postMux.names, ","))
	} else {
		t.Logf("DISK|after_mux|dir=%s|files=0|bytes=0|entries=none_job_dir_removed_by_mux", jobDir)
	}

	// ---- Cleanup ----
	start = time.Now()
	err = stager.Cleanup(recRow.ID)
	rec.record("cleanup_staging", time.Since(start), 0, 0, 0, err == nil, "")
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, statErr := os.Stat(jobDir); statErr == nil {
		t.Logf("DISK|after_cleanup|dir=%s|still_present=true", jobDir)
	} else {
		t.Logf("DISK|after_cleanup|dir=%s|still_present=false", jobDir)
	}

	rec.stopParentSampler()

	// ---- Summary ----
	// sync_stage_total_wall_ms is the LEGACY parser-compatible field: it sums
	// every row except extract_vault/cleanup_staging, and because analyze_total
	// already contains the six engine_spawn_* walls it double-counts them
	// (~32 s on the baseline). Do not quote it as a whole-movie time — use
	// TOTAL|movie_wall_ms.
	var totalWall time.Duration
	var peak int64
	var peakStep string
	var childUserMS, childSysMS int64
	for _, row := range rec.rows {
		if row.Step == "extract_vault" || row.Step == "cleanup_staging" {
			continue
		}
		totalWall += row.Wall
		childUserMS += row.ChildUserMS
		childSysMS += row.ChildSysMS
		if row.MaxRSS > peak {
			peak, peakStep = row.MaxRSS, row.Step
		}
	}
	steps := len(rec.rows)
	t.Logf("SUMMARY|sync_stage_total_wall_ms=%d|peak_child_rss_kb=%d|peak_step=%s|job_dir_pre_mux_bytes=%d|job_dir_post_mux_bytes=%d|parent_peak_rss_kb=%d|parent_vmhwm_kb=%d|parent_cpu_user_ms=%d|parent_cpu_sys_ms=%d|child_cpu_user_ms=%d|child_cpu_sys_ms=%d|steps=%d",
		totalWall.Milliseconds(), peak, peakStep, preMux.bytes, postMux.bytes,
		rec.parentRSS, rec.parentHWm, rec.parentCPUUserMS, rec.parentCPUSysMS,
		childUserMS, childSysMS, steps)

	// One monotonic clock, no summation, no double count: the honest
	// end-to-end "process a movie" number (R2). job_wall_ms is the production
	// sync-job span (analysis start → publish end).
	t.Logf("TOTAL|movie_wall_ms=%d|job_wall_ms=%d|extract_included=true|cleanup_included=true",
		movieEnd.Sub(movieStart).Milliseconds(), muxEnd.Sub(analyzeStart).Milliseconds())
}
