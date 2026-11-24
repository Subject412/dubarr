//go:build syncmeasure

// Measurement harness for issue #22/#23 evidence. It drives the same
// acceptance fixture and the same production seams as TestShrekEndToEnd, but
// invokes every stage individually so each step's wall time, child peak RSS
// (getrusage maxrss) and the {WORK_DIR}/jobs/{id} disk footprint can be
// recorded. It is behind the `syncmeasure` build tag so normal `go test ./...`
// never pays for it.
//
// Run:
//
//	cd backend && go test -tags syncmeasure -run TestMeasureSyncResources \
//	  -v -count=1 -timeout 60m ./internal/acceptance
package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/extract"
	"github.com/Subject412/dubarr/backend/internal/gate"
	"github.com/Subject412/dubarr/backend/internal/mux"
	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/reflang"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
	syncengine "github.com/Subject412/dubarr/backend/internal/sync"
	"github.com/Subject412/dubarr/backend/internal/workdir"
)

type measureRow struct {
	Step   string
	Wall   time.Duration
	MaxRSS int64 // KiB from getrusage(RUSAGE_CHILDREN of the child); 0 unknown
	OK     bool
	Detail string
}

type recorder struct {
	t    *testing.T
	mu   sync.Mutex
	rows []measureRow
}

func (r *recorder) record(step string, wall time.Duration, maxRSS int64, ok bool, detail string) {
	r.mu.Lock()
	r.rows = append(r.rows, measureRow{Step: step, Wall: wall, MaxRSS: maxRSS, OK: ok, Detail: detail})
	r.mu.Unlock()
	line := fmt.Sprintf("STEP|%s|wall_ms=%d|maxrss_kb=%d|ok=%t", step, wall.Milliseconds(), maxRSS, ok)
	if detail != "" {
		line += "|" + detail
	}
	r.t.Log(line)
}

// runCommand executes one external tool and records its wall time and the
// child's peak RSS (Linux: ru_maxrss in KiB).
func (r *recorder) runCommand(ctx context.Context, step, name string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	start := time.Now()
	err := cmd.Run()
	wall := time.Since(start)
	var maxRSS int64
	if cmd.ProcessState != nil {
		if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
			maxRSS = ru.Maxrss
		}
	}
	detail := ""
	if err != nil {
		detail = "error=" + strings.ReplaceAll(strings.TrimSpace(errOut.String()), "|", " ")
	}
	r.record(step, wall, maxRSS, err == nil, detail)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, errOut.String())
	}
	return out.Bytes(), nil
}

// measuredEngine is the production JSON-over-stdio contract with timing and RSS
// capture, so each of the six engine spawns is measured individually.
type measuredEngine struct {
	bin string
	rec *recorder
	n   int
}

func (e *measuredEngine) Run(ctx context.Context, job syncengine.Job) (syncengine.Result, error) {
	payload, err := json.Marshal(job)
	if err != nil {
		return syncengine.Result{}, err
	}
	e.n++
	step := fmt.Sprintf("engine_spawn_%d", e.n)
	out, err := e.rec.runCommand(ctx, step, e.bin, payload)
	if err != nil {
		return syncengine.Result{}, err
	}
	var result syncengine.Result
	if err := json.Unmarshal(bytes.TrimSpace(out), &result); err != nil {
		return syncengine.Result{}, fmt.Errorf("decode engine result: %w", err)
	}
	return result, nil
}

// decodeAudio mirrors syncjob.decodeAudio exactly (same ffmpeg argv), measured.
func (r *recorder) decodeAudio(ctx context.Context, step, ffmpeg, input string, stream int, filter, output string) error {
	args := []string{
		"-v", "error", "-nostdin", "-y",
		"-i", input,
		"-map", "0:" + strconv.Itoa(stream),
		"-ac", "1", "-ar", "8000",
		"-af", filter,
		"-c:a", "pcm_f32le",
		output,
	}
	_, err := r.runCommand(ctx, step, ffmpeg, nil, args...)
	return err
}

type dirUsage struct {
	files int
	bytes int64
	names []string
}

func measureDir(t *testing.T, dir string) dirUsage {
	t.Helper()
	var u dirUsage
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		u.files++
		u.bytes += info.Size()
		u.names = append(u.names, fmt.Sprintf("%s=%d", filepath.Base(path), info.Size()))
		return nil
	})
	if err != nil {
		t.Fatalf("measure %s: %v", dir, err)
	}
	sort.Strings(u.names)
	return u
}

func analysisFilter(method reflang.Method) string {
	if method == reflang.LowpassFallback {
		return "highpass=f=30,lowpass=f=200"
	}
	return "highpass=f=80,lowpass=f=3500"
}

func trackAudioTrack(rec store.VaultTrackRecord, index int) probe.AudioTrack {
	return probe.AudioTrack{
		Index:         index,
		Channels:      rec.ChannelCount,
		Language:      rec.LangISO2,
		LanguageISO1:  rec.LangISO2,
		LanguageISO2B: rec.LangISO3,
		Disposition: probe.Disposition{
			Comment: strings.EqualFold(strings.TrimSpace(rec.Variant), "commentary"),
		},
	}
}

func medianOffsetMS(offsets []float64) float64 {
	if len(offsets) == 0 {
		return 0
	}
	sorted := append([]float64(nil), offsets...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return (sorted[mid-1] + sorted[mid]) / 2
	}
	return sorted[mid]
}

// TestMeasureSyncResources runs the Shrek same-language fixture once through
// every sync-pipeline stage and prints one STEP| line per invocation plus
// job-directory disk measurements.
func TestMeasureSyncResources(t *testing.T) {
	requireAcceptanceTools(t)
	h := newHarness(t)
	rec := &recorder{t: t}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	defer cancel()

	target := h.copyFixture(t, shrekTargetName, h.media)
	donor := h.copyFixture(t, shrekDonorName, h.donors)
	instanceID := h.insertInstance(t, "http://127.0.0.1:1", false)
	libraryID := h.insertLibraryRow(t, instanceID, "Shrek", 2001, "en", target)

	created, err := h.st.RecordImportCandidate(ctx, donor, filepath.Base(donor), fileSize(t, donor))
	if err != nil || !created {
		t.Fatalf("RecordImportCandidate = %v/%v", created, err)
	}
	h.approveImport(t, 1, libraryID, donor)

	// Extraction → vault (not part of the sync job; reported for completeness).
	ew, err := extract.New(extract.Dependencies{
		Store: h.st, Settings: h.ss, DataDir: h.data,
		VaultHook: h.vaultHook(t), Logf: h.logs.printf,
	})
	if err != nil {
		t.Fatalf("extract.New: %v", err)
	}
	start := time.Now()
	processed, err := ew.ProcessOnce(ctx)
	rec.record("extract_vault", time.Since(start), 0, err == nil && processed, "")
	if err != nil || !processed {
		t.Fatalf("extract.ProcessOnce = %v/%v", processed, err)
	}

	pol := h.trackByLang(t, "pl")
	eng := h.trackByLang(t, "en")
	recRow, err := h.st.CreateSyncJob(ctx, store.SyncJobCreate{
		VaultTrackID: pol.ID, TargetLibraryCacheID: libraryID,
		TargetVideoPath: target, TargetLangISO2: "pl",
	})
	if err != nil {
		t.Fatalf("CreateSyncJob: %v", err)
	}

	jobDir := filepath.Join(h.work, "jobs", strconv.FormatInt(recRow.ID, 10))

	// ---- Stage (Copy-on-Work) ----
	stager, err := workdir.New(h.work, h.st)
	if err != nil {
		t.Fatalf("workdir.New: %v", err)
	}
	start = time.Now()
	staging, err := stager.Stage(ctx, workdir.Job{
		ID: recRow.ID, TargetTrackID: pol.ID, ReferenceTrackIDs: []int64{eng.ID},
	})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	rec.record("stage_copy", time.Since(start), 0, true, "")

	// ---- Probe ----
	start = time.Now()
	prober := probe.New(probe.Dependencies{})
	info, err := prober.Probe(ctx, target)
	rec.record("probe_target", time.Since(start), 0, err == nil, "")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}

	// ---- Reference resolution (same as syncjob.analysisPolicy) ----
	policy := reflang.Languages{}
	if raw, ok, err := h.ss.Get(ctx, settings.KeyLanguagesPolicy); err == nil && ok {
		var lp settings.LanguagesPolicy
		if json.Unmarshal(raw, &lp) == nil {
			policy = reflang.Languages{
				Targets:                 append([]string(nil), lp.TargetImportLanguages...),
				References:              append([]string(nil), lp.ExtraReferenceLanguages...),
				PreferOriginalLanguage:  lp.PreferOriginalLanguage,
				OriginalLanguageFromArr: lp.OriginalLanguageFromArr,
			}
		}
	}
	cfg := analyze.DefaultConfig()
	if raw, ok, err := h.ss.Get(ctx, settings.KeySyncDefaults); err == nil && ok {
		var sd settings.SyncDefaults
		if json.Unmarshal(raw, &sd) == nil {
			if len(sd.CheckpointPositionsPct) > 0 {
				cfg.CheckpointPositionsPct = append([]int(nil), sd.CheckpointPositionsPct...)
			}
			if sd.CheckpointWindowSec > 0 {
				cfg.CheckpointWindowSec = sd.CheckpointWindowSec
			}
			if sd.MaxOffsetSec > 0 {
				cfg.MaxOffsetSec = sd.MaxOffsetSec
			}
		}
	}
	vaultTracks := []probe.AudioTrack{trackAudioTrack(store.VaultTrackRecord{
		ID: pol.ID, LangISO2: pol.LangISO2, ChannelCount: 2,
	}, 0)}
	polRec, ok, err := h.st.VaultTrackByID(ctx, pol.ID)
	if err != nil || !ok {
		t.Fatalf("VaultTrackByID(%d) = %v/%v", pol.ID, ok, err)
	}
	siblings, err := h.st.SiblingVaultTracks(ctx, polRec.VaultMediaID, polRec.GroupUUID, pol.ID)
	if err != nil {
		t.Fatalf("SiblingVaultTracks: %v", err)
	}
	for _, sib := range siblings {
		vaultTracks = append(vaultTracks, trackAudioTrack(sib, len(vaultTracks)))
	}
	resolved := reflang.Resolve(info.Tracks, vaultTracks, policy, "en")
	if len(resolved.Pairs) == 0 {
		t.Fatalf("no reference pair: %s", resolved.Reason)
	}
	pair := resolved.Pairs[0]
	donorPath := staging.Target.Path
	if pair.Vault.Index > 0 && pair.Vault.Index <= len(staging.References) {
		donorPath = staging.References[pair.Vault.Index-1].Path
	}

	// ---- Two ffmpeg decodes into the whole-movie WAVs ----
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatalf("ffmpeg: %v", err)
	}
	targetWAV := filepath.Join(jobDir, "target.wav")
	donorWAV := filepath.Join(jobDir, "donor.wav")
	filter := analysisFilter(pair.Method)
	stream := pair.Target.Index
	start = time.Now()
	if err := rec.decodeAudio(ctx, "ffmpeg_decode_target", ffmpeg, target, stream, filter, targetWAV); err != nil {
		t.Fatalf("decode target: %v", err)
	}
	_ = start
	if err := rec.decodeAudio(ctx, "ffmpeg_decode_donor", ffmpeg, donorPath, 0, filter, donorWAV); err != nil {
		t.Fatalf("decode donor: %v", err)
	}

	engine, err := syncengine.New(syncBinary)
	if err != nil {
		t.Fatalf("sync.New: %v", err)
	}
	_ = engine

	// ---- Six engine spawns driven by the production analyzer ----
	req := analyze.Request{
		TargetPath:       targetWAV,
		DonorPath:        donorWAV,
		DurationSec:      0,
		TargetTracks:     info.Tracks,
		VaultTracks:      vaultTracks,
		Policy:           policy,
		OriginalLanguage: "en",
	}
	start = time.Now()
	report, err := analyze.Analyze(ctx, &measuredEngine{bin: syncBinary, rec: rec}, cfg, req)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	rec.record("analyze_total", time.Since(start), 0, true,
		fmt.Sprintf("offsets_ms=%v", report.OffsetsMS))

	// ---- Disk footprint of the staging directory before mux ----
	preMux := measureDir(t, jobDir)
	t.Logf("DISK|after_analysis|dir=%s|files=%d|bytes=%d|entries=%s",
		jobDir, preMux.files, preMux.bytes, strings.Join(preMux.names, ","))

	// ---- Mux ----
	raw, err := json.Marshal(settings.TargetTagFilter{
		RequiredFilenameTags: []string{"[ArcA310]"}, TagMatchMode: "ANY", EnforceTagFilter: true,
	})
	if err != nil {
		t.Fatalf("marshal tag filter: %v", err)
	}
	if err := h.ss.Set(ctx, settings.KeyTargetTagFilter, raw); err != nil {
		t.Fatalf("set tag filter: %v", err)
	}
	rule := gate.RuleFromTagFilter(settings.TargetTagFilter{
		RequiredFilenameTags: []string{"[ArcA310]"}, TagMatchMode: "ANY", EnforceTagFilter: true,
	})
	audio := filepath.Join(jobDir, "target-"+strconv.FormatInt(pol.ID, 10)+"-"+filepath.Base(pol.Path))
	start = time.Now()
	res, err := mux.New(mux.Dependencies{}).Mux(ctx, mux.Job{
		ID:             recRow.ID,
		WorkDir:        h.work,
		TargetPath:     target,
		TargetLangISO2: "pl",
		AudioPath:      audio,
		SyncOffsetMS:   int64(math.Round(medianOffsetMS(report.OffsetsMS))),
		TrackTitle:     "Polski Dubbing",
		OutputMode:     mux.OutputModeReplace,
		Gate:           rule,
		Report:         report,
	})
	rec.record("mux_mkvmerge", time.Since(start), 0, err == nil, "")
	if err != nil {
		t.Fatalf("mux: %v", err)
	}
	t.Logf("MUX|output=%s|backup=%s|workcopy=%s", res.OutputPath, res.BackupPath, res.WorkCopy)

	postMux := measureDir(t, jobDir)
	t.Logf("DISK|after_mux|dir=%s|files=%d|bytes=%d|entries=%s",
		jobDir, postMux.files, postMux.bytes, strings.Join(postMux.names, ","))

	// ---- Cleanup ----
	start = time.Now()
	err = stager.Cleanup(recRow.ID)
	rec.record("cleanup_staging", time.Since(start), 0, err == nil, "")
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, statErr := os.Stat(jobDir); statErr == nil {
		t.Logf("DISK|after_cleanup|dir=%s|still_present=true", jobDir)
	} else {
		t.Logf("DISK|after_cleanup|dir=%s|still_present=false", jobDir)
	}

	// ---- Summary ----
	var totalWall time.Duration
	var peak int64
	var peakStep string
	for _, row := range rec.rows {
		if row.Step == "extract_vault" || row.Step == "cleanup_staging" {
			continue
		}
		totalWall += row.Wall
		if row.MaxRSS > peak {
			peak, peakStep = row.MaxRSS, row.Step
		}
	}
	t.Logf("SUMMARY|sync_stage_total_wall_ms=%d|peak_child_rss_kb=%d|peak_step=%s|job_dir_pre_mux_bytes=%d|job_dir_post_mux_bytes=%d",
		totalWall.Milliseconds(), peak, peakStep, preMux.bytes, postMux.bytes)
}
