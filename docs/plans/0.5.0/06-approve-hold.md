# 06 — #48 Approval records intent; eligibility becomes a projection

Branch: `feat/48-approve-hold` · Merge position: 6 of 8 · Depends on: #46/#47-era queue and sync-job code already in `main` (no 0.5.0 slice) · Blocks: nothing (07 and 08 land on top) · ADR: 0021

## Goal

Approval must stop pretending it can decide eligibility, and must stop
*timing out* on it. Today `POST /api/queue/{id}/approve` refuses a donor whose
matched target happens to be `waiting_for_tags`, which means the operator
cannot express "yes, dub this title" until an external pipeline renames a file —
often days later, at which point nobody is watching the queue. #48 splits the
two questions: **approval records intent and is idempotent in time**;
**eligibility is a projection recomputed whenever the world changes**. A job
whose target is not yet eligible is created in a new `held` status on
`sync_mux_jobs` and is promoted to `queued` by the projection, never by a
timer. Approval still refuses when the pair is *unformable*: no matched
identity, no target file, an unreadable target folder, or no donor track. Those
are data errors, and 409 remains the honest answer.

ADR-0005 is untouched in substance: **donors are never tag-gated**. The hold
lives on the *sync job*, not on `import_queue`, so no queue CHECK constraint is
rebuilt and a donor can never be held for tags.

## Design

### Two state machines, one intent

```
import_queue (unchanged statuses, no migration):
  pending_approval ──approve──> extracting ──> completed | failed
  pending_approval ──reject───> ignored
        │
        └─ approve also inserts exactly one sync_mux_jobs row (intent)

sync_mux_jobs (0008 adds 'held' and 'cancelled'):
  held ──eligibility projection true──> queued
  queued ──claim──> analyzing ──> ready_for_review ──> muxing ──> completed
      │                  │                              │
      │                  └────────> failed <────────────┘
      └──operator cancel──> cancelled
  held ──operator cancel / queue reject──> cancelled
  analyzing|muxing ──claim-time re-check fails──> held   (no mux performed)
```

Invariants:

1. `held` is never entered from `pending_approval` refusal — approval always
   yields a row (`queued` or `held`) *or* a 409 for an unformable pair.
2. Only `queued` is claimable. The claim query must stay
   `WHERE status = 'queued'`; `held` is invisible to the worker.
3. `held -> queued` is performed **only** by `store.ReleaseHeldJobs`, guarded
   by `WHERE status = 'held'`, so two pollers racing cannot double-release.
4. A force-approved job (`force_ignore_tag_filter = 1`) is inserted `queued`
   and is never re-held by the projection — the operator already overrode the
   gate. This preserves the current force semantics exactly.
5. `cancelled` is terminal and is the only transition caused by rejection.

### Why a rebuild of `sync_mux_jobs`

SQLite cannot alter a `CHECK` constraint. Migration 0008 performs the standard
twelve-step table rebuild (create `sync_mux_jobs_new`, copy, drop, rename,
recreate the three indexes). The table is a job ledger, not a media table; it
has no inbound foreign keys other than `audio_tracks`/`arr_library_cache`
outbound references, so the copy is lossless. No applied migration (0001–0007)
is edited.

### Types and signatures

```go
// backend/internal/store/sync_jobs.go
const (
    SyncJobQueued         = "queued"
    SyncJobHeld           = "held"
    SyncJobAnalyzing      = "analyzing"
    SyncJobReadyForReview = "ready_for_review"
    SyncJobMuxing         = "muxing"
    SyncJobCompleted      = "completed"
    SyncJobFailed         = "failed"
    SyncJobCancelled      = "cancelled"
)

// HeldJob is one held row plus everything the eligibility projection needs,
// joined once so the projection performs no N+1 queries.
type HeldJob struct {
    ID                    int64
    TargetVideoPath       string
    TargetLibraryCacheID  sql.NullInt64
    ForceIgnoreTagFilter  bool
    HeldReason            string
    ArrTitle              string   // arr_library_cache.title, "" when no row
    ArrRootFolder         string   // arr_library_cache.root_folder_path
    ArrTags               []string // arr_library_cache.arr_tag_labels_json, decoded
    HasArrRow             bool
}

// HeldJobs returns every held job with its arr metadata in one query.
func (s *Store) HeldJobs(ctx context.Context) ([]HeldJob, error)

// ReleaseHeldJobs evaluates nowEligible for each held job and flips the ones
// that pass to 'queued' in one transaction. It never touches force-approved
// rows. Returns the number released. Safe to call concurrently and at boot.
func (s *Store) ReleaseHeldJobs(ctx context.Context, nowEligible func(HeldJob) bool) (int, error)

// HoldSyncJob demotes a claimed job back to held. Guards on the current status
// so a completed job can never be re-held.
func (s *Store) HoldSyncJob(ctx context.Context, id int64, reason string) (bool, error)

// CancelSyncJob cancels a job by id; only queued|held may transition.
func (s *Store) CancelSyncJob(ctx context.Context, id int64, reason string) (bool, error)

// CancelSyncJobsByOrigin cancels every non-terminal job created by a queue row.
func (s *Store) CancelSyncJobsByOrigin(ctx context.Context, queueID int64, reason string) (int, error)
```

`CreateSyncJob` grows fields on `SyncJobCreate`:

```go
type SyncJobCreate struct {
    // existing fields unchanged ...
    Status                string // "" => queued; "held" when the gate says no
    ForceIgnoreTagFilter  bool   // persisted override, not re-derived
    HeldReason            string // "waiting_for_tags" | "missing_target"
    OriginQueueID         *int64 // set by approval; nil for manual Studio jobs
}
```

Projection seam (pure, no I/O):

```go
// backend/internal/gate/rule.go
// EligiblePath answers the only question the projection asks: does this target
// pass the configured rule right now? A missing arr row fails closed exactly as
// Classify already does (ADR-0009).
func (r Rule) EligiblePath(path string, meta ArrMetadata) bool
```

Callers build the closure:

```go
// backend/internal/arrsync/poller.go — end of one pass
func (p *Poller) releaseHeld(ctx context.Context) {
    rule, err := p.gateRule(ctx)
    if err != nil { p.logf(...); return }
    n, err := p.store.ReleaseHeldJobs(ctx, func(j store.HeldJob) bool {
        return rule.EligiblePath(j.TargetVideoPath, store.ArrMetadata(j))
    })
    ...
}
```

Held-reason vocabulary is closed: `waiting_for_tags`, `missing_target`,
`cancelled_by_rejection`, `target_changed`. Never free text from an error
string — the UI translates the code.

### Flows

```
POST /api/queue/{id}/approve  {"force_ignore_tag_filter": true?}
  load candidate (must be pending_approval)                    422 otherwise
  ├ identity: arr_library_cache_id IS NULL                     -> 409 {code:"no_match"}
  ├ target:   target_video_path empty / stat ENOENT            -> 409 {code:"no_target"}
  │           EACCES / EIO / ELOOP                             -> 409 {code:"target_unreadable"}
  ├ donor:    zero export-eligible vault tracks for the match   -> 409 {code:"no_donor"}
  ├ import_queue: pending_approval -> extracting                (unchanged)
  └ INSERT sync job:
        force || rule.EligiblePath(target)  -> status 'queued'
        else                                -> status 'held', held_reason='waiting_for_tags'
```

```
projection triggers (all call the same store method):
  arrsync.run   (scheduled loop, end of pass) ─┐
  arrsync.Trigger (manual sync button)        ─┼─> ReleaseHeldJobs
  library/rescan.go after a successful rescan ─┤      held ──> queued
  cmd/dubarr boot, before syncworker.Start    ─┘
```

```
worker claim:
  UPDATE sync_mux_jobs SET status='analyzing'
   WHERE id = (SELECT id FROM sync_mux_jobs WHERE status='queued' ORDER BY created_at LIMIT 1)
     AND status='queued'
  immediately after claim:
    if !force && target file missing        -> HoldSyncJob(reason='missing_target'); return nil
    if !force && !rule.EligiblePath(...)    -> HoldSyncJob(reason='waiting_for_tags'); return nil
    else continue Execute -> Mux -> Rescan
```

The claim-time re-check closes the release-to-loss window: releasing is a
read of the world, and the world can change between release and claim. A job
that re-holds writes no output and logs at `info` (this is expected, not an
error).

### Migration 0008 (new file, applied migrations frozen)

```sql
-- 0008_approve_hold.sql
CREATE TABLE sync_mux_jobs_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    target_track_id INTEGER NOT NULL REFERENCES audio_tracks(id) ON DELETE CASCADE,
    ref_track_id INTEGER REFERENCES audio_tracks(id) ON DELETE SET NULL,
    target_library_cache_id INTEGER REFERENCES arr_library_cache(id) ON DELETE SET NULL,
    target_video_path TEXT NOT NULL,
    target_lang_iso2 TEXT NOT NULL,
    target_audio_stream_index INTEGER,
    work_dir_path TEXT,
    status TEXT NOT NULL CHECK (status IN
        ('queued','held','analyzing','ready_for_review','muxing','completed','failed','cancelled')),
    force_ignore_tag_filter INTEGER NOT NULL DEFAULT 0 CHECK (force_ignore_tag_filter IN (0,1)),
    held_reason TEXT,
    origin_queue_id INTEGER REFERENCES import_queue(id) ON DELETE SET NULL,
    sync_method TEXT CHECK (sync_method IN ('same_lang_reference','lowpass_fallback','manual')),
    calculated_offset_ms INTEGER DEFAULT 0,
    manual_offset_ms INTEGER,
    confidence_score REAL DEFAULT 0.0,
    detected_fps_ratio REAL DEFAULT 1.0,
    speed_mode TEXT NOT NULL DEFAULT 'none' CHECK (speed_mode IN ('none','preserve_pitch','pal_pitch_restore')),
    checkpoints_json TEXT DEFAULT '[]',
    track_title_in_mkv TEXT NOT NULL DEFAULT 'Polski Dubbing',
    set_as_default BOOLEAN DEFAULT 0,
    error_message TEXT,
    output_path TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
INSERT INTO sync_mux_jobs_new (/* every old column, in order */)
    SELECT /* every old column, in order */ FROM sync_mux_jobs;
DROP TABLE sync_mux_jobs;
ALTER TABLE sync_mux_jobs_new RENAME TO sync_mux_jobs;
CREATE INDEX IF NOT EXISTS idx_sync_jobs_status ON sync_mux_jobs(status, created_at);
CREATE INDEX IF NOT EXISTS idx_sync_jobs_target_track ON sync_mux_jobs(target_track_id);
CREATE INDEX IF NOT EXISTS idx_sync_jobs_target_cache ON sync_mux_jobs(target_library_cache_id);
CREATE INDEX IF NOT EXISTS idx_sync_jobs_origin_queue ON sync_mux_jobs(origin_queue_id);
```

The INSERT/SELECT lists are generated from the live schema at implementation
time (0001 + 0005 + 0006 + 0007 columns) — copy by explicit column name, never
`SELECT *`. `PRAGMA foreign_keys` must be OFF for the duration of the rebuild
and restored afterwards (the migration runner already wraps statements in a
transaction; verify `PRAGMA legacy_alter_table` is untouched).

`schema.sql` is **not** edited for this slice: the repo's convention (see the
header of `0003_sync_mux_output_path.sql`) is that `schema.sql` mirrors
`0001_initial.sql` and forward migrations are authoritative. Adding the new
column only to 0008 keeps `TestSchemaSQLMirrorsMigration` green.

### Settings and UI

No new setting. `security.options.allow_force_tag_override` still decides
whether the override is offered; it is now read at *approval* time as well as
at job creation.

D11 copy for the two controls this slice touches:

| control | label | tooltip (what / when / cost) |
|---|---|---|
| override checkbox | “Approve without waiting for tags” | “Creates the sync job now even though the target does not carry the required tags. Use it when you know the upstream pipeline is finished. Cost: the job muxes into a file that pipeline may still replace.” |
| held badge | “Waiting for tags” | “The dub is approved and queued. It starts by itself the next time the target's tags match your rule — no timer, no action needed.” |

Approval dialog body (only when the gate refuses and override is allowed):

> **Target is not ready yet.** “…/Shrek (2001) [ArcA310].mkv” does not carry
> your required tags. Approve anyway and Dubarr will hold the job until the
> file is tagged? [Approve and hold] [Approve and start now] [Cancel]

“Approve and hold” sends `{"force_ignore_tag_filter": false}`; “Approve and
start now” sends `true` and is only rendered when the setting allows it.

Error copy (409), keyed by code so the UI never parses prose:
`no_match` “This import has no matched library title.” · `no_target` “No
target file exists for this title.” · `target_unreadable` “The target folder
cannot be read.” · `no_donor` “No usable audio track was found in this donor.”

### API

| route | change |
|---|---|
| `POST /api/queue/{id}/approve` | accepts optional `{"force_ignore_tag_filter": bool}`; 409 only for the four unformable cases; 200 body gains `sync_job_id`, `sync_job_status` (`queued`/`held`) |
| `POST /api/queue/{id}/reject` | additionally cancels `sync_mux_jobs` rows with `origin_queue_id = id` and non-terminal status |
| `GET /api/sync/jobs` | response gains `held_reason`, `force_ignore_tag_filter`, `origin_queue_id`; existing fields unchanged (additive, no breaking change) |
| `GET /api/sync/jobs?status=held` | filter accepts the new value |

No manual “release” endpoint is added. Release is a projection; a button would
reintroduce the timer the design removes. Force remains the operator's
explicit, audited path.

## File-by-file changes

| file | change |
|---|---|
| `backend/internal/store/migrations/0008_approve_hold.sql` | **new** — table rebuild as above |
| `backend/internal/store/sync_jobs.go` | status constants; `HeldJob`; `HeldJobs`; `ReleaseHeldJobs`; `HoldSyncJob`; `CancelSyncJob`; `CancelSyncJobsByOrigin`; `SyncJobCreate` fields; extend `syncJobColumns`/`scanSyncJob` |
| `backend/internal/store/sync_jobs_test.go` | unit tests for each new method and the guarded transitions |
| `backend/internal/gate/rule.go` | `EligiblePath` thin wrapper over `Classify` |
| `backend/internal/gate/rule_test.go` | table test: neutral rule, arr-only fail-closed, force bypass not implemented here |
| `backend/internal/queue/queue.go` | `handleApprove` — record intent, four 409 codes, insert job with status, return `sync_job_id`; `handleReject` cancels by origin |
| `backend/internal/queue/queue_test.go` | approve/reject matrix |
| `backend/internal/arrsync/poller.go` | `releaseHeld` after `syncPass` and at the end of `Trigger` |
| `backend/internal/library/rescan.go` | release after a successful per-item rescan |
| `backend/internal/syncworker/worker.go` | `processJob`: claim-time re-check before `Execute`; `HoldSyncJob` on failure |
| `backend/cmd/dubarr/main.go` (or the wiring file that starts the worker) | one `ReleaseHeldJobs` call before `syncworker.Start` |
| `frontend` queue row + approval dialog | held badge, approve-and-hold / approve-and-start, code→copy map; wherever `Approve` is wired today |

## Test plan

Store/migration:
- `TestMigration0008RebuildsSyncMuxJobsPreservingRows` — seed rows in every old status, migrate, assert row count, ids, `output_path` preserved, new columns defaulted.
- `TestReleaseHeldJobsOnlyPromotesEligible` — three held rows (eligible, ineligible, forced) → exactly one flips.
- `TestReleaseHeldJobsIsIdempotent` — second call returns 0.
- `TestHoldSyncJobGuardsTerminalStates` — completed/failed/cancelled are not re-held.
- `TestCancelSyncJobsByOriginLeavesCompletedRows` — only non-terminal rows cancel.

HTTP/behaviour:
- `TestApproveHoldsWhenTargetWaitingForTags` — 200, import row `extracting`, job `held`, reason `waiting_for_tags`.
- `TestApproveQueuesWhenTargetEligible` — 200, job `queued`.
- `TestApproveRefusesWithoutMatchedIdentity` — 409 `no_match`, no job row.
- `TestApproveRefusesWithoutTargetFile` — 409 `no_target`.
- `TestApproveRefusesUnreadableTargetFolder` — 409 `target_unreadable` (chmod 000 dir, skipped as root).
- `TestApproveRefusesWithoutDonor` — 409 `no_donor`.
- `TestApproveWithForceOverrideQueuesImmediately` — `force_ignore_tag_filter: true`, job `queued`, column persisted.
- `TestRejectCancelsHeldIntent` — reject after approve → job `cancelled`, import row `ignored`.
- `TestPollerPassReleasesHeldJobWhenTagAppears` — rename target to carry the tag, run one pass, job becomes `queued` with no worker involvement.
- `TestManualTriggerReleasesHeldJob` — same via `Poller.Trigger`.
- `TestRescanReleasesHeldJobForItem` — rescan of the matched item promotes the job.
- `TestBootReleasesHeldJobsBeforeWorkerStart` — held job whose target is already tagged at startup is `queued` before the worker's first tick.
- `TestProcessJobReholdsWhenTargetLostTag` — release, then strip the tag, then claim: job ends `held`, no `muxing`, no output file, nil error.
- `TestHeldIntentSurvivesRestart` — approve (held), close the store, reopen (migrations re-run as no-ops), assert the row is still `held`, then release with a tag-carrying target and assert `queued`. Pins that the hold is state, not an in-memory timer.
- `TestStaffacSyncJobsResponseIncludesHeldReason` — additive JSON fields present, old fields byte-identical.

## Acceptance criteria

1. Approving a candidate whose target lacks the configured tags returns 200 with `sync_job_status: "held"`; the import row is `extracting`; exactly one `sync_mux_jobs` row exists.
2. The four unformable cases return 409 with the documented codes and create **no** `sync_mux_jobs` row and leave the import row `pending_approval`.
3. With `allow_force_tag_override=false`, the override field is ignored (job held) and force-approval returns `403`/code `force_not_allowed` — one of the two, chosen at implementation, documented in the test name.
4. A held job becomes `queued` within one poller pass after the tag appears, with no restart and no operator action; `ReleaseHeldJobs` is the only writer of that transition (grep-provable: no other `SET status='queued'` on that table).
5. A job released moments before the tag disappears does not mux: after claim it is `held` again, `output_path` is NULL, and no file under the vault changed.
6. `held` and `cancelled` exist in the 0008 CHECK; no migration 0001–0007 file is modified (`git diff --stat` shows only 0008 as new).
7. `import_queue`'s status CHECK is unchanged; a donor is never refused for tags.
8. `go test ./...` passes, including the restart test; `go vet` clean.

## Migration and compatibility

- **New migration only**: 0008. Fresh databases run 0001→0008 at `store.Open`; existing databases rebuild in place. Estimated rebuild cost is trivial (jobs table, not tracks).
- API responses are additive: new fields on sync jobs, extra keys in approve. No renaming, no removal.
- Old jobs default `force_ignore_tag_filter = 0`, `held_reason = NULL`, `origin_queue_id = NULL`; none become `held`.
- A downgrade (binary N-1 against a 0008 database) is unsupported, as with every forward migration; document it in the release notes rather than writing a down migration.
- Coordination with #47: the naming apply preflight refuses while a job is `analyzing`/`muxing`; it must also treat `held` as harmless (a held job has no work dir). No coupling to this migration.
- Coordination with #56: library resources never expose sync-job status; no shared columns.

## Risks and mitigations

| risk | mitigation |
|---|---|
| Table rebuild loses a column added by a later migration | Copy by explicit column list generated from `PRAGMA table_info`; test seeds all columns and compares |
| Double release from poller + rescan racing | `WHERE status='held'` guard; release is idempotent; test asserts count |
| Re-hold loop: job flips held→queued→held forever because tag flaps | Claim re-check is one-way per attempt (a re-held job waits for the *next* projection); log once per job per reason, not per pass |
| Approve now creates a job for a title the operator later rejects | reject cancels by `origin_queue_id`; terminal rows untouched |
| Held jobs invisible to the operator | `GET /api/sync/jobs?status=held` + badge; the queue row shows “approved, waiting for tags” |
| `arrsync` disabled → nothing releases | Boot release + rescan release still work; document that with no arr instance the filename-tag rule is the only releaser and the library refresh button also counts as a pass |
| Force column trusted after a downgrade | CHECK `IN (0,1)`, default 0 |

## Deliberately not adopted from arr

Monitored/unmonitored flags, quality profiles, import lists, hardlinks,
recycle bin. Dubarr's gate stays a filename/arr-tag policy (ADR-0005/0009); a
`held` state is not a monitoring feature and must not grow one.

## Do not touch

`AGENTS.md`, `PLAN.md`, `GLOSSARY.md` (the orchestrator widens Match State
`ready` to “no gate verdict blocks approval” there), `docs/adr/**` (ADR-0021 is
drafted in `ADR-DRAFTS-B.md`, not applied by this slice), `LICENSE`, migrations
0001–0007, `backend/schema.sql`, `internal/vault/pipeline.go`'s `.{iso2}.mka`
suffix rule, ADR-0005's donor wording.
