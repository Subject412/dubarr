package extract

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// tool resolves a media binary, skipping the test when the host lacks it.
func tool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not on PATH: %v", name, err)
	}
	return path
}

// writeTestWAV writes a 0.1 s mono 16-bit 8 kHz PCM WAV, a valid tiny Matroska
// payload source that needs no ffmpeg.
func writeTestWAV(t *testing.T, path string) {
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

// buildDonor muxes one WAV audio track per language into a donor MKV.
func buildDonor(t *testing.T, dir, name string, langs ...string) string {
	t.Helper()
	mkvmerge := tool(t, "mkvmerge")
	out := filepath.Join(dir, name)
	args := []string{"-o", out, "--quiet"}
	for i, lang := range langs {
		wav := filepath.Join(dir, fmt.Sprintf("track-%d.wav", i))
		writeTestWAV(t, wav)
		args = append(args, "--language", "0:"+lang, wav)
	}
	if outp, err := exec.Command(mkvmerge, args...).CombinedOutput(); err != nil {
		t.Fatalf("mkvmerge: %v: %s", err, outp)
	}
	return out
}

type harness struct {
	t      *testing.T
	ctx    context.Context
	db     *store.Store
	data   string
	work   string
	worker *Worker
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	tool(t, "ffprobe")
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
	w, err := New(Dependencies{
		Store:    st,
		Settings: settingsStore,
		DataDir:  dir,
		Prober:   probe.New(probe.Dependencies{}),
		Logf:     func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("extract.New: %v", err)
	}
	return &harness{t: t, ctx: context.Background(), db: st, data: dir, work: filepath.Join(dir, "work"), worker: w}
}

// seedExtracting registers a donor and moves it to the extracting state,
// returning its queue id.
func (h *harness) seedExtracting(sourcePath string) int64 {
	h.t.Helper()
	created, err := h.db.RecordImportCandidate(h.ctx, sourcePath, filepath.Base(sourcePath), 1)
	if err != nil || !created {
		h.t.Fatalf("RecordImportCandidate: created=%v err=%v", created, err)
	}
	if _, err := h.db.DB.ExecContext(h.ctx,
		`UPDATE import_queue SET status = 'extracting' WHERE source_file_path = ?`, sourcePath); err != nil {
		h.t.Fatalf("mark extracting: %v", err)
	}
	var id int64
	if err := h.db.DB.QueryRowContext(h.ctx,
		`SELECT id FROM import_queue WHERE source_file_path = ?`, sourcePath).Scan(&id); err != nil {
		h.t.Fatalf("queue id: %v", err)
	}
	return id
}

func (h *harness) status(id int64) (status, errMsg string) {
	h.t.Helper()
	if err := h.db.DB.QueryRowContext(h.ctx,
		`SELECT status, COALESCE(error_message, '') FROM import_queue WHERE id = ?`, id).Scan(&status, &errMsg); err != nil {
		h.t.Fatalf("read status: %v", err)
	}
	return status, errMsg
}

func (h *harness) stagingDir(id int64) string {
	return filepath.Join(h.work, "extract", fmt.Sprintf("%d", id))
}

func TestExtractionSucceedsAndStagesLosslessTrack(t *testing.T) {
	h := newHarness(t)
	donor := buildDonor(t, t.TempDir(), "Movie 2021 PL.DUB.mkv", "pol", "eng")
	id := h.seedExtracting(donor)

	processed, err := h.worker.ProcessOnce(h.ctx)
	if err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if !processed {
		t.Fatal("ProcessOnce processed = false, want true")
	}

	status, msg := h.status(id)
	if status != "completed" || msg != "" {
		t.Fatalf("status = %q err %q, want completed with no error", status, msg)
	}
	entries, err := os.ReadDir(h.stagingDir(id))
	if err != nil {
		t.Fatalf("staging dir: %v", err)
	}
	// The settled policy selects the Polish dub and the English reference.
	if len(entries) != 2 {
		t.Fatalf("staged files = %d, want 2 (pl dub + en reference)", len(entries))
	}
	for _, entry := range entries {
		if !strings.Contains(entry.Name(), ".pl.") && !strings.Contains(entry.Name(), ".en.") {
			t.Errorf("staging name = %q, want a language segment (.pl./.en.)", entry.Name())
		}
		out := filepath.Join(h.stagingDir(id), entry.Name())
		info, err := h.worker.prober.Probe(h.ctx, out)
		if err != nil {
			t.Fatalf("probe staged output: %v", err)
		}
		if len(info.Tracks) != 1 || info.Tracks[0].Codec != "pcm_s16le" || info.Tracks[0].Channels != 1 {
			t.Errorf("staged track %s = %+v, want one pcm_s16le mono track", entry.Name(), info.Tracks)
		}
	}

	// The queue is drained: a second pass has nothing to do.
	if processed, err := h.worker.ProcessOnce(h.ctx); err != nil || processed {
		t.Fatalf("second ProcessOnce = (%v, %v), want (false, nil)", processed, err)
	}
}

func TestExtractionFailsOnCorruptDonorAndWipesStaging(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	donor := filepath.Join(dir, "broken.mkv")
	if err := os.WriteFile(donor, []byte("this is not matroska"), 0o600); err != nil {
		t.Fatalf("write corrupt donor: %v", err)
	}
	id := h.seedExtracting(donor)

	// Simulate debris from an interrupted run so cleanup is observable.
	if err := os.MkdirAll(h.stagingDir(id), 0o700); err != nil {
		t.Fatalf("seed staging: %v", err)
	}
	if err := os.WriteFile(filepath.Join(h.stagingDir(id), "junk.mka"), []byte("junk"), 0o600); err != nil {
		t.Fatalf("seed junk: %v", err)
	}

	processed, err := h.worker.ProcessOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("ProcessOnce = (%v, %v), want (true, nil)", processed, err)
	}
	status, msg := h.status(id)
	if status != "failed" || msg == "" {
		t.Fatalf("status = %q err %q, want failed with a reason", status, msg)
	}
	if _, err := os.Stat(h.stagingDir(id)); !os.IsNotExist(err) {
		t.Fatalf("staging dir still present after failure (stat err = %v)", err)
	}
}

func TestExtractionFailsOnMissingDonor(t *testing.T) {
	h := newHarness(t)
	id := h.seedExtracting(filepath.Join(t.TempDir(), "gone.mkv"))

	processed, err := h.worker.ProcessOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("ProcessOnce = (%v, %v), want (true, nil)", processed, err)
	}
	status, msg := h.status(id)
	if status != "failed" || !strings.Contains(msg, "no such file") {
		t.Fatalf("status = %q err %q, want failed mentioning the missing donor", status, msg)
	}
	if _, err := os.Stat(h.stagingDir(id)); !os.IsNotExist(err) {
		t.Fatalf("staging dir present after missing donor (stat err = %v)", err)
	}
}

func TestStartReapsStaleStagingAndProcessesQueue(t *testing.T) {
	h := newHarness(t)
	stale := filepath.Join(h.work, "extract", "999")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("mkdir stale: %v", err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	donor := buildDonor(t, t.TempDir(), "Reaped.mkv", "pol")
	id := h.seedExtracting(donor)

	h.worker.Start(h.ctx)
	defer h.worker.Stop()

	// Start reaps synchronously, before the loop goroutine is launched.
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale staging dir not reaped (stat err = %v)", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if status, _ := h.status(id); status == "completed" {
			break
		}
		if time.Now().After(deadline) {
			status, msg := h.status(id)
			t.Fatalf("queue row not processed: status = %q err %q", status, msg)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestValidateExtractionRejectsMismatch(t *testing.T) {
	h := newHarness(t)
	out := buildDonor(t, t.TempDir(), "probe.mka", "pol")
	donor := probe.AudioTrack{Index: 0, Codec: "pcm_s16le", Channels: 1}
	info, err := h.worker.prober.Probe(h.ctx, out)
	if err != nil {
		t.Fatalf("probe fixture: %v", err)
	}

	if err := validateExtraction(h.ctx, h.worker.prober, out, donor, info.Duration); err != nil {
		t.Fatalf("matching track rejected: %v", err)
	}
	badCodec := donor
	badCodec.Codec = "aac"
	if err := validateExtraction(h.ctx, h.worker.prober, out, badCodec, info.Duration); err == nil {
		t.Error("codec mismatch accepted")
	}
	badChannels := donor
	badChannels.Channels = 6
	if err := validateExtraction(h.ctx, h.worker.prober, out, badChannels, info.Duration); err == nil {
		t.Error("channel mismatch accepted")
	}
	if err := validateExtraction(h.ctx, h.worker.prober, out, donor, info.Duration+DurationTolerance+time.Second); err == nil {
		t.Error("duration mismatch accepted")
	}
}
