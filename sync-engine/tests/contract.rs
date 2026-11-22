//! JSON contract tests: the stdin job schema and the stdout result schema.

mod common;

use common::{run_cli, stdout_json, TempDir};

#[test]
fn pcm_job_emits_documented_result_schema() {
    let sr = 8_000u32;
    let base = common::make_base(sr, 3.0, 0x1234_5678);
    let target = common::shift(&base, 300 * sr as i64 / 1000);
    let payload = serde_json::json!({
        "target": { "pcm": { "sample_rate": sr, "samples": target } },
        "donor": { "pcm": { "sample_rate": sr, "samples": base } },
        "slice_start_sec": 1.0,
        "slice_len_sec": 2.0,
        "window_sec": 0.8,
    })
    .to_string();

    let output = run_cli(&payload);
    assert!(
        output.status.success(),
        "CLI failed: {:?} stderr={}",
        output.status.code(),
        String::from_utf8_lossy(&output.stderr)
    );
    let json = stdout_json(&output);
    let object = json.as_object().expect("top-level object");

    let mut keys: Vec<&str> = object.keys().map(String::as_str).collect();
    keys.sort_unstable();
    assert_eq!(
        keys,
        vec![
            "analyzed",
            "correlation_quality",
            "offset_ms",
            "sample_rate"
        ],
        "unexpected result keys: {keys:?}"
    );

    let offset = json["offset_ms"].as_f64().expect("offset_ms number");
    assert!((offset - 300.0).abs() <= 2.0, "offset was {offset}");
    let quality = json["correlation_quality"].as_f64().expect("quality");
    assert!((0.0..=1.0).contains(&quality), "quality {quality}");
    assert_eq!(json["sample_rate"].as_u64(), Some(sr as u64));

    let analyzed = json["analyzed"].as_object().expect("analyzed object");
    let mut akeys: Vec<&str> = analyzed.keys().map(String::as_str).collect();
    akeys.sort_unstable();
    assert_eq!(akeys, vec!["len", "start"]);
    assert_eq!(analyzed["start"].as_f64(), Some(1.0));
    assert_eq!(analyzed["len"].as_f64(), Some(2.0));
}

#[test]
fn path_job_accepts_omitted_window_sec() {
    let dir = TempDir::new("contract-window");
    let sr = 8_000u32;
    let base = common::make_base(sr, 4.0, 0xCAFE);
    let target = common::shift(&base, 200 * sr as i64 / 1000);
    let target_path = dir.path().join("t.wav");
    let donor_path = dir.path().join("d.wav");
    common::write_wav_mono_i16(&target_path, sr, &target);
    common::write_wav_mono_i16(&donor_path, sr, &base);
    let payload = serde_json::json!({
        "target": { "path": target_path },
        "donor": { "path": donor_path },
        "slice_start_sec": 1.0,
        "slice_len_sec": 2.0,
    })
    .to_string();
    let output = run_cli(&payload);
    assert!(
        output.status.success(),
        "stderr={}",
        String::from_utf8_lossy(&output.stderr)
    );
    let json = stdout_json(&output);
    let offset = json["offset_ms"].as_f64().unwrap();
    assert!((offset - 200.0).abs() <= 2.0, "offset was {offset}");
}

#[test]
fn version_flag_is_available() {
    let output = std::process::Command::new(env!("CARGO_BIN_EXE_dubarr-sync"))
        .arg("--version")
        .output()
        .expect("run --version");
    assert!(output.status.success());
    let text = String::from_utf8_lossy(&output.stdout);
    assert!(text.contains("dubarr-sync"), "version output: {text}");
}

#[test]
fn selftest_exits_zero_on_synthetic_signal() {
    let output = std::process::Command::new(env!("CARGO_BIN_EXE_dubarr-sync"))
        .arg("--selftest")
        .output()
        .expect("run --selftest");
    assert!(
        output.status.success(),
        "selftest failed: stderr={}",
        String::from_utf8_lossy(&output.stderr)
    );
    let json = stdout_json(&output);
    let offset = json["offset_ms"].as_f64().expect("selftest offset_ms");
    assert!((offset - 1_500.0).abs() <= 10.0, "selftest offset {offset}");
}
