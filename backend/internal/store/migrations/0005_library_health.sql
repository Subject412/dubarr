-- 0005: per-instance arr sync health and library row freshness (#29).
--
-- The Library screen has to answer "did the last sync pass work?" and "is this
-- row still in the arr library?" without re-querying the instances. Neither is
-- derivable from the existing columns: last_sync_at only records the last
-- SUCCESS, so a failing pass is indistinguishable from no pass at all.
--
-- last_sync_ok / last_sync_error / last_attempt_at describe the last ATTEMPT
-- (NULL last_attempt_at = never attempted), while last_sync_at keeps meaning
-- the last successful pass. A failed pass therefore records the error without
-- destroying the last-good snapshot, per ADR-0005 and ADR-0009.
--
-- arr_library_cache.last_seen_at is stamped on every row a pass observes, so a
-- row that stopped appearing can be told apart from one that was deleted.
-- updated_at is deliberately left alone: it tracks content changes, not
-- observation.
--
-- Additive ALTER TABLE only. schema.sql keeps mirroring 0001_initial.sql (see
-- TestSchemaSQLMirrorsMigration), exactly like 0002, 0003 and 0004; this
-- forward migration is the authoritative change. Rollback, if ever needed, is
-- a forward migration 0006 - never an edit to this file.

ALTER TABLE arr_instances ADD COLUMN last_sync_ok INTEGER;
ALTER TABLE arr_instances ADD COLUMN last_sync_error TEXT;
ALTER TABLE arr_instances ADD COLUMN last_attempt_at DATETIME;

ALTER TABLE arr_library_cache ADD COLUMN last_seen_at DATETIME;
