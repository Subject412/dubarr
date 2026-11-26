package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/arrrescan"
	"github.com/Subject412/dubarr/backend/internal/extract"
	"github.com/Subject412/dubarr/backend/internal/gate"
	"github.com/Subject412/dubarr/backend/internal/mux"
	"github.com/Subject412/dubarr/backend/internal/reflang"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
	syncengine "github.com/Subject412/dubarr/backend/internal/sync"
	"github.com/Subject412/dubarr/backend/internal/syncjob"
	"github.com/Subject412/dubarr/backend/internal/syncworker"
)

// TestShrekEndToEnd is the M5-5 same-language proof over copies of the Shrek
// fixtures: queue → approve → extract → vault → sync job → syncworker
// (execute → mux) → rescan-skip. The 5.1 Polish dub is archived as a vault
// track and later muxed into a copy of the AV1 target using the English
// sibling track as the reference, because the target is already English
// (reflang priority 1/2), which is exactly the same-language path.
func TestShrekEndToEnd(t *testing.T) {
	requireAcceptanceTools(t)
	h := newHarness(t)
	const secretMarker = "sk-acceptance-DO-NOT-LOG-5hrek"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// 1. Copies of the fixtures, then the arr library row the job targets.
	target := h.copyFixture(t, shrekTargetName, h.media)
	donor := h.copyFixture(t, shrekDonorName, h.donors)
	instanceID := h.insertInstance(t, "http://127.0.0.1:1", false) // disabled → rescan-skip
	libraryID := h.insertLibraryRow(t, instanceID, "Shrek", 2001, "en", target)

	preTargetSHA := sha256File(t, target)
	preVideo := videoStreamHash(t, target)

	// 2. Queue: the donor is discovered (store candidate) and approved.
	created, err := h.st.RecordImportCandidate(ctx, donor, filepath.Base(donor), fileSize(t, donor))
	if err != nil || !created {
		t.Fatalf("RecordImportCandidate = %v/%v, want created", created, err)
	}
	// Zero-untagged-mux proof (PLAN.md M5-5): a target without the required
	// [ArcA310] tag must classify as waiting_for_tags and the muxer must
	// refuse it before writing. This runs before the job is approved so the
	// donor remains candidate 1.
	h.assertUntaggedGateBlocksMux(t, target)
	h.approveImport(t, 1, libraryID, donor)

	// 3. Extract → vault with the real worker and vault writer.
	ew, err := extract.New(extract.Dependencies{
		Store: h.st, Settings: h.ss, DataDir: h.data,
		VaultHook: h.vaultHook(t), Logf: h.logs.printf,
	})
	if err != nil {
		t.Fatalf("extract.New: %v", err)
	}
	if processed, err := ew.ProcessOnce(ctx); err != nil || !processed {
		t.Fatalf("extract.ProcessOnce = %v/%v, want true/nil", processed, err)
	}
	if status := importStatus(t, h, 1); status != "completed" {
		t.Fatalf("import_queue status = %q, want completed", status)
	}

	tracks := h.vaultTracks(t)
	if len(tracks) != 2 {
		t.Fatalf("vault tracks = %d, want pol + eng sibling", len(tracks))
	}
	pol := h.trackByLang(t, "pl")
	eng := h.trackByLang(t, "en")
	for _, tr := range []vaultTrack{pol, eng} {
		if got := sha256File(t, tr.Path); got != tr.SHA256 {
			t.Fatalf("vault track %d (%s) sha256 = %s, file is %s (checksum seeding bug)",
				tr.ID, tr.LangISO2, tr.SHA256, got)
		}
	}
	polVaultSHA, engVaultSHA := sha256File(t, pol.Path), sha256File(t, eng.Path)

	// 4. Sync job + the real worker chain.
	rec, err := h.st.CreateSyncJob(ctx, store.SyncJobCreate{
		VaultTrackID: pol.ID, TargetLibraryCacheID: libraryID,
		TargetVideoPath: target, TargetLangISO2: "pl",
	})
	if err != nil {
		t.Fatalf("CreateSyncJob: %v", err)
	}
	engine, err := syncengine.New(syncBinary, syncengine.WithTimeout(analyze.JobTimeout(analyze.DefaultConfig())))
	if err != nil {
		t.Fatalf("sync.New: %v", err)
	}
	router := http.NewServeMux()
	jobSvc := syncjob.RegisterRoutes(router, syncjob.Dependencies{
		DB: h.st.DB, Settings: h.ss, WorkDir: h.work, Engine: engine,
		RequireAdmin: func(next http.Handler) http.Handler { return next },
	})
	rescanner, err := arrrescan.New(arrrescan.Deps{DB: h.st.DB, Secrets: fakeDecrypter{marker: secretMarker}})
	if err != nil {
		t.Fatalf("arrrescan.New: %v", err)
	}
	worker, err := syncworker.New(syncworker.Dependencies{
		Store: h.st, Settings: h.ss, Executor: jobSvc,
		Muxer: mux.New(mux.Dependencies{}), Rescanner: rescanner,
		WorkDir: h.work, Interval: time.Millisecond, Logf: h.logs.printf,
	})
	if err != nil {
		t.Fatalf("syncworker.New: %v", err)
	}
	if processed, err := worker.ProcessOnce(ctx); err != nil || !processed {
		t.Fatalf("syncworker.ProcessOnce = %v/%v, want true/nil", processed, err)
	}

	final, ok, err := h.st.SyncJobByID(ctx, rec.ID)
	if err != nil || !ok {
		t.Fatalf("SyncJobByID = %v/%v", ok, err)
	}
	if final.Status != "completed" {
		t.Fatalf("job status = %q (%s), want completed; checkpoints: %s; logs:\n%s", final.Status, final.ErrorMessage, final.CheckpointsJSON, h.logs.String())
	}

	// Same-language proof: the English sibling was the reference, priority 1.
	var report analyze.Report
	if err := json.Unmarshal([]byte(final.CheckpointsJSON), &report); err != nil {
		t.Fatalf("decode persisted report: %v", err)
	}
	if report.SyncMethod != reflang.SameLangReference {
		t.Fatalf("sync_method = %q, want %q", report.SyncMethod, reflang.SameLangReference)
	}
	if report.Verdict != analyze.VerdictOK {
		t.Fatalf("verdict = %q (%s), want ok", report.Verdict, report.Reason)
	}
	// New peak-prominence metric: the real fixtures must land in the constant
	// around −335..−352 ms (observed −335.5..−349) with a small spread
	// (observed 14 ms; the settled constant-offset bound is 40 ms).
	if len(report.OffsetsMS) != len(analyze.DefaultCheckpointPositionsPct) {
		t.Fatalf("checkpoint count = %d, want %d", len(report.OffsetsMS), len(analyze.DefaultCheckpointPositionsPct))
	}
	for i, off := range report.OffsetsMS {
		if off > -330 || off < -355 {
			t.Fatalf("checkpoint %d offset = %v ms, want within −355..−330; all %v", i, off, report.OffsetsMS)
		}
	}
	if report.MaxDriftMS > 25 {
		t.Fatalf("checkpoint spread = %v ms, want <= 25 (constant offset); all %v", report.MaxDriftMS, report.OffsetsMS)
	}
	for i, q := range report.Qualities {
		if q < analyze.LowQualityThreshold {
			t.Fatalf("checkpoint %d quality = %v, want >= %v; all %v", i, q, analyze.LowQualityThreshold, report.Qualities)
		}
	}

	// 5. Muxed output: the Polish track was appended, tagged, video untouched.
	tags := audioStreamTags(t, target)
	if len(tags) != 2 {
		t.Fatalf("published target audio streams = %v, want original + pl", tags)
	}
	foundPol := false
	for _, tag := range tags {
		if strings.HasPrefix(tag, "pol|") && strings.Contains(tag, "Polski Dubbing") {
			foundPol = true
		}
	}
	if !foundPol {
		t.Fatalf("published target audio tags = %v, want a pol track titled Polski Dubbing", tags)
	}
	if got := videoStreamHash(t, target); got != preVideo {
		t.Fatalf("video stream hash changed: %s → %s (target video must never be transcoded)", preVideo, got)
	}

	// 6. No backup sidecar: the temp was validated before the atomic rename,
	// so the target was replaced in place (its bytes changed) and no
	// <target>.bak appeared.
	if got := sha256File(t, target); got == preTargetSHA {
		t.Fatalf("published target sha256 = %s, unchanged from the pre-mux original", got)
	}
	if _, err := os.Stat(target + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("publish created a .bak beside the target (stat err = %v)", err)
	}

	// 7. Vault is write-once: both archives are byte-identical across the run.
	if got := sha256File(t, pol.Path); got != polVaultSHA {
		t.Fatalf("vault pol track changed during the run: %s → %s", polVaultSHA, got)
	}
	if got := sha256File(t, eng.Path); got != engVaultSHA {
		t.Fatalf("vault eng sibling changed during the run: %s → %s", engVaultSHA, got)
	}

	// 8. Rescan-skip: the disabled instance produced no request and the job
	// still completed (M5-4 best-effort).
	logs := h.logs.String()
	if !strings.Contains(logs, "rescan") || !strings.Contains(logs, "disabled") {
		t.Fatalf("missing rescan-skip diagnostic; logs:\n%s", logs)
	}

	// The API response is another leak surface: fetch the completed job
	// through the real handler and scan the body alongside the logs.
	respRec := httptest.NewRecorder()
	router.ServeHTTP(respRec, httptest.NewRequest(http.MethodGet, "/api/sync/jobs/"+strconv.FormatInt(rec.ID, 10), nil))
	if respRec.Code != http.StatusOK {
		t.Fatalf("GET /api/sync/jobs/%d = %d: %s", rec.ID, respRec.Code, respRec.Body.String())
	}
	apiResponse := respRec.Body.String()
	if !strings.Contains(apiResponse, `"status":"done"`) {
		t.Fatalf("job response %q does not report done", apiResponse)
	}
	assertNoSecrets(t, secretMarker, logs, apiResponse)

	// Issue #23: a completed sync job leaves no {WORK_DIR}/jobs/{id}/ behind.
	if _, err := os.Stat(filepath.Join(h.work, "jobs", strconv.FormatInt(rec.ID, 10))); !os.IsNotExist(err) {
		t.Fatalf("staging should be removed after a completed job, stat err = %v", err)
	}
}

func importStatus(t *testing.T, h *harness, id int64) string {
	t.Helper()
	var status string
	if err := h.st.DB.QueryRow(`SELECT status FROM import_queue WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("read import_queue %d: %v", id, err)
	}
	return status
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

// assertUntaggedGateBlocksMux proves PLAN.md M5-5's "zero untagged mux": an
// untagged copy of the target classifies as waiting_for_tags through the real
// gate rule/settings store, and the muxer rejects it with ErrIneligible before
// any write (no .bak, no audio stream, bytes unchanged). The control is a
// hardlink so the proof costs no additional fixture bytes; mux publishes by
// renaming a temp over its own target, so the link never aliases a written
// file.
func (h *harness) assertUntaggedGateBlocksMux(t *testing.T, tagged string) {
	t.Helper()
	ctx := context.Background()
	// ADR-0012 ships an empty required-tag default, so the M5-5 proof sets the
	// marker it exercises explicitly instead of relying on seed data.
	raw, err := json.Marshal(settings.TargetTagFilter{
		RequiredFilenameTags: []string{"[ArcA310]"},
		TagMatchMode:         "ANY",
		EnforceTagFilter:     true,
	})
	if err != nil {
		t.Fatalf("marshal tag filter: %v", err)
	}
	if err := h.ss.Set(ctx, settings.KeyTargetTagFilter, raw); err != nil {
		t.Fatalf("set target.tag_filter: %v", err)
	}
	stored, ok, err := h.ss.Get(ctx, settings.KeyTargetTagFilter)
	if err != nil || !ok {
		t.Fatalf("read target.tag_filter = %v/%v, want seeded value", ok, err)
	}
	var tf settings.TargetTagFilter
	if err := json.Unmarshal(stored, &tf); err != nil {
		t.Fatalf("decode target.tag_filter: %v", err)
	}
	rule := gate.RuleFromTagFilter(tf)
	if got := rule.Classify(filepath.Base(tagged)); got.Status != gate.StatusEligible {
		t.Fatalf("tagged target %q gate = %s, want eligible", filepath.Base(tagged), got.Status)
	}

	control := filepath.Join(h.media, "Shrek (2001) untagged control.mkv")
	if err := os.Link(tagged, control); err != nil {
		t.Fatalf("link untagged control: %v", err)
	}
	controlSHA := sha256File(t, control)
	if created, err := h.st.RecordImportCandidate(ctx, control, filepath.Base(control), fileSize(t, control)); err != nil || !created {
		t.Fatalf("RecordImportCandidate(control) = %v/%v, want created", created, err)
	}

	svc := gate.NewService(rule, h.st)
	res, err := svc.ClassifyCandidate(ctx, control, control)
	if err != nil {
		t.Fatalf("ClassifyCandidate(control): %v", err)
	}
	if res.Status != gate.StatusWaitingForTags {
		t.Fatalf("untagged control gate = %s, want waiting_for_tags", res.Status)
	}
	persisted, ok, err := h.st.ImportCandidateGate(ctx, control)
	if err != nil || !ok {
		t.Fatalf("ImportCandidateGate(control) = %v/%v", ok, err)
	}
	if persisted.Status != string(gate.StatusWaitingForTags) {
		t.Fatalf("persisted control gate = %s, want waiting_for_tags", persisted.Status)
	}

	_, err = mux.New(mux.Dependencies{}).Mux(ctx, mux.Job{
		ID:             9000,
		WorkDir:        h.work,
		TargetPath:     control,
		TargetLangISO2: "pl",
		AudioPath:      filepath.Join(h.work, "unused-control.mka"),
		Gate:           rule,
		Report:         analyze.Report{Verdict: analyze.VerdictOK},
	})
	if !errors.Is(err, mux.ErrIneligible) {
		t.Fatalf("mux on untagged control error = %v, want ErrIneligible", err)
	}
	if _, err := os.Stat(control + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("untagged mux produced a .bak (stat err = %v)", err)
	}
	if got := sha256File(t, control); got != controlSHA {
		t.Fatalf("untagged control bytes changed: %s → %s", controlSHA, got)
	}
	if tags := audioStreamTags(t, control); len(tags) != 1 {
		t.Fatalf("untagged control audio streams = %v, want the untouched single track", tags)
	}
}
