// Package wav reads a frame range out of a RIFF/WAVE file as mono float32
// samples, seeking inside the `data` chunk so a job never loads the file.
//
// It is the Go port of `sync-engine/src/wav.rs` (`WavRangeReader`), and the
// behaviour it preserves is the fix for issue #22: the engine used to collect
// the whole file per checkpoint job — twice per job, once per source — so peak
// RSS grew with movie length. A `data` chunk is a flat PCM array, so a job only
// needs the frames of the window it analyses. Frame indices are sample-exact,
// so the analysed samples are identical to the former whole-file path and the
// reported offsets do not move.
//
// # Contract
//
//   - [Open] reads the RIFF/WAVE header only, validates the channel layout
//     (`1 <= channels <= 2`), and mirrors the encoding check at header time
//     instead of at first read.
//   - [RangeReader.ReadMono] decodes only the requested frames. A range that
//     runs past the end of the file is truncated, not an error — a short donor
//     tail must not fail a job. `frames == 0` (or a start at/after the end)
//     returns an empty slice.
//   - Encodings and divisors are hound's, exactly as `wav.rs:88-117` writes
//     them: 16-bit int `/32768`, 24-bit int `/8388608`, 32-bit int
//     `/2147483648`, 32-bit float passthrough. `WAVE_FORMAT_EXTENSIBLE` is
//     accepted when its sub-format tag is PCM (1) or IEEE float (3).
//   - Mixdown is mono copy, or the stereo arithmetic mean `(l + r) * 0.5`
//     evaluated in float32 — the expression at `wav.rs:132`. The bits match
//     only while it stays one f32 add followed by one multiply by a power of
//     two; folding frames into an f64 accumulator moves the last bit.
//
// # Errors
//
// Two typed values, matching the two shapes the bridge maps to `KindAnalysis`
// today (the third, `SliceOutOfRange`, belongs to the job layer):
//
//   - [Io] — the file could not be opened, or its RIFF/WAVE/chunk structure is
//     broken (bad magic, truncated header, no `fmt `/`data`). This is the same
//     mapping `wav.rs:41-44` uses: every `hound` error, `FormatError` included,
//     becomes `SyncError::Io{path, message}`.
//   - [UnsupportedWav] — the file is structurally fine but its layout or
//     encoding is outside what the engine reads: channel count other than 1-2,
//     or samples that are not 16/24/32-bit int or 32-bit float. `wav.rs:46-50`
//     reports the channel case the same way, and the encoding case carries the
//     same message wording (`wav.rs:113-117`).
//
// Both render the engine's text: `cannot read <path>: <msg>` and
// `unsupported WAV: <msg>` (`lib.rs:73-77`), so a job's recorded error string
// does not change shape after the port.
//
// The reader uses [os.File.ReadAt] with explicit offsets rather than a shared
// seek cursor, so one [RangeReader] may be read from several goroutines and no
// per-call position state exists to corrupt.
package wav
