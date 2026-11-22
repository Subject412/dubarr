package vault

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

type harness struct {
	t        *testing.T
	ctx      context.Context
	db       *store.Store
	settings *settings.Store
	writer   *Writer
	data     string
	vault    string
	work     string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
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
	h := &harness{
		t:        t,
		ctx:      context.Background(),
		db:       st,
		settings: settingsStore,
		data:     dir,
		vault:    filepath.Join(dir, "vault"),
		work:     filepath.Join(dir, "work"),
	}
	h.setPaths(h.vault, h.work)

	w, err := New(Dependencies{Store: st, Settings: settingsStore, DataDir: dir})
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	h.writer = w
	return h
}

func (h *harness) setPaths(vaultDir, workDir string) {
	h.t.Helper()
	raw, err := json.Marshal(settings.StoragePaths{VaultDir: vaultDir, WorkDir: workDir})
	if err != nil {
		h.t.Fatalf("marshal storage.paths: %v", err)
	}
	if err := h.settings.Set(h.ctx, settings.KeyStoragePaths, raw); err != nil {
		h.t.Fatalf("set storage.paths: %v", err)
	}
}

// stage writes content to WORK_DIR/extract/<id>/<name> and returns the path.
func (h *harness) stage(id int64, name string, content []byte) string {
	h.t.Helper()
	dir := filepath.Join(h.work, "extract", formatID(id))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Fatalf("create staging: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		h.t.Fatalf("write staged: %v", err)
	}
	return path
}

func formatID(id int64) string {
	const digits = "0123456789"
	if id == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for id > 0 {
		i--
		buf[i] = digits[id%10]
		id /= 10
	}
	return string(buf[i:])
}

func movieMedia() store.VaultMediaRecord {
	return store.VaultMediaRecord{
		MediaType:            "movie",
		Title:                "Spirited Away",
		Year:                 2001,
		TMDBID:               129,
		OriginalLanguageISO2: "ja",
	}
}

func baseRequest(staged string) Request {
	return Request{
		StagedPath:   staged,
		BaseName:     "Spirited Away (2001)",
		Language:     "pol",
		Media:        movieMedia(),
		Codec:        "eac3",
		Channels:     "5.1",
		ChannelCount: 6,
		SampleRate:   48000,
		BitrateBPS:   640000,
		DurationMS:   11620,
	}
}

func TestWriteArchivesAtomicallyAndRecords(t *testing.T) {
	h := newHarness(t)
	content := bytes.Repeat([]byte("dubarr-vault-payload\x00"), 256)
	staged := h.stage(1, "Donor 2021 PL.DUB.pl.2.mka", content)

	entry, err := h.writer.Write(h.ctx, baseRequest(staged))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if entry == nil {
		t.Fatal("Write returned nil entry")
	}
	wantPath := filepath.Join(h.vault, "Spirited Away (2001).pl.mka")
	if entry.Path != wantPath {
		t.Errorf("entry.Path = %q, want %q", entry.Path, wantPath)
	}
	wantSum := sha256.Sum256(content)
	wantDigest := hex.EncodeToString(wantSum[:])
	if entry.SHA256 != wantDigest {
		t.Errorf("entry.SHA256 = %q, want %q", entry.SHA256, wantDigest)
	}
	if entry.Size != int64(len(content)) {
		t.Errorf("entry.Size = %d, want %d", entry.Size, len(content))
	}
	if entry.LanguageISO2 != "pl" || entry.LanguageISO3 != "pol" {
		t.Errorf("entry languages = (%q, %q), want (pl, pol)", entry.LanguageISO2, entry.LanguageISO3)
	}
	if entry.ID == 0 {
		t.Error("entry.ID = 0, want the audio_tracks id")
	}

	info, err := os.Stat(entry.Path)
	if err != nil {
		t.Fatalf("stat vault file: %v", err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Errorf("vault file mode = %04o, want 0444", info.Mode().Perm())
	}
	got, err := os.ReadFile(entry.Path)
	if err != nil {
		t.Fatalf("read vault file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Error("vault content differs from staged content")
	}

	// Database: one media row and one track row with the archived metadata.
	var mediaCount int
	if err := h.db.DB.QueryRow(`SELECT count(*) FROM vault_media`).Scan(&mediaCount); err != nil {
		t.Fatalf("count vault_media: %v", err)
	}
	if mediaCount != 1 {
		t.Errorf("vault_media rows = %d, want 1", mediaCount)
	}
	var (
		filePath, sum, iso2, iso3, variant, codec, channels string
		channelCount, mediaID                               int
	)
	if err := h.db.DB.QueryRow(
		`SELECT vault_media_id, file_path, sha256, lang_iso2, lang_iso3, variant, codec, channels, channel_count
		 FROM audio_tracks WHERE id = ?`, entry.ID,
	).Scan(&mediaID, &filePath, &sum, &iso2, &iso3, &variant, &codec, &channels, &channelCount); err != nil {
		t.Fatalf("read audio_tracks: %v", err)
	}
	if filePath != wantPath || sum != wantDigest || iso2 != "pl" || iso3 != "pol" ||
		variant != "standard" || codec != "eac3" || channels != "5.1" || channelCount != 6 {
		t.Errorf("audio_tracks row = (%q,%q,%q,%q,%q,%q,%q,%d), unexpected",
			filePath, sum, iso2, iso3, variant, codec, channels, channelCount)
	}
	if mediaID == 0 {
		t.Error("vault_media_id = 0, want the ensured media row")
	}

	// Zero staging leftovers and no temp files in the vault.
	if _, err := os.Stat(staged); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("staged file still present (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Dir(staged)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("empty import directory still present (stat err = %v)", err)
	}
	assertNoTemps(t, h.vault)
}

func TestWriteRefusesOverwrite(t *testing.T) {
	h := newHarness(t)
	original := []byte("original vault payload")
	staged1 := h.stage(1, "donor.pl.1.mka", original)
	first, err := h.writer.Write(h.ctx, baseRequest(staged1))
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}

	replacement := []byte("this must never replace the archive")
	staged2 := h.stage(2, "donor2.pl.1.mka", replacement)
	entry, err := h.writer.Write(h.ctx, baseRequest(staged2))
	if entry != nil {
		t.Errorf("second Write entry = %+v, want nil", entry)
	}
	if !errors.Is(err, ErrCollision) {
		t.Fatalf("second Write error = %v, want ErrCollision", err)
	}
	if !errors.Is(err, fs.ErrExist) {
		t.Errorf("collision error does not match fs.ErrExist: %v", err)
	}

	got, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatalf("read vault file: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Error("vault content was overwritten")
	}
	if _, err := os.Stat(staged2); err != nil {
		t.Errorf("refused staged file should be left in place: %v", err)
	}
	var tracks int
	if err := h.db.DB.QueryRow(`SELECT count(*) FROM audio_tracks`).Scan(&tracks); err != nil {
		t.Fatalf("count audio_tracks: %v", err)
	}
	if tracks != 1 {
		t.Errorf("audio_tracks rows = %d, want 1", tracks)
	}
	assertNoTemps(t, h.vault)
}

func TestWriteRenameFailureLeavesVaultClean(t *testing.T) {
	h := newHarness(t)
	staged := h.stage(1, "donor.pl.1.mka", []byte("payload that never lands"))
	h.writer.renameFile = func(oldPath, newPath string) error {
		return errors.New("injected rename failure")
	}

	entry, err := h.writer.Write(h.ctx, baseRequest(staged))
	if entry != nil {
		t.Errorf("entry = %+v, want nil", entry)
	}
	if err == nil || !strings.Contains(err.Error(), "injected rename failure") {
		t.Fatalf("error = %v, want rename failure", err)
	}
	finalPath := filepath.Join(h.vault, "Spirited Away (2001).pl.mka")
	if _, statErr := os.Stat(finalPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("final file exists after rename failure (stat err = %v)", statErr)
	}
	if _, statErr := os.Stat(staged); statErr != nil {
		t.Errorf("staged file removed on pre-rename failure: %v", statErr)
	}
	var tracks, media int
	if err := h.db.DB.QueryRow(`SELECT count(*) FROM audio_tracks`).Scan(&tracks); err != nil {
		t.Fatalf("count audio_tracks: %v", err)
	}
	if err := h.db.DB.QueryRow(`SELECT count(*) FROM vault_media`).Scan(&media); err != nil {
		t.Fatalf("count vault_media: %v", err)
	}
	if tracks != 0 || media != 0 {
		t.Errorf("database rows after failure = (%d tracks, %d media), want zero", tracks, media)
	}
	assertNoTemps(t, h.vault)
}

// failingRecorder lets a test fail the database record after the rename.
type failingRecorder struct {
	Recorder
}

func (f failingRecorder) RecordVaultTrack(context.Context, store.VaultTrackRecord) (int64, error) {
	return 0, errors.New("injected database failure")
}

func TestWriteDatabaseFailureReportsDurableFile(t *testing.T) {
	h := newHarness(t)
	staged := h.stage(1, "donor.pl.1.mka", []byte("durable payload"))
	writer, err := New(Dependencies{
		Store:    failingRecorder{Recorder: h.db},
		Settings: h.settings,
		DataDir:  h.data,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	entry, err := writer.Write(h.ctx, baseRequest(staged))
	if entry != nil {
		t.Errorf("entry = %+v, want nil", entry)
	}
	if err == nil || !strings.Contains(err.Error(), "injected database failure") {
		t.Fatalf("error = %v, want the database failure", err)
	}
	// The post-rename failure must name the durable file and its digest.
	wantPath := filepath.Join(h.vault, "Spirited Away (2001).pl.mka")
	sum := sha256.Sum256([]byte("durable payload"))
	if !strings.Contains(err.Error(), wantPath) || !strings.Contains(err.Error(), hex.EncodeToString(sum[:])) {
		t.Errorf("error %q does not report the durable file and sha256", err)
	}
	info, statErr := os.Stat(wantPath)
	if statErr != nil {
		t.Fatalf("vault file missing after post-rename failure: %v", statErr)
	}
	if info.Mode().Perm() != 0o444 {
		t.Errorf("vault file mode = %04o, want 0444", info.Mode().Perm())
	}
	if _, statErr := os.Stat(staged); statErr != nil {
		t.Errorf("staged file removed despite failed database record: %v", statErr)
	}
	assertNoTemps(t, h.vault)
}

func TestWriteRejectsBadRequestsWithoutTouchingVault(t *testing.T) {
	h := newHarness(t)
	staged := h.stage(1, "donor.pl.1.mka", []byte("payload"))

	cases := []struct {
		name   string
		mutate func(*Request)
	}{
		{"empty base", func(r *Request) { r.BaseName = "" }},
		{"path separator in base", func(r *Request) { r.BaseName = "../escape" }},
		{"missing language", func(r *Request) { r.Language = "" }},
		{"untagged language", func(r *Request) { r.Language = "und" }},
		{"bad media type", func(r *Request) { r.Media.MediaType = "episode" }},
		{"missing codec", func(r *Request) { r.Codec = "" }},
		{"zero channels", func(r *Request) { r.ChannelCount = 0 }},
		{"bad variant", func(r *Request) { r.Variant = "trailer" }},
	}
	for _, tc := range cases {
		req := baseRequest(staged)
		tc.mutate(&req)
		entry, err := h.writer.Write(h.ctx, req)
		if entry != nil || !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: Write = (%+v, %v), want (nil, ErrInvalidRequest)", tc.name, entry, err)
		}
	}
	if _, err := os.Stat(staged); err != nil {
		t.Errorf("staged file touched by rejected request: %v", err)
	}
	assertNoTemps(t, h.vault)
}

func TestWriteSharedMediaAndStagingDirectoryLifecycle(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(h.work, "extract", "7")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create staging: %v", err)
	}
	plPath := filepath.Join(dir, "donor.pl.1.mka")
	enPath := filepath.Join(dir, "donor.en.2.mka")
	if err := os.WriteFile(plPath, []byte("polish dub"), 0o600); err != nil {
		t.Fatalf("write pl: %v", err)
	}
	if err := os.WriteFile(enPath, []byte("english reference"), 0o600); err != nil {
		t.Fatalf("write en: %v", err)
	}

	plReq := baseRequest(plPath)
	plReq.GroupUUID = "group-1"
	if _, err := h.writer.Write(h.ctx, plReq); err != nil {
		t.Fatalf("write pl: %v", err)
	}
	// A sibling track means the import directory must survive.
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("import directory removed while a sibling track remained: %v", err)
	}

	enReq := baseRequest(enPath)
	enReq.Language = "eng"
	enReq.GroupUUID = "group-1"
	enReq.Variant = "original"
	if _, err := h.writer.Write(h.ctx, enReq); err != nil {
		t.Fatalf("write en: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("import directory still present after last track (stat err = %v)", err)
	}

	var media, tracks int
	if err := h.db.DB.QueryRow(`SELECT count(*) FROM vault_media`).Scan(&media); err != nil {
		t.Fatalf("count vault_media: %v", err)
	}
	if err := h.db.DB.QueryRow(`SELECT count(*) FROM audio_tracks`).Scan(&tracks); err != nil {
		t.Fatalf("count audio_tracks: %v", err)
	}
	if media != 1 || tracks != 2 {
		t.Errorf("rows = (%d media, %d tracks), want (1, 2)", media, tracks)
	}
}

// TestWriteRealMatroskaIntegration archives a real .mka built with mkvmerge.
// The pipeline itself never parses the container, so this proves the byte
// pipeline end to end without ffmpeg.
func TestWriteRealMatroskaIntegration(t *testing.T) {
	mkvmerge, err := exec.LookPath("mkvmerge")
	if err != nil {
		t.Skipf("mkvmerge not on PATH: %v", err)
	}
	h := newHarness(t)
	mka := filepath.Join(t.TempDir(), "track.mka")

	// A 0.1 s mono 16-bit 8 kHz PCM WAV is a valid .mka payload without ffmpeg.
	const (
		sampleRate = 8000
		samples    = 800
		dataLen    = samples * 2
	)
	wav := filepath.Join(t.TempDir(), "track.wav")
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(36+dataLen))
	buf.WriteString("WAVEfmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sampleRate*2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(dataLen))
	buf.Write(make([]byte, dataLen))
	if err := os.WriteFile(wav, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write wav: %v", err)
	}
	if outp, err := exec.Command(mkvmerge, "-o", mka, "--language", "0:pol", wav).CombinedOutput(); err != nil {
		t.Fatalf("mkvmerge: %v: %s", err, outp)
	}

	staged := h.stage(3, "real.pl.1.mka", nil)
	if err := os.Remove(staged); err != nil {
		t.Fatalf("clear placeholder: %v", err)
	}
	if err := os.Rename(mka, staged); err != nil {
		t.Fatalf("move real mka into staging: %v", err)
	}
	raw, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("read staged mka: %v", err)
	}

	req := baseRequest(staged)
	req.Codec = "pcm_s16le"
	req.Channels = "1.0"
	req.ChannelCount = 1
	entry, err := h.writer.Write(h.ctx, req)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	sum := sha256.Sum256(raw)
	if entry.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %q, want digest of the real .mka", entry.SHA256)
	}
	if !strings.HasSuffix(entry.Path, ".pl.mka") {
		t.Errorf("final name = %q, want a .pl.mka suffix", entry.Path)
	}
	if _, err := os.Stat(entry.Path); err != nil {
		t.Errorf("archived real .mka missing: %v", err)
	}
}

func assertNoTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("read vault dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".dubarr-") || strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("temp file left in vault: %s", entry.Name())
		}
	}
}
