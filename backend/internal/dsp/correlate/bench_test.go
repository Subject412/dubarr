package correlate

import "testing"

// The two benchmarks U-DSP.5 asks for. They are the human-readable half of the
// gate: `go test -bench . -run '^$' ./internal/dsp/correlate` prints the
// per-checkpoint cost of the correlation and of this host's three-transform
// floor, and the ratio between them is what TestCorrelationCostRatio asserts.
// Keep the measurement bodies in perf_test.go shared between the two, so the
// benchmark and the gate can never drift into measuring different code.

// BenchmarkCorrelateProduction is one production checkpoint: 190 s at 8 kHz,
// nfft = 2^22, fft.Options{NoFMA: true}, through a [Scratch] shared across
// iterations as a job's worker shares it across checkpoints.
func BenchmarkCorrelateProduction(b *testing.B) {
	scratch := NewScratch()

	// Warm-up, excluded from the timing: the first correlation of a fresh pool
	// allocates the two padded inputs, and go-fft caches its plan32 twiddle
	// table per (n, fused) pair on first use. A job pays both once per worker.
	if _, err := runCorrelation(scratch); err != nil {
		b.Fatalf("warm-up correlation: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := runCorrelation(scratch)
		if err != nil {
			b.Fatalf("correlation: %v", err)
		}
		perfCorrelationSink = got
	}
}

// BenchmarkReferenceTransform is the same host's floor for the three transforms
// a correlation is built from: two forward and one inverse of nfft points, same
// options, on buffers that already hold the checkpoint's samples.
func BenchmarkReferenceTransform(b *testing.B) {
	bufs := newTransformBuffers()
	perfTransformSink = runThreeTransforms(bufs) // plan32 warm-up, untimed

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		perfTransformSink = runThreeTransforms(bufs)
	}
}
