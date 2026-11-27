# 04 — #83 MFCC correlation as an alternative matcher

Branch: `feat/83-mfcc-matcher` · Merge position: 4 of 8 · Depends on: 03 (technique pipeline), 01 (match_z / precision vocabulary) · Blocks: nothing

## Goal

Raw-waveform correlation breaks when the same music and effects arrive with a
different phase and envelope — a low-bitrate re-encode, broadcast processing,
clipping. #83 asks for a second matcher that compares the *shape* of the sound
(mel-frequency cepstral coefficients) instead of its samples, producing the same
answer contract as the waveform matcher so the strategy pipeline from slice 03
can run either. It must be honest about its accuracy: the matcher is
hop-quantised, so it reports `precision_ms = 10` and never claims sample
accuracy (D3). It is **not** in the default list (D3), so 0.5.0 run as shipped
never consults it.

## Design

### Where it plugs in: the existing correlator seam

`sync.Runner` already takes the correlation as a function
(`backend/internal/sync/runner.go:26-28`), because tests substitute it. This
slice exports that seam rather than inventing a second one:

```go
// backend/internal/sync/runner.go
// Correlator is the correlation seam: one answer for one pair of slices.
// correlate.Analyze is the waveform implementation; mfcc.Analyze is the
// mel-cepstral one. Both fill the same correlate.Analysis.
type Correlator func(ctx context.Context, target, donor []float32, sampleRate uint32, windowSec float64) (correlate.Analysis, error)

// WithCorrelator replaces the waveform correlator. Everything else the runner
// does — validation, WAV reading, the slice echo, deadline classification,
// panic recovery — is unchanged, so a second matcher inherits it all.
func WithCorrelator(c Correlator) Option
```

`mfcc` returns `correlate.Analysis` and therefore imports `correlate`. This is
deliberate, not an accident: #83 asks for "the same `(offset_ms, confidence)`
contract", and reusing the type also reuses `correlate.SilentSlice`,
`correlate.SliceTooShort`, `correlate.InvalidJob` and `correlate.NoValidLag`,
which the runner already maps to its error kinds. One contract, one error
vocabulary, no adapter.

Consequence: `sync.WithTimeout` still applies, the analysed-slice echo still
works, and `analyze` cannot tell the two matchers apart except by the precision
they report.

### Types — `backend/internal/dsp/mfcc/mfcc.go` (new package)

```go
package mfcc

// Analysis parameters. They are constants, not settings: a knob that changes
// the meaning of every stored offset is not an option (D11 says technical
// knobs live behind defaults that are correct for everybody).
const (
    FrameMS      = 25   // analysis frame
    HopMS        = 10   // frame step == the matcher's precision
    MelBands     = 26   // triangular filters, 0..min(4000, rate/2)
    Cepstra      = 13   // DCT-II coefficients kept per frame
    Preemphasis  = 0.97
    FMaxHz       = 4000
    MinOverlapPt = 1.0 / 20 // same overlap rule as correlate: 5 % of the slice
)

// Analyze correlates the mel-cepstral representation of two slices and returns
// the answer in the shared shape.
//
// OffsetSamples is quantised to the hop: the answer is the lag, in hops, of the
// best match, times the hop's sample count. OffsetSubSamples is that value (no
// fine interpolation: a sub-hop number computed from a 10 ms grid would be a
// guess dressed as precision). PrecisionMS is the hop in milliseconds, which is
// what the report and the UI must show.
//
// MatchZ and Quality are computed exactly as the waveform matcher computes them
// — (peak-mean)/std and (peak-median)/(1-median) over the matcher's own lag
// curve — so the strategy pipeline's thresholds mean the same thing for both.
func Analyze(ctx context.Context, target, donor []float32, sampleRate uint32, windowSec float64) (correlate.Analysis, error)
```

Internal shape:

```go
// frames converts one slice into a [frames][Cepstra] matrix plus its per-frame
// total energy (the denominator of the NCC).
func frames(samples []float32, sampleRate uint32) (coeffs [][]float32, energy []float64)

// filterbank builds MelBands triangles over [0, min(FMaxHz, rate/2)] on the
// frame's FFT bins (Hz -> mel -> Hz, the standard conversion).
func filterbank(sampleRate uint32, nfft int) [][]float32

// dctII keeps the first Cepstra coefficients of the orthonormal DCT-II.
func dctII(power []float64, keep int) []float64
```

### The algorithm, in order

```
target WAV slice (mono f32, 8 kHz)
  │
  ├─ pre-emphasis            y[n] = x[n] - 0.97·x[n-1]
  ├─ framing                 frame length = round(rate·0.025), hop = round(rate·0.010)
  │                          (200 / 80 samples at 8 kHz), Hamming window
  ├─ |RFFT|²                 per frame, nfft = nextPow2(frameLen) = 256 at 8 kHz
  ├─ mel filterbank          26 triangles → 26 band energies → log
  ├─ DCT-II, keep 13         → one 13-vector per frame
  └─ standardise             per coefficient over the whole slice: mean 0, unit var
        ▼
   X : [N][13]   Ex[n] = Σ_c X[n][c]²          (donor: Y, Ey)
        │
        ├─ S(k) = Σ_c Σ_n X[n][c]·Y[n-k][c]     ← 13 linear correlations, summed
        │        computed with RFFT/IRFFT at nfft = nextPow2(2N−1)  (sixteen
        │        thousand points for a 190 s slice, i.e. milliseconds)
        ├─ NCC(k) = S(k) / sqrt(ExOverlap(k)·EyOverlap(k))
        │        same overlap rule and same 1/20 minimum as correlate
        ├─ peak, MatchZ = (peak−mean)/std, Quality = (peak−median)/(1−median)
        └─ OffsetSamples = bestLagHops · hopSamples
           PrecisionMS   = 1000·hopSamples/rate          (10.0 at 8 kHz)
```

Why standardising per coefficient matters: a low-bitrate encode attenuates high
frequencies; without per-coefficient standardisation the later cepstra would be
near-constant and contribute nothing, and the loudness change would dominate the
first coefficient. Standardising makes every coefficient carry equal weight,
which is what makes the matcher survive a re-encode.

Why `c0` is kept: it is the frame's log-energy contour — the cheapest, most
compression-robust cue — and standardising removes the level offset that makes it
fragile.

Why there is no per-coefficient NCC: the coefficients of one frame are not
independent measurements of the same thing, so a single summed correlation with a
single energy normalisation is the honest statistic. It is also what the BBC tool
does (cross-correlate the standardised cepstra, then z-score the peak).

Cost: one 256-point real FFT per 10 ms of audio plus 39 transforms at 64 k for
the correlation — well under a tenth of the waveform matcher's ~1 s per 190 s
checkpoint. That is why the `fast` preset can use it.

### Wiring

`backend/internal/syncjob/syncjob.go` — `runAnalysis` already builds one engine.
It now builds a map, adding MFCC only when the strategy asks for it:

```go
engines := map[analyze.TechniqueID]analyze.Engine{
    analyze.TechniqueWaveform: sync.New(sync.WithTimeout(analyze.JobTimeout(cfg)), sync.WithLogger(s.logger)),
}
if strategyUses(cfg.Strategy, analyze.TechniqueMFCC) {
    engines[analyze.TechniqueMFCC] = sync.New(
        sync.WithCorrelator(mfcc.Analyze),
        sync.WithTimeout(analyze.JobTimeout(cfg)),
        sync.WithLogger(s.logger),
    )
}
```

`analyze.Analyze` (slice 03) already refuses a strategy whose enabled technique
has no engine, so a reported `mfcc` entry without an engine is a loud
composition-root failure, never a silent fallback to waveform.

Nothing else changes: `analyze` passes the same `sync.Job` (same paths, slice
start/length, window bound), reads the same echo, and stores the same fields; the
only observable difference is `precision_ms = 10` and the technique name in the
report.

## File-by-file changes

| Path | Change |
|---|---|
| `backend/internal/dsp/mfcc/mfcc.go` | **New.** Constants, `Analyze`, the frame/standardise/correlate pipeline, validation and errors mirroring `correlate`'s order and wording. |
| `backend/internal/dsp/mfcc/mel.go` | **New.** `hzToMel`, `melToHz`, `filterbank`, `dctII`, `powerSpectrum`. |
| `backend/internal/dsp/mfcc/doc.go` | **New.** Package doc: what it is for, why the precision is the hop, what it does **not** do (no dynamic time warping, no sub-hop claim), and the pointer to `correlate.Analysis`. |
| `backend/internal/dsp/mfcc/mfcc_test.go` | **New.** The unit tests below. |
| `backend/internal/dsp/mfcc/mel_test.go` | **New.** Filterbank/DCT tests. |
| `backend/internal/dsp/mfcc/golden_test.go` | **New.** `TestMFCCAgreesWithWaveformOnCommittedExcerpts` over `backend/internal/dsp/testdata/wav`. |
| `backend/internal/dsp/mfcc/testdata/gen/main.go` | **New.** The committed generator (`//go:build ignore`), see below. |
| `backend/internal/dsp/mfcc/testdata/expected.json` | **New, committed.** The numbers the generator measured and the tests assert. |
| `backend/internal/sync/runner.go` | Export `Correlator`, add `WithCorrelator`, keep the default `correlate.Analyze`; copy `PrecisionMS`/`MatchZ`/`OffsetSubMS` through as slice 01 specified. |
| `backend/internal/sync/runner_test.go` | `TestRunnerUsesTheInjectedCorrelator` — a stub correlator is called with the slice the job asks for, and its precision/z reach the `Result`. |
| `backend/internal/syncjob/syncjob.go` | Build the engines map; import `internal/dsp/mfcc`. |
| `backend/internal/analyze/technique.go` | No change — `TechniqueMFCC` already exists from slice 03. |
| `frontend/src/lib/settings-help.ts` | The MFCC tooltip text (already listed in slice 03) now has a working technique behind it; no new field. |
| `frontend/src/pages/Sync.tsx` | When the winning technique's `precision_ms >= 5`, render `±10 ms · MFCC` next to the offset instead of the sample-precision badge. This is D3's "must not present a hop-quantised offset as sample-accurate", and it is the only UI change. |

## Settings

**No new key.** MFCC is switched on by adding a technique entry to
`sync.strategy` (slice 03), and it belongs to the `fast` and
`maximum_accuracy` presets, never to `balanced` (D3: not in the default list).
The strings are already specified in `03-technique-pipeline.md`; this slice makes
them true. Nothing here is hidden behind a flag that pretends to work (D1): if
the entry is present, the matcher runs.

## Fixture design (D10)

`backend/internal/dsp/mfcc/testdata/gen/main.go`, `//go:build ignore`, run with
`cd backend && go run ./internal/dsp/mfcc/testdata/gen`:

1. Builds a 120 s deterministic "programme-like" signal: five sinusoids with
   different envelopes, a slow amplitude modulation, a percussive click every
   0.5 s, and a seeded noise floor. Deterministic from a constant seed, so the
   file is reproducible byte for byte.
2. Writes `target.wav` (mono PCM s16, 8 kHz).
3. Writes `donor.wav`: the same signal delayed by exactly 250 ms (2000 samples),
   trimmed to the same length.
4. Writes `donor-degraded.wav`: `donor.wav` through the degradation chain —
   4-tap moving-average low-pass, `tanh(3x)` soft clipping, 6-bit quantisation,
   and additive noise at −10 dB SNR. All pure Go, no ffmpeg, so it runs anywhere.
5. Measures and writes `expected.json`:

```json
{
  "offset_ms": 250,
  "precision_ms": 10,
  "mfcc_max_error_ms": 20,
  "degraded_waveform_quality_max": 0.10,
  "degraded_mfcc_min_confidence": 0.10,
  "degraded_mfcc_max_error_ms": 30,
  "sample_hash": "0x5eedbeef"
}
```

Total committed weight: a generator (~200 lines) plus a small JSON. The WAVs are
**gitignored** — add `backend/internal/dsp/mfcc/testdata/*.wav` to
`.gitignore` next to the existing `testfiles/` and `data/` entries.

Two copies of the synthesis exist on purpose, and a test keeps them honest:

- `backend/internal/dsp/mfcc/testsynth_test.go` builds the pairs in
  `t.TempDir()` for `go test ./...`, with no external tool and no committed
  media.
- `backend/internal/dsp/mfcc/testdata/gen/main.go` (`//go:build ignore`) is the
  committed, runnable generator an operator uses to inspect or regenerate the
  media. It carries its own copy of the same synthesis.
- `TestSyntheticBuilderMatchesCommittedHash` recomputes the 32-bit FNV hash of
  the generated target samples and compares it to `expected.json`'s
  `sample_hash`. If the two builders ever diverge, the test fails instead of the
  committed numbers silently describing audio nobody renders.


If the degradation turns out not to push the waveform matcher below the 0.10
floor, the degradation chain is tightened in the generator **and the test's
assertion stays as written** — the fixture serves the assertion, never the
reverse. That is recorded in `expected.json`'s own git history, not by relaxing
the test.

## Test plan

### Unit tests (new, by name)

`backend/internal/dsp/mfcc/mel_test.go`:

- `TestFilterbankTrianglesTileTheBand` — for a 8 kHz rate with `nfft = 256`, every filter is non-negative, adjacent filters overlap by exactly one bin, the first starts at 0 Hz and the last ends at 4000 Hz, and each filter's weights sum to 1 within 1e-6.
- `TestFilterbankClampsToNyquist` — at a 1000 Hz rate the top edge is 500 Hz, not 4000 Hz.
- `TestDCTIIRecoversAConstant` — a constant vector's first coefficient is the scaled sum and the rest are 0.
- `TestDCTIIIsOrthonormalOnAKnownVector` — a hand-computed 8-point case, compared within 1e-9.

`backend/internal/dsp/mfcc/mfcc_test.go`:

- `TestFramesCoverTheSliceAtTenMillisecondHops` — a 10 s slice at 8 kHz yields 998 frames (`(80000−200)/80 + 1`), each 200 samples.
- `TestMFCCRecoversAKnownShift` — the synthetic pair delayed by 250 ms ⇒ `OffsetSamples == 2000` exactly (a whole number of hops), `OffsetSubSamples == 2000`, `PrecisionMS == 10`.
- `TestMFCCQuantisesToTheHop` — a pair delayed by 255 ms ⇒ `OffsetSamples == 2000` (nearest hop, 10 ms), never 2040; the test asserts the answer is a multiple of the hop by construction.
- `TestMFCCMatchZUsesTheSameScaleAsWaveform` — the clean pair yields `MatchZ > th.MatchZGood` (10) and the unrelated-noise pair `MatchZ < th.MatchZMaybe` (5), using the *same* `DefaultThresholds`.
- `TestMFCCSurvivesClippingAndLowPass` — the degraded fixture: `Quality >= th.LowQuality`, `|OffsetSamples·1000/rate − 250| <= 30`, and the **waveform** matcher on the same input reports `Quality < th.LowQuality`. This is #83's acceptance in one test.
- `TestMFCCRejectsSilence` — a zero slice returns `*correlate.SilentSlice` with `Which == "target"`.
- `TestMFCCRejectsAShortSlice` — 63 samples returns `*correlate.SliceTooShort` with `Min == correlate.MinSliceSamples`.
- `TestMFCCRejectsABadWindow` — `windowSec <= 0` and `sampleRate == 0` return `*correlate.InvalidJob` with the same wording as `correlate`.
- `TestMFCCPinsABoundaryPeakToTheSearchEdge` — a shift larger than `windowSec` returns `|offset| == maxLag` **and** `MatchZ < th.MatchZMaybe`, so the pipeline cannot mistake exhaustion of the search for a match.
- `TestMFCCIsCancellable` — a cancelled context returns an error wrapping `ctx.Err()` and never a partial `Analysis`.

`backend/internal/dsp/mfcc/golden_test.go`:

- `TestMFCCAgreesWithWaveformOnCommittedExcerpts` — for every entry in `backend/internal/dsp/testdata/golden` whose recorded quality is ≥ 0.5, `|mfcc.OffsetMS − golden.OffsetMS| <= 20` ms (the hop plus one sample of grid error). Entries below 0.5 quality are skipped by name in the test, with a comment: the waveform matcher itself does not trust them. This is #83's "reproduces the waveform matcher offset within a few ms" measured on real audio, with a tolerance the hop can actually meet.

`backend/internal/sync/runner_test.go` (extend):

- `TestRunnerUsesTheInjectedCorrelator` — the injected stub records the `(len(target), len(donor), rate, window)`; assert the slice lengths match the planned window and that its `PrecisionMS`/`MatchZ` land on the `Result`.

`backend/internal/syncjob/syncjob_test.go` (extend):

- `TestStrategyWithoutMFCCBuildsNoMFCEEngine` — the balanced list ⇒ the engines map has exactly `waveform` (asserted through a test hook on `runAnalysis`, or by asserting the report's technique and that no mfcc-only behaviour occurs).
- `TestStrategyWithMFCCTechniqueRunsIt` — a two-entry strategy over the synthetic pair ⇒ `report.checkpoints[i].technique == "mfcc"` where waveform was weak, `precision_ms == 10`.

### Opt-in (real content)

`DUBARR_PARITY=1 go test ./internal/acceptance -run MFCC` (new test in
`backend/internal/acceptance/mfcc_test.go`) on the Shrek and Minions pairs:
assert `|mfcc − waveform| <= 20` ms on each pair where the waveform verdict is
`ok`, and log both numbers. Media and tools required ⇒ `t.Skip` when absent,
same pattern as `requireAcceptanceTools`.

## Acceptance criteria

1. `cd backend && go build ./cmd/dubarr && go test ./... && go vet ./...` exits 0; `cd frontend && npm run build && npm run lint && npm run test` exits 0.
2. `TestMFCCSurvivesClippingAndLowPass` passes: MFCC succeeds on the degraded fixture while the waveform matcher's quality is below 0.10.
3. `TestMFCCRecoversAKnownShift` reports exactly `2000` samples for a 250 ms delay at 8 kHz, and `PrecisionMS == 10`.
4. `go test ./internal/dsp/correlate -run TestGoldenExcerpts` still passes — the MFCC package adds no file under `internal/dsp/correlate` or `testdata/golden`.
5. A fresh install's `sync.strategy` still contains exactly one waveform entry (`TestDefaultStrategyIsOneWaveformEntry`), i.e. MFCC did not leak into the default.
6. Choosing the **Fast** preset and running a job yields `report.checkpoints[*].technique == "mfcc"`, `report.precision_ms == 10`, and the Sync page shows `±10 ms · MFCC` for every row.
7. `git status --porcelain` after `go test ./...` shows no `.wav` file added to git (`backend/internal/dsp/mfcc/testdata/*.wav` is ignored).
8. No new dependency in `backend/go.mod` (MFCC is stdlib + the pinned `go-fft/fft`).

## Migration / compatibility

- **Nothing stored changes shape.** A job run with MFCC stores the same fields,
  with `precision_ms = 10` and `technique = "mfcc"`.
- **No settings migration:** the technique id already exists in the schema from
  slice 03; before this slice it would be refused by `Analyze` ("no engine"), and
  from this slice it runs. A DB written by slice 03 that already contains an
  `mfcc` entry therefore starts working on upgrade rather than erroring — worth a
  release-note line.
- **Old reports** decode unchanged; the UI's precision badge falls back to `—`.
- **Accuracy claim:** `offset_ms` is a multiple of 10 ms when MFCC answered. Any
  consumer that assumed 0.125 ms resolution for all reports must read
  `precision_ms`; that is exactly why it is stored (D3).

## Risks

| Risk | Mitigation |
|---|---|
| A hop-quantised offset is presented as precise. | `PrecisionMS` is part of the answer type, the report, the UI badge and the acceptance criteria. There is no code path that reports an MFCC offset without it. |
| MFCC agrees with waveform on clean audio but drifts on real content. | The committed-excerpt test measures exactly that on real audio, and the opt-in Shrek/Minions run reports both numbers. |
| The degraded fixture is not harsh enough to beat waveform. | The degradation is tuned in the generator until the assertion holds; the assertion is not relaxed. |
| Standardising per coefficient hides a genuine level change that matters for alignment. | Standardisation is the point (it removes encode gain), and the *envelope* contour survives in `c0`'s shape. |
| Memory: 13 coefficient streams + 64 k transform buffers per worker. | Under 10 MB per checkpoint versus the waveform matcher's ~500 MB; the 400 MB/worker model from `analyze.go` still over-estimates. |
| Two matchers disagree, and the pipeline picks the wrong one. | The winner rule is first-accepted in list order (slice 03); `maximum_accuracy` puts waveform first, so MFCC can only override a weak waveform answer. |

## Do not touch

- `backend/internal/dsp/correlate/**` — not one line. The waveform matcher is the
  parity baseline (`TestGoldenExcerpts`).
- `backend/internal/dsp/wav/**` — MFCC reads the slices the runner already hands
  it; it does not open files.
- `backend/internal/dsp/testdata/golden/**` — read-only input to the
  cross-matcher test.
- `backend/internal/analyze/plan.go` and the window geometry — MFCC consumes the
  same windows.
- `backend/go.mod` — no new dependency; `fpcalc`/chromaprint is rejected (#88).
- `schema.sql`, applied migrations, `docs/adr/**`.
