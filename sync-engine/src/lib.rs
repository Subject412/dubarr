//! `dubarr-sync` core — the Dubarr dubbing-sync offset engine.
//!
//! The engine compares a *target* audio slice with a *donor* slice and reports
//! `offset_ms = t_target - t_donor` as found by normalized FFT cross-correlation
//! (`rustfft`). A positive offset means the target event happens later than the
//! donor event, so the donor track must be delayed.
//!
//! Audio is decoded from WAV via `hound` (16/24/32-bit integer PCM or 32-bit
//! float). Mono is used as-is; stereo is averaged to mono: `(L + R) / 2`.

use std::fmt;
use std::path::PathBuf;

pub mod correlation;
pub mod job;
pub mod selftest;
pub mod wav;

pub use correlation::{analyze, Analysis};
pub use job::{run_job, Analyzed, Job, PcmSlice, Source, SyncResult};
pub use selftest::selftest;
pub use wav::{read_wav_mono, Audio};

/// Default maximum absolute offset searched by [`analyze`], in seconds.
pub const DEFAULT_WINDOW_SEC: f64 = 5.0;

/// Minimum number of samples in an analyzed slice.
pub const MIN_SLICE_SAMPLES: usize = 64;

/// Typed failures of the sync engine.
///
/// [`SyncError::exit_code`] maps these onto the CLI contract: usage problems
/// exit `2`, analysis problems exit `3`.
#[derive(Debug)]
pub enum SyncError {
    /// Job JSON could not be deserialized (usage error).
    Json(serde_json::Error),
    /// Job was syntactically valid but semantically invalid (usage error).
    InvalidJob(String),
    /// WAV could not be opened or read.
    Io { path: PathBuf, message: String },
    /// WAV layout is not supported (channels or sample encoding).
    UnsupportedWav(String),
    /// Target and donor sample rates differ. M4-1 requires identical rates.
    SampleRateMismatch { target: u32, donor: u32 },
    /// The requested slice has too few samples to correlate.
    SliceTooShort { samples: usize, min: usize },
    /// The requested slice starts beyond the end of the audio.
    SliceOutOfRange { start_sec: f64, duration_sec: f64 },
    /// One of the slices carries no signal (RMS is effectively zero).
    SilentSlice { which: &'static str },
    /// No lag inside the search window had enough overlap to score.
    NoValidLag,
    /// The built-in `--selftest` synthetic check did not meet its tolerance.
    SelftestFailed(String),
}

impl SyncError {
    /// Process exit code associated with this error.
    pub fn exit_code(&self) -> i32 {
        match self {
            SyncError::Json(_) | SyncError::InvalidJob(_) => 2,
            _ => 3,
        }
    }
}

impl fmt::Display for SyncError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            SyncError::Json(e) => write!(f, "invalid job JSON: {e}"),
            SyncError::InvalidJob(message) => write!(f, "invalid job: {message}"),
            SyncError::Io { path, message } => {
                write!(f, "cannot read {}: {message}", path.display())
            }
            SyncError::UnsupportedWav(message) => write!(f, "unsupported WAV: {message}"),
            SyncError::SampleRateMismatch { target, donor } => write!(
                f,
                "sample rate mismatch: target {target} Hz vs donor {donor} Hz \
                 (M4-1 requires identical rates)"
            ),
            SyncError::SliceTooShort { samples, min } => {
                write!(f, "slice too short: {samples} samples (minimum {min})")
            }
            SyncError::SliceOutOfRange {
                start_sec,
                duration_sec,
            } => write!(
                f,
                "slice starts at {start_sec:.3}s beyond audio duration {duration_sec:.3}s"
            ),
            SyncError::SilentSlice { which } => {
                write!(f, "{which} slice is silent (no signal to correlate)")
            }
            SyncError::NoValidLag => write!(f, "no valid correlation lag inside the search window"),
            SyncError::SelftestFailed(message) => write!(f, "selftest failed: {message}"),
        }
    }
}

impl std::error::Error for SyncError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            SyncError::Json(e) => Some(e),
            _ => None,
        }
    }
}

impl From<serde_json::Error> for SyncError {
    fn from(value: serde_json::Error) -> Self {
        SyncError::Json(value)
    }
}
