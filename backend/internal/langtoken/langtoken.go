// Package langtoken defines the reserved Reference Language tokens that the
// Extra reference languages setting may contain, and expands them into concrete
// ISO 639-1 languages.
//
// The tokens are defined here once so every consumer — the sync resolver
// (reflang.Resolve) and the export policy (gate.SplitExportTracks) — cannot
// diverge on their spelling or meaning:
//
//	org — the arr-reported Original Language
//	all — every usable shared language between the Donor Group and the Target
//
// Tokens are canonical lowercase. Expansion is deterministic: explicit
// configured languages keep their configured order and priority, then `org`,
// then `all` (explicit entries first, remaining shared languages lexical).
// Target import languages, commentary tracks and und are never candidates; a
// token that cannot expand is skipped, never treated as a failure.
package langtoken

import (
	"sort"
	"strings"

	"github.com/Subject412/dubarr/backend/internal/probe"
)

// The reserved tokens, canonical lowercase.
const (
	// Org is the arr-reported Original Language.
	Org = "org"
	// All is every usable shared language between the Donor Group and Target.
	All = "all"
)

// Token reports whether raw is a reserved token and returns its canonical
// lowercase spelling. Leading/trailing space and letter case are ignored.
func Token(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case Org:
		return Org, true
	case All:
		return All, true
	default:
		return "", false
	}
}

// IsReserved reports whether raw is a reserved reference-language token.
func IsReserved(raw string) bool {
	_, ok := Token(raw)
	return ok
}

// Expand turns the configured Extra reference languages into an ordered,
// deduplicated list of ISO 639-1 candidates.
//
//	entries   configured entries, in user priority order; may contain tokens
//	original  arr-reported original language, "" when absent or disabled
//	available usable shared languages (donor ∩ target for sync, donor tracks
//	          for export), in any order
//	targets   target import languages, never references
//
// Explicit entries come first in configured order. `org` expands to original
// (when it is usable and not a target). `all` expands to every usable
// available language not already listed, in lexical order. und and duplicate
// entries are dropped.
func Expand(entries []string, original string, available, targets []string) []string {
	targetSet := langSet(targets)
	seen := make(map[string]bool, len(entries)+len(available)+1)
	out := make([]string, 0, len(entries)+len(available))

	add := func(code string) {
		if code == "" || targetSet[code] || seen[code] {
			return
		}
		seen[code] = true
		out = append(out, code)
	}

	// First pass: explicit languages keep their priority over token-expanded
	// candidates, wherever the tokens sit in the configured list.
	var hasOrg, hasAll bool
	for _, raw := range entries {
		if tok, ok := Token(raw); ok {
			switch tok {
			case Org:
				hasOrg = true
			case All:
				hasAll = true
			}
			continue
		}
		add(probe.ISO639_1(raw))
	}

	if hasOrg {
		add(probe.ISO639_1(original))
	}
	if hasAll {
		codes := make([]string, 0, len(available))
		for _, raw := range available {
			code := probe.ISO639_1(raw)
			if code == "" || targetSet[code] {
				continue
			}
			codes = append(codes, code)
		}
		sort.Strings(codes)
		for _, code := range codes {
			add(code)
		}
	}
	return out
}

// langSet normalizes configured language spellings to ISO 639-1 codes.
func langSet(raw []string) map[string]bool {
	out := make(map[string]bool, len(raw))
	for _, code := range raw {
		if lang := probe.ISO639_1(code); lang != "" {
			out[lang] = true
		}
	}
	return out
}
