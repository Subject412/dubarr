-- 0006: enforce the arr_library_cache identity the writers already assume (#29).
--
-- Every writer keys a cache row on (arr_instance_id, media_type,
-- external_item_id), but nothing in the schema made that triple unique:
-- idx_arr_cache_lookup, idx_arr_cache_eligibility and idx_arr_cache_instance
-- are all non-unique. Both writers read first and write second — the poller
-- (SyncLibraryCache) reads libraryCacheRows outside its write transaction and
-- then INSERTs per unseen key, and the rescan (UpdateLibraryCacheItem) reads
-- one row and then writes it in a separate transaction — so the invariant has
-- only ever been a convention held by the code paths, never by the database.
--
-- A duplicate would be permanent if one ever appeared: SyncLibraryCache loads
-- the instance's rows into a map keyed by that same triple, so a second row
-- with the same key collapses in that map. Only the map's winner is updated,
-- and the delete loop only iterates map entries, so the collapsed row would
-- never be updated and never be deleted: it would keep a stale
-- target_file_path and a frozen last_seen_at while the API still listed it.
--
-- So this migration makes the invariant explicit (a UNIQUE index) and cleans
-- up any row pair a previous version could leave behind, before the index
-- refuses new ones.
--
-- Scope of the claim, stated plainly: the current writers were not observed
-- producing such a pair. The poller rejects duplicate keys inside one snapshot,
-- and the rescan only rewrites a row its own read found (a missing key is a
-- 404, not an insert). The narrow, timing-dependent window is a row the poller
-- deleted and then re-inserted while a rescan still held the old id, whose
-- zero-row fallback would then insert a second row for the key. So the index is
-- an invariant guard first — it makes the assumed identity explicit and stops a
-- future writer from breaking it — and a cleanup second.
--
-- SURVIVOR RULE (deterministic): highest last_seen_at (NULLs last), then
-- highest id. The most recently observed row is the one carrying live data,
-- and the id breaks ties so the pick is stable across runs.
--
-- ORDER MATTERS: import_queue.matched_library_cache_id and
-- sync_mux_jobs.target_library_cache_id are both REFERENCES
-- arr_library_cache(id) ON DELETE SET NULL. Deleting a loser first would
-- silently unmatch the queue row and the mux job, so both are repointed to the
-- survivor BEFORE any delete.
--
-- Safe on a database with zero duplicates (the normal case) and it never
-- touches a row that is already unique: every statement is set-based, and the
-- delete only targets a group's non-survivor rows.
--
-- READ-ONLY pre-upgrade audit (run it against a live database to see what this
-- migration would do; it changes nothing):
--
--   SELECT c.arr_instance_id, c.media_type, c.external_item_id,
--          COUNT(*) AS rows,
--          (SELECT s.id FROM arr_library_cache s
--            WHERE s.arr_instance_id  = c.arr_instance_id
--              AND s.media_type       = c.media_type
--              AND s.external_item_id = c.external_item_id
--            ORDER BY s.last_seen_at IS NULL, s.last_seen_at DESC, s.id DESC
--            LIMIT 1) AS survivor_id
--     FROM arr_library_cache c
--    GROUP BY c.arr_instance_id, c.media_type, c.external_item_id
--   HAVING COUNT(*) > 1
--    ORDER BY rows DESC;
--
-- schema.sql keeps mirroring 0001_initial.sql (see TestSchemaSQLMirrorsMigration),
-- exactly like 0002-0005; this forward migration is the authoritative change.

-- One grouped definition of the survivor rule, reused by all three steps so
-- the repoint and the delete can never disagree about who survives.
-- last_seen_at IS NULL sorts 0/1, so NULLs land last in ascending order.
CREATE TEMP TABLE _arr_cache_survivor AS
SELECT dup.id AS loser_id, keep.id AS survivor_id
  FROM arr_library_cache dup
  JOIN (
        SELECT id, arr_instance_id, media_type, external_item_id
          FROM (SELECT id, arr_instance_id, media_type, external_item_id,
                       ROW_NUMBER() OVER (
                           PARTITION BY arr_instance_id, media_type, external_item_id
                           ORDER BY last_seen_at IS NULL, last_seen_at DESC, id DESC
                       ) AS rn
                  FROM arr_library_cache)
         WHERE rn = 1
       ) keep
    ON keep.arr_instance_id  = dup.arr_instance_id
   AND keep.media_type       = dup.media_type
   AND keep.external_item_id = dup.external_item_id
 WHERE dup.id <> keep.id;

-- 1. Repoint queue rows off a loser before it disappears.
UPDATE import_queue
   SET matched_library_cache_id = (
        SELECT m.survivor_id FROM _arr_cache_survivor m
         WHERE m.loser_id = import_queue.matched_library_cache_id)
 WHERE matched_library_cache_id IN (SELECT loser_id FROM _arr_cache_survivor);

-- 2. Repoint mux jobs off a loser likewise.
UPDATE sync_mux_jobs
   SET target_library_cache_id = (
        SELECT m.survivor_id FROM _arr_cache_survivor m
         WHERE m.loser_id = sync_mux_jobs.target_library_cache_id)
 WHERE target_library_cache_id IN (SELECT loser_id FROM _arr_cache_survivor);

-- 3. Only now drop the losers.
DELETE FROM arr_library_cache
 WHERE id IN (SELECT loser_id FROM _arr_cache_survivor);

DROP TABLE _arr_cache_survivor;

-- 4. Make the identity load-bearing: a second writer that decides to insert
-- from a stale read now hits this index instead of creating a second row
-- (insertLibraryCacheRow turns that conflict into an update).
CREATE UNIQUE INDEX idx_arr_cache_identity
    ON arr_library_cache(arr_instance_id, media_type, external_item_id);
