-- Dubarr initial schema (migration 0001).
--
-- Mirrors IDEA.md §4 "SQLite Database Schema" exactly for the nine spec
-- tables. Tables not specified there but required by PLAN.md M1 (sessions,
-- secrets) are appended and documented inline; PRAGMAs live in Open() because
-- journal_mode cannot be changed inside the migration transaction.
--
-- backend/schema.sql mirrors this file byte-for-byte; TestSchemaSQLMirrorsMigration guards against drift.

-- 1. Authentication & API access -------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,          -- Argon2id PHC string
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS api_tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,      -- SHA-256 hex digest of bearer token
    token_prefix TEXT NOT NULL,           -- First chars for display
    last_used_at DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Sessions are not given DDL in IDEA.md; PLAN.md M1 requires them. Only the
-- SHA-256 hash of the signed session cookie value is persisted, so a database
-- leak cannot be replayed as a login.
CREATE TABLE IF NOT EXISTS sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,      -- SHA-256 hex of the session cookie value
    expires_at DATETIME NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    last_seen_at DATETIME
);

CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

-- 2. Application settings (JSON-serialized values stored in App Data folder) -----
-- Keys include: storage.paths, languages.policy, target.tag_filter, sync.defaults, security.options
CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- 3. Radarr / Sonarr instances ---------------------------------------------------
CREATE TABLE IF NOT EXISTS arr_instances (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    type TEXT NOT NULL CHECK (type IN ('radarr', 'sonarr')),
    base_url TEXT NOT NULL,
    api_key_enc BLOB NOT NULL,            -- AES-256-GCM encrypted (12-byte nonce || ciphertext || tag)
    api_key_last4 TEXT NOT NULL,          -- Masked suffix only
    path_mappings_json TEXT DEFAULT '[]', -- [{"remote": "/movies", "local": "/media/movies"}]
    tag_filter_override_json TEXT,        -- Optional per-instance override of target filename tag rules
    enabled BOOLEAN NOT NULL DEFAULT 1,
    last_sync_at DATETIME
);

-- 4. Cached media library synced from Radarr / Sonarr ----------------------------
CREATE TABLE IF NOT EXISTS arr_library_cache (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    arr_instance_id INTEGER NOT NULL REFERENCES arr_instances(id) ON DELETE CASCADE,
    media_type TEXT NOT NULL CHECK (media_type IN ('movie', 'episode')),
    external_item_id INTEGER NOT NULL,    -- Radarr movieId or Sonarr episodeId
    series_id INTEGER,                    -- Sonarr seriesId (NULL for movies)
    tmdb_id INTEGER,
    tvdb_id INTEGER,
    imdb_id TEXT,
    title TEXT NOT NULL,
    original_title TEXT,
    original_language_iso2 TEXT,          -- Normalized ISO-2 from Radarr/Sonarr originalLanguage (e.g., 'en', 'ja')
    year INTEGER,
    season_number INTEGER,
    episode_number INTEGER,
    episode_title TEXT,
    media_folder_path TEXT,               -- Mapped local folder path (handles Tdarr filename renames)
    target_file_path TEXT,                -- Mapped local path to target video file
    matches_target_tags BOOLEAN NOT NULL DEFAULT 0,
    eligibility_status TEXT NOT NULL DEFAULT 'missing_target'
        CHECK (eligibility_status IN ('eligible', 'waiting_for_tags', 'missing_target')),
    audio_languages_json TEXT NOT NULL DEFAULT '[]', -- ISO-2 audio languages detected in target file (e.g., ["ja","en"])
    video_codec TEXT,
    raw_metadata_json TEXT,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_arr_cache_lookup
    ON arr_library_cache(tmdb_id, tvdb_id, season_number, episode_number);
CREATE INDEX IF NOT EXISTS idx_arr_cache_eligibility
    ON arr_library_cache(eligibility_status, matches_target_tags);
CREATE INDEX IF NOT EXISTS idx_arr_cache_instance
    ON arr_library_cache(arr_instance_id);

-- 5. Watch folder queue awaiting user approval (Approval Queue) -----------------
CREATE TABLE IF NOT EXISTS import_queue (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    source_file_path TEXT NOT NULL UNIQUE,
    filename TEXT NOT NULL,
    file_size INTEGER NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('scanning', 'pending_approval', 'extracting', 'completed', 'ignored', 'failed')),
    error_message TEXT,

    -- Match results from /api/v3/parse
    matched_arr_instance_id INTEGER REFERENCES arr_instances(id) ON DELETE SET NULL,
    matched_library_cache_id INTEGER REFERENCES arr_library_cache(id) ON DELETE SET NULL,
    parse_confidence TEXT,                -- 'exact' | 'ambiguous' | 'unmatched'
    parsed_raw_json TEXT,

    -- Donor ffprobe analysis
    source_fps REAL,
    source_duration_ms INTEGER,
    ffprobe_streams_json TEXT NOT NULL DEFAULT '[]',

    -- User approval choices: JSON array of selected tracks to export
    -- [{"stream_index": 1, "lang_iso2": "pl", "variant": "dubbing", "track_title": "Polski Dubbing"}]
    selected_exports_json TEXT NOT NULL DEFAULT '[]',
    delete_source_after_extract BOOLEAN DEFAULT 0,

    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_import_queue_status ON import_queue(status, created_at);
CREATE INDEX IF NOT EXISTS idx_import_queue_match ON import_queue(matched_library_cache_id);

-- 6. Movies/Series cataloged in Audio Vault -------------------------------------
CREATE TABLE IF NOT EXISTS vault_media (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    media_type TEXT NOT NULL CHECK (media_type IN ('movie', 'series')),
    title TEXT NOT NULL,
    year INTEGER,
    tmdb_id INTEGER,
    tvdb_id INTEGER,
    original_language_iso2 TEXT,
    vault_folder_path TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(media_type, tmdb_id, tvdb_id)
);

-- 7. Archived immutable .mka audio tracks in Audio Vault ------------------------
CREATE TABLE IF NOT EXISTS audio_tracks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    vault_media_id INTEGER NOT NULL REFERENCES vault_media(id) ON DELETE CASCADE,
    season_number INTEGER,
    episode_number INTEGER,
    group_uuid TEXT NOT NULL,             -- Links all tracks extracted from the same donor file
    lang_iso2 TEXT NOT NULL,              -- Normalized ISO 639-1 code (e.g., 'pl', 'ja', 'en')
    lang_iso3 TEXT,                       -- Original/ISO 639-2 code (e.g., 'pol', 'jpn', 'eng')
    variant TEXT NOT NULL DEFAULT 'standard'
        CHECK (variant IN ('dubbing', 'lektor', 'original', 'standard', 'commentary')),
    is_original_lang BOOLEAN NOT NULL DEFAULT 0,
    file_path TEXT NOT NULL UNIQUE,       -- Always ends with .<lang_iso2>.mka
    sha256 TEXT NOT NULL,                 -- Integrity digest to guarantee Vault immutability
    codec TEXT NOT NULL,                  -- e.g., eac3, ac3, aac, opus
    channels TEXT NOT NULL,               -- e.g., 5.1, 2.0
    channel_count INTEGER NOT NULL,
    bitrate_bps INTEGER,
    sample_rate INTEGER,
    source_fps REAL,
    duration_ms INTEGER,
    source_release_name TEXT,
    notes TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_audio_tracks_group ON audio_tracks(group_uuid);
CREATE INDEX IF NOT EXISTS idx_audio_tracks_lang ON audio_tracks(vault_media_id, lang_iso2);
CREATE INDEX IF NOT EXISTS idx_audio_tracks_episode ON audio_tracks(vault_media_id, season_number, episode_number);

-- 8. Synchronization and Muxing jobs (executed strictly on WORK_DIR copies) -----
CREATE TABLE IF NOT EXISTS sync_mux_jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    target_track_id INTEGER NOT NULL REFERENCES audio_tracks(id) ON DELETE CASCADE,
    ref_track_id INTEGER REFERENCES audio_tracks(id) ON DELETE SET NULL,
    target_library_cache_id INTEGER REFERENCES arr_library_cache(id) ON DELETE SET NULL,
    target_video_path TEXT NOT NULL,
    target_lang_iso2 TEXT NOT NULL,       -- Language of incoming track (e.g., 'pl')
    target_audio_stream_index INTEGER,    -- Stream index inside target MKV used as timing reference
    work_dir_path TEXT,                   -- Isolated job scratch folder inside WORK_DIR

    status TEXT NOT NULL CHECK (status IN ('queued', 'analyzing', 'ready_for_review', 'muxing', 'completed', 'failed')),
    sync_method TEXT CHECK (sync_method IN ('same_lang_reference', 'lowpass_fallback', 'manual')),

    -- Mathematical analysis results (sign convention: offset_ms = t_target - t_donor)
    calculated_offset_ms INTEGER DEFAULT 0,
    manual_offset_ms INTEGER,             -- Optional override from Spectrogram Studio
    confidence_score REAL DEFAULT 0.0,    -- 0.0 - 100.0
    detected_fps_ratio REAL DEFAULT 1.0,  -- e.g., 1.0 or ~0.959041 (23.976 / 25.0)
    speed_mode TEXT NOT NULL DEFAULT 'none' CHECK (speed_mode IN ('none', 'preserve_pitch', 'pal_pitch_restore')),
    checkpoints_json TEXT DEFAULT '[]',   -- Multi-point checkpoint results across the timeline

    -- Muxing parameters
    track_title_in_mkv TEXT NOT NULL DEFAULT 'Polski Dubbing',
    set_as_default BOOLEAN DEFAULT 0,
    error_message TEXT,

    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_sync_jobs_status ON sync_mux_jobs(status, created_at);
CREATE INDEX IF NOT EXISTS idx_sync_jobs_target_track ON sync_mux_jobs(target_track_id);
CREATE INDEX IF NOT EXISTS idx_sync_jobs_target_cache ON sync_mux_jobs(target_library_cache_id);

-- 9. Encrypted secret blobs ------------------------------------------------------
-- Not given DDL in IDEA.md; ADR-0003 / PLAN.md T4 require an AES-256-GCM
-- encrypted secret store (OS keyring with .master.key 0600 fallback).
CREATE TABLE IF NOT EXISTS secrets (
    key TEXT PRIMARY KEY,
    value_enc BLOB NOT NULL,              -- AES-256-GCM (12-byte nonce || ciphertext || tag)
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
