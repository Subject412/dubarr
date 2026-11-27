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

	got := replaceCheckpoint(report, 0, retriedResult)
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
		got := replaceCheckpoint(reportWithHistory(attempts), 0, retriedResult)
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
		got := replaceCheckpoint(reportWithHistory(nil), 0, retriedResult)
		if got.Checkpoints[0].Attempts != nil {
			t.Fatalf("attempts = %+v, want none: a 0.4.0 row has no history to rewrite",
				got.Checkpoints[0].Attempts)
		}
	})

	t.Run("a history of failures is dropped", func(t *testing.T) {
		attempts := []analyze.Attempt{{Technique: "waveform", Error: "engine exploded"}}
		got := replaceCheckpoint(reportWithHistory(attempts), 0, retriedResult)
		if got.Checkpoints[0].Attempts != nil {
			t.Fatalf("attempts = %+v, want none: no attempt describes the retry's answer",
				got.Checkpoints[0].Attempts)
		}
	})
}
