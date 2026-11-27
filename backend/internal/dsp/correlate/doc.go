// Package correlate finds the sample offset between two audio slices with
// normalised FFT cross-correlation, in float32.
//
// It is the Go port of `sync-engine/src/correlation.rs`, and it is a port, not
// a redesign: the same zero padding, the same lag scan, the same overlap rule
// and the same quality formula, so the offsets and qualities recorded from the
// reference engine are reproduced (see `TestGoldenExcerpts` and
// `internal/dsp/testdata/GOLDEN.md`).
//
// # Normalisation: the one number that must not be "fixed"
//
// A linear correlation `c[k] = sum_n x[n] * y[n-k]` is computed as
// `IFFT(FFT(x) * conj(FFT(y)))` after zero-padding to at least `nx+ny-1`
// points. The two libraries disagree about the inverse: `rustfft`'s is
// unnormalised, so `correlation.rs:123-126` multiplies it by `1/nfft`; go-fft's
// `IFFT32` already divides by `n` (`plan32.go`, `IFFTNorm` with the default
// `NormBackward`). The port therefore drops the Rust scale line instead of
// transcribing it. Keeping it would divide every lag by `nfft` twice, which
// leaves the argmax alone but collapses the peak: at `nfft = 2^22` the
// normalised cross-correlation would land near `2.4e-7`, `Quality` would be
// `~1e-7` for every input, and every verdict in `internal/analyze` would read
// as unreliable. There is no post-inverse scaling anywhere in this package, and
// `TestAutocorrelationPeakIsOne` is the guard.
//
// The DC removal and unit-RMS scaling in `prepare` is a different
// normalisation and stays: mean and RMS are accumulated in float64 from the
// float32 samples, `1/rms` is rounded once to float32, and the samples are
// scaled in float32 — the arithmetic of `correlation.rs:23-40`, including the
// `rms <= 1e-9` → `SilentSlice` rejection.
//
// The second guard §3.1 asks for ("the quality stays in the Rust band") is
// `TestGoldenExcerpts`: it asserts each recorded quality within ±0.02 of the
// reference engine's value, which is stricter than any absolute band and cannot
// paper over the weak Minions checkpoints by adding a floor.
//
// # Confidence: two numbers, one verdict
//
// A correlation reports two confidence numbers and one precision, and they do
// different jobs:
//
//   - [Analysis.Quality] is the peak's prominence over the median lag response,
//     `(peak - median)/(1 - median)`. It is the 0.4.0 number: its formula, its
//     value and its stored key are unchanged, and `internal/analyze` keeps
//     reading it as the low-quality floor. Prominence is not comparable across
//     content — a quiet dub and a loud remaster of the same scene land in
//     different bands — so it cannot be the only word.
//   - [Analysis.MatchZ] is the peak's distance from the window's own lag
//     distribution, `(peak - mean)/std` over every scored lag (the BBC
//     `audio-offset-finder` form cited by #85). It is scale-free, so the same
//     band means the same thing on a quiet dub and a loud one. It is computed
//     by [matchZ] from `sum` and `sumSq` accumulated inside the lag scan: no
//     second pass and no second allocation, which is what keeps the cost gate
//     and the 400 MiB/worker memory model in `internal/analyze` untouched.
//
// D2 asked for a "robust z / MAD-based" figure. The estimator here is mean/std
// instead, for one measured reason: a median absolute deviation needs a second
// pass over the same ~2.9 M scores *and* a second order-statistic selection at
// nfft = 2^22, i.e. a ~23 MB allocation and two extra O(n) passes per
// checkpoint, against a reference workload the cost gate prices with a single
// median extraction. That is a real risk to a gate that must not be loosened
// for a slice's convenience, for a robustness gain the mean/std form does not
// need on real content. Keeping the estimator in [matchZ] makes a switch to MAD
// a one-function change if the operator asks for it.
//
// [Analysis.OffsetSubSamples] is the third new number and it is diagnostics,
// not a decision: a parabola through the three lag scores around the peak gives
// a fractional offset, reported as `offset_sub_ms` and never applied. The
// applied offset stays `round(OffsetSamples * 1000 / rate)` — one sample at the
// analysis rate ([Analysis.PrecisionMS], 0.125 ms at 8 kHz) — because the
// consumer is `mkvmerge --sync 0:<integer ms>`. Its neighbours are captured
// during the lag scan, never looked up afterwards: [peakProminence] reorders
// `scores` in place, so after a correlation that slice is unordered and must
// not be indexed by lag.
//
// The verdict itself — which bands count as reliable — lives in exactly one
// place, `analyze.DefaultThresholds`, and the report echoes the table that
// judged it. This package only measures.
//
// # Arithmetic width
//
// Samples, spectra and the `X * conj(Y)` product are float32, because float32
// is the reference engine's width: that is what makes the recorded qualities
// land within tolerance. The accumulations Rust does in float64 stay float64
// here — mean, RMS, prefix energy, the NCC division, the median.
//
// Every transform passes `fft.Options{NoFMA: true}`. The float32 transforms do
// honour `NoFMA` (`fuseds.go:20`: `fusedS32` is false when it is set), which
// selects the unfused passes; the bare `fft.FFT32` takes the fused kernel
// wherever `fusedSEnabled[n]` is set, and those bits depend on whether the CPU
// has AVX2+FMA3. Reproducible goldens across machines are worth more than the
// fused kernel's margin, so the slower, unfused path is deliberate.
//
// Go may fuse some float32/float64 expressions on arm64. That cannot move a
// golden beyond its parity tolerance — the goldens are tolerance-based by
// design, never bit equality.
//
// # Buffers
//
// There is no `fft.NewPlan32With`, so the unfused path is only reachable
// through the `...With` functions, and those copy their input into a freshly
// allocated `[]complex64` of `nfft` on every call (33.55 MB at `nfft = 2^22`).
// A single 190 s checkpoint at 8 kHz therefore holds, at its peak: two spectra,
// the inverse output and one padded input, plus the two prepared float32
// slices and their two float64 prefix arrays — about 171 MB.
//
// The padded inputs are released one at a time — each as soon as its own
// spectrum exists, never held as a third copy alongside the spectra — and a
// [Scratch] recycles the `nfft`-sized buffers between checkpoints of the same
// length, so a six-checkpoint job does not allocate two fresh 33.55 MB inputs
// per checkpoint. The numbers do not change with or without a pool, and
// `TestScratchReuseKeepsNumbersIdentical` says so. Note what a pool cannot do:
// the `...With` functions still allocate their output on every call, so the
// three spectra of a checkpoint are garbage either way — that is the 171 MB
// floor of §3.4, not a missing optimisation.
//
// # Cancellation
//
// The scan checks `ctx.Err()` before the first transform, after each transform
// and every 4096 lags. A cancelled context returns a wrapped `context.Canceled`
// (or `context.DeadlineExceeded`), never a partial [Analysis]. The worst-case
// overrun after a cancellation is one transform of `nfft` points.
package correlate
