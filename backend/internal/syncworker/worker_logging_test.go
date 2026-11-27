package syncworker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/logs"
	"github.com/Subject412/dubarr/backend/internal/mux"
)

// errEngineGone is the underlying cause of the failing job: errors.Is must
// still find it through the stage wrapper.
var errEngineGone = errors.New("engine failed")

// newRingLogger builds the real logger over a throwaway stderr buffer, so these
// tests read the ring exactly like internal/logs/logs_test.go does.
func newRingLogger(t *testing.T) *logs.Logger {
	t.Helper()
	var stderr bytes.Buffer
	lg, err := logs.New(logs.Options{Level: "info", Stderr: &stderr})
	if err != nil {
		t.Fatalf("logs.New: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	return lg
}

// newRingWorker is newWorkerLogging with both real seams: info claims and
// publishes go to Logf, a job failure goes to Errorf.
func newRingWorker(t *testing.T, r *rig, ex Executor, mx Muxer, rs Rescanner, lg *logs.Logger) *Worker {
	t.Helper()
	w, err := New(Dependencies{
		Store:     r.store,
		Settings:  r.settings,
		Executor:  ex,
		Muxer:     mx,
		Rescanner: rs,
		WorkDir:   r.workDir,
		Interval:  time.Millisecond,
		Logf:      lg.Logf("syncworker"),
		Errorf:    lg.Errorf("syncworker"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w
}

// funcExecutor is a per-job Executor: one job fails, the rest analyse cleanly.
type funcExecutor func(ctx context.Context, id int64) (analyze.Report, error)

func (f funcExecutor) Execute(ctx context.Context, id int64) (analyze.Report, error) {
	return f(ctx, id)
}

// workerEntriesAt returns the ring's "syncworker" lines at exactly one level.
func workerEntriesAt(lg *logs.Logger, level string) []logs.Entry {
	out := make([]logs.Entry, 0, 4)
	for _, e := range lg.Entries(logs.Query{Component: "syncworker"}) {
		if e.Level == level {
			out = append(out, e)
		}
	}
	return out
}

// TestFailedJobLogsErrorLineWithStageAndKeepsSentinel pins the U1.6 failure
// verdict: a failing job emits exactly one error line naming the job id and the
// stage it stopped at (so /logs?min_level=error answers "which job, where"), and
// the stage wrapper preserves errors.Is for the underlying cause — the mux
// sentinel is still reachable through it.
func TestFailedJobLogsErrorLineWithStageAndKeepsSentinel(t *testing.T) {
	r := newRig(t)
	failingID := r.newJob(t) // lowest id: ProcessOnce picks it first
	muxID := r.newJob(t)
	r.stage(t, muxID)

	ex := funcExecutor(func(_ context.Context, id int64) (analyze.Report, error) {
		if id == failingID {
			return analyze.Report{}, fmt.Errorf("engine exited 3: %w", errEngineGone)
		}
		return stableReport(), nil
	})
	mx := &fakeMuxer{err: fmt.Errorf("%w: target is not a Matroska container", mux.ErrUnsupportedContainer)}
	lg := newRingLogger(t)
	w := newRingWorker(t, r, ex, mx, &fakeRescanner{}, lg)
	if processed, err := w.ProcessOnce(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}

	// The claim stays info; the failure is the only error line.
	claims := workerEntriesAt(lg, "info")
	if len(claims) != 1 || !strings.Contains(claims[0].Message, fmt.Sprintf("job %d claimed", failingID)) {
		t.Errorf("info lines = %+v, want a single %q line", claims, fmt.Sprintf("job %d claimed", failingID))
	}
	errs := workerEntriesAt(lg, "error")
	if len(errs) != 1 {
		t.Fatalf("%d error line(s), want exactly 1: %+v", len(errs), errs)
	}
	msg := errs[0].Message
	if want := fmt.Sprintf("job %d failed at analyze", failingID); !strings.Contains(msg, want) {
		t.Errorf("error line = %q, want it to carry %q", msg, want)
	}
	if !strings.Contains(msg, errEngineGone.Error()) {
		t.Errorf("error line = %q, want the underlying cause %q", msg, errEngineGone.Error())
	}
	// A cancelled/absent stage must not be reported as an unknown failure.
	if strings.Contains(msg, "failed at unknown") {
		t.Errorf("error line lost the stage name: %q", msg)
	}

	// ProcessOnce logs a job error and swallows it (only a store failure is
	// returned), so errors.Is is observed on the chain processJob returns —
	// the same wrapper the logged line names. A real mux sentinel survives it.
	jobErr := w.processJob(context.Background(), muxID)
	if jobErr == nil {
		t.Fatal("processJob returned nil for a mux that failed")
	}
	if !errors.Is(jobErr, mux.ErrUnsupportedContainer) {
		t.Errorf("errors.Is(err, mux.ErrUnsupportedContainer) = false through the stage wrapper: %v", jobErr)
	}
	if stage := failureStage(jobErr); stage != "mux" {
		t.Errorf("failureStage = %q, want mux", stage)
	}
	if !strings.Contains(jobErr.Error(), "not a Matroska container") {
		t.Errorf("job error = %q, want the mux detail preserved", jobErr)
	}
}
