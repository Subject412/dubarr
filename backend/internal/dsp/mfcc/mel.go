package mfcc

import (
	"math"

	"github.com/go-fft/fft"
)

// This file holds the spectral front end: the mel scale, the filterbank that
// converts an FFT frame into band energies, the DCT-II that turns those into
// cepstra, and the single real transform they share.

// hzToMel converts a frequency in Hz to the mel scale, in the HTK form
// `2595·log10(1 + f/700)`. The exact formula matters only for the band spacing,
// but it must be the same in the filterbank and in the tests, so there is one
// copy.
func hzToMel(hz float64) float64 { return 2595 * math.Log10(1+hz/700) }

// melToHz inverts [hzToMel].
func melToHz(mel float64) float64 { return 700 * (math.Pow(10, mel/2595) - 1) }

// bandEdgesHz returns the [MelBands]+2 triangle edges in Hz: MelBands+1 bands,
// evenly spaced on the mel scale between 0 Hz and min([FMaxHz], rate/2).
//
// The top is clamped to Nyquist because a filter above it would weigh bins that
// do not exist; the endpoints are then pinned exactly, so the first filter
// starts at 0 Hz and the last ends at the band's top edge rather than at the
// float value melToHz returns for it.
func bandEdgesHz(sampleRate uint32) []float64 {
	top := math.Min(float64(FMaxHz), float64(sampleRate)/2)
	edges := make([]float64, MelBands+2)
	lo, hi := hzToMel(0), hzToMel(top)
	last := float64(len(edges) - 1)
	for i := range edges {
		edges[i] = melToHz(lo + (hi-lo)*float64(i)/last)
	}
	edges[0] = 0
	edges[len(edges)-1] = top
	return edges
}

// filterbank builds the [MelBands] triangular filters over the non-redundant
// bins of an nfft-point real transform: band b rises from 0 at edge b to 1 at
// edge b+1 and falls back to 0 at edge b+2, so neighbouring triangles share
// exactly their edge. Each filter is then normalised to sum to 1, which is what
// makes the band energies comparable across a narrow low band and a wide high
// one instead of weighting every band by its width.
//
// The edge positions are kept in floating-point bin coordinates, not rounded to
// whole bins: at 8 kHz with 26 bands over 128 bins the lowest bands are ~1.5
// bins apart, and rounding would collapse some of them to zero width.
func filterbank(sampleRate uint32, nfft int) [][]float32 {
	bins := nfft/2 + 1
	edges := bandEdgesHz(sampleRate)

	// Hz -> FFT bin.
	toBin := float64(nfft) / float64(sampleRate)
	edge := make([]float64, len(edges))
	for i, hz := range edges {
		edge[i] = hz * toBin
	}

	bank := make([][]float32, MelBands)
	for b := 0; b < MelBands; b++ {
		left, centre, right := edge[b], edge[b+1], edge[b+2]
		weights := make([]float32, bins)

		var total float64
		for k := 0; k < bins; k++ {
			x := float64(k)
			var w float64
			switch {
			case x <= left || x >= right:
				w = 0
			case x <= centre:
				if centre > left {
					w = (x - left) / (centre - left)
				} else {
					w = 1
				}
			default:
				if right > centre {
					w = (right - x) / (right - centre)
				}
			}
			weights[k] = float32(w)
			total += w
		}

		// A degenerate triangle — both edges inside one bin — cannot be
		// normalised and would hand every frame the log floor in that band.
		// Give it the bin nearest its centre instead.
		if total == 0 {
			if k := int(math.Round(centre)); k >= 0 && k < bins {
				weights[k] = 1
				total = 1
			}
		}

		if total > 0 {
			inv := float32(1 / total)
			for k := range weights {
				weights[k] *= inv
			}
		}
		bank[b] = weights
	}
	return bank
}

// dctII returns the first keep coefficients of the orthonormal DCT-II of power:
//
//	c_k = sqrt(2/N)·Σ_i power[i]·cos(π·k·(i+½)/N),  k >= 1
//	c_0 = sqrt(1/N)·Σ_i power[i]
//
// The k = 0 scaling is the orthonormal form's, not the unnormalised cosine
// transform's: it makes the transform energy-preserving, which is what lets the
// per-coefficient standardisation in [frames] treat c0 and c12 alike.
func dctII(power []float64, keep int) []float64 {
	n := len(power)
	out := make([]float64, keep)
	if n == 0 {
		return out
	}

	for k := 0; k < keep; k++ {
		var sum float64
		for i, v := range power {
			sum += v * math.Cos(math.Pi*float64(k)*(float64(i)+0.5)/float64(n))
		}
		if k == 0 {
			out[k] = sum * math.Sqrt(1/float64(n))
		} else {
			out[k] = sum * math.Sqrt(2/float64(n))
		}
	}
	return out
}

// powerSpectrum returns |RFFT(frame)|² for the non-redundant bins 0..nfft/2 of
// one windowed frame, which already has nfft samples.
//
// The transform is float32 like the rest of the DSP path and takes
// fft.Options{NoFMA: true}, as correlate does: the fused kernel's bits depend on
// the CPU, and the fixture's numbers must not.
func powerSpectrum(frame []float32, nfft int) []float64 {
	spectrum := fft.RFFT32With(frame, fft.Options{N: nfft, NoFMA: true})
	power := make([]float64, len(spectrum))
	for i, c := range spectrum {
		re, im := float64(real(c)), float64(imag(c))
		power[i] = re*re + im*im
	}
	return power
}
