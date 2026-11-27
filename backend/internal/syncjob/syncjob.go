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

// Public status vocabulary of GET /api/sync/jobs (PLAN.md M4-6). The schema
// status is mapped onto it so the API never leaks M5-internal states.
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
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
}

// RegisterRoutes mounts:
//
//	POST /api/sync/jobs         create a queued job (eligibility gate)
//	GET  /api/sync/jobs         list jobs, newest first
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
	ErrorMessage       string          `json:"error_message,omitempty"`
	CreatedAt          string          `json:"created_at,omitempty"`
	UpdatedAt          string          `json:"updated_at,omitempty"`
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
	recs, err := s.store.ListSyncJobs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read sync jobs")
		return
	}
	items := make([]JobInfo, 0, len(recs))
	for _, rec := range recs {
		items = append(items, jobInfo(rec))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
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
		ErrorMessage:         rec.ErrorMessage,
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
	report, execErr := s.runAnalysis(ctx, rec)
	if execErr != nil {
		_ = s.store.SetSyncJobStatus(context.WithoutCancel(ctx), jobID, statusFailed, execErr.Error())
		return analyze.Report{}, execErr
	}
	offset, err := medianOffset(report.OffsetsMS)
	if err != nil {
		_ = s.store.SetSyncJobStatus(context.WithoutCancel(ctx), jobID, statusFailed, err.Error())
		return analyze.Report{}, err
	}
	if err := s.store.FinishSyncJobAnalysis(ctx, jobID, statusCompleted, string(report.SyncMethod), offset, report, ""); err != nil {
		return analyze.Report{}, err
	}
	return report, nil
}

func (s *Service) runAnalysis(ctx context.Context, rec store.SyncJobRecord) (analyze.Report, error) {
	track, ok, err := s.store.VaultTrackByID(ctx, rec.TargetTrackID)
	if err != nil {
		return analyze.Report{}, err
	}
	if !ok {
		return analyze.Report{}, fmt.Errorf("syncjob: vault track %d not found", rec.TargetTrackID)
	}
	lib, ok, err := s.store.LibraryCacheJobTargetByID(ctx, rec.TargetLibraryCacheID)
	if err != nil {
		return analyze.Report{}, err
	}
	if !ok || lib.TargetFilePath == "" {
		return analyze.Report{}, fmt.Errorf("syncjob: library target %d has no file", rec.TargetLibraryCacheID)
	}
	if _, err := os.Stat(lib.TargetFilePath); err != nil {
		return analyze.Report{}, fmt.Errorf("syncjob: target file: %w", err)
	}

	workDir := s.workDir
	if workDir == "" {
		workDir = s.configuredWorkDir(ctx)
	}
	if workDir == "" {
		return analyze.Report{}, errors.New("syncjob: storage.paths.work_dir is not configured")
	}

	siblings, err := s.store.SiblingVaultTracks(ctx, track.VaultMediaID, track.GroupUUID, rec.TargetTrackID)
	if err != nil {
		return analyze.Report{}, err
	}
	ids := make([]int64, 0, len(siblings))
	for _, sib := range siblings {
		ids = append(ids, sib.ID)
	}

	stager, err := workdir.New(workDir, s.store)
	if err != nil {
		return analyze.Report{}, err
	}
	staging, err := stager.Stage(ctx, workdir.Job{
		ID:                rec.ID,
		TargetTrackID:     rec.TargetTrackID,
		ReferenceTrackIDs: ids,
	})
	if err != nil {
		return analyze.Report{}, err
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
	info, err := prober.Probe(ctx, lib.TargetFilePath)
	if err != nil {
		return analyze.Report{}, fmt.Errorf("syncjob: probe target: %w", err)
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
		return analyze.Report{}, fmt.Errorf("syncjob: no reference strategy: %s", resolved.Reason)
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
		return analyze.Report{}, fmt.Errorf("syncjob: staged copy of track %d is missing", donorID)
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
		return analyze.Report{}, err
	}
	targetWAV := filepath.Join(staging.Dir, "target.wav")
	donorWAV := filepath.Join(staging.Dir, "donor.wav")
	filter := analysisFilter(pair.Method)
	if err := decodeAudio(ctx, ffmpeg, lib.TargetFilePath, pair.Target.Index, filter, targetWAV); err != nil {
		return analyze.Report{}, fmt.Errorf("syncjob: decode target: %w", err)
	}
	if err := decodeAudio(ctx, ffmpeg, donorPath, 0, filter, donorWAV); err != nil {
		return analyze.Report{}, fmt.Errorf("syncjob: decode donor: %w", err)
	}

	engine := s.engine
	if engine == nil {
		engine = sync.New(sync.WithTimeout(analyze.JobTimeout(cfg)), sync.WithLogger(s.logger))
	}
	req := analyze.Request{
		TargetPath: targetWAV,
		DonorPath:  donorWAV,
		// The whole-file path: analyze derives the duration base from the
		// headers of these two WAVs. The container-side numbers probed above
		// are not consumed by this path.
		DurationSec:      0,
		TargetTracks:     targetTracks,
		VaultTracks:      vaultTracks,
		Policy:           policy,
		OriginalLanguage: lib.OriginalLanguageISO2,
	}
	report, err := analyze.Analyze(ctx, engine, cfg, req)
	if err != nil {
		return analyze.Report{}, err
	}
	succeeded = true
	return report, nil
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

// logSourceTimings records the probed timings once per job. Nothing acts on
// them yet; the line is what makes a later mode decision auditable ("why did
// this job run full?").
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
	return policy, cfg
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
