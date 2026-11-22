package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// SyncJobRecord is one row of sync_mux_jobs (IDEA.md §4 table 8) as the sync
// job API needs it. Status holds the persisted schema value
// (queued/analyzing/ready_for_review/muxing/completed/failed); the API layer
// maps it onto the public queued/running/done/failed vocabulary.
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
}

// SyncJobCreate is the insert shape for POST /api/sync/jobs.
type SyncJobCreate struct {
	VaultTrackID         int64
	TargetLibraryCacheID int64
	TargetVideoPath      string
	TargetLangISO2       string
}

// CreateSyncJob inserts a queued job and returns the persisted row.
func (s *Store) CreateSyncJob(ctx context.Context, c SyncJobCreate) (SyncJobRecord, error) {
	if c.VaultTrackID <= 0 || c.TargetLibraryCacheID <= 0 {
		return SyncJobRecord{}, errors.New("store: sync job ids must be positive")
	}
	if c.TargetVideoPath == "" || c.TargetLangISO2 == "" {
		return SyncJobRecord{}, errors.New("store: sync job target path and language are required")
	}
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO sync_mux_jobs
		    (target_track_id, target_library_cache_id, target_video_path,
		     target_lang_iso2, status)
		VALUES (?, ?, ?, ?, 'queued')`,
		c.VaultTrackID, c.TargetLibraryCacheID, c.TargetVideoPath, c.TargetLangISO2)
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

const syncJobColumns = `
	SELECT id, target_track_id, COALESCE(target_library_cache_id, 0),
	       COALESCE(target_video_path, ''), target_lang_iso2,
	       status, COALESCE(sync_method, ''),
	       COALESCE(calculated_offset_ms, 0),
	       COALESCE(checkpoints_json, '[]'),
	       COALESCE(error_message, ''),
	       COALESCE(created_at, ''), COALESCE(updated_at, ''),
	       COALESCE(work_dir_path, '')
	FROM sync_mux_jobs`

func scanSyncJob(scanner interface{ Scan(...any) error }) (SyncJobRecord, error) {
	var rec SyncJobRecord
	err := scanner.Scan(&rec.ID, &rec.TargetTrackID, &rec.TargetLibraryCacheID,
		&rec.TargetVideoPath, &rec.TargetLangISO2, &rec.Status, &rec.SyncMethod,
		&rec.CalculatedOffsetMS, &rec.CheckpointsJSON, &rec.ErrorMessage,
		&rec.CreatedAt, &rec.UpdatedAt, &rec.WorkDirPath)
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

// FinishSyncJobAnalysis persists the terminal state of an analysis run.
// checkpoints is marshalled as-is; a nil report stores an empty array.
func (s *Store) FinishSyncJobAnalysis(ctx context.Context, id int64, status, syncMethod string, offsetMS int64, checkpoints any, errMessage string) error {
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
		    error_message = NULLIF(?, ''), updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`, status, syncMethod, offsetMS, string(raw), errMessage, id)
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

// LibraryCacheJobTarget is the arr library row a sync job needs: the target
// video path, its gate verdict and the original language for reference
// resolution.
type LibraryCacheJobTarget struct {
	ID                   int64
	TargetFilePath       string
	EligibilityStatus    string
	MatchesTargetTags    bool
	OriginalLanguageISO2 string
	MediaType            string
}

// LibraryCacheJobTargetByID reads the sync-relevant columns of one
// arr_library_cache row.
func (s *Store) LibraryCacheJobTargetByID(ctx context.Context, id int64) (LibraryCacheJobTarget, bool, error) {
	var (
		rec    LibraryCacheJobTarget
		target sql.NullString
		elig   sql.NullString
		orig   sql.NullString
	)
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, target_file_path, eligibility_status, matches_target_tags,
		       original_language_iso2, media_type
		FROM arr_library_cache WHERE id = ?`, id).
		Scan(&rec.ID, &target, &elig, &rec.MatchesTargetTags, &orig, &rec.MediaType)
	if errors.Is(err, sql.ErrNoRows) {
		return LibraryCacheJobTarget{}, false, nil
	}
	if err != nil {
		return LibraryCacheJobTarget{}, false, fmt.Errorf("store: read library cache %d: %w", id, err)
	}
	rec.TargetFilePath = target.String
	rec.EligibilityStatus = elig.String
	rec.OriginalLanguageISO2 = orig.String
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
