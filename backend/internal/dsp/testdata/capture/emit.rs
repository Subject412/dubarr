//! Golden-capture emitter for U-DSP.1 — synthetic tier.
//!
//! This file is **not** part of the `sync-engine` test suite and must never be
//! added to it. The capture tool next to it (`main.go`) copies `sync-engine/`
//! into a scratch directory, drops this file in as `tests/emit.rs`, appends a
//! `[[test]] name = "emit"  harness = false` stanza to the *copied* manifest and
//! runs
//!
//! ```text
//! cargo test --offline --release --test emit -- --out <scratch>/synthetic
//! ```
//!
//! Because the copied `tests/emit.rs` sits beside the harness's own
//! `tests/common/mod.rs`, this emitter calls `common::wav_case` and the exact
//! body of `tests/golden.rs::stereo_target_is_mixed_down`. That is the whole
//! point: the committed WAVs are the *harness's* generator output, not a Go
//! re-implementation of it, so no generator drift can make the goldens
//! meaningless.
//!
//! Layout written under `--out`:
//!
//! ```text
//! <case>/target.wav   the harness's own file, untouched
//! <case>/donor.wav
//! <case>/job.json     the harness's payload (absolute scratch paths)
//! ```
//!
//! The analyser only ever reads `[slice_start_sec, slice_start_sec+slice_len_sec)`
//! of each file, so the capture tool trims the committed copies to that region
//! (see `GOLDEN.md`); the samples it analyses are byte-identical either way.

mod common;

use std::path::{Path, PathBuf};

/// The five `wav_case` scenarios of `tests/golden.rs`.
const WAV_CASES: [(&str, i64, f64, f64, f64, Option<f64>); 5] = [
    // name, offset_ms, slice_start_sec, slice_len_sec, window_sec, snr_db
    ("pos1500", 1_500, 5.0, 10.0, 5.5, None),
    ("neg800", -800, 5.0, 10.0, 5.5, None),
    ("pos5s", 5_000, 5.0, 10.0, 5.0, None),
    ("neg5s", -5_000, 5.0, 10.0, 5.0, None),
    ("noise6db", 1_500, 5.0, 10.0, 5.5, Some(6.0)),
];

fn main() {
    let out = parse_out();
    std::fs::create_dir_all(&out).expect("create output dir");

    for (name, offset_ms, start, len, window, snr) in WAV_CASES {
        let dir = out.join(name);
        std::fs::create_dir_all(&dir).expect("create case dir");
        let payload = common::wav_case(&dir, 8_000, offset_ms, start, len, window, snr);
        std::fs::write(dir.join("job.json"), payload).expect("write job.json");
        eprintln!("emit: {name} -> {}", dir.display());
    }

    // Verbatim body of tests/golden.rs::stereo_target_is_mixed_down (lines
    // 77-96): 20 s programme shifted by 1000 samples, independent L/R noise,
    // so averaging down must still correlate with the mono i16 donor.
    let dir = out.join("stereo");
    std::fs::create_dir_all(&dir).expect("create case dir");
    let sr = 8_000u32;
    let base = common::make_base(sr, 20.0, 0x5157_4445);
    let target = common::shift(&base, 1_000 * sr as i64 / 1000);
    let mut left = target.clone();
    let mut right = target.clone();
    common::add_noise(&mut left, 30.0, 0x1111);
    common::add_noise(&mut right, 30.0, 0x2222);
    let target_path = dir.join("target.wav");
    let donor_path = dir.join("donor.wav");
    common::write_wav_stereo_f32(&target_path, sr, &left, &right);
    common::write_wav_mono_i16(&donor_path, sr, &base);
    let payload = serde_json::json!({
        "target": { "path": target_path },
        "donor": { "path": donor_path },
        "slice_start_sec": 5.0,
        "slice_len_sec": 10.0,
        "window_sec": 2.0,
    })
    .to_string();
    std::fs::write(dir.join("job.json"), payload).expect("write job.json");
    eprintln!("emit: stereo -> {}", dir.display());
}

fn parse_out() -> PathBuf {
    let mut args = std::env::args().skip(1);
    let mut out: Option<PathBuf> = None;
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--out" => out = args.next().map(PathBuf::from),
            other => panic!("emit: unknown argument {other:?}; usage: emit --out <dir>"),
        }
    }
    let out = out.expect("emit: --out <dir> is required");
    if !is_dir_or_new(&out) {
        panic!("emit: {out:?} is not a usable directory");
    }
    out
}

fn is_dir_or_new(path: &Path) -> bool {
    path.is_dir() || path.parent().map(Path::is_dir).unwrap_or(false)
}
