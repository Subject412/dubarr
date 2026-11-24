//! WAV decoding into mono `f32` samples.
//!
//! Issue #22: the engine used to collect the entire file into memory for every
//! checkpoint job (twice per job, once per source), so peak RSS grew with movie
//! length. A RIFF/WAVE `data` chunk is a flat PCM array, so a job only needs
//! the frames of its analysed window: [`WavRangeReader`] seeks to the exact
//! frame inside the chunk and decodes just that range. Frame indices are
//! sample-exact, so the analysed samples are bit-identical to the former
//! whole-file path and the reported offsets do not move.

use std::fs::File;
use std::io::BufReader;
use std::path::{Path, PathBuf};

use hound::{SampleFormat, WavReader};

use crate::SyncError;

/// Decoded mono audio.
#[derive(Debug, Clone)]
pub struct Audio {
    pub sample_rate: u32,
    /// Mono samples in `[-1.0, 1.0]` (stereo is averaged to mono).
    pub samples: Vec<f32>,
}

/// Header-validated WAV reader that decodes only a requested frame range.
///
/// `hound` seeks by frame inside the `data` chunk, so memory use is bounded by
/// the requested window rather than by the file length.
pub struct WavRangeReader {
    reader: WavReader<BufReader<File>>,
    path: PathBuf,
    channels: usize,
}

impl WavRangeReader {
    /// Open a WAV and validate the channel layout. Supported encodings are
    /// checked when samples are read, exactly like the former whole-file path.
    pub fn open(path: &Path) -> Result<Self, SyncError> {
        let reader = WavReader::open(path).map_err(|e| SyncError::Io {
            path: path.to_path_buf(),
            message: e.to_string(),
        })?;
        let channels = reader.spec().channels as usize;
        if !(1..=2).contains(&channels) {
            return Err(SyncError::UnsupportedWav(format!(
                "{} channel(s); only mono or stereo is supported",
                reader.spec().channels
            )));
        }
        Ok(Self {
            reader,
            path: path.to_path_buf(),
            channels,
        })
    }

    /// Sample rate from the `fmt` chunk.
    pub fn sample_rate(&self) -> u32 {
        self.reader.spec().sample_rate
    }

    /// Number of frames (samples per channel) in the file.
    pub fn duration(&self) -> u32 {
        self.reader.duration()
    }

    /// Decode up to `frames` mono frames starting at frame `start`, mixing
    /// stereo down by averaging `(L + R) / 2`. A range that runs past the end of
    /// the file is truncated, which mirrors the former whole-file slice.
    pub fn read_mono(&mut self, start: u32, frames: u32) -> Result<Vec<f32>, SyncError> {
        if frames == 0 {
            return Ok(Vec::new());
        }
        let path_err = |e: hound::Error| SyncError::Io {
            path: self.path.clone(),
            message: e.to_string(),
        };
        self.reader
            .seek(start)
            .map_err(|e: std::io::Error| SyncError::Io {
                path: self.path.clone(),
                message: e.to_string(),
            })?;
        let spec = self.reader.spec();
        let want = frames as usize * self.channels;
        let interleaved: Vec<f32> = match (spec.sample_format, spec.bits_per_sample) {
            (SampleFormat::Int, 16) => self
                .reader
                .samples::<i16>()
                .take(want)
                .map(|s| s.map(|v| v as f32 / 32_768.0).map_err(path_err))
                .collect::<Result<Vec<_>, _>>()?,
            (SampleFormat::Int, 24) => self
                .reader
                .samples::<i32>()
                .take(want)
                .map(|s| s.map(|v| v as f32 / 8_388_608.0).map_err(path_err))
                .collect::<Result<Vec<_>, _>>()?,
            (SampleFormat::Int, 32) => self
                .reader
                .samples::<i32>()
                .take(want)
                .map(|s| s.map(|v| v as f32 / 2_147_483_648.0).map_err(path_err))
                .collect::<Result<Vec<_>, _>>()?,
            (SampleFormat::Float, 32) => self
                .reader
                .samples::<f32>()
                .take(want)
                .map(|s| s.map_err(path_err))
                .collect::<Result<Vec<_>, _>>()?,
            (format, bits) => {
                return Err(SyncError::UnsupportedWav(format!(
                    "{bits}-bit {format:?} samples; expected 16/24/32-bit int or 32-bit float"
                )))
            }
        };
        Ok(mixdown(interleaved, self.channels))
    }
}

/// Fold interleaved mono/stereo samples into mono frames.
fn mixdown(interleaved: Vec<f32>, channels: usize) -> Vec<f32> {
    let frames = interleaved.len() / channels;
    let mut samples = Vec::with_capacity(frames);
    if channels == 1 {
        samples.extend_from_slice(&interleaved[..frames]);
    } else {
        // Stereo mixdown choice: arithmetic mean of the two channels.
        for frame in interleaved.chunks_exact(channels) {
            samples.push((frame[0] + frame[1]) * 0.5);
        }
    }
    samples
}

/// Read a WAV file as mono `f32` samples.
///
/// Supported encodings: 16/24/32-bit integer PCM and 32-bit float. One or two
/// channels are accepted; stereo is mixed down by averaging L and R. Multi-
/// channel files are rejected (donor extraction always produces mono/stereo).
///
/// Kept for callers that really need the whole programme; the job path uses
/// [`WavRangeReader`] so it never allocates more than its analysed window.
pub fn read_wav_mono(path: &Path) -> Result<Audio, SyncError> {
    let mut reader = WavRangeReader::open(path)?;
    let sample_rate = reader.sample_rate();
    let duration = reader.duration();
    let samples = reader.read_mono(0, duration)?;
    Ok(Audio {
        sample_rate,
        samples,
    })
}
