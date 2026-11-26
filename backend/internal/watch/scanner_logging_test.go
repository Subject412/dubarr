package watch

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/logs"
)

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

// watchEntriesAt returns the ring's "watch" lines at exactly one level.
func watchEntriesAt(lg *logs.Logger, level string) []logs.Entry {
	out := make([]logs.Entry, 0, 4)
	for _, e := range lg.Entries(logs.Query{Component: "watch"}) {
		if e.Level == level {
			out = append(out, e)
		}
	}
	return out
}

// TestScanLogsOneInfoLinePerScan pins the scan summary line: exactly one info
// line per pass, with the pass's own numbers (seen / added / skipped / errors),
// so two scans produce two lines and the counters come from the pass rather
// than from a constant.
//
// The clock is hand-driven, so the three passes are deterministic: the first
// only observes the files, the second finds them settled and queues both, the
// third finds them already queued.
func TestScanLogsOneInfoLinePerScan(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.mkv"))
	writeFile(t, filepath.Join(root, "b.mp4"))
	writeFile(t, filepath.Join(root, "notes.txt")) // not a media file: never seen

	st := newFakeStore()
	sc, err := New(st, []string{root}, &fakeProvider{interval: time.Second, window: 4 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	sc.now = clock.Now

	lg := newRingLogger(t)
	sc.SetLogging(lg.Logf("watch"), lg.Debugf("watch"))

	steps := []struct {
		advance  time.Duration
		want     string
		wantRows int
	}{
		{
			advance:  0,
			want:     "watch: scan finished: 2 file(s) seen, 0 candidate(s) added, 2 skipped, 0 error(s)",
			wantRows: 0,
		},
		{
			advance:  4 * time.Second, // exactly the stability window
			want:     "watch: scan finished: 2 file(s) seen, 2 candidate(s) added, 0 skipped, 0 error(s)",
			wantRows: 2,
		},
		{
			advance:  0, // both files are already queued: offered, not added
			want:     "watch: scan finished: 2 file(s) seen, 0 candidate(s) added, 2 skipped, 0 error(s)",
			wantRows: 2,
		},
	}

	for i, step := range steps {
		clock.Advance(step.advance)
		if _, err := sc.Scan(context.Background()); err != nil {
			t.Fatalf("scan %d: %v", i+1, err)
		}
		if got := len(st.paths()); got != step.wantRows {
			t.Fatalf("scan %d: %d queued candidate(s), want %d", i+1, got, step.wantRows)
		}

		info := watchEntriesAt(lg, "info")
		if len(info) != i+1 {
			t.Fatalf("after scan %d: %d info line(s), want exactly one per scan (%d): %+v",
				i+1, len(info), i+1, info)
		}
		if got := info[i].Message; got != step.want {
			t.Errorf("scan %d line = %q, want %q", i+1, got, step.want)
		}
	}
}

// TestScanDebugLinesFollowTheLevelSwitch pins both directions of the level
// switch on the per-path seam: at the info threshold a scan emits its one info
// line and no debug detail; after SetLevel("debug") the per-path lines appear
// without a restart; setting it back to info drops them again (the info line
// keeps coming), and a third switch to debug brings them back. A one-way test
// would pass with a logger that ignores the level.
func TestScanDebugLinesFollowTheLevelSwitch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "donor.mkv"))

	st := newFakeStore()
	sc, err := New(st, []string{root}, &fakeProvider{interval: time.Second, window: 4 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The clock never advances, so every pass re-reads the file as "not stable
	// yet": two per-path debug lines (scanned + skipped) per scan, every scan.
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	sc.now = clock.Now

	lg := newRingLogger(t) // boots at info
	sc.SetLogging(lg.Logf("watch"), lg.Debugf("watch"))

	scan := func(stage string, wantDebug, wantInfo int) {
		t.Helper()
		if _, err := sc.Scan(context.Background()); err != nil {
			t.Fatalf("%s: Scan: %v", stage, err)
		}
		debug := watchEntriesAt(lg, "debug")
		if len(debug) != wantDebug {
			t.Fatalf("%s: %d debug line(s), want %d: %+v", stage, len(debug), wantDebug, debug)
		}
		if info := watchEntriesAt(lg, "info"); len(info) != wantInfo {
			t.Fatalf("%s: %d info line(s), want %d (the scan summary is level-independent)",
				stage, len(info), wantInfo)
		}
	}

	// info: the per-path detail is dropped at the ring boundary.
	scan("at info", 0, 1)

	if err := lg.SetLevel("debug"); err != nil {
		t.Fatalf("SetLevel(debug): %v", err)
	}
	scan("after SetLevel(debug)", 2, 2)
	debug := watchEntriesAt(lg, "debug")
	for _, e := range debug {
		if !strings.Contains(e.Message, "donor.mkv") {
			t.Errorf("debug line does not name the path: %q", e.Message)
		}
	}
	if !strings.Contains(debug[0].Message, "watch: scanned ") {
		t.Errorf("first debug line = %q, want the per-path scan detail", debug[0].Message)
	}
	if !strings.Contains(debug[1].Message, "not stable for 4s yet") {
		t.Errorf("second debug line = %q, want the skip reason", debug[1].Message)
	}

	// Back to info: no new debug lines, the info line still appears.
	if err := lg.SetLevel("info"); err != nil {
		t.Fatalf("SetLevel(info): %v", err)
	}
	scan("back at info", 2, 3)

	// And on again: the switch is not one-way.
	if err := lg.SetLevel("debug"); err != nil {
		t.Fatalf("SetLevel(debug) again: %v", err)
	}
	scan("debug again", 4, 4)
}
