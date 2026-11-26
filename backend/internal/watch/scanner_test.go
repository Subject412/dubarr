package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// testEventTimeout bounds waiting for the scanner loop or a filesystem event in
// these tests. It is a hang guard, not a performance assertion: a loaded runner
// taking longer to deliver an inotify event is not a defect.
const testEventTimeout = 15 * time.Second

// fakeStore records candidates in memory and flags duplicates, mirroring the
// UNIQUE(source_file_path) constraint of the import_queue table.
type fakeStore struct {
	mu      sync.Mutex
	seen    map[string]bool
	order   []string
	failFor map[string]error
	block   chan struct{} // when non-nil, Scan blocks inside the first record
	entered chan struct{}
	once    sync.Once
}

func newFakeStore() *fakeStore {
	return &fakeStore{seen: map[string]bool{}, failFor: map[string]error{}}
}

func (f *fakeStore) RecordImportCandidate(_ context.Context, sourcePath, _ string, _ int64) (bool, error) {
	if f.block != nil {
		f.once.Do(func() {
			if f.entered != nil {
				close(f.entered)
			}
		})
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failFor[sourcePath]; ok {
		return false, err
	}
	if f.seen[sourcePath] {
		return false, nil
	}
	f.seen[sourcePath] = true
	f.order = append(f.order, sourcePath)
	return true, nil
}

func (f *fakeStore) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

// deleteRow removes a recorded candidate, mirroring a queue row that was
// deleted while its donor file stayed in the watch folder.
func (f *fakeStore) deleteRow(sourcePath string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.seen, sourcePath)
	for i, p := range f.order {
		if p == sourcePath {
			f.order = append(f.order[:i], f.order[i+1:]...)
			break
		}
	}
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte("media"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestScanRecordsNewMediaFilesOnly(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.mkv"))
	writeFile(t, filepath.Join(root, "sub", "b.MP4"))
	writeFile(t, filepath.Join(root, "sub", "c.m2ts"))
	writeFile(t, filepath.Join(root, "notes.txt"))
	writeFile(t, filepath.Join(root, "partial.mkv.part"))
	writeFile(t, filepath.Join(root, "temp.mkv.!qB"))

	st := newFakeStore()
	sc, err := New(st, []string{root}, testProvider(time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := settleScan(t, sc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.Seen != 3 {
		t.Errorf("Seen = %d, want 3 (media files only)", res.Seen)
	}
	if res.Added != 3 {
		t.Errorf("Added = %d, want 3", res.Added)
	}
	got := st.paths()
	if len(got) != 3 {
		t.Fatalf("recorded %d candidates, want 3: %v", len(got), got)
	}
	for _, p := range got {
		if !filepath.IsAbs(p) || strings.Contains(p, "..") {
			t.Errorf("candidate path %q is not canonical", p)
		}
	}

	// A second scan of the same tree must not enqueue duplicates.
	res, err = sc.Scan(context.Background())
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if res.Added != 0 {
		t.Errorf("second scan Added = %d, want 0", res.Added)
	}
	if len(st.paths()) != 3 {
		t.Errorf("store holds %d candidates after rescan, want 3", len(st.paths()))
	}
}

func TestScanDoesNotFollowSymlinkOutsideRoot(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "outside.mkv")
	writeFile(t, secret)

	root := t.TempDir()
	if err := os.Symlink(secret, filepath.Join(root, "link.mkv")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	writeFile(t, filepath.Join(root, "real.mkv"))

	st := newFakeStore()
	sc, err := New(st, []string{root}, testProvider(time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := settleScan(t, sc)
	// The escaping symlink is a per-file error, aggregated but non-fatal.
	if err == nil {
		t.Errorf("Scan: expected an error for the escaping symlink")
	}
	if len(res.Errors) == 0 {
		t.Errorf("expected a confinement error for the escaping symlink")
	}
	for _, p := range st.paths() {
		if strings.HasSuffix(p, "link.mkv") {
			t.Errorf("escaping symlink was recorded as candidate: %q", p)
		}
	}
	if len(st.paths()) != 1 {
		t.Errorf("recorded %d candidates, want 1 (real.mkv)", len(st.paths()))
	}
}

func TestNewCanonicalizesSymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	writeFile(t, filepath.Join(real, "movie.mkv"))

	link := filepath.Join(t.TempDir(), "rootlink")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	st := newFakeStore()
	sc, err := New(st, []string{link}, testProvider(time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := settleScan(t, sc); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	got := st.paths()
	if len(got) != 1 {
		t.Fatalf("recorded %d candidates, want 1", len(got))
	}
	resolvedReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if want := filepath.Join(resolvedReal, "movie.mkv"); got[0] != want {
		t.Errorf("candidate path = %q, want canonical %q", got[0], want)
	}
}

func TestNewRejectsInvalidRoots(t *testing.T) {
	if _, err := New(newFakeStore(), nil, testProvider(time.Second)); err == nil {
		t.Error("New with no roots: want error")
	}
	if _, err := New(newFakeStore(), []string{"relative/path"}, testProvider(time.Second)); err == nil {
		t.Error("New with relative root: want error")
	}
	if _, err := New(newFakeStore(), []string{filepath.Join(t.TempDir(), "missing")}, testProvider(time.Second)); err == nil {
		t.Error("New with missing root: want error")
	}
	if _, err := New(newFakeStore(), []string{t.TempDir()}, testProvider(-time.Second)); err == nil {
		t.Error("New with negative interval: want error")
	}
	// A non-positive window must never mean "gate off": both 0 and a negative
	// value fall back to the default instead of disabling the stability gate.
	for _, window := range []time.Duration{0, -time.Second} {
		sc, err := New(newFakeStore(), []string{t.TempDir()}, &fakeProvider{interval: time.Second, window: window})
		if err != nil {
			t.Fatalf("New with window %v: %v", window, err)
		}
		if got := sc.stabilityWindow(); got != DefaultStabilityWindow {
			t.Errorf("New with window %v: stability = %v, want %v", window, got, DefaultStabilityWindow)
		}
	}
	if _, err := New(newFakeStore(), []string{t.TempDir()}, nil); err == nil {
		t.Error("New without an options provider: want error")
	}
}

func TestNewDefaultsIntervalToTenSeconds(t *testing.T) {
	sc, err := New(newFakeStore(), []string{t.TempDir()}, testProvider(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if sc.Interval() != 10*time.Second {
		t.Errorf("Interval() = %v, want 10s", sc.Interval())
	}
}

// TestScanInvokesCandidateHook checks the additive post-record callback: it
// fires once per newly recorded candidate, not on rescans, and a hook error is
// aggregated without dropping the candidate.
func TestScanInvokesCandidateHook(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "tagged [ArcA310].mkv"))

	st := newFakeStore()
	sc, err := New(st, []string{root}, testProvider(time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var calls int
	var got string
	sc.SetCandidateHook(func(_ context.Context, sourcePath, _ string, _ int64) error {
		calls++
		got = sourcePath
		return nil
	})

	res, err := settleScan(t, sc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.Added != 1 || calls != 1 {
		t.Errorf("Added = %d, hook calls = %d, want 1 and 1", res.Added, calls)
	}
	if !strings.HasSuffix(got, "tagged [ArcA310].mkv") {
		t.Errorf("hook sourcePath = %q, want the recorded candidate", got)
	}

	// A rescan records nothing new, so the hook must not run again.
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if calls != 1 {
		t.Errorf("hook calls after rescan = %d, want 1", calls)
	}

	// A failing hook is surfaced as a per-file error but the row stays.
	writeFile(t, filepath.Join(root, "broken.mkv"))
	sc.SetCandidateHook(func(context.Context, string, string, int64) error {
		return errors.New("gate is sad")
	})
	res, err = settleScan(t, sc)
	if err == nil || len(res.Errors) != 1 {
		t.Errorf("Scan after hook failure: err=%v errors=%v, want one aggregated error", err, res.Errors)
	}
	if res.Added != 1 || len(st.paths()) != 2 {
		t.Errorf("candidate dropped by hook failure: Added=%d stored=%v", res.Added, st.paths())
	}
}

// TestScanSurvivesPanickingHook is the regression for the M2-wiring review
// finding: an unrecovered panic in the CandidateHook unwound Scan and killed
// the scan loop goroutine for good. A panicking hook must behave like any
// other per-file hook error — the pass completes, both files are recorded, and
// the panic is reported in Result.Errors.
func TestScanSurvivesPanickingHook(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "first.mkv"))
	writeFile(t, filepath.Join(root, "second.mkv"))

	st := newFakeStore()
	sc, err := New(st, []string{root}, testProvider(time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sc.SetCandidateHook(func(context.Context, string, string, int64) error {
		panic("gate exploded")
	})

	res, err := settleScan(t, sc)
	if err == nil {
		t.Fatal("Scan: expected the hook panic to surface as an error")
	}
	if len(st.paths()) != 2 {
		t.Errorf("recorded %d candidates, want both files: %v", len(st.paths()), st.paths())
	}
	if res.Seen != 2 || res.Added != 2 {
		t.Errorf("Seen/Added = %d/%d, want 2/2 (scan must complete)", res.Seen, res.Added)
	}
	if len(res.Errors) != 2 {
		t.Fatalf("Errors = %d, want one per file: %v", len(res.Errors), res.Errors)
	}
	found := false
	for _, e := range res.Errors {
		if strings.Contains(e.Error(), "watch: candidate hook panic: gate exploded") {
			found = true
		}
	}
	if !found {
		t.Errorf("Errors = %v, want a %q entry", res.Errors, "watch: candidate hook panic: gate exploded")
	}
}

func TestScanDebouncesOverlappingRuns(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "movie.mkv"))

	st := newFakeStore()
	st.block = make(chan struct{})
	st.entered = make(chan struct{})

	sc, err := New(st, []string{root}, testProvider(time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Prime the stability observation so the concurrent scan below reaches the
	// store: with testProvider's nanosecond window the first pass only records
	// the reading, the next one records the file (and blocks).
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("prime Scan: %v", err)
	}

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		if _, err := sc.Scan(context.Background()); err != nil {
			t.Errorf("first Scan: %v", err)
		}
	}()
	<-st.entered

	start := time.Now()
	if _, err := sc.Scan(context.Background()); !errors.Is(err, ErrScanInProgress) {
		t.Errorf("overlapping Scan error = %v, want ErrScanInProgress", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("overlapping Scan blocked for %v, want immediate skip", elapsed)
	}

	close(st.block)
	<-firstDone

	// The debounce flag must be released after the first scan finishes.
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Errorf("Scan after completion: %v", err)
	}
}

func TestScanContinuesAfterPerFileRecordError(t *testing.T) {
	root := t.TempDir()
	bad := filepath.Join(root, "bad.mkv")
	writeFile(t, bad)
	writeFile(t, filepath.Join(root, "good.mkv"))

	st := newFakeStore()
	st.failFor[bad] = errors.New("db is sad")
	sc, err := New(st, []string{root}, testProvider(time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := settleScan(t, sc)
	if err == nil {
		t.Fatal("Scan: expected aggregated error for the failing file")
	}
	if len(res.Errors) != 1 {
		t.Errorf("Errors = %d, want 1", len(res.Errors))
	}
	if len(st.paths()) != 1 || !strings.HasSuffix(st.paths()[0], "good.mkv") {
		t.Errorf("good.mkv was not recorded after bad.mkv failed: %v", st.paths())
	}
}

func TestRunScansImmediatelyAndStopsWithContext(t *testing.T) {
	root := t.TempDir()
	st := newFakeStore()
	sc, err := New(st, []string{root}, testProvider(5*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	sc.Start(ctx)

	writeFile(t, filepath.Join(root, "dropped.mkv"))
	deadline := time.Now().Add(2 * time.Second)
	for len(st.paths()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(st.paths()) != 1 {
		t.Fatalf("file dropped after startup was not picked up: %v", st.paths())
	}

	cancel()
	sc.Wait()

	time.Sleep(20 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines leaked: before=%d after=%d", before, after)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	sc, err := New(newFakeStore(), []string{t.TempDir()}, testProvider(time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sc.Start(context.Background())
	sc.Stop()
	sc.Stop()
	sc.Wait()
}

// TestScannerStopBeforeStartDoesNotDisarm checks the lifecycle regression:
// Stop before Start must be safe and must not consume the stop state, so a
// later Start (a no-op, since stopped is sticky) and Stop still terminate.
func TestScannerStopBeforeStartDoesNotDisarm(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dropped.mkv"))
	st := newFakeStore()

	sc, err := New(st, []string{root}, testProvider(5*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sc.Stop() // safe: nothing started yet

	sc.Start(context.Background())
	time.Sleep(10 * time.Millisecond)
	if got := st.paths(); len(got) != 0 {
		t.Errorf("Start after Stop launched a scan loop, recorded %v", got)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		sc.Stop()
	}()
	select {
	case <-done:
	case <-time.After(testEventTimeout):
		t.Fatal("Stop after Start did not terminate: later Stop was disarmed")
	}
}

// TestScannerConcurrentStartStop exercises concurrent Start/Stop calls under
// -race: no data race, no deadlock, every Stop returns.
func TestScannerConcurrentStartStop(t *testing.T) {
	for i := 0; i < 25; i++ {
		sc, err := New(newFakeStore(), []string{t.TempDir()}, testProvider(time.Millisecond))
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			sc.Start(context.Background())
		}()
		go func() {
			defer wg.Done()
			sc.Stop()
		}()

		finished := make(chan struct{})
		go func() {
			wg.Wait()
			close(finished)
		}()
		select {
		case <-finished:
		case <-time.After(testEventTimeout):
			t.Fatalf("iteration %d: concurrent Start/Stop did not terminate", i)
		}
		// A Stop after the dust settles must also return.
		sc.Stop()
	}
}

// fakeProvider is the injectable OptionsProvider. Tests set the cadence and
// window directly and can make a read fail to pin the keep-last-value rule.
type fakeProvider struct {
	mu          sync.Mutex
	interval    time.Duration
	window      time.Duration
	intervalErr error
}

// testProvider is the provider the pre-existing scanner tests use: a fixed
// cadence with the smallest stability window that still works. It is 1 ns, not
// 0, because a non-positive window is clamped to DefaultStabilityWindow (a
// zero window must never mean "gate off"). A 1 ns window still needs two
// passes — the first records the reading, the second finds it settled — which
// is what settleScan does.
func testProvider(interval time.Duration) *fakeProvider {
	return &fakeProvider{interval: interval, window: time.Nanosecond}
}

// settleScan runs the two passes a testProvider-backed scanner needs: the
// first records each file's size/mtime, the second finds the file settled and
// is the pass whose result the caller asserts. The first pass's errors are
// dropped deliberately — per-file errors (an escaping symlink, a failing hook)
// recur on the recording pass, which is the pass under test.
func settleScan(t *testing.T, sc *Scanner) (Result, error) {
	t.Helper()
	ctx := context.Background()
	if _, err := sc.Scan(ctx); err != nil {
		t.Logf("settleScan: observation pass reported %v (expected for per-file failures)", err)
	}
	return sc.Scan(ctx)
}

func (f *fakeProvider) ScanInterval() (time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.intervalErr != nil {
		return 0, f.intervalErr
	}
	return f.interval, nil
}

func (f *fakeProvider) StabilityWindow() (time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.window, nil
}

func (f *fakeProvider) setInterval(d time.Duration) {
	f.mu.Lock()
	f.interval = d
	f.mu.Unlock()
}

// fakeTicker is a hand-driven ticker: ch delivers a tick when a test sends one,
// and resets reports every cadence the loop asks for. No wall-clock period is
// ever waited on.
type fakeTicker struct {
	ch     chan time.Time
	resets chan time.Duration
}

func newFakeTicker() *fakeTicker {
	return &fakeTicker{ch: make(chan time.Time, 64), resets: make(chan time.Duration, 64)}
}

func (f *fakeTicker) Chan() <-chan time.Time { return f.ch }
func (f *fakeTicker) Reset(d time.Duration)  { f.resets <- d }
func (f *fakeTicker) Stop()                  {}

// startWithFakeTicker builds a scanner over an empty root with a hand-driven
// ticker and starts its loop.
func startWithFakeTicker(t *testing.T, provider *fakeProvider) (*Scanner, *fakeTicker) {
	t.Helper()
	sc, err := New(newFakeStore(), []string{t.TempDir()}, provider)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tr := newFakeTicker()
	sc.newTicker = func(time.Duration) scanTicker { return tr }
	sc.Start(context.Background())
	t.Cleanup(sc.Stop)
	return sc, tr
}

// TestRunRebuildsTickerWhenIntervalChanges pins the live-cadence fix: after
// the provider moves from 10 s to 2 s, the loop rebuilds its ticker to 2 s
// instead of holding the boot value until a restart.
func TestRunRebuildsTickerWhenIntervalChanges(t *testing.T) {
	provider := testProvider(10 * time.Second)
	_, tr := startWithFakeTicker(t, provider)

	provider.setInterval(2 * time.Second)
	tr.ch <- time.Now() // wake the loop so it re-reads the provider

	select {
	case got := <-tr.resets:
		if got != 2*time.Second {
			t.Fatalf("ticker rebuilt to %v, want 2s", got)
		}
	case <-time.After(testEventTimeout):
		t.Fatal("the loop did not rebuild its ticker after the interval changed")
	}
}

// TestIntervalReportsProviderChange checks the public accessor follows the
// live cadence, not the value captured at construction.
func TestIntervalReportsProviderChange(t *testing.T) {
	provider := testProvider(10 * time.Second)
	sc, tr := startWithFakeTicker(t, provider)

	provider.setInterval(3 * time.Second)
	tr.ch <- time.Now()

	select {
	case <-tr.resets:
	case <-time.After(testEventTimeout):
		t.Fatal("the loop did not apply the new interval")
	}
	if got := sc.Interval(); got != 3*time.Second {
		t.Errorf("Interval() = %v, want 3s", got)
	}
}

// TestRunKeepsIntervalOnProviderError pins the error path: a failed settings
// read keeps the last known cadence instead of stopping or reverting.
func TestRunKeepsIntervalOnProviderError(t *testing.T) {
	provider := testProvider(10 * time.Second)
	sc, tr := startWithFakeTicker(t, provider)

	provider.mu.Lock()
	provider.intervalErr = errors.New("settings store hiccup")
	provider.interval = 2 * time.Second // a later value that must not be applied
	provider.mu.Unlock()

	tr.ch <- time.Now()
	tr.ch <- time.Now()

	select {
	case got := <-tr.resets:
		t.Fatalf("ticker rebuilt to %v after a provider read error", got)
	case <-time.After(200 * time.Millisecond):
	}
	if got := sc.Interval(); got != 10*time.Second {
		t.Errorf("Interval() = %v after a read error, want the last known 10s", got)
	}
}

// fakeClock is a hand-advanced clock, so stability windows are tested without
// sleeping through them.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// stabilityScanner builds a scanner whose stability window comes from the
// provider and whose clock the test drives.
func stabilityScanner(t *testing.T, root string, window time.Duration) (*Scanner, *fakeStore, *fakeClock) {
	t.Helper()
	st := newFakeStore()
	sc, err := New(st, []string{root}, &fakeProvider{interval: time.Second, window: window})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	sc.now = clock.Now
	return sc, st, clock
}

// TestScanRecordsFileOnlyAfterStabilityWindow is the dead-setting regression:
// a candidate is enqueued only once its size and mtime have been unchanged for
// the window, and then exactly once.
func TestScanRecordsFileOnlyAfterStabilityWindow(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "stable.mkv"))
	sc, st, clock := stabilityScanner(t, root, 4*time.Second)
	ctx := context.Background()

	if _, err := sc.Scan(ctx); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if got := st.paths(); len(got) != 0 {
		t.Fatalf("recorded %v before the stability window elapsed", got)
	}

	clock.Advance(3 * time.Second)
	if _, err := sc.Scan(ctx); err != nil {
		t.Fatalf("Scan at 3s: %v", err)
	}
	if got := st.paths(); len(got) != 0 {
		t.Fatalf("recorded %v before the window elapsed", got)
	}

	clock.Advance(time.Second) // exactly the 4 s window
	res, err := sc.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan at 4s: %v", err)
	}
	if res.Added != 1 || len(st.paths()) != 1 {
		t.Fatalf("Added = %d, recorded %v, want the settled file once", res.Added, st.paths())
	}

	// A settled file must never be enqueued twice.
	clock.Advance(time.Minute)
	if res, err := sc.Scan(ctx); err != nil || res.Added != 0 {
		t.Fatalf("rescan: err=%v Added=%d, want 0 and no error", err, res.Added)
	}
	if got := st.paths(); len(got) != 1 {
		t.Fatalf("stability check recorded the file twice: %v", got)
	}
}

// TestScanDefersFileThatKeepsChanging covers the file-still-copying case: a
// size change between observations restarts the window, and the file is
// recorded exactly once after it settles.
func TestScanDefersFileThatKeepsChanging(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "copying.mkv")
	writeFile(t, path)
	sc, st, clock := stabilityScanner(t, root, 4*time.Second)
	ctx := context.Background()

	if _, err := sc.Scan(ctx); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	clock.Advance(5 * time.Second)
	if err := os.WriteFile(path, []byte("media-plus-more-bytes"), 0o644); err != nil {
		t.Fatalf("grow file: %v", err)
	}
	if _, err := sc.Scan(ctx); err != nil {
		t.Fatalf("Scan after growth: %v", err)
	}
	if got := st.paths(); len(got) != 0 {
		t.Fatalf("a file changing between observations was recorded: %v", got)
	}

	clock.Advance(3 * time.Second) // less than the window since the change
	if _, err := sc.Scan(ctx); err != nil {
		t.Fatalf("Scan at change+3s: %v", err)
	}
	if got := st.paths(); len(got) != 0 {
		t.Fatalf("recorded %v before the window since the last change", got)
	}

	clock.Advance(time.Second) // change+4s: now stable
	res, err := sc.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan at change+4s: %v", err)
	}
	if res.Added != 1 || len(st.paths()) != 1 {
		t.Fatalf("Added = %d, recorded %v, want the settled file once", res.Added, st.paths())
	}

	clock.Advance(time.Minute)
	if res, err := sc.Scan(ctx); err != nil || res.Added != 0 {
		t.Fatalf("rescan: err=%v Added=%d, want 0", err, res.Added)
	}
	if got := st.paths(); len(got) != 1 {
		t.Fatalf("recorded %d rows, want exactly one: %v", len(got), got)
	}
}

// TestStabilityWindowHonoursProvider distinguishes a 4 s window from an 8 s
// one: after 4 s of no change only the short window has recorded the file.
func TestStabilityWindowHonoursProvider(t *testing.T) {
	check := func(window time.Duration, recorded4s bool) {
		t.Helper()
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "donor.mkv"))
		sc, st, clock := stabilityScanner(t, root, window)
		ctx := context.Background()

		if _, err := sc.Scan(ctx); err != nil {
			t.Fatalf("window %v: first Scan: %v", window, err)
		}
		clock.Advance(4 * time.Second)
		if _, err := sc.Scan(ctx); err != nil {
			t.Fatalf("window %v: Scan at 4s: %v", window, err)
		}
		if got := len(st.paths()) == 1; got != recorded4s {
			t.Fatalf("window %v: recorded after 4s = %v, want %v", window, got, recorded4s)
		}

		clock.Advance(4 * time.Second) // 8 s since the first observation
		if _, err := sc.Scan(ctx); err != nil {
			t.Fatalf("window %v: Scan at 8s: %v", window, err)
		}
		if got := len(st.paths()); got != 1 {
			t.Fatalf("window %v: recorded %d candidates after 8s, want 1", window, got)
		}
	}

	check(4*time.Second, true)
	check(8*time.Second, false)
}

// TestScanPrunesObservationForVanishedFile keeps the in-memory observation map
// bounded: an entry disappears with its file, so a re-created file starts a
// fresh window instead of inheriting the old one.
func TestScanPrunesObservationForVanishedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "temporary.mkv")
	writeFile(t, path)
	sc, st, _ := stabilityScanner(t, root, 4*time.Second)
	ctx := context.Background()

	if _, err := sc.Scan(ctx); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove file: %v", err)
	}
	if _, err := sc.Scan(ctx); err != nil {
		t.Fatalf("Scan after removal: %v", err)
	}
	sc.obsMu.Lock()
	held := len(sc.observations)
	sc.obsMu.Unlock()
	if held != 0 {
		t.Fatalf("observations hold %d entries after the file vanished, want 0", held)
	}

	// A re-created file must not be recorded on the strength of the old entry.
	writeFile(t, path)
	if _, err := sc.Scan(ctx); err != nil {
		t.Fatalf("Scan after re-create: %v", err)
	}
	if got := st.paths(); len(got) != 0 {
		t.Fatalf("re-created file recorded before the window elapsed: %v", got)
	}
}

// TestScanReoffersSettledFileAfterQueueRowDeleted is the stranded-file
// regression: the scanner keeps no "already recorded" latch, so a settled file
// whose queue row was deleted is offered to the store again on the next pass
// and ends as exactly one row — not zero (stranded) and not two (duplicated).
func TestScanReoffersSettledFileAfterQueueRowDeleted(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "stranded.mkv"))
	sc, st, clock := stabilityScanner(t, root, 4*time.Second)
	ctx := context.Background()

	scan := func(stage string) Result {
		t.Helper()
		res, err := sc.Scan(ctx)
		if err != nil {
			t.Fatalf("%s: Scan: %v", stage, err)
		}
		return res
	}

	scan("observe")
	clock.Advance(4 * time.Second)
	if res := scan("settled"); res.Added != 1 || len(st.paths()) != 1 {
		t.Fatalf("Added = %d, rows = %v, want the settled file queued exactly once", res.Added, st.paths())
	}

	// The queue row is deleted but the file stays in the watch folder.
	st.deleteRow(st.paths()[0])
	if got := st.paths(); len(got) != 0 {
		t.Fatalf("row not deleted, store still holds %v", got)
	}

	clock.Advance(time.Minute)
	if res := scan("after row deletion"); res.Added != 1 {
		t.Fatalf("Added = %d after the queue row was deleted, want 1 (the file must be re-offered)", res.Added)
	}
	if got := st.paths(); len(got) != 1 {
		t.Fatalf("queue holds %d rows after the re-offer, want exactly 1: %v", len(got), got)
	}

	// The next pass still offers the settled file, and the store's dedupe turns
	// that into nothing: the single row is left alone.
	clock.Advance(time.Minute)
	if res := scan("after re-offer"); res.Added != 0 || len(st.paths()) != 1 {
		t.Fatalf("Added = %d, rows = %v, want the existing row left alone", res.Added, st.paths())
	}
}
