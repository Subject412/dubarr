//! Issue #22 regression: the new frame-ranged WAV read must reproduce the
//! former whole-file-decode-then-slice path on a long input.
//!
//! The former behaviour is re-implemented inline here (full decode via
//! `read_wav_mono`, then a manual slice) so the numbers it produced are pinned
//! before the ranged path is compared against them. Offsets must agree within
//! ±1 ms and the correlation quality must not regress.

mod common;

use common::{make_base, shift, write_wav_mono_i16, TempDir};
use dubarr_sync::wav::read_wav_mono;
use dubarr_sync::{analyze, run_job, Job, Source};

const SAMPLE_RATE: u32 = 8_000;
const DURATION_SEC: f64 = 600.0;
const WINDOW_SEC: f64 = 30.0;
/// True target/donor displacement. Integer samples at 8 kHz, so the pinned
/// legacy offsets are exactly 1500.0 ms.
const LEAD_MS: f64 = 1_500.0;
/// Production checkpoint positions (`analyze.DefaultCheckpointPositionsPct`).
const CHECKPOINT_POSITIONS_PCT: [f64; 6] = [10.0, 25.0, 40.0, 55.0, 70.0, 85.0];

/// Window geometry exactly as `backend/internal/analyze` derives it: the
/// checkpoint percentage is the window centre, clamped at the file start.
fn window_for(pct: f64) -> (f64, f64) {
    let center = DURATION_SEC * pct / 100.0;
    let start = (center - WINDOW_SEC / 2.0).max(0.0);
    let len = WINDOW_SEC.min(DURATION_SEC - start);
    (start, len)
}

/// Former implementation: whole file in memory, then slice the window.
fn legacy_offset_ms(target: &[f32], donor: &[f32], start_sec: f64, len_sec: f64) -> f64 {
    let start = (start_sec * SAMPLE_RATE as f64).round() as usize;
    let len = (len_sec * SAMPLE_RATE as f64).round() as usize;
    let end = start.saturating_add(len).min(target.len());
    let analysis = analyze(
        &target[start..end],
        &donor[start..end],
        SAMPLE_RATE,
        WINDOW_SEC,
    )
    .expect("legacy analyze");
    analysis.offset_samples as f64 * 1_000.0 / SAMPLE_RATE as f64
}

#[test]
fn ranged_reads_match_the_former_whole_file_path() {
    let dir = TempDir::new("slice-window");
    let base = make_base(SAMPLE_RATE, DURATION_SEC, 0xB0BA_2022);
    let lead_samples = (LEAD_MS / 1000.0 * SAMPLE_RATE as f64).round() as i64;
    let target_samples = shift(&base, lead_samples);
    let target_path = dir.path().join("target.wav");
    let donor_path = dir.path().join("donor.wav");
    write_wav_mono_i16(&target_path, SAMPLE_RATE, &target_samples);
    write_wav_mono_i16(&donor_path, SAMPLE_RATE, &base);

    // Reference data: the full decode the engine used to perform per job.
    let target_full = read_wav_mono(&target_path).expect("read target").samples;
    let donor_full = read_wav_mono(&donor_path).expect("read donor").samples;
    assert_eq!(target_full.len(), base.len() + lead_samples as usize);

    for &pct in CHECKPOINT_POSITIONS_PCT.iter() {
        let (start, len) = window_for(pct);

        // Pinned former numbers first: any drift in the harness shows up here
        // rather than being silently absorbed by the comparison below.
        let legacy = legacy_offset_ms(&target_full, &donor_full, start, len);
        assert!(
            (legacy - LEAD_MS).abs() <= 1.0,
            "legacy offset at {pct}% = {legacy} ms, pinned expectation {LEAD_MS} ±1 ms"
        );

        let result = run_job(&Job {
            target: Source::Path {
                path: target_path.clone(),
            },
            donor: Source::Path {
                path: donor_path.clone(),
            },
            slice_start_sec: start,
            slice_len_sec: len,
            window_sec: Some(WINDOW_SEC),
        })
        .expect("ranged job");

        assert!(
            (result.offset_ms - legacy).abs() <= 1.0,
            "offset moved at {pct}% ({start} s + {len} s): ranged {} ms vs legacy {legacy} ms",
            result.offset_ms
        );
        assert!(
            result.correlation_quality >= 0.99,
            "quality regressed at {pct}%: {}",
            result.correlation_quality
        );
        assert_eq!(result.analyzed.start, start);
        assert_eq!(result.analyzed.len, len);
    }
}

/// The ranged reader must also return exactly the same samples as a full read
/// for every supported encoding and channel layout, including a seek past the
/// first buffer (`BufReader` boundary).
#[test]
fn ranged_read_is_sample_exact_for_every_encoding() {
    let dir = TempDir::new("slice-exact");
    let samples = make_base(8_000, 12.0, 0x5EED);
    let start = 5_000u32; // well past the 8 KiB buffer
    let frames = 3_000u32;

    for &(bits, format) in &[(16u16, "int"), (24, "int"), (32, "int"), (32, "float")] {
        let path = dir.path().join(format!("{bits}-{format}.wav"));
        let spec = hound::WavSpec {
            channels: 1,
            sample_rate: 8_000,
            bits_per_sample: bits,
            sample_format: if format == "float" {
                hound::SampleFormat::Float
            } else {
                hound::SampleFormat::Int
            },
        };
        let mut writer = hound::WavWriter::create(&path, spec).expect("create wav");
        for &s in &samples {
            match (bits, format) {
                (16, "int") => writer
                    .write_sample((s.clamp(-1.0, 1.0) * i16::MAX as f32).round() as i16)
                    .expect("w"),
                (24, "int") => writer
                    .write_sample((s as f64 * 8_388_607.0).round() as i32)
                    .expect("w"),
                (32, "int") => writer
                    .write_sample((s as f64 * 2_147_483_647.0).round() as i32)
                    .expect("w"),
                _ => writer.write_sample(s).expect("w"),
            }
        }
        writer.finalize().expect("finalize");

        let full = read_wav_mono(&path).expect("full read").samples;
        let mut reader = dubarr_sync::wav::WavRangeReader::open(&path).expect("open");
        let ranged = reader.read_mono(start, frames).expect("ranged read");
        assert_eq!(
            ranged.as_slice(),
            &full[start as usize..(start + frames) as usize],
            "{bits}-bit {format} ranged read differs"
        );
    }
}

/// Stereo is mixed down identically by both paths: the range must start on a
/// frame boundary, not on an interleaved sample.
#[test]
fn ranged_read_keeps_stereo_frames_aligned() {
    let dir = TempDir::new("slice-stereo");
    let left = make_base(8_000, 8.0, 0x1111);
    let right = make_base(8_000, 8.0, 0x2222);
    let path = dir.path().join("stereo.wav");
    common::write_wav_stereo_f32(&path, 8_000, &left, &right);

    let full = read_wav_mono(&path).expect("full read").samples;
    let mut reader = dubarr_sync::wav::WavRangeReader::open(&path).expect("open");
    let ranged = reader.read_mono(20_000, 4_000).expect("ranged read");
    assert_eq!(ranged.as_slice(), &full[20_000..24_000]);
}
