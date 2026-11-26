# Vault lifecycle: operator-initiated removal and replacement

ADR-0002 makes the Audio Vault write-once: processing copies tracks out and never
modifies them, so a sync or mux bug cannot corrupt the archive. That protects the
vault from **the pipeline**. It was never meant to protect it from **the
operator**, who needs two things it currently forbids:

- removing a bad import without reaching for the filesystem, while keeping the
  database consistent;
- replacing audio with a better source — a Polish dub archived as AC3, later
  found as lossless.

This ADR defines those two actions and the identity that makes them safe.

## Track identity

A vault track is identified by `(tmdb_or_tvdb_id, language, edition)` — the
identifier arr reports, not arr's own record id and not dubarr's row id. An arr
reinstall or a re-clone therefore does not orphan the vault, and a replacement
updates the same logical track rather than creating a second one.

## Removal

Removing a track deletes its row and **moves the file to the Vault Trash**. It
does not unlink the file. Purging the trash is a separate, explicit action.

Rationale: the operator asked for one-click cleanup, and a misclick must be
recoverable. "Never delete without approval" is satisfied by two deliberate
decisions rather than one accidental one.

## Replacement

Replacing a track's audio keeps the same identity and:

1. archives the new file into the vault;
2. moves the previous file to the Vault Trash;
3. updates the recorded hash, codec, channels, size and provenance on the same
   row;
4. reports which targets were muxed from the previous audio so they can be
   re-muxed.

Rationale: a second row for the same language forces a choice on every later mux
and lets the two drift apart. Replacing in place keeps jobs and targets valid.

## Orphaning

When the mirrored title disappears from arr, the vault track is marked
**orphaned**. It is never deleted automatically: arr's library cleanup is not a
decision about dubarr's archive.

## Invariants that do not move

- The pipeline still never mutates a vault file; only these operator actions do.
- Every removal and replacement is previewed before it runs and recorded after.
- The target video is still never transcoded, and publication is still atomic.
- Nothing is purged without an explicit second decision.

Status: accepted. Amends ADR-0002.

## What this needs before it can be built

None of the following exists today, and the ADR is a decision rather than a
description of the schema:

- **No `edition` column, no orphan marker, no trash table.** `arr_library_cache`
  and `audio_tracks` have no concept of any of them, so this needs a migration
  (0007) covering: edition, an orphaned flag, a trash table, and the identity
  columns.
- **Identity collides with `variant`.** The schema already distinguishes tracks
  by `variant` (`schema.sql:155-156`) — dubbing versus lektor. An identity of
  `(tmdb/tvdb id, language, edition)` would treat a lektor and a dub of the same
  language as the same track. `variant` belongs in the identity, and this ADR
  should be amended when that is decided.
- **Removal cannot delete the row as written.** `sync_mux_jobs.target_track_id`
  is `ON DELETE CASCADE` (`schema.sql:179`), so deleting a track erases the job
  history that ADR-0017 requires in order to explain why a mux ran. The foreign
  key must stop cascading, or removal must tombstone the row instead.
