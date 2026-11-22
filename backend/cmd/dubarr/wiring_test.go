package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

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
