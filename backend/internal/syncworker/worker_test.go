package syncworker

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/arrrescan"
	"github.com/Subject412/dubarr/backend/internal/mux"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
	"github.com/Subject412/dubarr/backend/internal/transcode"
)

type fakeExecutor struct {
	report analyze.Report
	err    error
	panic  any
	calls  []int64
}

func (f *fakeExecutor) Execute(_ context.Context, id int64) (analyze.Report, error) {
	f.calls = append(f.calls, id)
	if f.panic != nil {
		panic(f.panic)
	}
	return f.report, f.err
}

type fakeMuxer struct {
	result mux.Result
	err    error
	jobs   []mux.Job
	// after runs once the publish returns, simulating a seam that observes a
	// shutdown cancellation between mux and the terminal status write.
	after func()
}

func (f *fakeMuxer) Mux(_ context.Context, job mux.Job) (mux.Result, error) {
	f.jobs = append(f.jobs, job)
	if f.after != nil {
		f.after()
	}
	return f.result, f.err
}

type fakeRescanner struct {
	err  error
	rows []arrrescan.LibraryCacheRow
}

func (f *fakeRescanner) RescanAfterMux(_ context.Context, row arrrescan.LibraryCacheRow) error {
	f.rows = append(f.rows, row)
	return f.err
}

type rig struct {
	store    *store.Store
	settings *settings.Store
	workDir  string
	target   string
	libID    int64
	trackID  int64
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ss := settings.New(st.DB)
	if err := ss.Seed(context.Background(), filepath.Join(dir, "data")); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	raw, err := json.Marshal(settings.StoragePaths{WorkDir: work, VaultDir: filepath.Join(dir, "vault")})
	if err != nil {
		t.Fatalf("marshal storage: %v", err)
	}
	if err := ss.Set(context.Background(), settings.KeyStoragePaths, raw); err != nil {
		t.Fatalf("set storage: %v", err)
	}

	media := filepath.Join(dir, "library")
	if err := os.MkdirAll(media, 0o700); err != nil {
		t.Fatalf("mkdir library: %v", err)
	}
	target := filepath.Join(media, "Movie (2020) [ArcA310].mkv")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	trackPath := filepath.Join(media, "track.pl.mka")
	if err := os.WriteFile(trackPath, []byte("mka"), 0o600); err != nil {
		t.Fatalf("write track: %v", err)
	}

	mediaID, err := st.EnsureVaultMedia(context.Background(), store.VaultMediaRecord{
		MediaType: "movie", Title: "Movie", Year: 2020, FolderPath: media,
	})
	if err != nil {
		t.Fatalf("EnsureVaultMedia: %v", err)
	}
	trackID := insertTrack(t, st.DB, mediaID, trackPath)

	res, err := st.DB.Exec(`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		VALUES ('test', 'radarr', 'http://localhost', X'00', '0000')`)
	if err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	instanceID, _ := res.LastInsertId()
	res, err = st.DB.Exec(`INSERT INTO arr_library_cache
		(arr_instance_id, media_type, external_item_id, title, original_language_iso2,
		 target_file_path, eligibility_status, matches_target_tags, audio_languages_json)
		VALUES (?, 'movie', 7, 'Movie', 'eng', ?, 'eligible', 1, '["eng"]')`,
		instanceID, target)
	if err != nil {
		t.Fatalf("insert library: %v", err)
	}
	libID, _ := res.LastInsertId()

	return &rig{store: st, settings: ss, workDir: work, target: target, libID: libID, trackID: trackID}
}

func insertTrack(t *testing.T, db *sql.DB, mediaID int64, path string) int64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read track: %v", err)
	}
	sum := sha256.Sum256(data)
	res, err := db.Exec(`INSERT INTO audio_tracks
		(vault_media_id, group_uuid, lang_iso2, lang_iso3, variant, file_path, sha256,
		 codec, channels, channel_count)
		VALUES (?, 'group-1', 'pl', 'pol', 'dubbing', ?, ?, 'pcm_s16le', '2.0', 2)`,
		mediaID, path, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("insert track: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (r *rig) newJob(t *testing.T) int64 {
	t.Helper()
	rec, err := r.store.CreateSyncJob(context.Background(), store.SyncJobCreate{
		VaultTrackID:         r.trackID,
		TargetLibraryCacheID: r.libID,
		TargetVideoPath:      r.target,
		TargetLangISO2:       "pl",
	})
	if err != nil {
		t.Fatalf("CreateSyncJob: %v", err)
	}
	return rec.ID
}

// stage makes the per-job directory look like a successful Execute left it.
func (r *rig) stage(t *testing.T, jobID int64) string {
	t.Helper()
	dir := filepath.Join(r.workDir, "jobs", strconv.FormatInt(jobID, 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("target-%d-track.pl.mka", r.trackID))
	if err := os.WriteFile(path, []byte("mka"), 0o600); err != nil {
		t.Fatalf("write staged audio: %v", err)
	}
	return path
}

func newWorker(t *testing.T, r *rig, ex Executor, mx Muxer, rs Rescanner) *Worker {
	t.Helper()
	return newWorkerLogging(t, r, ex, mx, rs, t.Logf)
}

// newWorkerLogging is newWorker with a caller-supplied log sink, so a test can
// assert on the exact lines one job produces.
func newWorkerLogging(t *testing.T, r *rig, ex Executor, mx Muxer, rs Rescanner, logf func(string, ...any)) *Worker {
	t.Helper()
	w, err := New(Dependencies{
		Store:     r.store,
		Settings:  r.settings,
		Executor:  ex,
		Muxer:     mx,
		Rescanner: rs,
		WorkDir:   r.workDir,
		Interval:  time.Millisecond,
		Logf:      logf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w
}

func stableReport() analyze.Report {
	return analyze.Report{
		Verdict:        analyze.VerdictOK,
		Classification: analyze.ClassificationStable,
		SyncMethod:     "same_lang_reference",
		OffsetsMS:      []float64{1200, 1210},
	}
}

func TestProcessOnceRunsExecuteMuxRescanChain(t *testing.T) {
	r := newRig(t)
	jobID := r.newJob(t)
	audio := r.stage(t, jobID)

	ex := &fakeExecutor{report: stableReport()}
	mx := &fakeMuxer{}
	rs := &fakeRescanner{}
	w := newWorker(t, r, ex, mx, rs)

	processed, err := w.ProcessOnce(context.Background())
	if err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if !processed {
		t.Fatal("ProcessOnce processed = false, want true")
	}
	if len(ex.calls) != 1 || ex.calls[0] != jobID {
		t.Fatalf("Execute calls = %v, want [%d]", ex.calls, jobID)
	}
	if len(mx.jobs) != 1 {
		t.Fatalf("Mux calls = %d, want 1", len(mx.jobs))
	}
	got := mx.jobs[0]
	if got.ID != jobID || got.WorkDir != r.workDir || got.TargetPath != r.target {
		t.Fatalf("mux job identity = %+v", got)
	}
	if got.TargetLangISO2 != "pl" {
		t.Fatalf("mux language = %q, want pl", got.TargetLangISO2)
	}
	if got.AudioPath != audio {
		t.Fatalf("mux audio = %q, want %q", got.AudioPath, audio)
	}
	if got.SyncOffsetMS != 1205 {
		t.Fatalf("mux offset = %d, want median 1205", got.SyncOffsetMS)
	}
	if got.Report.Verdict != analyze.VerdictOK {
		t.Fatalf("mux report verdict = %q, want ok", got.Report.Verdict)
	}
	if len(rs.rows) != 1 || rs.rows[0].ExternalItemID != 7 || rs.rows[0].MediaType != "movie" {
		t.Fatalf("rescan rows = %+v", rs.rows)
	}

	rec, _, err := r.store.SyncJobByID(context.Background(), jobID)
	if err != nil {
		t.Fatalf("SyncJobByID: %v", err)
	}
	if rec.Status != "completed" {
		t.Fatalf("status = %q, want completed", rec.Status)
	}
	// Issue #23: a completed job leaves no staging behind.
	if _, err := os.Stat(filepath.Dir(audio)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging should be removed after a completed job, stat err = %v", err)
	}
}

// TestProcessOnceToleratesRetiredKeepBakRow: a mux.options row stored before
// keep_bak was retired must still feed the muxer — the deleted key is dropped
// on read rather than failing the decode — and the job carries the surviving
// options. This replaces the old keep_bak -> mux.Job plumbing test.
func TestProcessOnceToleratesRetiredKeepBakRow(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	legacy := json.RawMessage(`{"keep_bak":true,"track_title_in_mkv":"Polski Dubbing"}`)
	if _, err := r.store.DB.Exec(
		`UPDATE settings SET value = ? WHERE key = ?`, string(legacy), settings.KeyMuxOptions,
	); err != nil {
		t.Fatalf("write legacy mux.options row: %v", err)
	}
	jobID := r.newJob(t)
	r.stage(t, jobID)

	mx := &fakeMuxer{}
	w := newWorker(t, r, &fakeExecutor{report: stableReport()}, mx, &fakeRescanner{})
	if _, err := w.ProcessOnce(ctx); err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if len(mx.jobs) != 1 {
		t.Fatalf("Mux calls = %d, want 1", len(mx.jobs))
	}
	if got := mx.jobs[0].TrackTitle; got != "Polski Dubbing" {
		t.Errorf("mux track title = %q, want the stored title", got)
	}
	rec, ok, err := r.store.SyncJobByID(ctx, jobID)
	if err != nil || !ok {
		t.Fatalf("SyncJobByID = %v/%v", ok, err)
	}
	if rec.Status != "completed" {
		t.Errorf("status = %q, want completed", rec.Status)
	}
}

func TestProcessOnceFailsUnmuxableVerdictWithoutMux(t *testing.T) {
	r := newRig(t)
	jobID := r.newJob(t)
	audio := r.stage(t, jobID)

	report := stableReport()
	report.Verdict = analyze.VerdictDrifting
	report.Classification = analyze.ClassificationDrifting
	mx := &fakeMuxer{}
	w := newWorker(t, r, &fakeExecutor{report: report}, mx, &fakeRescanner{})

	processed, err := w.ProcessOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	if len(mx.jobs) != 0 {
		t.Fatalf("Mux called %d times for a drifting verdict, want 0", len(mx.jobs))
	}
	rec, _, _ := r.store.SyncJobByID(context.Background(), jobID)
	if rec.Status != "failed" || !strings.Contains(rec.ErrorMessage, "drifting") {
		t.Fatalf("status/message = %q/%q, want failed mentioning drifting", rec.Status, rec.ErrorMessage)
	}
	if _, err := os.Stat(filepath.Dir(audio)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging should be cleaned on failure, stat err = %v", err)
	}
}

func TestProcessOnceMuxFailureFailsJobAndCleansStaging(t *testing.T) {
	r := newRig(t)
	jobID := r.newJob(t)
	audio := r.stage(t, jobID)
	w := newWorker(t, r, &fakeExecutor{report: stableReport()},
		&fakeMuxer{err: errors.New("mux: mkvmerge failed\nsecond line")}, &fakeRescanner{})

	processed, err := w.ProcessOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	rec, _, _ := r.store.SyncJobByID(context.Background(), jobID)
	if rec.Status != "failed" || rec.ErrorMessage != "mux: mkvmerge failed" {
		t.Fatalf("status/message = %q/%q, want failed/first line", rec.Status, rec.ErrorMessage)
	}
	if _, err := os.Stat(filepath.Dir(audio)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging should be cleaned on failure, stat err = %v", err)
	}
}

func TestProcessOncePanicIsRecordedAsJobFailure(t *testing.T) {
	r := newRig(t)
	jobID := r.newJob(t)
	audio := r.stage(t, jobID)
	w := newWorker(t, r, &fakeExecutor{panic: "boom"}, &fakeMuxer{}, &fakeRescanner{})

	processed, err := w.ProcessOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	rec, _, _ := r.store.SyncJobByID(context.Background(), jobID)
	if rec.Status != "failed" || !strings.Contains(rec.ErrorMessage, "panic") {
		t.Fatalf("status/message = %q/%q, want failed mentioning panic", rec.Status, rec.ErrorMessage)
	}
	if _, err := os.Stat(filepath.Dir(audio)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging should be cleaned after a panic, stat err = %v", err)
	}
}

func TestMuxableVerdict(t *testing.T) {
	for _, verdict := range []string{analyze.VerdictOK, analyze.ClassificationStable} {
		if !muxableVerdict(verdict) {
			t.Errorf("muxableVerdict(%q) = false, want true", verdict)
		}
	}
	for _, verdict := range []string{analyze.VerdictDrifting, analyze.VerdictUnreliable, analyze.VerdictOutOfRange, ""} {
		if muxableVerdict(verdict) {
			t.Errorf("muxableVerdict(%q) = true, want false", verdict)
		}
	}
}

func TestProcessOnceExecuteErrorMarksFailed(t *testing.T) {
	r := newRig(t)
	jobID := r.newJob(t)
	w := newWorker(t, r, &fakeExecutor{err: errors.New("engine missing\nstack")}, &fakeMuxer{}, &fakeRescanner{})

	if processed, err := w.ProcessOnce(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	rec, _, _ := r.store.SyncJobByID(context.Background(), jobID)
	if rec.Status != "failed" || rec.ErrorMessage != "engine missing" {
		t.Fatalf("status/message = %q/%q", rec.Status, rec.ErrorMessage)
	}
}

// M5-4 contract: an arr rescan is best-effort and must not undo a good mux.
func TestProcessOnceRescanFailureKeepsJobDone(t *testing.T) {
	r := newRig(t)
	jobID := r.newJob(t)
	r.stage(t, jobID)
	w := newWorker(t, r, &fakeExecutor{report: stableReport()},
		&fakeMuxer{}, &fakeRescanner{err: errors.New("arrrescan: connection refused")})

	if processed, err := w.ProcessOnce(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	rec, _, _ := r.store.SyncJobByID(context.Background(), jobID)
	if rec.Status != "completed" {
		t.Fatalf("status = %q, want completed despite rescan failure", rec.Status)
	}
}

// A shutdown cancellation observed right after the mux returns must not turn an
// already-published job into a failure: the terminal status write is detached
// from the drain context. Staging is still removed for the finished job
// (issue #23).
func TestProcessOncePublishedJobSurvivesShutdownCancellation(t *testing.T) {
	r := newRig(t)
	jobID := r.newJob(t)
	audio := r.stage(t, jobID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel as soon as the publish returns, mimicking a SIGTERM that lands in
	// the rescan window before the terminal write.
	mx := &fakeMuxer{after: cancel}
	w := newWorker(t, r, &fakeExecutor{report: stableReport()}, mx, &fakeRescanner{})

	processed, err := w.ProcessOnce(ctx)
	if err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	if len(mx.jobs) != 1 {
		t.Fatalf("Mux calls = %d, want 1", len(mx.jobs))
	}
	rec, _, err := r.store.SyncJobByID(context.Background(), jobID)
	if err != nil {
		t.Fatalf("SyncJobByID: %v", err)
	}
	if rec.Status != "completed" {
		t.Fatalf("status = %q, want completed after a published mux", rec.Status)
	}
	if _, err := os.Stat(filepath.Dir(audio)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging should be removed after a published job, stat err = %v", err)
	}
}

// Issue #23: staging left by an interrupted run must not survive boot. Start
// reaps it before the drain loop runs, mirroring the extraction worker.
func TestStartReapsStaleSyncStaging(t *testing.T) {
	r := newRig(t)
	stale := filepath.Join(r.workDir, "jobs", "999")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("mkdir stale staging: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stale, "target.wav"), []byte("wav"), 0o600); err != nil {
		t.Fatalf("write stale file: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("age stale staging: %v", err)
	}

	w := newWorker(t, r, &fakeExecutor{}, &fakeMuxer{}, &fakeRescanner{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	w.Stop()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale sync staging not reaped (stat err = %v)", err)
	}
}

func TestNewRejectsIncompleteDependencies(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ok := Dependencies{
		Store: st, Executor: &fakeExecutor{}, Muxer: &fakeMuxer{},
		Rescanner: &fakeRescanner{}, WorkDir: "/tmp/work",
	}
	cases := []struct {
		name string
		mut  func(*Dependencies)
		want error
	}{
		{"no work dir", func(d *Dependencies) { d.WorkDir = " " }, ErrNoWorkDir},
		{"no store", func(d *Dependencies) { d.Store = nil }, ErrNoStore},
		{"no executor", func(d *Dependencies) { d.Executor = nil }, ErrNoExecutor},
		{"no muxer", func(d *Dependencies) { d.Muxer = nil }, ErrNoMuxer},
		{"no rescanner", func(d *Dependencies) { d.Rescanner = nil }, ErrNoRescanner},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := ok
			tc.mut(&d)
			if _, err := New(d); !errors.Is(err, tc.want) {
				t.Fatalf("New err = %v, want %v", err, tc.want)
			}
		})
	}
}

func setStoragePaths(t *testing.T, s *settings.Store, paths settings.StoragePaths) {
	t.Helper()
	raw, err := json.Marshal(paths)
	if err != nil {
		t.Fatalf("marshal storage.paths: %v", err)
	}
	if err := s.Set(context.Background(), settings.KeyStoragePaths, raw); err != nil {
		t.Fatalf("set storage.paths: %v", err)
	}
}

func setMuxOptions(t *testing.T, s *settings.Store, opts settings.MuxOptions) {
	t.Helper()
	raw, err := json.Marshal(opts)
	if err != nil {
		t.Fatalf("marshal mux.options: %v", err)
	}
	if err := s.Set(context.Background(), settings.KeyMuxOptions, raw); err != nil {
		t.Fatalf("set mux.options: %v", err)
	}
}

// TestProcessOnceSeparateModeSkipsRescanAndRecordsOutputPath covers ADR-0011:
// the worker resolves storage.paths.output_dir, passes it to the muxer, skips
// the arr rescan (the library file is untouched) and stores the final path on
// the job so GET /api/sync/jobs can show it.
func TestProcessOnceSeparateModeSkipsRescanAndRecordsOutputPath(t *testing.T) {
	r := newRig(t)
	outDir := filepath.Join(t.TempDir(), "output")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}
	resolvedOut, err := filepath.EvalSymlinks(outDir)
	if err != nil {
		t.Fatalf("resolve output: %v", err)
	}
	vaultDir := filepath.Join(filepath.Dir(outDir), "vault")
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		t.Fatalf("mkdir vault: %v", err)
	}
	setStoragePaths(t, r.settings, settings.StoragePaths{
		WorkDir: r.workDir, VaultDir: vaultDir, OutputDir: outDir,
	})
	setMuxOptions(t, r.settings, settings.MuxOptions{
		TrackTitleInMKV: "Polski Dubbing", OutputMode: settings.MuxOutputSeparate,
	})

	jobID := r.newJob(t)
	r.stage(t, jobID)
	dest := filepath.Join(resolvedOut, "Movie (2020) [ArcA310].dubarr.mkv")
	mx := &fakeMuxer{result: mux.Result{OutputPath: dest}}
	rs := &fakeRescanner{}
	w := newWorker(t, r, &fakeExecutor{report: stableReport()}, mx, rs)

	if processed, err := w.ProcessOnce(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	if len(mx.jobs) != 1 {
		t.Fatalf("Mux calls = %d, want 1", len(mx.jobs))
	}
	if got := mx.jobs[0].OutputMode; got != settings.MuxOutputSeparate {
		t.Errorf("mux output mode = %q, want separate", got)
	}
	if got := mx.jobs[0].OutputDir; got != resolvedOut {
		t.Errorf("mux output dir = %q, want %q", got, resolvedOut)
	}
	if len(rs.rows) != 0 {
		t.Errorf("rescan ran in separate mode: %+v", rs.rows)
	}
	rec, ok, err := r.store.SyncJobByID(context.Background(), jobID)
	if err != nil || !ok {
		t.Fatalf("SyncJobByID = %v/%v", ok, err)
	}
	if rec.Status != "completed" {
		t.Errorf("status = %q, want completed", rec.Status)
	}
	if rec.OutputPath != dest {
		t.Errorf("output_path = %q, want %q", rec.OutputPath, dest)
	}
}

// TestProcessOnceSeparateModeRejectsVaultOutputDir is the worker-side ADR-0002
// guard: a settings row edited straight in SQLite cannot make the muxer write
// into the Audio Vault.
func TestProcessOnceSeparateModeRejectsVaultOutputDir(t *testing.T) {
	r := newRig(t)
	setStoragePaths(t, r.settings, settings.StoragePaths{
		WorkDir: r.workDir, VaultDir: filepath.Join(t.TempDir(), "vault"), OutputDir: "",
	})
	// Bypass settings.Set so the worker's own guard is what rejects the row,
	// mimicking a settings row edited directly in SQLite.
	vaultDir := filepath.Join(t.TempDir(), "vault2")
	outDir := filepath.Join(vaultDir, "deliver")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	raw, err := json.Marshal(settings.StoragePaths{
		WorkDir: r.workDir, VaultDir: vaultDir, OutputDir: outDir,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := r.store.DB.Exec(
		`UPDATE settings SET value = ? WHERE key = ?`, string(raw), settings.KeyStoragePaths,
	); err != nil {
		t.Fatalf("write storage.paths row: %v", err)
	}
	setMuxOptions(t, r.settings, settings.MuxOptions{
		TrackTitleInMKV: "Polski Dubbing", OutputMode: settings.MuxOutputSeparate,
	})

	jobID := r.newJob(t)
	r.stage(t, jobID)
	mx := &fakeMuxer{}
	w := newWorker(t, r, &fakeExecutor{report: stableReport()}, mx, &fakeRescanner{})

	if processed, err := w.ProcessOnce(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	if len(mx.jobs) != 0 {
		t.Fatalf("Mux ran with an output dir inside the vault: %+v", mx.jobs)
	}
	rec, _, _ := r.store.SyncJobByID(context.Background(), jobID)
	if rec.Status != "failed" || !strings.Contains(rec.ErrorMessage, "vault") {
		t.Errorf("status/message = %q/%q, want failed mentioning vault", rec.Status, rec.ErrorMessage)
	}
}

// TestProcessOnceSeparateModeRequiresExistingOutputDir fails before mux when
// the configured directory is missing or unset.
func TestProcessOnceSeparateModeRequiresExistingOutputDir(t *testing.T) {
	cases := []struct {
		name string
		dir  string
	}{
		{name: "missing directory", dir: "/definitely/not/here"},
		{name: "unset output dir", dir: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			setStoragePaths(t, r.settings, settings.StoragePaths{
				WorkDir: r.workDir, VaultDir: filepath.Join(t.TempDir(), "vault"), OutputDir: tc.dir,
			})
			setMuxOptions(t, r.settings, settings.MuxOptions{
				TrackTitleInMKV: "Polski Dubbing", OutputMode: settings.MuxOutputSeparate,
			})
			jobID := r.newJob(t)
			r.stage(t, jobID)
			mx := &fakeMuxer{}
			w := newWorker(t, r, &fakeExecutor{report: stableReport()}, mx, &fakeRescanner{})

			if processed, err := w.ProcessOnce(context.Background()); err != nil || !processed {
				t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
			}
			if len(mx.jobs) != 0 {
				t.Fatalf("Mux ran without a usable output dir: %+v", mx.jobs)
			}
			rec, _, _ := r.store.SyncJobByID(context.Background(), jobID)
			if rec.Status != "failed" {
				t.Errorf("status = %q, want failed", rec.Status)
			}
		})
	}
}

// fakeTranscoder records the pre-flight checks and encodes the worker asks for.
type fakeTranscoder struct {
	available map[string]bool
	probes    []string
	requests  []transcode.Request
	// segments is the Segments slice of each call, recorded separately so a
	// test reads "was this a segmented render?" without unpacking the request.
	segments [][]transcode.Segment
	err      error
}

func (f *fakeTranscoder) EncoderAvailable(_ context.Context, encoder string) (bool, string) {
	f.probes = append(f.probes, encoder)
	if f.available == nil {
		return true, ""
	}
	ok := f.available[encoder]
	if !ok {
		return false, "encoder not found"
	}
	return true, ""
}

func (f *fakeTranscoder) Transcode(_ context.Context, req transcode.Request) (transcode.Result, error) {
	f.requests = append(f.requests, req)
	f.segments = append(f.segments, req.Segments)
	if f.err != nil {
		return transcode.Result{}, f.err
	}
	return transcode.Result{
		Path: req.Output, Codec: req.Plan.Codec,
		Channels: req.Plan.Channels, BitrateBPS: req.Plan.BitrateBPS,
	}, nil
}

func setAudioRules(t *testing.T, r *rig, body string) {
	t.Helper()
	if err := r.settings.Set(context.Background(), settings.KeyAudioOptions, json.RawMessage(body)); err != nil {
		t.Fatalf("set audio.options: %v", err)
	}
}

// TestProcessOnceAppliesTranscodeRule pins the ADR-0010 job path: the first
// matching rule encodes the copy in the temp folder (never the vault file), and
// the mux receives the encoded .mka instead of the staged original.
func TestProcessOnceAppliesTranscodeRule(t *testing.T) {
	r := newRig(t)
	setAudioRules(t, r, `{"rules":[
		{"enabled":false,"when":{},"then":{"codec":"flac"}},
		{"enabled":true,"when":{"source_codecs":["pcm_s16le"],"lang_iso2":"pl"},"then":{"codec":"opus","bitrate_bps":96000,"channels":2}}
	]}`)
	jobID := r.newJob(t)
	staged := r.stage(t, jobID)

	vaultPath := filepath.Join(filepath.Dir(r.target), "track.pl.mka")
	before, err := os.ReadFile(vaultPath)
	if err != nil {
		t.Fatalf("read vault track: %v", err)
	}

	mx := &fakeMuxer{}
	tr := &fakeTranscoder{}
	w := newWorker(t, r, &fakeExecutor{report: stableReport()}, mx, &fakeRescanner{})
	w.transcoder = tr

	if processed, err := w.ProcessOnce(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	if len(tr.probes) != 1 || tr.probes[0] != "libopus" {
		t.Fatalf("encoder probes = %v, want [libopus]", tr.probes)
	}
	if len(tr.requests) != 1 {
		t.Fatalf("transcode requests = %d, want 1", len(tr.requests))
	}
	req := tr.requests[0]
	if req.Input != staged {
		t.Fatalf("transcode input = %q, want staged copy %q", req.Input, staged)
	}
	if req.Plan.RuleIndex != 1 || req.Plan.Codec != "opus" || req.Plan.Channels != 2 || req.Plan.BitrateBPS != 96000 {
		t.Fatalf("plan = %+v, want rule 1 opus stereo 96k", req.Plan)
	}
	if req.SourceChannels != 2 || req.SourceDurationMS != 0 || req.LangISO2 != "pl" {
		t.Fatalf("source metadata = %+v", req)
	}
	if got, want := req.Output, filepath.Join(filepath.Dir(staged), "audio.transcoded.mka"); got != want {
		t.Fatalf("transcode output = %q, want %q", got, want)
	}
	if len(mx.jobs) != 1 || mx.jobs[0].AudioPath != req.Output {
		t.Fatalf("mux audio = %+v, want the encoded track", mx.jobs)
	}
	// The vault row's file is not an input and must stay byte-identical.
	after, err := os.ReadFile(vaultPath)
	if err != nil {
		t.Fatalf("re-read vault track: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("vault track content changed during the job")
	}
	rec, _, _ := r.store.SyncJobByID(context.Background(), jobID)
	if rec.Status != "completed" {
		t.Fatalf("status = %q (%s), want completed", rec.Status, rec.ErrorMessage)
	}
}

// TestProcessOnceWithoutRulesKeepsStagedCopy covers both legacy shapes: an
// absent section and an explicit empty list must mean "no transcode".
func TestProcessOnceWithoutRulesKeepsStagedCopy(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string // empty means: delete the row entirely
	}{
		{"absent section", ""},
		{"explicit empty rules", `{"rules":[]}`},
		{"legacy object without rules", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			if tc.body == "" {
				if _, err := r.store.DB.Exec(`DELETE FROM settings WHERE key = ?`, settings.KeyAudioOptions); err != nil {
					t.Fatalf("delete audio.options: %v", err)
				}
			} else {
				setAudioRules(t, r, tc.body)
			}
			jobID := r.newJob(t)
			staged := r.stage(t, jobID)

			mx := &fakeMuxer{}
			tr := &fakeTranscoder{}
			w := newWorker(t, r, &fakeExecutor{report: stableReport()}, mx, &fakeRescanner{})
			w.transcoder = tr

			if processed, err := w.ProcessOnce(context.Background()); err != nil || !processed {
				t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
			}
			if len(tr.probes) != 0 || len(tr.requests) != 0 {
				t.Fatalf("transcoder used without rules: probes=%v requests=%d", tr.probes, len(tr.requests))
			}
			if len(mx.jobs) != 1 || mx.jobs[0].AudioPath != staged {
				t.Fatalf("mux audio = %+v, want the untouched staged copy", mx.jobs)
			}
		})
	}
}

// TestProcessOnceMissingEncoderFailsBeforeExecute proves a missing encoder is a
// pre-flight error: no analysis, no staging, no mux.
func TestProcessOnceMissingEncoderFailsBeforeExecute(t *testing.T) {
	r := newRig(t)
	setAudioRules(t, r, `{"rules":[{"enabled":true,"when":{},"then":{"codec":"opus","bitrate_bps":96000}}]}`)
	jobID := r.newJob(t)

	ex := &fakeExecutor{report: stableReport()}
	mx := &fakeMuxer{}
	tr := &fakeTranscoder{available: map[string]bool{"libopus": false}}
	w := newWorker(t, r, ex, mx, &fakeRescanner{})
	w.transcoder = tr

	if processed, err := w.ProcessOnce(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	if len(ex.calls) != 0 || len(mx.jobs) != 0 || len(tr.requests) != 0 {
		t.Fatalf("work happened without the encoder: exec=%v mux=%d transcode=%d", ex.calls, len(mx.jobs), len(tr.requests))
	}
	rec, _, _ := r.store.SyncJobByID(context.Background(), jobID)
	if rec.Status != "failed" {
		t.Fatalf("status = %q, want failed", rec.Status)
	}
	if !strings.Contains(rec.ErrorMessage, "libopus") {
		t.Fatalf("error message %q does not name the missing encoder", rec.ErrorMessage)
	}
}

// TestProcessOnceTranscodeVerificationFailureFailsJob proves a mismatch is a
// hard job failure and never reaches mux.
func TestProcessOnceTranscodeVerificationFailureFailsJob(t *testing.T) {
	r := newRig(t)
	setAudioRules(t, r, `{"rules":[{"enabled":true,"when":{"channel_count":2},"then":{"codec":"aac","bitrate_bps":128000}}]}`)
	jobID := r.newJob(t)
	r.stage(t, jobID)

	mx := &fakeMuxer{}
	tr := &fakeTranscoder{err: transcode.ErrVerify}
	w := newWorker(t, r, &fakeExecutor{report: stableReport()}, mx, &fakeRescanner{})
	w.transcoder = tr

	if processed, err := w.ProcessOnce(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	if len(mx.jobs) != 0 {
		t.Fatalf("mux ran after a failed encode: %+v", mx.jobs)
	}
	rec, _, _ := r.store.SyncJobByID(context.Background(), jobID)
	if rec.Status != "failed" || !strings.Contains(rec.ErrorMessage, "verification") {
		t.Fatalf("job = %q / %q, want a verification failure", rec.Status, rec.ErrorMessage)
	}
	if _, err := os.Stat(filepath.Join(r.workDir, "jobs", strconv.FormatInt(jobID, 10))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging should be cleaned after a failure: %v", err)
	}
}

// TestProcessOnceContainerRefusalIsTerminalFailure pins the failure policy for
// mux.ErrUnsupportedContainer: a target or destination that is not Matroska is
// a terminal `failed` status whose message names the container, and the job is
// never retried (one executor call, one mux attempt, no queued row left).
func TestProcessOnceContainerRefusalIsTerminalFailure(t *testing.T) {
	r := newRig(t)
	jobID := r.newJob(t)
	audio := r.stage(t, jobID)

	ex := &fakeExecutor{report: stableReport()}
	mx := &fakeMuxer{err: fmt.Errorf("%w: %s is not a Matroska container (.mkv or .mka)",
		mux.ErrUnsupportedContainer, r.target)}
	w := newWorker(t, r, ex, mx, &fakeRescanner{})

	processed, err := w.ProcessOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	if len(ex.calls) != 1 {
		t.Fatalf("executor called %d times, want exactly 1 (no retry)", len(ex.calls))
	}
	if len(mx.jobs) != 1 {
		t.Fatalf("Mux called %d times, want exactly 1 (no retry)", len(mx.jobs))
	}
	rec, _, _ := r.store.SyncJobByID(context.Background(), jobID)
	if rec.Status != statusFailed {
		t.Fatalf("status = %q, want %q", rec.Status, statusFailed)
	}
	if !strings.Contains(rec.ErrorMessage, "container") {
		t.Fatalf("error_message = %q, want it to mention the container", rec.ErrorMessage)
	}
	if _, err := os.Stat(filepath.Dir(audio)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging should be cleaned after a terminal failure: %v", err)
	}

	// Terminal means the row is not requeued: a second pass finds no work.
	if again, err := w.ProcessOnce(context.Background()); err != nil || again {
		t.Fatalf("second ProcessOnce = %v/%v, want false/nil (job must not be retried)", again, err)
	}
	if len(ex.calls) != 1 || len(mx.jobs) != 1 {
		t.Fatalf("job was retried: executor %d, mux %d calls", len(ex.calls), len(mx.jobs))
	}
}

// TestProcessOnceTargetChangedRequeuesJob pins the worker half of ADR-0018: a
// mux that refused to publish because the media manager replaced the target
// mid-run did not fail the job. The row goes back to queued, the requeue is
// reported exactly once and the row is claimed exactly once (a requeued job is
// not claimed twice in the same pass), the drain pass it ran in ends instead of
// immediately re-running it, and a later pass does run it again against the new
// file.
func TestProcessOnceTargetChangedRequeuesJob(t *testing.T) {
	r := newRig(t)
	jobID := r.newJob(t)
	audio := r.stage(t, jobID)

	var logs []string
	ex := &fakeExecutor{report: stableReport()}
	mx := &fakeMuxer{err: fmt.Errorf("%w: %s", mux.ErrTargetChanged, r.target)}
	w := newWorkerLogging(t, r, ex, mx, &fakeRescanner{}, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})

	// One drain pass: the requeued job must not be picked up again inside it.
	w.drain(context.Background())

	if len(ex.calls) != 1 || len(mx.jobs) != 1 {
		t.Fatalf("executor %d, mux %d calls in one drain pass, want one each", len(ex.calls), len(mx.jobs))
	}
	rec, _, err := r.store.SyncJobByID(context.Background(), jobID)
	if err != nil {
		t.Fatalf("SyncJobByID: %v", err)
	}
	if rec.Status != statusQueued {
		t.Fatalf("status = %q, want %q (a changed target must not fail the job)", rec.Status, statusQueued)
	}
	if rec.ErrorMessage != "" {
		t.Fatalf("error_message = %q, want it cleared", rec.ErrorMessage)
	}
	// Count by message, not by total: the claim is an info line of its own
	// (U1.6), so the property here is that the requeue is reported exactly
	// once and the row was claimed exactly once — a requeued job must not be
	// claimed twice in the same pass.
	var claimed, requeued int
	for _, line := range logs {
		if strings.Contains(line, fmt.Sprintf("job %d claimed", jobID)) {
			claimed++
		}
		if strings.Contains(line, fmt.Sprintf("job %d target changed", jobID)) && strings.Contains(line, "requeued") {
			requeued++
		}
	}
	if requeued != 1 {
		t.Fatalf("requeue log lines = %d in %v, want exactly one explaining the changed target and the requeue", requeued, logs)
	}
	if claimed != 1 {
		t.Fatalf("claimed log lines = %d in %v, want exactly one (a requeued job must not be claimed twice)", claimed, logs)
	}
	// Issue #23: the staging goes with the pass; the next attempt re-stages.
	if _, err := os.Stat(filepath.Dir(audio)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging should be cleaned after a requeue: %v", err)
	}

	// A later pass does run it again: the fake executor does not re-stage, so
	// recreate what Execute would have left behind. A one-shot ProcessOnce
	// reports the requeued job as processed: this call did run it, and the row
	// is simply queued again for a later pass.
	r.stage(t, jobID)
	if processed, err := w.ProcessOnce(context.Background()); err != nil || !processed {
		t.Fatalf("second ProcessOnce = %v/%v, want true/nil (run again, still queued)", processed, err)
	}
	if len(ex.calls) != 2 || len(mx.jobs) != 2 {
		t.Fatalf("executor %d, mux %d calls, want the job to run again in a later pass", len(ex.calls), len(mx.jobs))
	}
}

// scriptedMuxer fails the job ids in errs (mapping them to a mux sentinel) and
// publishes everything else, so one drain pass can cover a mixed queue.
type scriptedMuxer struct {
	jobs []mux.Job
	errs map[int64]error
}

func (f *scriptedMuxer) Mux(_ context.Context, job mux.Job) (mux.Result, error) {
	f.jobs = append(f.jobs, job)
	if err, ok := f.errs[job.ID]; ok {
		return mux.Result{}, err
	}
	return mux.Result{OutputPath: job.TargetPath}, nil
}

func (f *scriptedMuxer) count(id int64) int {
	n := 0
	for _, job := range f.jobs {
		if job.ID == id {
			n++
		}
	}
	return n
}

// TestDrainSkipsRequeuedJobAndRunsTheJobsBehindIt pins the head-of-line fix: a
// job put back on the queue keeps its low id, so the pass must remember it as
// visited and carry on with the next queued job. Without the exclusion the
// requeued row is selected first on every iteration and the job behind it
// waits a whole interval - and, with repeated churn, starves.
func TestDrainSkipsRequeuedJobAndRunsTheJobsBehindIt(t *testing.T) {
	r := newRig(t)
	requeuedID := r.newJob(t)
	r.stage(t, requeuedID)
	laterID := r.newJob(t)
	r.stage(t, laterID)

	ex := &fakeExecutor{report: stableReport()}
	mx := &scriptedMuxer{errs: map[int64]error{
		requeuedID: fmt.Errorf("%w: %s", mux.ErrTargetChanged, r.target),
	}}
	w := newWorker(t, r, ex, mx, &fakeRescanner{})

	w.drain(context.Background())

	if got := mx.count(requeuedID); got != 1 {
		t.Fatalf("requeued job muxed %d times in one pass, want exactly 1", got)
	}
	if got := mx.count(laterID); got != 1 {
		t.Fatalf("job behind the requeued one muxed %d times in one pass, want exactly 1 (it must not be starved)", got)
	}
	if len(ex.calls) != 2 {
		t.Fatalf("executor calls = %v, want both jobs", ex.calls)
	}

	rec, _, err := r.store.SyncJobByID(context.Background(), requeuedID)
	if err != nil {
		t.Fatalf("SyncJobByID(requeued): %v", err)
	}
	if rec.Status != statusQueued {
		t.Fatalf("requeued job status = %q, want %q", rec.Status, statusQueued)
	}
	rec, _, err = r.store.SyncJobByID(context.Background(), laterID)
	if err != nil {
		t.Fatalf("SyncJobByID(later): %v", err)
	}
	if rec.Status != statusCompleted {
		t.Fatalf("later job status = %q, want %q", rec.Status, statusCompleted)
	}

	// The pass ended on an empty unvisited set, not by re-running anything: a
	// fresh pass (visited starts empty) still selects the requeued row.
	r.stage(t, requeuedID)
	if processed, err := w.ProcessOnce(context.Background()); err != nil || !processed {
		t.Fatalf("next ProcessOnce = %v/%v, want true/nil for the requeued row in a new pass", processed, err)
	}
	if got := mx.count(requeuedID); got != 2 {
		t.Fatalf("requeued job muxed %d times over two passes, want 2", got)
	}
}

// driftReport is the analysis of a clean frame-rate drift: the offsets ramp at
// 40.96 ms/s (a 25.000 fps dub against a 23.976 fps target) from an intercept of
// -340 ms, and the tempo factor that removes it is 1 - 40.96/1000 = 0.95904 —
// below 1, because the dub runs fast and has to be slowed. The median of the
// ramp is nowhere near the intercept, which is what makes the
// intercept-as-offset assertion bite.
func driftReport() analyze.Report {
	const (
		intercept = -340.0
		slope     = 40.96
	)
	positions := []float64{60, 150, 240, 330, 420, 510}
	offsets := make([]float64, len(positions))
	for i, p := range positions {
		offsets[i] = intercept + slope*p
	}
	return analyze.Report{
		Verdict:        analyze.VerdictDrifting,
		Classification: analyze.ClassificationDrifting,
		SyncMethod:     "same_lang_reference",
		OffsetsMS:      offsets,
		Fit: &analyze.LineFit{
			SlopeMSPerSec: slope,
			InterceptMS:   intercept,
			R2:            1,
			Points:        len(positions),
			Shape:         analyze.ShapeDrift,
			TempoFactor:   0.95904,
		},
	}
}

// TestMuxableReport pins what the worker may publish: ok/stable as before, and
// a drifting report only when the trend is a clean drift the worker has
// actually compensated. A cut, or a drift this worker did not fix, is review.
func TestMuxableReport(t *testing.T) {
	cases := []struct {
		name   string
		report analyze.Report
		want   bool
	}{
		{"ok", analyze.Report{Verdict: analyze.VerdictOK}, true},
		{"stable classification", analyze.Report{Verdict: analyze.ClassificationStable}, true},
		{"drifting without a fit", analyze.Report{Verdict: analyze.VerdictDrifting}, false},
		{"drifting with a cut", analyze.Report{
			Verdict: analyze.VerdictDrifting,
			Fit:     &analyze.LineFit{Shape: analyze.ShapeCut},
		}, false},
		{"drifting with a compensated drift", analyze.Report{
			Verdict: analyze.VerdictDrifting,
			Fit:     &analyze.LineFit{Shape: analyze.ShapeDrift, TempoFactor: 0.95904, TempoApplied: true},
		}, true},
		{"drifting with an uncompensated drift", analyze.Report{
			Verdict: analyze.VerdictDrifting,
			Fit:     &analyze.LineFit{Shape: analyze.ShapeDrift, TempoFactor: 0.95904},
		}, false},
		{"unreliable with a compensated drift", analyze.Report{
			Verdict: analyze.VerdictUnreliable,
			Fit:     &analyze.LineFit{Shape: analyze.ShapeDrift, TempoApplied: true},
		}, false},
		// #84: an accepted cut is muxable because the worker renders it with
		// per-segment offsets — the ok verdict alone would already let it
		// through, and this case pins that the piecewise shape does not turn
		// it into a review.
		{"ok with an accepted two-segment cut", analyze.Report{
			Verdict:        analyze.VerdictOK,
			Classification: analyze.ClassificationPiecewise,
			Piecewise:      &analyze.Piecewise{Segments: twoSegments()},
		}, true},
		// An accepted plan with a single segment is the ordinary constant-offset
		// job (no render), so it is judged exactly as before.
		{"ok with an accepted one-segment plan", analyze.Report{
			Verdict:   analyze.VerdictOK,
			Piecewise: &analyze.Piecewise{Segments: []analyze.Segment{{StartSec: 0, EndSec: 2400, OffsetMS: 1205}}},
		}, true},
		// A rejection is never published, whatever the verdict says: the whole
		// point of the rejection is that a human looks at the cut instead of
		// trusting a boundary the analysis could not locate (D5).
		{"ok with a rejected piecewise plan", analyze.Report{
			Verdict: analyze.VerdictOK,
			Piecewise: &analyze.Piecewise{
				Segments:       []analyze.Segment{{StartSec: 0, EndSec: 2400, OffsetMS: 1205}},
				RejectedReason: "boundary at 1500 s is not confident enough",
			},
		}, false},
		{"drifting with a rejected piecewise plan", analyze.Report{
			Verdict: analyze.VerdictDrifting,
			Piecewise: &analyze.Piecewise{
				Segments:       []analyze.Segment{{StartSec: 0, EndSec: 2400, OffsetMS: 1205}},
				RejectedReason: "the timeline is a single offset",
			},
		}, false},
		// An accepted cut the worker did not render (the verdict is not ok, so
		// the switch never cut it) must not slip through on the piecewise shape.
		{"drifting with an accepted cut that was not rendered", analyze.Report{
			Verdict:   analyze.VerdictDrifting,
			Piecewise: &analyze.Piecewise{Segments: twoSegments()},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := muxableReport(tc.report); got != tc.want {
				t.Fatalf("muxableReport(%q, fit %+v) = %v, want %v",
					tc.report.Verdict, tc.report.Fit, got, tc.want)
			}
		})
	}
}

// TestProcessOnceAppliesDriftTempoInOneEncode is #86 end to end at the worker:
// a drifting report with a clean fit is compensated in ONE ffmpeg pass, muxed
// at the intercept scaled by the factor it was stretched with (not the median),
// and the persisted report says so.
func TestProcessOnceAppliesDriftTempoInOneEncode(t *testing.T) {
	r := newRig(t)
	jobID := r.newJob(t)
	staged := r.stage(t, jobID)

	mx := &fakeMuxer{}
	tr := &fakeTranscoder{}
	w := newWorker(t, r, &fakeExecutor{report: driftReport()}, mx, &fakeRescanner{})
	w.transcoder = tr

	processed, err := w.ProcessOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	if len(tr.requests) != 1 {
		t.Fatalf("Transcode calls = %d, want exactly 1 (tempo and codec in one pass)", len(tr.requests))
	}
	req := tr.requests[0]
	if req.Input != staged {
		t.Fatalf("transcode input = %q, want the staged copy %q", req.Input, staged)
	}
	if req.Plan.TempoFactor != 0.95904 {
		t.Fatalf("plan tempo factor = %v, want 0.95904", req.Plan.TempoFactor)
	}
	// Direction: the fixture's offsets ramp UP (+40.96 ms/s), so the dub runs
	// fast and the correction must slow it. A factor of 1 + slope/1000 (the
	// sign this slice shipped inverted) would stretch the fast dub further and
	// double the drift instead of removing it.
	fixture := driftReport()
	if fixture.Fit.SlopeMSPerSec <= 0 {
		t.Fatalf("fixture slope = %v, want a positive ramp", fixture.Fit.SlopeMSPerSec)
	}
	if req.Plan.TempoFactor >= 1 || req.Plan.TempoFactor <= 0 {
		t.Fatalf("plan tempo factor = %v for a positive offset slope, want 0 < factor < 1 (slow the fast dub)",
			req.Plan.TempoFactor)
	}
	// No rule is configured, so the tempo-only plan is materialised as FLAC:
	// lossless, so a feature the operator never asked for cannot degrade the
	// dub's audio.
	if req.Plan.Codec != transcode.CodecFLAC {
		t.Fatalf("plan codec = %q, want %q for a tempo-only encode", req.Plan.Codec, transcode.CodecFLAC)
	}
	if req.Plan.RuleIndex != transcode.NoRule {
		t.Fatalf("plan rule index = %d, want %d (no rule produced this encode)", req.Plan.RuleIndex, transcode.NoRule)
	}

	if len(mx.jobs) != 1 {
		t.Fatalf("Mux calls = %d, want 1", len(mx.jobs))
	}
	// The intercept is measured on the uncorrected timeline, and the tempo
	// encode has already stretched that timeline by the factor — so the
	// constant mkvmerge shifts by is intercept/factor, not the raw intercept.
	// A 30 s intercept at 5 ms/s would otherwise leave ~150 ms of shift, well
	// outside the 40 ms stable band.
	if want := int64(math.Round(-340 / 0.95904)); want != -355 {
		t.Fatalf("round(-340/0.95904) = %d, want the fixture's -355", want)
	}
	if got := mx.jobs[0].SyncOffsetMS; got != -355 {
		t.Fatalf("mux offset = %d ms, want round(intercept/tempo_factor) = -355, not the raw intercept -340",
			got)
	}
	if got := mx.jobs[0].AudioPath; got != req.Output {
		t.Fatalf("mux audio = %q, want the encoded track %q", got, req.Output)
	}
	if mx.jobs[0].Report.Fit == nil || !mx.jobs[0].Report.Fit.TempoApplied {
		t.Fatalf("mux report fit = %+v, want tempo_applied true", mx.jobs[0].Report.Fit)
	}

	rec, ok, err := r.store.SyncJobByID(context.Background(), jobID)
	if err != nil || !ok {
		t.Fatalf("SyncJobByID = %v/%v", ok, err)
	}
	if rec.Status != statusCompleted {
		t.Fatalf("status = %q (%s), want completed", rec.Status, rec.ErrorMessage)
	}
	// The persisted offset is the one the job actually muxed with, so it is the
	// scaled residual too — the Sync page's "applied offset" badge reads it,
	// and reporting the raw intercept there would disagree with the mux.
	if rec.CalculatedOffsetMS != -355 {
		t.Fatalf("calculated_offset_ms = %d, want round(intercept/tempo_factor) = -355", rec.CalculatedOffsetMS)
	}
	var stored analyze.Report
	if err := json.Unmarshal([]byte(rec.CheckpointsJSON), &stored); err != nil {
		t.Fatalf("unmarshal stored report: %v", err)
	}
	if stored.Application == nil {
		t.Fatalf("stored report has no application block: %s", rec.CheckpointsJSON)
	}
	// %.5f, not %.4f: the factor is 0.95904 and four decimals would print
	// 0.9590 — a different number from the one the encode was given.
	if stored.Application.Mode != "re-encoded" || !strings.Contains(stored.Application.Note, "0.95904") {
		t.Fatalf("application = %+v, want a re-encoded note naming tempo 0.95904", stored.Application)
	}
	if stored.Fit == nil || !stored.Fit.TempoApplied {
		t.Fatalf("stored fit = %+v, want tempo_applied true", stored.Fit)
	}
}

// TestProcessOnceDriftingWithoutTempoStaysUnmuxable is the other half: a cut is
// a drifting verdict this worker cannot fix, so today's behaviour stands —
// failed, nothing encoded, nothing muxed.
func TestProcessOnceDriftingWithoutTempoStaysUnmuxable(t *testing.T) {
	r := newRig(t)
	jobID := r.newJob(t)
	r.stage(t, jobID)

	report := driftReport()
	report.Fit.Shape = analyze.ShapeCut
	report.Fit.TempoFactor = 0
	report.Fit.CutAtSec = 712
	report.Fit.CutUncertaintySec = 100

	mx := &fakeMuxer{}
	tr := &fakeTranscoder{}
	w := newWorker(t, r, &fakeExecutor{report: report}, mx, &fakeRescanner{})
	w.transcoder = tr

	processed, err := w.ProcessOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	if len(tr.requests) != 0 || len(tr.probes) != 0 {
		t.Fatalf("transcoder used for a cut: probes=%v requests=%d", tr.probes, len(tr.requests))
	}
	if len(mx.jobs) != 0 {
		t.Fatalf("Mux called %d times for a cut, want 0", len(mx.jobs))
	}
	rec, _, _ := r.store.SyncJobByID(context.Background(), jobID)
	if rec.Status != statusFailed {
		t.Fatalf("status = %q, want failed", rec.Status)
	}
	if !strings.Contains(rec.ErrorMessage, "not muxable") || !strings.Contains(rec.ErrorMessage, "drifting") {
		t.Fatalf("error message = %q, want it to name the unmuxable drifting verdict", rec.ErrorMessage)
	}
}

// TestProcessOnceTempoNeedsItsEncoder: the factor does not exist until the
// report does, so this encoder check is the deferred one — after Execute, and
// before any encode or mux.
func TestProcessOnceTempoNeedsItsEncoder(t *testing.T) {
	r := newRig(t)
	jobID := r.newJob(t)
	r.stage(t, jobID)

	ex := &fakeExecutor{report: driftReport()}
	mx := &fakeMuxer{}
	tr := &fakeTranscoder{available: map[string]bool{"flac": false}}
	w := newWorker(t, r, ex, mx, &fakeRescanner{})
	w.transcoder = tr

	processed, err := w.ProcessOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	if len(ex.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1: the check can only run after the report exists", len(ex.calls))
	}
	if len(tr.probes) != 1 || tr.probes[0] != "flac" {
		t.Fatalf("encoder probes = %v, want [flac]", tr.probes)
	}
	if len(tr.requests) != 0 || len(mx.jobs) != 0 {
		t.Fatalf("work happened without the encoder: transcode=%d mux=%d", len(tr.requests), len(mx.jobs))
	}
	rec, _, _ := r.store.SyncJobByID(context.Background(), jobID)
	if rec.Status != statusFailed {
		t.Fatalf("status = %q, want failed", rec.Status)
	}
	if !strings.Contains(rec.ErrorMessage, "flac") {
		t.Fatalf("error message %q does not name the missing encoder", rec.ErrorMessage)
	}
}

// twoSegments is the accepted cut the piecewise tests share: one boundary at
// 1500 s where the dub jumps by 2 s, on a 2400 s timeline — tiled from the film's
// first sample to its last, which is the only shape analyze stores (and the only
// one the renderer accepts).
func twoSegments() []analyze.Segment {
	return []analyze.Segment{
		{StartSec: 0, EndSec: 1500, OffsetMS: 800, Confidence: 0.91, Points: 60},
		{StartSec: 1500, EndSec: 2400, OffsetMS: 2800, Confidence: 0.86, Points: 40},
	}
}

// piecewiseCutReport is what analyze returns for a confident re-cut: verdict ok,
// classification piecewise, two accepted segments.
func piecewiseCutReport() analyze.Report {
	r := stableReport()
	r.Classification = analyze.ClassificationPiecewise
	r.OffsetsMS = []float64{800, 810, 2790, 2800}
	r.Piecewise = &analyze.Piecewise{
		Segments:         twoSegments(),
		Boundaries:       []analyze.Boundary{{AtSec: 1500, UncertaintySec: 22.5, StepMS: 2000}},
		DominantOffsetMS: 800,
		StepSec:          45,
	}
	return r
}

// TestProcessOncePiecewiseReencodesOnceWithZeroSync is #84 at the worker: an
// accepted cut is rendered ONCE with the segments, as FLAC when no rule
// matched, and muxed at offset 0 because the cut track is already aligned.
func TestProcessOncePiecewiseReencodesOnceWithZeroSync(t *testing.T) {
	r := newRig(t)
	jobID := r.newJob(t)
	staged := r.stage(t, jobID)

	mx := &fakeMuxer{}
	tr := &fakeTranscoder{}
	w := newWorker(t, r, &fakeExecutor{report: piecewiseCutReport()}, mx, &fakeRescanner{})
	w.transcoder = tr

	if processed, err := w.ProcessOnce(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	if len(tr.requests) != 1 {
		t.Fatalf("Transcode calls = %d, want exactly 1 for a 2-segment cut", len(tr.requests))
	}
	req := tr.requests[0]
	if req.Input != staged {
		t.Fatalf("transcode input = %q, want the staged copy %q", req.Input, staged)
	}
	if len(tr.segments) != 1 || len(tr.segments[0]) != 2 {
		t.Fatalf("recorded segments = %v, want the 2 segments of the plan", tr.segments)
	}
	for i, want := range twoSegments() {
		got := tr.segments[0][i]
		if got.StartSec != want.StartSec || got.EndSec != want.EndSec || got.OffsetMS != want.OffsetMS {
			t.Fatalf("segment %d = %+v, want %+v", i, got, want)
		}
	}
	// No rule matched, so the render is lossless: the operator switched this on
	// for timing, not to lose audio.
	if req.Plan.Codec != transcode.CodecFLAC || req.Plan.RuleIndex != transcode.NoRule {
		t.Fatalf("plan = %+v, want the FLAC no-rule plan", req.Plan)
	}
	if len(mx.jobs) != 1 {
		t.Fatalf("Mux calls = %d, want 1", len(mx.jobs))
	}
	if mx.jobs[0].SyncOffsetMS != 0 {
		t.Fatalf("mux offset = %d ms, want 0: every segment was placed absolutely", mx.jobs[0].SyncOffsetMS)
	}
	if mx.jobs[0].AudioPath != req.Output {
		t.Fatalf("mux audio = %q, want the rendered track %q", mx.jobs[0].AudioPath, req.Output)
	}

	rec, ok, err := r.store.SyncJobByID(context.Background(), jobID)
	if err != nil || !ok {
		t.Fatalf("SyncJobByID = %v/%v", ok, err)
	}
	if rec.Status != statusCompleted {
		t.Fatalf("status = %q (%s), want completed", rec.Status, rec.ErrorMessage)
	}
	if rec.CalculatedOffsetMS != 0 {
		t.Fatalf("stored offset = %d, want 0 for a piecewise job", rec.CalculatedOffsetMS)
	}
	var stored analyze.Report
	if err := json.Unmarshal([]byte(rec.CheckpointsJSON), &stored); err != nil {
		t.Fatalf("unmarshal stored report: %v", err)
	}
	if stored.Application == nil {
		t.Fatalf("stored report has no application block: %s", rec.CheckpointsJSON)
	}
	if got, want := stored.Application.Note, "re-encoded (piecewise, 2 segments)"; got != want {
		t.Fatalf("application note = %q, want %q", got, want)
	}
	if stored.Application.Mode != "piecewise" {
		t.Fatalf("application mode = %q, want piecewise", stored.Application.Mode)
	}
	if stored.Application.SegmentCount != 2 || len(stored.Application.Segments) != 2 {
		t.Fatalf("application segments = %d/%v, want 2 of them",
			stored.Application.SegmentCount, stored.Application.Segments)
	}
	if stored.Application.Segments[1].OffsetMS != 2800 {
		t.Fatalf("stored second segment = %+v, want the plan's 2800 ms offset", stored.Application.Segments[1])
	}
}

// TestProcessOncePiecewiseStartsTheTrackAtZero is the shift defect seen where it
// would have been published. The analysis tiles an accepted plan from 0, so the
// first segment the worker hands the encoder starts at 0 even when the first
// checkpoint that could testify was the second one (a quiet or fade-in opening
// falls below the quality floor and is dropped). The mux then runs at offset 0,
// and the published track's implied start — the first run's position minus the
// sync offset, which is where mkvmerge puts the track — is 0 rather than the
// 45 s of the first kept window.
func TestProcessOncePiecewiseStartsTheTrackAtZero(t *testing.T) {
	r := newRig(t)
	jobID := r.newJob(t)
	r.stage(t, jobID)

	report := piecewiseCutReport()
	report.Piecewise.Segments = []analyze.Segment{
		// What analyze stores when window 0 was dropped: the plan still runs from
		// the film's first sample, so run 0 carries the offset measured from the
		// first kept window onwards.
		{StartSec: 0, EndSec: 1500, OffsetMS: 800, Confidence: 0.91, Points: 59},
		{StartSec: 1500, EndSec: 2400, OffsetMS: 2800, Confidence: 0.86, Points: 40},
	}

	mx := &fakeMuxer{}
	tr := &fakeTranscoder{}
	w := newWorker(t, r, &fakeExecutor{report: report}, mx, &fakeRescanner{})
	w.transcoder = tr

	if processed, err := w.ProcessOnce(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	if len(tr.segments) != 1 || len(tr.segments[0]) != 2 {
		t.Fatalf("recorded segments = %v, want the plan's 2", tr.segments)
	}
	segments := tr.segments[0]
	if got := segments[0].StartSec; got != 0 {
		t.Fatalf("first segment starts at %v s, want 0: the plan tiles the film", got)
	}
	if got := segments[len(segments)-1].EndSec; got != 2400 {
		t.Fatalf("last segment ends at %v s, want the film's 2400 s", got)
	}
	if len(mx.jobs) != 1 {
		t.Fatalf("Mux calls = %d, want 1", len(mx.jobs))
	}
	impliedStartMS := int64(math.Round(segments[0].StartSec*1000)) - mx.jobs[0].SyncOffsetMS
	if impliedStartMS != 0 {
		t.Fatalf("muxed track implies a start of %d ms, want 0 (offset %d ms, first segment %v s)",
			impliedStartMS, mx.jobs[0].SyncOffsetMS, segments[0].StartSec)
	}
	rec, _, _ := r.store.SyncJobByID(context.Background(), jobID)
	if rec.CalculatedOffsetMS != 0 {
		t.Fatalf("stored offset = %d ms, want 0: the tiled track needs no shift", rec.CalculatedOffsetMS)
	}
}

// TestProcessOncePiecewiseUsesTheRuleCodec: a segment render is still an encode
// the operator configured, so a matching rule's codec wins over FLAC.
func TestProcessOncePiecewiseUsesTheRuleCodec(t *testing.T) {
	r := newRig(t)
	setAudioRules(t, r, `{"rules":[{"enabled":true,"when":{"source_codecs":["pcm_s16le"],"lang_iso2":"pl"},
		"then":{"codec":"opus","bitrate_bps":96000,"channels":2}}]}`)
	jobID := r.newJob(t)
	r.stage(t, jobID)

	mx := &fakeMuxer{}
	tr := &fakeTranscoder{}
	w := newWorker(t, r, &fakeExecutor{report: piecewiseCutReport()}, mx, &fakeRescanner{})
	w.transcoder = tr

	if processed, err := w.ProcessOnce(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	if len(tr.requests) != 1 {
		t.Fatalf("Transcode calls = %d, want 1", len(tr.requests))
	}
	req := tr.requests[0]
	if req.Plan.Codec != "opus" || req.Plan.BitrateBPS != 96000 || req.Plan.RuleIndex != 0 {
		t.Fatalf("plan = %+v, want the matching rule's opus 96k", req.Plan)
	}
	if len(tr.segments[0]) != 2 {
		t.Fatalf("segments = %v, want the 2 segments", tr.segments[0])
	}
	if len(mx.jobs) != 1 || mx.jobs[0].SyncOffsetMS != 0 {
		t.Fatalf("mux jobs = %+v, want one at offset 0", mx.jobs)
	}
}

// TestProcessOncePiecewiseRejectionFallsBackToMedian is D5's safe half: an
// unconfident boundary is never guessed at, the job keeps the dominant single
// offset, and the reason a human must look travels with the failure.
func TestProcessOncePiecewiseRejectionFallsBackToMedian(t *testing.T) {
	r := newRig(t)
	jobID := r.newJob(t)
	r.stage(t, jobID)

	report := piecewiseCutReport()
	report.Verdict = analyze.VerdictDrifting
	report.Classification = analyze.ClassificationDrifting
	report.Piecewise = &analyze.Piecewise{
		Segments:         []analyze.Segment{{StartSec: 300, EndSec: 2400, OffsetMS: 800, Points: 100}},
		DominantOffsetMS: 800,
		RejectedReason:   "boundary at 1500 s is not confident enough",
	}
	report.ReviewReason = "cut not confident — applied the dominant offset (boundary at 1500 s is not confident enough)"

	mx := &fakeMuxer{}
	tr := &fakeTranscoder{}
	w := newWorker(t, r, &fakeExecutor{report: report}, mx, &fakeRescanner{})
	w.transcoder = tr

	processed, err := w.ProcessOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	if len(tr.requests) != 0 || len(tr.probes) != 0 {
		t.Fatalf("transcoder used for a rejected cut: probes=%v requests=%d", tr.probes, len(tr.requests))
	}
	if len(mx.jobs) != 0 {
		t.Fatalf("Mux called %d times for a rejected cut, want 0", len(mx.jobs))
	}
	rec, _, _ := r.store.SyncJobByID(context.Background(), jobID)
	if rec.Status != statusFailed {
		t.Fatalf("status = %q, want failed", rec.Status)
	}
	if !strings.Contains(rec.ErrorMessage, "not muxable") || !strings.Contains(rec.ErrorMessage, "drifting") {
		t.Fatalf("error message = %q, want the unmuxable drifting verdict", rec.ErrorMessage)
	}
	// The review reason must reach the row: "not muxable" alone does not tell
	// the operator that a cut was found and refused.
	if !strings.Contains(rec.ErrorMessage, "cut not confident") {
		t.Fatalf("error message = %q, want it to carry report.review_reason", rec.ErrorMessage)
	}
}

// TestProcessOncePiecewiseNeedsAnEncoder: when no rule matched the render is
// materialised as FLAC, so that encoder has to exist before anything is cut.
func TestProcessOncePiecewiseNeedsAnEncoder(t *testing.T) {
	r := newRig(t)
	jobID := r.newJob(t)
	r.stage(t, jobID)

	mx := &fakeMuxer{}
	tr := &fakeTranscoder{available: map[string]bool{"flac": false}}
	w := newWorker(t, r, &fakeExecutor{report: piecewiseCutReport()}, mx, &fakeRescanner{})
	w.transcoder = tr

	if processed, err := w.ProcessOnce(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	if len(tr.probes) != 1 || tr.probes[0] != "flac" {
		t.Fatalf("encoder probes = %v, want [flac]", tr.probes)
	}
	if len(tr.requests) != 0 || len(mx.jobs) != 0 {
		t.Fatalf("work happened without the encoder: transcode=%d mux=%d", len(tr.requests), len(mx.jobs))
	}
	rec, _, _ := r.store.SyncJobByID(context.Background(), jobID)
	if rec.Status != statusFailed {
		t.Fatalf("status = %q, want failed", rec.Status)
	}
	if !strings.Contains(rec.ErrorMessage, "flac") {
		t.Fatalf("error message %q does not name the missing encoder", rec.ErrorMessage)
	}
}

// TestProcessOncePiecewiseSingleSegmentTakesTheOrdinaryPath: a dense plan that
// found no boundary is not a cut. The job copies the track and applies the one
// offset it has — no segmented render — unless a rule already demands an encode.
func TestProcessOncePiecewiseSingleSegmentTakesTheOrdinaryPath(t *testing.T) {
	single := func() analyze.Report {
		r := stableReport()
		r.Piecewise = &analyze.Piecewise{
			Segments:         []analyze.Segment{{StartSec: 0, EndSec: 2400, OffsetMS: 1205, Confidence: 0.93, Points: 120}},
			DominantOffsetMS: 1205,
			StepSec:          45,
		}
		// analyze records the copy it means to make for a one-segment plan.
		r.Application = &analyze.Application{Mode: "copy", Note: "copied untouched"}
		return r
	}

	t.Run("no rule", func(t *testing.T) {
		r := newRig(t)
		jobID := r.newJob(t)
		staged := r.stage(t, jobID)

		mx := &fakeMuxer{}
		tr := &fakeTranscoder{}
		w := newWorker(t, r, &fakeExecutor{report: single()}, mx, &fakeRescanner{})
		w.transcoder = tr

		if processed, err := w.ProcessOnce(context.Background()); err != nil || !processed {
			t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
		}
		if len(tr.requests) != 0 || len(tr.probes) != 0 {
			t.Fatalf("one segment was rendered: probes=%v requests=%d", tr.probes, len(tr.requests))
		}
		if len(mx.jobs) != 1 {
			t.Fatalf("Mux calls = %d, want 1", len(mx.jobs))
		}
		if got := mx.jobs[0].SyncOffsetMS; got != 1205 {
			t.Fatalf("mux offset = %d ms, want the median 1205", got)
		}
		if mx.jobs[0].AudioPath != staged {
			t.Fatalf("mux audio = %q, want the untouched staged copy", mx.jobs[0].AudioPath)
		}

		rec, _, _ := r.store.SyncJobByID(context.Background(), jobID)
		var stored analyze.Report
		if err := json.Unmarshal([]byte(rec.CheckpointsJSON), &stored); err != nil {
			t.Fatalf("unmarshal stored report: %v", err)
		}
		if stored.Application == nil || stored.Application.Mode != "copy" {
			t.Fatalf("application = %+v, want the copy analyze recorded", stored.Application)
		}
		if stored.Application.SegmentCount != 0 {
			t.Fatalf("application segment count = %d, want 0: nothing was cut", stored.Application.SegmentCount)
		}
	})

	t.Run("rule matched", func(t *testing.T) {
		r := newRig(t)
		setAudioRules(t, r, `{"rules":[{"enabled":true,"when":{"source_codecs":["pcm_s16le"]},
			"then":{"codec":"opus","bitrate_bps":96000}}]}`)
		jobID := r.newJob(t)
		r.stage(t, jobID)

		mx := &fakeMuxer{}
		tr := &fakeTranscoder{}
		w := newWorker(t, r, &fakeExecutor{report: single()}, mx, &fakeRescanner{})
		w.transcoder = tr

		if processed, err := w.ProcessOnce(context.Background()); err != nil || !processed {
			t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
		}
		if len(tr.requests) != 1 {
			t.Fatalf("Transcode calls = %d, want the rule's single pass", len(tr.requests))
		}
		if len(tr.segments[0]) != 0 {
			t.Fatalf("segments = %v, want none on the ordinary rule path", tr.segments[0])
		}
		if tr.requests[0].Plan.Codec != "opus" {
			t.Fatalf("plan = %+v, want the rule's opus", tr.requests[0].Plan)
		}
		if len(mx.jobs) != 1 || mx.jobs[0].SyncOffsetMS != 1205 {
			t.Fatalf("mux jobs = %+v, want one at the median offset 1205", mx.jobs)
		}
	})
}
