package syncworker

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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arrrescan"
	"github.com/Subject412/dubarr/backend/internal/mux"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
	syncengine "github.com/Subject412/dubarr/backend/internal/sync"
	"github.com/Subject412/dubarr/backend/internal/syncjob"
)

var syncBinary string
var syncBuildErr error

func TestMain(m *testing.M) {
	os.Exit(buildSyncEngine(m))
}

// buildSyncEngine compiles the real Rust engine once for the integration test,
// mirroring the syncjob suite's TestMain. Unit tests do not need it.
func buildSyncEngine(m *testing.M) int {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		syncBuildErr = err
		return m.Run()
	}
	manifest := filepath.Join(root, "sync-engine", "Cargo.toml")
	if _, err := os.Stat(manifest); err != nil {
		syncBuildErr = fmt.Errorf("sync-engine source missing: %w", err)
		return m.Run()
	}
	cargo, err := exec.LookPath("cargo")
	if err != nil {
		syncBuildErr = fmt.Errorf("cargo not on PATH: %w", err)
		return m.Run()
	}
	dir, err := os.MkdirTemp("", "dubarr-syncworker-build-")
	if err != nil {
		syncBuildErr = err
		return m.Run()
	}
	defer os.RemoveAll(dir)
	cmd := exec.Command(cargo, "build", "--offline", "--manifest-path", manifest, "--target-dir", dir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		syncBuildErr = fmt.Errorf("cargo build --offline: %w", err)
		return m.Run()
	}
	syncBinary = filepath.Join(dir, "debug", syncengine.BinaryName)
	if _, err := os.Stat(syncBinary); err != nil {
		syncBuildErr = fmt.Errorf("built binary missing: %w", err)
	}
	return m.Run()
}

// fakeDecrypter stands in for the AES secret store; arrrescan only needs a
// non-empty API key for the mock arr server.
type fakeDecrypter struct{}

func (fakeDecrypter) DecryptString([]byte) (string, error) { return "test-api-key", nil }

type arrCapture struct {
	mu     sync.Mutex
	bodies []string
}

func (c *arrCapture) record(body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, body)
}

func (c *arrCapture) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bodies...)
}

// TestProcessOnceRunsRealExecuteMuxRescanChain is the M5-3 acceptance path:
// the real Rust engine analyses staged synthetic audio, mkvmerge publishes the
// track into the target, and the real arr client posts a Radarr rescan.
func TestProcessOnceRunsRealExecuteMuxRescanChain(t *testing.T) {
	if syncBuildErr != nil {
		t.Skipf("dubarr-sync build unavailable: %v", syncBuildErr)
	}
	for _, tool := range []string{"ffmpeg", "ffprobe", "mkvmerge"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}

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
	media := filepath.Join(dir, "library")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(media, 0o700); err != nil {
		t.Fatal(err)
	}
	storageRaw, _ := json.Marshal(settings.StoragePaths{
		WorkDir: work, VaultDir: filepath.Join(dir, "vault"), MediaRoots: []string{media},
	})
	if err := ss.Set(context.Background(), settings.KeyStoragePaths, storageRaw); err != nil {
		t.Fatalf("set storage: %v", err)
	}

	const sampleRate = 8000
	base := noise16(30*sampleRate, 21)
	donor := shifted(base, 1200*sampleRate/1000)
	baseWAV := filepath.Join(dir, "base.wav")
	donorWAV := filepath.Join(dir, "donor.wav")
	writePCM16WAV(t, baseWAV, base, sampleRate)
	writePCM16WAV(t, donorWAV, donor, sampleRate)

	target := filepath.Join(media, "Movie (2020) [ArcA310].mkv")
	if out, err := exec.Command("mkvmerge", "-o", target, "--quiet", "--language", "0:eng", baseWAV).CombinedOutput(); err != nil {
		t.Fatalf("mkvmerge target: %v\n%s", err, out)
	}

	plPath := filepath.Join(dir, "track.pl.mka")
	engPath := filepath.Join(dir, "track.eng.mka")
	copyFileForTest(t, baseWAV, plPath)
	copyFileForTest(t, donorWAV, engPath)

	mediaID, err := st.EnsureVaultMedia(context.Background(), store.VaultMediaRecord{
		MediaType: "movie", Title: "Movie", Year: 2020, FolderPath: media,
	})
	if err != nil {
		t.Fatalf("EnsureVaultMedia: %v", err)
	}
	plID := insertTrack(t, st.DB, mediaID, plPath)
	// Sibling reference in the original language (eng), hashed like the vault
	// writer would.
	engData, err := os.ReadFile(engPath)
	if err != nil {
		t.Fatalf("read eng track: %v", err)
	}
	engSum := sha256.Sum256(engData)
	res, err := st.DB.Exec(`INSERT INTO audio_tracks
		(vault_media_id, group_uuid, lang_iso2, lang_iso3, variant, file_path, sha256,
		 codec, channels, channel_count)
		VALUES (?, 'group-1', 'en', 'eng', 'original', ?, ?, 'pcm_s16le', '2.0', 2)`,
		mediaID, engPath, hex.EncodeToString(engSum[:]))
	if err != nil {
		t.Fatalf("insert sibling: %v", err)
	}

	var capture arrCapture
	arrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		capture.record(buf.String())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer arrSrv.Close()

	res, err = st.DB.Exec(`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		VALUES ('radarr', 'radarr', ?, X'00', '0000')`, arrSrv.URL)
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

	rec, err := st.CreateSyncJob(context.Background(), store.SyncJobCreate{
		VaultTrackID:         plID,
		TargetLibraryCacheID: libID,
		TargetVideoPath:      target,
		TargetLangISO2:       "pl",
	})
	if err != nil {
		t.Fatalf("CreateSyncJob: %v", err)
	}

	engine, err := syncengine.New(syncBinary)
	if err != nil {
		t.Fatalf("sync.New: %v", err)
	}
	jobSvc := syncjob.RegisterRoutes(http.NewServeMux(), syncjob.Dependencies{
		DB:       st.DB,
		Settings: ss,
		WorkDir:  work,
		Engine:   engine,
	})
	rescanner, err := arrrescan.New(arrrescan.Deps{DB: st.DB, Secrets: fakeDecrypter{}})
	if err != nil {
		t.Fatalf("arrrescan.New: %v", err)
	}
	w, err := New(Dependencies{
		Store:     st,
		Settings:  ss,
		Executor:  jobSvc,
		Muxer:     mux.New(mux.Dependencies{}),
		Rescanner: rescanner,
		WorkDir:   work,
		Interval:  time.Millisecond,
		Logf:      t.Logf,
	})
	if err != nil {
		t.Fatalf("syncworker.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	processed, err := w.ProcessOnce(ctx)
	if err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}

	final, _, err := st.SyncJobByID(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("SyncJobByID: %v", err)
	}
	if final.Status != "completed" {
		t.Fatalf("job status = %q (%s), want completed", final.Status, final.ErrorMessage)
	}
	var report struct {
		Verdict   string    `json:"verdict"`
		OffsetsMS []float64 `json:"offsets_ms"`
	}
	if err := json.Unmarshal([]byte(final.CheckpointsJSON), &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if report.Verdict != "ok" {
		t.Fatalf("report verdict = %q, want ok", report.Verdict)
	}
	if final.CalculatedOffsetMS < 1100 || final.CalculatedOffsetMS > 1300 {
		t.Fatalf("offset = %d, want ~1200", final.CalculatedOffsetMS)
	}

	// The target now carries the target audio plus the appended pl track.
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a",
		"-show_entries", "stream=index", "-of", "csv=p=0", target).Output()
	if err != nil {
		t.Fatalf("ffprobe published target: %v", err)
	}
	if n := len(strings.Fields(string(out))); n != 2 {
		t.Fatalf("published target audio streams = %d, want 2", n)
	}
	if _, err := os.Stat(target + ".bak"); err != nil {
		t.Fatalf("backup beside target: %v", err)
	}

	bodies := capture.snapshot()
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"name":"RescanMovie"`) || !strings.Contains(bodies[0], `"movieId":7`) {
		t.Fatalf("arr rescan bodies = %v, want one Radarr RescanMovie movieId 7", bodies)
	}

	if _, err := os.Stat(filepath.Join(work, "jobs", fmt.Sprintf("%d", rec.ID))); err != nil {
		t.Fatalf("staging should be kept on success: %v", err)
	}
}

func copyFileForTest(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func writePCM16WAV(t *testing.T, path string, samples []int16, sampleRate int) {
	t.Helper()
	var buf bytes.Buffer
	dataLen := len(samples) * 2
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
	for _, s := range samples {
		binary.Write(&buf, binary.LittleEndian, s)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write wav: %v", err)
	}
}

func noise16(n int, seed uint64) []int16 {
	out := make([]int16, n)
	x := seed | 1
	for i := range out {
		x = x*6364136223846793005 + 1442695040888963407
		out[i] = int16(int64(x>>33) >> 20)
	}
	return out
}

func shifted(samples []int16, lead int) []int16 {
	out := make([]int16, len(samples))
	copy(out, samples[lead:])
	return out
}
