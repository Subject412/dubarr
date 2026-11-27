# 02 — #86 distinguish FPS drift from a mid-movie cut with a line fit

Branch: `feat/86-line-fit-drift` · Merge position: 2 of 8 · Depends on: 01 · Blocks: 03 (`above_drift`), 05 (segment confidence)

## Goal

Today a frame-rate mismatch and a mid-movie cut look identical: both produce a
large spread, both get `verdict: drifting`, both are refused by the worker
(`muxableVerdict`, `backend/internal/syncworker/worker.go:537-548`). This slice
fits a robust line through the checkpoint offsets, reports slope, residual, R²
and the implied tempo factor, classifies the timeline as `constant` / `drift` /
`cut` / `mixed` / `insufficient`, and — when the timeline is a clean drift —
makes the job **muxable** by folding the tempo factor into the single ffmpeg
encode that `transcode.Plan.WithTempo` (`backend/internal/transcode/rule.go:237-242`)
was written for and that no caller has ever used. The verdict vocabulary and its
precedence do not change (D2): a clean drift is still `verdict: drifting`, it
just stops being a dead end.

## Design

### Reuse, do not reinvent

`backend/internal/analyze/fallback.go` already contains a Theil–Sen fit
(`fitLine`, lines 232-252) written for the suspect rule, plus `medianOf`, `abs`
and `timelinePoint`. The comment at lines 219-227 explains why a least-squares
fit must never replace it: a single spike drags least squares so far that the
"both neighbours on the line" test can never fire. #86 needs exactly the same
estimator, for the same reason (one bad checkpoint must not define the trend).

So: **move** `timelinePoint`, `fitLine`, `medianOf`, `abs` out of `fallback.go`
into a new `backend/internal/analyze/fit.go` **without changing a line of their
bodies**, and add the fit report on top. `fallback.go` keeps `Suspects`,
`Accepts`, `residualsAgainstLine`, `withinBand`, `oneStep`, `flatMean`,
`loneSpike` and calls the moved helpers. This is a pure file move: every existing
test in `fallback_test.go` must pass unchanged.

### Types

`backend/internal/analyze/fit.go` (new):

```go
// Fit shapes.
const (
    ShapeConstant     = "constant"     // flat line: one offset for the whole film
    ShapeDrift        = "drift"        // sloped line, small residuals: an FPS mismatch
    ShapeCut          = "cut"          // one step, both sides flat: a re-cut
    ShapeMixed        = "mixed"        // neither a line nor a single step
    ShapeInsufficient = "insufficient" // fewer than three usable checkpoints
)

// LineFit is the trend through the checkpoint offsets, reported alongside the
// spread. Slope is in milliseconds of offset per second of film: a 25.000 ->
// 23.976 fps mismatch gives +42.7 ms/s (factor 1.0427).
type LineFit struct {
    SlopeMSPerSec     float64 `json:"slope_ms_per_sec"`
    InterceptMS       float64 `json:"intercept_ms"`
    ResidualRMSMS     float64 `json:"residual_rms_ms"`
    MaxResidualMS     float64 `json:"max_residual_ms"`
    R2                float64 `json:"r2"`
    Points            int     `json:"points"`
    Shape             string  `json:"shape"`
    CutAtSec          float64 `json:"cut_at_sec,omitempty"`
    CutUncertaintySec float64 `json:"cut_uncertainty_sec,omitempty"`
    TempoFactor       float64 `json:"tempo_factor,omitempty"`
    TempoApplied      bool    `json:"tempo_applied"`
}

// FitTimeline fits the robust line through (film position, offset) and
// classifies it. It uses every checkpoint whose confidence clears the table
// when at least three do, and every checkpoint otherwise, so a genuinely
// unreadable checkpoint cannot bend the trend it is supposed to be measured
// against.
func FitTimeline(windows []Window, offsetsMS, qualities, matchZ []float64, th Thresholds) LineFit

// tempoFactorFor turns a slope in ms/s into the atempo factor that removes it:
// factor = 1 + slope/1000. Returns (0,false) outside the range the single-pass
// encoder can honour (transcode.ValidateTempo: 0.5..2.0).
func tempoFactorFor(slopeMSPerSec float64) (float64, bool)
```

Classification order (first match wins), all limits from `DefaultThresholds`:

| Shape | Rule |
|---|---|
| `insufficient` | `Points < 3` (slope/intercept are still reported) |
| `drift` | `R2 >= th.FitR2Min` **and** `abs(SlopeMSPerSec) >= th.DriftSlopeFloorMSPerSec` |
| `cut` | `oneStep(offsets)` is true (the existing helper) |
| `constant` | `R2 >= th.FitR2Min` |
| `mixed` | anything else |

`TempoFactor` is set only when `Shape == drift` and `tempoFactorFor` succeeds;
`TempoApplied` is set later by the worker, never by `analyze` (the analysis does
not know whether the operator's encode path exists).

Two new `Thresholds` fields (the same table from slice 01):

```go
FitR2Min             float64 `json:"fit_r2_min"`              // 0.98
DriftSlopeFloorMSPerSec float64 `json:"drift_slope_floor_ms_per_sec"` // 1.0  (0.1 %)
AboveDriftResidualMS float64 `json:"above_drift_residual_ms"`  // 160  (4 x the stable band)
```

`AboveDriftResidualMS` is unused in this slice and exists for slice 03.

`Report` gains `Fit *LineFit json:"fit,omitempty"` and `Application *Application`.

```go
// Application records what was done to the dub copy before the mux, so the job
// row can say it in words instead of leaving the operator to infer it from an
// offset of 0.
type Application struct {
    Mode string `json:"mode"` // "copy" | "re-encoded"
    Note string `json:"note"` // "copied untouched", "re-encoded (tempo 1.0427)"
}
```

`Application.Segments` / `SegmentCount` are added by slice 05; the type is
introduced here because a tempo re-encode is the first thing that has to be
narrated.

### The applied offset when the tempo is applied

This is the part that is easy to get wrong and must be stated once:

```
 offsets
   ^
   |            /   offset(t) = intercept + slope*t
   |          /
   |        /          <- median offset (0.4.0's answer)
   |      /
   |    /
   +------------------> t (film seconds)
   intercept
```

`mkvmerge --sync 0:N` shifts the *whole* track by a constant. Once `atempo`
has removed the slope, the remaining constant shift is the offset at `t = 0`,
i.e. **the intercept** — not the median. Using the median would leave half the
drift in place (for a 90-minute film with 42.7 ms/s, about 1.9 s at the ends).

So in `syncworker.processJob`:

```go
offset := medianOffsetMS(report.OffsetsMS)          // unchanged default
if report.Fit != nil && report.Fit.TempoApplied {
    offset = int64(math.Round(report.Fit.InterceptMS))
}
```

### Where the tempo is applied

`plan.Encode()` is false when no Transcode Rule matched. Applying tempo requires
an encode, so a tempo-only plan is materialised as FLAC — lossless, so the
operator's audio is not quietly degraded by a feature they did not ask for
(ADR-0010's lossless boundary is preserved; only the *container* of the dub copy
changes).

```go
// transcode.Plan gains no field; RuleIndex == -1 is the documented marker for
// "no rule matched; this encode exists to apply a tempo".
const NoRule = -1
```

In `syncworker.processJob`, after `Execute` returns the report and before
`transcodeStaged`:

```go
if report.Fit != nil && report.Fit.TempoApplied {
    plan = plan.WithTempo(report.Fit.TempoFactor)
    if !plan.Encode() {
        plan = transcode.Plan{RuleIndex: transcode.NoRule, Codec: transcode.CodecFLAC}
        plan = plan.WithTempo(report.Fit.TempoFactor)
    }
    if ok, detail := w.transcoder.EncoderAvailable(ctx, plan.Encoder()); !ok {
        return atStage("encoder", fmt.Errorf("%w: tempo correction needs %s: %s",
            ErrEncoderUnavailable, plan.Encoder(), detail))
    }
    report.Application = &analyze.Application{
        Mode: "re-encoded",
        Note: fmt.Sprintf("re-encoded (tempo %.4f)", report.Fit.TempoFactor),
    }
}
```

(The rule-driven encoder pre-check before `Execute` stays exactly where it is —
that plan is known before analysis. Only the tempo path defers its check,
because the factor does not exist until the report does.)

`transcodeStaged`'s log line must handle `RuleIndex == NoRule` ("tempo
correction" instead of "rule 0"), and `Plan.Encode()` / `ExpectedCodec()` /
`BuildArgs` need no change: `Codec: flac`, `TempoFactor: f` already produce
`atempo=<f>` followed by the FLAC encode, in one ffmpeg invocation
(`BuildArgs`, `backend/internal/transcode/transcode.go:225-273`).

Duration verification already accounts for tempo:
`expectedDuration = durationMS / factor` (`transcode.go:194-197`).

### Muxability

```go
// muxableReport accepts what the analysis may publish: ok/stable as before, and
// a drifting report whose trend is a clean, encoder-representable drift that the
// worker has actually compensated. A cut is never muxable — it needs slice 05.
func muxableReport(report analyze.Report) bool {
    if muxableVerdict(report.Verdict) {
        return true
    }
    return strings.EqualFold(strings.TrimSpace(report.Verdict), analyze.VerdictDrifting) &&
        report.Fit != nil &&
        report.Fit.Shape == analyze.ShapeDrift &&
        report.Fit.TempoApplied
}
```

`muxableVerdict` is kept as-is (other call sites and tests use it); `processJob`
switches to `muxableReport`. If the drifted job then fails verification
(`transcode.ErrVerify`), the job fails with that error and nothing is published —
the same terminal failure a rule-driven encode already produces.

## Data flow

```
analyze.Analyze
  checkpoints (6) ──► offsets, qualities, match_z
        │
        ├── FitTimeline(windows, offsets, qualities, matchZ, DefaultThresholds)
        │      └── fitLine (Theil-Sen, moved from fallback.go)
        │            ├── slope, intercept
        │            ├── residuals → RMS, max, R²
        │            ├── shape: drift | cut | constant | mixed | insufficient
        │            └── tempoFactorFor(slope) → TempoFactor
        ├── classify(...)            (unchanged precedence, slice 01)
        └── Report{verdict, fit, thresholds, …}
        ▼
syncworker.processJob
  ├── muxableReport(report)?  no → failed "verdict" (as today)
  ├── tempo applied? → plan.WithTempo, FLAC if no rule, encoder check
  ├── offset := intercept (tempo) | median (otherwise)
  ├── transcodeStaged (one ffmpeg pass: atempo → codec)
  └── mux (mkvmerge --sync 0:<offset>)
```

## File-by-file changes

| Path | Change |
|---|---|
| `backend/internal/analyze/fit.go` | **New.** The moved helpers (`timelinePoint`, `fitLine`, `medianOf`, `abs`) verbatim, plus `LineFit`, the shape constants, `FitTimeline`, `tempoFactorFor`, and the shape-selection rules. |
| `backend/internal/analyze/fallback.go` | Delete the moved helpers and their comments; keep every rule. Add a one-line pointer to `fit.go` where `residualsAgainstLine` calls `fitLine`. |
| `backend/internal/analyze/analyze.go` | `Thresholds` += `FitR2Min`, `DriftSlopeFloorMSPerSec`, `AboveDriftResidualMS`; `Report` += `Fit *LineFit`, `Application *Application`; `Application` type; call `FitTimeline` in `Analyze` after the checkpoint loop and before `classify`; pass the fit to `classify` for slice 05's use (see that plan). |
| `backend/internal/transcode/rule.go` | `const NoRule = -1` with a doc comment; mention the tempo-only plan in `Plan.TempoFactor`'s comment. No behaviour change. |
| `backend/internal/syncworker/worker.go` | `muxableReport`; tempo branch after `Execute`; intercept-as-offset; `Application` note; `transcodeStaged` log handles `NoRule`. |
| `backend/internal/syncworker/worker_test.go` | Update the fake transcoder assertions and add the tempo tests below. |
| `frontend/src/lib/api.ts` | `SyncReport` += `fit?: SyncFit`, `application?: { mode: string; note: string }`. New `SyncFit` type mirroring `LineFit`. |
| `frontend/src/pages/Sync.tsx` | A **Trend** block in `ReportView`: shape badge, slope (`+42.7 ms/min`), R², tempo factor + whether it was applied, cut position when `shape === 'cut'`, and `application.note` on the job row when present. `Max drift` stays. |
| `frontend/src/components/CheckpointChart.tsx` | Accept an optional `fit?: SyncFit` and draw a dashed grey polyline `y = intercept + slope·slice_start_sec` over the checkpoint dots. Omit when `shape === 'insufficient'` or the fit is absent. |

## Settings

**None.** D11 governs options; a line fit has no option. The one user-visible
consequence — that a drifting job is now corrected instead of parked — is
described in the **Trend** block's tooltip, not configured:

> *Trend.* "A steady slope across the run means the dub was made at a slightly
> different frame rate. Dubarr corrects it by stretching the audio to the film's
> speed (pitch is preserved) and re-encodes the dub copy once, losslessly. A step
> instead of a slope means the film was re-cut, which this release cannot fix
> automatically."

Sitting behind `shape` means the sentence is only shown when it applies.

## Test plan

### Unit tests (new, by name)

`backend/internal/analyze/fit_test.go` (**new**):

- `TestFitTimelineConstant` — offsets `[1200,1205,1195,1202,1198,1201]` at 10..85 % of 5400 s ⇒ `Shape == constant`, `abs(SlopeMSPerSec) < 1`, `R2 >= 0.98`, `TempoFactor == 0`.
- `TestFitTimelineDriftRecoversTempoFactor` — offsets generated as `intercept + 42.7·t` ⇒ `Shape == drift`, `abs(SlopeMSPerSec − 42.7) < 0.2`, `abs(TempoFactor − 1.0427) < 2e-4`, `TempoApplied == false` (analyze never sets it).
- `TestFitTimelineCutAtTheSplit` — two flat halves differing by 2500 ms split between 40 % and 55 % ⇒ `Shape == cut`, `CutAtSec` strictly between the two neighbouring film positions, `CutUncertaintySec == (gap/2)`, `TempoFactor == 0`.
- `TestFitTimelineMixedWhenTheLineDoesNotHold` — random offsets ⇒ `Shape == mixed`.
- `TestFitTimelineInsufficientBelowThreePoints` — two points ⇒ `Shape == insufficient`, slope still finite.
- `TestFitTimelineIgnoresAnUnconfidentPoint` — five clean drift points plus one point with `quality` below the floor that sits 3 s off the line ⇒ slope is the drift's, not the outlier's, and `Points == 5`.
- `TestFitTimelineOutlierDoesNotMoveTheSlope` — one spike among six (the `fallback_test.go` fixture shape) ⇒ `abs(SlopeMSPerSec) < 1`.
- `TestTempoFactorForRejectsOutOfRange` — `+400 ms/s` ⇒ `(0,false)`; `+42.7` ⇒ `(1.0427,true)`; `−42.7` ⇒ `(0.9573,true)`.
- `TestFitLineIsUnchangedByTheMove` — the exact `(points, slope, intercept)` tuples the existing `fallback_test.go` cases exercise, asserted against `fitLine` directly (guards the move).

`backend/internal/analyze/analyze_test.go` (extend):

- `TestReportCarriesFit` — the existing `TestAnalyzeDrifting` fixture gains `report.fit.shape == drift` and a non-zero slope.
- `TestIntegrationPiecewiseDriftKeepsDriftingVerdict` — `TestIntegrationPiecewiseDrift` (line 547) must keep its `verdict == drifting` assertion: a step is not a drift, so it must **not** become muxable. Add `report.fit.shape == cut`.
- `TestReportJSONShapeCarriesFit` — `fit` present when the timeline has ≥3 points.

`backend/internal/syncworker/worker_test.go` (extend):

- `TestMuxableReport` — table: `ok` ⇒ true; `drifting` + nil fit ⇒ false; `drifting` + `shape=cut` ⇒ false; `drifting` + `shape=drift` + `TempoApplied` ⇒ true; `drifting` + `shape=drift` + `!TempoApplied` ⇒ false.
- `TestProcessOnceAppliesDriftTempoInOneEncode` — report `verdict=drifting`, `fit{shape:drift, tempo_factor:1.0427, tempo_applied:true, intercept_ms:−340}`; assert (a) exactly one `Transcode` call, (b) its `Plan.TempoFactor == 1.0427`, (c) `Plan.Codec == transcode.CodecFLAC` when no rule was configured, (d) the mux call received `SyncOffsetMS == −340`, (e) the persisted report's `application.note == "re-encoded (tempo 1.0427)"`.
- `TestProcessOnceDriftingWithoutTempoStaysUnmuxable` — `shape=cut` ⇒ `ErrUnmuxableVerdict`, zero transcode calls, zero mux calls.
- `TestProcessOnceTempoNeedsItsEncoder` — `EncoderAvailable` false ⇒ `ErrEncoderUnavailable`, positioned after `Execute` and before any transcode.

`backend/internal/transcode/rule_test.go` (extend):

- `TestTempoOnlyPlanBuildsOnePass` — `Plan{RuleIndex: NoRule, Codec: flac}.WithTempo(1.0427)` through `BuildArgs`: argv contains exactly one `atempo=1.042700`, appears before `-c:a flac`, and has exactly one `-i`.

### Fixtures

No new media. The synthetic writers in `analyze_test.go` build a linear drift by
shifting each window by `round((intercept + slope·t)·rate/1000)` — a small helper
`driftDonor(target, slopeMSPerSec, interceptMS)` added next to `piecewiseDonor`.
The expected numbers are computed in the test from the same formula, so they
cannot rot.

### Opt-in (real content)

`DUBARR_PARITY=1 go test ./internal/acceptance -run Sync` on the Shrek and
Minions pairs must still report `ok` (or the same verdict as 0.4.0) — no real
pair may flip because the fit now runs. If a real pair flips to `drift` and gets
tempo-corrected, that is a finding to report, not a test to relax.

## Acceptance criteria

1. `cd backend && go build ./cmd/dubarr && go test ./... && go vet ./...` exits 0; `cd frontend && npm run build && npm run lint && npm run test` exits 0.
2. `git diff backend/internal/analyze/fallback.go` shows deletions and one pointer comment only — every rule (`Suspects`, `Accepts`, `oneStep`, `loneSpike`, `withinBand`, `flatMean`, `residualsAgainstLine`) has an unchanged body.
3. `TestFitTimelineDriftRecoversTempoFactor` reports `1.0427 ± 0.0002` for the 25.000 → 23.976 ramp, and `TestFitTimelineCutAtTheSplit` reports `cut` with the boundary inside the known gap.
4. A job whose six offsets ramp at 42.7 ms/s ends `completed`, and `GET /api/sync/jobs/{id}` shows `report.verdict == "drifting"`, `report.fit.shape == "drift"`, `report.fit.tempo_applied == true`, `report.application.note` starting with `re-encoded (tempo`, and `calculated_offset_ms == round(fit.intercept_ms)`.
5. That same job performs exactly **one** `transcode.Transcode` call (`Transcode` invoked once is asserted by the fake) and the target's video stream hash is unchanged (existing `videoStreamHash` helper in `backend/internal/acceptance/harness_test.go`).
6. A job whose offsets step by 2.5 s at 50 % ends `failed` with `ErrUnmuxableVerdict` and `report.fit.shape == "cut"`; nothing is muxed, nothing is transcoded.
7. The Sync page renders `—` for the trend block on a 0.4.0 report (no `fit` key) and does not throw.
8. `git diff` touches none of: `backend/schema.sql`, `backend/internal/store/migrations/**`, `docs/adr/**`, `backend/internal/mux/**`.

## Migration / compatibility

- **Stored columns:** none. `fit` and `application` live in `checkpoints_json`.
- **Existing rows:** no `fit` ⇒ the trend block renders `—`; no job is
  re-judged. `application` absent ⇒ the row shows the old offset line.
- **Behaviour change, deliberate and documented:** `drifting` jobs with a clean
  drift now complete instead of failing. Jobs already `failed` stay failed; the
  operator re-runs them explicitly. This is the one place 0.5.0 changes what a
  stored verdict *does*, and it is exactly what #86 asks for. It must be in the
  release notes and in the plan README's "what changed" list at tag time.
- **`calculated_offset_ms` semantics:** unchanged in meaning ("the shift applied
  to the track"), but for a tempo-corrected job it is now the intercept. The
  report's `fit.intercept_ms` is the same number, so a reader can verify it.
- **Schema columns `detected_fps_ratio` and `speed_mode`** (`schema.sql:194-195`)
  exist, are `NOT NULL`/defaulted, and are written by nothing. Populating them
  would need a store change and a reader; out of scope for this slice, recorded
  so the next person does not assume the drift is unrecorded.

## Risks

| Risk | Mitigation |
|---|---|
| Tempo correction quietly changes audio the operator liked. | FLAC only (no lossy generation), `atempo` preserves pitch, one pass, and `application.note` says so on the job row. An operator who does not want it can still review the job before it runs, because jobs are created explicitly. |
| The tempo encode of a feature-length dub is slow. | It runs only for a clean drift, which is rare, and the cost is one pass at the source rate (FLAC at 8 ch / 48 kHz encodes far faster than real time). Documented in the tooltip's "cost" sentence. |
| Intercept-vs-median mistake leaves a residual shift. | `TestProcessOnceAppliesDriftTempoInOneEncode` asserts the mux offset equals `intercept_ms`; the plan states the derivation. |
| Theil–Sen on six points is noisy at small slopes. | The `DriftSlopeFloorMSPerSec` (1 ms/s) gate means a noisy slope below 0.1 % classifies as `constant` (still muxable as today), so noise cannot trigger an encode. The #86 arithmetic (10 ms noise over a 4050 s span ⇒ ~0.002 ms/s slope SE against a 42.7 ms/s signal) is 4 orders of magnitude of margin. |
| `oneStep` (built for 6 points) misbehaves on 160 dense points. | Slice 05 does not use `oneStep` for segmentation; it uses its own split-penalty DP. `FitTimeline` is called on dense plans only for `shape` reporting, and `ShapeCut` there is informational. |
| R² threshold 0.98 rejects a real drift because one checkpoint is bad. | `FitTimeline` drops unconfident points first; if that leaves <3 it uses all points and reports `insufficient`-adjacent numbers rather than a wrong shape. The floor is in the table, one edit away. |

## Do not touch

- `backend/internal/analyze/fallback.go`'s rules — the suspect/retry policy is
  the guard for the windowed path; this slice only relocates its arithmetic.
- `backend/internal/mux/**` — the offset is an input, not this slice's business.
- `backend/internal/transcode/transcode.go`'s `BuildArgs` precedence
  (tempo → layout → codec) and `ValidateTempo`'s 0.5..2.0 range.
- `backend/internal/transcode/rule.go`'s Match/Validate logic.
- `backend/schema.sql`, applied migrations, `docs/adr/**`.
