package correlate

import "math"

// This file holds the two confidence numbers #85 adds on top of the 0.4.0
// prominence score: the sub-sample position of the peak, and the peak's
// distance from the window's own lag distribution.
//
// Both are reported, neither decides anything here: `internal/analyze` owns the
// verdict table and the applied offset stays the integer lag of the global
// maximum (D2, D4). Note that [peakProminence] reorders the `scores` slice in
// place, so once a correlation has finished, `scores` is unordered and must
// never be indexed by lag — which is why the neighbours below are captured
// during the scan instead.

// parabolicPeak returns the fractional lag offset of the peak of the parabola
// through (left, centre, right), or 0 when a neighbour is missing (NaN), the
// curvature is non-negative, or the result is not finite. The result is clamped
// to [-0.5, 0.5]: the peak cannot be nearer to a neighbour than that neighbour.
//
// The vertex of y(x) = a*x^2 + b*x + c through the three samples sits at
// delta = 0.5*(left - right)/(left - 2*centre + right), with a = curvature/2
// and b = (right - left)/2.
func parabolicPeak(left, centre, right float64) float64 {
	if math.IsNaN(left) || math.IsNaN(centre) || math.IsNaN(right) {
		return 0
	}
	curvature := left - 2*centre + right
	// A non-negative curvature is a trough, a slope or a flat line: there is
	// no peak to interpolate.
	if curvature >= 0 {
		return 0
	}
	delta := 0.5 * (left - right) / curvature
	if math.IsNaN(delta) || math.IsInf(delta, 0) {
		return 0
	}
	return min(max(delta, -0.5), 0.5)
}

// matchZ scores the peak against the whole lag distribution:
// (peak - mean)/std, with `n`, `sum` and `sumSq` accumulated by the scan.
// Returns 0 for n < 2 or a standard deviation below 1e-9 (a flat curve carries
// no evidence either way, and 0 is the documented "no z" sentinel).
//
// This is the BBC audio-offset-finder form cited by #85. It is scale-free, so a
// quiet dub and a loud one are comparable, which the prominence score is not.
// The variance is the sample variance `(sumSq - sum*sum/n)/(n-1)`; the
// accumulator can make it very slightly negative on a flat curve, hence the
// clamp at zero before the square root.
func matchZ(peak float64, n int, sum, sumSq float64) float64 {
	if n < 2 {
		return 0
	}
	mean := sum / float64(n)
	variance := (sumSq - sum*sum/float64(n)) / float64(n-1)
	std := math.Sqrt(max(variance, 0))
	if std < 1e-9 {
		return 0
	}
	return (peak - mean) / std
}
