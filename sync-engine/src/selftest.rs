//! Built-in synthetic self-check used by `dubarr-sync --selftest`.
//!
//! Generates a deterministic pseudo-random programme, delays a copy by exactly
//! 1500 ms, and verifies that [`crate::analyze`] recovers that offset. This
//! exercises the same code path as a real job without touching the filesystem.

use crate::correlation::analyze;
use crate::job::{Analyzed, SyncResult};
use crate::SyncError;

const SELFTEST_SEC: f64 = 1.5;
const SELFTEST_TOLERANCE_MS: f64 = 10.0;
const SELFTEST_MIN_QUALITY: f32 = 0.3;

struct Lcg(u64);

impl Lcg {
    fn new(seed: u64) -> Self {
        Self(seed | 1)
    }

    fn next_f32(&mut self) -> f32 {
        self.0 = self
            .0
            .wrapping_mul(6364136223846793005)
            .wrapping_add(1442695040888963407);
        ((self.0 >> 40) as f32) / 8_388_608.0 - 1.0
    }
}

fn synthetic_programme(sample_rate: u32, seconds: f64) -> Vec<f32> {
    let sr = sample_rate as f64;
    let len = (seconds * sr).round() as usize;
    let mut rng = Lcg::new(0xD0BA_5EED);
    let mut lp = 0.0f32;
    let mut out = Vec::with_capacity(len);
    for n in 0..len {
        let white = rng.next_f32();
        lp = 0.9 * lp + 0.1 * white;
        let t = n as f64 / sr;
        let phase = t % 0.37;
        let mut value = lp * 1.8;
        if phase < 0.12 {
            let burst = (t / 0.37).floor() as u64;
            let freq = 300.0 + ((burst.wrapping_mul(137) % 1000) as f64);
            let env = (std::f64::consts::PI * phase / 0.12).sin();
            value += (0.7 * env * (std::f64::consts::TAU * freq * t).sin()) as f32;
        }
        out.push(value.clamp(-0.95, 0.95));
    }
    out
}

/// Run the synthetic check and return the result that the CLI prints.
pub fn selftest() -> Result<SyncResult, SyncError> {
    let sample_rate = 8_000u32;
    let base = synthetic_programme(sample_rate, 10.0);
    let delay = (SELFTEST_SEC * sample_rate as f64).round() as usize;
    let mut target = vec![0.0f32; delay];
    target.extend_from_slice(&base);

    let start = 2 * sample_rate as usize;
    let len = 4 * sample_rate as usize;
    let target_slice = &target[start..start + len];
    let donor_slice = &base[start..start + len];
    let analysis = analyze(target_slice, donor_slice, sample_rate, 2.5)?;

    let offset_ms = analysis.offset_samples as f64 * 1_000.0 / sample_rate as f64;
    let error_ms = (offset_ms - SELFTEST_SEC * 1_000.0).abs();
    if error_ms > SELFTEST_TOLERANCE_MS {
        return Err(SyncError::SelftestFailed(format!(
            "recovered {offset_ms:.3} ms, expected {:.1} ms (error {error_ms:.3} ms)",
            SELFTEST_SEC * 1_000.0
        )));
    }
    if analysis.quality < SELFTEST_MIN_QUALITY {
        return Err(SyncError::SelftestFailed(format!(
            "correlation quality {:.3} below {SELFTEST_MIN_QUALITY}",
            analysis.quality
        )));
    }

    Ok(SyncResult {
        offset_ms: (offset_ms * 1_000.0).round() / 1_000.0,
        correlation_quality: (analysis.quality as f64 * 1_000.0).round() / 1_000.0,
        sample_rate,
        analyzed: Analyzed {
            start: start as f64 / sample_rate as f64,
            len: len as f64 / sample_rate as f64,
        },
    })
}
