// Package reflang resolves which audio track pairs drive a synchronization
// job (IDEA.md §5.2 "Reference Language Resolution", PLAN.md M4-4).
//
// The resolver is a pure function over ffprobe metadata: it sees the audio
// tracks of the target video file and the sibling tracks of the donor group,
// plus the persisted language policy and the arr-reported original language,
// and returns candidate reference pairs in priority order.
//
// Priority order (IDEA.md:365-369):
//
//  1. Priority 1 (same_lang_reference): the media's original language exists
//     in both the donor group and the target (e.g. ja <-> ja). Active only when
//     the policy trusts arr metadata and prefers the original (see Languages).
//  2. Priority 2 (same_lang_reference): any other shared, non-target language
//     (e.g. en <-> en), configured reference languages first, then the rest
//     in lexical order. The reserved tokens org/all expand here through
//     internal/langtoken.
//  3. Priority 3 (lowpass_fallback): no shared reference language exists, so
//     the incoming (target-language) vault track is compared low-pass 30-200
//     Hz against the target's original, English or first non-commentary
//     stream (IDEA.md:369, IDEA.md:379).
//
// Tracks tagged "und" or with no usable language are never selected as a
// language reference (settled: und never policy, see gate.SplitExportTracks).
// Commentary tracks are never references.
//
// The package performs no I/O and executes no processes; callers own the
// actual copying and FFmpeg slicing.
package reflang

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Subject412/dubarr/backend/internal/langtoken"
	"github.com/Subject412/dubarr/backend/internal/probe"
)

// Languages is the language policy input to Resolve. It mirrors the language
// fields of settings.LanguagesPolicy without importing it, keeping this package
// a pure metadata function.
type Languages struct {
	// Targets are the target import (dub/lektor) languages, e.g. ["pl"]. They
	// are the languages being synchronized and therefore never references.
	Targets []string
	// References are extra reference languages in user priority order, e.g.
	// ["en"] (settings.ExtraReferenceLanguages). They may contain the reserved
	// tokens "org" and "all" (see internal/langtoken).
	References []string
	// PreferOriginalLanguage makes the arr-reported original language the
	// first-choice reference (Priority 1). settings.PreferOriginalLanguage.
	PreferOriginalLanguage bool
	// OriginalLanguageFromArr gates arr metadata: when false, Priority 1 is
	// disabled and the org token expands to nothing.
	// settings.OriginalLanguageFromArr.
	OriginalLanguageFromArr bool
}

// Method is the sync_method a pair resolves to (IDEA.md:295).
type Method string

const (
	// SameLangReference correlates tracks that share a language.
	SameLangReference Method = "same_lang_reference"
	// LowpassFallback correlates across languages in the 30-200 Hz band.
	LowpassFallback Method = "lowpass_fallback"
)

// Pair is one vault-track <-> target-track reference candidate.
type Pair struct {
	// Method is the synchronization strategy the pair implies.
	Method Method
	// Vault is the donor-group track (the WORK_DIR copy's source).
	Vault probe.AudioTrack
	// Target is the target-file track to correlate against.
	Target probe.AudioTrack
	// Reason states why this pair was chosen (for job status/reporting).
	Reason string
}

// Result is the ordered resolution outcome. Pairs is best-first; when it is
// empty, Reason explains why no reference pair could be formed.
type Result struct {
	Pairs  []Pair
	Reason string
}

// Resolve returns reference pairs in priority order.
//
// Tie-breaks: within a language, the track with the highest channel count wins
// (IDEA.md:367 prefers higher channel count/bitrate; probe.AudioTrack carries
// no bitrate, so channels is the available quality proxy), then the lowest
// container stream index (earliest track). Pair order is: Priority 1, then
// Priority 2 languages in policy-reference order followed by remaining shared
// languages in lexical order, then Priority 3 only when no shared reference
// exists.
func Resolve(targetTracks []probe.AudioTrack, vaultTracks []probe.AudioTrack, policy Languages, originalLanguage string) Result {
	res := Result{Pairs: make([]Pair, 0, len(policy.References)+1)}
	if len(vaultTracks) == 0 {
		res.Reason = "reference resolver: donor group has no audio tracks"
		return res
	}
	if len(targetTracks) == 0 {
		res.Reason = "reference resolver: target has no audio tracks"
		return res
	}

	original := probe.ISO639_1(originalLanguage)
	// The arr original is only consulted for Priority 1 and the org token when
	// the operator trusts arr metadata. Priority 3 keeps using the metadata as
	// a target preference, which is independent of reference-language policy.
	arrOriginal := ""
	if policy.OriginalLanguageFromArr {
		arrOriginal = original
	}
	targets := langSet(policy.Targets)
	seen := make(map[string]bool, len(policy.Targets)+len(policy.References)+1)

	// add appends one pair for lang when both sides have a usable track.
	add := func(lang string, method Method, reason string) bool {
		if lang == "" || targets[lang] || seen[lang] {
			return false
		}
		v, ok := bestTrack(vaultTracks, lang)
		if !ok {
			return false
		}
		t, ok := bestTrack(targetTracks, lang)
		if !ok {
			return false
		}
		seen[lang] = true
		res.Pairs = append(res.Pairs, Pair{Method: method, Vault: v, Target: t, Reason: reason})
		return true
	}

	// Priority 1: original language present in both groups. A target import
	// language is the dub being synchronized, so it cannot double as its own
	// reference even when the arr reports it as original. The guard holds
	// regardless of the preference toggle.
	if arrOriginal != "" && policy.PreferOriginalLanguage {
		add(arrOriginal, SameLangReference,
			fmt.Sprintf("priority 1 (same_lang_reference): original language %q present in donor group and target", arrOriginal))
	}

	// Priority 2: policy reference languages in their configured order, then
	// any other shared non-target language deterministically. The reserved
	// tokens org/all expand here, after explicit entries, so both the resolver
	// and the export policy use one normalizer (internal/langtoken).
	shared := sharedLanguages(vaultTracks, targetTracks)
	for _, lang := range langtoken.Expand(policy.References, arrOriginal, shared, policy.Targets) {
		add(lang, SameLangReference,
			fmt.Sprintf("priority 2 (same_lang_reference): shared reference language %q", lang))
	}
	sort.Strings(shared)
	for _, lang := range shared {
		if targets[lang] || seen[lang] {
			continue
		}
		add(lang, SameLangReference,
			fmt.Sprintf("priority 2 (same_lang_reference): shared non-target language %q", lang))
	}

	if len(res.Pairs) > 0 {
		return res
	}

	// Priority 3: no shared reference exists. Correlate the incoming
	// target-language track against the target's original/English/first
	// non-commentary stream using the 30-200 Hz low-pass band.
	vaultRef, ok := targetLanguageTrack(vaultTracks, policy.Targets)
	if !ok {
		res.Reason = fmt.Sprintf(
			"reference resolver: no shared reference language between donor group and target and no donor track in a target import language %v for low-pass fallback",
			policy.Targets)
		return res
	}
	targetRef, ok := lowpassTarget(targetTracks, original)
	if !ok {
		res.Reason = "reference resolver: target has no non-commentary audio track usable for low-pass fallback"
		return res
	}
	reason := fmt.Sprintf("priority 3 (lowpass_fallback): no shared reference language; low-pass 30-200 Hz against target language %q", normLang(targetRef))
	if normLang(targetRef) == "" {
		reason = "priority 3 (lowpass_fallback): no shared reference language; low-pass 30-200 Hz against target's first non-commentary stream"
	}
	res.Pairs = append(res.Pairs, Pair{Method: LowpassFallback, Vault: vaultRef, Target: targetRef, Reason: reason})
	return res
}

// SameLang reports the same-lang report case: the target already carries a
// non-commentary audio track in vaultLang, the language of the vault track
// being synchronized (PLAN.md M4-5, IDEA.md:365). und/empty never matches.
func SameLang(targetTracks []probe.AudioTrack, vaultLang string) bool {
	lang := probe.ISO639_1(vaultLang)
	if lang == "" {
		return false
	}
	_, ok := bestTrack(targetTracks, lang)
	return ok
}

// Lowpass reports the low-pass report case: no target track shares the vault
// track's language, so IDEA.md:369 mandates the cross-language fallback with
// the 30-200 Hz band (IDEA.md:379).
func Lowpass(targetTracks []probe.AudioTrack, vaultLang string) bool {
	return !SameLang(targetTracks, vaultLang)
}

// normLang returns the usable ISO 639-1 language of t, or "" for und/missing.
func normLang(t probe.AudioTrack) string {
	if lang := probe.ISO639_1(t.LanguageISO1); lang != "" {
		return lang
	}
	return probe.ISO639_1(t.Language)
}

// usable reports whether t may act as a language reference at all.
func usable(t probe.AudioTrack) bool {
	return !t.Disposition.Comment && normLang(t) != ""
}

// bestTrack selects the highest-quality non-commentary track in lang.
func bestTrack(tracks []probe.AudioTrack, lang string) (probe.AudioTrack, bool) {
	var best probe.AudioTrack
	found := false
	for _, t := range tracks {
		if !usable(t) || normLang(t) != lang {
			continue
		}
		if !found || better(t, best) {
			best, found = t, true
		}
	}
	return best, found
}

// better is the quality tie-break: more channels first, then the lower stream
// index (container order).
func better(a, b probe.AudioTrack) bool {
	if a.Channels != b.Channels {
		return a.Channels > b.Channels
	}
	return a.Index < b.Index
}

// targetLanguageTrack picks the donor track being synchronized: the first
// policy target language with a usable vault track, highest quality first.
func targetLanguageTrack(vaultTracks []probe.AudioTrack, targets []string) (probe.AudioTrack, bool) {
	for _, raw := range targets {
		lang := probe.ISO639_1(raw)
		if lang == "" {
			continue
		}
		if t, ok := bestTrack(vaultTracks, lang); ok {
			return t, true
		}
	}
	return probe.AudioTrack{}, false
}

// lowpassTarget picks the target stream for Priority 3: original language,
// else English, else the first non-commentary stream (IDEA.md:369).
func lowpassTarget(targetTracks []probe.AudioTrack, original string) (probe.AudioTrack, bool) {
	if original != "" {
		if t, ok := bestTrack(targetTracks, original); ok {
			return t, true
		}
	}
	if t, ok := bestTrack(targetTracks, "en"); ok {
		return t, true
	}
	for _, t := range targetTracks {
		if !t.Disposition.Comment {
			return t, true
		}
	}
	return probe.AudioTrack{}, false
}

// sharedLanguages lists usable languages present on both sides, deduplicated.
func sharedLanguages(a, b []probe.AudioTrack) []string {
	have := make(map[string]bool, len(a))
	for _, t := range a {
		if usable(t) {
			have[normLang(t)] = true
		}
	}
	seen := make(map[string]bool, len(have))
	out := make([]string, 0, len(have))
	for _, t := range b {
		if !usable(t) {
			continue
		}
		lang := normLang(t)
		if have[lang] && !seen[lang] {
			seen[lang] = true
			out = append(out, lang)
		}
	}
	return out
}

// langSet normalizes the policy list into a set of ISO 639-1 codes.
func langSet(raw []string) map[string]bool {
	out := make(map[string]bool, len(raw))
	for _, code := range raw {
		if lang := probe.ISO639_1(strings.TrimSpace(code)); lang != "" {
			out[lang] = true
		}
	}
	return out
}
