package acceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/extract"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
	syncengine "github.com/Subject412/dubarr/backend/internal/sync"
	"github.com/Subject412/dubarr/backend/internal/vault"
)

// Fixture names. The suite never points the pipeline at these originals: every
// test copies the files it drives into its own t.TempDir tree.
const (
	shrekTargetName   = "Shrek (2001) - [ArcA310][Remux-2160p][HDR10][Opus 7.1][AV1][imdb-tt0126029]-FraMeSToR.mkv"
	shrekDonorName    = "Shrek.2001.1080p.SKST.WEB-DL.MULTi.DDP5.1.H.264-KIFR.mkv"
	minionsTargetName = "Minions and Monsters (2026) - [ArcA310][Remux-2160p][HDR10][Opus 7.1][AV1][imdb-tt32890033]-FraMeSToR.mkv"
	minionsDonorName  = "Minions and Monsters 2026 PL.DUB.MD.1080p.AMZN.WEB-DL.XviD-MAXX.mp4"
)

// testJobTimeout is the explicit per-checkpoint engine budget for the
// acceptance runs, matching the analyze integration tests.
//
// It is deliberately much larger than production's derived budget
// ([analyze.DefaultJobTimeout] floors the settled 190 s slice at 120 s): that
// budget is a production policy, and reusing it here would make a loaded
// machine indistinguishable from a broken correlation.
const testJobTimeout = 10 * time.Minute

// requireAcceptanceTools skips (never silently passes) when the host cannot run
// the acceptance chain at all. The sync engine is compiled into the test
// binary now, so only the external tools and the media can be missing.
//
// The skip is the design, not a gap: testfiles/ is tens of GiB of real media
// and is not in the repository, so these suites cannot run on a bare checkout
// or in CI. The numeric gate that *cannot* skip is internal/dsp/correlate's
// golden test over the committed excerpts in internal/dsp/testdata/wav (a few
// MiB, in-repo, no external tool but the Go toolchain) — that is what CI runs,
// and where a port regression must be caught. Never "fix" a skip here by
// deleting that test or its goldens; it is the only branch that always runs.
func requireAcceptanceTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe", "mkvmerge", "mkvextract"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
}

// capture is a concurrency-safe log sink. Secret scans run over everything the
// extract/syncworker chain emitted during the test.
type capture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capture) printf(format string, args ...any) {
	fmt.Fprintf(c, format+"\n", args...)
}

func (c *capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// harness owns the temp tree, data dir, settings and vault writer every
// acceptance path shares.
type harness struct {
	t      *testing.T
	root   string
	data   string
	work   string
	vault  string
	media  string
	donors string
	st     *store.Store
	ss     *settings.Store
	logs   *capture
	writer *vault.Writer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	// Copying the fixtures needs room for the target video plus the mux work
	// copy and temp. Skip instead of failing on hosts whose TMPDIR is a small
	// tmpfs; the assertion value comes from the chain, not the disk size.
	const wantFree = 14 << 30
	var stat syscall.Statfs_t
	if err := syscall.Statfs(root, &stat); err != nil {
		t.Fatalf("statfs %s: %v", root, err)
	}
	if free := int64(stat.Bavail) * int64(stat.Bsize); free < wantFree {
		t.Skipf("temp dir %s has %d GiB free, need %d GiB for fixture copies", root, free>>30, wantFree>>30)
	}

	h := &harness{
		t:      t,
		root:   root,
		data:   filepath.Join(root, "data"),
		work:   filepath.Join(root, "work"),
		vault:  filepath.Join(root, "vault"),
		media:  filepath.Join(root, "library"),
		donors: filepath.Join(root, "donors"),
		logs:   &capture{},
	}
	for _, dir := range []string{h.work, h.media, h.donors, h.data} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	st, err := store.Open(h.data)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h.st = st
	h.ss = settings.New(st.DB)
	if err := h.ss.Seed(context.Background(), h.data); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	storageRaw, _ := json.Marshal(settings.StoragePaths{
		WorkDir: h.work, VaultDir: h.vault, MediaRoots: []string{h.media},
	})
	if err := h.ss.Set(context.Background(), settings.KeyStoragePaths, storageRaw); err != nil {
		t.Fatalf("set storage paths: %v", err)
	}
	writer, err := vault.New(vault.Dependencies{Store: st, Settings: h.ss, DataDir: h.data})
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	h.writer = writer

	// Capture package diagnostics (arrrescan/syncworker log through log.Printf
	// when no Logf is wired) alongside the explicit sinks.
	old := log.Writer()
	log.SetOutput(io.MultiWriter(h.logs, os.Stderr))
	t.Cleanup(func() { log.SetOutput(old) })
	return h
}

// copyFixture copies one read-only fixture into dstDir and returns its path.
//
// The media itself is not in the repository, so a checkout without testfiles/
// skips rather than fails: before the engine moved in-process these suites
// skipped on a missing reference toolchain, and that guard hid the fact that
// the fixtures were the real prerequisite all along. See requireAcceptanceTools
// for why the skip is correct and which gate covers the same numbers in CI.
func (h *harness) copyFixture(t *testing.T, name, dstDir string) string {
	t.Helper()
	src := filepath.Join("..", "..", "..", "testfiles", name)
	if _, err := os.Stat(src); err != nil {
		t.Skipf("media fixtures unavailable (%v); the acceptance suites need testfiles/", err)
	}
	dst := filepath.Join(dstDir, name)
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copy fixture %s: %v", name, err)
	}
	return dst
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// insertInstance stores an arr instance. Disabled instances are the documented
// M5-4 rescan-skip case: the mux publishes, no request is sent.
func (h *harness) insertInstance(t *testing.T, baseURL string, enabled bool) int64 {
	t.Helper()
	flag := 0
	if enabled {
		flag = 1
	}
	res, err := h.st.DB.Exec(`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4, enabled)
		VALUES ('radarr', 'radarr', ?, X'00', '0000', ?)`, baseURL, flag)
	if err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("arr instance id: %v", err)
	}
	return id
}

func (h *harness) insertLibraryRow(t *testing.T, instanceID int64, title string, year int, origLang, targetPath string) int64 {
	t.Helper()
	res, err := h.st.DB.Exec(`INSERT INTO arr_library_cache
		(arr_instance_id, media_type, external_item_id, title, year, original_language_iso2,
		 target_file_path, eligibility_status, matches_target_tags, audio_languages_json)
		VALUES (?, 'movie', 7, ?, ?, ?, ?, 'eligible', 1, '["eng"]')`,
		instanceID, title, year, origLang, targetPath)
	if err != nil {
		t.Fatalf("insert library row: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("library row id: %v", err)
	}
	return id
}

// vaultHook mirrors cmd/dubarr's composition-root adapter: one vault Write per
// staged track sharing a donor group UUID. The writer computes the archive
// SHA-256 itself, so the recorded checksum always matches the bytes on disk.
func (h *harness) vaultHook(t *testing.T) extract.VaultHook {
	t.Helper()
	return func(ctx context.Context, job store.ImportExtractionJob, tracks []extract.StagedTrack) error {
		baseName := job.Title
		if job.Year > 0 {
			baseName = fmt.Sprintf("%s (%d)", job.Title, job.Year)
		}
		for _, track := range tracks {
			if _, err := h.writer.Write(ctx, vault.Request{
				StagedPath:        track.Path,
				BaseName:          baseName,
				Language:          track.Language,
				Media:             store.VaultMediaRecord{MediaType: "movie", Title: job.Title, Year: job.Year, OriginalLanguageISO2: job.OriginalLanguage},
				GroupUUID:         "acceptance-group",
				Variant:           track.Variant,
				Codec:             track.Codec,
				Channels:          track.Channels,
				ChannelCount:      track.ChannelCount,
				DurationMS:        track.Duration.Milliseconds(),
				SourceReleaseName: job.Filename,
			}); err != nil {
				return fmt.Errorf("archive %s: %w", filepath.Base(track.Path), err)
			}
		}
		return nil
	}
}

// vaultTrack is the archived-track projection assertions use.
type vaultTrack struct {
	ID       int64
	LangISO2 string
	Path     string
	SHA256   string
}

func (h *harness) vaultTracks(t *testing.T) []vaultTrack {
	t.Helper()
	rows, err := h.st.DB.Query(`SELECT id, lang_iso2, file_path, sha256 FROM audio_tracks ORDER BY id`)
	if err != nil {
		t.Fatalf("query audio_tracks: %v", err)
	}
	defer rows.Close()
	var out []vaultTrack
	for rows.Next() {
		var tr vaultTrack
		if err := rows.Scan(&tr.ID, &tr.LangISO2, &tr.Path, &tr.SHA256); err != nil {
			t.Fatalf("scan audio_tracks: %v", err)
		}
		out = append(out, tr)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audio_tracks: %v", err)
	}
	return out
}

func (h *harness) trackByLang(t *testing.T, lang string) vaultTrack {
	t.Helper()
	for _, tr := range h.vaultTracks(t) {
		if tr.LangISO2 == lang {
			return tr
		}
	}
	t.Fatalf("no vault track with lang %q", lang)
	return vaultTrack{}
}

// sha256File hashes a file without loading it into memory: the target videos
// are multiple GiB.
func sha256File(t *testing.T, path string) string {
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

// videoStreamHash returns ffmpeg's streamhash of the first video stream. The
// mux contract (ADR-0004) is that this value never changes.
func videoStreamHash(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-i", path,
		"-map", "0:v:0", "-c", "copy", "-f", "streamhash", "-hash", "sha256", "-").Output()
	if err != nil {
		t.Fatalf("ffmpeg streamhash %s: %v", path, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// `0,  2787844839, <hex>, S=2, ...` — stream, size, hash.
		if fields := strings.Split(line, ","); len(fields) >= 3 {
			return strings.TrimSpace(fields[2])
		}
	}
	t.Fatalf("streamhash %s: no SHA256 line in %q", path, out)
	return ""
}

// audioStreamTags lists the target's audio streams as lang/title pairs.
func audioStreamTags(t *testing.T, path string) []string {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a",
		"-show_entries", "stream_tags=language,title", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe audio tags %s: %v", path, err)
	}
	var doc struct {
		Streams []struct {
			Tags struct {
				Language string `json:"language"`
				Title    string `json:"title"`
			} `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode ffprobe tags: %v", err)
	}
	tags := make([]string, 0, len(doc.Streams))
	for _, s := range doc.Streams {
		tags = append(tags, strings.ToLower(s.Tags.Language)+"|"+s.Tags.Title)
	}
	return tags
}

// setImportApproved drives the store seam behind POST /api/import/approve: the
// pending candidate is paired with the donor and matched library row, which
// transitions it to 'extracting' for the extraction worker.
func (h *harness) approveImport(t *testing.T, id, libraryID int64, donor string) {
	t.Helper()
	ctx := context.Background()
	if err := h.st.SetImportCandidateApproval(ctx, id, donor, libraryID); err != nil {
		t.Fatalf("approve import candidate %d: %v", id, err)
	}
}

// assertNoSecrets scans captured sources (logs, HTTP response bodies) for
// credential material.
func assertNoSecrets(t *testing.T, marker string, sources ...string) {
	t.Helper()
	for _, source := range sources {
		for _, needle := range []string{marker, "Authorization:", "X-Api-Key", "api_key="} {
			if needle == "" {
				continue
			}
			if strings.Contains(source, needle) {
				t.Fatalf("captured output leaks credential material %q:\n%s", needle, source)
			}
		}
	}
}

// fakeDecrypter returns a recognisable marker so the secret scan has something
// to look for; the real secrets service is never part of the acceptance tree.
type fakeDecrypter struct{ marker string }

func (f fakeDecrypter) DecryptString([]byte) (string, error) { return f.marker, nil }

// budgetEngine runs the real engine under testJobTimeout per checkpoint.
//
// analyze.Analyze derives the job deadline from the production policy
// ([analyze.DefaultConfig] → 120 s) and passes it down as the job context, and
// syncjob has no seam to override that Config. The acceptance suite therefore
// replaces the inherited deadline with its own budget while still honouring a
// real cancellation of the job. The runner it wraps is built with the same
// budget via sync.WithTimeout so its own default cannot undercut it.
type budgetEngine struct{ inner analyze.Engine }

func (e budgetEngine) Run(ctx context.Context, job syncengine.Job) (syncengine.Result, error) {
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), testJobTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() {
		if errors.Is(ctx.Err(), context.Canceled) {
			cancel()
		}
	})
	defer stop()
	return e.inner.Run(runCtx, job)
}
