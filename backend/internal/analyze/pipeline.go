package analyze

import (
	"context"
	"errors"
	"fmt"
)

// This file is the technique pipeline's routing: one checkpoint's answers in
// phase 1, and the drift-driven second pass in phase 2 (#82, D4).
//
// Phase 1 is the per-checkpoint loop that runs inside a worker, so it touches
// only its own slot. Phase 2 cannot run there: "does this checkpoint sit on the
// line?" needs every checkpoint's first answer, so it is one sequential pass
// after the join, in checkpoint order, and it never runs more than once.

// errNoTechniqueAnswered is a checkpoint that has techniques to run but no
// answer at all. It cannot come from an engine error — those are recorded per
// attempt — so it means the strategy reached the pipeline with nothing runnable.
var errNoTechniqueAnswered = errors.New("no technique produced an answer")

// mergeEngines is the composition seam: the engine argument is the waveform
// matcher, which is what every 0.4.0 caller passes, and an explicit Engines map
// entry for waveform wins so a caller can override it.
func mergeEngines(engines map[TechniqueID]Engine, waveform Engine) map[TechniqueID]Engine {
	if len(engines) > 0 {
		if _, ok := engines[TechniqueWaveform]; ok {
			return engines
		}
	}
	if waveform == nil {
		return engines
	}
	merged := make(map[TechniqueID]Engine, len(engines)+1)
	for id, engine := range engines {
		merged[id] = engine
	}
	merged[TechniqueWaveform] = waveform
	return merged
}

// engineFor resolves the seam that runs one technique. A missing (or nil) entry
// means "this build cannot run that technique" (see [Config.Engines]).
func (c Config) engineFor(id TechniqueID) (Engine, bool) {
	engine, ok := c.Engines[id]
	if !ok || engine == nil {
		return nil, false
	}
	return engine, true
}

// entries is what this job will actually run in phase 1: the enabled techniques
// that have an engine, in list order. A technique this build cannot run is
// skipped rather than fatal — slice 04 registers MFCC's engine, and a settings
// row naming it must not fail every job in the meantime.
func (c Config) entries() []Technique {
	enabled := c.Strategy.normalised().Enabled()
	out := make([]Technique, 0, len(enabled))
	for _, entry := range enabled {
		if _, ok := c.engineFor(entry.ID); ok {
			out = append(out, entry)
		}
	}
	return out
}

// aboveDriftEntries is the phase-2 list: enabled `above_drift` techniques that
// have an engine, in list order.
func (c Config) aboveDriftEntries() []Technique {
	enabled := c.Strategy.normalised().Enabled()
	out := make([]Technique, 0, len(enabled))
	for _, entry := range enabled {
		if entry.When != WhenAboveDrift {
			continue
		}
		if _, ok := c.engineFor(entry.ID); ok {
			out = append(out, entry)
		}
	}
	return out
}

// skipped is the enabled techniques this build has no engine for, in list
// order. [Analyze] records one note per entry so a report says why a configured
// step never ran.
func (c Config) skipped() []TechniqueID {
	enabled := c.Strategy.normalised().Enabled()
	out := make([]TechniqueID, 0, len(enabled))
	for _, entry := range enabled {
		if _, ok := c.engineFor(entry.ID); !ok {
			out = append(out, entry.ID)
		}
	}
	return out
}

// evaluateCheckpoint answers one checkpoint in phase 1.
//
// The audio is cut once and every technique on this checkpoint reads the same
// slice: the [Slicer] contract is one Slice per checkpoint per worker, and a
// second opinion must not cost a second decode of the same window.
//
// The loop is the operator's list, in order:
//
//   - `always` runs unconditionally;
//   - `below_confidence` is skipped once the best answer so far already clears
//     its own floor (a second opinion nobody needs is pure cost);
//   - `above_drift` never runs here (see [applyAboveDrift]);
//   - the first accepted answer wins and stops the loop; until one is accepted
//     the healthiest answer so far is kept as the fallback.
//
// An engine error is recorded on its attempt and the next technique still runs.
// A checkpoint where every technique errored returns the first of those errors,
// so [runCheckpoints] can name the lowest failing checkpoint and a failure never
// becomes a zero offset.
func evaluateCheckpoint(ctx context.Context, cfg Config, slicer Slicer, w Window) (Checkpoint, error) {
	checkpoint := Checkpoint{
		PositionPct:   w.PositionPct,
		SliceStartSec: w.FilmStartSec,
		SliceLenSec:   w.LenSec,
	}

	slice, err := slicer.Slice(ctx, w)
	if err != nil {
		return Checkpoint{}, err
	}
	// Every slice is released, success or not: a window implementation keeps at
	// most one checkpoint's files per worker.
	defer slicer.Release(w)

	var (
		attempts []Attempt
		failure  error
	)
	for _, entry := range cfg.entries() {
		if entry.When == WhenAboveDrift {
			continue
		}
		if entry.When == WhenBelowConfidence {
			if best, ok := winner(attempts); ok && best.Quality >= entry.MinConfidence {
				continue
			}
		}
		engine, ok := cfg.engineFor(entry.ID)
		if !ok {
			continue
		}
		result, err := runJob(ctx, engine, cfg, slice, w)
		attempt := Attempt{Technique: string(entry.ID)}
		if err != nil {
			attempt.Error = err.Error()
			attempts = append(attempts, attempt)
			if failure == nil {
				failure = err
			}
			continue
		}
		attempt.OffsetMS = result.OffsetMS
		attempt.OffsetSubMS = result.OffsetSubMS
		attempt.Quality = result.CorrelationQuality
		attempt.MatchZ = result.MatchZ
		attempt.PrecisionMS = result.PrecisionMS
		attempt.Accepted = accepts(entry, result)
		attempts = append(attempts, attempt)
		if attempt.Accepted {
			break
		}
	}

	chosen, ok := winner(attempts)
	if !ok {
		if failure != nil {
			return Checkpoint{}, failure
		}
		return Checkpoint{}, errNoTechniqueAnswered
	}
	checkpoint.OffsetMS = chosen.OffsetMS
	checkpoint.OffsetSubMS = chosen.OffsetSubMS
	checkpoint.Quality = chosen.Quality
	checkpoint.MatchZ = chosen.MatchZ
	checkpoint.PrecisionMS = chosen.PrecisionMS
	checkpoint.Technique = chosen.Technique
	checkpoint.Attempts = attempts
	return checkpoint, nil
}

// applyAboveDrift is phase 2: the one sequential pass that gives `above_drift`
// techniques a turn.
//
// It runs after the join because the question it answers — "does this
// checkpoint sit on the line through the rest?" — needs every checkpoint's
// first answer. The line is the same robust fit the report publishes, and a
// checkpoint whose residual exceeds [Thresholds.AboveDriftResidualMS] is the
// one the extra technique is tried on, so the cost of the step is bounded by
// the number of outliers (normally zero).
//
// Determinism: strictly checkpoint order, then list order, one pass, no second
// run per checkpoint. The caller re-fits once on the final offsets.
//
// A phase-2 technique can only improve a checkpoint it already has an answer
// for, so its errors are recorded and never fail the job. A cancelled caller
// context still does.
func applyAboveDrift(ctx context.Context, cfg Config, slicer Slicer, plan []Window, runs []checkpointRun) error {
	entries := cfg.aboveDriftEntries()
	if len(entries) == 0 {
		return nil
	}
	n := len(plan)
	if len(runs) < n {
		n = len(runs)
	}
	if n == 0 {
		return nil
	}

	offsets := make([]float64, n)
	qualities := make([]float64, n)
	matchZ := make([]float64, n)
	for i := 0; i < n; i++ {
		offsets[i] = runs[i].checkpoint.OffsetMS
		qualities[i] = runs[i].checkpoint.Quality
		matchZ[i] = runs[i].checkpoint.MatchZ
	}
	// The trend the verdict is about, fitted on the phase-1 answers. Phase 2
	// exists because a single bad checkpoint must not define the trend it is
	// judged against, which is why the estimator is robust (see [fitLine]).
	fit := FitTimeline(plan, offsets, qualities, matchZ, DefaultThresholds)

	for i := 0; i < n; i++ {
		residual := offsets[i] - (fit.SlopeMSPerSec*plan[i].FilmStartSec + fit.InterceptMS)
		if abs(residual) <= DefaultThresholds.AboveDriftResidualMS {
			continue
		}
		if err := applyAboveDriftToCheckpoint(ctx, cfg, entries, slicer, plan[i], &runs[i].checkpoint); err != nil {
			return err
		}
	}
	return nil
}

// applyAboveDriftToCheckpoint runs every `above_drift` technique on one outlier,
// in list order, keeping the best accepted answer.
//
// The window is cut once for all of them, and released when this checkpoint is
// done. The previous winner's accepted flag is cleared when it is replaced: the
// report must not claim two accepted answers for one checkpoint.
func applyAboveDriftToCheckpoint(ctx context.Context, cfg Config, entries []Technique, slicer Slicer, w Window, checkpoint *Checkpoint) error {
	slice, err := slicer.Slice(ctx, w)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("analyze: checkpoint %d%%: %w", w.PositionPct, cerr)
		}
		for _, entry := range entries {
			checkpoint.Attempts = append(checkpoint.Attempts, Attempt{Technique: string(entry.ID), Error: err.Error()})
		}
		return nil
	}
	defer slicer.Release(w)

	// The technique replaces an answer, it does not produce one: a checkpoint
	// with no phase-1 answer has nothing to improve.
	best, ok := winner(checkpoint.Attempts)
	if !ok {
		return nil
	}

	for _, entry := range entries {
		engine, ok := cfg.engineFor(entry.ID)
		if !ok {
			continue
		}
		result, err := runJob(ctx, engine, cfg, slice, w)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return fmt.Errorf("analyze: checkpoint %d%%: %w", w.PositionPct, cerr)
			}
			checkpoint.Attempts = append(checkpoint.Attempts, Attempt{Technique: string(entry.ID), Error: err.Error()})
			continue
		}
		attempt := Attempt{
			Technique:   string(entry.ID),
			OffsetMS:    result.OffsetMS,
			OffsetSubMS: result.OffsetSubMS,
			Quality:     result.CorrelationQuality,
			MatchZ:      result.MatchZ,
			PrecisionMS: result.PrecisionMS,
		}
		attempt.Accepted = accepts(entry, result)
		if attempt.Accepted && attempt.Quality > best.Quality {
			for k := range checkpoint.Attempts {
				checkpoint.Attempts[k].Accepted = false
			}
			checkpoint.OffsetMS = attempt.OffsetMS
			checkpoint.OffsetSubMS = attempt.OffsetSubMS
			checkpoint.Quality = attempt.Quality
			checkpoint.MatchZ = attempt.MatchZ
			checkpoint.PrecisionMS = attempt.PrecisionMS
			checkpoint.Technique = attempt.Technique
			best = attempt
		}
		checkpoint.Attempts = append(checkpoint.Attempts, attempt)
	}
	return nil
}
