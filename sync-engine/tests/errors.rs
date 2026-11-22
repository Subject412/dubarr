//! Error-path tests: typed analysis failures (exit 3) and usage failures (exit 2).

mod common;

use common::{run_cli, TempDir};

fn pcm(sample_rate: u32, samples: Vec<f32>) -> serde_json::Value {
    serde_json::json!({ "pcm": { "sample_rate": sample_rate, "samples": samples } })
}

#[test]
fn mismatched_sample_rates_are_rejected() {
    let payload = serde_json::json!({
        "target": pcm(8_000, vec![0.1f32; 8_000]),
        "donor": pcm(16_000, vec![0.1f32; 16_000]),
        "slice_start_sec": 0.0,
        "slice_len_sec": 0.5,
        "window_sec": 0.1,
    })
    .to_string();
    let output = run_cli(&payload);
    assert_eq!(output.status.code(), Some(3), "expected analysis failure");
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(
        stderr.to_lowercase().contains("sample rate"),
        "stderr should mention sample rate: {stderr}"
    );
}

#[test]
fn short_slice_is_rejected() {
    let payload = serde_json::json!({
        "target": pcm(8_000, vec![0.1f32; 100]),
        "donor": pcm(8_000, vec![0.2f32; 100]),
        "slice_start_sec": 0.0,
        "slice_len_sec": 0.005,
        "window_sec": 0.001,
    })
    .to_string();
    let output = run_cli(&payload);
    assert_eq!(output.status.code(), Some(3), "expected analysis failure");
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(
        stderr.to_lowercase().contains("too short"),
        "stderr should mention short slice: {stderr}"
    );
}

#[test]
fn malformed_json_is_a_usage_error() {
    let output = run_cli("{ not json");
    assert_eq!(output.status.code(), Some(2), "expected usage failure");
    assert!(!output.stderr.is_empty());
}

#[test]
fn missing_required_field_is_a_usage_error() {
    let output = run_cli(r#"{"target":{"pcm":{"sample_rate":8000,"samples":[0.1,0.2]}}}"#);
    assert_eq!(output.status.code(), Some(2), "expected usage failure");
}

#[test]
fn non_positive_window_is_a_usage_error() {
    let payload = serde_json::json!({
        "target": pcm(8_000, vec![0.1f32; 800]),
        "donor": pcm(8_000, vec![0.1f32; 800]),
        "slice_start_sec": 0.0,
        "slice_len_sec": 0.05,
        "window_sec": 0.0,
    })
    .to_string();
    let output = run_cli(&payload);
    assert_eq!(output.status.code(), Some(2), "expected usage failure");
}

#[test]
fn missing_wav_file_is_an_analysis_failure() {
    let payload = serde_json::json!({
        "target": { "path": "/nonexistent/dubarr/target.wav" },
        "donor": { "path": "/nonexistent/dubarr/donor.wav" },
        "slice_start_sec": 0.0,
        "slice_len_sec": 1.0,
    })
    .to_string();
    let output = run_cli(&payload);
    assert_eq!(output.status.code(), Some(3), "expected analysis failure");
}

#[test]
fn mismatched_rates_expose_typed_error() {
    let target = dubarr_sync::PcmSlice {
        sample_rate: 8_000,
        samples: vec![0.1; 8_000],
    };
    let donor = dubarr_sync::PcmSlice {
        sample_rate: 16_000,
        samples: vec![0.1; 16_000],
    };
    let job = dubarr_sync::Job {
        target: dubarr_sync::Source::Pcm { pcm: target },
        donor: dubarr_sync::Source::Pcm { pcm: donor },
        slice_start_sec: 0.0,
        slice_len_sec: 0.5,
        window_sec: Some(0.1),
    };
    match dubarr_sync::run_job(&job) {
        Err(dubarr_sync::SyncError::SampleRateMismatch { target, donor }) => {
            assert_eq!((target, donor), (8_000, 16_000));
        }
        other => panic!("expected SampleRateMismatch, got {other:?}"),
    }
}

#[test]
fn silent_slice_is_an_analysis_failure() {
    let payload = serde_json::json!({
        "target": pcm(8_000, vec![0.0f32; 8_000]),
        "donor": pcm(8_000, vec![0.0f32; 8_000]),
        "slice_start_sec": 0.0,
        "slice_len_sec": 0.5,
        "window_sec": 0.1,
    })
    .to_string();
    let output = run_cli(&payload);
    assert_eq!(output.status.code(), Some(3), "expected analysis failure");
    assert!(String::from_utf8_lossy(&output.stderr)
        .to_lowercase()
        .contains("silent"));
}

#[test]
fn slice_past_end_is_an_analysis_failure() {
    let dir = TempDir::new("past-end");
    let path = dir.path().join("short.wav");
    common::write_wav_mono_i16(&path, 8_000, &[0.1f32; 800]);
    let payload = serde_json::json!({
        "target": { "path": path },
        "donor": { "path": path },
        "slice_start_sec": 10.0,
        "slice_len_sec": 1.0,
    })
    .to_string();
    let output = run_cli(&payload);
    assert_eq!(output.status.code(), Some(3), "expected analysis failure");
}
