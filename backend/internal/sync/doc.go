// Package sync holds dubbing-sync job orchestration and the Rust engine contract.
//
// The DSP engine itself is the Rust binary `dubarr-sync` (sync-engine/). This
// package is the Go side of that bridge: it locates the binary, serialises a
// [Job] to JSON on stdin, runs the engine under a context deadline and decodes
// the single JSON object it prints on stdout.
//
// The JSON shapes in types.go mirror sync-engine/src/job.rs (`Job`, `Source`,
// `PcmSlice`, `SyncResult`, `Analyzed`) field for field. Renaming a field on one
// side requires the same rename on the other.
package sync
