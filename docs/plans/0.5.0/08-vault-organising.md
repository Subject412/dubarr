# 08 — #47 Vault naming templates and safe reorganisation

Branch: `feat/47-vault-organising` · Merge position: 8 of 8 · Depends on: 06 (`held` jobs must be harmless to this preflight) · ADR: 0022 · Migration: 0010

## Goal

The vault works but reads like a hash dump: `<Title> (Year) [whatever].pl.mka`
from extraction-day choices, no season folders, no way to fix it without
`mv` plus hand-edited SQLite. #47 gives Dubarr arr-style naming — a template
per media type, a preview that shows every rename, and an apply that is
all-or-nothing — while preserving the one invariant everything else depends on:
**every vault file still ends in `.<iso2>.mka`**. The suffix is not a style
choice; `audio_tracks.file_path` documents it (`backend/schema.sql:158`) and
`vault.Writer` writes it at publish time. Therefore the template covers the
**stem only**, and `{Language}` is not offered as a token — a token that could
contradict the suffix would be a foot-gun, not a feature.

## Design

### Naming model

```
vault_dir/                                  storage.paths.vault_dir
  Spirited Away (2001)/                     vault_media.vault_folder_path (movies)
    Spirited Away (2001) [Dubbing EAC3 5.1].pl.mka
  The Office (2005)/                        vault_media.vault_folder_path (series)
    Season 02/                              vault.naming.season_folder
      The Office - S02E03 - Office Olympics [Lektor EAC3 2.0].pl.mka
                                              episode template + ".<iso2>.mka"
```

Directory choice is **not** re-derived from a template: the media folder is
already persisted in `vault_media.vault_folder_path` (NOT NULL), so apply moves
a file inside folders that Dubarr already owns. The only directory the template
layer may create is the season subfolder for episode tracks.

### Tokens

| token | source | notes |
|---|---|---|
| `{Title}` | `vault_media.title` (movie title; series title for episodes) | cleaned by `CleanTitle` |
| `{Year}` | `vault_media.year` | empty when 0 |
| `{Season:00}` | `audio_tracks.season_number` | `:00` = zero-pad to 2; `:000` supported; padding modifier invalid on other tokens |
| `{Episode:00}` | `audio_tracks.episode_number` | same |
| `{Episode Title}` | `arr_library_cache.episode_title`, matched by `(tvdb_id/tmdb_id, season, episode)`; empty when unmatched | never invented from the release name |
| `{Variant}` | `audio_tracks.variant` | display form: `dubbing→Dubbing`, `lektor→Lektor`, `original→Original`, `standard→Standard`, `commentary→Commentary` |
| `{Codec}` | `audio_tracks.codec` | upper-cased (`eac3→EAC3`) |
| `{Channels}` | `audio_tracks.channels` | as stored (`5.1`, `2.0`) |

Rejected by validation, with a message naming the token: `{Language}`,
`{Lang}`, `{ISO2}`, `{Ext}`, `{Extension}`, `{Original Title}`, any unknown
`{…}`, and any modifier other than `:0…` on Season/Episode. The error text is
“`{Language}` is not available: vault files always end in `.<language>.mka`.”

### Defaults (exact strings)

| setting | default |
|---|---|
| `vault.naming.movie_template` | `{Title} ({Year}) [{Variant} {Codec} {Channels}]` |
| `vault.naming.episode_template` | `{Title} - S{Season:00}E{Episode:00} - {Episode Title} [{Variant} {Codec} {Channels}]` |
| `vault.naming.season_folder` | `Season {Season:00}` (blank = no season subfolder) |

Rendered:

```
Spirited Away (2001)/Spirited Away (2001) [Dubbing EAC3 5.1].pl.mka
The Office (2005)/Season 02/The Office - S02E03 - Office Olympics [Lektor EAC3 2.0].pl.mka
```

### Cleaning, empty groups, validation

```go
// Render produces the stem. Order is fixed so every caller agrees:
//  1. substitute tokens (empty string for unknown values, never "0"/"nil")
//  2. remove empty optional groups: (), [], {} containing only whitespace,
//     repeated until stable (handles "[]" and "[ ]" and "[  ]")
//  3. collapse runs of spaces, trim, collapse dangling " - " / "- " / " -"
//  4. CleanTitle each path component, re-join
//  5. reject empty stem, reserved names, separators, over-length
func Render(tmpl string, tr Track) (string, error)
```

`CleanTitle` / `CleanFileName` are adapted from Sonarr's
`NzbDrone.Core/Organizer/{FileNameBuilder.cs,FileNameValidationService.cs}` and
Radarr's `Organizer/FileNameBuilder.cs`:

- replace `/ \ : * ? " < > |` and control chars with a space; collapse spaces; trim dots and spaces at both ends;
- reject `.` and `..`, and Windows device names (`CON`, `PRN`, `AUX`, `NUL`, `COM1-9`, `LPT1-9`) in any case — Linux-only product (ADR-0016), but vaults get shared over SMB;
- reject a stem that already ends in `.mka` (would become `.mka.pl.mka`);
- cap the stem at 200 bytes and the full destination path at 3800 bytes; over-length is a rendered-plan error, not a silent truncation.

Adapted services, not ported code:

```go
// backend/internal/naming/naming.go (new package)
type Track struct {
    Title, EpisodeTitle, Variant, Codec, Channels, LangISO2 string
    Year, Season, Episode                                    int
    MediaFolderPath                                          string
}

type Template struct{ raw string; parts []part }

func Parse(raw string) (Template, error)         // token whitelist + modifier rules
func (t Template) Render(tr Track) (stem string, err error)
func (t Template) Sample() string                // for the settings preview line
func Validate(raw string) error                  // Parse + reserved-name/length probe
func CleanTitle(s string) string
func CleanFileName(s string) (string, error)

// VaultPath joins the media folder, optional season folder and the suffixed
// file name. It is the only function allowed to append ".<iso2>.mka".
func VaultPath(mediaFolder, seasonFolder string, tr Track, stem string) string
```

`VaultPath` is deliberately the single place that appends the suffix; the
writer (`internal/vault/pipeline.go`) keeps its own append until a follow-up
refactor, and a unit test asserts both produce the same string for the same
inputs.

### Preview / apply API

New package `backend/internal/vaultapi/organise.go`, mounted by the existing
vault API service:

| route | body | 200 | 409 codes |
|---|---|---|---|
| `POST /api/vault/organise/preview` | `{"scope":{"media_type":"movie"\|"series"\|"all","ids":[1,2]}}` | `PlanResponse` | `invalid_template`, `name_invalid`, `jobs_running` |
| `POST /api/vault/organise/apply` | `{"plan_id":"sha256…","confirm":true}` | `RunResponse` | `conflict`, `stale_plan`, `jobs_running`, `path_cycle`, `cross_device` |
| `GET /api/vault/organise/runs?limit=50` | — | `{items:[RunRow]}` | — |

`PlanResponse` is exactly what the UI table renders — every file, no pagination
(a plan is bounded by the vault's track count; 50k rows is ~10 MB and is
refused above `PlanMaxFiles = 20000`, see risks):

```json
{
  "plan_id": "sha256:…", "expires_at": "2026-11-27T20:15:00Z",
  "files": [
    {"track_id": 12, "media_id": 3, "source": "/vault/…/A.pl.mka",
     "destination": "/vault/The Office (2005)/Season 02/….pl.mka",
     "changed": true, "reason": "template_change", "conflict": null}
  ],
  "conflicts": [
    {"kind":"destination_exists","destination":"…","track_ids":[12,13]}
  ],
  "impact": {
    "renamed": 41, "unchanged": 0,
    "jobs_referencing_tracks": 7,
    "jobs_blocked": 0,
    "library_rows_affected": 0
  }
}
```

`impact.jobs_referencing_tracks` comes from a single query on
`sync_mux_jobs.target_track_id` / `ref_track_id` — those jobs reference tracks
**by id**, so a path rewrite does not break them; the number is reported so the
operator is not surprised, and `jobs_blocked` is non-zero only when apply is
refused. `library_rows_affected` is 0 by construction: `arr_library_cache`
points at target videos, never at vault audio.

`plan_id` is `sha256` over the sorted tuple list
`(track_id, source, destination, size, mtime_ns)`. The plan is cached in memory
for 10 minutes (`sync.Map`, swept on write). Apply recomputes the same tuple
list and refuses with `stale_plan` if the hash differs — this is the TOCTOU
guard, and it is why apply takes a `plan_id` rather than a file list.

### Apply protocol — filesystem two-phase with SQL compensation

```
preflight (no mutation)
  1. rebuild plan, hash must equal plan_id                        409 stale_plan
  2. no sync_mux_jobs row in ('analyzing','muxing')                409 jobs_running
  3. every source exists, is a regular file, is not a symlink      409 conflict{source_missing}
  4. every destination inside storage.paths.vault_dir              409 path_escape
     (filepath.Rel + strings.HasPrefix ".." check, after EvalSymlinks of the root)
  5. no two tracks share a destination                             409 conflict{destination_duplicate}
  6. destination not another track's source (swap/cycle)           409 path_cycle
  7. destination does not exist, or exists only as this track's own source  409 conflict{destination_exists}
  8. destination parent exists or is creatable; same device as source (EXDEV probe via stat.Dev)  409 cross_device

phase A — directories
  mkdir -p each destination parent; record created dirs (for undo only)

phase B — rename (filesystem)
  for each file, in plan order:
      os.Rename(source, destination)
      on error: undo phase B in reverse (rename destination back to source),
                remove only directories this call created and only if empty,
                return 500 {code:"apply_failed", stage:"rename", undone:true}
                — SQL untouched

phase C — SQL, ONE transaction (BEGIN IMMEDIATE)
  for each file:
      UPDATE audio_tracks SET file_path = :dest
       WHERE id = :track_id AND file_path = :source   -- guard: 0 rows aborts
      INSERT INTO vault_organise_files (run_id, track_id, source_path, destination_path, result='renamed')
  INSERT INTO vault_organise_runs (plan_hash, file_count, status='applied')
  COMMIT

compensation
  commit fails  -> phase C rolled back by SQLite; rename phase B back in reverse
                   all renames back ok   -> 500 {code:"apply_failed", stage:"sql", undone:true}
                   any rename-back fails -> RECONCILE: for each affected track, stat the
                                            destination then the source, UPDATE
                                            audio_tracks.file_path to whichever exists,
                                            mark run 'needs_attention', return 500 with
                                            {code:"manual_attention", files:[…]} and log
                                            at error level. The response is loud on
                                            purpose: the DB now reflects disk, and the
                                            operator is told which files to inspect.
```

Never deletes media. The only `os.Remove` calls allowed are `os.Remove` on
directories this request created, during undo, and only when empty. No
`os.Remove` on any file path appears in the package (grep-checkable).

A single-file failure that happens **before** phase C leaves the vault exactly
as it was; a failure **during** phase C is the only path to
`needs_attention`.

### Migration 0010 (new: audit only)

```sql
-- 0010_vault_organise_audit.sql
CREATE TABLE IF NOT EXISTS vault_organise_runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at DATETIME NOT NULL,
    finished_at DATETIME,
    status TEXT NOT NULL CHECK (status IN ('applied','rolled_back','needs_attention')),
    plan_hash TEXT NOT NULL,
    file_count INTEGER NOT NULL,
    actor TEXT,
    detail TEXT
);
CREATE TABLE IF NOT EXISTS vault_organise_files (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL REFERENCES vault_organise_runs(id) ON DELETE CASCADE,
    track_id INTEGER REFERENCES audio_tracks(id) ON DELETE SET NULL,
    source_path TEXT NOT NULL,
    destination_path TEXT NOT NULL,
    result TEXT NOT NULL CHECK (result IN ('renamed','renamed_back','failed','reconciled'))
);
CREATE INDEX IF NOT EXISTS idx_vault_organise_files_run ON vault_organise_files(run_id);
CREATE INDEX IF NOT EXISTS idx_vault_organise_files_track ON vault_organise_files(track_id);
```

No change to `audio_tracks`: `file_path` is already `UNIQUE`, which is exactly
why a swap must be refused (`path_cycle`) rather than attempted inside one
transaction.

### Settings UI (D11)

Section “Vault → Naming”, plain-language labels, one tooltip per option, presets
named, advanced collapsed.

| control | label | tooltip (what / when / cost) |
|---|---|---|
| preset select | “Naming style” | “Fills the name patterns below. *Arr style* matches what Sonarr and Radarr would produce. You can edit any pattern afterwards.” |
| preset options | “Arr style (recommended)” · “Title and year only” · “Custom” | — |
| movie pattern | “Movie file name” | “The name of the audio file, without the language suffix — Dubarr always adds `.pl.mka` (or the track's language). Tokens: {Title}, {Year}, {Variant}, {Codec}, {Channels}. Cost: none; nothing is renamed until you press Preview.” |
| episode pattern | “Episode file name” | “Same idea for TV episodes. {Season:00} and {Episode:00} pad with zeroes. Cost: none until applied.” |
| season folder (advanced) | “Season folder” | “Where episodes go inside the show's folder. Leave blank to keep them beside the show folder. Default `Season 02`. Cost: applying creates the folder.” |
| pattern preview | “Preview” (read-only line) | “A live example using your first movie or episode. It updates as you type.” |
| sample button | “Show a sample” | “Renders the pattern with example values so you can see the result before touching anything. Cost: none.” |

Buttons on the Vault page: **Preview changes** (opens the table, no writes) and
**Apply 41 renames** (disabled until a fresh preview exists; confirmation dialog
repeats the count and the destination root).

Refusal copy: `conflict` “Two files would get the same name, or a name is
already taken. Nothing was changed.” · `jobs_running` “A sync job is muxing
right now. Wait for it to finish — nothing was changed.” · `stale_plan` “The
vault changed since the preview. Preview again.” · `path_cycle` “The new names
would swap two files. Nothing was changed — adjust the pattern.” ·
`manual_attention` “The rename partly failed and could not be undone. The
library now reflects the files on disk; check these paths: …”

## File-by-file changes

| file | change |
|---|---|
| `backend/internal/store/migrations/0010_vault_organise_audit.sql` | **new** — two audit tables |
| `backend/internal/naming/naming.go` | **new** — Parse/Render/Sample/Validate/CleanTitle/CleanFileName/VaultPath |
| `backend/internal/naming/naming_test.go` | **new** — token, cleaning, suffix, length, reserved-name tests |
| `backend/internal/vaultapi/organise.go` | **new** — preview, apply, runs; plan cache; preflight; two-phase apply |
| `backend/internal/vaultapi/organise_apply_test.go` | **new** — collision, rollback, reconcile, jobs-running tests |
| `backend/internal/store/vault_organise.go` | **new** — `VaultTrackPlanRows`, `UpdateTrackPathsTx`, `InsertOrganiseRun`, `ListOrganiseRuns`, `JobsInFlight` |
| `backend/internal/settings/settings.go` | three new keys + defaults + validation (template `Validate`) |
| `backend/internal/settings/settings_test.go` | default rendering, invalid template rejection, key round-trip |
| `frontend/src/pages/Vault.tsx` | Preview/Apply actions, plan table, run history |
| `frontend/src/components/VaultNamingSettings.tsx` | **new** — presets, patterns, live preview |
| `frontend/src/lib/api.ts` | three typed calls |

## Test plan

Naming (pure, fast):
- `TestRenderMovieDefault` — the exact `Spirited Away (2001) [Dubbing EAC3 5.1]` stem.
- `TestRenderEpisodeDefault` — the exact Office stem, season/episode padded.
- `TestRenderRemovesEmptyOptionalGroups` — missing Year/Variant/Codec → no `[]`, no double spaces, no dangling ` - `.
- `TestRenderRejectsLanguageToken` — `{Language}` and `{Lang}` produce the documented message.
- `TestRenderRejectsUnknownTokenAndBadModifier`.
- `TestCleanTitleStripsForbiddenCharacters` — `A/B: C?` → `A B C`.
- `TestCleanFileNameRejectsReservedAndDotNames` — `CON`, `.`, `..`.
- `TestRenderRejectsDoubleMkaSuffix` — a pattern ending in `.mka` is refused.
- `TestRenderRejectsOverlongStem` — 201-byte stem.
- `TestVaultPathAlwaysAppendsIso2Suffix` — and matches `vault.Writer`'s output for the same inputs.
- `TestRenderUnmatchedEpisodeTitleLeavesCleanName` — episode title empty → `The Office - S02E03 [Lektor EAC3 2.0]`.
- `TestSampleRendersForEveryPreset`.

API / apply:
- `TestPreviewReturnsEverySourceToDestination` — N tracks → N rows, `changed` flags correct.
- `TestPreviewRefusesDuplicateDestination` — two variants collapsing to one name → `conflicts` populated, `plan_id` absent.
- `TestPreviewReportsJobsReferencingTracks` — job rows counted, no writes.
- `TestApplyRenamesAndRewritesPathsInOneTransaction` — files on disk moved, `audio_tracks.file_path` matches, one audit row per file, run `applied`.
- `TestApplyCollisionRefusesWholePlan` — one of 5 files collides → 409, **zero** files moved, DB unchanged (compare full `audio_tracks` snapshot).
- `TestApplyRefusesWhileJobMuxing` — job in `muxing` → 409 `jobs_running`, zero moves; and a `held` job does **not** block (coordination with #48).
- `TestApplyStalePlanAfterFileChanges` — touch one source's mtime → 409 `stale_plan`.
- `TestApplyRefusesPathCycle` — swapped names → 409 `path_cycle`, nothing moved.
- `TestApplyRollsBackRenamesWhenSQLFails` — injected store failure after renames → files back at sources, DB unchanged, run `rolled_back`.
- `TestApplyRenameFailureMidwayUndoesEarlierRenames` — chmod a destination dir read-only (non-root) → first files restored, 500 `apply_failed{undone:true}`.
- `TestApplyReconcilesWhenRollbackAlsoFails` — injected rename-back failure → `needs_attention`, DB path equals the file that actually exists, response lists the file.
- `TestApplyNeverDeletesMedia` — inode count of `.mka` files identical before/after; only empty directories created by the call may disappear.
- `TestApplyWritesAuditRunForEveryOutcome` — applied, rolled_back, needs_attention each produce a run row.
- `TestApplyRefusesPathOutsideVault` — a media folder row pointing outside `vault_dir` (seeded) → 409 `path_escape`.
- `TestOrganiseSurvivesRestart` — apply a plan, restart the process, `GET /api/vault/organise/runs` still reports it (in-memory plan cache is not state).

Frontend: `TestVaultNamingPreviewShowsSample` — typing a pattern updates the sample without a request; Apply is disabled until Preview ran.

## Acceptance criteria

1. Defaults render exactly the two example paths in this document (byte-for-byte, including `.pl.mka` and `Season 02`).
2. No code path can emit a vault filename without `.<iso2>.mka`; `{Language}` is refused by settings validation with the documented message (test name proves it).
3. A collision anywhere in the plan yields 409 and **zero** filesystem changes; verified by comparing directory listings and `audio_tracks` before/after.
4. Apply while a job is `analyzing` or `muxing` yields 409; with only `held`/`queued` jobs it proceeds.
5. Successful apply rewrites all paths and writes one audit row per file in a single transaction; `PRAGMA integrity_check` clean.
6. A forced SQL failure after renames leaves files at their original paths and the DB unchanged, or — if undo also fails — returns `manual_attention` with the affected paths listed and a `needs_attention` run row.
7. No `os.Remove` of a file exists in `internal/vaultapi`; only `os.Rename`, `os.Mkdir*`, and directory cleanup during undo.
8. `go test ./...`, `go vet`, frontend typecheck pass; `git diff --stat` shows only 0010 as a new migration.

## Migration and compatibility

- New migration **0010**, audit tables only; `audio_tracks`/`vault_media` untouched.
- Settings keys are new; absent rows fall back to the defaults, so an upgrade renames nothing until the operator runs a preview.
- Existing vault paths stay valid: nothing rewrites paths except apply, and apply is operator-initiated with a preview.
- API is additive (three new routes); no existing vault route changes.
- Downgrade: an older binary ignores the audit tables and reads whatever paths are current — safe, because apply keeps `file_path` authoritative.
- Coordination with #48: apply refuses only `analyzing`/`muxing`; a `held` job cannot be mid-mux by definition.

## Risks and mitigations

| risk | mitigation |
|---|---|
| Rename succeeds, SQL commit fails → DB points at a missing file | Reverse-rename compensation; if that fails, reconcile DB to disk and return `manual_attention` loudly |
| Cross-process mux writing while files move | `jobs_running` preflight; mux reads a **work-dir copy**, not the vault file (`ADR-0002`), so a move cannot corrupt an in-flight mux — this is why the refusal is correctness-of-paths, not data safety |
| Two operators applying concurrently | Plan cache keyed by hash + `applied` guard inside the transaction (`WHERE file_path = source`); second apply gets 0 rows and aborts/rolls back |
| Huge vault makes preview slow or huge | `PlanMaxFiles = 20000`; above it, 409 `plan_too_large` with the count and a hint to scope by media type |
| Episode title lookup misses | Token renders empty and cleanup removes the dangling separator; no invented metadata (ADR-0014) |
| SMB/Windows clients choke on reserved names | Reserved-name validation even on Linux |
| Fresh installs bypassing migrations get no audit tables | Not possible: `store.Open` always runs migrations; `schema.sql` mirrors 0001 only and is not edited |

## Deliberately not adopted from arr

Per-file rename endpoints (`PUT /api/v3/renamemovie`-style), `monitored` flags,
hardlinks/copy-on-import, recycle bin (we rename, never delete), import lists,
quality profiles, naming tokens for video (`{Quality Full}`, `{MediaInfo Video}`)
— Dubarr names audio tracks, not releases.

## Do not touch

`AGENTS.md`, `PLAN.md`, `GLOSSARY.md`, `docs/adr/**` (ADR-0022 lives in
`ADR-DRAFTS-B.md` until the orchestrator applies it), `LICENSE`, migrations
0001–0009, `backend/schema.sql` (the `.<iso2>.mka` comment at line 158 is the
invariant this plan obeys), `backend/internal/vault/pipeline.go` publish/write
path (it keeps its own suffix append this release), media file **contents**
(no re-encode, no re-hash, no rewrite — paths only).
