package acceptance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
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
	"github.com/Subject412/dubarr/backend/internal/syncworker"
	"github.com/Subject412/dubarr/backend/internal/transcode"
)

// transcodeExecutor stands in for the Rust analysis: this test proves the
// ADR-0010 encode/mux artifact, not the sync maths.
type transcodeExecutor struct{}

func (transcodeExecutor) Execute(context.Context, int64) (analyze.Report, error) {
	return analyze.Report{
		Verdict:        analyze.VerdictOK,
		Classification: analyze.ClassificationStable,
		SyncMethod:     "same_lang_reference",
		OffsetsMS:      []float64{0},
	}, nil
}

type noRescan struct{}

func (noRescan) RescanAfterMux(context.Context, arrrescan.LibraryCacheRow) error { return nil }

// TestTranscodeRulePublishesRequestedStream is the issue #11 acceptance path:
// a matched rule encodes the temp folder copy once, ffprobe verifies it, mux
// publishes it, and the FINAL MKV carries the requested codec/channels/bitrate
// while the Audio Vault file is byte-identical and the video stream untouched.
func TestTranscodeRulePublishesRequestedStream(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe", "mkvmerge"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	work := filepath.Join(dir, "work")
	vault := filepath.Join(dir, "vault")
	media := filepath.Join(dir, "library")
	for _, d := range []string{work, vault, media} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	st, err := store.Open(data)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ss := settings.New(st.DB)
	ctx := context.Background()
	if err := ss.Seed(ctx, data); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	storageRaw, _ := json.Marshal(settings.StoragePaths{WorkDir: work, VaultDir: vault, MediaRoots: []string{media}})
	if err := ss.Set(ctx, settings.KeyStoragePaths, storageRaw); err != nil {
		t.Fatalf("set storage: %v", err)
	}

	// Target: MPEG-4 video + a 6 s stereo PCM English track.
	target := filepath.Join(media, "Movie (2020).mkv")
	video := filepath.Join(dir, "video.mkv")
	runTool(t, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=10:duration=6",
		"-c:v", "mpeg4", "-b:v", "200k", "-an", video)
	baseAudio := filepath.Join(dir, "base.wav")
	runTool(t, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=220:duration=6:sample_rate=48000",
		"-c:a", "pcm_s16le", baseAudio)
	runTool(t, "mkvmerge", "-o", target, "--quiet", video, "--language", "0:eng", baseAudio)
	preVideo := streamHash(t, target, "v:0")

	// Vault track: a 5.1 48 kHz PCM .mka, as the extractor archives it.
	vaultTrack := filepath.Join(vault, "Movie (2020).pl.mka")
	runTool(t, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=6:sample_rate=48000",
		"-af", "aformat=channel_layouts=5.1", "-c:a", "pcm_s16le", vaultTrack)
	preVault := sha256FileForTest(t, vaultTrack)

	mediaID, err := st.EnsureVaultMedia(ctx, store.VaultMediaRecord{MediaType: "movie", Title: "Movie", Year: 2020, FolderPath: media})
	if err != nil {
		t.Fatalf("EnsureVaultMedia: %v", err)
	}
	res, err := st.DB.Exec(`INSERT INTO audio_tracks
		(vault_media_id, group_uuid, lang_iso2, lang_iso3, variant, file_path, sha256,
		 codec, channels, channel_count, bitrate_bps, sample_rate, duration_ms)
		VALUES (?, 'g1', 'pl', 'pol', 'dubbing', ?, ?, 'pcm_s16le', '5.1', 6, 4608000, 48000, 6000)`,
		mediaID, vaultTrack, preVault)
	if err != nil {
		t.Fatalf("insert audio track: %v", err)
	}
	trackID, _ := res.LastInsertId()

	res, err = st.DB.Exec(`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		VALUES ('radarr', 'radarr', 'http://127.0.0.1:1', X'00', '0000')`)
	if err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	instanceID, _ := res.LastInsertId()
	res, err = st.DB.Exec(`INSERT INTO arr_library_cache
		(arr_instance_id, media_type, external_item_id, title, original_language_iso2,
		 target_file_path, eligibility_status, matches_target_tags, audio_languages_json)
		VALUES (?, 'movie', 7, 'Movie', 'eng', ?, 'eligible', 1, '["eng"]')`, instanceID, target)
	if err != nil {
		t.Fatalf("insert library row: %v", err)
	}
	libID, _ := res.LastInsertId()

	rec, err := st.CreateSyncJob(ctx, store.SyncJobCreate{
		VaultTrackID: trackID, TargetLibraryCacheID: libID,
		TargetVideoPath: target, TargetLangISO2: "pl",
	})
	if err != nil {
		t.Fatalf("CreateSyncJob: %v", err)
	}
	// Stage what Copy-on-Work leaves behind: a verified copy, not a vault read.
	jobDir := filepath.Join(work, "jobs", strconv.FormatInt(rec.ID, 10))
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatalf("mkdir job dir: %v", err)
	}
	copyForTest(t, vaultTrack, filepath.Join(jobDir, "target-0-track.pl.mka"))

	rules := `{"rules":[{"enabled":true,"when":{"source_codecs":["pcm_s16le"],"min_bitrate_bps":1536000,"channel_count":6},"then":{"codec":"opus","bitrate_bps":96000,"channels":2,"channel_layout":"stereo"}}]}`
	if err := ss.Set(ctx, settings.KeyAudioOptions, json.RawMessage(rules)); err != nil {
		t.Fatalf("set audio.options: %v", err)
	}

	w, err := syncworker.New(syncworker.Dependencies{
		Store: st, Settings: ss, Executor: transcodeExecutor{},
		Muxer:     mux.New(mux.Dependencies{}),
		Rescanner: noRescan{},
		WorkDir:   work, Interval: time.Millisecond, Logf: t.Logf,
		Transcoder: transcode.New(transcode.Dependencies{}),
	})
	if err != nil {
		t.Fatalf("syncworker.New: %v", err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if processed, err := w.ProcessOnce(runCtx); err != nil || !processed {
		t.Fatalf("ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	final, _, err := st.SyncJobByID(ctx, rec.ID)
	if err != nil {
		t.Fatalf("SyncJobByID: %v", err)
	}
	if final.Status != "completed" {
		t.Fatalf("job status = %q (%s), want completed", final.Status, final.ErrorMessage)
	}

	// The final MKV: original video, original English PCM, appended Opus stereo.
	streams := probeStreams(t, target)
	if len(streams) != 2 {
		t.Fatalf("final audio streams = %d, want 2: %+v", len(streams), streams)
	}
	got := streams[1]
	if got.codec != "opus" || got.channels != 2 {
		t.Fatalf("published track = %+v, want opus stereo", got)
	}
	if got.language != "pol" && got.language != "pl" {
		t.Fatalf("published language = %q, want the archived Polish tag", got.language)
	}
	if got.title != "Polish" {
		t.Fatalf("published title = %q, want the language name", got.title)
	}
	bps := audioBitrateBPS(t, target, got.index)
	if bps < 48000 || bps > 192000 {
		t.Fatalf("published audio bitrate = %d bps, want roughly the requested 96000", bps)
	}

	// Video bitstream and the vault file are untouched (ADR-0002).
	if postVideo := streamHash(t, target, "v:0"); postVideo != preVideo {
		t.Fatalf("target video stream changed: %s -> %s", preVideo, postVideo)
	}
	if postVault := sha256FileForTest(t, vaultTrack); postVault != preVault {
		t.Fatalf("vault track changed: %s -> %s", preVault, postVault)
	}
}

type probedStream struct {
	index    int
	codec    string
	channels int
	language string
	title    string
}

func probeStreams(t *testing.T, path string) []probedStream {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-select_streams", "a", "-show_entries",
		"stream=index,codec_name,channels:stream_tags=language,title", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe streams: %v", err)
	}
	var doc struct {
		Streams []struct {
			Index     int    `json:"index"`
			CodecName string `json:"codec_name"`
			Channels  int    `json:"channels"`
			Tags      struct {
				Language string `json:"language"`
				Title    string `json:"title"`
			} `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode ffprobe: %v: %s", err, out)
	}
	got := make([]probedStream, 0, len(doc.Streams))
	for _, s := range doc.Streams {
		got = append(got, probedStream{index: s.Index, codec: s.CodecName, channels: s.Channels, language: s.Tags.Language, title: s.Tags.Title})
	}
	return got
}

// audioBitrateBPS sums the appended stream's packet sizes over the container
// duration: Matroska omits per-stream bit_rate for Opus, so this measures the
// real published payload instead of trusting container metadata.
func audioBitrateBPS(t *testing.T, path string, streamIndex int) int {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "packet=stream_index,size",
		"-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe packets: %v", err)
	}
	total := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) != 2 || fields[0] != strconv.Itoa(streamIndex) {
			continue
		}
		n, err := strconv.Atoi(fields[1])
		if err == nil {
			total += n
		}
	}
	durOut, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe duration: %v", err)
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(durOut)), 64)
	if err != nil || seconds <= 0 {
		t.Fatalf("format duration %q: %v", durOut, err)
	}
	return int(float64(total) * 8 / seconds)
}

func streamHash(t *testing.T, path, stream string) string {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-i", path,
		"-map", "0:"+stream, "-c", "copy", "-f", "streamhash", "-hash", "sha256", "-").Output()
	if err != nil {
		t.Fatalf("stream hash %s %s: %v", path, stream, err)
	}
	return strings.TrimSpace(string(out))
}

func sha256FileForTest(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func copyForTest(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func runTool(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
	}
}
