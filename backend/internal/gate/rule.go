package gate

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Subject412/dubarr/backend/internal/settings"
)

// Status is the Tdarr Gate classification of a target (GLOSSARY.md).
type Status string

const (
	// StatusEligible means every required processed-tag is present.
	StatusEligible Status = "eligible"
	// StatusWaitingForTags means the target exists but lacks required tags, or
	// carries an excluded tag; muxing is blocked while the filter is enforced.
	StatusWaitingForTags Status = "waiting_for_tags"
	// StatusMissingTarget means no target file could be resolved on disk.
	StatusMissingTarget Status = "missing_target"
)

// MatchMode is the tag matching strategy mirrored from
// settings.TargetTagFilter.tag_match_mode.
type MatchMode string

const (
	MatchAny   MatchMode = "ANY"
	MatchAll   MatchMode = "ALL"
	MatchRegex MatchMode = "REGEX"
)

// Tag is one Matroska tag pair observed on a target.
type Tag struct {
	Name  string
	Value string
}

// Result is the outcome of classifying one target.
type Result struct {
	Status Status `json:"status"`
	// TargetPath is the file that was inspected ("" for a missing target).
	TargetPath string `json:"target_path"`
	// Matched lists the required tags that were found.
	Matched []string `json:"matched_tags"`
	// Missing lists the required tags that were not found.
	Missing []string `json:"missing_tags"`
}

// Rule is the settled tag gate: the required processed-tags, optional exclusion
// tags, match mode, case sensitivity and the enforce switch.
//
// Enforce=false disables the gate entirely (every target is eligible); it is
// the "warn only" mode. Force override is not modelled: M2 keeps
// settings.security.allow_force_tag_override false and never bypasses a
// waiting_for_tags verdict.
type Rule struct {
	Required      []string
	Excluded      []string
	Mode          MatchMode
	CaseSensitive bool
	Enforce       bool
}

// RuleFromTagFilter converts the persisted target.tag_filter settings section
// into a Rule. Unknown match modes fall back to ANY, the settled default.
func RuleFromTagFilter(tf settings.TargetTagFilter) Rule {
	mode := MatchMode(strings.ToUpper(strings.TrimSpace(tf.TagMatchMode)))
	switch mode {
	case MatchAll, MatchRegex:
	default:
		mode = MatchAny
	}
	return Rule{
		Required:      trimAll(tf.RequiredFilenameTags),
		Excluded:      trimAll(tf.ExcludedFilenameTags),
		Mode:          mode,
		CaseSensitive: tf.TagCaseSensitive,
		Enforce:       tf.EnforceTagFilter,
	}
}

// Classify evaluates the gate against a target filename and its container tags.
//
// Requirements are matched, in order of precedence, against:
//   - the target's base filename (ADR-0005: Tdarr writes the marker there), and
//   - every container tag rendered as "NAME=value".
//
// ANY is satisfied by one required tag, ALL by all of them; REGEX treats each
// required entry as a regular expression and any match satisfies the gate.
// Excluded tags always win: a target carrying one is waiting_for_tags even
// when every required tag is present.
func (r Rule) Classify(filename string, tags []Tag) Result {
	res := Result{Matched: []string{}, Missing: []string{}}
	required := trimAll(r.Required)
	if !r.Enforce || len(required) == 0 {
		res.Status = StatusEligible
		return res
	}

	hay := r.haystack(filename, tags)
	for _, want := range required {
		if r.matches(want, hay) {
			res.Matched = append(res.Matched, want)
		} else {
			res.Missing = append(res.Missing, want)
		}
	}

	blocked := false
	for _, excl := range trimAll(r.Excluded) {
		if r.matches(excl, hay) {
			blocked = true
		}
	}

	switch {
	case blocked:
		res.Status = StatusWaitingForTags
	case r.Mode == MatchAll && len(res.Missing) > 0:
		res.Status = StatusWaitingForTags
	case len(res.Matched) == 0:
		res.Status = StatusWaitingForTags
	default:
		res.Status = StatusEligible
	}
	return res
}

// haystack renders the searchable strings for a target: the base filename plus
// one entry per container tag. Entries are lower-cased unless the rule is
// case-sensitive.
func (r Rule) haystack(filename string, tags []Tag) []string {
	hay := make([]string, 0, len(tags)+1)
	if base := filepath.Base(filename); filename != "" && base != "." {
		hay = append(hay, base)
	}
	for _, tag := range tags {
		hay = append(hay, tag.Name+"="+tag.Value)
	}
	if !r.CaseSensitive {
		for i, v := range hay {
			hay[i] = strings.ToLower(v)
		}
	}
	return hay
}

// matches reports whether want is present in the haystack under the rule's
// match mode. An invalid regular expression never matches, so a typo in the
// settings can block muxing but can never let an untagged file through.
func (r Rule) matches(want string, hay []string) bool {
	if want == "" {
		return false
	}
	if r.Mode == MatchRegex {
		expr := want
		if !r.CaseSensitive {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return false
		}
		for _, entry := range hay {
			if re.MatchString(entry) {
				return true
			}
		}
		return false
	}
	needle := want
	if !r.CaseSensitive {
		needle = strings.ToLower(needle)
	}
	for _, entry := range hay {
		if strings.Contains(entry, needle) {
			return true
		}
	}
	return false
}

// trimAll drops blank entries so an empty list item cannot make the gate
// trivially satisfiable.
func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
