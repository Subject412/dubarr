// Package sync runs one dubbing-sync correlation job in-process.
//
// The DSP engine used to be the Rust binary `dubarr-sync`; it is now
// internal/dsp, compiled into the server. This package is the job layer above
// it: it opens the staged WAVs, slices the requested window from each,
// correlates through internal/dsp/correlate and builds the [Result] the
// analysis report is written from.
//
// Two guarantees survive the move from a child process to a call:
//
//   - the job deadline is still enforced, and an expired deadline is
//     [ErrTimeout] (cooperatively — see [Runner.Run] for the worst-case
//     overrun), while an ordinary cancellation is [ErrExecution];
//   - a panic below this layer cannot take the server down: [Runner.Run]
//     recovers it into a typed [ErrExecution] and the next job still runs.
//
// Matching the reference engine is the contract: the window, the truncation at
// each file's end, the 64-sample minimum and the three-decimal rounding of
// `offset_ms` and `correlation_quality` are all job.rs's behaviour, not
// choices made here.
package sync
