// Package mfcc finds the offset between two audio slices by correlating their
// mel-frequency cepstral coefficients instead of their samples.
//
// It is the second matcher behind #83, and it exists because the waveform
// matcher's answer depends on what a re-encode does to phase and envelope: a
// low-bitrate copy, broadcast processing or clipping changes the samples far
// more than it changes the *shape* of the sound. [Analyze] therefore reduces
// each frame to [Cepstra] cepstra over [MelBands] mel-spaced bands, standardises
// every coefficient over the whole slice, and cross-correlates those sequences.
// A fixed linear filter — the shape of most codec damage — shifts each
// coefficient by a constant, and the standardisation removes a constant, which
// is why the matcher still aligns audio the sample correlator cannot.
//
// # The contract is correlate's, not a second one
//
// [Analyze] returns a [correlate.Analysis] and raises correlate's errors, value
// for value and in the same order: *correlate.InvalidJob,
// *correlate.SilentSlice, *correlate.SliceTooShort, *correlate.NoValidLag. That
// is deliberate. The engine seam (#83's WithCorrelator) swaps one implementation
// for the other, and `analyze` must not be able to tell them apart except by the
// precision they report. OffsetSamples means the same thing (`t_target −
// t_donor`), Quality is the same peak prominence over the matcher's own lag
// curve, and MatchZ is the same (peak−mean)/std, so the threshold table in
// `analyze` judges both answers with the same numbers.
//
// # The precision is the hop, and that is the whole story
//
// Frames are 25 ms long and step 10 ms. The best lag is a whole number of hops,
// so the offset is a whole number of hops too: at 8 kHz, 80 samples, and
// PrecisionMS = 10.0. There is no parabolic interpolation of the peak —
// OffsetSubSamples is OffsetSamples — because a number interpolated between two
// 10 ms grid points would be a guess presented as measurement. Everything
// downstream must read PrecisionMS instead of assuming one sample; D3 is the
// reason `analyze` stores the field at all, and why the Sync page shows
// "±10 ms · MFCC" rather than a sample count.
//
// # What it does not do
//
// No dynamic time warping: a tempo change is the tempo stage's problem, and a
// warp here would break the single-offset contract. No sub-hop accuracy claim,
// no per-coefficient normalisation (the coefficients of one frame are not
// independent measurements of the same thing, so one summed correlation with one
// energy normalisation is the honest statistic), and no file access — it reads
// the float32 slices the runner already decoded. MFCC is not in the default
// technique list, so as shipped it only runs when a strategy asks for it.
//
// The fixture that proves the point lives in testdata: testdata/gen rebuilds it
// and records the gates in testdata/expected.json, and
// TestMFCCSurvivesClippingAndLowPass asserts that the cepstral matcher still
// answers on the degraded pair while the waveform matcher does not.
package mfcc
