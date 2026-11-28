package extract

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
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
	writeTestWAVTrack(t, path, 0.1, 1)
}

// writeTestWAVTrack writes a PCM WAV of the given length and channel count at
// 8 kHz, 16 bit.
func writeTestWAVTrack(t *testing.T, path string, seconds float64, channels int) {
	t.Helper()
	const sampleRate = 8000
	samples := int(math.Round(seconds * sampleRate))
	dataLen := samples * 2 * channels
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+dataLen))
	buf.WriteString("WAVEfmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(channels))
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate*2*channels))
	binary.Write(&buf, binary.LittleEndian, uint16(2*channels))
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

// testDonorTrack is one audio track of a synthetic donor.
type testDonorTrack struct {
	lang     string
	seconds  float64
	channels int
}

// buildVideoDonor renders a tiny MPEG-4 video track and muxes it in front of
// one PCM audio track per testDonorTrack with mkvmerge. This mirrors a real
// donor shape: the video stream occupies container index 0, so ffprobe's audio
// stream indices (1, 2, ...) no longer match mkvmerge's audio-only ids.
func buildVideoDonor(t *testing.T, dir, name string, tracks ...testDonorTrack) string {
	t.Helper()
	return buildTracksDonor(t, dir, name, true, tracks...)
}

// buildAudioDonor muxes PCM audio tracks only, so container indices still line
// up with mkvmerge's audio ids.
func buildAudioDonor(t *testing.T, dir, name string, tracks ...testDonorTrack) string {
	t.Helper()
	return buildTracksDonor(t, dir, name, false, tracks...)
}

func buildTracksDonor(t *testing.T, dir, name string, withVideo bool, tracks ...testDonorTrack) string {
	t.Helper()
	mkvmerge := tool(t, "mkvmerge")

	args := []string{"-o", filepath.Join(dir, name), "--quiet"}
	if withVideo {
		ffmpeg := tool(t, "ffmpeg")
		video := filepath.Join(dir, "video.mkv")
		vid := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "testsrc=size=64x64:rate=5:duration=1",
			"-c:v", "mpeg4", "-an", video)
		if outp, err := vid.CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg video fixture: %v: %s", err, outp)
		}
		args = append(args, video)
	}
	for i, tr := range tracks {
		wav := filepath.Join(dir, fmt.Sprintf("track-%d.wav", i))
		writeTestWAVTrack(t, wav, tr.seconds, tr.channels)
		args = append(args, "--language", "0:"+tr.lang, wav)
	}
	if outp, err := exec.Command(mkvmerge, args...).CombinedOutput(); err != nil {
		t.Fatalf("mkvmerge donor: %v: %s", err, outp)
	}
	return filepath.Join(dir, name)
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
	// Seed writes the settled default WORK_DIR (<DATA_DIR>/temp); read it back
	// from the same Defaults table so this fixture cannot drift from it again.
	paths, _ := settings.Defaults(dir)[settings.KeyStoragePaths].(settings.StoragePaths)
	return &harness{t: t, ctx: context.Background(), db: st, data: dir, work: paths.WorkDir, worker: w}
}

// seedExtracting registers a donor, pairs it with itself and moves it to the
// extracting state, returning its queue id.
func (h *harness) seedExtracting(sourcePath string) int64 {
	h.t.Helper()
	return h.seedApproved(sourcePath, sourcePath)
}

// seedApproved registers a candidate whose persisted donor is donorPath, then
// moves it to extracting. An empty donorPath marks the row extracting without a
// stored donor (the never-paired state).
func (h *harness) seedApproved(sourcePath, donorPath string) int64 {
	h.t.Helper()
	created, err := h.db.RecordImportCandidate(h.ctx, sourcePath, filepath.Base(sourcePath), 1)
	if err != nil || !created {
		h.t.Fatalf("RecordImportCandidate: created=%v err=%v", created, err)
	}
	var id int64
	if err := h.db.DB.QueryRowContext(h.ctx,
		`SELECT id FROM import_queue WHERE source_file_path = ?`, sourcePath).Scan(&id); err != nil {
		h.t.Fatalf("queue id: %v", err)
	}
	if donorPath != "" {
		if err := h.db.SetImportCandidateDonor(h.ctx, id, donorPath); err != nil {
			h.t.Fatalf("SetImportCandidateDonor: %v", err)
		}
		return id
	}
	if _, err := h.db.DB.ExecContext(h.ctx,
		`UPDATE import_queue SET status = 'extracting' WHERE id = ?`, id); err != nil {
		h.t.Fatalf("mark extracting: %v", err)
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

// TestVaultHookReceivesProbedBitrateAndSampleRate guards the metadata that
// Transcode Rules match on: an archived PCM track must carry the sample rate
// from its re-probe and a positive bitrate, not zeroes.
func TestVaultHookReceivesProbedBitrateAndSampleRate(t *testing.T) {
	h := newHarness(t)
	var captured []StagedTrack
	w, err := New(Dependencies{
		Store:    h.db,
		Settings: h.worker.settings,
		DataDir:  h.data,
		Prober:   h.worker.prober,
		VaultHook: func(_ context.Context, _ store.ImportExtractionJob, tracks []StagedTrack) error {
			captured = tracks
			return nil
		},
		Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("extract.New with vault hook: %v", err)
	}
	donor := buildAudioDonor(t, t.TempDir(), "Movie PL.mkv", testDonorTrack{lang: "pol", seconds: 0.1, channels: 1})
	id := h.seedExtracting(donor)
	if processed, err := w.ProcessOnce(h.ctx); err != nil || !processed {
		t.Fatalf("ProcessOnce = (%v, %v), want (true, nil)", processed, err)
	}
	if status, msg := h.status(id); status != "completed" || msg != "" {
		t.Fatalf("status = %q err %q, want completed", status, msg)
	}
	if len(captured) != 1 {
		t.Fatalf("vault hook got %d tracks, want 1", len(captured))
	}
	got := captured[0]
	if got.SampleRate != 8000 {
		t.Errorf("SampleRate = %d, want 8000", got.SampleRate)
	}
	if got.BitrateBPS <= 0 {
		t.Errorf("BitrateBPS = %d, want a positive probed bitrate", got.BitrateBPS)
	}
}

// stagedByLang returns the staged output whose name contains segment (".pl.",
// ".en."), or "" when none does.
func (h *harness) stagedByLang(id int64, segment string) string {
	h.t.Helper()
	entries, err := os.ReadDir(h.stagingDir(id))
	if err != nil {
		h.t.Fatalf("staging dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), segment) {
			return filepath.Join(h.stagingDir(id), entry.Name())
		}
	}
	return ""
}

// assertStagedTrack re-probes one staged file and checks codec, channels and
// duration against the donor track it came from.
func (h *harness) assertStagedTrack(path, codec string, channels int, duration time.Duration) {
	h.t.Helper()
	if path == "" {
		h.t.Fatal("expected a staged output, found none")
	}
	info, err := h.worker.prober.Probe(h.ctx, path)
	if err != nil {
		h.t.Fatalf("probe staged %s: %v", path, err)
	}
	if len(info.Tracks) != 1 {
		h.t.Fatalf("%s has %d audio tracks, want 1", path, len(info.Tracks))
	}
	got := info.Tracks[0]
	if got.Codec != codec || got.Channels != channels {
		h.t.Errorf("%s = %s/%d channels, want %s/%d", path, got.Codec, got.Channels, codec, channels)
	}
	if delta := info.Duration - duration; delta > DurationTolerance || delta < -DurationTolerance {
		h.t.Errorf("%s duration %v, want %v (±%v)", path, info.Duration, duration, DurationTolerance)
	}
}

// The direct donor measurement is the fallback for mkvmerge versions that do
// not report per-track statistics in JSON identification.
func TestMeasureDonorTrackReportsOwnDuration(t *testing.T) {
	h := newHarness(t)
	donor := buildAudioDonor(t, t.TempDir(), "Unequal.mkv",
		testDonorTrack{lang: "pol", seconds: 3, channels: 1},
		testDonorTrack{lang: "eng", seconds: 10, channels: 1})
	mkvTracks, err := h.worker.audioTracks(h.ctx, donor)
	if err != nil {
		t.Fatalf("audioTracks: %v", err)
	}
	if len(mkvTracks) < 2 {
		t.Fatalf("audioTracks = %d tracks, want 2", len(mkvTracks))
	}
	first := mkvTracks[0]
	got, err := h.worker.measureDonorTrack(h.ctx, donor, first.ID)
	if err != nil {
		t.Fatalf("measureDonorTrack: %v", err)
	}
	if delta := got - 3*time.Second; delta > 100*time.Millisecond || delta < -100*time.Millisecond {
		t.Errorf("measured duration = %v, want about 3s", got)
	}
}

// TestExtractionPairsAudioTracksPositionallyWithVideoStream is the M3-1 review
// regression: a donor with a video stream shifts ffprobe's audio stream indices
// to 1..N while mkvmerge's audio-only id list starts at 0..N-1, so indexing the
// id list by the ffprobe stream index panicked.
func TestExtractionPairsAudioTracksPositionallyWithVideoStream(t *testing.T) {
	h := newHarness(t)
	donor := buildVideoDonor(t, t.TempDir(), "Video Donor.mkv",
		testDonorTrack{lang: "pol", seconds: 1, channels: 1},
		testDonorTrack{lang: "eng", seconds: 1, channels: 2},
	)
	id := h.seedExtracting(donor)

	processed, err := h.worker.ProcessOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("ProcessOnce = (%v, %v), want (true, nil)", processed, err)
	}
	if status, msg := h.status(id); status != "completed" || msg != "" {
		t.Fatalf("status = %q err %q, want completed with no error", status, msg)
	}
	// Distinct channel counts prove the positional pairing is correct, not just
	// that extraction happened to survive.
	h.assertStagedTrack(h.stagedByLang(id, ".pl."), "pcm_s16le", 1, time.Second)
	h.assertStagedTrack(h.stagedByLang(id, ".en."), "pcm_s16le", 2, time.Second)
}

// TestExtractionValidatesPerTrackDonorDuration covers the review case where
// unequal-length donor audio tracks were validated against the container
// duration (here 10s) and falsely failed.
func TestExtractionValidatesPerTrackDonorDuration(t *testing.T) {
	h := newHarness(t)
	donor := buildAudioDonor(t, t.TempDir(), "Unequal Donor.mkv",
		testDonorTrack{lang: "pol", seconds: 3, channels: 1},
		testDonorTrack{lang: "eng", seconds: 10, channels: 1},
	)
	media, err := h.worker.prober.Probe(h.ctx, donor)
	if err != nil {
		t.Fatalf("probe donor: %v", err)
	}
	if media.Duration < 9*time.Second {
		t.Fatalf("donor container duration = %v, want the 10s track's length", media.Duration)
	}
	id := h.seedExtracting(donor)

	processed, err := h.worker.ProcessOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("ProcessOnce = (%v, %v), want (true, nil)", processed, err)
	}
	if status, msg := h.status(id); status != "completed" || msg != "" {
		t.Fatalf("status = %q err %q, want completed with no error (probed pl track duration %v, container %v)",
			status, msg, media.Tracks[0].Duration, media.Duration)
	}
	// The 3s track must validate against 3s, not the 10s container duration.
	h.assertStagedTrack(h.stagedByLang(id, ".pl."), "pcm_s16le", 1, 3*time.Second)
	h.assertStagedTrack(h.stagedByLang(id, ".en."), "pcm_s16le", 1, 10*time.Second)
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

func TestExtractionUsesPersistedDonor(t *testing.T) {
	h := newHarness(t)
	donor := buildDonor(t, t.TempDir(), "Approved Donor PL.DUB.mkv", "pol", "eng")
	// The queue's source_file_path is an unrelated drop; only the persisted
	// donor may be probed and extracted.
	candidate := filepath.Join(t.TempDir(), "candidate.mkv")
	id := h.seedApproved(candidate, donor)

	processed, err := h.worker.ProcessOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("ProcessOnce = (%v, %v), want (true, nil)", processed, err)
	}
	if status, msg := h.status(id); status != "completed" || msg != "" {
		t.Fatalf("status = %q err %q, want completed with no error", status, msg)
	}
	if h.stagedByLang(id, ".pl.") == "" || h.stagedByLang(id, ".en.") == "" {
		t.Error("expected the persisted donor's pl and en tracks to be staged")
	}
}

// TestExtractionFailsWithoutPersistedDonor pins the live bug: a row that was
// never paired must fail loudly instead of probing the candidate file (which
// here is a perfectly valid donor, so a fallback would wrongly succeed).
func TestExtractionFailsWithoutPersistedDonor(t *testing.T) {
	h := newHarness(t)
	candidate := buildDonor(t, t.TempDir(), "Looks Like A Donor PL.DUB.mkv", "pol", "eng")
	id := h.seedApproved(candidate, "")

	processed, err := h.worker.ProcessOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("ProcessOnce = (%v, %v), want (true, nil)", processed, err)
	}
	status, msg := h.status(id)
	if status != "failed" || !strings.Contains(msg, "no donor paired") {
		t.Fatalf("status = %q err %q, want failed with \"no donor paired\"", status, msg)
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
