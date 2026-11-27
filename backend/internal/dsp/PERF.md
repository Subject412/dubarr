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
- `BenchmarkReferenceCheckpoint` — this host's floor for a checkpoint's
  irreducible work: two forward and one inverse `nfft` transform with the same
  options, on buffers that already exist, **plus one median extraction over a
  `[]float32` of 2 888 001 pseudo-random scores**. That is the exact length of a
  checkpoint's lag-score slice (`2*maxLag+1 = 3 039 999` candidates less the lags
  the overlap rule drops), unsorted and spread over `-1..1`, because sorting
  sorted input or a zeroed slice is a different and cheaper workload.

`TestCorrelationCostRatio` takes best-of-5 of each (the fastest run, never the
mean: everything a shared machine adds makes a run slower) and asserts

```
correlate <= K x reference
```

Both benchmarks and the test share the same measurement bodies, so the printed
numbers and the assertion can never drift apart.

## Why the reference carries the median

The first version of this gate compared the correlation with the three
transforms alone. That reference does not travel, and CI proved it:

| host | correlation | 3 transforms | ratio (gate `K = 4.3`) |
|---|---|---|---|
| AMD Ryzen 5 2500U (dev laptop) | 981 ms | 289 ms | 3.40, passed |
| GitHub Actions runner | 401 ms | 83 ms | **4.84, failed** |

Same code, same inputs. The transforms are vectorised and scale with CPU width:
3.5x between those two hosts. The correlation only gained 2.4x (981 → 401 ms),
because more than half of it is a memory-bound sort that a wider CPU does not
speed up: the non-transform part went 692 → 318 ms, 2.2x. A denominator that
shrinks faster than the numerator makes the ratio *rise* on a better machine, so
no `K` could be both meaningful on the laptop and green on CI — 4.3 is already
~+25 % over the laptop's own noise, while CI sits at 4.84. Raising `K` past 5
would have tolerated a 50 % regression on the machine the gate was calibrated
on. That is the definition of a gate that has stopped meaning anything.

The fix is to put the dominant term into the reference. With the median over a
lag-score-sized slice on both sides, both hosts' dominant operations appear in
numerator and denominator, and the ratio becomes a property of the code.

That it travels can be estimated from the two hosts, since the two terms scale
differently by a measured factor: on CI the transform is ~3.5x faster and the
memory-bound term ~2.2x faster. The new reference would therefore be
`83 + 663/2.2 ≈ 384 ms` there against a 401 ms correlation — **ratio ≈ 1.04**,
far inside `K = 1.6`. This is arithmetic over two measured hosts, not a CI
measurement; the point is that the sensitivity to CPU width has moved to a
second-order term (the ratio between the median and the transforms) instead of
being the gate's whole subject.

## Calibration

**Host:** AMD Ryzen 5 2500U with Radeon Vega Mobile Gfx (Raven Ridge, 4 cores /
8 threads), 8 logical CPUs, 4 MiB L3, Linux amd64.
**Toolchain:** go1.27.4, `github.com/go-fft/fft v0.49.0`.
**Date:** 2026-11-27.

Five runs of `TestCorrelationCostRatio` (best-of-5 inside each run):

| measurement | run 1 | run 2 | run 3 | run 4 | run 5 |
|---|---|---|---|---|---|
| correlation, one checkpoint | 1 188 ms | 1 015 ms | 1 074 ms | 1 094 ms | 1 095 ms |
| reference checkpoint | 965 ms | 995 ms | 945 ms | 1 012 ms | 1 022 ms |
| **ratio** | **1.23** | **1.02** | **1.14** | **1.08** | **1.07** |

Two further runs measured 1.19 and 1.21, so the spread is reproducible rather
than a one-off.

Benchmarks (`-benchtime 3x`, allocation picture):

| benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `BenchmarkCorrelateProduction` | 1 224 159 637 | 206 968 552 | 11 |
| `BenchmarkReferenceCheckpoint` | 1 129 682 143 | 134 592 618 | 6 |

Per term, measured separately on the same host and session (`-benchtime 3x`):

| term | ns/op | B/op | allocs/op |
|---|---|---|---|
| three transforms | 399 305 653 | 100 663 688 | 4 |
| median over 2 888 001 float32 | 662 552 344 | 11 558 912 | 1 |

The transform's 100 MB is its three `[]complex64` outputs (3 x 33.55 MB): the
`...With` functions allocate their result on every call and there is no
`NewPlan32With` in the pinned library (`internal/dsp/correlate/doc.go`). The
median's 11.6 MB is its `slices.Clone` of the score slice — the production
`peakProminence` clones 23 MB of `float64` for the same reason. The combined
reference's `B/op` is slightly above the sum of the two terms and varies with
`-benchtime` (134.6 MB at 3x, 139.1 MB at 5x); read it as a magnitude, not a
ledger.

**Committed gate:** observed 1.23 (worst of the five runs, so the spread between
runs is inside the budget), and

```
K = ceil(1.23 x 1.25 x 10) / 10 = 1.6
```

The 25 % headroom is narrower than the old gate's in effect, because the same
headroom now covers a much smaller asymmetry: what is left of it is the ratio
between the memory-bound median and the vectorised transforms, not the ratio
between all the memory-bound work and none of it.

## Where the time goes, and why the ratio is not 1.5-2.0

The port plan expected 1.5-2.0 from "two forward transforms, one inverse and a
3 M iteration scan" — a transform-and-scan model. The correlation is in fact
dominated by an `O(n log n)` median the plan did not price, and that is why the
transform-only ratio was 3.3-3.4. A CPU profile of `BenchmarkCorrelateProduction`
(8.11 s of samples over five correlations) attributes the time like this:

| term | cumulative | share |
|---|---|---|
| `peakProminence`'s sort (`slices.Clone` + pdqsort over ~2.9 M scores) | 4.13 s | 51 % |
| the transforms (`sk32Pass4AVX2` / `sk32Last4AVX2`) | 1.94 s | 24 % |
| everything else (prepare, `X*conj(Y)`, prefix sums, lag scan) | ~2 s | 25 % |

The median is taken over every scored lag — up to `2*maxLag+1` = 3.04 M values,
2 888 001 after the overlap rule drops the outermost lags — by cloning 23 MB and
sorting it. `correlation.rs:192-194` sorts too, so this is parity with the
reference engine, not port damage. Both of the two largest terms are now in the
reference, which is why the gate reads 1.2 instead of 3.4: the ~0.2 above 1.0 is
the 25 % tail — prepare, the product, the prefix sums and the lag scan.

Two consequences worth stating plainly:

1. The sort, not the lag scan, is the largest single term in a checkpoint. A
   future optimisation of the correlator should look there first (a selection or
   a histogram median returns the same number in `O(n)`) — but that is a
   behaviour-adjacent change and is out of scope for the port, which is
   committed to 1:1 arithmetic.
2. The gate is calibrated against the total, so it is tight where the risk is: a
   second clone-and-sort of the scores adds ~0.7x of the reference (`ratio` →
   ~1.9) and fails, and so does ~30 % more total cost. A regression confined to
   the lag scan alone would need to be ~3x to trip it, because the scan is a
   small part of the total.

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
  checkpoint, while this host measures 1.0-1.2 s per checkpoint (~6-7 s for six,
  i.e. under +2 % on the job).
- **Issue #89 (window-only decode) is where the real speedup lives** — about
  3.7x on the job, because it replaces two whole-track decodes with twelve short
  window seeks. Concurrency (U-DSP.7) buys almost nothing until that lands: it
  can at most remove the ~4-6 s of correlation from a 125 s job.
