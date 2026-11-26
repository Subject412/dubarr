# Correlation performance

What one correlation of a production checkpoint costs, what the guard on that
cost is, and what it means for a real sync job.

The gate lives in `correlate/perf_test.go` (`TestCorrelationCostRatio`) and is
measured by `correlate/bench_test.go`. It runs in the normal `backend` CI job:
no media, no toolchain, no skip.

## The gate

An absolute millisecond budget is the wrong instrument here. It is measured on
one machine, it fails on a busy runner, it gets marked flaky and then deleted.
This gate compares the correlator with **something the same machine measured in
the same process**:

- `BenchmarkCorrelateProduction` — one full checkpoint: 190 s at 8 kHz
  (`nx = ny = 1 520 000`, `nfft = 2^22`), `fft.Options{NoFMA: true}`, through a
  `Scratch` reused across checkpoints, which is the shipping configuration.
- `BenchmarkReferenceTransform` — two forward and one inverse `nfft` transform
  with the same options, on buffers that already exist: this host's floor for
  the transform work a correlation cannot avoid.

`TestCorrelationCostRatio` takes best-of-5 of each (the fastest run, never the
mean: everything a shared machine adds makes a run slower) and asserts

```
correlate <= K x reference
```

Both benchmarks and the test share the same measurement bodies, so the printed
numbers and the assertion can never drift apart.

## Calibration

**Host:** AMD Ryzen 5 2500U with Radeon Vega Mobile Gfx (Raven Ridge, 4 cores /
8 threads), 8 logical CPUs, Linux amd64.
**Toolchain:** go1.27.4, `github.com/go-fft/fft v0.49.0`.
**Date:** 2026-11-27.

| measurement | run 1 | run 2 | run 3 |
|---|---|---|---|
| correlation, one checkpoint | 984 ms | 993 ms | 1002 ms |
| three transforms, `nfft = 2^22` | 295 ms | 295 ms | 293 ms |
| **ratio** | **3.33** | **3.37** | **3.42** |

Benchmarks (`-benchtime 3x`, for the allocation picture):

| benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `BenchmarkCorrelateProduction` | 1 055 229 460 | 206 968 976 | 12 |
| `BenchmarkReferenceTransform` | 327 148 703 | 100 663 688 | 4 |

The reference's 100 MB is its three `[]complex64` outputs (3 x 33.55 MB): the
`...With` functions allocate their result on every call and there is no
`NewPlan32With` in the pinned library (`internal/dsp/correlate/doc.go`).

**Committed gate:** observed 3.42 (worst of the three runs, so the spread between
runs is inside the budget), and

```
K = ceil(3.42 x 1.25 x 10) / 10 = 4.3
```

The 25 % headroom is for the difference a runner may legitimately show: the
median sort and the transform kernel react differently to a smaller L3 and to a
shared memory bus.

## Where the time goes, and why the ratio is not 1.5-2.0

The port plan expected 1.5-2.0 — "two forward transforms, one inverse and a 3 M
iteration scan". The measured ratio is 3.3-3.4, and it is **not** a regression.
A CPU profile of `BenchmarkCorrelateProduction` (8.11 s of samples over five
correlations) attributes the time like this:

| term | cumulative | share |
|---|---|---|
| `peakProminence`'s sort (`slices.Clone` + pdqsort over ~2.9 M scores) | 4.13 s | 51 % |
| the transforms (`sk32Pass4AVX2` / `sk32Last4AVX2`) | 1.94 s | 24 % |
| everything else (prepare, `X*conj(Y)`, prefix sums, lag scan) | ~2 s | 25 % |

The median is taken over every scored lag — up to `2*maxLag+1` = 3.04 M values,
about 2.9 M after the overlap rule drops the outermost lags — and it is taken by
cloning 23 MB and sorting it. That is ~800 ms per checkpoint, against ~295 ms
for all three transforms. `correlation.rs:192-194` sorts too, so this is parity
with the reference engine, not port damage: the plan simply did not price an
`O(n log n)` median over three million lags.

Two consequences worth stating plainly:

1. The sort, not the lag scan, is the largest single term in a checkpoint. A
   future optimisation of the correlator should look there first (a selection or
   a histogram median returns the same number in `O(n)`) — but that is a
   behaviour-adjacent change and is out of scope for the port, which is
   committed to 1:1 arithmetic.
2. The gate is calibrated against the total, so it is tight where the risk is:
   a second clone-and-sort of the scores (~1.7x) fails, and so does ~25 % more
   total cost. A regression confined to the lag scan alone would need to be ~3x
   to trip it.

## What this gate does not catch

Cost is cost whatever the answer is. This file guards **cost**; the goldens guard
**correctness**.

- Removing the overlap guard (`count < minOverlap`) makes the scan *cheaper* and
  still passes.
- Adding a `1/nfft` multiply after the inverse costs one extra pass and still
  passes.

Those are exactly the two mistakes that matter most (an overlap-guard removal
narrows the search silently; a `1/nfft` collapses `quality` by six orders of
magnitude). They are caught by `TestAutocorrelationPeakIsOne` and
`TestGoldenExcerpts`, never here.

## What it costs a real job

The correlation number above is **per checkpoint**, not per job. A job runs six
checkpoints, and U-DSP.7 runs several at once and will make this gate N-aware.

Measured on the real Shrek pair (both tracks 5431 s), with the corrected job
model — `decodeAudio` decodes each whole track **once**, not once per
checkpoint:

| stage | Rust today | Go after the port |
|---|---|---|
| target full decode (8 kHz mono f32) | 86.2 s | 86.2 s |
| donor full decode (staged `.mka`) | 34.9 s | 34.9 s |
| correlation, 6 checkpoints | 4.1 s | 9.1 s |
| **job total** | **125.2 s** | **130.2 s** |

So the port costs **+4.0 %** on a job, not the +19 % an earlier per-checkpoint
model suggested. Two caveats, both load-bearing:

- **Decode is 97 % of the job** (121 s of 125 s). Correlation is not where a job's
  time is, so no amount of correlator tuning changes what an operator waits for.
  This measurement is also the conservative end: the 9.1 s figure is 1.5 s per
  checkpoint, while this host measures 0.99 s per checkpoint (~5.9 s for six,
  i.e. +1.4 % on the job).
- **Issue #89 (window-only decode) is where the real speedup lives** — about
  3.7x on the job, because it replaces two whole-track decodes with twelve short
  window seeks. Concurrency (U-DSP.7) buys almost nothing until that lands: it
  can at most remove the ~4 s of correlation from a 125 s job.
