package analyze

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/sync"
)

// vmHWM returns this process's peak resident set size in bytes, i.e. the
// kernel's VmHWM high-water mark.
func vmHWM() (uint64, bool) { return procFieldBytes("/proc/self/status", "VmHWM") }

// BenchmarkParallelCheckpoints measures a whole six-checkpoint analysis at two
// worker counts: the elapsed time, and how many resident bytes one worker adds
// (the VmHWM delta over the run, divided by the worker count).
//
// VmHWM only ever rises, so a process that has already run n=1 cannot measure
// n=4: each worker count needs its own process.
//
//	go test ./internal/analyze -run '^$' -bench 'ParallelCheckpoints/n=1$' -benchtime 1x -v
//	go test ./internal/analyze -run '^$' -bench 'ParallelCheckpoints/n=4$' -benchtime 1x -v
//
// [CheckpointFixedBytes] and [CheckpointBytesPerWorker] are derived from these
// numbers (two terms, so that both n=1 and n=4 are described), and
// internal/dsp/PERF.md records the run they were taken on. The figure includes
// whatever the collector let the heap reach (GOGC's default doubling), because
// that is what the container's memory budget actually pays.
func BenchmarkParallelCheckpoints(b *testing.B) {
	const (
		rate = 8000
		dur  = 600
		lead = 1200 // samples; ~150 ms at 8 kHz
	)
	dir := b.TempDir()
	target := noiseSignal(dur*rate, 23)
	writeWAVRate(b, filepath.Join(dir, "target.wav"), target, rate)
	writeWAVRate(b, filepath.Join(dir, "donor.wav"), shiftedDonor(target, lead), rate)
	target = nil
	runtime.GC()

	req := request(dir)
	req.DurationSec = dur
	eng := sync.New(sync.WithTimeout(testJobTimeout))
	ctx := context.Background()

	for _, workers := range []int{1, 4} {
		b.Run(fmt.Sprintf("n=%d", workers), func(b *testing.B) {
			cfg := DefaultConfig()
			cfg.JobTimeout = testJobTimeout
			cfg.ParallelCheckpoints = workers
			// The benchmark measures what N workers cost, not what this host
			// happens to allow: grant the seam a budget that cannot reduce N,
			// so the numbers stay comparable between machines and runs.
			prev := memAvailable
			memAvailable = func() (uint64, bool) {
				return 2 * checkpointBytes(MaxParallelCheckpoints), true
			}
			defer func() { memAvailable = prev }()
			if got := parallelWorkers(cfg); got != workers {
				b.Fatalf("worker count %d was clamped to %d", workers, got)
			}

			runtime.GC()
			base, ok := vmHWM()
			if !ok {
				b.Fatal("/proc/self/status has no VmHWM")
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := Analyze(ctx, eng, cfg, req); err != nil {
					b.Fatalf("Analyze: %v", err)
				}
			}
			b.StopTimer()
			peak, ok := vmHWM()
			if !ok {
				b.Fatal("/proc/self/status has no VmHWM")
			}
			if peak <= base {
				b.Fatalf("VmHWM did not rise (base %d): was this worker count already measured in this process?", base)
			}
			b.Logf("VmHWM base %.1f MiB, peak %.1f MiB, delta %.1f MiB",
				float64(base)/(1<<20), float64(peak)/(1<<20), float64(peak-base)/(1<<20))
			b.ReportMetric(float64(peak-base)/float64(workers*b.N), "B/worker")
			b.ReportMetric(float64(peak-base)/float64(b.N), "B/job")
		})
	}
}
