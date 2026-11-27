# 03 — #82 let the operator choose which sync techniques run, and in what order

Branch: `feat/82-technique-pipeline` · Merge position: 3 of 8 · Depends on: 01, 02 · Blocks: 04, 05

## Goal

The matcher is hardcoded: `analyze.Analyze` runs `cfg`'s engine on every
checkpoint and `classify` aggregates. Any improvement is therefore a global
behaviour change, and an operator with a difficult release has no lever. This
slice introduces a **sync strategy**: an ordered list of techniques, each with a
`when` condition (`always` / `below_confidence` / `above_drift`) and its own
acceptance thresholds, evaluated **per checkpoint** (D4). It ships with exactly
one enabled entry — `waveform` / `always` — so 0.5.0 produces byte-identical
offsets to 0.4.0 until the operator adds a technique (D4), and it carries the
seam slice 04 (MFCC) plugs into without touching the pipeline again.

## Design

### Vocabulary

A **technique** is a way to get one checkpoint's answer. A **strategy** is the
ordered list plus its presets. A **stage** (introduced by slice 05) is a
plan-level step that consumes the per-checkpoint answers; it is deliberately not
a technique, because a segment list is not an answer to "what is the offset
here?".

### Types — `backend/internal/analyze/technique.go` (new)

```go
// TechniqueID names a matcher the pipeline can run.
type TechniqueID string

const (
    // TechniqueWaveform is the FFT cross-correlation of the raw samples: the
    // only matcher 0.4.0 had, sample-accurate at the analysis rate.
    TechniqueWaveform TechniqueID = "waveform"
    // TechniqueMFCC is the mel-cepstral matcher added by issue #83: more
    // tolerant of re-encoding and broadcast processing, hop-limited precision.
    TechniqueMFCC TechniqueID = "mfcc"
)

// When decides whether a technique runs on a checkpoint at all.
type When string

const (
    // WhenAlways runs the technique on every checkpoint.
    WhenAlways When = "always"
    // WhenBelowConfidence runs it only when the best answer so far on this
    // checkpoint is weaker than MinConfidence (a second opinion).
    WhenBelowConfidence When = "below_confidence"
    // WhenAboveDrift runs it only on a checkpoint that does not sit on the
    // timeline's fitted line (see phase 2 below).
    WhenAboveDrift When = "above_drift"
)

// Technique is one entry of the strategy list.
type Technique struct {
    ID    TechniqueID `json:"id"`
    Enabled bool      `json:"enabled"`
    When  When        `json:"when"`
    // MinConfidence and MinMatchZ are this entry's acceptance gates: an answer
    // is good enough when Quality >= MinConfidence AND (MatchZ == 0 OR
    // MatchZ >= MinMatchZ). A zero MatchZ means the technique does not report
    // one, and is not treated as a failure.
    MinConfidence float64 `json:"min_confidence"`
    MinMatchZ     float64 `json:"min_match_z"`
}

// Strategy is the whole pipeline policy.
type Strategy struct {
    Preset     string      `json:"preset"`     // fast | balanced | maximum_accuracy | custom
    Techniques []Technique `json:"techniques"`
}

func DefaultStrategy() Strategy   // exactly [{ waveform, enabled, always, 0.10, 5.0 }]
func (s Strategy) Validate() error
func (s Strategy) Enabled() []Technique // enabled entries, list order
```

`DefaultStrategy` is the D4 guarantee and is asserted by name in a test.

### `Config` and `Report`

```go
type Config struct {
    // ... existing fields unchanged ...
    // Strategy is the technique pipeline. The zero value means DefaultStrategy.
    Strategy Strategy `json:"strategy"`
    // Engines maps a technique onto the seam that runs it. Nil (or a missing
    // key) means "this technique is unavailable", which Validate refuses at
    // construction time rather than discovering per checkpoint.
    Engines map[TechniqueID]Engine `json:"-"`
}

type Attempt struct {
    Technique   string  `json:"technique"`
    OffsetMS    float64 `json:"offset_ms"`
    Quality     float64 `json:"quality"`
    MatchZ      float64 `json:"match_z"`
    PrecisionMS float64 `json:"precision_ms"`
    Accepted    bool    `json:"accepted"`
    Error       string  `json:"error,omitempty"`
}

type Checkpoint struct {
    // ... slices 01/02 fields unchanged ...
    Technique string    `json:"technique"`          // the winner
    Attempts  []Attempt `json:"attempts,omitempty"` // every technique that ran here
}

type Report struct {
    // ... existing ...
    Strategy Strategy `json:"strategy"` // what ran, so a report is self-describing
}
```

`Report.PrecisionMS` (slice 01) becomes the **coarsest** precision among the
selected techniques — a report whose answers are half sample-accurate and half
hop-quantised must not advertise the better number.

### Evaluation: two phases, per checkpoint

D4 says techniques are evaluated *per checkpoint* and "a single weak checkpoint
falls back on that checkpoint only". `above_drift` cannot be evaluated in the
same pass as the rest, because "does this checkpoint sit on the line?" needs
every checkpoint's first answer. Hence two phases:

```
PHASE 1 — per checkpoint, in parallel, unchanged worker model
┌──────────────────────────────────────────────────────────────────────┐
│ for each checkpoint window w (worker-owned slot):                    │
│   best = ∅                                                           │
│   for entry in strategy.Enabled() where when != above_drift:         │
│       if entry.when == below_confidence                              │
│          and best ≠ ∅ and best.Quality >= entry.MinConfidence:       │
│             continue            # second opinion not needed          │
│       r = engines[entry.ID].Run(w)          # the sync.Job seam      │
│       record Attempt{entry, r, accepted}                             │
│       if accepted(entry, r):                                         │
│           best = r; break       # first satisfactory technique wins  │
│       if best == ∅: best = r    # keep the best-scoring fallback     │
│   if best == ∅: checkpoint fails (all techniques errored)            │
└──────────────────────────────────────────────────────────────────────┘
                              ▼
PHASE 2 — single-threaded, after the join
┌──────────────────────────────────────────────────────────────────────┐
│ fit = FitTimeline(windows, offsets, qualities, matchZ, thresholds)    │
│ for each checkpoint i with |residual_i| > th.AboveDriftResidualMS:    │
│     for entry where when == above_drift (list order):                 │
│         r = engines[entry.ID].Run(window_i)    # sequential, capped   │
│         if accepted(entry, r) and r.Quality > best_i.Quality:         │
│             best_i = r; mark Attempt.accepted (clear the old one)     │
│ fit = FitTimeline(...) again, once, on the final offsets              │
└──────────────────────────────────────────────────────────────────────┘
```

Determinism, which the report depends on:

- Phase 1 writes only its own slot (today's guarantee), so the report is
  identical for any worker count.
- Phase 2 is strictly sequential in checkpoint order, then list order, and runs
  at most once. A second run of `FitTimeline` is the only re-aggregation.
- The winner rule is "first accepted in list order"; ties between two *unaccepted*
  answers are broken by higher `Quality`, then by lower list index. Two identical
  inputs therefore always produce the same report.
- Phase 2 runs `above_drift` only on checkpoints already over the residual
  threshold, so the cost of an extra technique is bounded by the number of
  outliers (normally zero).

`above_drift` is inert in 0.5.0 when no technique is configured with it; the
branch exists because D4 names it and because it is the mechanism an operator
reaches for when a single checkpoint is stubborn.

### Failure semantics

- One technique erroring (a real engine error: decode failure, timeout) is
  recorded in `Attempt.Error` and the next technique runs.
- If **every** technique on a checkpoint errors, the job fails with the same
  deterministic "lowest checkpoint index that genuinely failed" rule
  `runCheckpoints` already implements (`analyze.go:745-763`). A technique error
  never silently becomes a zero offset.
- With the default single entry the behaviour is today's, exactly.

### `runCheckpoints` reshape

```go
// before
func runCheckpoints(ctx context.Context, engine Engine, cfg Config, slicer Slicer, plan []Window, workers int) ([]checkpointRun, error)
// after
func runCheckpoints(ctx context.Context, cfg Config, slicer Slicer, plan []Window, workers int) ([]checkpointRun, error)
func runWindow(ctx context.Context, entry Technique, eng Engine, cfg Config, slicer Slicer, w Window) (sync.Result, error)
func evaluateCheckpoint(ctx context.Context, cfg Config, slicer Slicer, w Window) (Checkpoint, error)
func applyAboveDrift(ctx context.Context, cfg Config, slicer Slicer, plan []Window, runs []checkpointRun) error
```

`checkpointRun` grows one field: `attempts []Attempt`, so the aggregation loop
copies attempts in checkpoint order after the join — nothing is appended from a
goroutine into shared state.

## Settings

New section key `sync.strategy` (D4 asks for the list in settings; the existing
`sync.defaults` card is about bounds and windows and must not grow a list
editor). It follows the `audio.options` precedent: the schema type lives in
`analyze` and `settings` aliases it.

```go
// backend/internal/settings/settings.go
const KeySyncStrategy = "sync.strategy"

// StrategyOptions is the sync.strategy section: a named type over
// analyze.Strategy, so it can carry the UnmarshalJSON that materialises the
// shipped default for a section written before 0.5.0 (an alias cannot have
// methods). Convert with analyze.Strategy(opts) at the boundary.
type StrategyOptions analyze.Strategy
```

Stored JSON (the shipped default, byte-for-byte what a fresh install writes):

```json
{
  "preset": "balanced",
  "techniques": [
    { "id": "waveform", "enabled": true, "when": "always", "min_confidence": 0.1, "min_match_z": 5.0 }
  ]
}
```

`SyncStrategy.UnmarshalJSON` materialises the whole default when the section is
absent (a legacy row), exactly as `SyncDefaults.UnmarshalJSON` does — otherwise a
pre-0.5.0 install reads `techniques: null` and the pipeline would have nothing to
run.

### Presets (the label is stored, the list is authoritative)

`preset` ∈ `fast` | `balanced` | `maximum_accuracy` | `custom`. The server never
expands a preset; the **UI** rewrites `techniques` when a preset button is
pressed, and the server forces `preset = "custom"` when the incoming list differs
from the named preset's canonical list. One rule, one direction:

| Preset | `techniques` | checkpoint budget |
|---|---|---|
| `balanced` (default) | `[{waveform, always, 0.10, 5.0}]` | 6 (unchanged) |
| `fast` | `[{waveform, always, 0.10, 5.0}]` | **3** |
| `maximum_accuracy` | `[{waveform, always, 0.10, 5.0}, {mfcc, below_confidence, 0.15, 8.0}]` | 6 (unchanged) |

**Fast measures less, not worse** (ruling D4a): it keeps the waveform matcher, so
offsets stay sample-exact, and spends fewer checkpoints. An operator who picks
"Fast" must never silently accept a ±10 ms error on clean material, so MFCC is
never a preset — it is only reachable by editing the list by hand.

Because the checkpoint count lives in `sync.defaults` (it is a bounds/window
setting, not a technique), pressing a preset button writes **both** sections:
`sync.strategy` (the list + label) and, for `fast`/`balanced`, the checkpoint
count. The tooltip says so in one sentence. The server never expands a preset —
if the incoming list differs from the named preset's canonical list it forces
`preset = "custom"`, exactly as below.

`maximum_accuracy` references `mfcc` before slice 04 registers an MFCC engine:
validation must accept the id, and a technique with no registered engine is
skipped at run time with a note in the report rather than failing the job.

### Validation (`settings.validateStrategy`, called from `normalize`)

| Rule | Error text |
|---|---|
| 1..4 entries | `sync.strategy: techniques must have 1..4 entries` |
| ids known and unique | `sync.strategy: unknown technique %q` / `duplicate technique %q` |
| at least one enabled entry | `sync.strategy: at least one technique must be enabled` |
| an enabled `when: always` entry exists and precedes every non-always entry | `sync.strategy: the first enabled technique must run always` |
| `below_confidence` has a preceding enabled entry | `sync.strategy: technique %q needs an earlier step to react to` |
| `0 <= min_confidence <= 1` | `sync.strategy: min_confidence must be 0..1` |
| `0 <= min_match_z <= 100` | `sync.strategy: min_match_z must be 0..100` |
| `preset` known | `sync.strategy: preset must be fast, balanced, maximum_accuracy or custom` |
| engine availability is a runtime concern, not a settings one | — |

The section is added to `knownKeys` and to the `Defaults()` map
(`backend/internal/settings/settings.go:65-68`, `:420-430`); `normalize` gains a
case mirroring `KeySyncDefaults`'s "decode into the current schema and
re-marshal" behaviour so an absent key is served materialised.

## File-by-file changes

| Path | Change |
|---|---|
| `backend/internal/analyze/technique.go` | **New.** `TechniqueID`, `When`, `Technique`, `Strategy`, `DefaultStrategy`, `Validate`, `Enabled`, `accepts(entry, sync.Result) bool`, `winner(attempts) (Attempt, bool)`. |
| `backend/internal/analyze/analyze.go` | `Config.Strategy` / `Config.Engines`; `Report.Strategy`; `Checkpoint.Technique` / `Attempts`; `Analyze` normalises a zero `Strategy` to `DefaultStrategy` and validates engines against it; the checkpoint loop adopts `Technique`/`Attempts` from the run slots; `PrecisionMS` = max over accepted attempts. |
| `backend/internal/analyze/pipeline.go` | **New.** `evaluateCheckpoint`, `applyAboveDrift`, the phase-2 fit call, the deterministic winner rule. Keeps `analyze.go` from doubling in size. |
| `backend/internal/analyze/analyze_test.go` | Adapt the `scriptedEngine` helpers to the engines map (a small `enginesFor(...)` helper); add the tests below. |
| `backend/internal/settings/settings.go` | `KeySyncStrategy`, `knownKeys` (line 65), `Defaults()` (line 389), `normalize` case (decode + re-marshal, like `KeySyncDefaults`), `validateStrategy`, and preset canonical lists (`PresetTechniques(preset string) []analyze.Technique`). The section type is a **named type** — `type StrategyOptions analyze.Strategy` — because an alias cannot carry the `UnmarshalJSON` that materialises the default for an absent section; it converts to/from `analyze.Strategy` at the boundary (`analyze.Strategy(o)`). |
| `backend/internal/settings/settings_test.go` | `TestSyncStrategyDefaultsAreTheBalancedList`, `TestSyncStrategyAbsentSectionMaterialises`, `TestSyncStrategyValidationTable`, `TestSyncStrategyPresetBecomesCustomWhenEdited`, `TestKnownKeysIncludesSyncStrategy`. |
| `backend/internal/syncjob/syncjob.go` | `analysisPolicy` reads `settings.KeySyncStrategy` into `cfg.Strategy` (defence-in-depth: an unknown id is dropped, an empty list falls back to `DefaultStrategy`); `runAnalysis` builds `cfg.Engines` — `waveform` = `sync.New(sync.WithTimeout(analyze.JobTimeout(cfg)), sync.WithLogger(s.logger))`, `mfcc` only from slice 04 on. |
| `backend/internal/syncjob/syncjob_test.go` | `TestAnalysisPolicyReadsStrategy`, `TestAnalysisPolicyFallsBackToDefaultOnGarbage`. |
| `frontend/src/lib/api.ts` | `SyncStrategy`, `SyncTechnique`, `SyncWhen`, `SyncPreset`; `'sync.strategy'` added to the `SettingsKey` union and to `DubarrSettings`; `SyncReport.strategy?`; `SyncCheckpoint.technique?`, `.attempts?`. |
| `frontend/src/lib/defaults.ts` | `DEFAULT_SETTINGS['sync.strategy']` = the balanced list (must match the Go default byte for byte; a comment names `settings.PresetTechniques("balanced")`). |
| `frontend/src/lib/settings-sections.ts` | New entry `{ key: 'sync.strategy', title: 'Sync strategy', description: 'Which matching techniques run, in what order, and when the next one is tried.' }` — inserted after `sync.defaults`. |
| `frontend/src/lib/settings-help.ts` | `'sync.strategy'` help block (strings below). |
| `frontend/src/components/StrategyEditor.tsx` | **New.** Preset buttons, the ordered technique list (up/down buttons, enable toggle, technique select, `when` select), a collapsed **Advanced** disclosure per row holding `min_confidence` and `min_match_z`, and an "Add a step" button. Mirrors the structure of `frontend/src/components/TranscodeRulesEditor.tsx` (same list idioms, no drag library). |
| `frontend/src/pages/Settings.tsx` | One `<Section<SyncStrategy> sectionKey="sync.strategy">` block after the `sync.defaults` card, body `<StrategyEditor draft={draft} update={update} />`. |
| `frontend/src/test/settings-strategy.test.tsx` | **New.** See the test plan. |
| `frontend/src/pages/Sync.tsx` | Checkpoint table shows the winning technique per row when more than one ran; a per-row expandable "attempts" line when `attempts.length > 1`. |

## Settings UI strings (label, tooltip, preset membership)

| Field | Label | Tooltip (what / when / cost) | In presets |
|---|---|---|---|
| `preset` | **Starting point** | "Balanced matches the previous release: waveform correlation on every checkpoint, nothing else. Fast keeps the same waveform method but spends three checkpoints instead of six, so a job finishes sooner and says less about cuts and drift — it also sets the checkpoint positions in Sync defaults. Maximum accuracy adds a second, different method when the first is unsure. You can edit the steps afterwards; the starting point then reads Custom." | all three |
| `techniques[]` | **Matching steps** | "Each step runs on every checkpoint, in order. The first step that returns a confident match decides that checkpoint. A weak checkpoint is retried by the next step — the whole job is never re-run because one point was bad." | — |
| `techniques[].id` | **Method** | "Waveform: compares the actual sound samples; sample-accurate, but fragile if the dub was re-encoded or broadcast-processed. MFCC: compares the shape of the sound instead of the samples; survives heavy compression and clipping, but is only accurate to about ±10 ms." | balanced/max/fast = waveform |
| `techniques[].when` | **When it runs** | "Always: on every checkpoint. Only when the previous step was unsure: as a second opinion, when the match so far is weaker than the confidence below. Only on checkpoints that break the trend: on the points that do not sit on the straight line through the rest — the ones that a mid-film edit or a bad decode produces." | always in all presets |
| `techniques[].enabled` | **Enabled** | "Disabled steps stay in the list but never run." | — |
| `techniques[].min_confidence` (Advanced) | **Confidence needed** | "How good a match must be for this step's answer to be accepted. 0.10 is the shipped floor: lower accepts more matches, including wrong ones; higher sends more checkpoints to review. Cost of raising it: more checkpoints fall through to the next step." | 0.10; max accuracy 0.15 |
| `techniques[].min_match_z` (Advanced) | **Peak must stand out by (σ)** | "How far the correlation peak must stand above the rest of the curve, in standard deviations. 5 is the 'a human should look at this' line, 10 is 'almost certainly correct'. Raise it to reject doubtful matches; lower it only if a difficult release keeps coming back as unreliable." | 5.0; max accuracy 8.0 |

The section description in `settings-sections.ts` is the D11 one-liner above; the
per-field "cost" clause is mandatory — the release rule is that no tooltip ships
with only a definition.

## Data flow

```
settings sync.strategy ──► syncjob.analysisPolicy ──► analyze.Config{Strategy, Engines}
                                                              │
analyze.Analyze ──► plan (6 windows)                          │
        │                                                     ▼
        └──► runCheckpoints ──► evaluateCheckpoint(entry×N) ──► engine.Run(job)
                                     │                          (waveform | mfcc)
                                     ▼
                              Attempts per checkpoint
                                     ▼
                              applyAboveDrift (phase 2, sequential)
                                     ▼
                              FitTimeline (slice 02) → refine
                                     ▼
                              classify(offsets, qualities, matchZ, …)
                                     ▼
        Report{strategy, checkpoints[technique, attempts], verdict, …}
```

## Test plan

### Unit tests (new, by name)

`backend/internal/analyze/technique_test.go` (**new**):

- `TestDefaultStrategyIsOneWaveformEntry` — `len == 1`, `ID == TechniqueWaveform`, `When == WhenAlways`, `MinConfidence == 0.10`, `MinMatchZ == 5.0`, `Preset == "balanced"`. This is the D4 contract; a change here is a release-note event.
- `TestStrategyValidateTable` — one case per rule in the validation table above, each asserting the error substring.
- `TestStrategyEnabledPreservesOrder` — a three-entry list with one disabled returns two entries in list order.
- `TestAcceptsNeedsBothGates` — `(quality 0.5, z 0)` accepted; `(0.05, 30)` refused by confidence; `(0.5, 4.9)` refused by z; `(0.5, 5.0)` accepted (inclusive).

`backend/internal/analyze/pipeline_test.go` (**new**):

- `TestPipelineDefaultStrategyIsOutputNeutral` — the same synthetic pair through (a) `Config{Strategy: DefaultStrategy(), Engines: {waveform: eng}}` and (b) an engine-only config with no strategy (the 0.4.0 path, reconstructed by calling the per-checkpoint engine loop directly): `offsets_ms`, `qualities`, `max_drift_ms`, `verdict`, `classification` are equal element-for-element. **This is the merge gate for the slice.**
- `TestPipelineFallsBackOnOneCheckpointOnly` — two scripted engines: waveform returns quality 0.05 at index 2 and 0.9 elsewhere; mfcc returns quality 0.8 at index 2. With `[waveform always, mfcc below_confidence 0.10]`: checkpoint 2 has two attempts and the mfcc offset; every other checkpoint has one attempt and the waveform offset; each engine's `calls` has length 5 and 1 respectively.
- `TestPipelineBelowConfidenceIsSkippedWhenConfident` — waveform 0.9 everywhere ⇒ mfcc is never called (`calls == 0`).
- `TestPipelineAboveDriftRunsOnOutliersOnly` — five flat offsets plus one 2 s spike with an `above_drift` entry: the entry runs exactly once, on the spike's index, and the re-fit's `MaxResidualMS` shrinks.
- `TestPipelineAllTechniquesFailIsTheLowestIndexError` — both engines error at indices 1 and 3 ⇒ the error names 1 % (the lowest index), not whichever goroutine lost.
- `TestPipelineReportRecordsStrategy` — `report.strategy.techniques` equals the config's enabled list, including thresholds.
- `TestPipelinePrecisionIsTheCoarsestSelected` — one checkpoint answered by mfcc (10 ms) among waveform answers ⇒ `report.precision_ms == 10`.

`backend/internal/syncjob/syncjob_test.go` (extend):

- `TestAnalysisPolicyReadsStrategy` — a stored `sync.strategy` with two entries reaches `cfg.Strategy` unchanged.
- `TestAnalysisPolicyFallsBackToDefaultOnGarbage` — an unparseable/unknown-id section ⇒ `DefaultStrategy()` and no error (a broken settings row must not fail every job).

`backend/internal/settings/settings_test.go` (extend):

- `TestSyncStrategyDefaultsAreTheBalancedList` — `Defaults()[KeySyncStrategy]` marshals to the documented JSON.
- `TestSyncStrategyAbsentSectionMaterialises` — a DB row `{}` (or no row) through `Get` returns the default list, not `null`.
- `TestSyncStrategyPresetBecomesCustomWhenEdited` — sending `preset: "balanced"` with an mfcc entry stores `custom`.
- `TestSyncStrategyValidationTable` — 400 on each violation via `PUT /api/settings` (`backend/internal/api/settings_test.go` already has the harness).

### Frontend tests

`frontend/src/test/settings-strategy.test.tsx` (**new**):

- renders one step, "Waveform", "Always", with the Balanced preset marked active;
- clicking **Fast** rewrites the step to MFCC and saves `preset: 'fast'`;
- **Add a step** appends a disabled-by-default mfcc entry and offers "Only when the previous step was unsure";
- opening **Advanced** reveals the two thresholds with their defaults;
- a 400 from the API renders the server's message and leaves the draft intact.

`frontend/src/test/sync.test.tsx` (extend): a report with two attempts renders the technique name per row and expands to show both attempts; a report without `attempts` renders as today.

### Fixtures

None new. The scripted engines in `analyze_test.go`
(`scriptedEngine`, `echoResult`) cover every routing case without audio; the real
engine is exercised once by `TestPipelineDefaultStrategyIsOutputNeutral` on the
synthetic pair.

## Acceptance criteria

1. `cd backend && go build ./cmd/dubarr && go test ./... && go vet ./...` exits 0; `cd frontend && npm run build && npm run lint && npm run test` exits 0.
2. `GET /api/settings` on a database created before 0.5.0 contains `sync.strategy` with exactly one `waveform`/`always` entry and `preset == "balanced"`.
3. `TestPipelineDefaultStrategyIsOutputNeutral` passes, and a job on the constant-offset fixture stores the same `offsets_ms`/`qualities`/`verdict` as the 0.4.0 code.
4. An operator who adds `{id: mfcc, when: below_confidence, min_confidence: 0.2}` sees the section save with `preset: "custom"`, and a job's report then carries `strategy.techniques` with two entries.
5. `PUT /api/settings` with an unknown technique id returns HTTP 400 whose message contains `unknown technique`.
6. A settings row whose `sync.strategy` value is the literal string `"garbage"` does not fail job creation: `GET /api/settings` still serves a section, and `Execute` runs with `DefaultStrategy()`.
7. The Settings page shows a **Sync strategy** card between *Sync defaults* and *Mux options*, with preset buttons, an ordered list, and every tooltip rendering the what/when/cost sentences above (`frontend/src/test/settings-strategy.test.tsx`).
8. No new column, no migration, no change to `backend/internal/mux/**`.

## Migration / compatibility

- **Settings:** a new key; absent ⇒ materialised default. Nothing is rewritten
  in the DB on upgrade (the store seeds defaults only for a fresh database —
  `settings.SeedDefaults` — and `normalize`-on-read covers the rest).
- **Reports:** `strategy` is new and always present from 0.5.0; old reports have
  no `strategy` and no `attempts`, which the UI renders as the old single-line
  row.
- **Offsets:** identical with the default list. This is test-gated, not promised.
- **Export/import:** the new section is a plain, non-secret value, so it travels
  in `GET /api/settings/export` (`backend/internal/api/settings.go:171`, backed by
  `store.Export`) like any other section. Import writes it through the same
  validator, so an exported file with a hand-edited technique id is rejected
  exactly as a `PUT /api/settings` would be.

## Risks

| Risk | Mitigation |
|---|---|
| The two-phase router changes results with the default list. | `TestPipelineDefaultStrategyIsOutputNeutral` plus the untouched golden suite and the opt-in windowed parity run before merge. |
| A second technique doubles analysis time on every checkpoint. | `below_confidence` only runs when the first is weak; `above_drift` only on outliers. The default list has one entry. The cost is stated in every tooltip and in the preset description. |
| Preset/custom drift between UI and server. | The server derives `custom` from the list, never trusts the label; one canonical-list function (`settings.PresetTechniques`) is the only place a preset's list exists, and the frontend default is asserted equal to it by a settings test. |
| `above_drift` re-fitting twice makes the report ambiguous. | Phase 2 is at most one pass and the final fit is the one stored; `report.fit.points` says how many checkpoints it used. |
| Engine availability is discovered mid-job. | `Analyze` validates that every enabled technique has an engine before the first checkpoint and fails with `analyze: technique %q has no engine` — a composition-root bug, loud and immediate. |

## Do not touch

- `backend/internal/dsp/**` — this slice adds no matcher (that is slice 04).
- `backend/internal/analyze/plan.go` / `window.go` — the window plan is not a
  technique concern.
- `backend/internal/analyze/fallback.go`'s suspect policy — unaffected by routing.
- The six shipped checkpoint positions and `sync.defaults`'s existing fields.
- `backend/internal/mux/**`, `schema.sql`, applied migrations, `docs/adr/**`.
