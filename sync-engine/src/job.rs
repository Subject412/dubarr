//! Job schema, slice extraction and the public analysis entry point.

use std::path::PathBuf;

use serde::{Deserialize, Serialize};

use crate::correlation::analyze;
use crate::wav::WavRangeReader;
use crate::{SyncError, DEFAULT_WINDOW_SEC, MIN_SLICE_SAMPLES};

/// Inline raw PCM slice used instead of a WAV file.
#[derive(Debug, Clone, Deserialize)]
pub struct PcmSlice {
    pub sample_rate: u32,
    pub samples: Vec<f32>,
}

/// Where a slice comes from: a WAV file path or inline PCM samples.
#[derive(Debug, Clone, Deserialize)]
#[serde(untagged)]
pub enum Source {
    Path { path: PathBuf },
    Pcm { pcm: PcmSlice },
}

/// Job accepted on stdin.
///
/// `slice_start_sec` and `slice_len_sec` select the analyzed window from both
/// sources independently (same timeline positions). `window_sec` bounds the
/// absolute offset search and defaults to [`DEFAULT_WINDOW_SEC`].
#[derive(Debug, Clone, Deserialize)]
pub struct Job {
    pub target: Source,
    pub donor: Source,
    pub slice_start_sec: f64,
    pub slice_len_sec: f64,
    #[serde(default)]
    pub window_sec: Option<f64>,
}

/// Actual window that was analyzed, in seconds.
#[derive(Debug, Clone, Copy, PartialEq, Serialize, Deserialize)]
pub struct Analyzed {
    pub start: f64,
    pub len: f64,
}

/// Result emitted on stdout.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SyncResult {
    pub offset_ms: f64,
    pub correlation_quality: f64,
    pub sample_rate: u32,
    pub analyzed: Analyzed,
}

/// A job source opened for one slice: a WAV file read by frame range, or
/// inline PCM. Opening only reads the header, so a pathological file (or a
/// sample-rate mismatch) still fails before any window is decoded.
enum SourceAudio {
    File(WavRangeReader),
    Pcm { sample_rate: u32, samples: Vec<f32> },
}

impl SourceAudio {
    fn open(source: &Source) -> Result<Self, SyncError> {
        match source {
            Source::Path { path } => Ok(SourceAudio::File(WavRangeReader::open(path)?)),
            Source::Pcm { pcm } => {
                if pcm.sample_rate == 0 {
                    return Err(SyncError::InvalidJob(
                        "PCM sample_rate must be greater than zero".to_string(),
                    ));
                }
                if !pcm.samples.iter().all(|s| s.is_finite()) {
                    return Err(SyncError::InvalidJob(
                        "PCM samples must all be finite numbers".to_string(),
                    ));
                }
                Ok(SourceAudio::Pcm {
                    sample_rate: pcm.sample_rate,
                    samples: pcm.samples.clone(),
                })
            }
        }
    }

    fn sample_rate(&self) -> u32 {
        match self {
            SourceAudio::File(reader) => reader.sample_rate(),
            SourceAudio::Pcm { sample_rate, .. } => *sample_rate,
        }
    }

    /// Frames (per-channel samples) in the source.
    fn frames(&self) -> usize {
        match self {
            SourceAudio::File(reader) => reader.duration() as usize,
            SourceAudio::Pcm { samples, .. } => samples.len(),
        }
    }

    /// Slice `len` frames from `start`, truncating at the end of the source
    /// exactly like the former whole-file `take_slice`.
    fn slice(&mut self, start: usize, len: usize, sample_rate: u32) -> Result<Vec<f32>, SyncError> {
        let total = self.frames();
        if start >= total {
            return Err(SyncError::SliceOutOfRange {
                start_sec: start as f64 / sample_rate as f64,
                duration_sec: total as f64 / sample_rate as f64,
            });
        }
        let end = start.saturating_add(len).min(total);
        match self {
            SourceAudio::File(reader) => reader.read_mono(start as u32, (end - start) as u32),
            SourceAudio::Pcm { samples, .. } => Ok(samples[start..end].to_vec()),
        }
    }
}

fn round_millis(value: f64) -> f64 {
    (value * 1_000.0).round() / 1_000.0
}

/// Run a job: load both sources, extract the slice, correlate, and build the
/// result. Rejects mismatched sample rates with
/// [`SyncError::SampleRateMismatch`].
pub fn run_job(job: &Job) -> Result<SyncResult, SyncError> {
    if !job.slice_start_sec.is_finite() || job.slice_start_sec < 0.0 {
        return Err(SyncError::InvalidJob(
            "slice_start_sec must be a non-negative number".to_string(),
        ));
    }
    if !job.slice_len_sec.is_finite() || job.slice_len_sec <= 0.0 {
        return Err(SyncError::InvalidJob(
            "slice_len_sec must be a positive number".to_string(),
        ));
    }
    if let Some(window) = job.window_sec {
        if !window.is_finite() || window <= 0.0 {
            return Err(SyncError::InvalidJob(
                "window_sec must be a positive number".to_string(),
            ));
        }
    }

    let mut target_audio = SourceAudio::open(&job.target)?;
    let mut donor_audio = SourceAudio::open(&job.donor)?;
    if target_audio.sample_rate() != donor_audio.sample_rate() {
        return Err(SyncError::SampleRateMismatch {
            target: target_audio.sample_rate(),
            donor: donor_audio.sample_rate(),
        });
    }
    let sample_rate = target_audio.sample_rate();
    if sample_rate == 0 {
        return Err(SyncError::InvalidJob(
            "sample_rate must be greater than zero".to_string(),
        ));
    }

    let start = (job.slice_start_sec * sample_rate as f64).round() as usize;
    let len = (job.slice_len_sec * sample_rate as f64).round() as usize;
    let target = target_audio.slice(start, len, sample_rate)?;
    let donor = donor_audio.slice(start, len, sample_rate)?;

    let samples = target.len().min(donor.len());
    if samples < MIN_SLICE_SAMPLES {
        return Err(SyncError::SliceTooShort {
            samples,
            min: MIN_SLICE_SAMPLES,
        });
    }

    let window = job.window_sec.unwrap_or(DEFAULT_WINDOW_SEC);
    let analysis = analyze(&target, &donor, sample_rate, window)?;
    let offset_ms = analysis.offset_samples as f64 * 1_000.0 / sample_rate as f64;

    Ok(SyncResult {
        offset_ms: round_millis(offset_ms),
        correlation_quality: round_millis(analysis.quality as f64),
        sample_rate,
        analyzed: Analyzed {
            start: start as f64 / sample_rate as f64,
            len: samples as f64 / sample_rate as f64,
        },
    })
}
