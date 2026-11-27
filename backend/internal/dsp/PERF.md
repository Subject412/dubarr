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
taken **as the port landed** (8.11 s of samples over five correlations)
attributes the time like this:

| term | cumulative | share |
|---|---|---|
| `peakProminence`'s sort (`slices.Clone` + pdqsort over ~2.9 M scores) | 4.13 s | 51 % |
| the transforms (`sk32Pass4AVX2` / `sk32Last4AVX2`) | 1.94 s | 24 % |
| everything else (prepare, `X*conj(Y)`, prefix sums, lag scan) | ~2 s | 25 % |

The median is taken over every scored lag — up to `2*maxLag+1` = 3.04 M values,
2 888 001 after the overlap rule drops the outermost lags. `correlation.rs:192-194`
sorts too, so this was parity with the reference engine, not port damage.

### The sort is gone (#95)

The port was committed to 1:1 arithmetic, so the sort stayed; #95 then replaced
it with an exact introselect (median-of-three Hoare partition, insertion sort
under 16, heap selection after `2*log2(n)` partitions). It returns **the same
order statistic** — the upper middle, `sorted[len/2]` — so no reported number
moved: all goldens still match with exact offsets and `|Δquality| <= 0.0002`.

| | before #95 | after #95 |
|---|---|---|
| median | ~800 ms (clone 23 MB + pdqsort) | ~130 ms (in-place selection) |
| correlation, one checkpoint | ~981 ms | 421-425 ms (three runs) |
| reference checkpoint (still contains the sort) | 894-910 ms | unchanged |
| `TestCorrelationCostRatio` | 1.2 | **0.47** |

`costRatioK` was recalibrated 1.6 -> 0.75 for this. The reference deliberately
still contains the sort: it is a fixed baseline, so the ratio falling to 0.47
*is* the saving. K sits above the measured 0.47 by 60 % — this gate's own
history records a 42 % spread between this host and CI for identical code — and
below 1.0, which is where production lands if the sort ever comes back. So the
gate fails a regression to sorting long before the absolute time does.

Two consequences worth stating plainly:

1. The sort was the largest single term in a checkpoint, and removing it made
   the correlator ~2.3x cheaper than the Rust engine's 687 ms on the same
   checkpoint, rather than 1.4x more expensive. What is left is dominated by
   the transforms, which is the shape the plan originally assumed.
2. The gate is calibrated against the total, so it is tight where the risk is: a
   return to sorting the ~2.9 M scores fails, and so does ~60 % more total cost.
   A regression confined to the lag scan alone would need to be large to trip
   it, because the scan is a small part of the total.

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

## Parallel checkpoints (U-DSP.7): what one worker costs

`analyze.Analyze` can run N checkpoints of one job at once
(`Config.ParallelCheckpoints`, clamped to 1..8, then to the checkpoint count,
then reduced by the memory guard below — see
[The limit is the container's](#the-limit-is-the-containers-not-the-hosts)).
Everything here is measured, not summed, because the buffer sum is misleading:
`X`, `Y`, the inverse output and one pooled scratch (4 x 33.55 MB) plus two f32
slices and two f64 prefix arrays is ~171 MB of *live* buffers, and a checkpoint
also clones 23 MB of `float64` scores for the median — but the resident peak is
what the collector lets the heap reach, which is a larger number.

**Method.** `BenchmarkParallelCheckpoints` (`internal/analyze`, real engine,
`-benchtime 1x`) analyses the settled six checkpoints of 600 s / 8 kHz WAVs and
takes `/proc/self/status` `VmHWM` immediately before and after the analysis.
`VmHWM` is a high-water mark that only rises, so each worker count is measured
in its own process:

```
go test ./internal/analyze -run '^$' -bench 'ParallelCheckpoints/n=1$' -benchtime 1x -v
go test ./internal/analyze -run '^$' -bench 'ParallelCheckpoints/n=4$' -benchtime 1x -v
```

The benchmark substitutes a large effective limit through the seam, so it
measures the cost of N workers rather than what the host allows.

**Host:** AMD Ryzen 5 2500U (4 cores / 8 threads), 8 GB RAM, MemAvailable
4.1 GB, go1.27.4, GOGC default. **Date:** 2026-11-27.

| workers | VmHWM base | VmHWM peak | delta/job | per worker | wall clock, 6 checkpoints |
|---|---|---|---|---|---|
| 1 | 44.6 MiB | 554.8-560.9 MiB | 510.6-516.3 MiB | **495-516 MiB** | 7.07-7.12 s |
| 4 | 44.0-45.8 MiB | 1 412-1 655 MiB | 1 367-1 609 MiB | **342-402 MiB** | 3.99-4.51 s |

(Two to three runs per row; `VmHWM` deltas of this size vary ~15 % between
processes because they inherit the collector's heap sizing, which is why the
ranges are given rather than a single figure.)

Two readings from the table:

1. **A worker costs ~0.5 GiB at n=1, not 171 MB**, and the gap is the
   collector, not a leak — see [Measured RSS is 3x the live
   buffers](#measured-rss-is-3x-the-live-buffers).
2. **The cost is not linear in N.** Per-worker drops to 342-402 MiB at n=4
   because four workers share one collector cycle instead of four heap
   doublings, so the process's own footprint (44-46 MiB `VmHWM` before the
   analysis) is charged to the single worker at n=1.

### The cost model: a base plus a per-worker term

A flat per-worker constant cannot describe both rows: charging the n=1 figure
per worker over-charges small N and under-charges large N, which is backwards
for an OOM guard. Two terms fit both measured points:

```
total(n) = base + n x perWorker

perWorker = (1488 - 513) / 3 = 325 MiB      -> rounded up to 400 MiB
base      =  513 - 325       = 188 MiB      -> rounded up to 200 MiB
```

using the central values of the two rows (513 MiB at n=1, 1488 MiB at n=4). The
constants are `analyze.CheckpointFixedBytes = 200 MiB` and
`analyze.CheckpointBytesPerWorker = 400 MiB`, both rounded up so the model is
never optimistic. What it charges:

| workers | modelled | measured |
|---|---|---|
| 1 | 600 MiB | 511-516 MiB |
| 2 | 1000 MiB | not measured |
| 4 | 1800 MiB | 1367-1609 MiB |
| 8 | 3400 MiB | not measured (extrapolated) |

`analyze.checkpointBytes(n)` and `TestCheckpointCostModel` /
`TestWorkerClampForContainerSizes` pin these numbers, so this table cannot drift
from the code.

### The limit is the container's, not the host's

The guard's budget is **half of the effective memory limit**, where the
effective limit is resolved in this order:

1. cgroup v2 `/sys/fs/cgroup/memory.max` (the literal `max` means unlimited);
2. cgroup v1 `/sys/fs/cgroup/memory/memory.limit_in_bytes` (a value within
   reach of 2^63 is the kernel's "unlimited" sentinel);
3. `/proc/meminfo` `MemAvailable`.

When both a cgroup limit and `MemAvailable` are readable the **smaller** wins.
Reading only `MemAvailable` would be a bug in the deployment that matters:
`MemAvailable` describes the *host*, so a 4 GB dubarr container on a 128 GB
TrueNAS box would report ~120 GB, the guard would permit all 8 workers, and the
container would be OOM-killed — exactly what the guard exists to prevent. Inside
a container, the cgroup limit is the ceiling that kills.

### What the clamp yields

| container | effective limit | budget (half) | workers by budget | workers, settled six |
|---|---|---|---|---|
| 1 GiB | 1 GiB | 512 MiB | 1 (floor) | 1 |
| 2 GiB | 2 GiB | 1024 MiB | 2 (1000 MiB) | 2 |
| **4 GiB** | **4 GiB** | **2048 MiB** | **4 (1800 MiB)** | **4** |
| 6 GiB | 6 GiB | 3072 MiB | 7 (3000 MiB) | 6 |
| 16 GiB | 16 GiB | 8192 MiB | 8 (3400 MiB, hard bound) | 6 |

So on the **4 GB deployment target** the setting of 8 is reduced to **4**
workers: 5 would model 2200 MiB against a 2048 MiB budget. **Four workers need a
container of 2 x 1800 MiB = 3600 MiB (~3.5 GiB)**, so a 4 GB container is that
plus ~0.5 GiB of slack for the runtime, the rest of dubarr and its ffmpeg
children — which is the reason for the 0.5 factor, not wasted headroom. The
reduction is logged once, as
`analyze: parallel checkpoints reduced 8 → 4 (memory budget)`.

The checkpoint count is a ceiling of its own: the settled policy has six, which
is why 6 GiB and up land on six even though the budget would allow 7 and 8. The
budget is what decides at 4 GiB and below — the sizes that matter for a
container.

These are the numbers the measurement gives; they were not tuned to make a
particular container size fit. A smaller container clamps harder, monotonically:
the `0.5 x limit` rule and the floor of one worker are the whole policy.

### Measured RSS is 3x the live buffers

The plan estimated ~171 MB of live buffers per checkpoint and the process
measures 511-516 MiB of RSS for the same work. That factor of ~3 is Go's
garbage-collector headroom, not a leak and not a bug. With `GOGC` at its default
100, the heap is allowed to grow to roughly twice the live set before a cycle
runs; each checkpoint allocates its ~207 MB of buffers (three 33.55 MB
`[]complex64` transform outputs, the inverse output, the pooled scratch) and
clones 23 MB of `float64` scores for the median, then drops most of it, and the
peak lands near twice the live set rather than at it. The buffers are freed —
the memory is retained for reuse — which is why VmHWM, not the buffer sum, is
the number that decides whether a container survives. That is why the guard
charges RSS.

The gap can be narrowed, and both ways are future options rather than part of
this slice: a lower `GOGC` (or `debug.SetMemoryLimit`) trades CPU for headroom,
and reusing the transform outputs instead of the `...With` allocations
(`correlate/doc.go` explains why the pinned library forces the allocation)
removes the churn that makes the heap double in the first place. Either changes
timing and allocation behaviour, so neither belongs in a parallelism change.

**What the setting buys today: little.** Six checkpoints take 7.1 s at n=1 and
~4.3 s at n=4 on this host — 1.6x on the correlation, which is a
memory-bandwidth-bound workload on a 4-core laptop, not 4x. Against a 125 s job
that is ~4.5 s saved of 125 s: correlation is only ~3 % of a job because decode
is 97 %, so the parallelism setting's payoff arrives with #89 (window-only
decode), not now. It is in the tree now because the report is
order-independent by construction and because #87's requirement was about one
job using several cores, which is exactly what this does: N concurrent
checkpoint reads and correlations, never N jobs and never N ffmpeg decodes.

## Windowed decode (#89): what six checkpoints cost when they do not read the whole film

Measured by `internal/acceptance`'s `TestWindowedParity`, which runs one real
job per mode on each film through the same `Execute` the worker calls: six
checkpoints, 190 s windows, 2.0 s pre-roll, `lowpass_fallback` band, one host,
serially so the wall clocks are not competing with each other.

| fixture | mode | wall clock | audio decoded | seeks |
|---|---|---|---|---|
| Shrek | full | 106.033 s | 10 863 s | 0 |
| Shrek | windows | 30.056 s | 2 304 s | 12 |
| Minions | full | 99.470 s | 10 787 s | 0 |
| Minions | windows | 27.228 s | 2 304 s | 12 |

That is 3.5x on Shrek and 3.6x on Minions, and 4.7x less audio read. The
decoded-seconds column is the mechanism: six 190 s windows plus 2 s of pre-roll
on each side is 2 304 s of audio whether the film is 90 minutes or three hours,
while the whole-track path reads every second of both tracks twice.

The saving is bounded by what the job does besides decode. Earlier measurement
put decode at ~97 % of a full job and correlation at ~3 %, which is why the
parallel-checkpoint setting above was worth little on its own; removing the
decode is what that 97 % was waiting for.

The wall clocks include the engine's correlation of six 190 s windows, so they
are job-shaped rather than decode-only. The two modes produce the same verdict,
the same checkpoint plan, |Δquality| 0.000 and |Δmax_drift| 0.000 on both
films. Offsets agree to 0.000 ms on Shrek and differ by a constant 1.000 ms on
Minions — one Matroska timestamp tick, explained at
`windowedCrossPathOffsetMS` in `internal/acceptance/windowed_test.go`, and
constant across all six checkpoints, so it cannot move drift or a 40 ms
threshold.
