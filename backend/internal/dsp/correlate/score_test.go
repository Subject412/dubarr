package correlate

import (
	"context"
	"math"
	"testing"
)

// The two z bands #85 names. They are written out here rather than read from
// analyze.DefaultThresholds because analyze imports sync, which imports this
// package: an import of analyze from a correlator test is an import cycle.
// analyze.DefaultThresholds' MatchZMaybe/MatchZGood must stay 5.0/10.0, and
// TestThresholdsTableIsTheOnlySource over there pins the table.
const (
	zMaybeFloor = 5.0
	zGoodFloor  = 10.0
)

// TestParabolicPeakRecoversHalfSample pins the vertex of the parabola through
// (left, centre, right): symmetric neighbours put the peak exactly on the
// sample, an asymmetric right neighbour pulls it towards that neighbour, and
// the fitted parabola must actually rise above the centre it interpolates.
func TestParabolicPeakRecoversHalfSample(t *testing.T) {
	if got := parabolicPeak(0.4, 1.0, 0.4); got != 0 {
		t.Fatalf("parabolicPeak(0.4, 1.0, 0.4) = %v, want 0 (the peak is on the sample)", got)
	}

	left, centre, right := 0.4, 1.0, 0.7
	delta := parabolicPeak(left, centre, right)
	if delta <= 0 || delta >= 0.5 {
		t.Fatalf("parabolicPeak(%v, %v, %v) = %v, want a value in (0, 0.5)", left, centre, right, delta)
	}

	// y(x) = a*x^2 + b*x + c through the three samples; the vertex must sit
	// above the centre, otherwise the interpolation is not fitting the peak.
	curvature := left - 2*centre + right
	a := curvature / 2
	b := (right - left) / 2
	fitted := a*delta*delta + b*delta + centre
	if fitted <= centre {
		t.Fatalf("fitted peak %v at delta %v does not rise above the centre %v", fitted, delta, centre)
	}
}

// TestParabolicPeakRejectsMissingNeighbour holds the documented sentinel: a
// neighbour the scan never saw (NaN) and a non-negative curvature both mean
// "no fractional offset", never a guess.
func TestParabolicPeakRejectsMissingNeighbour(t *testing.T) {
	cases := []struct {
		name   string
		left   float64
		centre float64
		right  float64
	}{
		{name: "left missing", left: math.NaN(), centre: 1.0, right: 0.7},
		{name: "right missing", left: 0.4, centre: 1.0, right: math.NaN()},
		{name: "both missing", left: math.NaN(), centre: 1.0, right: math.NaN()},
		{name: "positive curvature", left: 1.5, centre: 1.0, right: 1.5},
		{name: "flat neighbours", left: 1.0, centre: 1.0, right: 1.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parabolicPeak(tc.left, tc.centre, tc.right); got != 0 {
				t.Fatalf("parabolicPeak(%v, %v, %v) = %v, want exactly 0",
					tc.left, tc.centre, tc.right, got)
			}
		})
	}
}

// TestParabolicPeakIsClamped proves a pathological pair of neighbours — one
// far above the centre, one far below — cannot walk the offset past the
// neighbouring sample: the peak cannot be nearer to a neighbour than the
// neighbour itself.
func TestParabolicPeakIsClamped(t *testing.T) {
	cases := []struct {
		name         string
		left, centre float64
		right        float64
	}{
		{name: "right collapses", left: -5, centre: 1, right: 5},
		{name: "left collapses", left: 5, centre: 1, right: -5},
		{name: "huge right", left: 0, centre: 1, right: 1e9},
		{name: "huge left", left: 1e9, centre: 1, right: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parabolicPeak(tc.left, tc.centre, tc.right)
			if math.Abs(got) > 0.5 {
				t.Fatalf("parabolicPeak(%v, %v, %v) = %v, want |delta| <= 0.5",
					tc.left, tc.centre, tc.right, got)
			}
		})
	}
}

// TestMatchZOnCleanVersusNoise is the two-band guard #85 asks for: a clean
// shifted copy peaks far out in its own lag distribution, an unrelated noise
// pair does not.
//
// The slices are short on purpose: the expected maximum of ~2*maxLag standard
// normals grows like sqrt(2*ln(n)), so a long window would put the unrelated
// pair's z uncomfortably close to the 5.0 "needs a human look" floor.
func TestMatchZOnCleanVersusNoise(t *testing.T) {
	const rate uint32 = 8000
	const lead = 250
	base := whiteProgramme(4096, 0x2B)

	clean := make([]float32, len(base))
	copy(clean, base[lead:]) // the donor runs lead samples ahead

	got, err := Analyze(context.Background(), base, clean, rate, 0.03125)
	if err != nil {
		t.Fatalf("Analyze (clean pair): %v", err)
	}
	if got.OffsetSamples != lead {
		t.Fatalf("clean pair offset = %d samples, want %d", got.OffsetSamples, lead)
	}
	if float64(got.MatchZ) <= zGoodFloor {
		t.Fatalf("clean pair match_z = %v, want > %v (matched content must beat the \"likely correct\" band)",
			got.MatchZ, zGoodFloor)
	}

	unrelated := whiteProgramme(4096, 0x51D)
	noise, err := Analyze(context.Background(), base, unrelated, rate, 0.03125)
	if err != nil {
		t.Fatalf("Analyze (unrelated pair): %v", err)
	}
	if float64(noise.MatchZ) >= zMaybeFloor {
		t.Fatalf("unrelated pair match_z = %v, want < %v (a noise peak is not evidence)",
			noise.MatchZ, zMaybeFloor)
	}
}

// TestMatchZIsZeroOnAFlatCurve holds the documented sentinel: a curve with no
// spread carries no evidence either way, so the answer is 0 — never Inf, never
// NaN.
func TestMatchZIsZeroOnAFlatCurve(t *testing.T) {
	const peak, n = 0.5, 4
	sum := peak * n
	sumSq := peak * peak * n

	got := matchZ(peak, n, sum, sumSq)
	if got != 0 || math.IsNaN(got) || math.IsInf(got, 0) {
		t.Fatalf("matchZ(%v, %d, %v, %v) = %v, want exactly 0", peak, n, sum, sumSq, got)
	}
}

// TestMatchZNeedsTwoScores holds the n < 2 guard: one value has no spread to be
// scored against.
func TestMatchZNeedsTwoScores(t *testing.T) {
	if got := matchZ(0.9, 1, 0.9, 0.81); got != 0 {
		t.Fatalf("matchZ with n = 1 = %v, want 0", got)
	}
}
