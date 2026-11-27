package analyze

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/sync"
)

// defaultEntry is the shipped technique, spelled out so a change to
// [DefaultStrategy] fails here with a diff an operator can read.
func defaultEntry() Technique {
	return Technique{
		ID:            TechniqueWaveform,
		Enabled:       true,
		When:          WhenAlways,
		MinConfidence: 0.10,
		MinMatchZ:     5.0,
		ZGate:         true,
	}
}

// alwaysEntry is one enabled entry with the shipped thresholds.
func alwaysEntry(id TechniqueID, when When) Technique {
	return Technique{ID: id, Enabled: true, When: when, MinConfidence: 0.10, MinMatchZ: 5.0, ZGate: true}
}

// disabledEntry is one disabled entry with the shipped thresholds; disabled
// entries stay in the list but never run.
func disabledEntry(id TechniqueID, when When) Technique {
	return Technique{ID: id, Enabled: false, When: when, MinConfidence: 0.10, MinMatchZ: 5.0, ZGate: true}
}

func strategyOf(preset string, entries ...Technique) Strategy {
	return Strategy{Preset: preset, Techniques: entries}
}

// TestDefaultStrategyIsOneWaveformEntry is the D4 contract: 0.5.0 ships exactly
// one enabled entry — the waveform matcher, on every checkpoint — so every
// offset is byte-identical to 0.4.0 until the operator adds a technique. A
// change here is a release-note event, not a refactor.
func TestDefaultStrategyIsOneWaveformEntry(t *testing.T) {
	s := DefaultStrategy()
	if s.Preset != "balanced" {
		t.Fatalf("preset = %q, want %q", s.Preset, "balanced")
	}
	if len(s.Techniques) != 1 {
		t.Fatalf("techniques = %d, want 1: %+v", len(s.Techniques), s.Techniques)
	}
	entry := s.Techniques[0]
	if entry.ID != TechniqueWaveform {
		t.Fatalf("id = %q, want %q", entry.ID, TechniqueWaveform)
	}
	if !entry.Enabled {
		t.Fatal("the shipped entry is disabled; the default pipeline would run nothing")
	}
	if entry.When != WhenAlways {
		t.Fatalf("when = %q, want %q", entry.When, WhenAlways)
	}
	if entry.MinConfidence != 0.10 {
		t.Fatalf("min_confidence = %v, want 0.10", entry.MinConfidence)
	}
	if entry.MinMatchZ != 5.0 {
		t.Fatalf("min_match_z = %v, want 5.0", entry.MinMatchZ)
	}
	if !entry.ZGate {
		t.Fatal("the shipped entry is not z-gated: a weak peak would no longer be refused")
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("DefaultStrategy().Validate() = %v, want nil", err)
	}
	if got := s.Enabled(); len(got) != 1 || got[0] != entry {
		t.Fatalf("Enabled() = %+v, want the one shipped entry", got)
	}
}

// TestDefaultStrategyIsAFreshCopy: a caller that edits the returned strategy
// must not corrupt the next job's default.
func TestDefaultStrategyIsAFreshCopy(t *testing.T) {
	first := DefaultStrategy()
	first.Techniques[0].MinConfidence = 0.9
	if got := DefaultStrategy().Techniques[0].MinConfidence; got != 0.10 {
		t.Fatalf("DefaultStrategy() shares its techniques slice: min_confidence = %v, want 0.10", got)
	}
}

// TestStrategyValidateTable is one case per rule the settings layer enforces,
// each asserting the operator-facing substring.
func TestStrategyValidateTable(t *testing.T) {
	cases := []struct {
		name    string
		s       Strategy
		wantErr string
	}{
		{"no techniques", strategyOf(PresetBalanced), "techniques must have 1..4 entries"},
		{"one entry is the shipped shape", strategyOf(PresetBalanced, defaultEntry()), ""},
		{"every allowed when is accepted", strategyOf(PresetCustom, alwaysEntry(TechniqueWaveform, WhenAlways), alwaysEntry(TechniqueMFCC, WhenBelowConfidence)), ""},
		{"above_drift after an always step", strategyOf(PresetCustom, alwaysEntry(TechniqueWaveform, WhenAlways), alwaysEntry(TechniqueMFCC, WhenAboveDrift)), ""},
		{"a disabled extra entry is allowed", strategyOf(PresetCustom, alwaysEntry(TechniqueWaveform, WhenAlways), disabledEntry(TechniqueMFCC, WhenBelowConfidence)), ""},
		{"five entries", strategyOf(PresetCustom, alwaysEntry(TechniqueWaveform, WhenAlways), alwaysEntry(TechniqueMFCC, WhenAlways), alwaysEntry(TechniqueWaveform, WhenBelowConfidence), alwaysEntry(TechniqueMFCC, WhenAboveDrift), alwaysEntry(TechniqueWaveform, WhenAlways)), "techniques must have 1..4 entries"},
		{"unknown technique", strategyOf(PresetCustom, alwaysEntry("bogus", WhenAlways)), `unknown technique "bogus"`},
		{"duplicate technique", strategyOf(PresetCustom, alwaysEntry(TechniqueWaveform, WhenAlways), alwaysEntry(TechniqueWaveform, WhenBelowConfidence)), `duplicate technique "waveform"`},
		{"nothing enabled", strategyOf(PresetCustom, disabledEntry(TechniqueWaveform, WhenAlways)), "at least one technique must be enabled"},
		{"the first enabled step is not always", strategyOf(PresetCustom, alwaysEntry(TechniqueWaveform, WhenAboveDrift), alwaysEntry(TechniqueMFCC, WhenAlways)), "the first enabled technique must run always"},
		{"a disabled first step does not count as the first enabled one", strategyOf(PresetCustom, disabledEntry(TechniqueWaveform, WhenAlways), alwaysEntry(TechniqueMFCC, WhenAboveDrift)), "the first enabled technique must run always"},
		{"below_confidence needs an earlier step", strategyOf(PresetCustom, alwaysEntry(TechniqueMFCC, WhenBelowConfidence)), `technique "mfcc" needs an earlier step to react to`},
		{"unknown when", strategyOf(PresetCustom, Technique{ID: TechniqueWaveform, Enabled: true, When: "sometimes", MinConfidence: 0.10, MinMatchZ: 5.0}), "when must be always, below_confidence or above_drift"},
		{"min_confidence above 1", strategyOf(PresetCustom, Technique{ID: TechniqueWaveform, Enabled: true, When: WhenAlways, MinConfidence: 1.5, MinMatchZ: 5.0}), "min_confidence must be 0..1"},
		{"min_confidence below 0", strategyOf(PresetCustom, Technique{ID: TechniqueWaveform, Enabled: true, When: WhenAlways, MinConfidence: -0.1, MinMatchZ: 5.0}), "min_confidence must be 0..1"},
		{"min_match_z above 100", strategyOf(PresetCustom, Technique{ID: TechniqueWaveform, Enabled: true, When: WhenAlways, MinConfidence: 0.10, MinMatchZ: 101, ZGate: true}), "min_match_z must be 0..100"},
		{"min_match_z below 0", strategyOf(PresetCustom, Technique{ID: TechniqueWaveform, Enabled: true, When: WhenAlways, MinConfidence: 0.10, MinMatchZ: -1, ZGate: true}), "min_match_z must be 0..100"},
		{"a z-gated entry with no z band", strategyOf(PresetCustom, Technique{ID: TechniqueWaveform, Enabled: true, When: WhenAlways, MinConfidence: 0.10, MinMatchZ: 0, ZGate: true}), "min_match_z must be above 0"},
		{"a quality-gated entry needs no z band", strategyOf(PresetCustom, Technique{ID: TechniqueMFCC, Enabled: true, When: WhenAlways, MinConfidence: 0.15, MinMatchZ: 0, ZGate: false}), ""},
		{"a quality-gated entry may carry an ignored z band", strategyOf(PresetCustom, Technique{ID: TechniqueMFCC, Enabled: true, When: WhenAlways, MinConfidence: 0.15, MinMatchZ: 8, ZGate: false}), ""},
		{"unknown preset", strategyOf("turbo", defaultEntry()), "preset must be fast, balanced, maximum_accuracy or custom"},
		{"an unlabelled strategy is a custom one", strategyOf("", defaultEntry()), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestStrategyEnabledPreservesOrder: routing follows the list the operator
// wrote, never a map's iteration order.
func TestStrategyEnabledPreservesOrder(t *testing.T) {
	s := strategyOf(PresetCustom,
		alwaysEntry(TechniqueWaveform, WhenAlways),
		Technique{ID: TechniqueMFCC, Enabled: false, When: WhenBelowConfidence, MinConfidence: 0.15, MinMatchZ: 8.0},
		Technique{ID: TechniqueWaveform, Enabled: true, When: WhenBelowConfidence, MinConfidence: 0.20, MinMatchZ: 9.0},
	)
	got := s.Enabled()
	if len(got) != 2 {
		t.Fatalf("Enabled() = %d entries, want 2: %+v", len(got), got)
	}
	if got[0] != s.Techniques[0] || got[1] != s.Techniques[2] {
		t.Fatalf("Enabled() = %+v, want entries 0 and 2 in list order", got)
	}
}

// TestStrategyNormalisedFallsBackToTheDefault: the zero value is the shipped
// pipeline, so every caller that never heard of a strategy keeps 0.4.0's
// behaviour.
func TestStrategyNormalisedFallsBackToTheDefault(t *testing.T) {
	if got, want := (Strategy{}).normalised(), DefaultStrategy(); !reflect.DeepEqual(got, want) {
		t.Fatalf("zero Strategy normalised to %+v, want %+v", got, want)
	}
	custom := strategyOf(PresetCustom, defaultEntry())
	if got := custom.normalised(); !reflect.DeepEqual(got, custom) {
		t.Fatalf("a configured strategy was replaced: %+v", got)
	}
}

// TestAcceptsNeedsBothGates: an answer is good enough when its quality clears
// this entry's floor AND, for an entry that is z-gated, its peak either stands
// out enough or was never scored (a zero z means "this technique does not report
// one", not "unreliable"). The z gate is per technique: an entry with ZGate
// false is judged on quality alone, whatever z it reports. Only the confidence
// floor is always consulted.
func TestAcceptsNeedsBothGates(t *testing.T) {
	entry := defaultEntry() // min_confidence 0.10, min_match_z 5.0, z-gated
	cases := []struct {
		name       string
		quality, z float64
		want       bool
	}{
		{"quality alone clears the floor", 0.5, 0, true},
		{"a missing z is not a failure", 0.5, 0, true},
		{"confidence below the floor is refused", 0.05, 30, false},
		{"a weak peak is refused", 0.5, 4.9, false},
		{"the z gate is inclusive", 0.5, 5.0, true},
		{"both gates at their edges", 0.10, 5.0, true},
		{"quality at the floor", 0.10, 0, true},
		{"quality just below the floor", 0.0999999, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := accepts(entry, sync.Result{CorrelationQuality: tc.quality, MatchZ: tc.z})
			if got != tc.want {
				t.Fatalf("accepts(quality %v, z %v) = %v, want %v", tc.quality, tc.z, got, tc.want)
			}
		})
	}

	// The same entry with the z gate off — how MFCC is judged — accepts every
	// one of those answers on quality alone, and is still refused below its own
	// confidence floor.
	ungated := entry
	ungated.ZGate = false
	for _, z := range []float64{0, 1.0, 4.9, 5.8, 8.0, 30} {
		if !accepts(ungated, sync.Result{CorrelationQuality: 0.5, MatchZ: z}) {
			t.Fatalf("accepts(quality 0.5, z %v) = false for a quality-gated entry, want true", z)
		}
	}
	if accepts(ungated, sync.Result{CorrelationQuality: 0.05, MatchZ: 30}) {
		t.Fatal("accepts() = true under the confidence floor, want false: quality is always a gate")
	}
}

// TestWinnerIsTheFirstAcceptedThenTheHealthiest is the deterministic winner
// rule the report depends on: the first accepted answer in list order, and
// otherwise the healthiest answer, ties going to the earlier entry. Two
// identical inputs must produce the same winner, whatever a map does.
func TestWinnerIsTheFirstAcceptedThenTheHealthiest(t *testing.T) {
	attempt := func(id TechniqueID, quality float64, accepted bool) Attempt {
		return Attempt{Technique: string(id), OffsetMS: 1, Quality: quality, Accepted: accepted}
	}
	cases := []struct {
		name     string
		attempts []Attempt
		want     string
	}{
		{"the accepted answer wins over a healthier unaccepted one", []Attempt{attempt(TechniqueWaveform, 0.9, false), attempt(TechniqueMFCC, 0.2, true)}, string(TechniqueMFCC)},
		{"equal confidence resolves to the earlier entry", []Attempt{attempt(TechniqueWaveform, 0.5, false), attempt(TechniqueMFCC, 0.5, false)}, string(TechniqueWaveform)},
		{"the healthier answer wins even when listed second", []Attempt{attempt(TechniqueWaveform, 0.4, false), attempt(TechniqueMFCC, 0.6, false)}, string(TechniqueMFCC)},
		{"an errored technique is never an answer", []Attempt{{Technique: string(TechniqueWaveform), OffsetMS: 0, Error: "decode failed"}}, ""},
		{"no attempts at all", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := winner(tc.attempts)
			if tc.want == "" {
				if ok {
					t.Fatalf("winner = %+v, want none", got)
				}
				return
			}
			if !ok {
				t.Fatalf("winner = none, want %q", tc.want)
			}
			if got.Technique != tc.want {
				t.Fatalf("winner = %q, want %q", got.Technique, tc.want)
			}
		})
	}
}

// TestWinnerIsDeterministic is the invariant stated as a loop: the same list
// answers the same winner however many times it is asked.
func TestWinnerIsDeterministic(t *testing.T) {
	attempts := []Attempt{
		{Technique: string(TechniqueWaveform), Quality: 0.5},
		{Technique: string(TechniqueMFCC), Quality: 0.5},
	}
	first, _ := winner(attempts)
	for i := 0; i < 100; i++ {
		got, _ := winner(attempts)
		if got != first {
			t.Fatalf("run %d: winner = %+v, want %+v", i, got, first)
		}
	}
}

// TestKnownTechniqueIDs pins which ids a strategy may name: `mfcc` is accepted
// before slice 04 registers an engine for it, and anything else is a typo the
// settings layer rejects.
func TestKnownTechniqueIDs(t *testing.T) {
	for _, id := range []TechniqueID{TechniqueWaveform, TechniqueMFCC} {
		if !id.Known() {
			t.Errorf("%q is not known, want it accepted", id)
		}
	}
	for _, id := range []TechniqueID{"", "Waveform", "chroma", "bogus"} {
		if id.Known() {
			t.Errorf("%q is known, want it rejected", id)
		}
	}
}
