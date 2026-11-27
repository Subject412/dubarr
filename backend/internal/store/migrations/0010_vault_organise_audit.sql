-- 0010: audit the vault reorganisation (#47).
--
-- Renaming an archived track is the second operator lifecycle action that
-- touches VAULT_DIR (the first is remove-to-Trash, ADR-0015), so it is audited
-- the same way: one run row per apply and one file row per track it touched.
--
-- Audit tables only. audio_tracks and vault_media are untouched: file_path is
-- already UNIQUE and already authoritative, which is exactly why a swap is
-- refused (path_cycle) rather than attempted inside one transaction — a UNIQUE
-- column cannot hold two names for one row even for the length of a
-- transaction.
--
-- result is a closed vocabulary so the UI translates it and never parses prose:
--   renamed     the file moved and file_path was rewritten (status applied)
--   renamed_back the file moved and was put back by the undo (rolled_back)
--   failed      the file was never moved, or could not be found afterwards
--   reconciled  the undo failed and the DB was pointed at whatever is on disk
--               (status needs_attention: loud on purpose)
--
-- status 'rolled_back' means the vault and the database are both exactly as
-- they were; 'needs_attention' means the database was reconciled to disk and an
-- operator has to look. There is no 'running' status: the run row is written
-- inside the phase C transaction (applied) or immediately after compensation,
-- so a row that exists always describes a finished outcome.
--
-- plan_hash is the sha256 of the plan the operator previewed (sorted
-- (track_id, source, destination, size, mtime_ns) tuples), which is what makes a
-- run reconstructable after the fact without trusting the UI's word for it.
--
-- ON DELETE CASCADE from files to runs keeps a pruned run from leaving orphans;
-- track_id is ON DELETE SET NULL because the path history outlives a track row
-- (removing a track to Trash must not erase the record of the rename that
-- preceded it), so source_path/destination_path are stored as text, not derived.
--
-- schema.sql is NOT edited: it mirrors 0001_initial.sql (TestSchemaSQLMirrorsMigration),
-- exactly like 0002-0009; this forward migration is the authoritative change.

CREATE TABLE IF NOT EXISTS vault_organise_runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at DATETIME NOT NULL,
    finished_at DATETIME,
    status TEXT NOT NULL CHECK (status IN ('applied', 'rolled_back', 'needs_attention')),
    plan_hash TEXT NOT NULL,
    file_count INTEGER NOT NULL,
    actor TEXT,                           -- who asked; NULL when the API had no identity
    detail TEXT                           -- e.g. template_change, stage=sql: <error>
);

CREATE TABLE IF NOT EXISTS vault_organise_files (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL REFERENCES vault_organise_runs(id) ON DELETE CASCADE,
    track_id INTEGER REFERENCES audio_tracks(id) ON DELETE SET NULL,
    source_path TEXT NOT NULL,
    destination_path TEXT NOT NULL,
    result TEXT NOT NULL CHECK (result IN ('renamed', 'renamed_back', 'failed', 'reconciled'))
);

CREATE INDEX IF NOT EXISTS idx_vault_organise_files_run ON vault_organise_files(run_id);
CREATE INDEX IF NOT EXISTS idx_vault_organise_files_track ON vault_organise_files(track_id);
