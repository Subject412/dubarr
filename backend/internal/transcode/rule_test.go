package transcode

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func intPtr(v int) *int { return &v }

func TestMatchFirstMatchWins(t *testing.T) {
	rules := []Rule{
		{Enabled: true, When: When{SourceCodecs: []string{"pcm_s16le"}}, Then: Then{Codec: CodecOpus, BitrateBPS: intPtr(128000)}},
		{Enabled: true, When: When{}, Then: Then{Codec: CodecAAC}},
	}
	plan, ok := Match(rules, TrackMeta{Codec: "pcm_s16le", ChannelCount: 2})
	if !ok {
		t.Fatal("Match = no match, want the first rule")
	}
	if plan.RuleIndex != 0 || plan.Codec != CodecOpus || plan.BitrateBPS != 128000 {
		t.Fatalf("plan = %+v, want rule 0 opus 128k", plan)
	}
	if plan.Encoder() != "libopus" {
		t.Fatalf("encoder = %q, want libopus", plan.Encoder())
	}
}

func TestMatchDisabledRuleIsSkipped(t *testing.T) {
	rules := []Rule{
		{Enabled: false, When: When{}, Then: Then{Codec: CodecOpus}},
		{Enabled: true, When: When{}, Then: Then{Codec: CodecFLAC}},
	}
	plan, ok := Match(rules, TrackMeta{Codec: "pcm_s16le"})
	if !ok || plan.RuleIndex != 1 || plan.Codec != CodecFLAC {
		t.Fatalf("Match = %+v/%v, want rule 1 flac", plan, ok)
	}
}

func TestMatchSourceCodecListIsMembership(t *testing.T) {
	rules := []Rule{{
		Enabled: true,
		When:    When{SourceCodecs: []string{"ac3", "eac3"}},
		Then:    Then{Codec: CodecOpus},
	}}
	if _, ok := Match(rules, TrackMeta{Codec: "EAC3"}); !ok {
		t.Fatal("codec list should match any member (case-insensitive)")
	}
	if _, ok := Match(rules, TrackMeta{Codec: "truehd"}); ok {
		t.Fatal("codec outside the list must not match")
	}
}

func TestMatchRegex(t *testing.T) {
	rules := []Rule{{Enabled: true, When: When{SourceCodecRegex: `^pcm_`}, Then: Then{Codec: CodecFLAC}}}
	if _, ok := Match(rules, TrackMeta{Codec: "pcm_s24le"}); !ok {
		t.Fatal("regex should match pcm_s24le")
	}
	if _, ok := Match(rules, TrackMeta{Codec: "eac3"}); ok {
		t.Fatal("regex should not match eac3")
	}
}

func TestMatchBitrateThresholds(t *testing.T) {
	tests := []struct {
		name string
		when When
		meta TrackMeta
		want bool
	}{
		{
			name: "min is inclusive lower bound",
			when: When{MinBitrateBPS: intPtr(1536000)},
			meta: TrackMeta{BitrateBPS: 1536000},
			want: true,
		},
		{
			name: "below min does not match",
			when: When{MinBitrateBPS: intPtr(1536000)},
			meta: TrackMeta{BitrateBPS: 1535999},
			want: false,
		},
		{
			name: "max is inclusive upper bound",
			when: When{MaxBitrateBPS: intPtr(96000)},
			meta: TrackMeta{BitrateBPS: 96000},
			want: true,
		},
		{
			name: "above max does not match",
			when: When{MaxBitrateBPS: intPtr(96000)},
			meta: TrackMeta{BitrateBPS: 96001},
			want: false,
		},
		{
			name: "min and max bracket a range",
			when: When{MinBitrateBPS: intPtr(64000), MaxBitrateBPS: intPtr(128000)},
			meta: TrackMeta{BitrateBPS: 96000},
			want: true,
		},
		{
			name: "unknown bitrate never satisfies a threshold",
			when: When{MaxBitrateBPS: intPtr(512000)},
			meta: TrackMeta{BitrateBPS: 0},
			want: false,
		},
		{
			name: "no threshold ignores a missing bitrate",
			when: When{},
			meta: TrackMeta{BitrateBPS: 0},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Match([]Rule{{Enabled: true, When: tc.when, Then: Then{Codec: CodecOpus}}}, tc.meta)
			if ok != tc.want {
				t.Fatalf("Match ok = %v, want %v (plan %+v)", ok, tc.want, got)
			}
		})
	}
}

func TestMatchNoRuleCopiesUntouched(t *testing.T) {
	plan, ok := Match(nil, TrackMeta{Codec: "pcm_s16le"})
	if ok || plan.Encode() {
		t.Fatalf("empty rules = %+v/%v, want no match and no encode", plan, ok)
	}
	plan, ok = Match([]Rule{{Enabled: true, When: When{SourceCodecs: []string{"truehd"}}, Then: Then{Codec: CodecOpus}}}, TrackMeta{Codec: "pcm_s16le"})
	if ok || plan.Encode() {
		t.Fatalf("unmatched = %+v/%v, want no match and no encode", plan, ok)
	}
}

func TestMatchTrackConditions(t *testing.T) {
	base := TrackMeta{Codec: "pcm_s16le", ChannelCount: 6, ChannelLayout: "5.1(side)", SampleRate: 48000, LangISO2: "pl", Variant: "dubbing"}
	tests := []struct {
		name string
		when When
		want bool
	}{
		{"channel count", When{ChannelCount: intPtr(6)}, true},
		{"channel count mismatch", When{ChannelCount: intPtr(2)}, false},
		{"layout by name", When{ChannelLayout: "5.1(side)"}, true},
		{"layout by channel count tolerates side variant", When{ChannelLayout: "5.1"}, true},
		{"layout mismatch", When{ChannelLayout: "stereo"}, false},
		{"sample rate", When{SampleRate: intPtr(48000)}, true},
		{"sample rate mismatch", When{SampleRate: intPtr(44100)}, false},
		{"language", When{LangISO2: "pl"}, true},
		{"language mismatch", When{LangISO2: "en"}, false},
		{"variant", When{Variant: "dubbing"}, true},
		{"variant mismatch", When{Variant: "commentary"}, false},
		{"all conditions AND", When{ChannelCount: intPtr(6), LangISO2: "pl", SampleRate: intPtr(48000)}, true},
		{"one false condition fails the AND", When{ChannelCount: intPtr(6), LangISO2: "en"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rules := []Rule{{Enabled: true, When: tc.when, Then: Then{Codec: CodecOpus}}}
			if _, ok := Match(rules, base); ok != tc.want {
				t.Fatalf("Match ok = %v, want %v", ok, tc.want)
			}
		})
	}
}

func TestPlanDerivesLayoutAndAction(t *testing.T) {
	rules := []Rule{{Enabled: true, When: When{SourceCodecs: []string{"pcm_s16le"}}, Then: Then{
		Codec: CodecOpus, BitrateBPS: intPtr(160000), Channels: intPtr(2), SampleRate: intPtr(48000),
	}}}
	plan, ok := Match(rules, TrackMeta{Codec: "pcm_s16le", ChannelCount: 6})
	if !ok {
		t.Fatal("expected a match")
	}
	if plan.Channels != 2 || plan.ChannelLayout != "stereo" || plan.SampleRate != 48000 || plan.BitrateBPS != 160000 {
		t.Fatalf("plan = %+v, want opus stereo 48k 160k", plan)
	}
	if plan.ExpectedCodec() != CodecOpus {
		t.Fatalf("expected codec = %q", plan.ExpectedCodec())
	}
}

func TestPlanKeepStopsFollowingRules(t *testing.T) {
	rules := []Rule{
		{Enabled: true, When: When{SourceCodecs: []string{"pcm_s16le"}}, Then: Then{Codec: CodecKeep}},
		{Enabled: true, When: When{}, Then: Then{Codec: CodecOpus}},
	}
	plan, ok := Match(rules, TrackMeta{Codec: "pcm_s16le"})
	if !ok || plan.Codec != CodecKeep || plan.Encode() {
		t.Fatalf("plan = %+v/%v, want an explicit keep that stops the list", plan, ok)
	}
}

func TestValidateOptions(t *testing.T) {
	valid := []Rule{
		{Enabled: true, When: When{SourceCodecs: []string{"pcm_s16le"}, MinBitrateBPS: intPtr(1536000)}, Then: Then{Codec: CodecOpus, BitrateBPS: intPtr(160000), Channels: intPtr(2)}},
		{Enabled: false, When: When{SourceCodecRegex: `^(truehd|dts_hd)$`, ChannelLayout: "7.1"}, Then: Then{Codec: CodecFLAC}},
		{Enabled: true, When: When{LangISO2: "pl", Variant: "dubbing"}, Then: Then{Codec: CodecKeep}},
		{Enabled: true, Then: Then{Codec: CodecPCMS16LE, SampleRate: intPtr(48000)}},
	}
	if err := ValidateOptions(&Options{Rules: valid}); err != nil {
		t.Fatalf("ValidateOptions(valid) = %v, want nil", err)
	}

	bad := []struct {
		name string
		rule Rule
		want string
	}{
		{"unknown codec", Rule{Then: Then{Codec: "mp3"}}, "then.codec"},
		{"empty codec", Rule{Then: Then{}}, "then.codec"},
		{"bad layout in action", Rule{Then: Then{Codec: CodecOpus, ChannelLayout: "5.1.2"}}, "ffmpeg layout"},
		{"bad layout in condition", Rule{When: When{ChannelLayout: "surround"}, Then: Then{Codec: CodecOpus}}, "ffmpeg layout"},
		{"negative min bitrate", Rule{When: When{MinBitrateBPS: intPtr(-1)}, Then: Then{Codec: CodecOpus}}, "min_bitrate_bps"},
		{"negative action bitrate", Rule{Then: Then{Codec: CodecOpus, BitrateBPS: intPtr(-5)}}, "bitrate_bps"},
		{"malformed regex", Rule{When: When{SourceCodecRegex: `([a-z`}, Then: Then{Codec: CodecOpus}}, "source_codec_regex"},
		{"min above max", Rule{When: When{MinBitrateBPS: intPtr(200000), MaxBitrateBPS: intPtr(100000)}, Then: Then{Codec: CodecOpus}}, "must not exceed"},
		{"keep with actions", Rule{Then: Then{Codec: CodecKeep, BitrateBPS: intPtr(96000)}}, "keep"},
		{"bitrate on lossless", Rule{Then: Then{Codec: CodecFLAC, BitrateBPS: intPtr(96000)}}, "lossless"},
		{"channels contradict layout", Rule{Then: Then{Codec: CodecOpus, Channels: intPtr(2), ChannelLayout: "5.1"}}, "contradicts"},
		{"channels out of range", Rule{Then: Then{Codec: CodecOpus, Channels: intPtr(12)}}, "then.channels"},
		{"bad language", Rule{When: When{LangISO2: "Polish"}, Then: Then{Codec: CodecOpus}}, "lang_iso2"},
		{"bad codec name in list", Rule{When: When{SourceCodecs: []string{"pcm s16le"}}, Then: Then{Codec: CodecOpus}}, "codec name"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateOptions(&Options{Rules: []Rule{tc.rule}})
			if err == nil {
				t.Fatal("ValidateOptions accepted an invalid rule")
			}
			if !errors.Is(err, ErrInvalidRule) {
				t.Fatalf("error %v is not ErrInvalidRule", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateOptionsCanonicalisesAndCaps(t *testing.T) {
	o := Options{Rules: []Rule{{Enabled: true, When: When{SourceCodecs: []string{" PCM_S16LE "}, SourceCodecRegex: `^pcm_`, ChannelLayout: " 5.1 ", LangISO2: " PL "}, Then: Then{Codec: " OPUS ", ChannelLayout: " Stereo "}}}}
	if err := ValidateOptions(&o); err != nil {
		t.Fatalf("ValidateOptions: %v", err)
	}
	got := o.Rules[0]
	if got.When.SourceCodecs[0] != "pcm_s16le" || got.When.ChannelLayout != "5.1" || got.When.LangISO2 != "pl" ||
		got.Then.Codec != CodecOpus || got.Then.ChannelLayout != "stereo" {
		t.Fatalf("canonical rule = %+v", got)
	}
	empty := Options{}
	if err := ValidateOptions(&empty); err != nil || empty.Rules == nil {
		t.Fatalf("zero options = %v/%#v, want empty non-nil rules", err, empty.Rules)
	}
	many := make([]Rule, 101)
	for i := range many {
		many[i] = Rule{Then: Then{Codec: CodecOpus}}
	}
	if err := ValidateOptions(&Options{Rules: many}); err == nil {
		t.Fatal("more than 100 rules accepted")
	}
}

func TestOptionsLegacyDecode(t *testing.T) {
	var o Options
	if err := json.Unmarshal([]byte(`{}`), &o); err != nil {
		t.Fatalf("decode legacy section: %v", err)
	}
	if o.Rules == nil || len(o.Rules) != 0 {
		t.Fatalf("legacy decode = %#v, want empty non-nil rules", o.Rules)
	}
	if err := json.Unmarshal([]byte(`{"rules":null}`), &o); err != nil {
		t.Fatalf("decode null rules: %v", err)
	}
	if o.Rules == nil {
		t.Fatal("null rules should decode as an empty list")
	}
	if err := json.Unmarshal([]byte(`{"rules":[],"typo":1}`), &o); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestValidateTempo(t *testing.T) {
	for _, f := range []float64{0, 1, 0.5, 2, 1.001} {
		if err := ValidateTempo(f); err != nil {
			t.Errorf("ValidateTempo(%v) = %v", f, err)
		}
	}
	for _, f := range []float64{0.4, 2.1, -1} {
		if err := ValidateTempo(f); err == nil {
			t.Errorf("ValidateTempo(%v) accepted", f)
		}
	}
}

// TestTempoOnlyPlanBuildsOnePass is the #86 encode contract: a plan no rule
// produced (RuleIndex == NoRule) whose only job is the drift correction still
// renders as ONE ffmpeg invocation — atempo first, then the codec — not a
// tempo pass followed by an encode pass.
func TestTempoOnlyPlanBuildsOnePass(t *testing.T) {
	plan := Plan{RuleIndex: NoRule, Codec: CodecFLAC}.WithTempo(1.0427)
	if !plan.Encode() {
		t.Fatal("tempo-only plan does not encode")
	}
	if plan.Encoder() != "flac" {
		t.Fatalf("encoder = %q, want flac", plan.Encoder())
	}
	args, err := BuildArgs(Request{Input: "in.mka", Output: "out.mka", Plan: plan})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}

	var inputs int
	for _, arg := range args {
		if arg == "-i" {
			inputs++
		}
	}
	if inputs != 1 {
		t.Fatalf("argv has %d inputs, want exactly 1: %v", inputs, args)
	}

	atempo, codec := -1, -1
	var atempoCount int
	for i, arg := range args {
		switch arg {
		case "atempo=1.042700":
			atempo = i
			atempoCount++
		case "-c:a":
			codec = i
		}
	}
	if atempoCount != 1 || atempo < 0 {
		t.Fatalf("argv has %d atempo filters, want exactly one atempo=1.042700: %v", atempoCount, args)
	}
	if codec < 0 || atempo > codec {
		t.Fatalf("atempo at %d is not before -c:a at %d: %v", atempo, codec, args)
	}
	if codec+1 >= len(args) || args[codec+1] != "flac" {
		t.Fatalf("argv does not encode to flac after -c:a: %v", args)
	}
}
