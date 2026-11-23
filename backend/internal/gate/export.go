package gate

import (
	"strings"

	"github.com/Subject412/dubarr/backend/internal/langtoken"
	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/settings"
)

// ExportPolicy is the rule-based export decision (Q14b): which languages the
// appliance imports by default. Tracks outside the policy are extras the user
// must opt into.
type ExportPolicy struct {
	// TargetLanguages are the dub/lektor languages to import (ISO 639-1 or any
	// spelling probe.ISO639_1 understands), e.g. ["pl"].
	TargetLanguages []string
	// ReferenceLanguages are extra reference tracks kept beside the original,
	// e.g. ["en"] for same-language reference (Priority 1).
	ReferenceLanguages []string
	// OriginalLanguage is the arr-reported original language of the media.
	OriginalLanguage string
	// ExcludeCommentary drops commentary tracks entirely instead of offering
	// them as opt-in extras (settings default: true).
	ExcludeCommentary bool
}

// ExportSplit partitions probed tracks into the default policy set and the
// beyond-policy extras. Order within each slice follows the input.
type ExportSplit struct {
	// Policy tracks are exported by default.
	Policy []probe.AudioTrack
	// Extras are opt-in: commentary (unless excluded), untagged (und) tracks and
	// languages outside the policy.
	Extras []probe.AudioTrack
}

// PolicyFromLanguages builds an ExportPolicy from the persisted
// languages.policy section plus the original language reported by the arr.
//
// original_language_from_arr=false disables the arr original entirely: it is
// neither kept as a reference nor available to the org token, matching the
// resolver's Priority 1 behaviour.
func PolicyFromLanguages(lp settings.LanguagesPolicy, originalLanguage string) ExportPolicy {
	original := probe.ISO639_1(originalLanguage)
	if !lp.OriginalLanguageFromArr {
		original = ""
	}
	return ExportPolicy{
		TargetLanguages:    append([]string(nil), lp.TargetImportLanguages...),
		ReferenceLanguages: append([]string(nil), lp.ExtraReferenceLanguages...),
		OriginalLanguage:   original,
		ExcludeCommentary:  lp.ExcludeCommentary,
	}
}

// SplitExportTracks applies pol to tracks.
//
// A track is policy-selected when its normalized language is one of the target
// import languages, the original language, or a reference language — explicit
// or expanded from the reserved org/all tokens by the shared normalizer
// (internal/langtoken). Commentary tracks and tracks with no usable language
// tag ("und") are never policy-selected — the first is excluded or opt-in, the
// second is manual-pick only (settled defaults) — so they land in Extras
// (commentary is dropped when ExcludeCommentary is set). Everything else is an
// opt-in extra.
func SplitExportTracks(tracks []probe.AudioTrack, pol ExportPolicy) ExportSplit {
	imports := langSet(pol.TargetLanguages...)
	original := probe.ISO639_1(pol.OriginalLanguage)
	policy := make(map[string]bool, len(imports)+len(pol.ReferenceLanguages)+1)
	for code := range imports {
		policy[code] = true
	}
	if original != "" {
		policy[original] = true
	}
	for _, code := range langtoken.Expand(
		pol.ReferenceLanguages, original, usableLanguages(tracks), pol.TargetLanguages) {
		policy[code] = true
	}

	var split ExportSplit
	for _, track := range tracks {
		if track.Disposition.Comment || isCommentaryTitle(track.Title) {
			if !pol.ExcludeCommentary {
				split.Extras = append(split.Extras, track)
			}
			continue
		}
		lang := track.LanguageISO1
		if lang == "" {
			lang = probe.ISO639_1(track.Language)
		}
		if lang != "" && policy[lang] {
			split.Policy = append(split.Policy, track)
			continue
		}
		split.Extras = append(split.Extras, track)
	}
	return split
}

// usableLanguages lists the languages a donor track can contribute as a
// reference: non-commentary and tagged with a real language (never und).
func usableLanguages(tracks []probe.AudioTrack) []string {
	out := make([]string, 0, len(tracks))
	for _, track := range tracks {
		if track.Disposition.Comment || isCommentaryTitle(track.Title) {
			continue
		}
		lang := track.LanguageISO1
		if lang == "" {
			lang = probe.ISO639_1(track.Language)
		}
		if lang != "" {
			out = append(out, lang)
		}
	}
	return out
}

// langSet normalizes configured language spellings ("pol", "Polish", "pl") to
// ISO 639-1 codes and returns them as a set.
func langSet(langs ...string) map[string]bool {
	set := make(map[string]bool, len(langs))
	for _, lang := range langs {
		if code := probe.ISO639_1(lang); code != "" {
			set[code] = true
		}
	}
	return set
}

// isCommentaryTitle heuristically flags commentary tracks whose disposition
// flag a muxer did not set but whose title says so.
func isCommentaryTitle(title string) bool {
	return strings.Contains(strings.ToLower(title), "commentary")
}
