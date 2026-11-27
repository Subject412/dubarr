package correlate

import (
	"math"
	"slices"
	"sort"
	"testing"
)

// sortedValue is the oracle every case below is judged against: the value
// sort.Float64s leaves at index k. A selection that returns anything but the
// sorted pick — an interpolated median, a sampled one, the lower middle of an
// even-length slice — fails here, and so the goldens downstream cannot move.
func sortedValue(scores []float64, k int) float64 {
	sorted := slices.Clone(scores)
	sort.Float64s(sorted)
	return sorted[k]
}

// TestSelectOrderStatistic pins selectOrderStatistic against the sort it
// replaced, case by case: the shapes the lag scores and the goldens actually
// take, plus the ones a pivot scheme gets wrong if it is careless (a run of
// equal values, input that is already sorted either way, a median at an
// extreme).
func TestSelectOrderStatistic(t *testing.T) {
	ascending := make([]float64, 1000)
	for i := range ascending {
		ascending[i] = float64(i)
	}
	descending := slices.Clone(ascending)
	slices.Reverse(descending)

	duplicates := make([]float64, 1000)
	for i := range duplicates {
		duplicates[i] = 0.25
	}

	// A fixed-seed pseudo-random slice of the production score size: 100 000
	// values in -1..1 from the package's own generator, so the case is the same
	// values on every run.
	random := make([]float64, 100_000)
	rng := newLCG(95)
	for i := range random {
		random[i] = float64(rng.next())
	}

	cases := []struct {
		name   string
		scores []float64
		ks     []int
	}{
		{
			name:   "odd length",
			scores: []float64{0.3, -0.7, 0.9, 0.1, 0.5},
			ks:     []int{0, 2, 4},
		},
		{
			// sorted: 1 2 3 4, so k = len/2 = 2 is the upper middle.
			name:   "even length",
			scores: []float64{4, 1, 3, 2},
			ks:     []int{0, 1, 2, 3},
		},
		{
			// Every comparison is a tie: a pivot scheme that assumes the pivot
			// splits the window degenerates here.
			name:   "all duplicates",
			scores: duplicates,
			ks:     []int{0, 500, 500, 999},
		},
		{
			name:   "already sorted ascending",
			scores: ascending,
			ks:     []int{0, 1, 250, 500, 999},
		},
		{
			name:   "already sorted descending",
			scores: descending,
			ks:     []int{0, 499, 500, 998, 999},
		},
		{
			// sorted: 0.1 0.1 0.1 0.7 0.9 — the median IS the minimum.
			name:   "median is the minimum",
			scores: []float64{0.9, 0.1, 0.7, 0.1, 0.1},
			ks:     []int{2},
		},
		{
			// sorted: 0.2 0.4 0.9 0.9 0.9 — the median IS the maximum.
			name:   "median is the maximum",
			scores: []float64{0.9, 0.2, 0.9, 0.4, 0.9},
			ks:     []int{2},
		},
		{
			name:   "single element",
			scores: []float64{0.42},
			ks:     []int{0},
		},
		{
			name:   "two elements",
			scores: []float64{0.8, -0.3},
			ks:     []int{0, 1},
		},
		{
			name:   "pseudo-random 100k",
			scores: random,
			ks:     []int{0, 1, 49_999, 50_000, 99_998, 99_999},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range tc.ks {
				// A clone per call: the selection reorders its input, and the
				// oracle must see the untouched values.
				got := selectOrderStatistic(slices.Clone(tc.scores), k)
				want := sortedValue(tc.scores, k)
				if got != want {
					t.Fatalf("selectOrderStatistic(%d values, k=%d) = %v, want %v (the sorted pick)",
						len(tc.scores), k, got, want)
				}
			}
		})
	}
}

// TestSelectEvenLengthTakesTheUpperMiddle pins which side of an even-length
// slice the median comes from. peakProminence reads sorted[len(scores)/2], which
// is the upper middle — the side the pre-selection code took — and a selection
// that quietly returned the lower middle would still match a sorted oracle
// asked for k-1, so the side is asserted here instead of being implied.
func TestSelectEvenLengthTakesTheUpperMiddle(t *testing.T) {
	for _, scores := range [][]float64{
		{4, 1, 3, 2},
		{0.9, 0.1, 0.5, 0.3, 0.7, 0.2},
	} {
		k := len(scores) / 2
		got := selectOrderStatistic(slices.Clone(scores), k)

		sorted := slices.Clone(scores)
		sort.Float64s(sorted)
		if want := sorted[k]; got != want {
			t.Fatalf("%v: selectOrderStatistic(k=%d) = %v, want the upper middle %v", scores, k, got, want)
		}
		if lower := sorted[k-1]; lower != sorted[k] && got == lower {
			t.Fatalf("%v: selectOrderStatistic(k=%d) = %v, which is the lower middle, not sorted[%d] = %v",
				scores, k, got, k, sorted[k])
		}
	}
}

// TestSelectHeapFallback covers the depth limit's escape hatch, which no input
// in the table above reaches: heapSelect must agree with the sort for every
// rank, not just the middle.
func TestSelectHeapFallback(t *testing.T) {
	scores := make([]float64, 128)
	rng := newLCG(7)
	for i := range scores {
		scores[i] = float64(rng.next())
	}
	for k := 0; k < len(scores); k++ {
		if got, want := heapSelect(slices.Clone(scores), k), sortedValue(scores, k); got != want {
			t.Fatalf("heapSelect(k=%d) = %v, want %v", k, got, want)
		}
	}
}

// TestPeakProminenceEvenLengthMedianUsesUpperMiddle carries the same side
// assertion through the quality formula: each case is built so the lower middle
// would land well outside the float32 tolerance.
func TestPeakProminenceEvenLengthMedianUsesUpperMiddle(t *testing.T) {
	cases := []struct {
		name   string
		peak   float64
		scores []float64
	}{
		// upper middle 0.4 -> 2/3; lower middle 0.2 -> 3/4.
		{name: "two scores", peak: 0.8, scores: []float64{0.2, 0.4}},
		// upper middle 0.3 -> 0.2857; lower middle 0.2 -> 0.375.
		{name: "four scores", peak: 0.5, scores: []float64{0.1, 0.2, 0.3, 0.9}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sorted := slices.Clone(tc.scores)
			sort.Float64s(sorted)
			k := len(sorted) / 2

			want := (tc.peak - sorted[k]) / (1 - sorted[k])
			got := peakProminence(tc.peak, slices.Clone(tc.scores))
			if diff := math.Abs(float64(got) - want); diff > 1e-6 {
				t.Fatalf("peakProminence(%v, %v) = %v, want %v from the upper middle %v",
					tc.peak, tc.scores, got, want, sorted[k])
			}

			lower := (tc.peak - sorted[k-1]) / (1 - sorted[k-1])
			if math.Abs(want-lower) <= 0.05 {
				t.Fatalf("%v: the two middles give %v and %v; the case does not discriminate", tc.scores, want, lower)
			}
		})
	}
}
