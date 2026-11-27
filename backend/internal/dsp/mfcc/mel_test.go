package mfcc

import (
	"math"
	"testing"
)

// TestFilterbankTrianglesTileTheBand pins the geometry of the mel bank: 26
// triangles spanning 0 Hz to min(FMaxHz, Nyquist), every weight non-negative,
// every triangle normalised so its weights sum to 1, and adjacent triangles
// sharing exactly one edge (the band is tiled, not gapped or double-counted).
//
// The plan's wording is "adjacent filters overlap by exactly one bin". At 8 kHz
// nfft = 256 has 129 bins and the band spans 128 of them, so 26 mel triangles
// are ~4.9 bins wide and a discrete one-bin overlap is arithmetically
// impossible. What the phrase stands for — one shared edge per neighbouring
// pair, no gap and no third contributor — is what this asserts.
func TestFilterbankTrianglesTileTheBand(t *testing.T) {
	const (
		rate = uint32(8000)
		nfft = 256
		bins = nfft/2 + 1
	)

	edges := bandEdgesHz(rate)
	if len(edges) != MelBands+2 {
		t.Fatalf("bandEdgesHz returned %d edges, want MelBands+2 = %d", len(edges), MelBands+2)
	}
	if edges[0] != 0 {
		t.Errorf("the band starts at %v Hz, want 0 Hz", edges[0])
	}
	if top := edges[len(edges)-1]; math.Abs(top-float64(FMaxHz)) > 1e-9 {
		t.Errorf("the band ends at %v Hz, want %v Hz", top, float64(FMaxHz))
	}
	for i := 1; i < len(edges); i++ {
		if edges[i] <= edges[i-1] {
			t.Fatalf("band edge %d (%v Hz) is not above edge %d (%v Hz); the triangles would overlap or vanish",
				i, edges[i], i-1, edges[i-1])
		}
	}

	bank := filterbank(rate, nfft)
	if len(bank) != MelBands {
		t.Fatalf("filterbank returned %d filters, want %d", len(bank), MelBands)
	}

	firsts := make([]int, len(bank))
	lasts := make([]int, len(bank))
	peaks := make([]int, len(bank))
	for b, filter := range bank {
		if len(filter) != bins {
			t.Fatalf("filter %d has %d weights, want %d (nfft/2+1)", b, len(filter), bins)
		}
		var sum float64
		peak := 0
		for k, w := range filter {
			if w < 0 {
				t.Fatalf("filter %d, bin %d: weight %v is negative", b, k, w)
			}
			if w > filter[peak] {
				peak = k
			}
			sum += float64(w)
		}
		if math.Abs(sum-1) > 1e-6 {
			t.Errorf("filter %d weights sum to %v, want 1 (within 1e-6)", b, sum)
		}
		// A triangle rises to its peak bin and falls after it. The tolerance
		// is float32 rounding, not shape.
		for k := 1; k <= peak; k++ {
			if filter[k] < filter[k-1]-1e-6 {
				t.Fatalf("filter %d falls at bin %d before its peak %d", b, k, peak)
			}
		}
		for k := peak + 1; k < bins; k++ {
			if filter[k] > filter[k-1]+1e-6 {
				t.Fatalf("filter %d rises at bin %d after its peak %d", b, k, peak)
			}
		}

		firsts[b], lasts[b], peaks[b] = -1, -1, peak
		for k, w := range filter {
			if w > 0 {
				if firsts[b] < 0 {
					firsts[b] = k
				}
				lasts[b] = k
			}
		}
		if firsts[b] < 0 {
			t.Fatalf("filter %d is empty", b)
		}
	}

	// The band starts at 0 Hz: the first triangle's left edge is bin 0, so its
	// support begins at the very bottom of the spectrum (bin 0 itself carries
	// zero weight: the triangle is zero at its own edge, and pre-emphasis has
	// already removed the DC that lives there).
	if firsts[0] > 1 {
		t.Errorf("the first filter starts at bin %d, want bin 0 or 1 (0 Hz)", firsts[0])
	}
	// The last filter's right edge is bin 128, where its weight is 0 by
	// construction, so its last non-zero bin is 127.
	if lasts[len(bank)-1] != bins-2 {
		t.Errorf("the last filter ends at bin %d, want bin %d (its 4000 Hz edge is bin %d)",
			lasts[len(bank)-1], bins-2, bins-1)
	}

	for b := 1; b < len(bank); b++ {
		if peaks[b] <= peaks[b-1] {
			t.Errorf("filter %d peaks at bin %d, not above filter %d's peak bin %d; the centres are out of order",
				b, peaks[b], b-1, peaks[b-1])
		}
		if lasts[b-1] < firsts[b] {
			t.Errorf("filters %d and %d do not touch (bins %d and %d); the band has a gap",
				b-1, b, lasts[b-1], firsts[b])
		}
	}

	// No bin may be covered by three filters: that would mean the triangles
	// overlap by more than their shared edge.
	for k := 0; k < bins; k++ {
		covering := 0
		for _, filter := range bank {
			if filter[k] > 0 {
				covering++
			}
		}
		if covering > 2 {
			t.Fatalf("bin %d is covered by %d filters, want at most 2", k, covering)
		}
	}
}

// TestFilterbankClampsToNyquist pins the top edge at min(FMaxHz, rate/2): at
// 1000 Hz the band ends at 500 Hz, not at the 4000 Hz constant.
func TestFilterbankClampsToNyquist(t *testing.T) {
	for _, tc := range []struct {
		rate uint32
		top  float64
	}{
		{rate: 1000, top: 500},
		{rate: 8000, top: 4000},
		{rate: 16000, top: 4000},
	} {
		edges := bandEdgesHz(tc.rate)
		if got := edges[len(edges)-1]; math.Abs(got-tc.top) > 1e-9 {
			t.Errorf("rate %d: the band ends at %v Hz, want %v Hz", tc.rate, got, tc.top)
		}
		bank := filterbank(tc.rate, 64)
		bins := 64/2 + 1
		for b, filter := range bank {
			if len(filter) != bins {
				t.Fatalf("rate %d: filter %d has %d weights, want %d", tc.rate, b, len(filter), bins)
			}
			var sum float64
			for _, w := range filter {
				if w < 0 {
					t.Fatalf("rate %d: filter %d has a negative weight", tc.rate, b)
				}
				sum += float64(w)
			}
			if math.Abs(sum-1) > 1e-6 {
				t.Errorf("rate %d: filter %d weights sum to %v, want 1", tc.rate, b, sum)
			}
		}
	}
}

// TestDCTIIRecoversAConstant pins the orthonormal scaling: a constant vector
// has a single non-zero coefficient, sqrt(1/N) times the sum, and every higher
// coefficient is zero.
func TestDCTIIRecoversAConstant(t *testing.T) {
	const value = 3.0
	power := make([]float64, 8)
	for i := range power {
		power[i] = value
	}

	got := dctII(power, len(power))
	if len(got) != len(power) {
		t.Fatalf("dctII returned %d coefficients, want %d", len(got), len(power))
	}
	want0 := value * 8 / math.Sqrt(8) // sqrt(1/N) * sum
	if math.Abs(got[0]-want0) > 1e-12 {
		t.Errorf("c0 = %v, want %v (sqrt(1/N) times the sum)", got[0], want0)
	}
	for k := 1; k < len(got); k++ {
		if math.Abs(got[k]) > 1e-12 {
			t.Errorf("c%d = %v, want 0 on a constant vector", k, got[k])
		}
	}
}

// TestDCTIIIsOrthonormalOnAKnownVector checks the 8-point DCT-II against a
// hand-computed case: the impulse x = [1, 0, ..., 0] has
//
//	c0 = sqrt(1/8)                = 0.35355339059327373
//	ck = sqrt(2/8)·cos(πk/16)     = 0.5·cos(πk/16),  k = 1..7
//
// and an orthonormal transform preserves the impulse's unit energy.
func TestDCTIIIsOrthonormalOnAKnownVector(t *testing.T) {
	power := make([]float64, 8)
	power[0] = 1

	want := []float64{
		0.35355339059327373, // sqrt(1/8)
		0.4903926402016152,  // 0.5*cos(pi/16)
		0.46193976625564337, // 0.5*cos(2pi/16)
		0.4157348061512726,  // 0.5*cos(3pi/16)
		0.3535533905932738,  // 0.5*cos(4pi/16)
		0.2777851165098011,  // 0.5*cos(5pi/16)
		0.1913417161825449,  // 0.5*cos(6pi/16)
		0.09754516100806415, // 0.5*cos(7pi/16)
	}

	got := dctII(power, len(want))
	if len(got) != len(want) {
		t.Fatalf("dctII returned %d coefficients, want %d", len(got), len(want))
	}
	var powerSum float64
	for k := range want {
		if math.Abs(got[k]-want[k]) > 1e-9 {
			t.Errorf("c%d = %v, want %v", k, got[k], want[k])
		}
		powerSum += got[k] * got[k]
	}
	// Parseval: an orthonormal transform of a unit-energy vector keeps energy 1.
	if math.Abs(powerSum-1) > 1e-9 {
		t.Errorf("the coefficients carry energy %v, want 1 (orthonormal DCT-II)", powerSum)
	}
}
