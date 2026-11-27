# 01 — #85 confidence: a z-score and an interpolated peak

Branch: `feat/85-confidence-zscore` · Merge position: 1 of 8 · Depends on: nothing · Blocks: 02, 03, 04, 05

## Goal

`correlation_quality` — the peak's prominence over the median lag response,
`(peak − median)/(1 − median)` at `backend/internal/dsp/correlate/correlate.go:358-369`
— is an unvalidated formula, and the reported offset is the integer sample of the
global maximum. This slice adds a second, scale-free confidence number
(`match_z`, the peak's distance from the window's own lag distribution in
standard deviations), adds a parabolic peak interpolation reported as
`offset_sub_ms`, states the measurement precision explicitly
(`precision_ms`), and moves every verdict threshold into one table
(`analyze.DefaultThresholds`) that `classify` reads. `correlation_quality`
keeps its exact formula and keeps being stored (D2). Nothing that is `ok` in a
0.4.0 library may become `unreliable` because of this slice, and every applied
offset stays byte-identical to 0.4.0 (D2 + D4).

### What is deliberately *not* changed

The applied `offset_ms` remains `round(OffsetSamples · 1000 / rate)`. The
interpolated value is reported and never applied: the analysis runs at 8 kHz, so
one sample is 0.125 ms, and the consumer is `mkvmerge --sync 0:<integer ms>`
(`backend/internal/mux/mux.go:322`). A sub-sample number that no downstream
stage can express is diagnostics, not a sync decision. It is therefore reported
and *not* exposed as a setting.

## Design

### `match_z`

The lag scan already visits every scored lag and appends its normalised
cross-correlation to `scores` (`correlate.go`, the loop around lines 227-290).
Accumulate two extra `float64` sums inside that loop — no second pass, no second
allocation, so the `TestCorrelationCostRatio` gate and the 400 MiB/worker memory
model documented in `analyze.go` are untouched:

```
mean = sum / n
var  = (sumSq - sum*sum/n) / (n-1)
std  = sqrt(max(var, 0))
z    = (peak - mean) / std        // 0 when n < 2 or std < 1e-9
```

This is the BBC `audio-offset-finder` formula cited by #85: `(peak − mean)/std`.
It is scale-free, so it is comparable between a quiet dub and a loud one, which
prominence is not.

**Arbitration (D2 speaks of "robust z / MAD-based").** MAD needs a second pass
over the same 2.9 M values *and* a second `selectOrderStatistic`, i.e. a ~23 MB
allocation and two extra O(n) passes per checkpoint. The cost gate
(`backend/internal/dsp/correlate/perf_test.go`) compares the correlator against a
reference that already contains one median extraction; doubling that work risks
tripping it for a robustness gain that the mean/std form does not need on
real content (BBC ships mean/std). The estimator therefore lives in one function,
`matchZ`, so switching to MAD is a one-function change if the operator asks for
it. Recorded in `open questions`.

### Interpolated peak

Parabolic fit through `(i−1, i, i+1)` of the NCC, in lag units:

```
curvature = left - 2*centre + right
delta     = 0.5*(left - right)/curvature        // only when curvature < 0
delta     = clamp(delta, -0.5, +0.5)
offsetSub = float64(bestLag) + delta
```

The neighbours are captured **during** the scan and stored in two local
variables, not looked up afterwards: `scores` holds only the lags that survived
the overlap and `denom > 1e-12` tests, so it is not index-aligned with lag.
Capture rule: when a new best is found at loop index `i`, remember
`(i−1, ncc)` if the previously scored lag was `i−1`, else mark the left
neighbour invalid; when the scan later reaches `i+1`, store that ncc as the right
neighbour. A missing neighbour (`NaN`) means `delta = 0`.

`delta` must be computed **before** `peakProminence` reorders `scores` in place —
it does not read `scores`, but keeping the order explicit stops a future reader
from "optimising" it into a lookup.

### Types

`backend/internal/dsp/correlate/correlate.go`:

```go
// Analysis is one correlation's answer.
type Analysis struct {
    OffsetSamples    int64   // unchanged: the integer lag of the global max
    Quality          float32 // unchanged: peakProminence
    OffsetSubSamples float64 // NEW: offsetSamples + parabolic delta
    MatchZ           float32 // NEW: (peak - mean)/std over the lag curve
    PrecisionMS      float64 // NEW: 1000/sampleRate, one sample of the analysis rate
}

type result struct {
    offset     int64
    peak       float64
    quality    float32
    subSamples float64 // NEW
    matchZ     float64 // NEW
}
```

New file `backend/internal/dsp/correlate/score.go`:

```go
// parabolicPeak returns the fractional lag offset of the peak of the parabola
// through (left, centre, right), or 0 when a neighbour is missing (NaN), the
// curvature is non-negative, or the result is not finite. The result is clamped
// to [-0.5, 0.5]: the peak cannot be nearer to a neighbour than that neighbour.
func parabolicPeak(left, centre, right float64) float64

// matchZ scores the peak against the whole lag distribution:
// (peak - mean)/std, with `n`, `sum` and `sumSq` accumulated by the scan.
// Returns 0 for n < 2 or a standard deviation below 1e-9 (a flat curve carries
// no evidence either way, and 0 is the documented "no z" sentinel).
func matchZ(peak float64, n int, sum, sumSq float64) float64
```

`backend/internal/sync/types.go` — `Result` gains three fields, all after the
existing ones so the JSON key order of the existing object is unchanged:

```go
type Result struct {
    OffsetMS           float64  `json:"offset_ms"`
    CorrelationQuality float64  `json:"correlation_quality"`
    SampleRate         uint32   `json:"sample_rate"`
    Analyzed           Analyzed `json:"analyzed"`
    OffsetSubMS        float64  `json:"offset_sub_ms,omitempty"` // NEW
    MatchZ             float64  `json:"match_z,omitempty"`       // NEW
    PrecisionMS        float64  `json:"precision_ms,omitempty"`  // NEW
}
```

`backend/internal/analyze/analyze.go`:

```go
// Thresholds is the one table every verdict limit comes from. Re-tuning the
// engine is an edit here, not a hunt through classify.
type Thresholds struct {
    StableSpreadMS float64 `json:"stable_spread_ms"` // 40   — constant offset band
    LowQuality     float64 `json:"low_quality"`      // 0.10 — the 0.4.0 floor
    MatchZMaybe    float64 `json:"match_z_maybe"`    // 5.0  — "needs a human look"
    MatchZGood     float64 `json:"match_z_good"`     // 10.0 — "likely correct"
}

var DefaultThresholds = Thresholds{
    StableSpreadMS: StableSpreadThresholdMS, // 40.0, existing const
    LowQuality:     LowQualityThreshold,     // 0.10, existing const
    MatchZMaybe:    5.0,
    MatchZGood:     10.0,
}

type Checkpoint struct {
    PositionPct   int     `json:"position_pct"`
    SliceStartSec float64 `json:"slice_start_sec"`
    SliceLenSec   float64 `json:"slice_len_sec"`
    OffsetMS      float64 `json:"offset_ms"`
    Quality       float64 `json:"correlation_quality"`
    OffsetSubMS   float64 `json:"offset_sub_ms"` // NEW — 0 on a report written before 0.5.0
    MatchZ        float64 `json:"match_z"`       // NEW — 0 means "not recorded"
    PrecisionMS   float64 `json:"precision_ms"`  // NEW — 0.125 at 8 kHz
}

type Report struct {
    // ... existing fields unchanged ...
    MatchZ     []float64  `json:"match_z"`     // NEW — checkpoint-ordered, like Qualities
    PrecisionMS float64   `json:"precision_ms"` // NEW — coarsest precision among checkpoints
    Thresholds Thresholds `json:"thresholds"`  // NEW — the table that judged this report
}
```

Both new arrays are **not** `omitempty`: a new report always carries them, so a
consumer can tell "0.5.0, z = 0" from "0.4.0, no z at all" by
`len(report.match_z) == len(report.offsets_ms)`. The per-checkpoint `match_z` is
not `omitempty` for the same reason: `0` is the documented sentinel.

### Verdict gate (D2: nothing that shipped reclassifies)

`classify` keeps its precedence `out_of_range > unreliable > drifting > ok` and
gains the match-z slice plus the table:

```go
func classify(offsets, qualities, matchZ []float64, maxOffsetMS float64, th Thresholds) (
    verdict, classification string, maxDrift float64)
```

The unreliable rule becomes a **conjunction**:

```
unreliable at i  ⇔  qualities[i] < th.LowQuality
                    && ( no z recorded at i  ||  matchZ[i] < th.MatchZMaybe )
```

`no z recorded` means `len(matchZ) <= i` or `matchZ[i] == 0`.

Why conjunction and not replacement: D2 requires both that the verdicts "start
using" the new number *and* that a 0.4.0 `ok` item cannot silently become
`unreliable`. A checkpoint that 0.4.0 called fine has `quality ≥ 0.10`; under the
conjunction it can never be reclassified, because the old gate already fails
first. The new number can only confirm distrust the old number already had, so
the release adds a veto without inventing one. The pure-z rule that would
reclassify 0.4.0 rows is exactly what D2 forbids, and is not implemented.

The out-of-range and drifting rules are untouched.

## Data flow

```
correlate.correlate
  ├── lag scan ──► ncc ──► scores[]  (unchanged)
  │                   ├── sum, sumSq          (new, float64, same loop)
  │                   └── neighbour capture   (new, two locals)
  ├── matchZ(peak, n, sum, sumSq)             (new)
  ├── parabolicPeak(left, peak, right)        (new)
  └── peakProminence(peak, scores)            (unchanged, still reorders in place)
        ▼
Analysis{OffsetSamples, Quality, OffsetSubSamples, MatchZ, PrecisionMS}
        ▼
sync.Runner.Run ──► Result{OffsetMS, CorrelationQuality, OffsetSubMS, MatchZ, PrecisionMS, …}
        ▼
analyze.runWindow ──► Checkpoint{OffsetMS, OffsetSubMS, Quality, MatchZ, PrecisionMS}
        ▼
classify(offsets, qualities, matchZ, maxOffsetMS, DefaultThresholds)
        ▼
Report{verdict, classification, max_drift_ms, match_z[], precision_ms, thresholds}
        ▼
sync_mux_jobs.checkpoints_json (unchanged column, new keys)
        ▼
GET /api/sync/jobs/{id} ──► Sync.tsx (new "Match z" column, precision badge)
```

## File-by-file changes

| Path | Change |
|---|---|
| `backend/internal/dsp/correlate/correlate.go` | Extend `Analysis` and `result`; accumulate `sum`/`sumSq` and capture peak neighbours in the lag scan (the loop around lines 227-290); fill the new `result` fields; set `PrecisionMS` from `sampleRate` in both `Analyze` (line 122) and `(*Scratch).Analyze` (line 104). `prepare`, the lag scan bounds, the overlap rule and `peakProminence` are untouched. |
| `backend/internal/dsp/correlate/score.go` | **New.** `parabolicPeak`, `matchZ`, package doc for the two. |
| `backend/internal/dsp/correlate/doc.go` | Add a "Confidence: two numbers, one verdict" section: prominence stays the stored 0.4.0 number, `match_z` is the comparable one, `offset_sub_ms` is diagnostics, and why the mean/std form (D2 arbitration). |
| `backend/internal/sync/types.go` | Three additive `Result` fields. |
| `backend/internal/sync/runner.go` | Copy `OffsetSubSamples → OffsetSubMS`, `MatchZ`, `PrecisionMS` into the `Result` (lines 193-201), each through `roundMillis` (z is O(10), three decimals is plenty). |
| `backend/internal/analyze/analyze.go` | `Thresholds` + `DefaultThresholds`; new `Checkpoint`/`Report` fields; `MatchZ []float64` slot in `Analyze`'s per-checkpoint assembly; `classify` signature and the conjunctive unreliable rule; keep `StableSpreadThresholdMS` / `LowQualityThreshold` as the table's values (they are referenced by `fallback.go` and tests). |
| `backend/internal/analyze/fallback.go` | No logic change. `withinBand`, `flatMean`, `loneSpike` keep using `StableSpreadThresholdMS`; add a comment that the constant is now an alias of `DefaultThresholds.StableSpreadMS`. |
| `backend/internal/analyze/analyze_test.go` | Update the direct `classify`/`Report` literals for the new signature and fields; add the tests below. |
| `frontend/src/lib/api.ts` | `SyncCheckpoint` += `offset_sub_ms: number; match_z: number; precision_ms: number`. `SyncReport` += `match_z: number[]; precision_ms: number; thresholds?: { stable_spread_ms: number; low_quality: number; match_z_maybe: number; match_z_good: number }`. All optional-tolerant at the type level (`number | undefined` on the report's `thresholds`) because 0.4.0 rows exist. |
| `frontend/src/pages/Sync.tsx` | Checkpoint table gains a **Match z** column (`cp.match_z ? cp.match_z.toFixed(1) : '—'`) and the header gains a precision badge (`±{report.precision_ms} ms`). No layout rewrite. |
| `frontend/src/components/CheckpointChart.tsx` | Tooltip string gains `z <match_z>`; no geometry change (a fitted line arrives in slice 02). |

No settings, no migration, no API route change: `Report` is embedded in
`syncjob.JobInfo.Report` (`backend/internal/syncjob/syncjob.go:196`), so the new
keys travel automatically.

## Settings

**None.** D11 asks for a tooltip on every *option*; this slice adds no option.
The two z floors are engine policy in one Go table (`DefaultThresholds`) and are
echoed in each report as `thresholds`, so an operator reading an old job can see
which table judged it. Exposing them would create a second source of truth for a
number that must be re-tuned from evidence, not from a form.

## Test plan

### Unit tests (new, by name)

`backend/internal/dsp/correlate/score_test.go` (**new file**):

- `TestParabolicPeakRecoversHalfSample` — `left=0.4, centre=1.0, right=0.4` ⇒ `delta == 0`; `left=0.4, centre=1.0, right=0.7` ⇒ `delta` in `(0, 0.5)` and `centre + curvature·δ² < centre` (the parabola actually peaks there).
- `TestParabolicPeakRejectsMissingNeighbour` — `NaN` on either side, or a positive curvature, returns exactly `0`.
- `TestParabolicPeakIsClamped` — a pathological `(left, right)` outside the bracket cannot return `|δ| > 0.5`.
- `TestMatchZOnCleanVersusNoise` — same synthetic pair twice: a clean shifted copy must give `z > DefaultThresholds.MatchZGood`, an unrelated noise pair `z < MatchZMaybe` (the two bands #85 names).
- `TestMatchZIsZeroOnAFlatCurve` — every score equal ⇒ `n≥2` but `std == 0` ⇒ `0`, not `Inf`/`NaN`.
- `TestMatchZNeedsTwoScores` — `n = 1` ⇒ `0`.

`backend/internal/dsp/correlate/correlate_test.go` (extend):

- `TestInterpolatedOffsetMovesOffTheSampleGrid` — a synthetic pair shifted by a whole number of samples plus a deliberately asymmetric spectrum; assert `OffsetSamples` is unchanged from 0.4.0's answer **and** `OffsetSubSamples != float64(OffsetSamples)` while `|OffsetSubSamples − OffsetSamples| <= 0.5`.
- `TestAnalysisPrecisionIsTheAnalysisSample` — at 8000 Hz, `PrecisionMS == 0.125`; at 1000 Hz, `1.0`.
- `TestScratchReuseKeepsNumbersIdentical` (existing) must keep passing with the new fields included in its comparison.

`backend/internal/dsp/correlate/golden_test.go` (extend, **no golden regeneration**):

- `TestGoldenExcerpts` stays exactly as it is: offsets equal, quality within ±0.02. This is the 0.4.0 parity guard for this slice.
- `TestGoldenExcerptsCarryAPlausibleMatchZ` — every committed golden entry reports `MatchZ > 0` and finite; the entries whose recorded `correlation_quality` is ≥ 0.5 (the clean excerpt set) report `MatchZ ≥ DefaultThresholds.MatchZGood`. This guards the accumulator against a sign or overflow error without pinning numbers that the float32 sums may move.

`backend/internal/sync/runner_test.go` (extend):

- `TestResultCarriesMatchZAndPrecision` — with the stubbed correlator returning `Analysis{OffsetSamples: 2000, Quality: 0.9, OffsetSubSamples: 2000.3, MatchZ: 27.5, PrecisionMS: 0.125}` at 8000 Hz: `OffsetMS == 250`, `CorrelationQuality == 0.9`, `OffsetSubMS == 250.038` (3 dp), `MatchZ == 27.5`, `PrecisionMS == 0.125`.

`backend/internal/analyze/analyze_test.go` (extend):

- `TestVerdictUnreliableNeedsBothScores` — qualities `[0.9, 0.05, 0.9]`, z `[30, 3, 30]` ⇒ `unreliable`; the same qualities with z `[30, 12, 30]` ⇒ `ok` (old gate fails first, new one dissents, the conjunction keeps it `ok`).
- `TestVerdictUnreliableOnALegacyReport` — a report whose `match_z` slice is absent (`nil`) and whose quality is `0.05` ⇒ `unreliable`, exactly as 0.4.0.
- `TestVerdictCannotReclassifyA04OKCheckpoint` — a table of qualities `0.10, 0.11, 0.5` each paired with z `0, 1, 4.9`: every verdict is `ok`/`drifting` as the spread dictates, never `unreliable`.
- `TestThresholdsTableIsTheOnlySource` — a `classify` call with a hand-built `Thresholds{StableSpreadMS: 1e9}` reports `ok` for a 5 s spread; with `LowQuality: 0` it never reports `unreliable`. Proves no literal survives in the function.
- `TestReportJSONShapeCarriesMatchZ` — extends the existing `TestReportJSONShape`: `match_z`, `precision_ms` and `thresholds` are present in the marshalled object, and `offsets_ms`/`qualities`/`max_drift_ms`/`classification`/`sync_method`/`verdict` still are.
- `TestIntegrationConstantOffsetPrecision` — the existing `TestIntegrationConstantOffset` pair through the real engine: `OffsetMS` is bit-identical to the value the test asserts today, and `PrecisionMS == 0.125`.

### Fixtures

No new media. The synthetic WAV writers already in
`backend/internal/analyze/analyze_test.go` (`noiseSignal`, `shiftedDonor`,
`writeWAV`) cover every case at 1 kHz, and the committed excerpts under
`backend/internal/dsp/testdata/wav` cover the real-engine path. **Golden files
are not regenerated** — regenerating them would erase the parity evidence this
slice exists to preserve.

### Frontend

`frontend/src/test/sync.test.tsx` (extend): a report fixture carrying `match_z`
renders the column; a 0.4.0-shaped fixture (no `match_z`, no `precision_ms`)
renders `—` and no precision badge, and does not throw.

## Acceptance criteria

A reviewer can check each of these without judgement:

1. `cd backend && go build ./cmd/dubarr && go test ./... && go vet ./...` exits 0.
2. `cd frontend && npm run build && npm run lint && npm run test` exits 0.
3. `git diff --stat` for the branch does **not** touch `backend/schema.sql`,
   `backend/internal/store/migrations/**`, `docs/adr/**`, `AGENTS.md`,
   `PLAN.md`, `GLOSSARY.md`.
4. `git diff backend/internal/dsp/testdata/golden` is empty (no golden changed).
5. `go test ./internal/dsp/correlate -run TestGoldenExcerpts -v` passes with the
   committed tolerances (offset exact, quality ±0.02).
6. A `POST /api/sync/jobs` + worker run on the synthetic constant-offset fixture
   stores a report whose `offsets_ms` array equals the array the 0.4.0 code
   stores for the same fixture, and whose `verdict` is unchanged.
7. `GET /api/sync/jobs/{id}` for a job created *before* the upgrade (a row with
   the 0.4.0 JSON) returns HTTP 200 and `report.match_z` as `null` or absent —
   never an error, never a fabricated 0-length array mismatch.
8. For a pair with a known offset of exactly 250 ms, the stored report has
   `offset_ms == 250` and `precision_ms == 0.125`.
9. `match_z` is present in every new report and is `> 10` for the clean excerpt
   suite, `< 5` for the unrelated-noise pair (the two unit tests above).

## Migration / compatibility

- **Stored columns:** none added, none changed. `checkpoints_json` gains keys.
- **Existing rows:** decode into the new struct with `match_z == 0`,
  `precision_ms == 0`, `thresholds == zero value`. Every consumer treats zero as
  "not recorded". The verdict of an old row is not recomputed — verdicts are
  stored, not derived, at read time.
- **API shape:** `Report` is additive. `syncjob.JobInfo.Report` and
  `mux.Job.Report` need no change. Field *order* inside `Report` matters to
  nobody (JSON object), but new fields are appended after the existing ones so a
  textual diff of two reports reads cleanly.
- **Frontend:** the new columns must tolerate `undefined` (old rows) — hence the
  `—` rendering path is part of acceptance.

## Risks

| Risk | Mitigation |
|---|---|
| The extra per-lag accumulation slows the correlator past the cost gate. | Two `float64` adds per lag on a loop that already divides and squares. `TestCorrelationCostRatio` is the referee; if it moves, the fix is to accumulate only every 4th lag — decided by measurement, not by feel. |
| `match_z` is not comparable with the MFCC matcher's z (slice 04). | Both are `(peak − mean)/std` over their own lag curve, by contract. `TestMatchZOnCleanVersusNoise` and slice 04's `TestMFCCMatchZUsesTheSameScale` pin the shared bands (5 / 10). |
| The conjunctive gate makes the new number decorative. | It cannot: a checkpoint with `quality ≥ 0.10` but `z < 5` is accepted, and the *next* release can flip the conjunction to a disjunction once 0.5.0 reports exist as evidence. The table is the single edit point. |
| `scores` reordering breaks a later reader that assumes lag alignment. | The neighbour capture never reads `scores`; `score.go`'s doc states that `scores` is unordered after `peakProminence` and must not be indexed by lag. |
| Float32 `MatchZ` loses resolution at large z. | z is accumulated in `float64` and narrowed once. At z ≈ 40, float32 has ~7 significant digits — far more than a threshold comparison needs. |

## Do not touch

- `backend/internal/dsp/correlate/correlate.go`'s `prepare`, `fillPadded`, the
  lag-scan bounds/overlap rule, and `peakProminence` — the windowed-parity path
  (`backend/internal/syncjob/window_parity_test.go`) depends on them bit for bit.
- `backend/internal/dsp/testdata/golden/**` and `GOLDEN.md` — the parity record.
- `backend/internal/dsp/correlate/perf_test.go` / `bench_test.go` thresholds —
  a gate that gets loosened to accommodate a slice is not a gate.
- `backend/internal/store/migrations/**` and `backend/schema.sql`.
- `backend/internal/mux/**` — no offset application logic changes here.
- `docs/adr/**`.
