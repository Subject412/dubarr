-- 0004: arr tag and root-folder metadata for the Processing Gate (ADR-0009).
--
-- Arr tags are matched by label and root folders by canonical path, so the
-- library cache records both alongside each item. The snapshot is rewritten on
-- every sync pass (never appended), which is how removed tags disappear.
--
-- Both tag columns default to '[]' so rows written before this migration
-- decode as "no arr metadata" instead of NULL. Nothing reads these columns for
-- eligibility yet; M9c's gate slice does.
--
-- schema.sql keeps mirroring 0001_initial.sql (see TestSchemaSQLMirrorsMigration),
-- exactly like 0002 and 0003; this forward migration is the authoritative change.

ALTER TABLE arr_library_cache ADD COLUMN arr_tag_ids_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE arr_library_cache ADD COLUMN arr_tag_labels_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE arr_library_cache ADD COLUMN root_folder_path TEXT;
