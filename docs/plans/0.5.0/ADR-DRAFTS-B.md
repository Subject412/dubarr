# ADR drafts for 0.5.0 — batch B (0021, 0022)

Drafts for the orchestrator to apply under `docs/adr/`. Shape follows
`0005-tag-driven-processing-eligibility.md`, `0015-vault-lifecycle-delete-and-replace.md`
and `0018-publish-by-validated-temp.md`: a prose decision, the criteria in
bold, then status and supersession notes. Nothing here is edited into
`docs/adr/**` by the slice that implements it.

---

## ADR-0021 — Approval records intent; eligibility is a projection

Approving an import answers one question: *does the operator want this dub?* It
must not answer a second question — *is the target tag-eligible right now?* —
because the second answer changes with time while the first does not. A target
that is `waiting_for_tags` today may be tagged tonight by an upstream pipeline;
a target that is eligible today may be replaced tomorrow. Coupling approval to a
momentary verdict produced a queue that silently refused work, and an operator
who had to remember to come back.

**Approval records intent.** `POST /api/queue/{id}/approve` writes the intent —
the import row moves to `extracting` and exactly one sync job is created — and
no longer refuses because of the Processing Gate's *eligibility* verdict. It
still refuses, with 409, when no pair can be formed at all: **no matched
identity, no target file, an unreadable target folder, or no donor track.**
Those are data errors; waiting does not fix them.

**Eligibility is a projection, not a state the operator sets and not a timer.**
A job whose target is not yet eligible is created in a `held` status on
`sync_mux_jobs` and is promoted to `queued` by a projection recomputed whenever
the world changes: at the end of each `arrsync` pass (scheduled and manual
`Trigger`), after a successful per-item rescan in the library, and once at boot
before the sync worker starts. The projection is idempotent and guarded by
`WHERE status = 'held'`, so concurrent passes cannot double-release. A claim-time
re-check in the worker closes the gap between release and execution: a job whose
target lost the tag is re-held instead of muxing.

**The hold lives on the sync job, never on `import_queue`.** ADR-0005's
guarantee that donors are never tag-gated survives untouched, and the queue's
status CHECK is not rebuilt. Approval transitions are unchanged from the
operator's point of view; only the sync job's initial status differs.

**Force remains the only bypass, and it is unchanged.** A force-approved job is
inserted `queued`, persists `force_ignore_tag_filter`, and is never re-held;
the existing `security.options.allow_force_tag_override` still decides whether
the override is offered.

Consequences. `sync_mux_jobs` needs one table rebuild (new `held` and
`cancelled` statuses, `force_ignore_tag_filter`, `held_reason`,
`origin_queue_id`) in a forward migration; older databases rebuild in place and
existing rows default to non-held. Rejection cancels the intent it created.
The state machine is `pending_approval -> extracting -> completed|failed` for
the import row and `held <-> queued -> analyzing -> ready_for_review -> muxing ->
completed`, with `queued|held -> cancelled` for the operator and
`analyzing|muxing -> failed`. There is no manual release endpoint: a button
would reintroduce the timing assumption this ADR removes.

Status: proposed for 0.5.0.

Supersedes: Design A's rule that approval refuses on gate eligibility. Amends
neither ADR-0005's donor rule nor ADR-0009's arr criterion — both are still
evaluated, just at projection time rather than at approval time.

Alternatives rejected: keeping the refusal (dead-ends the operator); a new
`held` status on `import_queue` (rebuilds the queue CHECK and implies donors can
be tag-gated, contradicting ADR-0005); a retry timer (re-derives the same
verdict on a schedule nobody can reason about, and fires while nothing changed);
a nullable eligibility column on the queue row (a cached verdict that goes stale
silently, which is the bug being fixed).

---

## ADR-0022 — Vault naming and reorganisation: preview, apply, never delete

The Audio Vault has one inviolable property — every file ends in
`.<iso2>.mka`, recorded in `audio_tracks.file_path` and written by
`vault.Writer` — and one cosmetic deficit: names come from extraction day, with
no season folders and no way to improve them later. Renaming a vault is
therefore a *path* operation with an in-place database consequence, not a
re-import. Doing it with `mv` plus hand-edited SQLite leaves `file_path`
pointing at nothing, and silently, because the vault is write-once and nothing
re-checks it.

**Templates cover the file stem only.** The `.<iso2>.mka` suffix is appended by
the system and is not a token. `{Language}` and every spelling of it are
rejected by validation, because a template that can disagree with the suffix
would create files the rest of the product cannot address. Tokens are
`{Title}`, `{Year}`, `{Season:00}`, `{Episode:00}`, `{Episode Title}`,
`{Variant}`, `{Codec}`, `{Channels}`; empty optional bracket groups are removed
rather than rendered as `[]`, and names pass arr-style `CleanTitle` /
`CleanFileName` plus reserved-name and length validation. Defaults produce
`Spirited Away (2001)/Spirited Away (2001) [Dubbing EAC3 5.1].pl.mka` and
`The Office (2005)/Season 02/The Office - S02E03 - Office Olympics [Lektor EAC3 2.0].pl.mka`.
Folders come from `vault_media.vault_folder_path`, which Dubarr already owns;
the only directory a rename may create is the season folder for episodes.

**Reorganisation is preview, then apply, never delete.** Preview renders every
source → destination and reports conflicts and impact, including which sync
jobs reference the moved tracks, and returns a `plan_id` hashing the exact
tuple set. Apply requires that `plan_id` and refuses (`stale_plan`) if the world
changed since the preview. Any collision — duplicate destination, destination
already occupied, a swap between two tracks — **refuses the entire plan with 409
and moves nothing**. Apply is refused while a sync job is `analyzing` or
`muxing`; `held` and `queued` jobs do not block, because muxing reads a work-dir
copy (ADR-0002) rather than the vault file.

**Apply is filesystem-two-phase with SQL compensation.** Preflight, then the
renames, then one SQLite transaction that rewrites `audio_tracks.file_path`
(guarded by `WHERE file_path = source`) and writes an audit row per file. A
rename failure renames back; a SQL failure renames back; if the rename-back
itself fails, the database is reconciled to what is actually on disk, the run is
recorded as `needs_attention`, and the API reports the affected paths loudly.
The vault never loses bytes: no re-encode, no re-hash, no copy, and no delete of
any media file — only directories created by the call itself may be removed
during undo.

Consequences. The vault becomes arr-shaped without becoming arr-managed:
metadata still comes from the arrs (ADR-0014), and only paths are rewritten.
Naming settings are inert until the operator previews, so an upgrade renames
nothing. Two audit tables record every outcome, which is the only way to answer
"what happened to this track" after a partial failure. Per-file rename endpoints
are deliberately absent: a plan is the unit of atomicity, and offering a
single-file variant would let an operator build a half-applied state one click
at a time.

Status: proposed for 0.5.0.

Supersedes: nothing. Constrained by ADR-0002 (immutable vault copies) and
ADR-0015 (lifecycle: delete and replace only through their own paths) — this ADR
adds a rename operation and explicitly excludes deletion from it. ADR-0016
(Linux-only) still motivates Windows-safe name validation, for SMB clients
rather than local filesystems.

Alternatives rejected: renaming during extraction only (leaves every existing
vault broken, and the operator's complaint is about existing files); per-file
rename API (no atomicity, no plan-level collision check); SQL first, then
rename (a failed rename leaves the DB claiming paths that do not exist, which is
the failure mode with the worst blast radius); copy-then-delete (violates
ADR-0002's immutability, costs a full vault's I/O, and can strand duplicates);
introducing a `{Language}` token for "flexibility" (breaks the suffix invariant).
