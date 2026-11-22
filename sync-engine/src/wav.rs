//! WAV decoding into mono `f32` samples.

use std::path::Path;

use hound::{SampleFormat, WavReader};

use crate::SyncError;

/// Decoded mono audio.
#[derive(Debug, Clone)]
pub struct Audio {
    pub sample_rate: u32,
    /// Mono samples in `[-1.0, 1.0]` (stereo is averaged to mono).
    pub samples: Vec<f32>,
}

/// Read a WAV file as mono `f32` samples.
///
/// Supported encodings: 16/24/32-bit integer PCM and 32-bit float. One or two
/// channels are accepted; stereo is mixed down by averaging L and R. Multi-
/// channel files are rejected (donor extraction always produces mono/stereo).
pub fn read_wav_mono(path: &Path) -> Result<Audio, SyncError> {
    let mut reader = WavReader::open(path).map_err(|e| SyncError::Io {
        path: path.to_path_buf(),
        message: e.to_string(),
    })?;
    let spec = reader.spec();
    let channels = spec.channels as usize;
    if !(1..=2).contains(&channels) {
        return Err(SyncError::UnsupportedWav(format!(
            "{} channel(s); only mono or stereo is supported",
            spec.channels
        )));
    }

    let io_err = |e: hound::Error| SyncError::Io {
        path: path.to_path_buf(),
        message: e.to_string(),
    };

    let interleaved: Vec<f32> = match (spec.sample_format, spec.bits_per_sample) {
        (SampleFormat::Int, 16) => reader
            .samples::<i16>()
            .map(|s| s.map(|v| v as f32 / 32_768.0).map_err(io_err))
            .collect::<Result<Vec<_>, _>>()?,
        (SampleFormat::Int, 24) => reader
            .samples::<i32>()
            .map(|s| s.map(|v| v as f32 / 8_388_608.0).map_err(io_err))
            .collect::<Result<Vec<_>, _>>()?,
        (SampleFormat::Int, 32) => reader
            .samples::<i32>()
            .map(|s| s.map(|v| v as f32 / 2_147_483_648.0).map_err(io_err))
            .collect::<Result<Vec<_>, _>>()?,
        (SampleFormat::Float, 32) => reader
            .samples::<f32>()
            .map(|s| s.map_err(io_err))
            .collect::<Result<Vec<_>, _>>()?,
        (format, bits) => {
            return Err(SyncError::UnsupportedWav(format!(
                "{bits}-bit {format:?} samples; expected 16/24/32-bit int or 32-bit float"
            )))
        }
    };

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

    Ok(Audio {
        sample_rate: spec.sample_rate,
        samples,
    })
}
