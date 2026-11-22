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
	sc, err := New(st, []string{root}, time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := sc.Scan(context.Background())
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
	sc, err := New(st, []string{root}, time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := sc.Scan(context.Background())
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
	sc, err := New(st, []string{link}, time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := sc.Scan(context.Background()); err != nil {
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
	if _, err := New(newFakeStore(), nil, time.Second); err == nil {
		t.Error("New with no roots: want error")
	}
	if _, err := New(newFakeStore(), []string{"relative/path"}, time.Second); err == nil {
		t.Error("New with relative root: want error")
	}
	if _, err := New(newFakeStore(), []string{filepath.Join(t.TempDir(), "missing")}, time.Second); err == nil {
		t.Error("New with missing root: want error")
	}
	if _, err := New(newFakeStore(), []string{t.TempDir()}, -time.Second); err == nil {
		t.Error("New with negative interval: want error")
	}
}

func TestNewDefaultsIntervalToTenSeconds(t *testing.T) {
	sc, err := New(newFakeStore(), []string{t.TempDir()}, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if sc.Interval() != 10*time.Second {
		t.Errorf("Interval() = %v, want 10s", sc.Interval())
	}
}

func TestScanDebouncesOverlappingRuns(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "movie.mkv"))

	st := newFakeStore()
	st.block = make(chan struct{})
	st.entered = make(chan struct{})

	sc, err := New(st, []string{root}, time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
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
	sc, err := New(st, []string{root}, time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := sc.Scan(context.Background())
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
	sc, err := New(st, []string{root}, 5*time.Millisecond)
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
	sc, err := New(newFakeStore(), []string{t.TempDir()}, time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sc.Start(context.Background())
	sc.Stop()
	sc.Stop()
	sc.Wait()
}
