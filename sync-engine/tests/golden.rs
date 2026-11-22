//! Golden accuracy tests: synthetic WAVs with a known integer-sample offset,
//! spanning the +/-5 s search range, plus a noise-robustness case.

mod common;

use common::{stdout_json, wav_case, TempDir};

fn assert_offset(payload: &str, expected_ms: f64, tolerance_ms: f64, min_quality: f64) {
    let output = common::run_cli(payload);
    assert!(
        output.status.success(),
        "CLI failed: {:?}\nstderr: {}",
        output.status.code(),
        String::from_utf8_lossy(&output.stderr)
    );
    let json = stdout_json(&output);
    let offset = json["offset_ms"].as_f64().expect("offset_ms is a number");
    let quality = json["correlation_quality"]
        .as_f64()
        .expect("correlation_quality is a number");
    let error = (offset - expected_ms).abs();
    println!(
        "expected={expected_ms:+.1} ms got={offset:+.3} ms error={error:.3} ms quality={quality:.3}"
    );
    assert!(
        error <= tolerance_ms,
        "offset error {error:.3} ms exceeds {tolerance_ms:.1} ms (expected {expected_ms}, got {offset})"
    );
    assert!(
        quality >= min_quality,
        "correlation quality {quality:.3} below {min_quality}"
    );
    assert!(
        (0.0..=1.0).contains(&quality),
        "quality outside 0..1: {quality}"
    );
}

#[test]
fn positive_integer_offset_1500ms() {
    let dir = TempDir::new("pos1500");
    let payload = wav_case(dir.path(), 8_000, 1_500, 5.0, 10.0, 5.5, None);
    assert_offset(&payload, 1_500.0, 1.0, 0.5);
}

#[test]
fn negative_integer_offset_800ms() {
    let dir = TempDir::new("neg800");
    let payload = wav_case(dir.path(), 8_000, -800, 5.0, 10.0, 5.5, None);
    assert_offset(&payload, -800.0, 1.0, 0.5);
}

#[test]
fn positive_search_boundary_plus_5s() {
    let dir = TempDir::new("pos5s");
    let payload = wav_case(dir.path(), 8_000, 5_000, 5.0, 10.0, 5.0, None);
    assert_offset(&payload, 5_000.0, 1.0, 0.3);
}

#[test]
fn negative_search_boundary_minus_5s() {
    let dir = TempDir::new("neg5s");
    let payload = wav_case(dir.path(), 8_000, -5_000, 5.0, 10.0, 5.0, None);
    assert_offset(&payload, -5_000.0, 1.0, 0.3);
}

#[test]
fn noise_robustness_1500ms_at_6db_snr() {
    let dir = TempDir::new("noise");
    let payload = wav_case(dir.path(), 8_000, 1_500, 5.0, 10.0, 5.5, Some(6.0));
    assert_offset(&payload, 1_500.0, 20.0, 0.15);
}

#[test]
fn stereo_target_is_mixed_down() {
    let dir = TempDir::new("stereo");
    let sr = 8_000u32;
    let base = common::make_base(sr, 20.0, 0x5157_4445);
    let target = common::shift(&base, 1_000 * sr as i64 / 1000);
    // Slightly different L/R content; averaging must still correlate with mono donor.
    let mut left = target.clone();
    let mut right = target.clone();
    common::add_noise(&mut left, 30.0, 0x1111);
    common::add_noise(&mut right, 30.0, 0x2222);
    let target_path = dir.path().join("target_stereo.wav");
    let donor_path = dir.path().join("donor_mono.wav");
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
    assert_offset(&payload, 1_000.0, 2.0, 0.4);
}
