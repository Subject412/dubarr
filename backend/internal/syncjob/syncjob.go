// Package syncjob exposes the M4-6 sync job API: create a job for one vault
// track against one arr target, poll its status, and run the multi-point drift
// analysis through internal/analyze.
//
// POST /api/sync/jobs is the eligibility gate: a waiting_for_tags
// or missing_target library row is rejected unless the caller asks for a force
// override *and* settings security.options.allow_force_tag_override is true.
// The settled default is false, so a force request is itself rejected with 409
// and never reaches the job row.
//
// Execution is deliberately not a background worker. [Service.Execute] is the
// seam the M5 Sync Studio / worker will call; main.go only registers the HTTP
// routes. Execute stages the track group with internal/workdir (Copy-on-Work),
// decodes the resolved reference pair with ffmpeg, correlates it in-process with
// internal/dsp, and persists the DriftReport in checkpoints_json.
package syncjob

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/dsp/mfcc"
	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/reflang"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
	"github.com/Subject412/dubarr/backend/internal/sync"
	"github.com/Subject412/dubarr/backend/internal/workdir"
)

// Persisted sync_mux_jobs.status values used by this slice. The schema CHECK
// also allows ready_for_review/muxing, which M5 owns.
const (
	statusQueued    = "queued"
	statusAnalyzing = "analyzing"
	statusCompleted = "completed"
	statusFailed    = "failed"
)

// Public status vocabulary of GET /api/sync/jobs (PLAN.md M4-6, #48). The
// schema status is mapped onto it so the API never leaks M5-internal states;
// held and cancelled are public states of their own and pass through unmapped.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusDone      = "done"
	StatusFailed    = "failed"
	StatusHeld      = "held"
	StatusCancelled = "cancelled"
)

// SourceTiming is what the container says about one analysis input before it
// is decoded: how long the audio really is and where its timeline starts.
//
// All three values come from the file the job will decode, not from the WAV
// that a windowed decode never produces, which is the whole point: a
// percentage of a duration that only exists after a full decode is useless to
// a job that intends to skip the full decode.
type SourceTiming struct {
	// Duration is the length of the track the job analyses, from
	// probe.MediaInfo.TrackDuration: the stream's own duration when the
	// container provides one, else the container's, else unknown. It is 0
	// when unknown; Known is what says which case that is.
	Duration time.Duration
	// ContainerStart is the container's timeline origin
	// (ffprobe `format.start_time`) and TrackStart the analysed stream's own
	// origin. Their difference is the correction an input seek into the
	// container needs; on an audio-only file the two coincide. Both are 0
	// when their field is unknown, so read Known before trusting them.
	ContainerStart time.Duration
	TrackStart     time.Duration
	// Known reports that the container stated everything the window planner
	// needs: a duration and both timeline origins. It is false when any of
	// them was absent, "N/A" or unparseable — never silently 0. Callers can
	// still tell the two failure shapes apart: Duration == 0 && !Known is
	// "no duration", while Duration > 0 && !Known is "unknown origin". Both
	// mean whole-file analysis, for different reasons.
	Known bool
}

// SourceTimings are the timing facts of the two analysis inputs, probed once
// per job before any mode decision and before any decode. Nothing re-probes
// per checkpoint.
type SourceTimings struct {
	Target SourceTiming
	Donor  SourceTiming
}

// Dependencies are the collaborators the sync job API needs.
type Dependencies struct {
	DB       *sql.DB
	Settings *settings.Store
	// RequireAdmin wraps every route. When nil the API answers 503 rather than
	// running unauthenticated.
	RequireAdmin func(http.Handler) http.Handler
	// Engine overrides the in-process correlator (tests inject a stub).
	// Nil builds the default engine at Execute time.
	Engine analyze.Engine
	// WorkDir overrides storage.paths.work_dir (tests).
	WorkDir string
	// FFmpegPath overrides ffmpeg discovery (tests); empty resolves PATH.
	FFmpegPath string
	// Prober overrides the ffprobe wrapper (tests).
	Prober *probe.Prober
	// Logger is the central logger (nil keeps the stdlib default).
	Logger *slog.Logger
}

// Service holds the resolved dependencies for the registered handlers.
type Service struct {
	store        *store.Store
	settings     *settings.Store
	requireAdmin func(http.Handler) http.Handler
	engine       analyze.Engine
	workDir      string
	ffmpegPath   string
	prober       *probe.Prober
	logger       *slog.Logger
	// probeTimings is the probe seam for source timing: one call per analysis
	// input, returning the container's MediaInfo. Nil resolves the service's
	// prober, so tests supply ffprobe output instead of shelling out.
	probeTimings func(ctx context.Context, path string) (probe.MediaInfo, error)
	// probeTarget is the media-info probe used for reference resolution (which
	// audio tracks the target has). It is separate from probeTimings so a test
	// can drive one without standing in for the other; nil resolves the
	// service's prober.
	probeTarget func(ctx context.Context, path string) (probe.MediaInfo, error)
	// windowDecode is the window decode and frame gate. Nil means decodeWindow;
	// tests replace it so the windowed path runs without ffmpeg.
	windowDecode windowDecodeFunc
	// wholeDecode is the whole-track decode. Nil means decodeAudio; tests
	// replace it to count the fallback's two full decodes without ffmpeg.
	wholeDecode func(ctx context.Context, ffmpeg, input string, stream int, filter, out string) error
}

// RegisterRoutes mounts:
//
//	POST /api/sync/jobs         create a queued job (eligibility gate)
//	GET  /api/sync/jobs         list jobs, newest first; ?status= narrows
//	GET  /api/sync/jobs/{id}    one job with its DriftReport
func RegisterRoutes(mux *http.ServeMux, deps Dependencies) *Service {
	svc := &Service{
		store:        &store.Store{DB: deps.DB},
		settings:     deps.Settings,
		requireAdmin: deps.RequireAdmin,
		engine:       deps.Engine,
		workDir:      deps.WorkDir,
		ffmpegPath:   deps.FFmpegPath,
		prober:       deps.Prober,
		logger:       deps.Logger,
	}
	admin := func(h http.HandlerFunc) http.Handler {
		if svc.requireAdmin == nil {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeError(w, http.StatusServiceUnavailable, "authorization is not configured")
			})
		}
		return svc.requireAdmin(h)
	}
	mux.Handle("POST /api/sync/jobs", admin(svc.handleCreate))
	mux.Handle("GET /api/sync/jobs", admin(svc.handleList))
	mux.Handle("GET /api/sync/jobs/{id}", admin(svc.handleGet))
	return svc
}

// createRequest is the POST /api/sync/jobs body.
type createRequest struct {
	VaultTrackID         int64 `json:"vault_track_id"`
	TargetLibraryCacheID int64 `json:"target_library_cache_id"`
	ForceIgnoreTagFilter bool  `json:"force_ignore_tag_filter"`
}

// JobInfo is the API shape of one sync job.
type JobInfo struct {
	ID                   int64  `json:"id"`
	Status               string `json:"status"`
	VaultTrackID         int64  `json:"vault_track_id"`
	TargetLibraryCacheID int64  `json:"target_library_cache_id"`
	TargetVideoPath      string `json:"target_video_path,omitempty"`
	TargetLangISO2       string `json:"target_lang_iso2,omitempty"`
	// OutputPath is where mux published the file (the Target in replace mode,
	// the separate-mode file inside storage.paths.output_dir).
	OutputPath         string          `json:"output_path,omitempty"`
	SyncMethod         string          `json:"sync_method,omitempty"`
	CalculatedOffsetMS int64           `json:"calculated_offset_ms"`
	Report             *analyze.Report `json:"report,omitempty"`
	// AnalysisSource is the analysis mode the job actually used (windows, or
	// full after a fallback) and AnalysisFallback why a windowed request did
	// not run as windows ("2-suspect-windows", ...). Both are omitted for a job
	// analysed before the columns existed, which the UI renders as "—".
	AnalysisSource   string `json:"analysis_source,omitempty"`
	AnalysisFallback string `json:"analysis_fallback,omitempty"`
	ErrorMessage     string `json:"error_message,omitempty"`
	// HeldReason is why a held (or cancelled) job is not queued: the closed
	// vocabulary code (#48), empty on a job that is neither. The list and the
	// detail must agree, so both come from jobInfo.
	HeldReason string `json:"held_reason,omitempty"`
	// ForceIgnoreTagFilter reports that the operator approved this job past
	// the tag gate: it is born queued and is never re-held.
	ForceIgnoreTagFilter bool `json:"force_ignore_tag_filter"`
	// OriginQueueID is the import_queue row whose approval created the job;
	// absent for a manual Studio job.
	OriginQueueID int64  `json:"origin_queue_id,omitempty"`
	CreatedAt     string `json:"created_at,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
}

func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "sync job store is not configured")
		return
	}
	var req createRequest
	if r.Body == nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.VaultTrackID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid vault_track_id")
		return
	}
	if req.TargetLibraryCacheID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid target_library_cache_id")
		return
	}

	// Force is a settings-gated escape hatch. The settled configuration is OFF,
	// so asking for it fails loudly instead of silently proceeding untagged.
	if req.ForceIgnoreTagFilter && !s.forceOverrideAllowed(r.Context()) {
		writeError(w, http.StatusConflict, "force override disabled")
		return
	}

	track, ok, err := s.store.VaultTrackByID(r.Context(), req.VaultTrackID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the vault track")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "vault track not found")
		return
	}
	lib, ok, err := s.store.LibraryCacheJobTargetByID(r.Context(), req.TargetLibraryCacheID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the library target")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "library target not found")
		return
	}
	if err := eligibilityError(lib, req.ForceIgnoreTagFilter); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	rec, err := s.store.CreateSyncJob(r.Context(), store.SyncJobCreate{
		VaultTrackID:         req.VaultTrackID,
		TargetLibraryCacheID: req.TargetLibraryCacheID,
		TargetVideoPath:      lib.TargetFilePath,
		TargetLangISO2:       track.LangISO2,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create the sync job")
		return
	}
	writeJSON(w, http.StatusCreated, jobInfo(rec))
}

// eligibilityError enforces the Processing Gate verdict. Force (already checked
// against settings by the caller) bypasses the gate only: a target with no
// file path is not eligible under any setting.
func eligibilityError(lib store.LibraryCacheJobTarget, force bool) error {
	if strings.TrimSpace(lib.TargetFilePath) == "" {
		return errors.New("target file is missing")
	}
	if force {
		return nil
	}
	status := strings.TrimSpace(lib.EligibilityStatus)
	if status == "" {
		if lib.MatchesTargetTags {
			status = "eligible"
		} else {
			status = "waiting_for_tags"
		}
	}
	if status != "eligible" {
		return fmt.Errorf("target is not tag-eligible (%s)", status)
	}
	return nil
}

func (s *Service) forceOverrideAllowed(ctx context.Context) bool {
	if s.settings == nil {
		return false
	}
	raw, ok, err := s.settings.Get(ctx, settings.KeySecurityOptions)
	if err != nil || !ok {
		return false
	}
	var sec settings.SecurityOptions
	if err := json.Unmarshal(raw, &sec); err != nil {
		return false
	}
	return sec.AllowForceTagOverride
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "sync job store is not configured")
		return
	}
	filter, ok := statusFilter(r.URL.Query().Get("status"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid status filter")
		return
	}
	recs, err := s.store.ListSyncJobs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read sync jobs")
		return
	}
	items := make([]JobInfo, 0, len(recs))
	for _, rec := range recs {
		info := jobInfo(rec)
		// The filter compares the public status, so ?status=running still
		// covers the M5-internal states publicStatus folds into it.
		if filter != "" && info.Status != filter {
			continue
		}
		items = append(items, info)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// statusFilter validates the optional ?status= narrowing of the job list.
// "" is the whole list. Anything outside the public vocabulary is refused
// rather than silently returning everything, which would make a typo look
// like "no held jobs". One value only: the query is a single narrowing.
func statusFilter(raw string) (string, bool) {
	status := strings.TrimSpace(raw)
	switch status {
	case "", StatusQueued, StatusRunning, StatusDone, StatusFailed, StatusHeld, StatusCancelled:
		return status, true
	}
	return "", false
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "sync job store is not configured")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid sync job id")
		return
	}
	rec, ok, err := s.store.SyncJobByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the sync job")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "sync job not found")
		return
	}
	writeJSON(w, http.StatusOK, jobInfo(rec))
}

func jobInfo(rec store.SyncJobRecord) JobInfo {
	info := JobInfo{
		ID:                   rec.ID,
		Status:               publicStatus(rec.Status),
		VaultTrackID:         rec.TargetTrackID,
		TargetLibraryCacheID: rec.TargetLibraryCacheID,
		TargetVideoPath:      rec.TargetVideoPath,
		TargetLangISO2:       rec.TargetLangISO2,
		OutputPath:           rec.OutputPath,
		SyncMethod:           rec.SyncMethod,
		CalculatedOffsetMS:   rec.CalculatedOffsetMS,
		AnalysisSource:       rec.AnalysisSource,
		AnalysisFallback:     rec.AnalysisFallback,
		ErrorMessage:         rec.ErrorMessage,
		HeldReason:           rec.HeldReason,
		ForceIgnoreTagFilter: rec.ForceIgnoreTagFilter,
		OriginQueueID:        rec.OriginQueueID,
		CreatedAt:            rec.CreatedAt,
		UpdatedAt:            rec.UpdatedAt,
	}
	if strings.TrimSpace(rec.CheckpointsJSON) != "" && rec.CheckpointsJSON != "[]" {
		var report analyze.Report
		if err := json.Unmarshal([]byte(rec.CheckpointsJSON), &report); err == nil {
			info.Report = &report
		}
	}
	return info
}

func publicStatus(status string) string {
	switch status {
	case statusQueued:
		return StatusQueued
	case statusCompleted:
		return StatusDone
	case statusFailed:
		return StatusFailed
	case statusAnalyzing, "ready_for_review", "muxing":
		return StatusRunning
	case "held":
		// A held job is waiting, not finished: it is its own public state and
		// must never be folded into done or failed.
		return StatusHeld
	case "cancelled":
		return StatusCancelled
	default:
		return status
	}
}

// Execute resolves references and runs the multi-point analysis for one job.
// It is the M5 hand-off seam; no goroutine in main.go calls it.
//
// Staging is Copy-on-Work: vault files are only opened read-only and their
// copies are SHA-256 verified. On failure the staging directory is removed; on
// success it is kept for M5's preview/mux stages, which clean it up after mux.
func (s *Service) Execute(ctx context.Context, jobID int64) (analyze.Report, error) {
	rec, ok, err := s.store.SyncJobByID(ctx, jobID)
	if err != nil {
		return analyze.Report{}, err
	}
	if !ok {
		return analyze.Report{}, fmt.Errorf("syncjob: job %d not found", jobID)
	}
	if rec.Status != statusQueued {
		return analyze.Report{}, fmt.Errorf("syncjob: job %d is %s, want queued", jobID, rec.Status)
	}
	if err := s.store.SetSyncJobStatus(ctx, jobID, statusAnalyzing, ""); err != nil {
		return analyze.Report{}, err
	}
	run, execErr := s.runAnalysis(ctx, rec)
	if execErr != nil {
		_ = s.store.SetSyncJobStatus(context.WithoutCancel(ctx), jobID, statusFailed, execErr.Error())
		return analyze.Report{}, execErr
	}
	offset, err := medianOffset(run.Report.OffsetsMS)
	if err != nil {
		_ = s.store.SetSyncJobStatus(context.WithoutCancel(ctx), jobID, statusFailed, err.Error())
		return analyze.Report{}, err
	}
	// The row records what the job did, not what the setting asked for: a
	// windowed request that fell back is stored as full plus its reason, which
	// is the only version an operator can check afterwards.
	if err := s.store.FinishSyncJobAnalysis(ctx, jobID, statusCompleted, string(run.Report.SyncMethod), offset,
		run.Report, "", run.Source, run.Fallback); err != nil {
		return analyze.Report{}, err
	}
	return run.Report, nil
}

// analysisRun is what one job's analysis produced: the report and the record of
// how it was obtained. Execute persists Source and Fallback on the job row;
// DecodedSec is the structural speed gate (how much audio the job read) and is
// not persisted.
type analysisRun struct {
	Report analyze.Report
	// Source is the mode actually used: windows, or full after a fallback.
	Source string
	// Fallback is why a windowed request did not run as windows; empty when the
	// requested mode ran as requested.
	Fallback string
	// DecodedSec is every second of audio this job decoded, both sides: the
	// windows it cut (plus their pre-roll) and, after a fallback, both whole
	// tracks.
	DecodedSec float64
}

// runAnalysis runs one job's analysis: it stages the group, probes the two
// analysis inputs, decides the mode once, and then takes exactly one of the two
// paths — the windows with their retry/fallback policy, or the whole tracks.
func (s *Service) runAnalysis(ctx context.Context, rec store.SyncJobRecord) (analysisRun, error) {
	track, ok, err := s.store.VaultTrackByID(ctx, rec.TargetTrackID)
	if err != nil {
		return analysisRun{}, err
	}
	if !ok {
		return analysisRun{}, fmt.Errorf("syncjob: vault track %d not found", rec.TargetTrackID)
	}
	lib, ok, err := s.store.LibraryCacheJobTargetByID(ctx, rec.TargetLibraryCacheID)
	if err != nil {
		return analysisRun{}, err
	}
	if !ok || lib.TargetFilePath == "" {
		return analysisRun{}, fmt.Errorf("syncjob: library target %d has no file", rec.TargetLibraryCacheID)
	}
	if _, err := os.Stat(lib.TargetFilePath); err != nil {
		return analysisRun{}, fmt.Errorf("syncjob: target file: %w", err)
	}

	workDir := s.workDir
	if workDir == "" {
		workDir = s.configuredWorkDir(ctx)
	}
	if workDir == "" {
		return analysisRun{}, errors.New("syncjob: storage.paths.work_dir is not configured")
	}

	siblings, err := s.store.SiblingVaultTracks(ctx, track.VaultMediaID, track.GroupUUID, rec.TargetTrackID)
	if err != nil {
		return analysisRun{}, err
	}
	ids := make([]int64, 0, len(siblings))
	for _, sib := range siblings {
		ids = append(ids, sib.ID)
	}

	stager, err := workdir.New(workDir, s.store)
	if err != nil {
		return analysisRun{}, err
	}
	staging, err := stager.Stage(ctx, workdir.Job{
		ID:                rec.ID,
		TargetTrackID:     rec.TargetTrackID,
		ReferenceTrackIDs: ids,
	})
	if err != nil {
		return analysisRun{}, err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = stager.Cleanup(rec.ID)
		}
	}()

	// Staged paths keyed by audio_tracks id. staging.References is ordered
	// like ids, which mirrors the sibling query order.
	staged := map[int64]string{track.ID: staging.Target.Path}
	for i, sib := range siblings {
		if i < len(staging.References) {
			staged[sib.ID] = staging.References[i].Path
		}
	}

	prober := s.prober
	if prober == nil {
		prober = probe.New(probe.Dependencies{})
	}
	info, err := s.probeMediaInfo(ctx, prober, lib.TargetFilePath)
	if err != nil {
		return analysisRun{}, fmt.Errorf("syncjob: probe target: %w", err)
	}
	targetTracks := info.Tracks
	vaultTracks := make([]probe.AudioTrack, 0, 1+len(siblings))
	vaultTracks = append(vaultTracks, trackAudioTrack(track, 0))
	for _, sib := range siblings {
		vaultTracks = append(vaultTracks, trackAudioTrack(sib, len(vaultTracks)))
	}

	policy, cfg := s.analysisPolicy(ctx)
	resolved := reflang.Resolve(targetTracks, vaultTracks, policy, lib.OriginalLanguageISO2)
	if len(resolved.Pairs) == 0 {
		return analysisRun{}, fmt.Errorf("syncjob: no reference strategy: %s", resolved.Reason)
	}
	pair := resolved.Pairs[0]
	// Vault tracks are indexed by their position in the staged group: 0 is the
	// job's target track, i+1 is siblings[i].
	donorID := track.ID
	if pair.Vault.Index > 0 && pair.Vault.Index <= len(siblings) {
		donorID = siblings[pair.Vault.Index-1].ID
	}
	donorPath, ok := staged[donorID]
	if !ok {
		return analysisRun{}, fmt.Errorf("syncjob: staged copy of track %d is missing", donorID)
	}

	// The two analysis inputs' real timings, probed once, here, before
	// anything is decoded and before any mode is chosen: a window plan needs
	// the length and origin of the files it will seek, not of the WAVs a full
	// decode produces. Nothing re-probes per checkpoint.
	//
	// A failed probe is not a failed job. This step did not exist before
	// windowed analysis, so letting it abort would turn a job that worked into
	// one that does not, for a reason the operator cannot act on. Unknown
	// timings simply mean the window plan cannot be built, and the job takes
	// the whole-file path - which is exactly the behaviour that predates it.
	timings, err := s.sourceTimings(ctx, prober, lib.TargetFilePath, pair.Target.Index, donorPath)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("syncjob: source timing unknown; analysing the whole file",
				"job", rec.ID, "error", err)
		}
		timings = SourceTimings{}
	}
	s.logSourceTimings(rec.ID, timings)

	ffmpeg, err := s.resolveFFmpeg()
	if err != nil {
		return analysisRun{}, err
	}

	// ONE decision, here, before any decode: the setting asked for windows, the
	// container stated both durations and both origins, and the plan fits.
	// Anything else runs the whole tracks and says why.
	mode := decideAnalysisSource(cfg, timings)
	s.logAnalysisSource(rec.ID, mode)

	engine := s.engine
	if engine == nil {
		engine = sync.New(sync.WithTimeout(analyze.JobTimeout(cfg)), sync.WithLogger(s.logger))
	}
	engines := s.enginesFor(cfg, engine)

	jr := jobRun{
		jobID:    rec.ID,
		mode:     mode,
		cfg:      cfg,
		engines:  engines,
		timings:  timings,
		wholeSec: wholeDecodeSeconds(timings),
		req: analyze.Request{
			TargetPath:       filepath.Join(staging.Dir, "target.wav"),
			DonorPath:        filepath.Join(staging.Dir, "donor.wav"),
			DurationSec:      mode.DurationSec,
			TargetTracks:     targetTracks,
			VaultTracks:      vaultTracks,
			Policy:           policy,
			OriginalLanguage: lib.OriginalLanguageISO2,
		},
		decode: analysisDecode{
			ffmpeg:       ffmpeg,
			targetPath:   lib.TargetFilePath,
			targetStream: pair.Target.Index,
			donorPath:    donorPath,
			filter:       analysisFilter(pair.Method),
			targetWAV:    filepath.Join(staging.Dir, "target.wav"),
			donorWAV:     filepath.Join(staging.Dir, "donor.wav"),
		},
	}

	var run analysisRun
	if mode.Source == analyze.AnalysisSourceWindows {
		slicer := newWindowSlicer(windowSlicerSpec{
			ffmpeg:           ffmpeg,
			jobDir:           staging.Dir,
			targetPath:       lib.TargetFilePath,
			targetStream:     pair.Target.Index,
			donorPath:        donorPath,
			filter:           jr.decode.filter,
			targetCorrection: seekCorrection(timings.Target),
			donorCorrection:  seekCorrection(timings.Donor),
			decode:           s.windowDecode,
		})
		run, err = s.runWindowed(ctx, engine, jr, slicer)
	} else {
		run, err = s.runWhole(ctx, engine, jr)
	}
	if err != nil {
		return analysisRun{}, err
	}
	succeeded = true
	return run, nil
}

// analysisDecode is one job's decode inputs: the two analysis inputs, their
// streams and band filter, and the two WAV paths the whole-track path writes.
type analysisDecode struct {
	ffmpeg       string
	targetPath   string
	targetStream int
	donorPath    string
	filter       string
	targetWAV    string
	donorWAV     string
}

// jobRun is everything the analysis phase needs for one job, resolved once so
// neither path re-probes, re-plans or re-resolves anything.
type jobRun struct {
	jobID int64
	mode  analysisMode
	cfg   analyze.Config
	// engines registers the techniques this build can actually run. It is
	// resolved once, beside the engine itself, so a configured technique with
	// no engine is a report note rather than a per-checkpoint surprise.
	engines map[analyze.TechniqueID]analyze.Engine
	timings SourceTimings
	// wholeSec is how much audio the two whole-track decodes produce, in
	// seconds (0 when the containers stated no durations).
	wholeSec float64
	req      analyze.Request
	decode   analysisDecode
}

// Why a job did not request the windowed path. The first three are the mode
// decision's own reasons; the last is the plan refusing to place the windows.
const (
	reasonSettingFull   = "setting-full"
	reasonNoDuration    = "no-duration"
	reasonUnknownOrigin = "unknown-origin"
	reasonPlanInvalid   = "plan-invalid"
)

// Causes a fallback reason is built from, after its count:
// "2-suspect-windows", "1-short-window", "1-decode-error".
const (
	causeSuspectWindows = "suspect-windows"
	causeShortWindow    = "short-window"
	causeDecodeError    = "decode-error"
)

// analysisMode is the one decision a job makes before it decodes anything.
type analysisMode struct {
	// Source is what will be attempted: analyze.AnalysisSourceWindows or
	// analyze.AnalysisSourceFull.
	Source string
	// Reason is why the job did not request windows: one of
	// reasonSettingFull, reasonNoDuration, reasonUnknownOrigin,
	// reasonPlanInvalid. Empty when Source is windows.
	Reason string
	// DurationSec is the window plan's duration base: the shorter of the two
	// analysis inputs. It is 0 for a full mode chosen before any duration was
	// known, where Analyze derives its own base from the staged WAV headers.
	DurationSec float64
	// Checkpoints is how many windows the plan holds; it is what the mode log
	// line reports as `windows=N`.
	Checkpoints int
}

// decideAnalysisSource is the whole mode decision, in one place, so "why did
// this job run full?" has exactly one answer.
//
// Windows is chosen only when all three hold: the setting says windows, both
// inputs stated a duration and both of their timeline origins (a percentage of
// an unknown duration, or a seek with an unknown correction, is silent
// corruption), and the plan the job would run validates. Everything else runs
// the whole tracks with a reason, which the job row and the log both carry.
func decideAnalysisSource(cfg analyze.Config, timings SourceTimings) analysisMode {
	if cfg.AnalysisSource != analyze.AnalysisSourceWindows {
		return analysisMode{Source: analyze.AnalysisSourceFull, Reason: reasonSettingFull}
	}
	if timings.Target.Duration <= 0 || timings.Donor.Duration <= 0 {
		return analysisMode{Source: analyze.AnalysisSourceFull, Reason: reasonNoDuration}
	}
	if !timings.Target.Known || !timings.Donor.Known {
		return analysisMode{Source: analyze.AnalysisSourceFull, Reason: reasonUnknownOrigin}
	}
	planning := planTiming(timings)
	// The plan the job validates here is not kept: analyze.Analyze plans the
	// same windows again from the same duration, percentages and slice length
	// and returns them in the report, and the policy is judged on that one (see
	// windowsFromReport). This call is the gate, not a second plan.
	if _, err := analyze.PlanWindows(planning, cfg.CheckpointPositionsPct, sliceLenSec(cfg)); err != nil {
		return analysisMode{Source: analyze.AnalysisSourceFull, Reason: reasonPlanInvalid}
	}
	return analysisMode{
		Source:      analyze.AnalysisSourceWindows,
		DurationSec: planning.DurationSec(),
		Checkpoints: len(cfg.CheckpointPositionsPct),
	}
}

// planTiming projects the probed timings onto the planner's input.
func planTiming(t SourceTimings) analyze.Timing {
	return analyze.Timing{
		Target: analyze.SourceTiming{
			DurationSec:       t.Target.Duration.Seconds(),
			ContainerStartSec: t.Target.ContainerStart.Seconds(),
			TrackStartSec:     t.Target.TrackStart.Seconds(),
			OriginKnown:       t.Target.Known,
		},
		Donor: analyze.SourceTiming{
			DurationSec:       t.Donor.Duration.Seconds(),
			ContainerStartSec: t.Donor.ContainerStart.Seconds(),
			TrackStartSec:     t.Donor.TrackStart.Seconds(),
			OriginKnown:       t.Donor.Known,
		},
	}
}

// seekCorrection is how far one input's container seek must be shifted to land
// on the analysed track's timeline: TrackStart − ContainerStart. It is 0 for an
// unknown origin, and the job only seeks when both origins are known.
func seekCorrection(t SourceTiming) float64 {
	return t.TrackStart.Seconds() - t.ContainerStart.Seconds()
}

// sliceLenSec is the analysed slice length: the settled checkpoint window,
// widened to max_offset_sec + CheckWindowMarginSec so the engine's lag search
// reaches the full offset bound. It is analyze's own formula (searchWindowSec);
// the job needs it to validate a plan before committing to windows, and the two
// must agree or the plan checked here would not be the plan that runs.
func sliceLenSec(cfg analyze.Config) float64 {
	return math.Max(float64(cfg.CheckpointWindowSec), float64(cfg.MaxOffsetSec+analyze.CheckWindowMarginSec))
}

// wholeDecodeSeconds is how much audio the two whole-track decodes produce.
func wholeDecodeSeconds(t SourceTimings) float64 {
	return math.Max(0, t.Target.Duration.Seconds()) + math.Max(0, t.Donor.Duration.Seconds())
}

// windowsFromReport is the plan the analysis actually ran, as far as the
// policy needs it: position and length per checkpoint. Taking it from the
// report rather than planning again means the retry and the suspect test judge
// the windows that were decoded, never a second copy that could drift from
// them. Only those three fields are set; the slicer derives each side's seek
// from FilmStartSec and its own correction.
func windowsFromReport(report analyze.Report) []analyze.Window {
	windows := make([]analyze.Window, len(report.Checkpoints))
	for i, checkpoint := range report.Checkpoints {
		windows[i] = analyze.Window{
			PositionPct:  checkpoint.PositionPct,
			FilmStartSec: checkpoint.SliceStartSec,
			LenSec:       checkpoint.SliceLenSec,
		}
	}
	return windows
}

// maxLagMS is the engine's search bound in milliseconds: the longest planned
// window, which is the window_sec every checkpoint job was given.
func maxLagMS(windows []analyze.Window) float64 {
	longest := 0.0
	for _, w := range windows {
		if w.LenSec > longest {
			longest = w.LenSec
		}
	}
	return longest * 1000
}

// runWhole decodes both whole tracks exactly as the job always has — same argv,
// no slicer — and analyses them. The duration base is the mode's when the
// decision had one (the fallback, so its slice fields stay comparable with the
// windowed attempt) and Analyze's own WAV-derived base when it did not (the
// setting said full).
func (s *Service) runWhole(ctx context.Context, engine analyze.Engine, jr jobRun) (analysisRun, error) {
	decode := s.wholeDecode
	if decode == nil {
		decode = decodeAudio
	}
	if err := decode(ctx, jr.decode.ffmpeg, jr.decode.targetPath, jr.decode.targetStream,
		jr.decode.filter, jr.decode.targetWAV); err != nil {
		return analysisRun{}, fmt.Errorf("syncjob: decode target: %w", err)
	}
	if err := decode(ctx, jr.decode.ffmpeg, jr.decode.donorPath, 0,
		jr.decode.filter, jr.decode.donorWAV); err != nil {
		return analysisRun{}, fmt.Errorf("syncjob: decode donor: %w", err)
	}
	cfg := jr.cfg
	cfg.Slicer = nil
	cfg.AnalysisSource = analyze.AnalysisSourceFull
	cfg.Engines = jr.engines
	report, err := analyze.Analyze(ctx, engine, cfg, jr.req)
	if err != nil {
		return analysisRun{}, err
	}
	return analysisRun{
		Report:     report,
		Source:     analyze.AnalysisSourceFull,
		DecodedSec: jr.wholeSec,
	}, nil
}

// runWindowed runs the plan through the slicer and then applies the fallback
// policy: no suspect keeps the report, one suspect is re-decoded once at the
// longer pre-roll and judged against the windows that were never suspect, and
// anything the policy cannot repair goes full.
//
// Everything after the join is single-threaded: the workers have written their
// slots, the report exists, and the retry is one window's decode — so the
// decision cannot depend on which goroutine finished first.
func (s *Service) runWindowed(ctx context.Context, engine analyze.Engine, jr jobRun, slicer *windowSlicer) (analysisRun, error) {
	cfg := jr.cfg
	cfg.Slicer = slicer
	cfg.AnalysisSource = analyze.AnalysisSourceWindows
	cfg.Engines = jr.engines
	report, err := analyze.Analyze(ctx, engine, cfg, jr.req)
	if err != nil {
		// A window that could not be decoded or was cut short is not an
		// analysis failure: the policy answers it with one re-decode at the
		// longer pre-roll and then with a whole-track decode. Only a failure
		// the engine owns ends the job.
		if !windowSuspect(slicer, err) {
			return analysisRun{}, err
		}
		failures := 1
		if w, _, ok := slicer.Failure(); ok {
			// There is no report to repair (the pass aborted), so the re-decode
			// cannot rescue this job — but it is the policy's answer to one
			// untrustworthy window, and its outcome is what makes the reason a
			// count: "2-short-window" when the longer pre-roll fails too.
			if _, retryErr := s.retryWindow(ctx, engine, cfg, slicer, w); retryErr != nil {
				failures++
			}
		}
		return s.fallback(ctx, engine, jr, slicer.DecodedSeconds(),
			fmt.Sprintf("%d-%s", failures, windowCause(err)))
	}

	windows := windowsFromReport(report)
	suspects := analyze.Suspects(windows, report.OffsetsMS, report.Qualities, maxLagMS(windows))
	switch {
	case len(suspects) == 0:
		return analysisRun{
			Report:     report,
			Source:     analyze.AnalysisSourceWindows,
			DecodedSec: slicer.DecodedSeconds(),
		}, nil
	case analyze.RequiresFullDecode(suspects):
		return s.fallback(ctx, engine, jr, slicer.DecodedSeconds(),
			fmt.Sprintf("%d-%s", len(suspects), causeSuspectWindows))
	}

	suspect := suspects[0]
	w := windows[suspect.Index]
	retried, retryErr := s.retryWindow(ctx, engine, cfg, slicer, w)
	if retryErr != nil {
		return s.fallback(ctx, engine, jr, slicer.DecodedSeconds(),
			fmt.Sprintf("1-%s", windowCause(retryErr)))
	}
	if !acceptsRetry(windows, report.OffsetsMS, suspect.Index, w, retried.OffsetMS) {
		return s.fallback(ctx, engine, jr, slicer.DecodedSeconds(),
			fmt.Sprintf("1-%s", causeSuspectWindows))
	}
	// What was measured replaces what was guessed at: the report states the
	// retried offset and quality. The verdict stays the analysis's own — the
	// retry is accepted only within the stable band of the timeline that verdict
	// was computed from, so it cannot contradict the offsets it was drawn from,
	// and re-judging a partially different evidence set is not this layer's job.
	//
	// The row also names the technique that answered it. A retry always
	// re-decodes with the waveform engine (see [retryTechnique]), so an mfcc row
	// that gets retried must stop claiming the hop: otherwise it carries sample
	// precision under a label that says ±10 ms, and the report keeps a precision
	// no row accounts for.
	return analysisRun{
		Report:     replaceCheckpoint(report, suspect.Index, retryTechnique, retried),
		Source:     analyze.AnalysisSourceWindows,
		DecodedSec: slicer.DecodedSeconds(),
	}, nil
}

// fallback escalates one job to the whole-track path and records why. The
// whole decode is the only part of the job that reads the tracks: everything is
// cut in the same plan, so the report stays comparable with the windowed
// attempt it replaces.
func (s *Service) fallback(ctx context.Context, engine analyze.Engine, jr jobRun, windowsDecoded float64, reason string) (analysisRun, error) {
	s.logAnalysisFallback(jr.jobID, reason, jr.timings)
	run, err := s.runWhole(ctx, engine, jr)
	if err != nil {
		return analysisRun{}, err
	}
	run.Fallback = reason
	run.DecodedSec += windowsDecoded
	return run, nil
}

// retryWindow decodes one flagged window again at the retry pre-roll and runs
// the engine for that window only. It is analyze.Analyze's per-checkpoint body
// for a single window: the same slice, the same job shape, the same echo check.
// (Analyze runs a whole plan and offers no single-window entry point, so this
// is the one place the job reproduces it.)
func (s *Service) retryWindow(ctx context.Context, engine analyze.Engine, cfg analyze.Config, slicer *windowSlicer, w analyze.Window) (sync.Result, error) {
	slice, err := slicer.Retry(ctx, w)
	if err != nil {
		return sync.Result{}, err
	}
	defer slicer.Release(w)

	length := slice.LenSec
	job := sync.Job{
		TargetPath:    slice.TargetPath,
		DonorPath:     slice.DonorPath,
		SliceStartSec: slice.EngineStartSec,
		SliceLenSec:   slice.LenSec,
		WindowSec:     &length,
	}
	jobCtx, cancel := context.WithTimeout(ctx, analyze.JobTimeout(cfg))
	defer cancel()
	result, err := engine.Run(jobCtx, job)
	if err != nil {
		return sync.Result{}, err
	}
	if result.Analyzed.Len < w.LenSec-1.0/windowSampleRate {
		return sync.Result{}, fmt.Errorf("%w: analyzed.len %.6f s, planned %.6f s",
			analyze.ErrShortWindow, result.Analyzed.Len, w.LenSec)
	}
	return result, nil
}

// acceptsRetry judges one re-decoded window against the reference built from
// the windows that were never suspect.
func acceptsRetry(windows []analyze.Window, offsets []float64, index int, w analyze.Window, offsetMS float64) bool {
	reference := make([]analyze.Window, 0, len(windows)-1)
	referenceOffsets := make([]float64, 0, len(windows)-1)
	for i := range windows {
		if i == index {
			continue
		}
		reference = append(reference, windows[i])
		referenceOffsets = append(referenceOffsets, offsets[i])
	}
	return analyze.Accepts(reference, referenceOffsets, w, offsetMS)
}

// retryTechnique is the technique a retried window is re-measured with: the
// waveform runner, which is the engine the map registers under
// [analyze.TechniqueWaveform] and the one a retry deliberately keeps. A retry
// re-decodes a suspect window exactly; a hop-quantised second opinion is not
// what a re-measurement is for, so the engine is unchanged and only its name is
// recorded.
const retryTechnique analyze.TechniqueID = analyze.TechniqueWaveform

// replaceCheckpoint writes an accepted retry into the report at its own index,
// leaving every other checkpoint untouched.
//
// The 0.5.0 diagnostics are replaced with the retry's own: a z or a sub-sample
// offset left over from the attempt this one replaces would describe a peak the
// report no longer carries. The attempt history follows the row (see
// [retriedAttempts]), so the disclosure cannot keep describing the answer the
// retry displaced.
//
// technique is the matcher that produced the retry, and the row names it: a row
// carrying the retry's sample precision must not keep the hop-quantised label
// of the answer it replaced. The report's precision is recomputed from the rows
// after the write, because the flat and aggregate views must describe the same
// answers as the checkpoints do (see [coarsestPrecision]).
func replaceCheckpoint(report analyze.Report, index int, technique analyze.TechniqueID, result sync.Result) analyze.Report {
	report.Checkpoints[index].Attempts = retriedAttempts(report.Checkpoints[index].Attempts, technique, result)
	report.Checkpoints[index].Technique = string(technique)
	report.Checkpoints[index].OffsetMS = result.OffsetMS
	report.Checkpoints[index].Quality = result.CorrelationQuality
	report.Checkpoints[index].OffsetSubMS = result.OffsetSubMS
	report.Checkpoints[index].MatchZ = result.MatchZ
	report.Checkpoints[index].PrecisionMS = result.PrecisionMS
	report.OffsetsMS[index] = result.OffsetMS
	report.Qualities[index] = result.CorrelationQuality
	if index < len(report.MatchZ) {
		report.MatchZ[index] = result.MatchZ
	}
	report.PrecisionMS = coarsestPrecision(report.Checkpoints)
	return report
}

// coarsestPrecision is the report's precision: the largest sample period among
// the checkpoints that recorded one, so a report never claims more resolution
// than its worst answer. A checkpoint with 0 recorded no precision (a 0.4.0
// row) and contributes nothing rather than making the whole report claim none.
func coarsestPrecision(checkpoints []analyze.Checkpoint) float64 {
	precision := 0.0
	for _, checkpoint := range checkpoints {
		if checkpoint.PrecisionMS > precision {
			precision = checkpoint.PrecisionMS
		}
	}
	return precision
}

// retriedAttempts rewrites one checkpoint's history so that it describes the
// answer the retry put in the row.
//
// The winner is the attempt the row was drawn from: the accepted one, or — when
// nothing was accepted — the healthiest answer, which is the rule the pipeline
// itself fills a row with. It takes the retry's numbers and the technique that
// produced them: a retry is a re-measurement of the same window, so the entry
// describes that measurement, and leaving the displaced technique's label on it
// would state a matcher that did not answer. Every other attempt keeps its own
// numbers and loses the accepted flag, so at most one attempt per checkpoint
// says it was the answer.
//
// No winner at all means the history cannot describe the new answer — a
// pre-0.5.0 row (no attempts) or one where every technique errored — and it is
// left empty rather than carrying numbers nobody measured.
func retriedAttempts(attempts []analyze.Attempt, technique analyze.TechniqueID, result sync.Result) []analyze.Attempt {
	if len(attempts) == 0 {
		return nil
	}
	winner := -1
	for i, attempt := range attempts {
		if attempt.Accepted {
			winner = i
			break
		}
	}
	if winner < 0 {
		for i, attempt := range attempts {
			if attempt.Error != "" {
				continue
			}
			if winner < 0 || attempt.Quality > attempts[winner].Quality {
				winner = i
			}
		}
	}
	if winner < 0 {
		return nil
	}
	out := make([]analyze.Attempt, len(attempts))
	copy(out, attempts)
	for i := range out {
		out[i].Accepted = i == winner
	}
	out[winner].Technique = string(technique)
	out[winner].OffsetMS = result.OffsetMS
	out[winner].OffsetSubMS = result.OffsetSubMS
	out[winner].Quality = result.CorrelationQuality
	out[winner].MatchZ = result.MatchZ
	out[winner].PrecisionMS = result.PrecisionMS
	return out
}

// windowSuspect reports whether a windowed pass failed on a window rather than
// on the analysis: the slicer recorded a decode or frame-gate failure, or the
// engine's echo says it analysed less audio than the plan asked for.
func windowSuspect(slicer *windowSlicer, err error) bool {
	if _, _, ok := slicer.Failure(); ok {
		return true
	}
	return errors.Is(err, analyze.ErrShortWindow)
}

// windowCause names a window failure for the fallback reason: a short window is
// the frame gate or the engine's echo, anything else is a decode failure.
func windowCause(err error) string {
	var short *ErrShortWindow
	if errors.As(err, &short) || errors.Is(err, analyze.ErrShortWindow) {
		return causeShortWindow
	}
	return causeDecodeError
}

// logAnalysisSource records the mode decision once, with the job id, so
// `grep 'job 12'` answers "which mode did this job use, and why?".
func (s *Service) logAnalysisSource(jobID int64, mode analysisMode) {
	if s.logger == nil {
		return
	}
	if mode.Source == analyze.AnalysisSourceWindows {
		s.logger.Info(fmt.Sprintf("syncjob: job %d analysis_source=windows windows=%d preroll=%gs",
			jobID, mode.Checkpoints, WindowPrerollSec))
		return
	}
	s.logger.Info(fmt.Sprintf("syncjob: job %d analysis_source=full reason=%s", jobID, mode.Reason))
}

// logAnalysisFallback records the escalation, the reason and what it cost to
// decode: the two whole tracks, per side.
func (s *Service) logAnalysisFallback(jobID int64, reason string, timings SourceTimings) {
	if s.logger == nil {
		return
	}
	s.logger.Info(fmt.Sprintf("syncjob: job %d analysis fallback=full reason=%s decoded=%ds+%ds",
		jobID, reason,
		int64(timings.Target.Duration.Seconds()), int64(timings.Donor.Duration.Seconds())))
}

// probeMediaInfo is the media-info probe reference resolution runs: the test
// seam when set, else the real prober.
func (s *Service) probeMediaInfo(ctx context.Context, prober *probe.Prober, path string) (probe.MediaInfo, error) {
	if s.probeTarget != nil {
		return s.probeTarget(ctx, path)
	}
	info, err := prober.Probe(ctx, path)
	if err != nil {
		return probe.MediaInfo{}, err
	}
	return *info, nil
}

// sourceTimings probes the two files the job will decode and projects what the
// container says about them: the analysed track's length and both timeline
// origins. It is the only timing probe a job performs, and it runs before any
// decode.
//
// The target is lib.TargetFilePath read at the reference pair's container
// stream index, so the number describes the stream the decoder will map, not
// the longest stream in the remux. The donor is the staged audio-only .mka,
// whose single stream is the whole file; its format duration is exact there.
func (s *Service) sourceTimings(ctx context.Context, prober *probe.Prober, targetPath string, targetIndex int, donorPath string) (SourceTimings, error) {
	probeFn := s.probeTimings
	if probeFn == nil {
		probeFn = func(ctx context.Context, path string) (probe.MediaInfo, error) {
			info, err := prober.Probe(ctx, path)
			if err != nil {
				return probe.MediaInfo{}, err
			}
			return *info, nil
		}
	}
	targetInfo, err := probeFn(ctx, targetPath)
	if err != nil {
		return SourceTimings{}, fmt.Errorf("syncjob: probe target timings: %w", err)
	}
	donorInfo, err := probeFn(ctx, donorPath)
	if err != nil {
		return SourceTimings{}, fmt.Errorf("syncjob: probe donor timings: %w", err)
	}
	donorIndex := 0
	if len(donorInfo.Tracks) > 0 {
		donorIndex = donorInfo.Tracks[0].Index
	}
	return SourceTimings{
		Target: sourceTiming(targetInfo, targetIndex),
		Donor:  sourceTiming(donorInfo, donorIndex),
	}, nil
}

// sourceTiming projects one probe result onto the numbers the window planner
// consumes. Known stays false unless the container stated all three, so an
// absent duration or origin can never pass for a real 0.
func sourceTiming(info probe.MediaInfo, index int) SourceTiming {
	duration, durationKnown := info.TrackDuration(index)
	timing := SourceTiming{
		Duration:       duration,
		ContainerStart: info.StartTime,
	}
	for _, track := range info.Tracks {
		if track.Index != index {
			continue
		}
		timing.TrackStart = track.StartTime
		timing.Known = durationKnown && info.StartTimeKnown && track.StartTimeKnown
		return timing
	}
	return timing
}

// logSourceTimings records the probed timings once per job. The line is what
// makes the mode decision auditable ("why did this job run full?"), and it is
// written before that decision is taken.
func (s *Service) logSourceTimings(jobID int64, timings SourceTimings) {
	if s.logger == nil {
		return
	}
	s.logger.Info("syncjob: source timing",
		"job", jobID,
		"target_duration_s", timings.Target.Duration.Seconds(),
		"target_container_start_s", timings.Target.ContainerStart.Seconds(),
		"target_track_start_s", timings.Target.TrackStart.Seconds(),
		"target_known", timings.Target.Known,
		"donor_duration_s", timings.Donor.Duration.Seconds(),
		"donor_container_start_s", timings.Donor.ContainerStart.Seconds(),
		"donor_track_start_s", timings.Donor.TrackStart.Seconds(),
		"donor_known", timings.Donor.Known,
	)
}

// trackAudioTrack adapts a persisted vault track to the probe shape reflang
// consumes. Vault tracks are single-stream files, so Index is their position
// in the staged group.
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

func (s *Service) configuredWorkDir(ctx context.Context) string {
	if s.settings == nil {
		return ""
	}
	raw, ok, err := s.settings.Get(ctx, settings.KeyStoragePaths)
	if err != nil || !ok {
		return ""
	}
	var paths settings.StoragePaths
	if err := json.Unmarshal(raw, &paths); err != nil {
		return ""
	}
	return paths.WorkDir
}

func (s *Service) analysisPolicy(ctx context.Context) (reflang.Languages, analyze.Config) {
	policy := reflang.Languages{}
	cfg := analyze.DefaultConfig()
	// The shipped strategy is the default until a row says otherwise, so a job
	// with no settings service (or an unreadable row) still runs the 0.4.0
	// pipeline.
	cfg.Strategy = analyze.DefaultStrategy()
	if s.settings == nil {
		return policy, cfg
	}
	if raw, ok, err := s.settings.Get(ctx, settings.KeyLanguagesPolicy); err == nil && ok {
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
	if raw, ok, err := s.settings.Get(ctx, settings.KeySyncDefaults); err == nil && ok {
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
			// UnmarshalJSON already resolved an absent key to
			// DefaultParallelCheckpoints; the clamp against 0 is defence in
			// depth for a row that skipped validation.
			if sd.ParallelCheckpoints > 0 {
				cfg.ParallelCheckpoints = sd.ParallelCheckpoints
			}
			// UnmarshalJSON already resolved an absent key to
			// DefaultAnalysisSource, and the settings API serves a materialised
			// value. This switch is the defence in depth for a row that never
			// went through either: a direct Store.Get in a test, an imported
			// backup. "" (and anything unrecognised, which validation forbids)
			// must mean the compiled default, never "no mode".
			switch sd.AnalysisSource {
			case analyze.AnalysisSourceWindows:
				cfg.AnalysisSource = analyze.AnalysisSourceWindows
			case analyze.AnalysisSourceFull:
				cfg.AnalysisSource = analyze.AnalysisSourceFull
			default:
				cfg.AnalysisSource = settings.DefaultAnalysisSource
			}
		}
	}
	if raw, ok, err := s.settings.Get(ctx, settings.KeySyncStrategy); err == nil && ok {
		var so settings.StrategyOptions
		if json.Unmarshal(raw, &so) == nil {
			cfg.Strategy = sanitizeStrategy(analyze.Strategy(so))
		}
	}
	return policy, cfg
}

// enginesFor registers every technique this build can actually run, by id: the
// waveform runner passed in (a test's injected engine wins, as it always has)
// and MFCC's, built only when the strategy names the technique. The map never
// carries a placeholder, so a configured `mfcc` entry it does not hold stays
// analyze.Analyze's own verdict — a loud "no enabled technique has an engine"
// when nothing else can answer, and a report note otherwise. The waveform
// answer is never substituted for it.
//
// It is the composition root's whole decision about which matchers a job owns,
// in one place, so "did this job build a second engine, and which one?" is
// answerable without running a job.
func (s *Service) enginesFor(cfg analyze.Config, waveform analyze.Engine) map[analyze.TechniqueID]analyze.Engine {
	engines := map[analyze.TechniqueID]analyze.Engine{
		analyze.TechniqueWaveform: waveform,
	}
	if !strategyUses(cfg.Strategy, analyze.TechniqueMFCC) {
		return engines
	}
	engines[analyze.TechniqueMFCC] = sync.New(
		sync.WithCorrelator(mfcc.Analyze),
		sync.WithTimeout(analyze.JobTimeout(cfg)),
		sync.WithLogger(s.logger),
	)
	return engines
}

// strategyUses reports whether the strategy enables one technique. It is what
// decides which engines the composition root builds: an entry the strategy
// disables never runs, so its engine would never be called, and building one
// anyway would put a technique in the map that this job does not use.
//
// The list read is [analyze.Strategy.Enabled]'s, the same one the pipeline
// routes on, so "uses" means exactly "will be asked for". A zero strategy
// enables nothing and therefore asks for nothing: analyze resolves that to the
// shipped waveform-only strategy.
func strategyUses(s analyze.Strategy, id analyze.TechniqueID) bool {
	for _, entry := range s.Enabled() {
		if entry.ID == id {
			return true
		}
	}
	return false
}

// sanitizeStrategy drops what this build cannot run and keeps the rest.
//
// A stored row can name a technique from a newer release than the binary, or a
// list that skipped validation (a direct insert, an imported backup). Neither is
// a reason to fail a job: an unknown id goes, the rest stays, and a list left
// with nothing runnable — or with no enabled entry, or with an unsound first
// step — falls back to the shipped strategy. The pipeline is never empty.
func sanitizeStrategy(s analyze.Strategy) analyze.Strategy {
	out := analyze.Strategy{
		Preset:     s.Preset,
		Techniques: make([]analyze.Technique, 0, len(s.Techniques)),
		// The piecewise block rides along unchanged (#84): it is not a
		// technique, so there is nothing here to filter, and dropping it would
		// silently switch a job the operator opted in for back to six
		// checkpoints. A row that predates the block decodes to the documented
		// default ("off"), which is what [analyze.DefaultStrategy] ships.
		Piecewise: s.Piecewise,
	}
	for _, entry := range s.Techniques {
		if !entry.ID.Known() || !entry.When.Known() {
			continue
		}
		out.Techniques = append(out.Techniques, entry)
	}
	if len(out.Techniques) == 0 {
		return analyze.DefaultStrategy()
	}
	if out.Preset == "" {
		// A row without a label is whatever its list is; the settings layer
		// derives the name on the way in, so an unlabelled row here is one
		// that skipped it.
		out.Preset = analyze.PresetCustom
	}
	if err := out.Validate(); err != nil {
		return analyze.DefaultStrategy()
	}
	return out
}

func (s *Service) resolveFFmpeg() (string, error) {
	if s.ffmpegPath != "" {
		return s.ffmpegPath, nil
	}
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", fmt.Errorf("syncjob: ffmpeg not found on PATH: %w", err)
	}
	return path, nil
}

// analysisFilter shapes the correlation band: speech for a same-language
// pair, a 30-200 Hz low-pass for the cross-language fallback.
func analysisFilter(method reflang.Method) string {
	if method == reflang.LowpassFallback {
		return "highpass=f=30,lowpass=f=200"
	}
	return "highpass=f=80,lowpass=f=3500"
}

// decodeRequest is one analysis decode. SeekSec and DurationSec are nil for the
// whole-track decode, which reads the input from its first sample to its last;
// a window decode sets both.
type decodeRequest struct {
	Input  string
	Stream int
	Filter string
	Output string
	// SeekSec is an input-side seek: it goes before -i so ffmpeg jumps instead
	// of decoding and discarding, which is the only affordable form on a 2160p
	// remux. Nil means "from the start of the file".
	SeekSec *float64
	// DurationSec is the output limit (-t), counted from the produced file's
	// first frame — i.e. from the seek, not from the file's start. Nil means
	// "to the end of the input".
	DurationSec *float64
}

// decodeArgs builds the ffmpeg argv for req. It is the only place this argv
// exists: the whole-track decode and a window decode must hand the engine
// identical samples (-ac 1 -ar 8000 -af <band> -c:a pcm_f32le), and two copies
// of the list would drift apart one flag at a time.
func decodeArgs(req decodeRequest) []string {
	args := []string{"-v", "error", "-nostdin", "-y"}
	if req.SeekSec != nil {
		args = append(args, "-ss", decodeSeconds(*req.SeekSec))
	}
	args = append(args,
		"-i", req.Input,
		"-map", "0:"+strconv.Itoa(req.Stream),
	)
	if req.DurationSec != nil {
		args = append(args, "-t", decodeSeconds(*req.DurationSec))
	}
	return append(args,
		"-ac", "1", "-ar", strconv.Itoa(windowSampleRate),
		"-af", req.Filter,
		"-c:a", "pcm_f32le",
		req.Output,
	)
}

// decodeSeconds formats a seek or a duration for the argv: fixed six decimals,
// because a planned boundary is a whole sample (125 µs) and a seek that rounds
// to something else reads the wrong sample.
func decodeSeconds(sec float64) string {
	return strconv.FormatFloat(sec, 'f', 6, 64)
}

// runDecode runs one decode and turns ffmpeg's stderr into the error the job
// status shows.
func runDecode(ctx context.Context, ffmpeg string, req decodeRequest) error {
	cmd := exec.CommandContext(ctx, ffmpeg, decodeArgs(req)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s: %s", filepath.Base(req.Input), msg)
	}
	return nil
}

// decodeAudio extracts one audio stream as mono 8 kHz float PCM, the format the
// engine expects.
func decodeAudio(ctx context.Context, ffmpeg, input string, stream int, filter, output string) error {
	return runDecode(ctx, ffmpeg, decodeRequest{
		Input:  input,
		Stream: stream,
		Filter: filter,
		Output: output,
	})
}

func medianOffset(offsets []float64) (int64, error) {
	if len(offsets) == 0 {
		return 0, errors.New("syncjob: analysis produced no offsets")
	}
	sorted := append([]float64(nil), offsets...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	median := sorted[mid]
	if len(sorted)%2 == 0 {
		median = (sorted[mid-1] + sorted[mid]) / 2
	}
	return int64(math.Round(median)), nil
}
