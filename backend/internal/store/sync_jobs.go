package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Sync job statuses persisted in sync_mux_jobs.status. Migration 0008 added
// held and cancelled; only SyncJobQueued is claimable, so a held job is
// invisible to the worker until ReleaseHeldJobs promotes it.
const (
	SyncJobQueued         = "queued"
	SyncJobHeld           = "held"
	SyncJobAnalyzing      = "analyzing"
	SyncJobReadyForReview = "ready_for_review"
	SyncJobMuxing         = "muxing"
	SyncJobCompleted      = "completed"
	SyncJobFailed         = "failed"
	SyncJobCancelled      = "cancelled"
)

// The held/cancel reason vocabulary is closed on purpose: the UI translates the
// code, so a free-text error string must never reach held_reason.
const (
	// HeldReasonWaitingForTags means the target does not carry the configured
	// tags yet; the projection releases the job when it does.
	HeldReasonWaitingForTags = "waiting_for_tags"
	// HeldReasonMissingTarget means the approved target file is no longer on
	// disk; the projection releases the job when a file reappears.
	HeldReasonMissingTarget = "missing_target"
	// HeldReasonTargetChanged means the arr row now points at a different file,
	// so the intent has to be re-derived before it muxes.
	HeldReasonTargetChanged = "target_changed"
	// CancelReasonRejection means the import_queue row was rejected, which
	// cancels the intent approval created.
	CancelReasonRejection = "cancelled_by_rejection"
)

// validHoldReason reports whether reason may be recorded by HoldSyncJob.
func validHoldReason(reason string) bool {
	switch reason {
	case HeldReasonWaitingForTags, HeldReasonMissingTarget, HeldReasonTargetChanged:
		return true
	}
	return false
}

// validCancelReason reports whether reason may be recorded by a cancellation.
func validCancelReason(reason string) bool {
	return reason == CancelReasonRejection || reason == HeldReasonTargetChanged
}

// SyncJobRecord is one row of sync_mux_jobs (schema.sql) as the sync
// job API needs it. Status holds the persisted schema value
// (queued/held/analyzing/ready_for_review/muxing/completed/failed/cancelled);
// the API layer maps it onto the public queued/running/done/failed vocabulary.
type SyncJobRecord struct {
	ID                   int64
	TargetTrackID        int64
	TargetLibraryCacheID int64
	TargetVideoPath      string
	TargetLangISO2       string
	Status               string
	SyncMethod           string
	CalculatedOffsetMS   int64
	CheckpointsJSON      string
	ErrorMessage         string
	CreatedAt            string
	UpdatedAt            string
	WorkDirPath          string
	// OutputPath is where the mux published the file (ADR-0011). In replace
	// mode it is the Target; in separate mode it is the file inside
	// storage.paths.output_dir. Empty until a mux has completed.
	OutputPath string
	// AnalysisSource is the analysis mode the job actually used
	// (analyze.AnalysisSourceWindows or analyze.AnalysisSourceFull; full after
	// a fallback). AnalysisFallback is why a windowed request did not run as
	// windows ("2-suspect-windows", "1-short-window", ...), empty when the
	// requested mode ran as requested. Both are empty on a row analysed before
	// migration 0007.
	AnalysisSource   string
	AnalysisFallback string
	// ForceIgnoreTagFilter is the persisted operator override (migration 0008):
	// the job runs regardless of the gate and is never re-held.
	ForceIgnoreTagFilter bool
	// HeldReason is the closed-vocabulary code explaining a hold or a
	// cancellation, empty on a row that has neither.
	HeldReason string
	// OriginQueueID is the import_queue row approval created this job for; 0
	// for a manual Studio job.
	OriginQueueID int64
}

// HeldJob is one held row plus everything the eligibility projection needs,
// joined once so the projection performs no N+1 queries.
//
// ArrTitle, ArrRootFolder and ArrTags come from the job's arr_library_cache row
// (title, root_folder_path and the decoded arr_tag_labels_json snapshot); they
// are empty and HasArrRow is false when no row maps to the target, which the
// gate reads as "no arr metadata" and fails closed on when the arr source is
// active (ADR-0009).
type HeldJob struct {
	ID                   int64
	TargetVideoPath      string
	TargetLibraryCacheID sql.NullInt64
	ForceIgnoreTagFilter bool
	HeldReason           string
	ArrTitle             string
	ArrRootFolder        string
	ArrTags              []string
	HasArrRow            bool
}

// SyncJobCreate is the insert shape for POST /api/sync/jobs and for the intent
// approval records.
type SyncJobCreate struct {
	VaultTrackID         int64
	TargetLibraryCacheID int64
	TargetVideoPath      string
	TargetLangISO2       string
	// Status is the state the intent is recorded in: "" means queued, and
	// SyncJobHeld is used when the gate refuses the target right now.
	Status string
	// ForceIgnoreTagFilter records the operator's override instead of deriving
	// it again. A forced job is inserted queued and the projection never
	// re-holds it, so it can never be combined with Status held.
	ForceIgnoreTagFilter bool
	// HeldReason is required when Status is held (closed vocabulary).
	HeldReason string
	// OriginQueueID is the import_queue row this approval created the job for;
	// nil for a manual Studio job.
	OriginQueueID *int64
}

// CreateSyncJob inserts a job and returns the persisted row. The status is
// queued unless the caller records a held intent, which is how approval stops
// deciding eligibility and starts recording it.
func (s *Store) CreateSyncJob(ctx context.Context, c SyncJobCreate) (SyncJobRecord, error) {
	if c.VaultTrackID <= 0 || c.TargetLibraryCacheID <= 0 {
		return SyncJobRecord{}, errors.New("store: sync job ids must be positive")
	}
	if c.TargetVideoPath == "" || c.TargetLangISO2 == "" {
		return SyncJobRecord{}, errors.New("store: sync job target path and language are required")
	}

	status := c.Status
	if status == "" {
		status = SyncJobQueued
	}
	heldReason := ""
	switch status {
	case SyncJobQueued:
		// A queued job carries no hold: the reason belongs to the state.
	case SyncJobHeld:
		if c.ForceIgnoreTagFilter {
			return SyncJobRecord{}, errors.New("store: a forced sync job cannot be held")
		}
		if !validHoldReason(c.HeldReason) {
			return SyncJobRecord{}, fmt.Errorf("store: sync job held reason %q is not in the vocabulary", c.HeldReason)
		}
		heldReason = c.HeldReason
	default:
		return SyncJobRecord{}, fmt.Errorf("store: sync job cannot be created in status %q", status)
	}

	var origin any
	if c.OriginQueueID != nil {
		if *c.OriginQueueID <= 0 {
			return SyncJobRecord{}, errors.New("store: sync job origin queue id must be positive")
		}
		origin = *c.OriginQueueID
	}

	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO sync_mux_jobs
		    (target_track_id, target_library_cache_id, target_video_path,
		     target_lang_iso2, status, force_ignore_tag_filter, held_reason,
		     origin_queue_id)
		VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?)`,
		c.VaultTrackID, c.TargetLibraryCacheID, c.TargetVideoPath,
		c.TargetLangISO2, status, boolInt(c.ForceIgnoreTagFilter), heldReason, origin)
	if err != nil {
		return SyncJobRecord{}, fmt.Errorf("store: insert sync job: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return SyncJobRecord{}, fmt.Errorf("store: sync job id: %w", err)
	}
	rec, ok, err := s.SyncJobByID(ctx, id)
	if err != nil {
		return SyncJobRecord{}, err
	}
	if !ok {
		return SyncJobRecord{}, errors.New("store: inserted sync job not found")
	}
	return rec, nil
}

// boolInt renders a Go bool as the 0/1 the schema CHECK expects.
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

const syncJobColumns = `
	SELECT id, target_track_id, COALESCE(target_library_cache_id, 0),
	       COALESCE(target_video_path, ''), target_lang_iso2,
	       status, COALESCE(sync_method, ''),
	       COALESCE(calculated_offset_ms, 0),
	       COALESCE(checkpoints_json, '[]'),
	       COALESCE(error_message, ''),
	       COALESCE(created_at, ''), COALESCE(updated_at, ''),
	       COALESCE(work_dir_path, ''), COALESCE(output_path, ''),
	       COALESCE(analysis_source, ''), COALESCE(analysis_fallback, ''),
	       COALESCE(force_ignore_tag_filter, 0), COALESCE(held_reason, ''),
	       COALESCE(origin_queue_id, 0)
	FROM sync_mux_jobs`

func scanSyncJob(scanner interface{ Scan(...any) error }) (SyncJobRecord, error) {
	var rec SyncJobRecord
	err := scanner.Scan(&rec.ID, &rec.TargetTrackID, &rec.TargetLibraryCacheID,
		&rec.TargetVideoPath, &rec.TargetLangISO2, &rec.Status, &rec.SyncMethod,
		&rec.CalculatedOffsetMS, &rec.CheckpointsJSON, &rec.ErrorMessage,
		&rec.CreatedAt, &rec.UpdatedAt, &rec.WorkDirPath, &rec.OutputPath,
		&rec.AnalysisSource, &rec.AnalysisFallback,
		&rec.ForceIgnoreTagFilter, &rec.HeldReason, &rec.OriginQueueID)
	return rec, err
}

// SyncJobByID reads one job, reporting found=false when it does not exist.
func (s *Store) SyncJobByID(ctx context.Context, id int64) (SyncJobRecord, bool, error) {
	rec, err := scanSyncJob(s.DB.QueryRowContext(ctx, syncJobColumns+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return SyncJobRecord{}, false, nil
	}
	if err != nil {
		return SyncJobRecord{}, false, fmt.Errorf("store: read sync job %d: %w", id, err)
	}
	return rec, true, nil
}

// ListSyncJobs returns every job, newest first.
func (s *Store) ListSyncJobs(ctx context.Context) ([]SyncJobRecord, error) {
	rows, err := s.DB.QueryContext(ctx, syncJobColumns+` ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list sync jobs: %w", err)
	}
	defer rows.Close()
	out := []SyncJobRecord{}
	for rows.Next() {
		rec, err := scanSyncJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan sync job: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate sync jobs: %w", err)
	}
	return out, nil
}

// syncJobQuerier is the read surface HeldJobs needs, so the projection can run
// inside ReleaseHeldJobs' transaction and outside it with one implementation.
type syncJobQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// HeldJobs returns every held job with its arr metadata in one query.
func (s *Store) HeldJobs(ctx context.Context) ([]HeldJob, error) {
	return heldJobs(ctx, s.DB)
}

// heldJobs is the single-query read behind both HeldJobs and ReleaseHeldJobs.
// The arr columns come from a LEFT JOIN so a job whose cache row disappeared
// (ON DELETE SET NULL, or a row written before it existed) still reports its own
// state and asserts no arr metadata.
func heldJobs(ctx context.Context, q syncJobQuerier) ([]HeldJob, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT j.id, COALESCE(j.target_video_path, ''), j.target_library_cache_id,
		       COALESCE(j.force_ignore_tag_filter, 0), COALESCE(j.held_reason, ''),
		       CASE WHEN c.id IS NULL THEN 0 ELSE 1 END,
		       COALESCE(c.title, ''), COALESCE(c.root_folder_path, ''),
		       COALESCE(c.arr_tag_labels_json, '[]')
		FROM sync_mux_jobs j
		LEFT JOIN arr_library_cache c ON c.id = j.target_library_cache_id
		WHERE j.status = 'held'
		ORDER BY j.id`)
	if err != nil {
		return nil, fmt.Errorf("store: list held sync jobs: %w", err)
	}
	defer rows.Close()

	out := []HeldJob{}
	for rows.Next() {
		var (
			job    HeldJob
			labels string
		)
		if err := rows.Scan(&job.ID, &job.TargetVideoPath, &job.TargetLibraryCacheID,
			&job.ForceIgnoreTagFilter, &job.HeldReason, &job.HasArrRow,
			&job.ArrTitle, &job.ArrRootFolder, &labels); err != nil {
			return nil, fmt.Errorf("store: scan held sync job: %w", err)
		}
		// A malformed snapshot is reported rather than read as "no tags",
		// which would silently change an arr gate verdict (ADR-0009).
		var snapshot LibraryCacheItem
		if err := decodeArrTagSnapshot("[]", labels, &snapshot); err != nil {
			return nil, fmt.Errorf("store: held sync job %d: %w", job.ID, err)
		}
		job.ArrTags = snapshot.ArrTagLabels
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate held sync jobs: %w", err)
	}
	return out, nil
}

// ReleaseHeldJobs evaluates nowEligible for each held job and flips the ones
// that pass to 'queued' in one transaction. It never touches force-approved
// rows. Returns the number released. Safe to call concurrently and at boot.
//
// Release is a projection of eligibility, not a timer: this is the only writer
// of held -> queued. The UPDATE is guarded on status='held', so two pollers
// racing cannot double-release and a job whose target changed between the read
// and the write is left for the next pass.
func (s *Store) ReleaseHeldJobs(ctx context.Context, nowEligible func(HeldJob) bool) (int, error) {
	if nowEligible == nil {
		return 0, errors.New("store: release held jobs needs an eligibility projection")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin release held jobs: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	held, err := heldJobs(ctx, tx)
	if err != nil {
		return 0, err
	}

	released := 0
	for _, job := range held {
		if job.ForceIgnoreTagFilter || !nowEligible(job) {
			continue
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE sync_mux_jobs
			SET status = ?, held_reason = NULL, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND status = ? AND force_ignore_tag_filter = 0`,
			SyncJobQueued, job.ID, SyncJobHeld)
		if err != nil {
			return 0, fmt.Errorf("store: release sync job %d: %w", job.ID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("store: release sync job %d rows: %w", job.ID, err)
		}
		released += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit release held jobs: %w", err)
	}
	return released, nil
}

// HoldSyncJob demotes a claimed job back to held. Guards on the current status
// so a completed job can never be re-held: only a claimed job (analyzing or
// muxing) may return to the projection, which makes the claim-time re-check
// one-way per attempt. reported=false means the guard refused the transition.
func (s *Store) HoldSyncJob(ctx context.Context, id int64, reason string) (bool, error) {
	if !validHoldReason(reason) {
		return false, fmt.Errorf("store: hold sync job %d: reason %q is not in the vocabulary", id, reason)
	}
	res, err := s.DB.ExecContext(ctx, `
		UPDATE sync_mux_jobs
		SET status = ?, held_reason = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status IN (?, ?)`,
		SyncJobHeld, reason, id, SyncJobAnalyzing, SyncJobMuxing)
	if err != nil {
		return false, fmt.Errorf("store: hold sync job %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: hold sync job %d rows: %w", id, err)
	}
	return n == 1, nil
}

// CancelSyncJob cancels a job by id; only queued|held may transition, so a
// running job is left to the worker and cancelled stays terminal.
func (s *Store) CancelSyncJob(ctx context.Context, id int64, reason string) (bool, error) {
	if !validCancelReason(reason) {
		return false, fmt.Errorf("store: cancel sync job %d: reason %q is not in the vocabulary", id, reason)
	}
	res, err := s.DB.ExecContext(ctx, `
		UPDATE sync_mux_jobs
		SET status = ?, held_reason = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status IN (?, ?)`,
		SyncJobCancelled, reason, id, SyncJobQueued, SyncJobHeld)
	if err != nil {
		return false, fmt.Errorf("store: cancel sync job %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: cancel sync job %d rows: %w", id, err)
	}
	return n == 1, nil
}

// CancelSyncJobsByOrigin cancels every queued|held job created by a queue row:
// rejection withdraws the intent approval recorded. A finished job is untouched
// and a running one is left to the worker (cancel is a queued|held transition),
// so no work dir is orphaned mid-mux.
func (s *Store) CancelSyncJobsByOrigin(ctx context.Context, queueID int64, reason string) (int, error) {
	if queueID <= 0 {
		return 0, errors.New("store: cancel sync jobs: queue id must be positive")
	}
	if !validCancelReason(reason) {
		return 0, fmt.Errorf("store: cancel sync jobs of queue row %d: reason %q is not in the vocabulary", queueID, reason)
	}
	res, err := s.DB.ExecContext(ctx, `
		UPDATE sync_mux_jobs
		SET status = ?, held_reason = ?, updated_at = CURRENT_TIMESTAMP
		WHERE origin_queue_id = ? AND status IN (?, ?)`,
		SyncJobCancelled, reason, queueID, SyncJobQueued, SyncJobHeld)
	if err != nil {
		return 0, fmt.Errorf("store: cancel sync jobs of queue row %d: %w", queueID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: cancel sync jobs of queue row %d rows: %w", queueID, err)
	}
	return int(n), nil
}

// OriginHasLiveSyncJob reports whether an approval already owns a job that can
// still run (queued, held, analyzing, ready_for_review or muxing). Approve uses
// it to stay idempotent: a second POST for a row whose extraction already bred
// the job must answer success, not breed another one.
func (s *Store) OriginHasLiveSyncJob(ctx context.Context, queueID int64) (bool, error) {
	if queueID <= 0 {
		return false, errors.New("store: live sync job of queue row: queue id must be positive")
	}
	var one int
	err := s.DB.QueryRowContext(ctx, `
		SELECT 1 FROM sync_mux_jobs
		WHERE origin_queue_id = ? AND status IN (?, ?, ?, ?, ?)
		LIMIT 1`, queueID, SyncJobQueued, SyncJobHeld, SyncJobAnalyzing,
		SyncJobReadyForReview, SyncJobMuxing).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read live sync job of queue row %d: %w", queueID, err)
	}
	return true, nil
}

// FinishSyncJobAnalysis persists the terminal state of an analysis run.
// checkpoints is marshalled as-is; a nil report stores an empty array.
//
// analysisSource is the mode the job actually used (windows, or full after a
// fallback) and analysisFallback is why a windowed request did not run as
// windows ("" when it did). Both are written as NULL when empty, so a row that
// predates migration 0007 and a job that ran its requested mode read the same.
func (s *Store) FinishSyncJobAnalysis(ctx context.Context, id int64, status, syncMethod string, offsetMS int64, checkpoints any, errMessage, analysisSource, analysisFallback string) error {
	raw, err := json.Marshal(checkpoints)
	if err != nil {
		return fmt.Errorf("store: marshal checkpoints: %w", err)
	}
	if string(raw) == "null" {
		raw = []byte("[]")
	}
	res, err := s.DB.ExecContext(ctx, `
		UPDATE sync_mux_jobs
		SET status = ?, sync_method = NULLIF(?, ''),
		    calculated_offset_ms = ?, checkpoints_json = ?,
		    error_message = NULLIF(?, ''),
		    analysis_source = NULLIF(?, ''),
		    analysis_fallback = NULLIF(?, ''),
		    updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`, status, syncMethod, offsetMS, string(raw), errMessage,
		analysisSource, analysisFallback, id)
	if err != nil {
		return fmt.Errorf("store: finish sync job %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: finish sync job %d rows: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: sync job %d not found", id)
	}
	return nil
}

// SetSyncJobStatus is the pre-analysis transition (queued -> analyzing) and
// also records failures that happen before a report exists.
func (s *Store) SetSyncJobStatus(ctx context.Context, id int64, status, errMessage string) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE sync_mux_jobs
		SET status = ?, error_message = NULLIF(?, ''), updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`, status, errMessage, id)
	if err != nil {
		return fmt.Errorf("store: set sync job %d status: %w", id, err)
	}
	return nil
}

// SetSyncJobOutputPath records where a completed mux published the file
// (ADR-0011). An empty path clears the column.
func (s *Store) SetSyncJobOutputPath(ctx context.Context, id int64, path string) error {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE sync_mux_jobs
		SET output_path = NULLIF(?, ''), updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`, path, id)
	if err != nil {
		return fmt.Errorf("store: set sync job %d output path: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set sync job %d output path rows: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: sync job %d not found", id)
	}
	return nil
}

// LibraryCacheJobTarget is the arr library row a sync job needs: the target
// video path, its gate verdict, the original language for reference resolution
// and the arr metadata (tag labels + canonical root folder, ADR-0009) the gate
// re-check consumes.
type LibraryCacheJobTarget struct {
	ID                   int64
	TargetFilePath       string
	EligibilityStatus    string
	MatchesTargetTags    bool
	OriginalLanguageISO2 string
	MediaType            string
	ArrTagLabels         []string
	RootFolderPath       string
}

// LibraryCacheJobTargetByID reads the sync-relevant columns of one
// arr_library_cache row.
func (s *Store) LibraryCacheJobTargetByID(ctx context.Context, id int64) (LibraryCacheJobTarget, bool, error) {
	var (
		rec    LibraryCacheJobTarget
		target sql.NullString
		elig   sql.NullString
		orig   sql.NullString
		root   sql.NullString
		labels string
	)
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, target_file_path, eligibility_status, matches_target_tags,
		       original_language_iso2, media_type,
		       COALESCE(arr_tag_labels_json, '[]'), COALESCE(root_folder_path, '')
		FROM arr_library_cache WHERE id = ?`, id).
		Scan(&rec.ID, &target, &elig, &rec.MatchesTargetTags, &orig, &rec.MediaType, &labels, &root)
	if errors.Is(err, sql.ErrNoRows) {
		return LibraryCacheJobTarget{}, false, nil
	}
	if err != nil {
		return LibraryCacheJobTarget{}, false, fmt.Errorf("store: read library cache %d: %w", id, err)
	}
	rec.TargetFilePath = target.String
	rec.EligibilityStatus = elig.String
	rec.OriginalLanguageISO2 = orig.String
	rec.RootFolderPath = root.String
	// Legacy rows carry '[]' from the column default; a malformed snapshot is
	// reported rather than treated as untagged, because that would silently
	// change an arr gate verdict.
	var snapshot LibraryCacheItem
	if err := decodeArrTagSnapshot("[]", labels, &snapshot); err != nil {
		return LibraryCacheJobTarget{}, false, err
	}
	rec.ArrTagLabels = snapshot.ArrTagLabels
	return rec, true, nil
}

// SiblingVaultTracks lists the archived tracks that share a vault media row and
// group_uuid, excluding excludeID. These are the reference candidates staged by
// Copy-on-Work.
func (s *Store) SiblingVaultTracks(ctx context.Context, vaultMediaID int64, groupUUID string, excludeID int64) ([]VaultTrackRecord, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, vault_media_id, season_number, episode_number, group_uuid,
		       lang_iso2, COALESCE(lang_iso3, ''), variant, is_original_lang,
		       file_path, sha256, codec, channels, channel_count,
		       COALESCE(bitrate_bps, 0), COALESCE(sample_rate, 0),
		       COALESCE(source_fps, 0), COALESCE(duration_ms, 0),
		       COALESCE(source_release_name, ''), COALESCE(notes, '')
		FROM audio_tracks
		WHERE vault_media_id = ? AND group_uuid = ? AND id <> ?
		ORDER BY id`, vaultMediaID, groupUUID, excludeID)
	if err != nil {
		return nil, fmt.Errorf("store: list sibling vault tracks: %w", err)
	}
	defer rows.Close()
	out := []VaultTrackRecord{}
	for rows.Next() {
		var (
			rec             VaultTrackRecord
			season, episode sql.NullInt64
		)
		if err := rows.Scan(&rec.ID, &rec.VaultMediaID, &season, &episode, &rec.GroupUUID,
			&rec.LangISO2, &rec.LangISO3, &rec.Variant, &rec.IsOriginalLang,
			&rec.FilePath, &rec.SHA256, &rec.Codec, &rec.Channels, &rec.ChannelCount,
			&rec.BitrateBPS, &rec.SampleRate, &rec.SourceFPS, &rec.DurationMS,
			&rec.SourceReleaseName, &rec.Notes); err != nil {
			return nil, fmt.Errorf("store: scan sibling vault track: %w", err)
		}
		if season.Valid {
			v := int(season.Int64)
			rec.SeasonNumber = &v
		}
		if episode.Valid {
			v := int(episode.Int64)
			rec.EpisodeNumber = &v
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate sibling vault tracks: %w", err)
	}
	return out, nil
}
