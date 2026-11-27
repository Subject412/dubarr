-- 0008: approval records intent and the Processing Gate holds the mux (#48).
--
-- A held job is approved-but-not-yet-eligible: the operator said "dub this
-- title" while the target still lacks the required tags, so the job waits on
-- sync_mux_jobs — never on import_queue, so no queue CHECK is rebuilt and a
-- donor can never be held for tags (ADR-0005). Release is a projection of
-- eligibility (ReleaseHeldJobs), not a timer; 'cancelled' is the terminal
-- counterpart a rejection writes.
--
-- force_ignore_tag_filter persists the operator's override instead of deriving
-- it again: a forced job is inserted queued and the projection never re-holds it.
-- held_reason carries a closed vocabulary code (waiting_for_tags,
-- missing_target, target_changed, cancelled_by_rejection) so the UI translates
-- it and never parses prose. origin_queue_id ties the job to the approval that
-- created it, which is how a rejection cancels its intent.
--
-- SQLite cannot alter a CHECK constraint, so this is the standard table rebuild:
-- create, copy by explicit column name, drop, rename, recreate the indexes.
-- The column lists are generated from the live schema (0001 + 0003 + 0007) and
-- never `SELECT *`, so a column a later migration adds cannot be silently lost
-- by an earlier copy list.
--
-- Foreign keys: the runner wraps this file in one transaction and SQLite treats
-- PRAGMA foreign_keys as a no-op inside a transaction, so the pragmas below
-- document the intent and keep the file correct if it is ever applied outside
-- one. The rebuild is safe with foreign keys ON either way: nothing references
-- sync_mux_jobs, and the rows are copied against parents that still exist.
-- PRAGMA legacy_alter_table is deliberately left untouched: the modern RENAME
-- semantics rewrite inbound references, of which this table has none.
--
-- schema.sql is NOT edited: it mirrors 0001_initial.sql (TestSchemaSQLMirrorsMigration),
-- exactly like 0002-0007; this forward migration is the authoritative change.

PRAGMA foreign_keys = OFF;

CREATE TABLE sync_mux_jobs_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    target_track_id INTEGER NOT NULL REFERENCES audio_tracks(id) ON DELETE CASCADE,
    ref_track_id INTEGER REFERENCES audio_tracks(id) ON DELETE SET NULL,
    target_library_cache_id INTEGER REFERENCES arr_library_cache(id) ON DELETE SET NULL,
    target_video_path TEXT NOT NULL,
    target_lang_iso2 TEXT NOT NULL,       -- Language of incoming track (e.g., 'pl')
    target_audio_stream_index INTEGER,    -- Stream index inside target MKV used as timing reference
    work_dir_path TEXT,                   -- Isolated job scratch folder inside WORK_DIR

    status TEXT NOT NULL CHECK (status IN
        ('queued', 'held', 'analyzing', 'ready_for_review', 'muxing', 'completed', 'failed', 'cancelled')),
    force_ignore_tag_filter INTEGER NOT NULL DEFAULT 0
        CHECK (force_ignore_tag_filter IN (0, 1)),   -- operator override, persisted not re-derived
    held_reason TEXT,                     -- waiting_for_tags | missing_target | target_changed | cancelled_by_rejection
    origin_queue_id INTEGER REFERENCES import_queue(id) ON DELETE SET NULL,  -- the approval that created the intent

    sync_method TEXT CHECK (sync_method IN ('same_lang_reference', 'lowpass_fallback', 'manual')),

    -- Mathematical analysis results (sign convention: offset_ms = t_target - t_donor)
    calculated_offset_ms INTEGER DEFAULT 0,
    manual_offset_ms INTEGER,             -- Optional override from Spectrogram Studio
    confidence_score REAL DEFAULT 0.0,    -- 0.0 - 100.0
    detected_fps_ratio REAL DEFAULT 1.0,  -- e.g., 1.0 or ~0.959041 (23.976 / 25.0)
    speed_mode TEXT NOT NULL DEFAULT 'none' CHECK (speed_mode IN ('none', 'preserve_pitch', 'pal_pitch_restore')),
    checkpoints_json TEXT DEFAULT '[]',   -- Multi-point checkpoint results across the timeline
    analysis_source TEXT,                 -- which analysis mode ran (0007)
    analysis_fallback TEXT,               -- why a windowed run did not run as windows (0007)

    -- Muxing parameters
    track_title_in_mkv TEXT NOT NULL DEFAULT 'Polski Dubbing',
    set_as_default BOOLEAN DEFAULT 0,
    error_message TEXT,
    output_path TEXT,                     -- where the mux published the file (0003)

    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO sync_mux_jobs_new (
    id, target_track_id, ref_track_id, target_library_cache_id,
    target_video_path, target_lang_iso2, target_audio_stream_index,
    work_dir_path, status, force_ignore_tag_filter, held_reason,
    origin_queue_id, sync_method, calculated_offset_ms, manual_offset_ms,
    confidence_score, detected_fps_ratio, speed_mode, checkpoints_json,
    analysis_source, analysis_fallback, track_title_in_mkv, set_as_default,
    error_message, output_path, created_at, updated_at)
SELECT
    id, target_track_id, ref_track_id, target_library_cache_id,
    target_video_path, target_lang_iso2, target_audio_stream_index,
    work_dir_path, status, 0, NULL,
    NULL, sync_method, calculated_offset_ms, manual_offset_ms,
    confidence_score, detected_fps_ratio, speed_mode, checkpoints_json,
    analysis_source, analysis_fallback, track_title_in_mkv, set_as_default,
    error_message, output_path, created_at, updated_at
FROM sync_mux_jobs;

DROP TABLE sync_mux_jobs;
ALTER TABLE sync_mux_jobs_new RENAME TO sync_mux_jobs;

CREATE INDEX IF NOT EXISTS idx_sync_jobs_status ON sync_mux_jobs(status, created_at);
CREATE INDEX IF NOT EXISTS idx_sync_jobs_target_track ON sync_mux_jobs(target_track_id);
CREATE INDEX IF NOT EXISTS idx_sync_jobs_target_cache ON sync_mux_jobs(target_library_cache_id);
CREATE INDEX IF NOT EXISTS idx_sync_jobs_origin_queue ON sync_mux_jobs(origin_queue_id);

PRAGMA foreign_keys = ON;
