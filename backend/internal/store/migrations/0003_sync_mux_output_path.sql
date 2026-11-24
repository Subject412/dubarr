-- 0003: persist where a muxed job actually landed (ADR-0011).
--
-- mux.options.output_mode=separate writes the deliverable into
-- storage.paths.output_dir and leaves the Target untouched, so the target path
-- alone no longer identifies the result. The syncworker records mux.Result
-- here and GET /api/sync/jobs exposes it as output_path.
--
-- schema.sql keeps mirroring 0001_initial.sql (see TestSchemaSQLMirrorsMigration),
-- exactly like 0002; this forward migration is the authoritative change.

ALTER TABLE sync_mux_jobs ADD COLUMN output_path TEXT;
