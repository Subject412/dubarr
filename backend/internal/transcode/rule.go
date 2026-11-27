// Package transcode implements opt-in Audio Transcode Rules (ADR-0010, issue
// #11): ordered, first-match-wins rules that change the codec, channels,
// bitrate or sample rate of the vault track injected into the Target MKV.
//
// Rules are evaluated against PRE-TEMPO vault-track metadata (audio_tracks:
// codec, channel_count/channels, bitrate_bps, sample_rate, lang_iso2, variant),
// never against the file after a tempo/FPS adjustment, so a rule means the same
// thing regardless of how far the drift analysis had to stretch the track
// (Q19). An empty or absent rule list preserves the lossless `-c:a copy`
// behaviour.
//
// The Audio Vault stays write-once (ADR-0002): callers apply a Plan to the Work
// Area copy of a verified vault track only. This package never touches vault
// paths — it reads the input it is given and writes the output path it is
// given.
//
// One encode, two adjustments: a matched rule and a tempo/FPS correction are
// folded into a SINGLE ffmpeg invocation. Precedence is fixed in [BuildArgs]:
// tempo (atempo) → channel layout/rate conversion (aformat, -ar) → codec and
// bitrate (-c:a, -b:a). The mux path today applies drift as an mkvmerge
// `--sync` offset (no re-encode), so nothing is transcoded twice; any future
// tempo encode must be folded into this same command rather than added as a
// second generation loss (Q20).
package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Codec action values accepted in a rule's `then.codec`. "keep" is an explicit
// no-op: the track is copied untouched just like a non-matching track.
const (
	CodecKeep     = "keep"
	CodecOpus     = "opus"
	CodecAAC      = "aac"
	CodecFLAC     = "flac"
	CodecPCMS16LE = "pcm_s16le"
	CodecPCMS24LE = "pcm_s24le"
	CodecPCMF32LE = "pcm_f32le"
)

// encoders maps each action onto the ffmpeg encoder name. PCM actions are their
// own encoder names, so a missing encoder is reported per exact format.
var encoders = map[string]string{
	CodecOpus:     "libopus",
	CodecAAC:      "aac",
	CodecFLAC:     "flac",
	CodecPCMS16LE: "pcm_s16le",
	CodecPCMS24LE: "pcm_s24le",
	CodecPCMF32LE: "pcm_f32le",
}

// lossyCodecs may carry a target bitrate; flac and PCM are lossless and must
// not pretend to honour one.
var lossyCodecs = map[string]bool{CodecOpus: true, CodecAAC: true}

// LayoutChannels maps an ffmpeg channel-layout name onto its channel count.
// Only canonical ffmpeg names are accepted, so a typo is rejected at save time
// instead of silently never matching or failing inside ffmpeg.
var LayoutChannels = map[string]int{
	"mono":           1,
	"stereo":         2,
	"2.0":            2,
	"2.1":            3,
	"3.0":            3,
	"3.0(back)":      3,
	"3.1":            4,
	"4.0":            4,
	"quad":           4,
	"quad(side)":     4,
	"4.1":            5,
	"5.0":            5,
	"5.0(side)":      5,
	"5.1":            6,
	"5.1(side)":      6,
	"6.0":            6,
	"6.0(front)":     6,
	"hexagonal":      6,
	"6.1":            7,
	"6.1(back)":      7,
	"6.1(front)":     7,
	"7.0":            7,
	"7.0(front)":     7,
	"7.1":            8,
	"7.1(wide)":      8,
	"7.1(wide-side)": 8,
	"octagonal":      8,
	"cube":           8,
	"downmix":        2,
	"hexadecagonal":  16,
}

// DefaultLayout is ffmpeg's conventional layout for a channel count, used when
// a rule sets `channels` without a layout name (and when a probed track has a
// channel count but no layout).
func DefaultLayout(channels int) string {
	switch channels {
	case 1:
		return "mono"
	case 2:
		return "stereo"
	case 3:
		return "2.1"
	case 4:
		return "4.0"
	case 5:
		return "5.0"
	case 6:
		return "5.1"
	case 7:
		return "6.1"
	case 8:
		return "7.1"
	default:
		return ""
	}
}

// Options is the `audio.options` settings section: an ordered list of rules.
// The zero value (and a legacy row without `rules`) means no rules, i.e. the
// untouched lossless-copy behaviour.
type Options struct {
	Rules []Rule `json:"rules"`
}

// UnmarshalJSON materialises an absent `rules` as an empty list so the job path
// and the Get/All/Export helpers all see the same shape. Unknown fields are
// rejected, keeping the settings store's typo protection.
func (o *Options) UnmarshalJSON(data []byte) error {
	type plain Options
	aux := struct {
		*plain
	}{plain: (*plain)(o)}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&aux); err != nil {
		return err
	}
	if o.Rules == nil {
		o.Rules = []Rule{}
	}
	return nil
}

// Rule is one condition/action pair. Disabled rules are kept (so the UI can
// toggle them) but never matched. Order is significant: the first enabled rule
// whose conditions all hold wins.
type Rule struct {
	Enabled bool `json:"enabled"`
	When    When `json:"when"`
	Then    Then `json:"then"`
}

// When holds the AND-ed conditions. An empty When matches every track, which is
// how "always transcode to Opus" is expressed.
//
// Bitrate thresholds use the vault track's probed bitrate:
//
//	MinBitrateBPS — track bitrate must be AT LEAST this value (inclusive);
//	MaxBitrateBPS — track bitrate must be AT MOST this value (inclusive).
//
// A track with no reported bitrate (0) fails any configured threshold.
type When struct {
	// SourceCodecs is an exact-match list of ffmpeg codec names
	// (pcm_s16le, eac3, ...), compared case-insensitively. The source matches
	// when its codec appears anywhere in this list (membership/any-of).
	SourceCodecs []string `json:"source_codecs"`
	// SourceCodecRegex optionally matches the codec name as an RE2 pattern.
	SourceCodecRegex string `json:"source_codec_regex"`
	MinBitrateBPS    *int   `json:"min_bitrate_bps"`
	MaxBitrateBPS    *int   `json:"max_bitrate_bps"`
	ChannelCount     *int   `json:"channel_count"`
	ChannelLayout    string `json:"channel_layout"`
	SampleRate       *int   `json:"sample_rate"`
	LangISO2         string `json:"lang_iso2"`
	Variant          string `json:"variant"`
}

// Then is the action applied when the rule matches. Codec is required;
// "keep" means copy untouched and must not carry other actions.
type Then struct {
	Codec         string `json:"codec"`
	BitrateBPS    *int   `json:"bitrate_bps"`
	Channels      *int   `json:"channels"`
	ChannelLayout string `json:"channel_layout"`
	SampleRate    *int   `json:"sample_rate"`
}

// TrackMeta is the PRE-TEMPO vault-track metadata a rule is matched against.
type TrackMeta struct {
	Codec         string
	ChannelCount  int
	ChannelLayout string
	BitrateBPS    int
	SampleRate    int
	LangISO2      string
	Variant       string
}

// NoRule is the [Plan.RuleIndex] value of a plan no rule produced. A
// tempo-only plan (a drift correction with no rule match, see [Plan.WithTempo])
// carries it so the job log cannot claim rule 0 matched when none did.
const NoRule = -1

// Plan is the resolved action for one track. The zero Plan (Codec=="") means
// "no rule matched": copy the track untouched.
type Plan struct {
	// RuleIndex is the zero-based index of the matching rule, for logs; it is
	// [NoRule] when a plan carries a tempo correction but no rule matched.
	RuleIndex int
	// Codec is the canonical action (keep|opus|aac|flac|pcm_*).
	Codec string
	// BitrateBPS, Channels, ChannelLayout and SampleRate are the requested
	// output parameters; zero/empty means "inherit from the source".
	BitrateBPS    int
	Channels      int
	ChannelLayout string
	SampleRate    int
	// TempoFactor folds a drift correction into the same encode; 0 and 1 mean
	// no tempo change. A tempo-only plan is materialised as FLAC — lossless, so
	// a correction the operator never asked for cannot degrade the audio — but
	// the choice belongs to the caller: Plan only carries the factor.
	TempoFactor float64
}

// Encode reports whether the plan requires an ffmpeg encode.
func (p Plan) Encode() bool { return p.Codec != "" && p.Codec != CodecKeep }

// Encoder is the ffmpeg encoder a plan needs; it is empty for a keep plan.
func (p Plan) Encoder() string { return encoders[p.Codec] }

// ExpectedCodec is the ffprobe codec_name a verified encode must report.
func (p Plan) ExpectedCodec() string {
	if !p.Encode() {
		return ""
	}
	return p.Codec
}

// WithTempo returns the plan with a drift correction folded into the same
// encode: atempo runs inside the one ffmpeg invocation this plan already
// describes, as the first adjustment, so there is never a second generation of
// loss. The factor is validated by [ValidateTempo], not here.
func (p Plan) WithTempo(factor float64) Plan {
	p.TempoFactor = factor
	return p
}

// ErrInvalidRule marks a rule rejected by validation.
var ErrInvalidRule = errors.New("transcode: invalid rule")

var (
	codecNamePattern = regexp.MustCompile(`^[a-z0-9_]+$`)
	langPattern      = regexp.MustCompile(`^[a-z]{2,3}$`)
	variantPattern   = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
)

// ValidateOptions validates and canonicalises every rule. Disabled rules are
// validated too: a typo must surface while editing, not the day it is enabled.
func ValidateOptions(o *Options) error {
	if o.Rules == nil {
		o.Rules = []Rule{}
	}
	const maxRules = 100
	if len(o.Rules) > maxRules {
		return fmt.Errorf("%w: %d rules, at most %d are supported", ErrInvalidRule, len(o.Rules), maxRules)
	}
	for i := range o.Rules {
		if err := validateRule(&o.Rules[i]); err != nil {
			return fmt.Errorf("%w: rule %d: %v", ErrInvalidRule, i+1, err)
		}
	}
	return nil
}

func validateRule(r *Rule) error {
	w := &r.When
	for i := range w.SourceCodecs {
		codec := strings.ToLower(strings.TrimSpace(w.SourceCodecs[i]))
		if !codecNamePattern.MatchString(codec) {
			return fmt.Errorf("source_codecs entry %q is not a valid codec name", w.SourceCodecs[i])
		}
		w.SourceCodecs[i] = codec
	}
	if w.SourceCodecRegex != "" {
		if len(w.SourceCodecRegex) > 256 {
			return errors.New("source_codec_regex is too long (max 256 bytes)")
		}
		if _, err := regexp.Compile(w.SourceCodecRegex); err != nil {
			return fmt.Errorf("source_codec_regex %q: %v", w.SourceCodecRegex, err)
		}
	}
	if err := validateBitrateBounds(w.MinBitrateBPS, w.MaxBitrateBPS); err != nil {
		return err
	}
	if w.ChannelCount != nil && (*w.ChannelCount < 1 || *w.ChannelCount > 16) {
		return fmt.Errorf("channel_count must be 1..16, got %d", *w.ChannelCount)
	}
	w.ChannelLayout = strings.ToLower(strings.TrimSpace(w.ChannelLayout))
	if w.ChannelLayout != "" {
		if _, ok := LayoutChannels[w.ChannelLayout]; !ok {
			return fmt.Errorf("channel_layout %q is not a known ffmpeg layout", w.ChannelLayout)
		}
	}
	if w.SampleRate != nil && (*w.SampleRate < 1000 || *w.SampleRate > 768000) {
		return fmt.Errorf("sample_rate must be 1000..768000, got %d", *w.SampleRate)
	}
	w.LangISO2 = strings.ToLower(strings.TrimSpace(w.LangISO2))
	if w.LangISO2 != "" && !langPattern.MatchString(w.LangISO2) {
		return fmt.Errorf("lang_iso2 %q must be a lowercase ISO 639-1/2 code", w.LangISO2)
	}
	w.Variant = strings.ToLower(strings.TrimSpace(w.Variant))
	if w.Variant != "" && !variantPattern.MatchString(w.Variant) {
		return fmt.Errorf("variant %q is not a valid track variant", w.Variant)
	}

	t := &r.Then
	t.Codec = strings.ToLower(strings.TrimSpace(t.Codec))
	switch {
	case t.Codec == CodecKeep:
		if t.BitrateBPS != nil || t.Channels != nil || t.ChannelLayout != "" || t.SampleRate != nil {
			return errors.New(`then.codec "keep" must not carry bitrate, channels, channel_layout or sample_rate`)
		}
		return nil
	case encoders[t.Codec] != "":
	default:
		return fmt.Errorf("then.codec %q must be %s, %s, %s, %s, %s or %s",
			t.Codec, CodecKeep, CodecOpus, CodecAAC, CodecFLAC, CodecPCMS16LE, CodecPCMS24LE)
	}
	if t.BitrateBPS != nil {
		if !lossyCodecs[t.Codec] {
			return fmt.Errorf("then.codec %q is lossless: bitrate_bps is not supported", t.Codec)
		}
		lo, hi := 6000, 1500000
		if t.Codec == CodecOpus {
			hi = 512000
		}
		if *t.BitrateBPS < lo || *t.BitrateBPS > hi {
			return fmt.Errorf("bitrate_bps must be %d..%d for %s, got %d", lo, hi, t.Codec, *t.BitrateBPS)
		}
	}
	t.ChannelLayout = strings.ToLower(strings.TrimSpace(t.ChannelLayout))
	if t.ChannelLayout != "" {
		want, ok := LayoutChannels[t.ChannelLayout]
		if !ok {
			return fmt.Errorf("then.channel_layout %q is not a known ffmpeg layout", t.ChannelLayout)
		}
		if t.Channels != nil && *t.Channels != want {
			return fmt.Errorf("then.channels %d contradicts channel_layout %q (%d channels)", *t.Channels, t.ChannelLayout, want)
		}
	}
	if t.Channels != nil && (*t.Channels < 1 || *t.Channels > 8) {
		return fmt.Errorf("then.channels must be 1..8, got %d", *t.Channels)
	}
	if t.SampleRate != nil && (*t.SampleRate < 1000 || *t.SampleRate > 768000) {
		return fmt.Errorf("then.sample_rate must be 1000..768000, got %d", *t.SampleRate)
	}
	return nil
}

func validateBitrateBounds(min, max *int) error {
	const ceiling = 10_000_000
	if min != nil && (*min < 0 || *min > ceiling) {
		return fmt.Errorf("min_bitrate_bps must be 0..%d, got %d", ceiling, *min)
	}
	if max != nil && (*max < 0 || *max > ceiling) {
		return fmt.Errorf("max_bitrate_bps must be 0..%d, got %d", ceiling, *max)
	}
	if min != nil && max != nil && *min > *max {
		return fmt.Errorf("min_bitrate_bps (%d) must not exceed max_bitrate_bps (%d)", *min, *max)
	}
	return nil
}

// Match returns the plan of the first enabled rule whose conditions all hold.
// ok=false means no rule matched, i.e. copy the track untouched.
func Match(rules []Rule, meta TrackMeta) (Plan, bool) {
	for i := range rules {
		if !rules[i].Enabled {
			continue
		}
		if !rules[i].matches(meta) {
			continue
		}
		return planFor(i, rules[i]), true
	}
	return Plan{}, false
}

func (r Rule) matches(meta TrackMeta) bool {
	w := r.When
	codec := strings.ToLower(strings.TrimSpace(meta.Codec))
	if len(w.SourceCodecs) > 0 {
		found := false
		for _, want := range w.SourceCodecs {
			if strings.EqualFold(want, codec) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if w.SourceCodecRegex != "" {
		re, err := regexp.Compile(w.SourceCodecRegex)
		if err != nil || !re.MatchString(codec) {
			return false
		}
	}
	// An unknown bitrate cannot satisfy a threshold; treating 0 as "below
	// everything" would silently transcode tracks whose bitrate was never
	// probed.
	if w.MinBitrateBPS != nil && (meta.BitrateBPS == 0 || meta.BitrateBPS < *w.MinBitrateBPS) {
		return false
	}
	if w.MaxBitrateBPS != nil && (meta.BitrateBPS == 0 || meta.BitrateBPS > *w.MaxBitrateBPS) {
		return false
	}
	if w.ChannelCount != nil && meta.ChannelCount != *w.ChannelCount {
		return false
	}
	if w.ChannelLayout != "" && !layoutMatches(meta, w.ChannelLayout) {
		return false
	}
	if w.SampleRate != nil && meta.SampleRate != *w.SampleRate {
		return false
	}
	if w.LangISO2 != "" && !strings.EqualFold(w.LangISO2, meta.LangISO2) {
		return false
	}
	if w.Variant != "" && !strings.EqualFold(w.Variant, meta.Variant) {
		return false
	}
	return true
}

// layoutMatches accepts an exact canonical name or, when the probed file only
// reports a channel count, the rule layout's channel count. ffprobe is
// inconsistent about "(side)" variants, so count equality keeps "5.1" usable.
func layoutMatches(meta TrackMeta, want string) bool {
	got := strings.ToLower(strings.TrimSpace(meta.ChannelLayout))
	if got == want {
		return true
	}
	wantChannels, ok := LayoutChannels[want]
	if !ok {
		return false
	}
	if got == "" {
		return meta.ChannelCount == wantChannels
	}
	gotChannels, ok := LayoutChannels[got]
	if !ok {
		return meta.ChannelCount == wantChannels
	}
	return gotChannels == wantChannels
}

func planFor(index int, r Rule) Plan {
	p := Plan{RuleIndex: index, Codec: r.Then.Codec}
	if p.Codec == CodecKeep {
		return p
	}
	if r.Then.BitrateBPS != nil {
		p.BitrateBPS = *r.Then.BitrateBPS
	}
	if r.Then.Channels != nil {
		p.Channels = *r.Then.Channels
	}
	if r.Then.ChannelLayout != "" {
		p.ChannelLayout = r.Then.ChannelLayout
		p.Channels = LayoutChannels[r.Then.ChannelLayout]
	} else if p.Channels > 0 {
		p.ChannelLayout = DefaultLayout(p.Channels)
	}
	if r.Then.SampleRate != nil {
		p.SampleRate = *r.Then.SampleRate
	}
	return p
}

// ValidateTempo rejects a drift factor the single-pass command cannot honour.
// atempo itself supports 0.5..2.0 per instance; larger corrections are not
// needed by Dubarr's drift model (PLAN.md M4) and are refused loudly.
func ValidateTempo(factor float64) error {
	if factor == 0 || factor == 1 {
		return nil
	}
	if factor < 0.5 || factor > 2.0 {
		return fmt.Errorf("%w: tempo factor %.6f outside 0.5..2.0", ErrInvalidRule, factor)
	}
	return nil
}
