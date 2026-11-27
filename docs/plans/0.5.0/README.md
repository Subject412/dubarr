# 0.5.0 — implementation plans

This directory holds the executable plans for the eight issues 0.5.0 closes.
`DECISIONS.md` is the binding input (operator decisions D1–D11): **where a plan
here disagrees with it, `DECISIONS.md` wins**. Read `AGENTS.md`, `VISION.md`,
`GLOSSARY.md` and the ADRs cited below before executing any slice.

Issue #88 (chromaprint/fpcalc) is **closed as not planned** (D1). Nothing in
this directory may reintroduce it, `fpcalc`, or a second external binary.

## The eight issues

| # | Slice | Plan | One-line boundary |
|---|---|---|---|
| 85 | 0.4.1 | `01-confidence-zscore.md` | Add `match_z` + interpolated peak + a single thresholds table; `correlation_quality` and every applied offset stay byte-identical. |
| 86 | 0.4.2 | `02-line-fit-drift.md` | Fit a line through the checkpoint offsets and report slope / residual / R² / tempo factor; a clean drift becomes muxable with the tempo folded into one encode. |
| 82 | 0.4.3 | `03-technique-pipeline.md` | A `sync.strategy` settings section with an ordered technique list and per-checkpoint `when` routing; shipped list is exactly one waveform/always entry. |
| 83 | 0.4.4 | `04-mfcc-matcher.md` | A second matcher (`internal/dsp/mfcc`) behind the same correlator contract, reporting hop-quantised precision; never in the default list. |
| 84 | 0.4.5 | `05-piecewise-sync.md` | Dense sampling + a split-penalty segmenter; when >1 segment survives, the dub copy is cut/padded and re-encoded **once**; off by default. |
| 48 | 0.4.6 | `06-approve-hold.md` | Approve records intent; a new state holds an ineligible target; release is a projection of eligibility re-evaluated on every gate pass and once at boot. |
| 56 | 0.4.7 | `07-library-hierarchy.md` | Arr-shaped read-only hierarchy endpoints (series → seasons → episodes, movies flat), server-side search/paging, two tabs. |
| 47 | 0.4.8 | `08-vault-organising.md` | Arr-style vault naming with preview-then-apply, one transaction, one audit row per file, collisions refused, no deletion. |

Slices 06–08 cover issues #48, #56 and #47 and are written to the same standard
as 01–05; they are linked above rather than restated. ADR numbering continues
across the whole release: **0019** (technique pipeline) and **0020** (piecewise
re-encode) are drafted in `ADR-DRAFTS.md`, and 0021/0022 belong to slices 06 and
08. Any slice that wants to change a default another slice relies on edits the
ADR first — the authority order in `AGENTS.md` is not negotiable at merge time.

## Sequencing (why this order)

```
01 #85 ──► 02 #86 ──► 03 #82 ──► 04 #83 ──► 05 #84 ──► 06 #48 ──► 07 #56 ──► 08 #47 ──► v0.5.0
   report    report    pipeline   new         new         job         library     vault
   fields    fields    carries    matcher     strategy    state       UI          paths
             +tempo    one entry  into it     into it     machine
```

1. **#85 first, because it changes the shape of the report.** Every later slice
   adds fields to `analyze.Report` / `analyze.Checkpoint`; getting `match_z`,
   `precision_ms` and the thresholds table in first means #86, #83 and #84 extend
   a report that already knows how to describe a match, instead of each inventing
   its own confidence vocabulary.
2. **#86 second, because #82's `above_drift` routing needs a line fit.** D4's
   `when: above_drift` is defined against "the checkpoints that do not sit on the
   timeline's line" — that line only exists after #86. Doing #82 first would mean
   building a router with an unimplemented branch.
3. **#82 third, because it is the container the two new techniques plug into.**
   It lands carrying exactly one technique (waveform), which is the D4 guarantee:
   the pipeline itself must be provably output-neutral before anything new rides
   on it.
4. **#83 fourth**, a matcher behind the existing correlator seam
   (`sync.WithCorrelator`), selected by a technique entry the pipeline already
   supports. No pipeline change in this slice.
5. **#84 last of the engine slices**, because a segment list is a *plan-level*
   answer: it needs dense checkpoint planning, the fit from #86 for segment
   confidence, the per-checkpoint pipeline from #82 to run the dense plan, and
   the transcode single-pass rule from ADR-0010 to render the result.
6. **#48 after the engine**, because it adds a job state and a boot-time release
   pass in `syncworker` — the same file #84 edits. Landing it after avoids two
   conflicting rewrites of `processJob`.
7. **#56 then #47**, because #47's reorganisation UI and its "move open jobs and
   future sync sources with the files" rule want the hierarchy view that #56
   provides, and #47 rewrites vault paths — the riskiest thing in the release —
   so it goes last with the widest test surface behind it.

Each slice is one branch, one PR, merged only when green (D9). Nothing is
half-done behind a flag that pretends it exists (D1).

## Merge and tag order for 0.5.0

| Order | Branch | Merge gate |
|---|---|---|
| 1 | `feat/85-confidence-zscore` | `cd backend && go build ./cmd/dubarr && go test ./... && go vet ./...`; `cd frontend && npm run build && npm run lint && npm run test` |
| 2 | `feat/86-line-fit-drift` | same |
| 3 | `feat/82-technique-pipeline` | same, plus the parity test `TestPipelineDefaultStrategyIsOutputNeutral` |
| 4 | `feat/83-mfcc-matcher` | same, plus MFCC synthetic suite |
| 5 | `feat/84-piecewise-sync` | same, plus the generated-`.mkv` end-to-end test |
| 6 | `feat/48-approve-hold` | same |
| 7 | `feat/56-library-hierarchy` | same |
| 8 | `feat/47-vault-organising` | same, plus the vault-integrity acceptance run |
| — | tag `v0.5.0` | only after merge 8; then delete every branch, close every issue with evidence (D9) |

## Conventions every slice in this directory follows

- **The report is additive JSON.** `sync_mux_jobs.checkpoints_json` is a text
  blob (`backend/schema.sql:196`), so new report fields need **no migration**.
  No slice in 01–05 adds a column, edits `schema.sql`, or touches an applied
  migration. A 0.4.0 row decoded into the 0.5.0 `analyze.Report` reads the new
  fields as their zero values, and every consumer must treat zero as "not
  recorded" rather than as a measurement.
- **Three new migrations, in slices 06–08 only**, and they are new files —
  `0001`–`0007` are frozen and never edited: `0008_approve_hold.sql` (§06,
  `sync_mux_jobs` rebuild for the `held` status), `0009_library_hierarchy_indexes.sql`
  (§07), `0010_vault_organise_audit.sql` (§08). Slices 01–05 ship without one.
- **One thresholds table.** Numeric policy lives in `analyze.DefaultThresholds`
  (`backend/internal/analyze/analyze.go`). Verdict precedence
  `out_of_range > unreliable > drifting > ok` is unchanged in 0.5.0.
- **Nothing that shipped changes meaning.** `correlation_quality`, the six
  shipped checkpoint positions, the 40 ms stable band, the ±`max_offset_sec`
  bound and every field name already in the API stay as they are.
- **Vault immutability.** No slice writes to `VAULT_DIR`. Re-encodes read the
  Copy-on-Work artifact under `{WORK_DIR}/jobs/{id}/` and write only there
  (ADR-0002, ADR-0010). The target video is never re-encoded (ADR-0018) and the
  mkvmerge publication protocol is not modified by any slice here.
- **Settings rule (D11).** Every new option is plain language, has a tooltip
  answering *what it does / when to use it / what it costs*, belongs to a named
  preset, and any technical knob sits inside a collapsed "Advanced" disclosure.
  A field that cannot be explained in those terms is not exposed.
- **Fixtures (D10).** DSP-level tests use synthetic WAVs from a committed
  generator plus known offsets, so they run in CI with no media and no external
  tool. The end-to-end `.mkv` is generated by ffmpeg at test time. Real films in
  `testfiles/` stay behind `DUBARR_PARITY=1`.

## Definition of green

`cd backend && go build ./cmd/dubarr && go test ./... && go vet ./...` and
`cd frontend && npm run build && npm run lint && npm run test`. The windowed
parity suite (`DUBARR_PARITY=1 go test ./internal/syncjob ./internal/acceptance
-run Windowed`) is opt-in per `AGENTS.md` but **must be run locally before
merging 02, 03 and 05**, because those three change the path it guards.
