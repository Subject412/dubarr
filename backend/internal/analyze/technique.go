package analyze

import (
	"errors"
	"fmt"

	"github.com/Subject412/dubarr/backend/internal/sync"
)

// This file holds the sync strategy: the ordered list of techniques a job may
// run on a checkpoint and the conditions under which each one runs (#82, D4).
//
// The shipped strategy is exactly one entry — waveform, on every checkpoint —
// so a 0.5.0 job answers with the offsets 0.4.0 produced until the operator
// adds a technique. Everything here is policy: the routing that consumes it
// lives in pipeline.go.

// TechniqueID names a matcher the pipeline can run.
type TechniqueID string

const (
	// TechniqueWaveform is the FFT cross-correlation of the raw samples: the
	// only matcher 0.4.0 had, sample-accurate at the analysis rate.
	TechniqueWaveform TechniqueID = "waveform"
	// TechniqueMFCC is the mel-cepstral matcher added by issue #83: more
	// tolerant of re-encoding and broadcast processing, hop-limited precision.
	TechniqueMFCC TechniqueID = "mfcc"
)

// Known reports whether id names a technique this release understands. It is a
// schema question, not an availability one: `mfcc` is known before slice 04
// registers an engine for it, so a strategy may name it while the runner skips
// it with a note (see [Analyze]).
func (id TechniqueID) Known() bool {
	switch id {
	case TechniqueWaveform, TechniqueMFCC:
		return true
	}
	return false
}

// When decides whether a technique runs on a checkpoint at all.
type When string

const (
	// WhenAlways runs the technique on every checkpoint.
	WhenAlways When = "always"
	// WhenBelowConfidence runs it only when the best answer so far on this
	// checkpoint is weaker than MinConfidence (a second opinion).
	WhenBelowConfidence When = "below_confidence"
	// WhenAboveDrift runs it only on a checkpoint that does not sit on the
	// timeline's fitted line (see [applyAboveDrift]).
	WhenAboveDrift When = "above_drift"
)

// Known reports whether w is a condition the pipeline implements.
func (w When) Known() bool {
	switch w {
	case WhenAlways, WhenBelowConfidence, WhenAboveDrift:
		return true
	}
	return false
}

// Strategy presets. A preset is a label on the list, never a second source of
// truth: the server stores whichever name matches the section it was given and
// calls anything else `custom` (see settings.Preset, which is the one place a
// preset's contents exist). The checkpoint count a preset implies is a
// sync.defaults concern and lives there, not here.
const (
	PresetFast            = "fast"
	PresetBalanced        = "balanced"
	PresetMaximumAccuracy = "maximum_accuracy"
	PresetCustom          = "custom"
)

// Technique is one entry of the strategy list.
type Technique struct {
	ID      TechniqueID `json:"id"`
	Enabled bool        `json:"enabled"`
	When    When        `json:"when"`
	// MinConfidence and MinMatchZ are this entry's acceptance gates: an answer
	// is good enough when Quality >= MinConfidence AND, for an entry whose
	// ZGate is true, (MatchZ == 0 OR MatchZ >= MinMatchZ). A zero MatchZ means
	// the technique does not report one, and is not treated as a failure.
	MinConfidence float64 `json:"min_confidence"`
	MinMatchZ     float64 `json:"min_match_z"`
	// ZGate says whether MinMatchZ gates acceptance. It is per technique, not a
	// policy knob, because the z is only comparable within one matcher: the
	// sample correlator scores ~8·10⁴ near-independent lags while a hop-based
	// matcher scores a few thousand overlapping ones, so its peak stands out by
	// ~3x less for the same correct answer (5.3 against 21-28 on committed
	// audio). An entry with ZGate false is judged on quality alone and records
	// its MatchZ as a diagnostic; a zero band there means "not used", so the
	// value is carried and ignored. Always written, never omitted: an operator
	// reading a stored strategy must see which rule applied.
	ZGate bool `json:"z_gate"`
}

// Strategy is the whole pipeline policy: an ordered list of techniques plus the
// name of the starting point it came from.
//
// Piecewise is the segmentation block (#84). It travels inside the section —
// one stored document, one validation, one preset — rather than as a second
// settings row, and it is omitzero so a report's strategy block is unchanged
// byte for byte while the feature is off at the shipped numbers (D5).
type Strategy struct {
	Preset     string          `json:"preset"`
	Techniques []Technique     `json:"techniques"`
	Piecewise  PiecewisePolicy `json:"piecewise,omitzero"`
}

// DefaultStrategy is the D4 guarantee: exactly one enabled entry, the waveform
// matcher, on every checkpoint, with the thresholds 0.4.0's single gate had
// (quality 0.10) and the shipped "needs a human look" z (5.0). A change here
// changes every offset the analyser produces, so it is a release-note event and
// is asserted by name in TestDefaultStrategyIsOneWaveformEntry.
//
// Its piecewise block is the documented default at `enabled: false` (D5):
// segmentation is an opt-in that re-encodes the operator's dub copy, so a fresh
// install samples six points and produces 0.4.0's report.
func DefaultStrategy() Strategy {
	return Strategy{
		Preset: PresetBalanced,
		Techniques: []Technique{{
			ID:            TechniqueWaveform,
			Enabled:       true,
			When:          WhenAlways,
			MinConfidence: LowQualityThreshold,
			MinMatchZ:     DefaultThresholds.MatchZMaybe,
			ZGate:         true,
		}},
		Piecewise: DefaultPiecewisePolicy(),
	}
}

// normalised resolves the zero value to [DefaultStrategy]: a caller that has
// never heard of a strategy gets 0.4.0's behaviour rather than an empty
// pipeline. Any non-empty list is returned as it stands; validation is
// [Strategy.Validate]'s job.
func (s Strategy) normalised() Strategy {
	if len(s.Techniques) == 0 {
		return DefaultStrategy()
	}
	return s
}

// Enabled returns the enabled entries in list order. Routing follows the list
// the operator wrote and nothing else, so two techniques with equal confidence
// always resolve to the same winner.
func (s Strategy) Enabled() []Technique {
	out := make([]Technique, 0, len(s.Techniques))
	for _, entry := range s.Techniques {
		if entry.Enabled {
			out = append(out, entry)
		}
	}
	return out
}

// Validate enforces the shape a strategy must have to be runnable. The messages
// are the operator-facing ones the settings layer returns on a rejected save —
// the settings layer prefixes them with the section key — so they are spelled
// out here rather than re-worded per caller.
//
// An empty Preset is accepted and means "unlabelled": the settings layer owns
// the preset names and their canonical lists, so it resolves the label; the
// pipeline only needs the techniques to be sound.
func (s Strategy) Validate() error {
	entries := s.Techniques
	if len(entries) < 1 || len(entries) > 4 {
		return errors.New("techniques must have 1..4 entries")
	}

	seen := make(map[TechniqueID]bool, len(entries))
	for _, entry := range entries {
		if !entry.ID.Known() {
			return fmt.Errorf("unknown technique %q", entry.ID)
		}
		if seen[entry.ID] {
			return fmt.Errorf("duplicate technique %q", entry.ID)
		}
		seen[entry.ID] = true
		if !entry.When.Known() {
			return fmt.Errorf("technique %q: when must be always, below_confidence or above_drift", entry.ID)
		}
		if entry.MinConfidence < 0 || entry.MinConfidence > 1 {
			return errors.New("min_confidence must be 0..1")
		}
		if entry.MinMatchZ < 0 || entry.MinMatchZ > 100 {
			return errors.New("min_match_z must be 0..100")
		}
		// A z-gated entry must carry a band to be gated by. Zero is reserved for
		// the quality-gated spelling, where the number is a diagnostic and no
		// band is applied.
		if entry.ZGate && entry.MinMatchZ <= 0 {
			return fmt.Errorf("technique %q: min_match_z must be above 0 for a z-gated technique (set z_gate false to judge on quality alone)", entry.ID)
		}
	}

	enabled := s.Enabled()
	if len(enabled) == 0 {
		return errors.New("at least one technique must be enabled")
	}
	// A second opinion reacts to an earlier answer, so it needs one: a
	// below_confidence step with nothing before it can never run usefully.
	if enabled[0].When == WhenBelowConfidence {
		return fmt.Errorf("technique %q needs an earlier step to react to", enabled[0].ID)
	}
	// The first enabled step must run unconditionally, otherwise a checkpoint
	// could reach the end of the list with no answer at all — and every later
	// step ("the best answer so far") would have nothing to compare against.
	if enabled[0].When != WhenAlways {
		return errors.New("the first enabled technique must run always")
	}

	switch s.Preset {
	case "", PresetFast, PresetBalanced, PresetMaximumAccuracy, PresetCustom:
	default:
		return errors.New("preset must be fast, balanced, maximum_accuracy or custom")
	}
	// The piecewise block travels in this section, so its ranges are this
	// method's too: one validation for the whole document, whichever caller
	// (settings on save, the analyser before a job) asks. An absent block is
	// legal and means the documented defaults.
	return s.Piecewise.Normalised().validate()
}

// accepts reports whether one technique's answer is good enough under its own
// barriers: the confidence gate is always a floor on correlation quality, and
// the z gate — only for an entry that is z-gated — is a floor on how far the
// peak stands above the rest of the lag curve.
//
// The z gate is per technique because z is only comparable within one matcher.
// MFCC scores a few thousand heavily overlapping hop lags where the sample
// correlator scores ~8·10⁴ near-independent ones, so a correct mel-cepstral
// answer stands out by ~3x less (5.3 against 21-28 on the same committed pair).
// Gating it on the waveform band would refuse the answers it exists for, so its
// entry carries ZGate false: MinConfidence decides, and the z is recorded in the
// attempt as a diagnostic.
//
// A zero MatchZ means the engine did not score one (and is what a 0.4.0-shaped
// result carries), so it cannot be evidence of a bad match: only a recorded z
// can fail that gate.
func accepts(entry Technique, r sync.Result) bool {
	if r.CorrelationQuality < entry.MinConfidence {
		return false
	}
	if entry.ZGate && r.MatchZ != 0 && r.MatchZ < entry.MinMatchZ {
		return false
	}
	return true
}

// winner applies the deterministic rule the report depends on: the first
// accepted answer in list order wins, and when nothing was accepted the
// healthiest answer does, ties going to the earlier entry. A failed attempt is
// never an answer — an engine error must not become a zero offset.
func winner(attempts []Attempt) (Attempt, bool) {
	for _, attempt := range attempts {
		if attempt.Accepted {
			return attempt, true
		}
	}
	best := -1
	for i, attempt := range attempts {
		if attempt.Error != "" {
			continue
		}
		// Strictly greater, so equal qualities keep the earlier entry.
		if best < 0 || attempt.Quality > attempts[best].Quality {
			best = i
		}
	}
	if best < 0 {
		return Attempt{}, false
	}
	return attempts[best], true
}
