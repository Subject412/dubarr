package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/extract"
	"github.com/Subject412/dubarr/backend/internal/library"
	"github.com/Subject412/dubarr/backend/internal/queue"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// TestWireScannerClassifiesTaggedMKV is the M2 acceptance path at the
// composition root: a tagged MKV dropped into the configured watch folder is
// recorded by the scanner, classified by the gate and surfaced by GET
// /api/queue with the eligible badge.
func TestWireScannerClassifiesTaggedMKV(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	settingsStore := settings.New(st.DB)
	if err := settingsStore.Seed(ctx, dir); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	watchDir := filepath.Join(dir, "watch")
	if err := os.MkdirAll(watchDir, 0o700); err != nil {
		t.Fatalf("mkdir watch dir: %v", err)
	}
	storageRaw, err := json.Marshal(settings.StoragePaths{
		WatchDir:   watchDir,
		WorkDir:    filepath.Join(dir, "work"),
		MediaRoots: []string{},
	})
	if err != nil {
		t.Fatalf("marshal storage: %v", err)
	}
	if err := settingsStore.Set(ctx, settings.KeyStoragePaths, storageRaw); err != nil {
		t.Fatalf("set storage: %v", err)
	}
	// A 1 s cadence keeps the test fast; the default is 10 s.
	watchRaw, err := json.Marshal(settings.WatchOptions{ScanIntervalSec: 1, StabilityCheckIntervalSec: 4})
	if err != nil {
		t.Fatalf("marshal watch options: %v", err)
	}
	if err := settingsStore.Set(ctx, settings.KeyWatchOptions, watchRaw); err != nil {
		t.Fatalf("set watch options: %v", err)
	}
	// ADR-0012 ships an empty default; this test asserts a configured marker on
	// the donor filename, so it sets the criterion explicitly.
	tagRaw, err := json.Marshal(settings.TargetTagFilter{
		RequiredFilenameTags: []string{"[ArcA310]"},
		TagMatchMode:         "ANY",
		EnforceTagFilter:     true,
	})
	if err != nil {
		t.Fatalf("marshal tag filter: %v", err)
	}
	if err := settingsStore.Set(ctx, settings.KeyTargetTagFilter, tagRaw); err != nil {
		t.Fatalf("set tag filter: %v", err)
	}

	sc, err := wireScanner(ctx, scannerWiring{
		Settings: settingsStore,
		Store:    st,
		Logf:     t.Logf,
	})
	if err != nil {
		t.Fatalf("wireScanner: %v", err)
	}
	if sc == nil {
		t.Fatal("wireScanner returned no scanner for a configured watch dir")
	}
	sc.Start(ctx)
	defer sc.Stop()

	// Drop a freshly muxed MKV whose filename carries the ADR-0005 marker.
	target := buildWiringMKV(t, watchDir, "Shrek (2001) [ArcA310].mkv")

	var summary store.ImportCandidateSummary
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := st.ListPendingImportCandidates(ctx)
		if err != nil {
			t.Fatalf("list candidates: %v", err)
		}
		if len(rows) == 1 && rows[0].HasGate {
			summary = rows[0]
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if summary.SourceFilePath == "" {
		t.Fatal("scanner did not record the dropped MKV with a gate verdict")
	}
	if summary.Eligibility != "eligible" {
		t.Errorf("eligibility = %q, want eligible (matched=%v missing=%v)",
			summary.Eligibility, summary.Gate.MatchedTags, summary.Gate.MissingTags)
	}
	if len(summary.Gate.MatchedTags) != 1 || summary.Gate.MatchedTags[0] != "[ArcA310]" {
		t.Errorf("matched tags = %v, want [[ArcA310]]", summary.Gate.MatchedTags)
	}

	mux := http.NewServeMux()
	queue.RegisterRoutes(mux, queue.Dependencies{
		DB:           st.DB,
		Settings:     settingsStore,
		RequireAdmin: func(h http.Handler) http.Handler { return h },
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/queue", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/queue = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []struct {
			Filename    string   `json:"filename"`
			Eligibility string   `json:"eligibility"`
			SourceFile  string   `json:"source_file_path"`
			MatchedTags []string `json:"matched_tags"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode queue: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("queue items = %d, want 1", len(body.Items))
	}
	item := body.Items[0]
	if item.SourceFile != target || item.Eligibility != "eligible" {
		t.Errorf("queue item = %+v, want eligible candidate for %s", item, target)
	}
	if len(item.MatchedTags) != 1 || item.MatchedTags[0] != "[ArcA310]" {
		t.Errorf("queue matched tags = %v, want [[ArcA310]]", item.MatchedTags)
	}
}

// TestWireScannerWithoutWatchDirIsDisabled verifies the scanner stays off when
// storage.paths.watch_dir is unset, so a fresh install can serve the wizard.
func TestWireScannerWithoutWatchDirIsDisabled(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	settingsStore := settings.New(st.DB)
	if err := settingsStore.Seed(context.Background(), dir); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	var logged int
	sc, err := wireScanner(context.Background(), scannerWiring{
		Settings: settingsStore,
		Store:    st,
		Logf:     func(string, ...any) { logged++ },
	})
	if err != nil {
		t.Fatalf("wireScanner: %v", err)
	}
	if sc != nil {
		t.Fatal("wireScanner started a scanner without a configured watch dir")
	}
	if logged != 1 {
		t.Errorf("log calls = %d, want 1", logged)
	}
}

// TestWireLibraryRoutes mounts the library group the composition root wires:
// both read routes answer 200 on an admin-passing mux over a seeded store.
func TestWireLibraryRoutes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	res, err := st.DB.ExecContext(ctx,
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		 VALUES ('main', 'radarr', 'http://radarr.invalid', x'00', '0000')`)
	if err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	instanceID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("instance id: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, title, root_folder_path,
		  target_file_path, eligibility_status)
		 VALUES (?, 'movie', 1, 'Shrek', '/media/movies', '/media/movies/Shrek.mkv', 'eligible')`,
		instanceID); err != nil {
		t.Fatalf("insert library cache: %v", err)
	}

	mux := http.NewServeMux()
	library.RegisterRoutes(mux, library.Dependencies{
		DB:           st.DB,
		RequireAdmin: func(h http.Handler) http.Handler { return h },
	})

	for path, key := range map[string]string{
		"/api/library/items":  "items",
		"/api/library/health": "instances",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, body %s", path, rec.Code, rec.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if _, ok := body[key]; !ok {
			t.Errorf("GET %s body has no %q key: %s", path, key, rec.Body.String())
		}
	}
}

// TestWireExtractPipelineArchivesApprovedDonor is the M3-4 acceptance path at
// the composition root: POST /api/queue/{id}/approve on a synthetic donor+target
// moves the row to extracting, the wired extraction worker stages the
// policy-selected tracks, and the vault hook publishes them as immutable
// .{iso2}.mka files with a matching sha256 while leaving no staging behind.
func TestWireExtractPipelineArchivesApprovedDonor(t *testing.T) {
	rig := newExtractRig(t)
	donor := rig.buildWiringDonor(t, "Shrek (2001) PL.DUB.mkv", "pol", "eng")
	id := rig.seedApprovedDonor(t, donor, "Shrek", 2001)

	rig.startWorker(t)

	status, msg := rig.waitStatus(t, id, "completed")
	if msg != "" {
		t.Fatalf("completed row carries error %q", msg)
	}
	if status != "completed" {
		t.Fatalf("status = %q, want completed", status)
	}

	// The settled policy exports the Polish dub and keeps the English
	// reference, so the vault holds one file per selected track.
	want := map[string]bool{
		"Shrek (2001).pl.mka": false,
		"Shrek (2001).en.mka": false,
	}
	entries, err := os.ReadDir(rig.vaultDir)
	if err != nil {
		t.Fatalf("read vault dir: %v", err)
	}
	if len(entries) != len(want) {
		t.Fatalf("vault files = %d, want %d (%v)", len(entries), len(want), vaultNames(entries))
	}
	for _, entry := range entries {
		if _, ok := want[entry.Name()]; !ok {
			t.Fatalf("unexpected vault file %q", entry.Name())
		}
		want[entry.Name()] = true
		path := filepath.Join(rig.vaultDir, entry.Name())
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat vault file: %v", err)
		}
		if info.Mode().Perm() != 0o444 {
			t.Errorf("%s mode = %o, want 0444", entry.Name(), info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read vault file: %v", err)
		}
		if len(data) == 0 {
			t.Errorf("%s is empty", entry.Name())
		}
		sum := sha256.Sum256(data)
		var recorded string
		if err := rig.store.DB.QueryRow(
			`SELECT sha256 FROM audio_tracks WHERE file_path = ?`, path).Scan(&recorded); err != nil {
			t.Fatalf("read recorded sha256 for %s: %v", entry.Name(), err)
		}
		if got := hex.EncodeToString(sum[:]); got != recorded {
			t.Errorf("%s recorded sha256 = %s, want hash of file content %s", entry.Name(), recorded, got)
		}
	}

	// Zero staging leftovers: the import directory is gone and the extraction
	// root holds no job directories.
	rig.assertStagingEmpty(t)
}

// TestWireExtractPipelineFailsCorruptDonorCleanly covers the failure side: a
// corrupt donor fails the row with a reason, creates no vault file and leaves
// no staging behind.
func TestWireExtractPipelineFailsCorruptDonorCleanly(t *testing.T) {
	rig := newExtractRig(t)
	donor := filepath.Join(rig.mediaDir, "Broken (2020) PL.DUB.mkv")
	if err := os.WriteFile(donor, []byte("this is not matroska"), 0o600); err != nil {
		t.Fatalf("write corrupt donor: %v", err)
	}
	id := rig.seedApprovedDonor(t, donor, "Broken", 2020)

	rig.startWorker(t)

	status, msg := rig.waitStatus(t, id, "failed")
	if status != "failed" || msg == "" {
		t.Fatalf("status = %q err %q, want failed with a reason", status, msg)
	}
	entries, err := os.ReadDir(rig.vaultDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read vault dir: %v", err)
	}
	for _, entry := range entries {
		t.Errorf("vault file %q created for a failed extraction", entry.Name())
	}
	rig.assertStagingEmpty(t)
}

// extractRig is the composition-root fixture shared by the M3-4 tests.
type extractRig struct {
	t        *testing.T
	dir      string
	mediaDir string
	vaultDir string
	workDir  string
	store    *store.Store
	settings *settings.Store
	worker   *extract.Worker
}

func newExtractRig(t *testing.T) *extractRig {
	t.Helper()
	for _, name := range []string{"mkvmerge", "ffprobe"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not on PATH: %v", name, err)
		}
	}
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	settingsStore := settings.New(st.DB)
	if err := settingsStore.Seed(context.Background(), dir); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	rig := &extractRig{
		t:        t,
		dir:      dir,
		mediaDir: filepath.Join(dir, "media"),
		vaultDir: filepath.Join(dir, "vault"),
		workDir:  filepath.Join(dir, "work"),
		store:    st,
		settings: settingsStore,
	}
	if err := os.MkdirAll(rig.mediaDir, 0o700); err != nil {
		t.Fatalf("mkdir media: %v", err)
	}
	raw, err := json.Marshal(settings.StoragePaths{
		VaultDir:   rig.vaultDir,
		WorkDir:    rig.workDir,
		MediaRoots: []string{rig.mediaDir},
	})
	if err != nil {
		t.Fatalf("marshal storage: %v", err)
	}
	if err := settingsStore.Set(context.Background(), settings.KeyStoragePaths, raw); err != nil {
		t.Fatalf("set storage: %v", err)
	}
	return rig
}

func (r *extractRig) startWorker(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w, err := wireExtractWorker(ctx, extractWiring{
		Settings: r.settings,
		Store:    r.store,
		DataDir:  r.dir,
		Interval: 20 * time.Millisecond,
		Logf:     t.Logf,
	})
	if err != nil {
		t.Fatalf("wireExtractWorker: %v", err)
	}
	if w == nil {
		t.Fatal("wireExtractWorker returned no worker for configured storage")
	}
	w.Start(ctx)
	t.Cleanup(w.Stop)
	r.worker = w
}

// buildWiringDonor muxes one tiny WAV audio track per language into a donor MKV
// under the media root.
func (r *extractRig) buildWiringDonor(t *testing.T, name string, langs ...string) string {
	t.Helper()
	mkvmerge, err := exec.LookPath("mkvmerge")
	if err != nil {
		t.Skipf("mkvmerge not on PATH: %v", err)
	}
	out := filepath.Join(r.mediaDir, name)
	args := []string{"-o", out, "--quiet"}
	for i, lang := range langs {
		wav := filepath.Join(t.TempDir(), fmt.Sprintf("track-%d.wav", i))
		writeWiringWAV(t, wav)
		args = append(args, "--language", "0:"+lang, wav)
	}
	if outp, err := exec.Command(mkvmerge, args...).CombinedOutput(); err != nil {
		t.Fatalf("mkvmerge donor: %v: %s", err, outp)
	}
	return out
}

// seedApprovedDonor links a pending candidate to an eligible arr target and
// approves it through the real queue API, returning the queue row id.
func (r *extractRig) seedApprovedDonor(t *testing.T, donor, title string, year int) int64 {
	t.Helper()
	ctx := context.Background()
	targetPath := filepath.Join(r.mediaDir, fmt.Sprintf("%s (%d) [ArcA310].mkv", title, year))
	if err := os.WriteFile(targetPath, []byte("target"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	res, err := r.store.DB.ExecContext(ctx,
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		 VALUES ('main', 'radarr', 'http://radarr.invalid', x'00', '0000')`)
	if err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	instanceID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("instance id: %v", err)
	}
	res, err = r.store.DB.ExecContext(ctx,
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, title, original_language_iso2, year,
		  target_file_path, audio_languages_json, eligibility_status)
		 VALUES (?, 'movie', 1, ?, 'en', ?, ?, '["en"]', 'eligible')`,
		instanceID, title, year, targetPath)
	if err != nil {
		t.Fatalf("insert library cache: %v", err)
	}
	cacheID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("cache id: %v", err)
	}

	created, err := r.store.RecordImportCandidate(ctx, donor, filepath.Base(donor), 1)
	if err != nil || !created {
		t.Fatalf("RecordImportCandidate: created=%v err=%v", created, err)
	}
	if _, err := r.store.DB.ExecContext(ctx,
		`UPDATE import_queue SET matched_arr_instance_id = ?, matched_library_cache_id = ?
		 WHERE source_file_path = ?`, instanceID, cacheID, donor); err != nil {
		t.Fatalf("link candidate: %v", err)
	}

	mux := http.NewServeMux()
	queue.RegisterRoutes(mux, queue.Dependencies{
		DB:           r.store.DB,
		Settings:     r.settings,
		RequireAdmin: func(h http.Handler) http.Handler { return h },
	})
	var id int64
	if err := r.store.DB.QueryRowContext(ctx,
		`SELECT id FROM import_queue WHERE source_file_path = ?`, donor).Scan(&id); err != nil {
		t.Fatalf("queue id: %v", err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/queue/%d/approve", id), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST approve = %d, body %s", rec.Code, rec.Body.String())
	}
	return id
}

func (r *extractRig) waitStatus(t *testing.T, id int64, want string) (status, errMsg string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err := r.store.DB.QueryRow(`SELECT status, COALESCE(error_message, '') FROM import_queue WHERE id = ?`, id).
			Scan(&status, &errMsg); err != nil {
			t.Fatalf("read queue status: %v", err)
		}
		if status == want {
			return status, errMsg
		}
		if time.Now().After(deadline) {
			return status, errMsg
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (r *extractRig) assertStagingEmpty(t *testing.T) {
	t.Helper()
	root := filepath.Join(r.workDir, "extract")
	entries, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read staging root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging leftovers: %v", vaultNames(entries))
	}
}

func writeWiringWAV(t *testing.T, path string) {
	t.Helper()
	const (
		sampleRate = 8000
		samples    = 800
		dataLen    = samples * 2
	)
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+dataLen))
	buf.WriteString("WAVEfmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate*2))
	binary.Write(&buf, binary.LittleEndian, uint16(2))
	binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(dataLen))
	buf.Write(make([]byte, dataLen))
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write wav: %v", err)
	}
}

func vaultNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// buildWiringMKV muxes a tiny WAV into dir/name with mkvmerge, skipping when
// mkvtoolnix is unavailable (it is on PATH in production and CI).
func buildWiringMKV(t *testing.T, dir, name string) string {
	t.Helper()
	mkvmerge, err := exec.LookPath("mkvmerge")
	if err != nil {
		t.Skipf("mkvmerge not on PATH: %v", err)
	}

	wav := filepath.Join(t.TempDir(), "source.wav")
	const (
		sampleRate = 8000
		samples    = 800
		dataLen    = samples * 2
	)
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+dataLen))
	buf.WriteString("WAVEfmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate*2))
	binary.Write(&buf, binary.LittleEndian, uint16(2))
	binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(dataLen))
	buf.Write(make([]byte, dataLen))
	if err := os.WriteFile(wav, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write wav: %v", err)
	}

	out := filepath.Join(dir, name)
	if outp, err := exec.Command(mkvmerge, "-o", out, "--quiet", wav).CombinedOutput(); err != nil {
		t.Fatalf("mkvmerge: %v: %s", err, outp)
	}
	return out
}
