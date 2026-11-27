# 05 — #84 piecewise sync for mid-movie cuts

Branch: `feat/84-piecewise-sync` · Merge position: 5 of 8 · Depends on: 02 (fit), 03 (pipeline), 04 (optional technique) · Blocks: nothing

## Goal

A theatrical dub played against an extended cut does not have one offset; it has
one per segment, and the joins are exactly where scenes were inserted. Applying a
single median offset leaves every scene after the first insert out by the total
insert so far — the failure the report currently describes as `drifting` and the
worker refuses. This slice samples the timeline densely, finds the segment
boundaries with a split-penalty fit, and — **only when the operator turns it on**
(D5) — cuts and pads the dub copy per segment, concatenating it in **one ffmpeg
pass**, then muxes with `--sync 0:0`. The target video is never re-encoded, the
vault is never written, and a boundary the analysis is not confident about
aborts the piecewise attempt and falls back to the dominant single offset,
surfaced for review (D5).

## Design

### Dense sampling

Six checkpoints cannot locate a boundary. When piecewise is enabled, the plan is
a uniform grid instead of the shipped percentages:

```go
// backend/internal/analyze/plan.go
// PlanWindowsEvery builds a uniform checkpoint grid: one window every stepSec,
// centred in its slot, clamped to the timeline by the same rules PlanWindow
// already applies (margin, duration, unknown origin). Positions are reported as
// percentages so an old reader of Checkpoint.PositionPct still has a number.
func PlanWindowsEvery(t Timing, stepSec, windowSec float64) ([]Window, error)
```

- First centre at `stepSec/2`, then every `stepSec`, while `centre + windowSec/2
  <= dur - CheckWindowMarginSec`.
- A 2 h film at 45 s step is 160 checkpoints. Cost guard: `MaxDenseSamples = 400`
  in `analyze` — beyond it the step is widened to `dur/MaxDenseSamples` and the
  report records the widened step (`piecewise.step_sec`), so a 10-hour source
  degrades in resolution instead of exploding.
- Windows never overlap the ends (`clampWindow`, `plan.go:202-215`) — reused, not
  re-implemented.
- Boundary resolution is therefore ±`step_sec` (45 s by default), which is why
  `Boundary.UncertaintySec` is reported and why the muxer's result is verified
  rather than trusted.

The analysis source (`windows` / `full`) is unchanged; dense sampling is a plan,
not a new source. `parallel_checkpoints` already bounds the workers, so 160
checkpoints are just 160 slots in the existing pool.

### Segmentation — `backend/internal/analyze/segment.go` (new)

```go
// A Segment is one run of the film during which the offset is constant.
type Segment struct {
    StartSec   float64 `json:"start_sec"`
    EndSec     float64 `json:"end_sec"`
    OffsetMS   float64 `json:"offset_ms"`
    Confidence float64 `json:"confidence"` // min confidence of its points
    Points     int     `json:"points"`
}

// A Boundary is where one segment ends and the next begins, in film time.
type Boundary struct {
    AtSec          float64 `json:"at_sec"`
    UncertaintySec float64 `json:"uncertainty_sec"` // half the sampling step
    StepMS         float64 `json:"step_ms"`         // offset change across it
}

// Piecewise is the segmentation result. RejectedReason is empty on success.
type Piecewise struct {
    Segments         []Segment  `json:"segments"`
    Boundaries       []Boundary `json:"boundaries"`
    DominantOffsetMS float64    `json:"dominant_offset_ms"`
    RejectedReason   string     `json:"rejected_reason,omitempty"`
}

// BuildSegments finds the piecewise-constant structure of the offsets.
//
// It is a chi-squared segmentation: the cost of a run of points is the sum of
// squared residuals around that run's median, and a split is accepted only when
// it buys ChisqPerPoint degrees of freedom and lowers the cost by more than
// SplitPenaltyMS per new segment. That is what keeps ordinary jitter (±10 ms)
// from producing fifteen "segments" on a clean film.
func BuildSegments(windows []Window, offsets, qualities []float64, p PiecewisePolicy, th Thresholds) Piecewise
```

Algorithm (exact, so it is testable):

```
1. keep the points; drop a point whose quality < th.LowQuality (it cannot testify)
   if fewer than 8 points survive → RejectedReason = "too few confident checkpoints"
2. cost(i,j) = Σ (offset_k - median(i..j))²  for k in [i,j]      (prefix medians)
3. DP over j: best[j] = min_i ( best[i-1] + cost(i,j) + penalty )   penalty = SplitPenaltyMS²
   with best[0] = 0 and the usual "penalty is charged per extra segment" bookkeeping
4. read the split points back; that is the segment list
5. accept a boundary only if ALL of:
      a. both sides have >= MinSegmentSec of film
      b. |medianLeft - medianRight| >= MinStepMS   (2 x th.StableSpreadMS = 80 ms)
      c. each side has >= 3 points
      d. the point nearest the boundary on each side has quality >= th.LowQuality
   otherwise merge the two runs and RejectedReason = "boundary at <t> s is not
   confident enough"
6. if fewer than 2 segments survive → RejectedReason = "the timeline is a single
   offset"
7. DominantOffsetMS = median over all points (the fallback 0.4.0 would apply)
```

`PiecewisePolicy` (the settings block; see below) carries `StepSec`, `WindowSec`,
`SplitPenaltyMS`, `MinSegmentSec`, `MaxSegments`. `MaxSegments` caps the output:
if the DP wants more, the cheapest splits are dropped until it fits, and the
report says so via `RejectedReason` **only** if that leaves one segment.

The DP is O(n²·log n) with n ≤ 400 — a few milliseconds, and it runs once per
job, not per checkpoint.

### Report and verdict

```go
type Report struct {
    // ... slices 01/02 fields ...
    Piecewise *Piecewise `json:"piecewise,omitempty"`
}
```

- Vacuum case (disabled): `Piecewise == nil`, report byte-identical to slice 02.
- Enabled but rejected: `Piecewise != nil` with `RejectedReason`, `verdict`
  unchanged (`drifting`), and `ReviewReason` set so the queue row can say
  *"cut not confident — applied the dominant offset"*.
- Enabled and accepted with ≥2 segments: `verdict = "ok"`,
  `classification = "piecewise"`, `max_drift_ms` recomputed on the
  **residualised** offsets (each point against its own segment's median), so a
  successful piecewise job reports a small drift rather than the 2.5 s step.
- `Report.ReviewReason string json:"review_reason,omitempty"` is added by this
  slice; it is distinct from `Reason` (which belongs to the reference-language
  resolver).

`classify` gains one argument — the accepted `*Piecewise` — and one early rule,
placed **after** `out_of_range` (an out-of-range checkpoint is out of range
whatever the segmentation says) and **before** `unreliable`:

```
out_of_range > piecewise-ok > unreliable > drifting > ok
```

This is the only change to the precedence block in 0.5.0, it is additive (the
branch is dead when `Piecewise` is nil), and `TestAnalyzeVerdictPrecedence` keeps
passing with piecewise off.

### Rendering: one ffmpeg pass over the dub copy

`backend/internal/transcode/segments.go` (new):

```go
// A Segment tells the encoder which part of the dub to take and where to put it.
// StartSec/EndSec are positions in the TARGET timeline, not in the dub: the
// encoder shifts them by OffsetMS to find the source material.
type Segment struct {
    StartSec float64 `json:"start_sec"`
    EndSec   float64 `json:"end_sec"`
    OffsetMS float64 `json:"offset_ms"`
}

// SegmentsTotalMS is the output duration a segmented encode must produce.
func SegmentsTotalMS(segs []Segment) int64

// BuildSegmentedArgs renders the filtergraph. Exported for the argv test.
func BuildSegmentedArgs(req Request) ([]string, error)
```

Filtergraph for `N` segments (channel layout and rate come from the plan exactly
as `BuildArgs` already does):

```
i-th segment, source window = [StartSec - OffsetMS/1000, EndSec - OffsetMS/1000]
clamped to the dub's duration; a negative start becomes lead silence, an end
past the duration becomes trailing silence.

[0:a]atrim=start=<s>:end=<e>,
     asetpts=PTS-STARTPTS,
     aformat=sample_fmts=fltp:channel_layouts=<layout>,
     aresample=<rate>,
     adelay=delays=<leadMS>:all=1,
     apad=whole_dur=<lenSec>,
     atrim=duration=<lenSec>            [seg0]
...                                     [segN]
[seg0][seg1]...[segN-1]concat=n=N:v=0:a=1[out]

-map [out] -vn -sn -dn -c:a <codec> -ar <rate> ... -f matroska
```

`adelay` + `apad` + `atrim=duration` guarantee every segment is exactly
`EndSec − StartSec` long, so `concat` cannot drift and the total equals
`SegmentsTotalMS`. Nothing here is speculative: each of these filters is already
used elsewhere in the tree or is a documented ffmpeg filter, and the argv test
pins the exact string.

`Transcode` branches on `len(req.Segments) > 0`; verification expects
`SegmentsTotalMS` instead of `SourceDurationMS` (the existing ffprobe duration
check, `transcode.go:194-197`). The audio stream must be exactly `target
duration` long; if it is not, `ErrVerify` fails the job before any mux.

Codec: a piecewise render is **always** an encode. If a Transcode Rule matched,
its codec/settings apply. If none matched, the codec is FLAC — lossless, so the
operator's dub is not degraded by a feature they switched on for timing reasons
(ADR-0010's lossless boundary).

### Worker

`syncworker.processJob`, after the report and before the mux decision:

```go
switch {
case report.Piecewise != nil && len(report.Piecewise.Segments) > 1 && report.Verdict == analyze.VerdictOK:
    segs := toTranscodeSegments(report.Piecewise.Segments)
    if !plan.Encode() { plan = transcode.Plan{RuleIndex: transcode.NoRule, Codec: transcode.CodecFLAC} }
    // encoder availability, then:
    audio, err = w.transcodeStaged(ctx, job.ID, audio, plan, segs, track)
    offset = 0
    report.Application = &analyze.Application{
        Mode: "re-encoded", SegmentCount: len(segs), Segments: report.Piecewise.Segments,
        Note: fmt.Sprintf("re-encoded (piecewise, %d segments)", len(segs)),
    }
case report.Piecewise != nil:
    offset = int64(math.Round(report.Piecewise.DominantOffsetMS)) // rejected: dominant, and flagged
    // muxability is still judged by muxableReport — a rejected piecewise job on a
    // drifting verdict stays unmuxable unless slice 02's tempo path accepted it
default:
    // slice 02 behaviour
}
```

`muxableReport` gains one clause: `report.Verdict == ok && report.Piecewise
accepted` is muxable (it already is, via `muxableVerdict(ok)`), and a piecewise
**rejection** never makes a `drifting` report muxable — the whole point is that
the operator must look at it.

`transcodeStaged` gains a `segs []transcode.Segment` parameter (nil on the rule
path); its two call sites are updated. Its log line prints `piecewise, N
segments` instead of a rule index when `len(segs) > 0`.

The mux offset is `0`: the audio is already aligned. `SyncOffsetMS` is stored as
0, so a reader can tell the two mechanisms apart — with the `application` block
saying which one ran.

## Settings

`sync.strategy` gains one sub-object (D5: off by default). Slice 03 shipped the
section without it; this slice adds the field and an absent-tolerant
`UnmarshalJSON`, so no migration and no "exists but does nothing" flag (D1).

```json
"piecewise": {
  "enabled": false,
  "step_sec": 45,
  "window_sec": 60,
  "split_penalty_ms": 250,
  "min_segment_sec": 120,
  "max_segments": 12
}
```

Validation (added to `validateStrategy`):

| Field | Range | Error |
|---|---|---|
| `step_sec` | 15..300 | `sync.strategy: piecewise.step_sec must be 15..300` |
| `window_sec` | 20..190 | `sync.strategy: piecewise.window_sec must be 20..190` |
| `step_sec >= window_sec/2` | — | `sync.strategy: piecewise.window_sec must be at most twice step_sec` |
| `split_penalty_ms` | 50..5000 | `sync.strategy: piecewise.split_penalty_ms must be 50..5000` |
| `min_segment_sec` | 30..3600 | `sync.strategy: piecewise.min_segment_sec must be 30..3600` |
| `max_segments` | 2..40 | `sync.strategy: piecewise.max_segments must be 2..40` |

Also: when `enabled` is true, `min_confidence` of the first technique must be
≥ 0.10 (unchanged) and `window_sec` must not exceed `180 -
CheckWindowMarginSec` — i.e. a dense window still has to fit the ±`max_offset_sec`
search bound; the validator rejects a combination that cannot resolve.

### Presets

| Preset | `piecewise` |
|---|---|
| `balanced` (default) | `enabled: false` (D5) |
| `fast` | `enabled: false` |
| `maximum_accuracy` | `enabled: true`, everything else at the defaults above |

`PresetTechniques` becomes `Preset(preset) analyze.Strategy` returning the whole
section (techniques + piecewise), so one function remains the single source of
truth for what a preset means.

### UI strings (label, tooltip, preset membership)

| Field | Label | Tooltip (what / when / cost) | Presets |
|---|---|---|---|
| `piecewise.enabled` | **Split a re-cut dub into segments** | "Finds where the film was extended and shifts each part of the dub on its own, instead of applying one offset to the whole track. Use it when the dubbed release was cut differently from your copy — the giveaway is a small offset at the start that grows through the film and jumps at a scene change. Cost: the dub copy is re-encoded once, losslessly, and the analysis samples the whole film instead of six points (a few minutes more for a feature). The video is never touched." | maximum_accuracy |
| `piecewise.step_sec` (Advanced) | **Check every (seconds)** | "How often the timeline is sampled. Smaller finds a boundary more precisely and costs proportionally more analysis time. 45 s locates a boundary to within about ±22 s." | 45 |
| `piecewise.window_sec` (Advanced) | **Sample length (seconds)** | "How much audio is compared at each sample. Longer survives more noise but blurs a boundary that is close to a hard cut. Must be at most twice the sampling interval." | 60 |
| `piecewise.split_penalty_ms` (Advanced) | **Evidence needed for another segment** | "How much better a two-part explanation must fit before another segment is added. Higher means fewer, more certain segments; lower finds subtle re-cuts and invents segments in noisy material." | 250 |
| `piecewise.min_segment_sec` (Advanced) | **Shortest segment (seconds)** | "Segments shorter than this are merged into their neighbour, because a very short segment is far more likely to be a measurement error than a real edit." | 120 |
| `piecewise.max_segments` (Advanced) | **At most how many segments** | "A cap on the result. Real re-cuts have a handful of joins; a hit count in the dozens means the boundaries are noise." | 12 |

## Data flow

```
sync.strategy.piecewise.enabled
        │
        ▼
analyze.PlanWindowsEvery(dur, step 45, window 60)  → 160 windows
        │
        ▼
slice 03 pipeline, per checkpoint (parallel, existing pool)
        │   offsets[], qualities[], match_z[]
        ▼
BuildSegments(...) ──► Piecewise{segments, boundaries} | RejectedReason
        │                            │
        │ accepted, N>1              │ rejected / N==1
        ▼                            ▼
classify → verdict=ok,          verdict stays drifting,
classification=piecewise        ReviewReason="cut not confident…"
        │                            │
        ▼                            ▼
plan (rule | FLAC) + segments   mux with dominant offset (or fail as today)
        │
        ▼
transcode.Transcode (ONE pass: atrim/adelay/apad/concat → codec)
        │  verify: probed duration == SegmentsTotalMS
        ▼
mkvmerge --sync 0:0 → publish (ADR-0004, ADR-0018 untouched)
        │
        ▼
report.application.note = "re-encoded (piecewise, 3 segments)"
queue row shows the note + the boundary list
```

## File-by-file changes

| Path | Change |
|---|---|
| `backend/internal/analyze/segment.go` | **New.** `Segment`, `Boundary`, `Piecewise`, `PiecewisePolicy`, `BuildSegments`, the DP, `MinStepMS` derivation from `Thresholds`. |
| `backend/internal/analyze/plan.go` | `PlanWindowsEvery`, `MaxDenseSamples`. No change to `PlanWindow`/`clampWindow`. |
| `backend/internal/analyze/analyze.go` | `Config.Piecewise`; `Report.Piecewise`, `Report.ReviewReason`; choose the dense plan when enabled; call `BuildSegments` after the pipeline and after the fit; pass it to `classify`; recompute `max_drift_ms` on residuals when accepted. |
| `backend/internal/analyze/technique.go` | `Strategy.Piecewise PiecewisePolicy`; validation ranges; `DefaultStrategy` gains `enabled: false`. |
| `backend/internal/transcode/segments.go` | **New.** `Segment`, `SegmentsTotalMS`, `BuildSegmentedArgs`. |
| `backend/internal/transcode/transcode.go` | `Request.Segments []Segment`; branch to `BuildSegmentedArgs`; verify against `SegmentsTotalMS` when segmented. `BuildArgs` untouched. |
| `backend/internal/settings/settings.go` | piecewise validation in `validateStrategy`; `Preset()` replaces `PresetTechniques` for the whole section; absent-tolerant decode. |
| `backend/internal/syncworker/worker.go` | The `switch` above; `transcodeStaged` gains `segs`; `muxableReport` comment about piecewise rejections; `Application` note. |
| `backend/internal/syncworker/worker_test.go` | The piecewise tests below; fake transcoder records `Segments`. |
| `frontend/src/lib/api.ts` | `SyncPiecewise`, `SyncSegment`, `SyncBoundary`; `SyncReport.piecewise?`, `review_reason?`, `application.segment_count?`/`segments?`. |
| `frontend/src/components/StrategyEditor.tsx` | The piecewise block: toggle + collapsed Advanced holding the five knobs. |
| `frontend/src/lib/defaults.ts` | `piecewise` in the default section. |
| `frontend/src/pages/Sync.tsx` | Segment table (start/end/offset/confidence) and boundary list when `piecewise` is present and accepted; the `review_reason` badge; `application.note` on the job line. |
| `frontend/src/components/CheckpointChart.tsx` | Draw vertical dashed markers at accepted boundaries. No geometry change otherwise. |
| `backend/internal/acceptance/piecewise_test.go` | **New, opt-in.** Generated-media end-to-end (below). |

## Test plan

### Unit tests (new, by name)

`backend/internal/analyze/segment_test.go` (**new**):

- `TestPlanWindowsEveryCoversTheTimeline` — 5400 s, step 45, window 60 ⇒ 119 centres, each `step` apart, first at 22.5 s, last ≥ `5400−margin−30`, none overlapping the last 10 s.
- `TestPlanWindowsEveryWidensPastTheCap` — 36 000 s ⇒ at most `MaxDenseSamples` windows and the effective step reported.
- `TestBuildSegmentsFindsOneCut` — 160 points, offset 0 up to 50 % then +2500: exactly 2 segments, boundary within ±1 step of 50 %, `StepMS ≈ 2500`, `DominantOffsetMS ≈ 0`.
- `TestBuildSegmentsKeepsACleanTimelineAtOneSegment` — jitter ±10 ms ⇒ 1 segment, `RejectedReason == "the timeline is a single offset"`.
- `TestBuildSegmentsPenaltySuppressesNoiseSplits` — random ±200 ms noise on a flat line ⇒ 1 segment with the default penalty (the test asserts the penalty is what stops it: rerunning with `SplitPenaltyMS: 1` yields > 1 segment).
- `TestBuildSegmentsRejectsAnUnconfidentBoundary` — a 2500 ms step where the two points adjacent to the join have `quality 0.05` ⇒ `RejectedReason` naming the boundary's time, `len(Segments) == 1`, `DominantOffsetMS` still the median.
- `TestBuildSegmentsMergesShortRuns` — a 3-point island in the middle of a flat line ⇒ merged (`MinSegmentSec`).
- `TestBuildSegmentsDropsNothingWhenQualityIsHigh` — every point is used (`Points` sums to the input count).
- `TestBuildSegmentsCapsSegments` — a deliberately jagged input with `MaxSegments: 3` yields 3.

`backend/internal/analyze/analyze_test.go` (extend):

- `TestIntegratePiecewiseAcceptedIsOK` — the dense-plan half-window offset ramp through the real engine with `Piecewise.Enabled`: `verdict == ok`, `classification == "piecewise"`, 2 segments.
- `TestIntegrationPiecewiseDrift` **must keep its current assertion** (`drifting`) — that fixture runs with piecewise **disabled**, which is the shipped default. Add an explicit comment in the test saying so.
- `TestAnalyzeVerdictPrecedence` — unchanged, run with piecewise off.
- `TestPiecewiseOutOfRangeStillWins` — a point beyond `max_offset_sec` ⇒ `out_of_range` even with 3 accepted segments (precedence proof).
- `TestReportJSONShapeCarriesPiecewise` — `piecewise` and `review_reason` present.

`backend/internal/transcode/segments_test.go` (**new**):

- `TestBuildSegmentedArgsRendersTrimDelayAndConcat` — 2 segments ⇒ argv contains two `atrim=start=`, two `adelay=`, one `concat=n=2:v=0:a=1`, and exactly one `-map [out]`.
- `TestBuildSegmentedArgsClampsAtTheEdges` — a segment whose source start is negative gets `adelay=delays=<ms>:all=1` and a trimmed `atrim` start of 0; a segment past the end is `apad`-ed.
- `TestSegmentsTotalMSIsTheSum` — and `Transcode`'s verification uses it: a fake ffprobe reporting a 1 s short duration ⇒ `ErrVerify`.
- `TestSegmentedEncodeIsOnePass` — exactly one `ffmpeg` invocation for 3 segments (the fake runner counts calls).

`backend/internal/syncworker/worker_test.go` (extend):

- `TestProcessOncePiecewiseReencodesOnceWithZeroSync` — report `ok` with 2 segments: one `Transcode` call carrying 2 segments, `Plan.Codec == flac` when no rule matched, mux receives `SyncOffsetMS == 0`, and the stored report's `application.note == "re-encoded (piecewise, 2 segments)"` with `segment_count == 2`.
- `TestProcessOncePiecewiseRejectionFallsBackToMedian` — `verdict == drifting`, `Piecewise.RejectedReason` set ⇒ `ErrUnmuxableVerdict`, zero transcode calls, zero mux calls, `review_reason` surfaced on the job.
- `TestProcessOncePiecewiseUsesTheRuleCodec` — a matching Transcode Rule ⇒ its codec/bitrate are used for the segmented render, not FLAC.
- `TestProcessOncePiecewiseNeedsAnEncoder` — FLAC unavailable ⇒ `ErrEncoderUnavailable` before any transcode.

### End-to-end, generated media (D10)

`backend/internal/acceptance/piecewise_test.go` (new) builds its fixture with
ffmpeg at test time (no committed media):

1. `ffmpeg -f lavfi -i testsrc2=size=320x240:rate=24:duration=40` → `video.mkv`
   (video only).
2. A deterministic noise+tone 40 s audio track as the film's original audio.
3. The "dub": the same audio with a 3 s scene inserted at 20 s and the whole
   thing delayed by 800 ms.
4. Mux film video + film audio → `target.mkv`; mux the dub into `dub.mka`.
5. Run a job with `sync.strategy.piecewise.enabled = true`, step 5 s, window 5 s.
6. Assert: `report.piecewise.segments` has 2 entries, the boundary is within
   ±6 s of 20 s, `verdict == ok`, the published output's video streamhash equals
   the target's (`videoStreamHash` from `harness_test.go`), and the vault copy's
   SHA-256 is unchanged.

Skips via `requireAcceptanceTools` when ffmpeg/ffprobe/mkvmerge are absent, like
every other acceptance test; it needs **no** real media, so it is not behind
`DUBARR_PARITY`, only behind tool availability.

### Frontend

`frontend/src/test/settings-strategy.test.tsx` (extend): the piecewise toggle is
off by default, maximum accuracy turns it on, and the Advanced disclosure reveals
five knobs with the documented defaults.
`frontend/src/test/sync.test.tsx` (extend): a piecewise report renders the
segment table and the boundary markers; a rejected one renders the review badge
and no table.

## Acceptance criteria

1. `cd backend && go build ./cmd/dubarr && go test ./... && go vet ./...` exits 0; `cd frontend && npm run build && npm run lint && npm run test` exits 0.
2. A fresh install's `sync.strategy.piecewise.enabled` is `false`, and a job run without touching settings produces a report with **no** `piecewise` key (byte-identical offsets to slice 02).
3. The generated-media end-to-end test passes: 2 segments, boundary within ±6 s of the inserted scene, `verdict == "ok"`, `application.note == "re-encoded (piecewise, 2 segments)"`.
4. That job performs exactly **one** `ffmpeg` invocation and exactly **one** `mkvmerge` invocation (`SyncOffsetMS == 0`), asserted by the fakes.
5. `sha256sum` of the vault's dub file is identical before and after the job; the published output's video streamhash equals the target's.
6. A job whose boundary is unconfident ends `failed` with `ErrUnmuxableVerdict`, `report.review_reason` non-empty, and creates no file in `VAULT_DIR`.
7. `PUT /api/settings` with `step_sec: 5` returns 400 containing `piecewise.step_sec must be 15..300`; with `window_sec: 200`, 400 containing `20..190`.
8. `go test ./internal/analyze -run TestIntegrationPiecewiseDrift` still passes unchanged (piecewise off ⇒ `drifting`).
9. No new column, no migration, `backend/internal/mux/**` unchanged.

## Migration / compatibility

- **Settings:** one new sub-object inside an existing section; absent ⇒
  `enabled: false` and the documented numeric defaults. Old rows need no rewrite.
- **Reports:** `piecewise` absent unless the feature ran; `review_reason`
  absent otherwise. A 0.5.0 reader of a 0.4.0 row is unaffected.
- **`classification == "piecewise"`** is a new value in an existing field. Any
  consumer that switches on `classification` must add the case; the frontend is
  the only one, and it is updated here. `verdict` stays inside the existing
  four-value vocabulary.
- **`SyncOffsetMS == 0`** with `application.mode == "re-encoded"` is the
  documented signature of a piecewise job. Nothing else changes it.
- **Existing `drifting` jobs** are not re-run or re-judged. Turning piecewise on
  affects only new jobs.

## Risks

| Risk | Mitigation |
|---|---|
| A wrong boundary produces audio that is worse than a single offset. | Boundaries need ≥3 points per side, a ≥80 ms step, confident adjacent points, and a penalty; a rejected boundary falls back to the dominant offset and is surfaced. Verification then checks the rendered duration before any mux, and the output is published only through the existing validated-temp path (ADR-0018). |
| A 20-minute lossless re-encode of the dub. | Only for jobs the operator explicitly enabled piecewise for; FLAC-only when no rule matched; one pass; the tooltip's cost sentence states it. |
| Dense sampling multiplies analysis time. | `MaxDenseSamples = 400` cap, windows are the same size as today, the existing parallel pool is reused, and the cost is stated in the setting. |
| `concat` output is a few milliseconds off, shifting everything after a join. | `adelay`+`apad`+`atrim=duration` per segment make each piece exact; the probe must equal `SegmentsTotalMS` or the job fails. |
| ffmpeg's `concat` filter requires identical formats. | Every branch runs the same `aformat`+`aresample` before the join, taken from `planFor`'s existing layout/rate choice. |
| The DP's O(n²) cost with 400 points. | 160 k median computations over prefix structures, single-threaded at job end: milliseconds. If it ever matters, the penalty makes most splits prunable. |
| A piecewise job that silently also applied a tempo (slice 02). | Mutually exclusive by construction: the tempo path requires `shape == drift`, the piecewise path requires an accepted segmentation; if both somehow hold, the `switch` takes piecewise first and `Application.Mode` records only one. Documented in the worker comment. |

## Do not touch

- `backend/internal/mux/**` — the muxer takes an offset of 0 and a track; it does
  not learn about segments. ADR-0004 stands: mkvmerge still muxes.
- The target video, in any code path. ADR-0018's validated-temp publication and
  `videoStreamHash` check are the guard, not something to bypass.
- `VAULT_DIR`: the segmented render reads `{WORK_DIR}/jobs/{id}/` and writes
  `audio.transcoded.mka` there. No vault write anywhere in this slice.
- `transcode.BuildArgs` (the non-segmented single pass) and `ValidateTempo`.
- `analyze.PlanWindow` / `clampWindow` / `window.go`'s source modes.
- `schema.sql`, applied migrations, `docs/adr/**` — ADR-0020 is drafted in
  `ADR-DRAFTS.md` and committed by the orchestrator.
