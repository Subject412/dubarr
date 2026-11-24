# Dubarr — Architectural Specification and Implementation Plan (IDEA.md)

> **Authority order:** Normative: `AGENTS.md` rules and `docs/adr/**` decisions, then the working code. `IDEA.md` is historical design narrative; where it disagrees with an ADR or the code, follow the ADR/code and amend either explicitly. This file is retained as design history — see the ADR index in [`docs/adr/`](docs/adr/). Sections known to be superseded carry an inline **Superseded:** marker naming the governing ADR.

> **Document Purpose:** Complete technical specification (v2.0) of **Dubarr** intended for an AI orchestrator and coding agents. This document defines the architecture, responsibility boundaries, security model, database schema, API contracts, workflows, and phased milestones with acceptance criteria.

---

## 1. Vision and Project Scope

**Dubarr** is a lightweight, self-hosted web/desktop application built with an **API-first** architecture, serving as a specialized **Radarr / Sonarr counterpart for managing, archiving, synchronizing, and muxing audio tracks** (e.g., Polish dubbing / voice-over, or any user-configured target language).

### What Dubarr DOES:
1. **Monitors a Watch Folder (`WATCH_DIR`)** for incoming "donor" video files containing desired audio tracks (e.g., Polish dubbing or *lektor*, alongside original/reference audio)—often sourced from lower-quality releases (480p/720p, WEB-DL, DVD, TVRip).
2. **Identifies media directly via Radarr and Sonarr APIs** (`/api/v3/parse` as well as searching a locally cached library synced from those instances, including each item's **`originalLanguage`**), eliminating fragile custom release name parsers.
3. **Requires explicit user approval (Approval Queue)** before extracting or copying any files into the permanent repository, while pre-selecting donor audio tracks according to configurable **Language & Export Policy** settings.
4. **Losslessly extracts selected donor audio tracks (`-c:a copy`)** into a completely separate, **write-once / immutable Audio Vault (`VAULT_DIR`)** as self-describing **Matroska Audio (`.mka`)** files tagged with normalized **ISO 639-1 two-letter language suffixes** (`<media filename>.<lang>.mka`, e.g., `Spirited Away (2001).pl.mka`, `Spirited Away (2001).ja.mka`):
   - Supports exporting **all** non-commentary donor audio tracks OR following configurable rules (**Target Import Languages** + **Original Language** from Radarr/Sonarr + **Extra Reference Languages** such as `en`).
   - Keeping `VAULT_DIR` strictly separated guarantees that deleting, transcoding, or replacing a video file in Radarr/Sonarr or any external processing pipeline will **never** delete archived `.mka` tracks.
5. **Respects External Processing Pipelines via Filename Tag Filtering:**
   - Allows configuring required filename tags on destination media files (e.g., `[TDARR]`, `[AV1]` — Tdarr is only an example producer) so Dubarr **only synchronizes and muxes target files the operator marked as processed** (while still allowing donor ingestion into the Audio Vault at any time).
6. **Operates strictly on copies in an isolated Work Directory (`WORK_DIR`):**
   - Files in `VAULT_DIR` are **never** modified, written next to, or processed in place after initial extraction. Every synchronization job, preview slice, tempo adjustment, and mux input copies the needed `.mka` file(s) from `VAULT_DIR` into `WORK_DIR` first.
7. **Automatically synchronizes the copied `.mka` track** against the target video file using a generalized **Reference Language Resolver**:
   - **Priority 1 (Original Language Match):** Compares the donor track matching the media's `originalLanguage` (e.g., `ja <-> ja` for anime, `fr <-> fr` for French cinema, `en <-> en` for Hollywood) against the target MKV's original audio stream.
   - **Priority 2 (Shared Reference Language Match):** Compares any other shared language archived in the donor group against the target MKV (e.g., `en <-> en`).
   - **Priority 3 (Cross-Language Low-Pass Fallback):** When no shared reference language exists, compares low-frequency bands (`lowpass` 30–200 Hz: explosions, bass, score) between the target-language `.mka` and the target's Original/English/first audio stream.
   - Detects constant time offsets (`offset_ms`), speed/FPS drift (e.g., *PAL Speedup* `25.000 -> 23.976 fps`), and non-linear mid-movie cuts/edits.
8. **Provides a Manual Synchronization Studio (Spectrogram + Live Dual Playback)** in the WebUI for millisecond-level inspection and fine-tuning.
9. **Safely & atomically muxes the synchronized audio track** into the tagged target `.mkv` file:
   - For constant offset only: uses `mkvmerge --sync` without re-encoding video or any audio track.
   - For FPS/speed mismatch: uses `ffmpeg` on the `WORK_DIR` copy (`-c:v` untouched, applying tempo adjustment *exclusively* to the incoming audio track via `rubberband`/`atempo` or PAL pitch restoration), then muxes cleanly with `mkvmerge`.
   - Upon successful muxing, triggers `RescanMovie` / `RescanSeries` via Radarr/Sonarr APIs.
10. **Stores all configuration and secrets securely in the local App Data folder via WebUI/API**—never using environment variables for app settings or API keys.

### What Dubarr DOES NOT DO (Strict Boundaries):
* **Does NOT transcode video** (e.g., to AV1)—an external pipeline handles video transcoding and audio track cleanup. Video streams in target files must always remain untouched (`copy`).
* **Does NOT re-encode lossy audio tracks** (AAC, AC3, E-AC3, Opus) unless forced by an FPS/tempo adjustment (in which case only that single incoming track copy is re-encoded) or unless an explicit, user-configured **Transcode Rule** matches (ADR-0010). Transcode Rules apply only to a Work Area copy and never modify the Audio Vault, which remains write-once and lossless.
* **Does NOT modify or write inside `VAULT_DIR`** after initial atomic import of an `.mka` track.
* **Does NOT accept application settings or API keys via environment variables.**
* **Does NOT use Python** in runtime (neither in Docker nor natively)—to keep memory footprint minimal and distribution lightweight.

---

## 2. Tech Stack

The application runs seamlessly both as a **Docker container (WebUI exposed via `IP:PORT`)** and as a **standalone native app on Windows and Linux** (single local server; `--desktop` opens the default browser at the listen address).

| Layer | Technology | Role and Rationale |
| :--- | :--- | :--- |
| **Backend Core & API** | **Go (1.26+)** | HTTP server (REST API + WebSockets), auth & AES-256-GCM secret store, job queue, file watcher, Radarr/Sonarr API clients, FFmpeg/MKVToolNix orchestration, embedded frontend via `go:embed`. Small idle footprint — a design goal, not a strict RAM cap (ADR-0001). |
| **DSP Engine (Auto-Sync)** | **Rust (CLI `dubarr-sync`)** | Tiny compiled binary invoked by Go. Reads raw/WAV PCM slices and computes fast **FFT Cross-Correlation**. Zero Python/SciPy overhead. |
| **Database** | **SQLite** (WAL mode, pure-Go driver e.g. `modernc.org/sqlite`) | Single portable database file inside the App Data folder; no external DB server or Redis required. |
| **Security & Crypto** | **Go `crypto/aes` (GCM), `golang.org/x/crypto/argon2`**, optional OS Keyring | Password hashing (Argon2id), session/API token hashing, AES-256-GCM at-rest encryption for Radarr/Sonarr API keys inside the App Data folder. |
| **Frontend (WebUI)** | **React + TypeScript + Vite + Tailwind CSS** | Modern, cohesive SPA compiled into static assets and embedded inside the Go binary (`embed.FS`). |
| **Audio Visualization** | **wavesurfer.js** (Spectrogram plugin) + **Web Audio API** | Renders 60-second audio slices (waveform + spectrogram), supports 1 ms / 10 ms / 100 ms nudging, and performs live synchronized dual-track playback in the browser. |
| **Media Tools (ext.)** | **`ffmpeg`**, **`ffprobe`**, **`mkvmerge` (MKVToolNix)** | Lossless `-c:a copy` extraction to `.mka`, PCM/preview slicing, audio tempo correction, atomic Matroska muxing. |

> ⚠ **Superseded (resource claim):** the footprint goal is governed qualitatively by ADR-0001 — a small *idle* footprint, not a strict RAM cap; active jobs may exceed it. The “~20–40 MB” figure above is historical.

---

## 3. Operational Rules, Data Layout, and Security Model

### 3.1. Configuration Bootstrap & Zero-Env-Var Policy
Dubarr **does not read application settings, directory mappings, language rules, or Radarr/Sonarr API keys from environment variables**.

At startup, the binary requires only minimal bootstrap parameters (with zero-config defaults):
* `--data-dir` — Path to the **App Data Folder** (defaults to `/config` inside Docker, or `./data` alongside the executable natively).
* `--listen` — Bind address and port (defaults to `0.0.0.0:8080` in Docker, `127.0.0.1:8080` natively).
* `--desktop` — Optional flag to open a browser / `--app` window automatically in native mode.

All application settings (`watch_dir`, `vault_dir`, `work_dir`, `media_roots`, language policies, target filename tag filters, Radarr/Sonarr instances, and API keys) are configured exclusively through the **First-Run Setup Wizard / WebUI / REST API** and persisted inside `DATA_DIR`.

> ⚠ **Superseded:** bootstrap flags and logging configuration are governed by ADR-0003 (zero app-config environment variables; only `--data-dir`/`--listen`/`--desktop`) and ADR-0013 (logging via `--log-level`/`--log-file`). The `--app` window wording above is historical; native desktop mode opens the default browser (`xdg-open`), see PLAN.md:23.

### 3.2. Security Architecture for App Data & API Keys
Because `DATA_DIR` stores Radarr/Sonarr API keys and Dubarr exposes a full control WebUI/API:
1. **Filesystem Hardening:** `DATA_DIR` is created with `0700` permissions and sensitive files (`dubarr.db`, `.master.key`) with `0600` permissions on POSIX (and owner-restricted ACLs on Windows).
2. **At-Rest Encryption (`AES-256-GCM`):**
   * Radarr/Sonarr API keys are never stored in plaintext. They are encrypted using **AES-256-GCM** with a unique random 96-bit nonce per record (`arr_instances.api_key_enc`).
   * **Master Key Hierarchy:**
     * *Native Desktop (Windows/Linux):* When an OS credential store is available (Windows Credential Manager / Linux Secret Service), the 256-bit master key is stored in the **OS Keyring** so copying `DATA_DIR` alone cannot decrypt API keys.
     * *Headless / Docker:* Defaults to a CSPRNG 32-byte `.master.key` (`0600`) generated inside `DATA_DIR` on first startup (protecting `dubarr.db` exports and preventing plaintext config leaks), with optional support for a user-supplied **keyfile path** (e.g., Docker Secret `/run/secrets/dubarr_master_key`) or optional **Unlock Passphrase** (Argon2id KEK) configured via the Setup UI.
3. **Mandatory Authentication & Session Security:**
   * On first boot (`users` table empty), the WebUI opens the **Setup Wizard** requiring creation of an admin account (hashed with **Argon2id**).
   * Sessions use signed `HttpOnly; SameSite=Lax` cookies; external automation uses revocable Bearer API tokens stored as **SHA-256 hashes**.
4. **Write-Only Secrets & Log Scrubbing:**
   * `GET /api/arr/instances` **never** returns decrypted keys—only `api_key_masked` (e.g., `"••••a9f2"`) and `has_api_key: true`.
   * Requests to Radarr/Sonarr always transmit `X-Api-Key` in HTTP headers (never query strings), and logs/errors scrub secret headers.
5. **Canonical Path Traversal Guards:**
   * All filesystem browsing, watch scanning, vault writes, and target file accesses resolve symlinks (`filepath.EvalSymlinks`) and verify paths remain strictly inside configured roots.

### 3.3. Storage Layout & ISO-2 Vault Naming
Dubarr separates five logical storage areas (configured via the Setup Wizard / Settings UI):

1. **`DATA_DIR`** (e.g., `/config` or `./data`) — `dubarr.db`, `.master.key`, logs, and default `work/` scratch folder.
2. **`WATCH_DIR`** (e.g., `/watch`) — Dropzone for incoming donor video files.
3. **`VAULT_DIR`** (e.g., `/audio-vault`) — **Write-once / immutable audio archive** exclusively managed by Dubarr:
   * **Movies:** `{VAULT_DIR}/Movies/{Title} ({Year}) {tmdb-{TmdbId}}/`
     * Primary track for language `{iso2}`: `{Title} ({Year}).{iso2}.mka`
       *(e.g., `Spirited Away (2001).pl.mka`, `Spirited Away (2001).ja.mka`)*
     * Disambiguated path (when multiple tracks share the same `{iso2}` in a donor or across multiple donor imports):
       `{Title} ({Year}).{variant}.{codec}-{ch}.{shortGroup}.{iso2}.mka`
       *(e.g., `Shrek (2001).lektor.ac3-5ch.a4f9c2.pl.mka`)*
   * **Series / Anime:** `{VAULT_DIR}/Series/{Title} ({Year}) {tvdb-{TvdbId}}/Season {SS}/`
     * Primary track: `{Title} - S{SS}E{EE}.{iso2}.mka`
     * Disambiguated path: `{Title} - S{SS}E{EE}.{variant}.{codec}-{ch}.{shortGroup}.{iso2}.mka`
   * **ISO 639 Normalization & Self-Describing Containers:**
     * Incoming `ffprobe` language codes (`pol`, `jpn`, `ger`/`deu`, `fre`/`fra`, `eng`, etc.) are normalized to canonical **ISO 639-1 two-letter codes** (`pl`, `ja`, `de`, `fr`, `en`; falling back to ISO 639-2/3 or `und` only for rare languages lacking an ISO-2 code).
     * Every `.mka` always ends with `.{iso2}.mka` and carries embedded Matroska track tags (`LanguageIETF` / legacy `Language` + track title) written during extraction.
4. **`WORK_DIR`** (e.g., `/config/work` or a dedicated fast SSD mount `/work`) — Isolated scratch workspace:
   * `{WORK_DIR}/extract/{import_id}/` — Staging area for lossless extraction + `ffprobe` validation before atomic placement into `VAULT_DIR`.
   * `{WORK_DIR}/jobs/{job_id}/` — Per-job workspace holding **copied** `.mka` files from `VAULT_DIR`, WAV chunks, `.ogg` studio previews, tempo-adjusted audio files, and Work Area-only transcoded audio copies.
5. **`MEDIA_ROOTS`** (e.g., `/media/...`) — Target media library folders with Radarr/Sonarr **Path Mappings** (e.g., `/data/media/movies` -> `/media/movies`).

### 3.4. Vault Immutability (Copy-on-Work Contract)
* After extraction into `{WORK_DIR}/extract/{import_id}/` and `ffprobe` verification, Dubarr computes the file's **SHA-256** digest and atomically places the `.mka` into `VAULT_DIR` (setting POSIX mode `0444` read-only if enabled in settings).
* **No synchronization, preview generation, tempo/FPS conversion, metadata tagging, or muxing operation ever reads-and-modifies or creates temp files inside `VAULT_DIR`.**
* Whenever a job (`sync`, `preview`, or `mux`) needs a Vault track, Dubarr copies that `.mka` from `VAULT_DIR` into `{WORK_DIR}/jobs/{job_id}/` first, verifies integrity against `audio_tracks.sha256`, and runs all `ffmpeg` / `dubarr-sync` / `mkvmerge` audio reads strictly against the `WORK_DIR` copy.
* *(Large target `.mkv` video files in `MEDIA_ROOTS` are opened read-only in place rather than copied into `WORK_DIR`.)*

### 3.5. File Safety, Processing Gate, and Atomic Muxing
* **Watch Folder Stability Check:** Ignores partial/temp extensions (`*.!qB`, `*.part`, `*.tmp`, `*.crdownload`, `*.tdarr`). Verifies file size stability (two consecutive checks >= 4 seconds apart must match) before probing with `ffprobe`.
* **Destination Filename Tag Filtering (Processing Gate, targets only):**
  * Settings define `required_filename_tags` (empty by default — ADR-0012; `[TDARR]` is only an example marker), `tag_match_mode` (`ALL`, `ANY`, or `REGEX`), `tag_case_sensitive`, `excluded_filename_tags`, and `enforce_tag_filter` (boolean, default `true`), plus the optional arr criterion of ADR-0009 (`source_mode`, `required_arr_tags`/`excluded_arr_tags`/`root_folder_paths`). Historical `require_av1_warning`/`require_av1_strict` flags are not implemented.
  * **Import & Vault Archiving:** Never blocked by target tags—you can archive donor tracks into `VAULT_DIR` regardless of any external pipeline's progress.
  * **Sync & Mux Eligibility:** Target files missing `required_filename_tags` are marked `waiting_for_tags` and **blocked from Sync/Mux** while `enforce_tag_filter` is enabled. Donor ingestion is never gated (ADR-0005).
  * **External-Pipeline Rename Drift Recovery:** If an external tool recently renamed a file (e.g., `Movie (2021).mkv` -> `Movie (2021) [AV1].mkv`) before Radarr/Sonarr refreshed, and `target_file_path` is missing on disk, Dubarr triggers a targeted `RescanMovie`/`RescanSeries` (and checks `media_folder_path`) to refresh the updated filename before evaluating tags.
* **Atomic Muxing (Zero Risk of Corrupting Target `.mkv` Files):**
  1. Re-verify right before muxing that the target file exists, still matches `required_filename_tags`, and has stable mtime/size (no active external-pipeline/temp files in the folder).
  2. Run `mkvmerge` reading the target `.mkv` read-only and the **copied/adjusted audio track inside `WORK_DIR`**, writing output to `<target>.mkv.dubarr.tmp` in the same directory as `<target>.mkv`.
  3. After exit code `0`, `ffprobe` validates `<target>.mkv.dubarr.tmp` (verifying original video stream, all existing streams, and the newly added stream are intact with matching stream counts/durations).
  4. Atomically replace `<target>.mkv` (`os.Rename`), optionally keeping or cleaning up a `.bak` copy depending on user settings, then clean up `{WORK_DIR}/jobs/{job_id}/`.

> ⚠ **Superseded (gate concept):** this section describes the original filename-tag-only “Tdarr Gate”. The governing concept is the Processing Gate: ADR-0005 (filename criterion, **targets only**; donor ingestion is never tag-gated), amended by ADR-0009 (optional arr criterion: `source_mode` = `filename` | `arr` | `both`, `required_arr_tags`/`excluded_arr_tags`/`root_folder_paths`, sources AND-ed, fail closed) with the neutral empty default from ADR-0012. `required_filename_tags` ships empty.

---

## 4. SQLite Database Schema (`schema.sql`)

```sql
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

-- 1. Authentication & API Access
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

-- 2. Application settings (JSON-serialized values stored in App Data folder)
- Keys include: storage.paths, languages.policy, target.tag_filter, sync.defaults, security.options, mux.options, audio.options (ordered opt-in Transcode Rules; empty by default)
CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- 3. Radarr / Sonarr instances (supports multiple instances, e.g., Radarr, Sonarr, Sonarr-Anime)
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

-- 4. Cached media library synced from Radarr / Sonarr
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
    media_folder_path TEXT,               -- Mapped local folder path (handles external-pipeline filename renames)
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

-- 5. Watch folder queue awaiting user approval (Approval Queue)
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

-- 6. Movies/Series cataloged in Audio Vault
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

-- 7. Archived immutable .mka audio tracks in Audio Vault
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

-- 8. Synchronization and Muxing jobs (executed strictly on WORK_DIR copies)
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

5. Synchronization Engine Specification (Go + FFmpeg + Rust CLI)
5.1. dubarr-sync Binary Contract (Rust)

The engine is a stdin/stdout JSON bridge: it reads one job object from stdin and prints one result object to stdout (exit codes: 0 success, 2 usage error, 3 analysis failure). `--selftest` runs a built-in synthetic check and exits. There are no `--target`/`--donor`/`--max-offset-sec` flags; the settled ±180 s bound lives in `sync.defaults.max_offset_sec`.

A job selects a window from two mono sources (a WAV file path or inline PCM samples) at the same timeline positions on both sides:

Bash

{
  "target": {"path": "/work/jobs/42/target.wav"},
  "donor":  {"path": "/work/jobs/42/donor.wav"},
  "slice_start_sec": 420.0,
  "slice_len_sec": 190.0,
  "window_sec": 190.0
}

`window_sec` bounds the absolute offset search and defaults to 5 s when omitted (`sync-engine/src/lib.rs`).

Algorithm & Sign Convention inside Rust:

    Load both sources as mono samples (WAV path or inline PCM); reject mismatched sample rates.

    Take the slice [slice_start_sec, slice_start_sec + slice_len_sec) from both sources independently.

    Compute fast cross-correlation using FFT (rustfft): IFFT(FFT(target) * conj(FFT(donor))).

    Locate the correlation peak within [-window_sec, +window_sec].

    Derive sign-consistent offset (offset_ms = t_target - t_donor):

        Positive offset_ms (+1420) means the target event occurs 1420 ms later than the donor -> donor must be delayed (mkvmerge --sync 0:+1420).

        Negative offset_ms (-850) means the target event occurs 850 ms earlier than the donor -> donor must be advanced (mkvmerge --sync 0:-850).

    Score prominence as correlation_quality (peak over the median lag response, 0..1) and report the analysed window actually used.

    Output JSON to stdout:

JSON

{
  "offset_ms": 1420.0,
  "correlation_quality": 0.964,
  "sample_rate": 8000,
  "analyzed": {"start": 420.0, "len": 190.0}
}

5.2. Copy-on-Work & Multi-Point Analysis in Go

Checking only the start of a movie fails to detect FPS drift or mid-movie edits. Go orchestrates:

    Copy-on-Work Preparation:

        Create {WORK_DIR}/jobs/{job_id}/.

        Copy target_track (and all sibling reference tracks in its group_uuid, if needed) from VAULT_DIR into {WORK_DIR}/jobs/{job_id}/ and verify SHA-256 checksums. VAULT_DIR is never touched again for this job.

    Reference Language Resolution (sync_method selection):

        Inspect target MKV audio streams via ffprobe and match against sibling tracks in target_track's group_uuid:

            Priority 1 (same_lang_reference — Original Language): If arr_library_cache.original_language_iso2 exists in both the donor group and the target MKV (e.g., ja <-> ja), select that pair.

            Priority 2 (same_lang_reference — Other Shared Language): Otherwise, pick any other non-target shared language present in both the donor group and target MKV (e.g., en <-> en). Implemented tie-break: channel count, then track index (probe.AudioTrack carries no bitrate; documented in reflang.go).

            Priority 3 (lowpass_fallback): If no shared reference exists, compare the copied target_track (e.g., pl) directly against the target MKV's Original Language (or en, or first non-commentary) audio stream.

    Whole-Movie Decode via FFmpeg (once per side):

        ffmpeg decodes the chosen target stream and the copied donor track once each into whole-movie mono 8 kHz pcm_f32le WAVs ({job_dir}/target.wav and {job_dir}/donor.wav), applying the band-shaping filter:

            same_lang_reference: speech band shaping (highpass=f=80,lowpass=f=3500).

            lowpass_fallback: aggressive low-pass filter (lowpass=f=200,highpass=f=30) isolating bass, explosions, and score.

    Sequential Per-Checkpoint Correlation:

        Go walks the configured checkpoint percentages (settled default 10/25/40/55/70/85% of the shorter duration, `sync.defaults.checkpoint_positions_pct`) in order. For each checkpoint it sends the SAME two WAV paths plus slice_start_sec/slice_len_sec/window_sec to a fresh dubarr-sync process; there are no concurrent checkpoint goroutines.

        The analysed slice length is max(checkpoint_window_sec, max_offset_sec + 10 s) — 190 s with the settled 150 s window and ±180 s bound — centred on the checkpoint and clamped into the file; files shorter than the slice are analysed whole.

    Anomaly Classification:

        Constant offset: spread across valid checkpoints < 40 ms -> final offset is median, speed_mode = 'none'.

        Linear drift (FPS mismatch): linear regression shows a significant slope consistent with a FPS ratio (e.g., 23.976/25.000 or 25.000/23.976, R² > 0.97) -> sets detected_fps_ratio and recommends tempo mode.

        Non-linear jump / mid-movie cut: step change between checkpoints -> flags ready_for_review with a clear warning for manual inspection in Sync Studio.

        Verdict precedence: out_of_range (any offset beyond ±max_offset_sec) > unreliable (checkpoint quality below threshold) > drifting > ok.

> **Implementation pointer (issue #24):** this section previously described per-checkpoint `target_cp*.wav`/`donor_cp*.wav` extraction with concurrent goroutines; that design was never implemented. The text above documents the shipped behaviour (whole-movie decode + sequential per-checkpoint engine calls). Bounding the whole-movie decode’s memory/disk cost is a separate performance effort and is intentionally not documented here.

5.3. Final Muxing Rules

Always read audio from {WORK_DIR}/jobs/{job_id}/, never from VAULT_DIR. Pass --language 0:{target_lang_iso2} (MKVToolNix natively converts ISO 639-1 two-letter codes such as pl into both LanguageIETF and legacy Matroska Language), and preserve the configured track_title_in_mkv.

    Case 0 (an enabled Transcode Rule matches the Work Area copy — ADR-0010):

        Rules are ordered and global; conditions match the PRE-TEMPO vault-track metadata (codec, channels/layout, bitrate, sample rate, language, variant) and the first matching rule wins. The action may keep the track or re-encode it to Opus, AAC, FLAC or PCM with explicit bitrate/channels/layout/sample rate. A matching rule with codec `keep` stops evaluation.

        The re-encode reads the copied vault track and writes a temporary MKA in {WORK_DIR}/jobs/{job_id}/ using one ffmpeg invocation; if a tempo factor exists it is applied in the SAME pass (precedence: tempo first in the filter chain so the encoder sees the tempo-corrected stream; ffmpeg ordering documented in code). ffmpeg then runs ffprobe on the result and verifies the requested codec, channel count and duration within tolerance. Any mismatch fails the job with a clear error; the required encoder must be available before the job starts.

        The Audio Vault file is never written; its SHA-256 is unchanged. Language tags and the configured track title are carried onto the appended track by mkvmerge exactly as in the other cases. Tracks archived before Transcode Rules existed carry NULL bitrate/sample_rate, so bitrate/sample-rate conditions match only archives recorded with metadata (fresh or re-archived).

    Case 1 (speed_mode == 'none', no matching Transcode Rule):

        Invoke mkvmerge:
        Bash

        mkvmerge -o "<target>.mkv.dubarr.tmp" "<target>.mkv" \
          --language 0:<target_lang_iso2> \
          --track-name 0:"<track_title_in_mkv>" \
          --sync 0:<final_offset_ms> \
          --default-track-flag 0:<yes|no> \
          "<work_dir>/jobs/<job_id>/copied_target_track.<lang>.mka"

        100% lossless for both video and audio.

    Case 2 (speed_mode != 'none' — FPS/tempo adjustment required):

        Step A: ffmpeg reads <work_dir>/jobs/<job_id>/copied_target_track.<lang>.mka and writes an adjusted temporary track <work_dir>/jobs/<job_id>/adjusted.<ext> (Opus or E-AC3, preserving original channel count/layout), applying:

            preserve_pitch: rubberband=tempo=<ratio> (fallback to atempo=<ratio> if librubberband is unavailable).

            pal_pitch_restore: asetrate=<sr>*<ratio>,aresample=<sr> (restores theatrical pitch when converting 25 fps PAL speedup -> 23.976 fps).

            Initial offset trimming (atrim) or silence padding (adelay) according to final_offset_ms.

        Step B: mkvmerge muxes <work_dir>/jobs/<job_id>/adjusted.<ext> into <target>.mkv.dubarr.tmp without touching the video stream or existing audio streams.

6. REST API & WebSocket Contract (Go Backend)

All UI operations and external automation communicate via a unified JSON API under /api. Except for /api/health, /api/auth/status, /api/auth/setup, and /api/auth/login, all endpoints require an authenticated session cookie or Authorization: Bearer <token>.
Authentication, Setup & Settings

    GET /api/auth/status — returns { initialized: bool, authenticated: bool, keyring_available: bool }.

    POST /api/auth/setup — first-run wizard endpoint (valid only when users is empty): creates admin user and initial configuration.

    POST /api/auth/login / POST /api/auth/logout — session management (HttpOnly; SameSite=Lax).

    GET /api/auth/tokens / POST /api/auth/tokens / DELETE /api/auth/tokens/{id} — manage hashed automation API tokens.

    GET /api/settings / PUT /api/settings — read/update storage.paths (watch_dir, vault_dir, work_dir, media_roots), languages.policy (target languages, export mode, commentary exclusion), target.tag_filter (required/excluded filename tags, match mode, case sensitivity, enforce toggle, plus `source_mode` = filename|arr|both and the optional arr criterion `required_arr_tags`/`excluded_arr_tags`/`root_folder_paths` — ADR-0009; empty filename list is the neutral default, ADR-0012), mux.options (backup, track title, output mode), audio.options (ordered opt-in Transcode Rules; absent/empty means lossless copy), and sync.defaults.

    GET /api/settings/export — exports sanitized configuration JSON excluding all API keys, hashes, and master keys.

    GET /api/system/browse?path=... — safe server directory browser with path canonicalization and permission checks.

    GET /api/health — backend status and availability/versions of ffmpeg, ffprobe, mkvmerge, dubarr-sync.

*arr Instances & Library Sync

    GET /api/arr/instances — returns instances with masked keys (api_key_masked, has_api_key) and path mappings.

    POST /api/arr/instances / PATCH /api/arr/instances/{id} / DELETE /api/arr/instances/{id} — stores API key encrypted with AES-256-GCM.

    POST /api/arr/instances/test — tests Radarr/Sonarr connectivity and lists remote root folders before/after saving.

    POST /api/arr/sync — fetches movies, series, episodes, originalLanguage, and file paths from Radarr/Sonarr into arr_library_cache, evaluates target.tag_filter (eligible vs waiting_for_tags vs missing_target), and inspects local target files via ffprobe for audio_languages_json & video_codec.

Watch Folder & Approval Queue

    GET /api/imports — lists items in import_queue (filterable by status).

    POST /api/imports/rescan — triggers immediate scan of WATCH_DIR.

    GET /api/imports/search-library?q=... — searches arr_library_cache (and optionally Radarr/Sonarr /lookup) for manual matching when /api/v3/parse fails (e.g., localized titles like Kraina.Lodu.2013.PL.mkv).

    POST /api/imports/{id}/approve — body: { library_cache_id, exports: [{ stream_index, lang_iso2, variant, track_title }], delete_source }. Extracts into {WORK_DIR}/extract/{id}/, validates, computes SHA-256, and atomically archives into VAULT_DIR with ISO-2 filenames.

    POST /api/imports/{id}/ignore — marks queue entry as ignored (optionally deletes donor file).

Audio Vault (Catalog Browser)

    GET /api/vault/movies / GET /api/vault/series — Radarr/Sonarr-style library view showing:

        target file languages (audio_languages_json), original language, and Processing Gate eligibility status (eligible / waiting_for_tags / missing_target; ADR-0005, ADR-0009),

        archived .mka tracks in Vault grouped by donor release (lang_iso2, variant, codec, channels),

        clear status badges (e.g., "Waiting for required tag", "Vault track ready to sync & mux", "Target already has pl audio").

    DELETE /api/vault/tracks/{id} — explicitly deletes a .mka file from Vault disk and database (the only user-initiated Vault deletion path).

Synchronization, Manual Studio & Muxing

    POST /api/sync/jobs — body: { target_track_id, target_library_cache_id, force_ignore_tag_filter? }. Rejects waiting_for_tags targets unless explicitly overridden (when allowed by settings). Copies .mka files to WORK_DIR and runs multi-point analysis.

    GET /api/sync/jobs / GET /api/sync/jobs/{id} — job status, resolved reference strategy (same_lang_reference / lowpass_fallback), and checkpoint report (checkpoints_json).

    GET /api/sync/jobs/{id}/preview?timestamp_sec=1800&window_sec=60 — dynamically slices lightweight 60s audio clips (target.ogg / donor.ogg) into {WORK_DIR}/jobs/{id}/ from the copied Vault track for wavesurfer.js visualization.

    PATCH /api/sync/jobs/{id} — persists manual overrides (manual_offset_ms, speed_mode, track_title_in_mkv, set_as_default).

    POST /api/sync/jobs/{id}/mux — re-verifies target tag eligibility & stability, performs atomic muxing from the WORK_DIR copy, validates via ffprobe, triggers RescanMovie/RescanSeries, and cleans up WORK_DIR job files.

Real-Time Updates (WebSocket)

    WS /api/ws — pushes authenticated live JSON events: import_detected, extract_progress, sync_progress, mux_progress, job_completed, job_failed.

7. Frontend Specification (React WebUI)

Dark-mode UI inspired by the *arr ecosystem (Radarr/Sonarr and similar), responsive across desktop and mobile browsers:

    First-Run Setup Wizard (/setup): Guided setup for Admin credentials, server directory browser (WATCH_DIR, VAULT_DIR, WORK_DIR, MEDIA_ROOTS), target/export language policy, Processing Gate filename-tag criterion, and Radarr/Sonarr connections.

    Dashboard / Overview: Storage summary for Audio Vault & Work scratch usage, Processing Gate waiting-for-tags count, and quick-action cards for eligible tagged target files missing target-language audio that have tracks waiting in Vault.

    Import / Approval Queue (/imports):

        Cards for donor files detected in WATCH_DIR with matched Radarr/Sonarr entry (showing originalLanguage) and manual search override.

        ffprobe audio stream table with checkboxes, ISO-2 language override selector (handling und tracks), variant selector (dubbing, lektor, original, standard, commentary), and auto-selection driven by languages.policy.

    Audio Vault (/vault): Movies and Series/Anime views displaying ISO-2 track badges (PL DUB, JA ORIG, EN), Processing Gate eligibility status (eligible / waiting for tags / missing target), and "Sync & Mux to Target" actions.

    Sync Studio (/sync/{jobId}):

        Shows resolved sync strategy (e.g., Matched via JA Original Reference or Low-Pass 30–200 Hz Fallback) and multi-point checkpoint chart/table.

        Spectrogram & Waveform Editor: wavesurfer.js dual tracks across 60s slices (10%, 25%, 50%, 75%, 90%), precision nudge buttons (±1ms, ±10ms, ±100ms), and Web Audio API dual-track live mixer.

        Speed mode selector (None, Preserve Pitch, PAL Pitch Restore) and "Approve & Mux to MKV" action.

    Settings (/settings):

        Tabs for Storage Paths, Language & Export Flow, Processing Gate (filename tags + optional arr criteria), Radarr / Sonarr Instances (masked API keys + Test Connection), Security & API Tokens, and System Health (ffmpeg, ffprobe, mkvmerge, dubarr-sync).

8. Packaging and Deployment (Docker + Native Windows/Linux)
8.1. Docker (Primary Server Target)

    Multi-stage Dockerfile:

        Stage 1 (node:22-alpine): builds React frontend (npm run build -> dist/).

        Stage 2 (rust:1-bookworm or alpine): builds dubarr-sync (cargo build --release).

        Stage 3 (golang:1.27-alpine): compiles Go backend with embedded frontend/dist (go:embed).

        Stage 4 (Minimal runtime, e.g., debian:bookworm-slim or alpine): contains only the dubarr binary, dubarr-sync binary, ffmpeg, mkvtoolnix (mkvmerge), and CA certificates.

    Exposes port 8080, defaults --data-dir=/config and --listen=0.0.0.0:8080, supports container UID/GID mapping (PUID/PGID), and mounts /config, /watch, /audio-vault, /media (and optional /work for fast scratch SSD).

    docker-compose.yml contains no app settings or API keys in environment:—everything is configured via the Setup Wizard / WebUI inside /config.

8.2. Native Desktop Mode (Windows & Linux)

    When launched outside Docker, dubarr defaults --data-dir=./data and --listen=127.0.0.1:8080, and with --desktop launches the default browser (or Edge/Chrome in --app=http://127.0.0.1:8080 mode).

    Uses the OS Keyring for the master encryption key when available, falling back cleanly to 0600 keyfile storage.

    External binaries (ffmpeg, ffprobe, mkvmerge, dubarr-sync) are resolved from `PATH` only (`exec.LookPath`; `backend/internal/sync/runner.go:81`, `/api/health`). There is no `./bin/` next-to-executable lookup; ship both binaries in one directory on `PATH` (see `docs/RELEASING.md`).

9. Phased Implementation Plan (For AI Orchestrator)

Execute sequentially in 5 distinct milestones, verifying builds and automated tests at the end of each milestone before proceeding.
Milestone 1: Foundations, Security Store, SQLite, Go + React Skeleton + Docker

    Tasks:

        Initialize monorepo layout (backend/, sync-engine/, frontend/, docker/).

        Implement --data-dir / --listen / --desktop bootstrapper (zero app-setting env vars), SQLite initialization (schema.sql + migrations), Argon2id auth + sessions + hashed API tokens, and AES-256-GCM encrypted secret store (OS Keyring + .master.key 0600 fallback).

        Implement /api/health, /api/auth/*, /api/settings, and /api/system/browse with canonical path validation.

        Build React shell with First-Run Setup Wizard and Settings skeleton embedded via go:embed.

        Create multi-stage Dockerfile and clean docker-compose.yml.

    Acceptance Criteria: Container builds and starts cleanly with zero app env vars; Setup Wizard creates admin user, persists paths/rules inside DATA_DIR, and /api/health reports all tools available.

Milestone 2: Radarr/Sonarr Integration, Target Tag Filter & Approval Queue

    Tasks:

        Build Radarr and Sonarr REST clients (/api/v3/parse, /api/v3/movie, /api/v3/series, /api/v3/episode, /api/v3/episodefile, /api/v3/command) using encrypted X-Api-Key headers and path_mappings translation.

        Implement POST /api/arr/sync syncing originalLanguage, media_folder_path, target_file_path, evaluating target.tag_filter (eligible / waiting_for_tags / missing_target), and inspecting target streams via ffprobe for audio_languages_json.

        Implement Watch Folder stability scanner + ffprobe parser + ISO 639-1 normalization.

        Build *Settings (arr, Languages, Tag Filter) and Approval Queue (/imports) UI with per-stream ISO-2/variant selectors and manual library search fallback.

    Acceptance Criteria: Dropping a donor video into WATCH_DIR triggers ffprobe + /api/v3/parse and displays an approval card pre-selected via configured language rules; synced targets accurately show eligible vs waiting_for_tags.

Milestone 3: Immutable Audio Vault & Lossless ISO-2 Extraction (via WORK_DIR Staging)

    Tasks:

        Implement POST /api/imports/{id}/approve and background worker queue with WebSocket progress events.

        Extract selected streams via ffmpeg -map 0:<idx> -c:a copy into {WORK_DIR}/extract/{id}/ with Matroska ISO-2 language & title tags, validate via ffprobe, compute SHA-256, and atomically move into {VAULT_DIR} using <media filename>.<lang>.mka (with collision disambiguation before .<lang>.mka).

        Build Audio Vault (/vault) UI displaying ISO-2 track badges, target audio languages, and Processing Gate eligibility status.

    Acceptance Criteria: Approving an import creates validated .mka files ending in .{iso2}.mka inside VAULT_DIR, records sha256 hashes in audio_tracks, and leaves zero staging files behind.

Milestone 4: Rust Auto-Sync Engine + Copy-on-Work Multi-Point Analysis in Go

    Tasks:

        Implement dubarr-sync in sync-engine/ (clap, hound, rustfft, serde, serde_json) enforcing offset_ms = t_target - t_donor, plus unit tests with synthetic shifts (+1500ms, -800ms).

        Implement Copy-on-Work job initialization (copying .mka tracks from VAULT_DIR to {WORK_DIR}/jobs/{id}/ and verifying SHA-256).

        Implement generalized Reference Language Resolver (same_lang_reference prioritizing original_language_iso2, then any shared language, then lowpass_fallback), Go multi-point FFmpeg window slicing into WORK_DIR, concurrent dubarr-sync execution, and linear regression classification.

    Acceptance Criteria: cargo test and go test pass 100%; Vault .mka files remain byte-for-byte untouched (verified even when VAULT_DIR is mounted read-only during sync); Go returns accurate checkpoint reports for both original-language matches and low-pass fallbacks.

Milestone 5: Spectrogram Studio + Tag-Gated Atomic Muxing + *arr Rescan

    Tasks:

        Implement 60s .ogg preview clip generator (/api/sync/jobs/{id}/preview) reading strictly from the WORK_DIR copy.

        Build Sync Studio (/sync/{jobId}) in React with wavesurfer.js (waveform + spectrogram), ±1ms/±10ms/±100ms nudge buttons, and Web Audio API dual-track live mixer.

        Implement tag-gated atomic muxing (POST /api/sync/jobs/{id}/mux): re-verify target filename tags & folder stability (with Radarr/Sonarr rename-drift refresh if cached file moved), run mkvmerge --language 0:{iso2} --sync ... (or ffmpeg tempo adjustment on the WORK_DIR copy + mkvmerge), validate output via ffprobe, atomically rename, trigger RescanMovie/RescanSeries, and clean up {WORK_DIR}/jobs/{id}/.

    Acceptance Criteria: End-to-end flow (*Watch -> Approve -> Vault -> Copy-on-Work Auto/Manual Sync -> Tag-Verified Atomic Mux -> arr Rescan) completes with zero modification to VAULT_DIR files, zero alteration to target video streams, zero muxing of untagged targets, and zero leaked secrets.

10. Guidelines for the AI Orchestrator (Code Quality Rules)

    No Hallucinated Dependencies: Use only well-established, stable libraries: Go (modernc.org/sqlite, chi or gin or net/http, gorilla/websocket, golang.org/x/crypto/argon2), Rust (clap, hound, rustfft, serde, serde_json), React (wavesurfer.js, lucide-react, tailwindcss).

    Process Safety & Observability: Every external subprocess (ffmpeg, ffprobe, mkvmerge, dubarr-sync) must accept a context.Context for timeouts/cancellation, and capture full stderr on failure for logging and UI error reporting (with secrets scrubbed).

    Strict API Separation: The Go backend serves no server-rendered HTML templates—only the compiled static SPA and authenticated JSON/WebSocket endpoints under /api.

    Security & Immutability Invariants: Never accept app settings or API keys via env vars; never return plaintext API keys in API responses; never open VAULT_DIR files for write outside initial atomic import; always gate automatic/default muxing on required_filename_tags.

    Testability: AES-256-GCM encryption/decryption, API key redaction, canonical path guards, ISO 639-1 normalization, target filename tag matching, path-mapping logic, linear regression drift classification, Copy-on-Work Vault immutability verification, and FFT cross-correlation must be covered by automated tests (go test ./..., cargo test).
