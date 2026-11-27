package syncjob

import (
	"reflect"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/sync"
)

// retriedResult is the retry's answer in these tests: different in every field
// from the attempt it replaces, so a value left behind is visible.
var retriedResult = sync.Result{
	OffsetMS:           1236,
	OffsetSubMS:        -0.5,
	CorrelationQuality: 0.97,
	MatchZ:             9,
	PrecisionMS:        0.125,
}

// reportWithHistory is a two-checkpoint report whose first row carries the
// given attempt history; the second row is the untouched neighbour every
// replacement must leave alone.
func reportWithHistory(attempts []analyze.Attempt) analyze.Report {
	return analyze.Report{
		OffsetsMS: []float64{1200, 1210},
		Qualities: []float64{0.9, 0.8},
		MatchZ:    []float64{6, 7},
		Checkpoints: []analyze.Checkpoint{
			{
				PositionPct: 25, SliceStartSec: 55, SliceLenSec: 190,
				OffsetMS: 1200, Quality: 0.9, MatchZ: 6, PrecisionMS: 0.125,
				Technique: "mfcc", Attempts: attempts,
			},
			{
				PositionPct: 40, SliceStartSec: 145, SliceLenSec: 190,
				OffsetMS: 1210, Quality: 0.8, MatchZ: 7, PrecisionMS: 0.125,
				Technique: "waveform",
				Attempts: []analyze.Attempt{
					{Technique: "waveform", OffsetMS: 1210, Quality: 0.8, MatchZ: 7, Accepted: true},
				},
			},
		},
	}
}

// acceptedCount is how many attempts claim to be the row's answer, which is
// the invariant the disclosure has to keep: exactly one.
func acceptedCount(attempts []analyze.Attempt) int {
	n := 0
	for _, attempt := range attempts {
		if attempt.Accepted {
			n++
		}
	}
	return n
}

// TestReplaceCheckpointKeepsTheAttemptsConsistent: an accepted retry rewrites
// one row's answer, so the attempt history must describe the answer the row now
// carries.
//
// Left alone, the winner kept the numbers of the attempt the retry replaced and
// still said accepted, so the report's own disclosure contradicted its row —
// and a reader could not tell which peak the offset came from.
func TestReplaceCheckpointKeepsTheAttemptsConsistent(t *testing.T) {
	attempts := []analyze.Attempt{
		{Technique: "waveform", OffsetMS: 1100, Quality: 0.05, MatchZ: 3},
		{Technique: "mfcc", OffsetMS: 1200, Quality: 0.9, MatchZ: 6, OffsetSubMS: 0.25, PrecisionMS: 0.125, Accepted: true},
	}
	report := reportWithHistory(attempts)
	untouched := report.Checkpoints[1]

	// The technique passed in is the one that answered the retry. These cases
	// keep mfcc so the winner's own label is the one asserted; the real retry
	// path answers with the waveform matcher (see
	// TestRetriedCheckpointNamesTheTechniqueThatAnswered).
	got := replaceCheckpoint(report, 0, analyze.TechniqueMFCC, retriedResult)
	row := got.Checkpoints[0]

	if row.OffsetMS != retriedResult.OffsetMS || row.OffsetSubMS != retriedResult.OffsetSubMS ||
		row.Quality != retriedResult.CorrelationQuality || row.MatchZ != retriedResult.MatchZ ||
		row.PrecisionMS != retriedResult.PrecisionMS {
		t.Fatalf("row = %+v, want the retry's own offset/sub/quality/z/precision", row)
	}
	if got.OffsetsMS[0] != retriedResult.OffsetMS || got.Qualities[0] != retriedResult.CorrelationQuality ||
		got.MatchZ[0] != retriedResult.MatchZ {
		t.Fatalf("flat views = %v/%v/%v, want the retry's own values",
			got.OffsetsMS[0], got.Qualities[0], got.MatchZ[0])
	}

	// The winner takes the retry's numbers and keeps its label: the retry is a
	// re-measurement of the same window, not a new technique. The answer it beat
	// keeps its own numbers and loses the flag.
	want := []analyze.Attempt{
		{Technique: "waveform", OffsetMS: 1100, Quality: 0.05, MatchZ: 3},
		{Technique: "mfcc", OffsetMS: retriedResult.OffsetMS, OffsetSubMS: retriedResult.OffsetSubMS,
			Quality: retriedResult.CorrelationQuality, MatchZ: retriedResult.MatchZ,
			PrecisionMS: retriedResult.PrecisionMS, Accepted: true},
	}
	if !reflect.DeepEqual(row.Attempts, want) {
		t.Fatalf("attempts = %+v, want %+v: the winner describes the retry and the loser is cleared",
			row.Attempts, want)
	}
	if n := acceptedCount(row.Attempts); n != 1 {
		t.Fatalf("%d accepted attempts, want exactly one: %+v", n, row.Attempts)
	}

	if !reflect.DeepEqual(got.Checkpoints[1], untouched) || got.OffsetsMS[1] != 1210 ||
		got.Qualities[1] != 0.8 || got.MatchZ[1] != 7 {
		t.Fatalf("the neighbouring checkpoint changed: %+v (was %+v)", got.Checkpoints[1], untouched)
	}

	t.Run("an unaccepted history rewrites its healthiest attempt", func(t *testing.T) {
		attempts := []analyze.Attempt{
			{Technique: "waveform", OffsetMS: 1100, Quality: 0.3, MatchZ: 2},
			{Technique: "mfcc", OffsetMS: 1150, Quality: 0.6, MatchZ: 4},
		}
		got := replaceCheckpoint(reportWithHistory(attempts), 0, analyze.TechniqueMFCC, retriedResult)
		want := []analyze.Attempt{
			{Technique: "waveform", OffsetMS: 1100, Quality: 0.3, MatchZ: 2},
			{Technique: "mfcc", OffsetMS: retriedResult.OffsetMS, OffsetSubMS: retriedResult.OffsetSubMS,
				Quality: retriedResult.CorrelationQuality, MatchZ: retriedResult.MatchZ,
				PrecisionMS: retriedResult.PrecisionMS, Accepted: true},
		}
		if !reflect.DeepEqual(got.Checkpoints[0].Attempts, want) {
			t.Fatalf("attempts = %+v, want the healthiest answer to become the retry: %+v",
				got.Checkpoints[0].Attempts, want)
		}
	})

	t.Run("a pre-0.5.0 row keeps an empty history", func(t *testing.T) {
		got := replaceCheckpoint(reportWithHistory(nil), 0, analyze.TechniqueMFCC, retriedResult)
		if got.Checkpoints[0].Attempts != nil {
			t.Fatalf("attempts = %+v, want none: a 0.4.0 row has no history to rewrite",
				got.Checkpoints[0].Attempts)
		}
	})

	t.Run("a history of failures is dropped", func(t *testing.T) {
		attempts := []analyze.Attempt{{Technique: "waveform", Error: "engine exploded"}}
		got := replaceCheckpoint(reportWithHistory(attempts), 0, analyze.TechniqueMFCC, retriedResult)
		if got.Checkpoints[0].Attempts != nil {
			t.Fatalf("attempts = %+v, want none: no attempt describes the retry's answer",
				got.Checkpoints[0].Attempts)
		}
	})
}

// Precisions the fixture below mixes: an MFCC answer is hop-quantised, the
// waveform matcher resolves one sample at the 8 kHz analysis rate.
const (
	hopPrecisionMS    = 10
	samplePrecisionMS = 0.125
)

// TestRetriedCheckpointNamesTheTechniqueThatAnswered: a retry re-decodes one
// window with the waveform engine, so the row it writes must name that
// technique and carry its precision — not the hop-quantised label of the answer
// it displaced.
//
// Left alone, an MFCC row kept technique "mfcc" while carrying sample
// precision, so the report described an accuracy the row did not have; and the
// report's own precision_ms was never recomputed, so it could claim a hop no
// remaining row answered at.
func TestRetriedCheckpointNamesTheTechniqueThatAnswered(t *testing.T) {
	report := analyze.Report{
		OffsetsMS:   []float64{1200, 1210, 1190},
		Qualities:   []float64{0.9, 0.85, 0.8},
		MatchZ:      []float64{6, 7, 8},
		PrecisionMS: hopPrecisionMS,
		Checkpoints: []analyze.Checkpoint{
			{
				PositionPct: 10, SliceStartSec: 20, SliceLenSec: 190,
				OffsetMS: 1200, Quality: 0.9, MatchZ: 6, PrecisionMS: hopPrecisionMS,
				Technique: "mfcc",
				Attempts: []analyze.Attempt{
					{Technique: "waveform", OffsetMS: 1100, Quality: 0.05, MatchZ: 3, PrecisionMS: samplePrecisionMS},
					{Technique: "mfcc", OffsetMS: 1200, OffsetSubMS: 1200, Quality: 0.9, MatchZ: 6,
						PrecisionMS: hopPrecisionMS, Accepted: true},
				},
			},
			{
				PositionPct: 40, SliceStartSec: 90, SliceLenSec: 190,
				OffsetMS: 1210, Quality: 0.85, MatchZ: 7, PrecisionMS: hopPrecisionMS,
				Technique: "mfcc",
			},
			{
				PositionPct: 70, SliceStartSec: 160, SliceLenSec: 190,
				OffsetMS: 1190, Quality: 0.8, MatchZ: 8, PrecisionMS: samplePrecisionMS,
				Technique: "waveform",
			},
		},
	}

	got := replaceCheckpoint(report, 0, analyze.TechniqueWaveform, retriedResult)
	row := got.Checkpoints[0]

	if row.Technique != string(analyze.TechniqueWaveform) {
		t.Fatalf("checkpoint technique = %q, want %q: the row carries the retry's numbers",
			row.Technique, analyze.TechniqueWaveform)
	}
	if row.PrecisionMS != retriedResult.PrecisionMS {
		t.Fatalf("checkpoint precision = %v, want %v", row.PrecisionMS, retriedResult.PrecisionMS)
	}
	// The winning attempt states the same technique: an entry left labelled
	// mfcc while carrying the retry's sample numbers is the same lie, one level
	// down.
	if winner := row.Attempts[1]; winner.Technique != string(analyze.TechniqueWaveform) || !winner.Accepted {
		t.Fatalf("winning attempt = %+v, want the retry's technique and the acceptance", winner)
	}
	if n := acceptedCount(row.Attempts); n != 1 {
		t.Fatalf("%d accepted attempts, want exactly one: %+v", n, row.Attempts)
	}

	// Checkpoint 1 still answers at the hop, so the report keeps the coarsest
	// precision its rows carry: a report never claims more resolution than its
	// worst checkpoint.
	if got.PrecisionMS != hopPrecisionMS {
		t.Fatalf("report precision = %v, want %v: checkpoint 1 still answered at the hop",
			got.PrecisionMS, hopPrecisionMS)
	}

	t.Run("the coarsest remaining row decides the report precision", func(t *testing.T) {
		// The only hop-quantised row is the one retried, so the report follows
		// its rows down to sample precision instead of keeping a hop nobody
		// answered at. A row with no recorded precision (a pre-0.5.0 one)
		// contributes nothing rather than dragging the report to zero.
		pair := analyze.Report{
			OffsetsMS:   []float64{1200, 1190},
			Qualities:   []float64{0.9, 0.8},
			MatchZ:      []float64{6, 8},
			PrecisionMS: hopPrecisionMS,
			Checkpoints: []analyze.Checkpoint{
				{
					PositionPct: 10, SliceStartSec: 20, SliceLenSec: 190,
					OffsetMS: 1200, Quality: 0.9, MatchZ: 6, PrecisionMS: hopPrecisionMS,
					Technique: "mfcc",
				},
				{
					PositionPct: 70, SliceStartSec: 160, SliceLenSec: 190,
					OffsetMS: 1190, Quality: 0.8, MatchZ: 8,
					Technique: "waveform",
				},
			},
		}
		got := replaceCheckpoint(pair, 0, analyze.TechniqueWaveform, retriedResult)
		if got.Checkpoints[0].Technique != string(analyze.TechniqueWaveform) {
			t.Fatalf("checkpoint technique = %q, want %q", got.Checkpoints[0].Technique, analyze.TechniqueWaveform)
		}
		if got.PrecisionMS != retriedResult.PrecisionMS {
			t.Fatalf("report precision = %v, want %v: no checkpoint answers at the hop any more",
				got.PrecisionMS, retriedResult.PrecisionMS)
		}
	})
}
