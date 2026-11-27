# Approve records intent; eligibility is a projection

Approving an import answers one question: *does the operator want this dub?* It
must not answer a second one — *is the target tag-eligible right now?* — because
the second answer changes with time and the first does not. A target that is
`waiting_for_tags` today may be tagged tonight by an upstream pipeline; one that
is eligible today may be replaced tomorrow. Coupling approval to a momentary
verdict produced a queue that silently refused work and an operator who had to
remember to come back.

## Approve records intent

`POST /api/queue/{id}/approve` moves the row to `extracting` and no longer asks
the Processing Gate anything. It keeps only the four refusals a wait cannot fix,
answered 409: **no matched identity, no target file, an unreadable target
folder, no donor track.** Those are data errors. An ineligible film therefore no
longer blocks extraction, so the expensive half of the work happens while the
pair waits for tags.

## The job is born where its target exists

The sync job row cannot be created at approve time: `sync_mux_jobs.target_track_id`
is **NOT NULL** and references the vault track, and that track is written by the
vault pipeline after the extraction finishes. The row is therefore created at its
**birth** — immediately after the vault write archived the extracted track
(`extract.BirthSyncJob`) — which is also the only place the gate is consulted: an
eligible target is born `queued`, a refused one is born `held` with the matching
reason, and the approving row's `force_ignore_tag_filter` wins over the gate.
The job is always tagged with `origin_queue_id`, which is how a later rejection
cancels exactly this intent and nothing else.

## Release is a projection, at every epoch

A held job carries its reason (`waiting_for_tags`, `missing_target`,
`target_changed`) and is promoted to `queued` by a projection recomputed whenever
the world changes: at the end of each arr sync pass (scheduled and manual), after
a successful per-item rescan, once at boot before the sync worker starts, and again
when a worker claims the job. A withdrawn approval cancels the intent it created
and records `cancelled_by_rejection` on the cancelled row. The projection is
idempotent and guarded by `WHERE status = 'held'`, so concurrent passes cannot
double-release, and every epoch passes the same eligibility function, so no two
epochs can disagree. The claim-time re-check closes the gap between release and
execution: a job whose target lost the tag is re-held instead of muxing.

There is no manual release endpoint. A button would reintroduce exactly the
timing assumption this decision removes.

## The hold lives on the sync job, never on the queue

`import_queue` keeps its status vocabulary and its CHECK constraint; only the
sync job's initial status differs. ADR-0005's guarantee that donors are never
tag-gated survives untouched, and an approval transition looks the same to the
operator. Approval is idempotent in time as well: a row whose extraction already
bred a live job answers success without re-recording intent, so a retry neither
breeds a second job nor resets the first one's progress.

## Consequences

- Migration 0008 rebuilds `sync_mux_jobs` for the new states (`held` and
  `cancelled`, plus `force_ignore_tag_filter`, `held_reason`, `origin_queue_id`),
  and existing rows upgrade in place as non-held.
- A held job is not a failure: the UI calls it **waiting**, shows the reason as a
  sentence, and it needs no second press.
- The state machine is `pending_approval → extracting → completed|failed` for the
  import row and `held ⇄ queued → analyzing → ready_for_review → muxing →
  completed`, with `queued|held → cancelled` for the operator and
  `analyzing|muxing → failed`.
- Force remains the only bypass and is unchanged: a force-approved job is
  inserted `queued`, persists `force_ignore_tag_filter`, and is never re-held;
  `security.options.allow_force_tag_override` still decides whether the override
  is offered.

Status: accepted and implemented (`internal/queue`, `internal/extract/syncjob.go`,
`internal/arrsync`, `internal/syncworker`).

Amends: Design A's rule that approval refuses on gate eligibility. Amends neither
ADR-0005's donor rule nor ADR-0009's arr criterion — both are still evaluated,
at projection time rather than at approval time.

Alternatives rejected: keeping the refusal (dead-ends the operator); a `held`
status on `import_queue` (rebuilds the queue CHECK and implies donors can be
tag-gated, contradicting ADR-0005); a retry timer (re-derives the same verdict on
a schedule nobody can reason about, and fires while nothing changed); a nullable
eligibility column on the queue row (a cached verdict that goes stale silently,
which is the bug being fixed).
