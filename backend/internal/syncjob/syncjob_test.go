package syncjob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/reflang"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
	"github.com/Subject412/dubarr/backend/internal/sync"
)

type fixture struct {
	storeDir string
	workDir  string
	db       *store.Store
	settings *settings.Store
	mux      *http.ServeMux
	svc      *Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ss := settings.New(st.DB)
	if err := ss.Seed(context.Background(), filepath.Join(dir, "data")); err != nil {
		t.Fatalf("settings seed: %v", err)
	}
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	f := &fixture{storeDir: filepath.Join(dir, "data"), workDir: work, db: st, settings: ss}
	f.mux = http.NewServeMux()
	f.svc = RegisterRoutes(f.mux, Dependencies{
		DB:           st.DB,
		Settings:     ss,
		RequireAdmin: func(h http.Handler) http.Handler { return h },
		WorkDir:      work,
	})
	return f
}

// insertLibrary creates an arr instance + library row and returns its id.
func insertLibrary(t *testing.T, db *sql.DB, targetPath, eligibility, originalLang string) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		VALUES ('test', 'radarr', 'http://localhost', X'00', '0000')`)
	if err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	instanceID, _ := res.LastInsertId()
	res, err = db.Exec(`INSERT INTO arr_library_cache
		(arr_instance_id, media_type, external_item_id, title, original_language_iso2,
		 target_file_path, eligibility_status, matches_target_tags, audio_languages_json)
		VALUES (?, 'movie', 1, 'Fixture', ?, ?, ?, ?, '["eng"]')`,
		instanceID, originalLang, targetPath, eligibility, eligibility == "eligible")
	if err != nil {
		t.Fatalf("insert library: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func insertTrack(t *testing.T, db *sql.DB, mediaID int64, group, lang, path string) int64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read track fixture: %v", err)
	}
	sum := sha256.Sum256(data)
	res, err := db.Exec(`INSERT INTO audio_tracks
		(vault_media_id, group_uuid, lang_iso2, lang_iso3, variant, file_path, sha256,
		 codec, channels, channel_count)
		VALUES (?, ?, ?, ?, 'dubbing', ?, ?, 'pcm_s16le', '2.0', 2)`,
		mediaID, group, lang, lang, path, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("insert track: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func insertMedia(t *testing.T, st *store.Store) int64 {
	t.Helper()
	id, err := st.EnsureVaultMedia(context.Background(), store.VaultMediaRecord{
		MediaType: "movie", Title: "Fixture", Year: 2020, FolderPath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("EnsureVaultMedia: %v", err)
	}
	return id
}

func postJob(t *testing.T, mux *http.ServeMux, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/sync/jobs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestCreateJobEligibilityGate(t *testing.T) {
	f := newFixture(t)
	mediaID := insertMedia(t, f.db)
	dir := t.TempDir()
	trackPath := filepath.Join(dir, "pl.mka")
	if err := os.WriteFile(trackPath, []byte("wav"), 0o600); err != nil {
		t.Fatal(err)
	}
	trackID := insertTrack(t, f.db.DB, mediaID, "group-1", "pl", trackPath)

	eligible := insertLibrary(t, f.db.DB, filepath.Join(dir, "eligible.mkv"), "eligible", "eng")
	waiting := insertLibrary(t, f.db.DB, filepath.Join(dir, "waiting.mkv"), "waiting_for_tags", "eng")

	body := fmt.Sprintf(`{"vault_track_id":%d,"target_library_cache_id":%d}`, trackID, eligible)
	if rec := postJob(t, f.mux, body); rec.Code != http.StatusCreated {
		t.Fatalf("eligible create = %d %s, want 201", rec.Code, rec.Body)
	}

	body = fmt.Sprintf(`{"vault_track_id":%d,"target_library_cache_id":%d}`, trackID, waiting)
	rec := postJob(t, f.mux, body)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not tag-eligible") {
		t.Fatalf("waiting create = %d %s, want 409 not tag-eligible", rec.Code, rec.Body)
	}

	body = fmt.Sprintf(`{"vault_track_id":%d,"target_library_cache_id":%d,"force_ignore_tag_filter":true}`, trackID, waiting)
	rec = postJob(t, f.mux, body)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "force override disabled") {
		t.Fatalf("force off create = %d %s, want 409 force override disabled", rec.Code, rec.Body)
	}

	// Turning the setting on honours force for an otherwise ineligible row.
	raw, err := json.Marshal(settings.SecurityOptions{AllowForceTagOverride: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.settings.Set(context.Background(), settings.KeySecurityOptions, raw); err != nil {
		t.Fatalf("enable force: %v", err)
	}
	if rec := postJob(t, f.mux, body); rec.Code != http.StatusCreated {
		t.Fatalf("force on create = %d %s, want 201", rec.Code, rec.Body)
	}

	// Force overrides the tag filter only: a missing target file must still
	// be rejected as a conflict instead of reaching the store and 500ing.
	missing := insertLibrary(t, f.db.DB, "", "eligible", "eng")
	body = fmt.Sprintf(`{"vault_track_id":%d,"target_library_cache_id":%d,"force_ignore_tag_filter":true}`, trackID, missing)
	rec = postJob(t, f.mux, body)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "target file is missing") {
		t.Fatalf("force on missing target = %d %s, want 409 target file is missing", rec.Code, rec.Body)
	}
}

// TestTrackAudioTrackCommentaryNeverSelected guards the vault -> probe mapping:
// a commentary variant must carry Disposition.Comment so reflang never picks it
// as a reference donor, even when it is the highest-quality sibling.
func TestTrackAudioTrackCommentaryNeverSelected(t *testing.T) {
	job := store.VaultTrackRecord{ID: 1, LangISO2: "pl", ChannelCount: 2, Variant: "dubbing"}
	commentary := store.VaultTrackRecord{ID: 2, LangISO2: "en", ChannelCount: 6, Variant: " Commentary "}
	dub := store.VaultTrackRecord{ID: 3, LangISO2: "en", ChannelCount: 2, Variant: "dubbing"}

	if got := trackAudioTrack(commentary, 0); !got.Disposition.Comment {
		t.Fatalf("commentary variant mapped to Disposition.Comment = false, want true")
	}
	if got := trackAudioTrack(dub, 0); got.Disposition.Comment {
		t.Fatalf("dubbing variant mapped to Disposition.Comment = true, want false")
	}

	vaultTracks := []probe.AudioTrack{
		trackAudioTrack(job, 0),
		trackAudioTrack(commentary, 1),
		trackAudioTrack(dub, 2),
	}
	targetTracks := []probe.AudioTrack{{Index: 1, Channels: 2, Language: "eng", LanguageISO1: "en"}}
	resolved := reflang.Resolve(targetTracks, vaultTracks, reflang.Languages{Targets: []string{"pl"}}, "")
	if len(resolved.Pairs) != 1 {
		t.Fatalf("pairs = %d (%s), want 1 en reference", len(resolved.Pairs), resolved.Reason)
	}
	if got := resolved.Pairs[0].Vault.Index; got != 2 {
		t.Fatalf("donor vault index = %d, want 2 (dubbing sibling); commentary sibling selected", got)
	}
}

func TestListAndGetJobs(t *testing.T) {
	f := newFixture(t)
	mediaID := insertMedia(t, f.db)
	dir := t.TempDir()
	trackPath := filepath.Join(dir, "pl.mka")
	if err := os.WriteFile(trackPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	trackID := insertTrack(t, f.db.DB, mediaID, "g", "pl", trackPath)
	libID := insertLibrary(t, f.db.DB, filepath.Join(dir, "t.mkv"), "eligible", "eng")

	body := fmt.Sprintf(`{"vault_track_id":%d,"target_library_cache_id":%d}`, trackID, libID)
	first := postJob(t, f.mux, body)
	second := postJob(t, f.mux, body)
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("create codes = %d/%d", first.Code, second.Code)
	}
	var firstJob, secondJob JobInfo
	_ = json.Unmarshal(first.Body.Bytes(), &firstJob)
	_ = json.Unmarshal(second.Body.Bytes(), &secondJob)

	req := httptest.NewRequest(http.MethodGet, "/api/sync/jobs", nil)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	var list struct {
		Items []JobInfo `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Items) != 2 || list.Items[0].ID != secondJob.ID || list.Items[1].ID != firstJob.ID {
		t.Fatalf("list order = %+v, want newest (%d) first", list.Items, secondJob.ID)
	}
	if list.Items[0].Status != StatusQueued {
		t.Fatalf("new job status = %q, want queued", list.Items[0].Status)
	}

	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/sync/jobs/%d", firstJob.ID), nil)
	rec = httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"queued"`) {
		t.Fatalf("get = %d %s", rec.Code, rec.Body)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/sync/jobs/9999", nil)
	rec = httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown get = %d, want 404", rec.Code)
	}

	// A nil RequireAdmin must fail closed with 503.
	unwired := http.NewServeMux()
	RegisterRoutes(unwired, Dependencies{DB: f.db.DB, Settings: f.settings, WorkDir: f.workDir})
	rec = httptest.NewRecorder()
	unwired.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sync/jobs", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired list = %d, want 503", rec.Code)
	}
}

// --- integration with the real Rust engine --------------------------------

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

func TestExecuteEndToEnd(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe", "mkvmerge"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	f := newFixture(t)
	dir := t.TempDir()
	const sampleRate = 8000
	base := noise16(30*sampleRate, 21)
	donor := shifted(base, 1200*sampleRate/1000)

	baseWAV := filepath.Join(dir, "base.wav")
	donorWAV := filepath.Join(dir, "donor.wav")
	writePCM16WAV(t, baseWAV, base, sampleRate)
	writePCM16WAV(t, donorWAV, donor, sampleRate)

	targetMKV := filepath.Join(dir, "target.mkv")
	cmd := exec.Command("mkvmerge", "-o", targetMKV, "--quiet", "--language", "0:eng", baseWAV)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mkvmerge target: %v\n%s", err, out)
	}

	// Vault: target-language pl track (not the reference) + eng sibling.
	plPath := filepath.Join(dir, "track.pl.mka")
	engPath := filepath.Join(dir, "track.eng.mka")
	if err := os.WriteFile(plPath, mustRead(t, baseWAV), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(engPath, mustRead(t, donorWAV), 0o600); err != nil {
		t.Fatal(err)
	}
	mediaID := insertMedia(t, f.db)
	plID := insertTrack(t, f.db.DB, mediaID, "group-x", "pl", plPath)
	insertTrack(t, f.db.DB, mediaID, "group-x", "en", engPath)
	libID := insertLibrary(t, f.db.DB, targetMKV, "eligible", "eng")

	rec := postJob(t, f.mux, fmt.Sprintf(`{"vault_track_id":%d,"target_library_cache_id":%d}`, plID, libID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var job JobInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job: %v", err)
	}

	f.svc.engine = sync.New()
	// The job path must probe each analysis input exactly once, before any
	// decode, and never per checkpoint (six checkpoints here).
	prober := probe.New(probe.Dependencies{})
	timingProbes := 0
	f.svc.probeTimings = func(ctx context.Context, path string) (probe.MediaInfo, error) {
		timingProbes++
		info, err := prober.Probe(ctx, path)
		if err != nil {
			return probe.MediaInfo{}, err
		}
		return *info, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	report, err := f.svc.Execute(ctx, job.ID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if timingProbes != 2 {
		t.Errorf("source timing probes = %d, want 2 (one per analysis input)", timingProbes)
	}
	if report.Verdict != "ok" || report.SyncMethod == "" {
		t.Fatalf("report verdict/method = %q/%q", report.Verdict, report.SyncMethod)
	}
	for i, off := range report.OffsetsMS {
		if math.Abs(off-1200) > 10 {
			t.Fatalf("checkpoint %d offset = %v, want ~1200", i, off)
		}
	}

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/sync/jobs/%d", job.ID), nil)
	got := httptest.NewRecorder()
	f.mux.ServeHTTP(got, req)
	if got.Code != http.StatusOK {
		t.Fatalf("status get = %d %s", got.Code, got.Body)
	}
	var final JobInfo
	if err := json.Unmarshal(got.Body.Bytes(), &final); err != nil {
		t.Fatalf("decode final: %v", err)
	}
	if final.Status != StatusDone || final.Report == nil {
		t.Fatalf("final status/report = %q/%v, want done + report", final.Status, final.Report)
	}
	if final.Report.Verdict != "ok" || final.CalculatedOffsetMS < 1100 || final.CalculatedOffsetMS > 1300 {
		t.Fatalf("persisted report = %+v", final.Report)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// A languages.policy row written before prefer_original_language existed must
// resolve as true: the resolver's Priority 1 must not silently switch off on
// upgraded installs.
func TestAnalysisPolicyDefaultsLegacyPreferOriginal(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	legacy := `{"target_import_languages":["pl"],"extra_reference_languages":["en"],"original_language_from_arr":true,"exclude_commentary":true,"export_mode":"rule_based"}`
	if _, err := f.db.DB.ExecContext(ctx,
		`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`,
		settings.KeyLanguagesPolicy, legacy); err != nil {
		t.Fatalf("insert legacy languages.policy: %v", err)
	}
	policy, _ := f.svc.analysisPolicy(ctx)
	if !policy.PreferOriginalLanguage {
		t.Error("analysisPolicy: PreferOriginalLanguage = false for a legacy row, want true")
	}
}

// The setting must reach the engine's config through the same block that copies
// the analysis windows. A row without the key (every row written before
// U-DSP.8) means one worker, not zero.
func TestAnalysisPolicyParallelCheckpoints(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, cfg := f.svc.analysisPolicy(ctx); cfg.ParallelCheckpoints != 1 {
		t.Fatalf("seeded parallel_checkpoints = %d, want 1", cfg.ParallelCheckpoints)
	}

	legacy := `{"max_offset_sec":180,"checkpoint_positions_pct":[10,25,40,55,70,85],` +
		`"checkpoint_window_sec":150,"preview_positions_pct":[10,25,50,75,90],"preview_window_sec":60}`
	if _, err := f.db.DB.ExecContext(ctx,
		`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`,
		settings.KeySyncDefaults, legacy); err != nil {
		t.Fatalf("insert legacy sync.defaults: %v", err)
	}
	_, cfg := f.svc.analysisPolicy(ctx)
	if cfg.ParallelCheckpoints != 1 {
		t.Errorf("legacy row parallel_checkpoints = %d, want 1", cfg.ParallelCheckpoints)
	}
	if cfg.CheckpointWindowSec != 150 {
		t.Errorf("legacy row lost the window setting: %d", cfg.CheckpointWindowSec)
	}

	stored := `{"max_offset_sec":180,"checkpoint_positions_pct":[10,25,40,55,70,85],` +
		`"checkpoint_window_sec":150,"preview_positions_pct":[10,25,50,75,90],` +
		`"preview_window_sec":60,"parallel_checkpoints":5}`
	if _, err := f.db.DB.ExecContext(ctx,
		`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`,
		settings.KeySyncDefaults, stored); err != nil {
		t.Fatalf("insert stored sync.defaults: %v", err)
	}
	_, cfg = f.svc.analysisPolicy(ctx)
	if cfg.ParallelCheckpoints != 5 {
		t.Errorf("stored parallel_checkpoints = %d, want 5", cfg.ParallelCheckpoints)
	}
}

// analysis_source must reach analyze.Config through the same block that copies
// the windows, and every shape a row can have must resolve to a real mode: ""
// (and an absent key) is the compiled default, never "no mode at all". The
// switch is defence in depth for a row that skipped serveable - a direct
// Store.Get or an imported backup - which is exactly what these inserts are.
func TestAnalysisPolicyAnalysisSource(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, cfg := f.svc.analysisPolicy(ctx); cfg.AnalysisSource != settings.DefaultAnalysisSource {
		t.Fatalf("seeded analysis_source = %q, want %q", cfg.AnalysisSource, settings.DefaultAnalysisSource)
	}

	cases := []struct {
		name string
		row  string
		want string
	}{
		{"absent", `{}`, settings.DefaultAnalysisSource},
		{"empty", `{"analysis_source":""}`, settings.DefaultAnalysisSource},
		{"windows", `{"analysis_source":"windows"}`, analyze.AnalysisSourceWindows},
		{"full", `{"analysis_source":"full"}`, analyze.AnalysisSourceFull},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.db.DB.ExecContext(ctx,
				`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`,
				settings.KeySyncDefaults, tc.row); err != nil {
				t.Fatalf("insert sync.defaults %s: %v", tc.row, err)
			}
			_, cfg := f.svc.analysisPolicy(ctx)
			if cfg.AnalysisSource != tc.want {
				t.Errorf("cfg.AnalysisSource = %q, want %q for row %s", cfg.AnalysisSource, tc.want, tc.row)
			}
		})
	}
}

// The sync.strategy section reaches analyze.Config unchanged when it is valid,
// and never fails a job when it is not: a settings row is not a reason to stop
// syncing.

func TestAnalysisPolicyReadsStrategy(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	stored := `{"preset":"custom","techniques":[` +
		`{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5},` +
		`{"id":"mfcc","enabled":true,"when":"below_confidence","min_confidence":0.2,"min_match_z":8}]}`
	if _, err := f.db.DB.ExecContext(ctx,
		`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`,
		settings.KeySyncStrategy, stored); err != nil {
		t.Fatalf("insert sync.strategy: %v", err)
	}

	_, cfg := f.svc.analysisPolicy(ctx)
	want := []analyze.Technique{
		{ID: analyze.TechniqueWaveform, Enabled: true, When: analyze.WhenAlways, MinConfidence: 0.1, MinMatchZ: 5},
		{ID: analyze.TechniqueMFCC, Enabled: true, When: analyze.WhenBelowConfidence, MinConfidence: 0.2, MinMatchZ: 8},
	}
	if !reflect.DeepEqual(cfg.Strategy.Techniques, want) {
		t.Fatalf("cfg.Strategy.Techniques = %+v, want %+v", cfg.Strategy.Techniques, want)
	}
	if cfg.Strategy.Preset != analyze.PresetCustom {
		t.Fatalf("cfg.Strategy.Preset = %q, want %q", cfg.Strategy.Preset, analyze.PresetCustom)
	}
}

// TestAnalysisPolicyFallsBackToDefaultOnGarbage: an unparseable row, or one
// naming only techniques this build does not know, resolves to the shipped
// default. An unknown id among valid ones is dropped and the rest kept — the
// pipeline is never empty.
func TestAnalysisPolicyFallsBackToDefaultOnGarbage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cases := []struct{ name, row string }{
		{"not an object", `"garbage"`},
		{"truncated json", `{"preset":"balanced","techniques":[`},
		{"only unknown techniques", `{"preset":"balanced","techniques":[{"id":"bogus","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5}]}`},
		{"an empty list", `{"preset":"balanced","techniques":[]}`},
		{"a list with no enabled entry", `{"preset":"custom","techniques":[{"id":"waveform","enabled":false,"when":"always","min_confidence":0.1,"min_match_z":5}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.db.DB.ExecContext(ctx,
				`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`,
				settings.KeySyncStrategy, tc.row); err != nil {
				t.Fatalf("insert sync.strategy %s: %v", tc.row, err)
			}
			_, cfg := f.svc.analysisPolicy(ctx)
			if !reflect.DeepEqual(cfg.Strategy, analyze.DefaultStrategy()) {
				t.Fatalf("cfg.Strategy = %+v for row %s, want the shipped default", cfg.Strategy, tc.row)
			}
		})
	}
}

// TestAnalysisPolicyDropsAnUnknownTechniqueOnly: the bad entry goes, the good
// one stays, so a strategy naming a matcher this build lacks still runs.
func TestAnalysisPolicyDropsAnUnknownTechniqueOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	row := `{"preset":"custom","techniques":[` +
		`{"id":"bogus","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5},` +
		`{"id":"waveform","enabled":true,"when":"always","min_confidence":0.12,"min_match_z":5}]}`
	if _, err := f.db.DB.ExecContext(ctx,
		`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`,
		settings.KeySyncStrategy, row); err != nil {
		t.Fatalf("insert sync.strategy: %v", err)
	}
	_, cfg := f.svc.analysisPolicy(ctx)
	if len(cfg.Strategy.Techniques) != 1 {
		t.Fatalf("cfg.Strategy.Techniques = %+v, want only the known entry", cfg.Strategy.Techniques)
	}
	if got := cfg.Strategy.Techniques[0]; got.ID != analyze.TechniqueWaveform || got.MinConfidence != 0.12 {
		t.Fatalf("kept entry = %+v, want the waveform entry with its own threshold", got)
	}
}

// TestAnalysisPolicySeededStrategyIsTheDefault: on a fresh install the seeded
// section reaches the config unchanged, which is what keeps 0.5.0's offsets
// identical to 0.4.0's.
func TestAnalysisPolicySeededStrategyIsTheDefault(t *testing.T) {
	f := newFixture(t)
	_, cfg := f.svc.analysisPolicy(context.Background())
	if !reflect.DeepEqual(cfg.Strategy, analyze.DefaultStrategy()) {
		t.Fatalf("seeded cfg.Strategy = %+v, want %+v", cfg.Strategy, analyze.DefaultStrategy())
	}
}
