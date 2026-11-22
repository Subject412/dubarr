//! Shared helpers for the dubarr-sync integration tests: deterministic
//! synthetic audio, WAV fixtures and CLI invocation.

#![allow(dead_code)]

use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::{Command, Output, Stdio};
use std::sync::atomic::{AtomicU64, Ordering};

/// Minimal deterministic LCG so goldens never depend on external crates.
pub struct Lcg(u64);

impl Lcg {
    pub fn new(seed: u64) -> Self {
        Self(seed.wrapping_mul(6364136223846793005).wrapping_add(1))
    }

    /// Uniform in [-1.0, 1.0).
    pub fn next_f32(&mut self) -> f32 {
        self.0 = self
            .0
            .wrapping_mul(6364136223846793005)
            .wrapping_add(1442695040888963407);
        let bits = (self.0 >> 40) as u32; // 24 high bits
        bits as f32 / 8_388_608.0 - 1.0
    }
}

/// Synthetic "speech-like" programme: low-passed noise plus tone bursts whose
/// carrier frequency changes over time. Unique enough that a cross-correlation
/// has a single unambiguous peak, but never perfectly periodic.
pub fn make_base(sample_rate: u32, seconds: f64, seed: u64) -> Vec<f32> {
    let sr = sample_rate as f64;
    let len = (seconds * sr).round() as usize;
    let mut rng = Lcg::new(seed);
    let mut lp = 0.0f32;
    let mut out = Vec::with_capacity(len);
    for n in 0..len {
        let white = rng.next_f32();
        lp = 0.9 * lp + 0.1 * white;
        let t = n as f64 / sr;
        let period = 0.37;
        let phase = t % period;
        let mut v = lp * 1.8;
        if phase < 0.12 {
            let idx = (t / period).floor() as u64;
            let freq = 300.0 + ((idx.wrapping_mul(137) % 1000) as f64);
            let env = (std::f64::consts::PI * phase / 0.12).sin();
            v += (0.7 * env * (std::f64::consts::TAU * freq * t).sin()) as f32;
        }
        out.push(v.clamp(-0.95, 0.95));
    }
    out
}

/// Shift a signal in time. Positive `offset_samples` delays it (silence is
/// prepended); negative advances it (samples are dropped from the front).
pub fn shift(samples: &[f32], offset_samples: i64) -> Vec<f32> {
    if offset_samples >= 0 {
        let mut out = vec![0.0f32; offset_samples as usize];
        out.extend_from_slice(samples);
        out
    } else {
        let drop = (-offset_samples) as usize;
        if drop >= samples.len() {
            Vec::new()
        } else {
            samples[drop..].to_vec()
        }
    }
}

/// Add independent white noise at the requested SNR (dB relative to signal power).
pub fn add_noise(samples: &mut [f32], snr_db: f64, seed: u64) {
    let power: f64 = samples
        .iter()
        .map(|&s| (s as f64) * (s as f64))
        .sum::<f64>()
        / samples.len().max(1) as f64;
    let noise_power = power / 10f64.powf(snr_db / 10.0);
    let amp = noise_power.sqrt() * 3f64.sqrt(); // uniform [-a, a] variance a^2/3
    let mut rng = Lcg::new(seed);
    for s in samples.iter_mut() {
        *s = (*s + rng.next_f32() * amp as f32).clamp(-1.0, 1.0);
    }
}

pub fn write_wav_mono_i16(path: &Path, sample_rate: u32, samples: &[f32]) {
    let spec = hound::WavSpec {
        channels: 1,
        sample_rate,
        bits_per_sample: 16,
        sample_format: hound::SampleFormat::Int,
    };
    let mut writer = hound::WavWriter::create(path, spec).expect("create wav");
    for &s in samples {
        let q = (s.clamp(-1.0, 1.0) * i16::MAX as f32).round() as i16;
        writer.write_sample(q).expect("write sample");
    }
    writer.finalize().expect("finalize wav");
}

pub fn write_wav_stereo_f32(path: &Path, sample_rate: u32, left: &[f32], right: &[f32]) {
    assert_eq!(left.len(), right.len());
    let spec = hound::WavSpec {
        channels: 2,
        sample_rate,
        bits_per_sample: 32,
        sample_format: hound::SampleFormat::Float,
    };
    let mut writer = hound::WavWriter::create(path, spec).expect("create wav");
    for (&l, &r) in left.iter().zip(right.iter()) {
        writer.write_sample(l).expect("write l");
        writer.write_sample(r).expect("write r");
    }
    writer.finalize().expect("finalize wav");
}

static COUNTER: AtomicU64 = AtomicU64::new(0);

/// Self-cleaning scratch directory (no external tempfile crate).
pub struct TempDir(PathBuf);

impl TempDir {
    pub fn new(tag: &str) -> Self {
        let unique = COUNTER.fetch_add(1, Ordering::Relaxed);
        let dir = std::env::temp_dir().join(format!(
            "dubarr-sync-test-{}-{}-{}",
            std::process::id(),
            unique,
            tag
        ));
        std::fs::create_dir_all(&dir).expect("create temp dir");
        Self(dir)
    }

    pub fn path(&self) -> &Path {
        &self.0
    }
}

impl Drop for TempDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

/// Run the compiled binary with `payload` on stdin.
pub fn run_cli(payload: &str) -> Output {
    let mut child = Command::new(env!("CARGO_BIN_EXE_dubarr-sync"))
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .expect("spawn dubarr-sync");
    child
        .stdin
        .as_mut()
        .expect("stdin")
        .write_all(payload.as_bytes())
        .expect("write stdin");
    drop(child.stdin.take());
    child.wait_with_output().expect("wait dubarr-sync")
}

pub fn stdout_json(output: &Output) -> serde_json::Value {
    let text = String::from_utf8_lossy(&output.stdout);
    serde_json::from_str(text.trim()).unwrap_or_else(|e| {
        panic!(
            "stdout is not JSON ({e}): {:?} / stderr: {}",
            text,
            String::from_utf8_lossy(&output.stderr)
        )
    })
}

/// Build a path-based job payload plus its two WAV fixtures.
pub fn wav_case(
    dir: &Path,
    sample_rate: u32,
    offset_ms: i64,
    slice_start_sec: f64,
    slice_len_sec: f64,
    window_sec: f64,
    snr_db: Option<f64>,
) -> String {
    // 20 s base leaves room for a +/-5 s shift inside a [5,15] s slice.
    let base = make_base(sample_rate, 20.0, 0xD0BA_1234);
    let offset_samples = offset_ms * sample_rate as i64 / 1000;
    let mut target = shift(&base, offset_samples);
    let mut donor = base.clone();
    if let Some(snr) = snr_db {
        add_noise(&mut target, snr, 0xAAAA);
        add_noise(&mut donor, snr, 0xBBBB);
    }
    let target_path = dir.join("target.wav");
    let donor_path = dir.join("donor.wav");
    write_wav_mono_i16(&target_path, sample_rate, &target);
    write_wav_mono_i16(&donor_path, sample_rate, &donor);
    serde_json::json!({
        "target": { "path": target_path },
        "donor": { "path": donor_path },
        "slice_start_sec": slice_start_sec,
        "slice_len_sec": slice_len_sec,
        "window_sec": window_sec,
    })
    .to_string()
}
