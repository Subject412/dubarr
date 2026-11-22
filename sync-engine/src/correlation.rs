//! FFT-based normalized cross-correlation.
//!
//! For decoded slices `x` (target) and `y` (donor) the circular correlation
//! `c[k] = sum_n x[n] * y[n - k]` is computed as `IFFT(FFT(x) * conj(FFT(y)))`
//! after zero-padding to a length of at least `len(x) + len(y) - 1`, which makes
//! it a linear correlation. The peak lag `k` is `t_target - t_donor` in samples.

use rustfft::num_complex::Complex;
use rustfft::FftPlanner;

use crate::{SyncError, MIN_SLICE_SAMPLES};

/// Result of a correlation run.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Analysis {
    /// `t_target - t_donor` in samples (positive delays the donor).
    pub offset_samples: i64,
    /// Normalized correlation coefficient at the peak, clamped to `0..=1`.
    pub quality: f32,
}

/// Remove DC, normalize to unit RMS, and reject silent slices.
fn prepare(samples: &[f32], which: &'static str) -> Result<Vec<f32>, SyncError> {
    let mean = samples.iter().map(|&s| s as f64).sum::<f64>() / samples.len() as f64;
    let mut centered: Vec<f32> = samples.iter().map(|&s| (s as f64 - mean) as f32).collect();
    let rms = (centered
        .iter()
        .map(|&s| (s as f64) * (s as f64))
        .sum::<f64>()
        / centered.len() as f64)
        .sqrt();
    if rms <= 1e-9 {
        return Err(SyncError::SilentSlice { which });
    }
    let scale = 1.0 / rms as f32;
    for s in &mut centered {
        *s *= scale;
    }
    Ok(centered)
}

fn prefix_squares(samples: &[f32]) -> Vec<f64> {
    let mut prefix = Vec::with_capacity(samples.len() + 1);
    prefix.push(0.0);
    let mut acc = 0.0;
    for &s in samples {
        acc += (s as f64) * (s as f64);
        prefix.push(acc);
    }
    prefix
}

fn energy(prefix: &[f64], start: usize, len: usize) -> f64 {
    prefix[start + len] - prefix[start]
}

/// Correlate a target slice against a donor slice sampled at the same rate.
///
/// `window_sec` bounds the absolute offset searched. The returned quality is the
/// normalized cross-correlation coefficient at the best lag (`0.0..=1.0`).
pub fn analyze(
    target: &[f32],
    donor: &[f32],
    sample_rate: u32,
    window_sec: f64,
) -> Result<Analysis, SyncError> {
    if sample_rate == 0 {
        return Err(SyncError::InvalidJob(
            "sample_rate must be greater than zero".to_string(),
        ));
    }
    if !window_sec.is_finite() || window_sec <= 0.0 {
        return Err(SyncError::InvalidJob(
            "window_sec must be a positive number".to_string(),
        ));
    }
    let nx = target.len();
    let ny = donor.len();
    if nx < MIN_SLICE_SAMPLES {
        return Err(SyncError::SliceTooShort {
            samples: nx,
            min: MIN_SLICE_SAMPLES,
        });
    }
    if ny < MIN_SLICE_SAMPLES {
        return Err(SyncError::SliceTooShort {
            samples: ny,
            min: MIN_SLICE_SAMPLES,
        });
    }

    let requested_lag = (window_sec * sample_rate as f64).round() as usize;
    let max_lag = requested_lag.min(nx.min(ny) - 1);
    if max_lag == 0 {
        return Err(SyncError::NoValidLag);
    }

    let x = prepare(target, "target")?;
    let y = prepare(donor, "donor")?;

    let nfft = (nx + ny - 1).next_power_of_two();
    let mut planner = FftPlanner::<f32>::new();
    let forward = planner.plan_fft_forward(nfft);
    let inverse = planner.plan_fft_inverse(nfft);

    let mut x_buf = vec![Complex::new(0.0f32, 0.0); nfft];
    let mut y_buf = vec![Complex::new(0.0f32, 0.0); nfft];
    for (dst, &src) in x_buf.iter_mut().zip(x.iter()) {
        dst.re = src;
    }
    for (dst, &src) in y_buf.iter_mut().zip(y.iter()) {
        dst.re = src;
    }
    forward.process(&mut x_buf);
    forward.process(&mut y_buf);

    // X * conj(Y)
    for (xv, yv) in x_buf.iter_mut().zip(y_buf.iter()) {
        *xv = Complex::new(xv.re * yv.re + xv.im * yv.im, xv.im * yv.re - xv.re * yv.im);
    }
    inverse.process(&mut x_buf);
    let scale = 1.0 / nfft as f32;
    for c in &mut x_buf {
        *c *= scale;
    }

    let px = prefix_squares(&x);
    let py = prefix_squares(&y);
    let min_overlap = 16.max(nx.min(ny) / 20);

    let mut best: Option<(i64, f64)> = None;
    for lag in -(max_lag as i64)..=(max_lag as i64) {
        let (count, x_start, y_start) = if lag >= 0 {
            let l = lag as usize;
            (nx.min(ny.saturating_add(l)).saturating_sub(l), l, 0usize)
        } else {
            let l = (-lag) as usize;
            (nx.min(ny.saturating_sub(l)), 0usize, l)
        };
        if count < min_overlap {
            continue;
        }
        let index = if lag >= 0 {
            lag as usize
        } else {
            nfft - (-lag) as usize
        };
        let raw = x_buf[index].re as f64;
        let denom = (energy(&px, x_start, count) * energy(&py, y_start, count)).sqrt();
        if denom <= 1e-12 {
            continue;
        }
        let ncc = (raw / denom).clamp(-1.0, 1.0);
        match best {
            None => best = Some((lag, ncc)),
            Some((_, current)) if ncc > current => best = Some((lag, ncc)),
            _ => {}
        }
    }

    let (offset_samples, peak) = best.ok_or(SyncError::NoValidLag)?;
    Ok(Analysis {
        offset_samples,
        quality: peak.max(0.0) as f32,
    })
}
