package gate

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Subject412/dubarr/backend/internal/settings"
)

// Status is the Processing Gate classification of a target (GLOSSARY.md;
// ADR-0005 and ADR-0009).
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

// SourceMode selects which Processing Gate sources apply (ADR-0009).
type SourceMode string

const (
	// SourceFilename is ADR-0005 behaviour: only the base filename decides.
	SourceFilename SourceMode = "filename"
	// SourceArr decides on the arr criterion alone.
	SourceArr SourceMode = "arr"
	// SourceBoth AND-ed the filename and arr criteria.
	SourceBoth SourceMode = "both"
)

// ArrMetadata is the arr_library_cache metadata for one target.
type ArrMetadata struct {
	// Found reports whether an arr library row maps to this target. False
	// means there is no arr item for the file; when an arr criterion is active
	// the target fails closed.
	Found bool
	// Tags are the item's arr tag labels (for an episode, its series' tags).
	Tags []string
	// RootFolder is the canonical arr root folder containing the media folder.
	RootFolder string
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
	// ArrMatched/ArrMissing do the same for the arr tag criterion.
	ArrMatched []string `json:"arr_matched_tags,omitempty"`
	ArrMissing []string `json:"arr_missing_tags,omitempty"`
}

// Rule is the settled tag gate: the required processed-tags, optional exclusion
// tags, match mode, case sensitivity, enforce switch and — since ADR-0009 — the
// source mode plus the optional arr criterion.
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
	// SourceMode is filename (default, zero value), arr or both. Sources are
	// AND-ed: adding the arr source can only narrow eligibility.
	SourceMode SourceMode
	// RequiredArrTags/ExcludedArrTags are arr tag labels matched under Mode and
	// CaseSensitive; excluded labels always win.
	RequiredArrTags []string
	ExcludedArrTags []string
	// RootFolderPaths is an allowlist of canonical arr root-folder paths. Empty
	// means no root criterion.
	RootFolderPaths []string
}

// RuleFromTagFilter converts the persisted target.tag_filter settings section
// into a Rule. Unknown match modes fall back to ANY, the settled default, and a
// legacy/empty source mode falls back to filename (ADR-0005 behaviour).
func RuleFromTagFilter(tf settings.TargetTagFilter) Rule {
	mode := MatchMode(strings.ToUpper(strings.TrimSpace(tf.TagMatchMode)))
	switch mode {
	case MatchAll, MatchRegex:
	default:
		mode = MatchAny
	}
	return Rule{
		Required:        trimAll(tf.RequiredFilenameTags),
		Excluded:        trimAll(tf.ExcludedFilenameTags),
		Mode:            mode,
		CaseSensitive:   tf.TagCaseSensitive,
		Enforce:         tf.EnforceTagFilter,
		SourceMode:      normalizeSourceMode(tf.SourceMode),
		RequiredArrTags: trimAll(tf.RequiredArrTags),
		ExcludedArrTags: trimAll(tf.ExcludedArrTags),
		RootFolderPaths: canonicalRoots(tf.RootFolderPaths),
	}
}

// normalizeSourceMode maps the persisted spelling onto a known source mode.
// A zero value is the pre-ADR-0009 row and means filename-only.
func normalizeSourceMode(raw string) SourceMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(SourceArr):
		return SourceArr
	case string(SourceBoth):
		return SourceBoth
	default:
		return SourceFilename
	}
}

// FilenameOnly drops the arr criterion. Donor ingestion is never gated
// (ADR-0005): a donor drop can never map to an arr library row, so the watch
// path evaluates the filename source only.
func (r Rule) FilenameOnly() Rule {
	r.SourceMode = SourceFilename
	return r
}

// Classify evaluates the gate against a target filename and, when the arr
// source is active, its arr metadata.
//
// With source_mode=filename (and the legacy zero value) eligibility is decided
// solely by the base filename: ADR-0005 makes the filename the authoritative
// marker location, and container tags stay informational. ADR-0009 amends
// ADR-0005 for source_mode=arr/both: the arr criterion (tag labels and
// root-folder path from arr_library_cache) is AND-ed with the filename
// criterion, and a target with no arr library row fails closed.
//
// ANY is satisfied by one required tag, ALL by all of them; REGEX treats each
// required entry as a regular expression and any match satisfies that source.
// Excluded tags always win: a filename/label carrying one is waiting_for_tags
// even when every required tag is present.
//
// The arr metadata comes from the cache rows written by the 300 s arr poll, so
// an arr tag edit can take up to one poll interval (300 s) to change a verdict.
func (r Rule) Classify(filename string, arr ...ArrMetadata) Result {
	var meta ArrMetadata
	if len(arr) > 0 {
		meta = arr[0]
	}

	res := Result{Matched: []string{}, Missing: []string{}, ArrMatched: []string{}, ArrMissing: []string{}}
	if !r.Enforce {
		res.Status = StatusEligible
		return res
	}

	blocked := false
	if normalizeSourceMode(string(r.SourceMode)) != SourceArr {
		blocked = r.classifyFilename(filename, &res) || blocked
	}
	if normalizeSourceMode(string(r.SourceMode)) != SourceFilename {
		blocked = r.classifyArr(meta, &res) || blocked
	}

	if blocked {
		res.Status = StatusWaitingForTags
	} else {
		res.Status = StatusEligible
	}
	return res
}

// EligiblePath answers the only question the eligibility projection asks: does
// this target pass the configured rule right now? It is Classify's verdict
// reduced to one boolean, so the projection can never drift from the gate the
// mux and the queue already enforce.
//
// A missing arr row fails closed exactly as Classify does (ADR-0009), and the
// neutral rule (Enforce=false, ADR-0012) is eligible for every target. Force
// override is deliberately not modelled here: the caller owns the operator's
// escape hatch and skips the projection for a forced job.
func (r Rule) EligiblePath(path string, meta ArrMetadata) bool {
	return r.Classify(path, meta).Status == StatusEligible
}

// classifyFilename evaluates the filename source and reports whether it blocks
// eligibility. An empty required list is a no-op (ADR-0012 neutral default),
// but excluded filename tags still block whenever the gate is enforced.
func (r Rule) classifyFilename(filename string, res *Result) bool {
	hay := r.haystack(filename)
	required := trimAll(r.Required)
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
	if len(required) == 0 {
		return blocked
	}
	switch {
	case r.Mode == MatchAll && len(res.Missing) > 0:
		return true
	case len(res.Matched) == 0:
		return true
	}
	return blocked
}

// classifyArr evaluates the arr source and reports whether it blocks
// eligibility. A missing library row fails closed while the source is active.
func (r Rule) classifyArr(meta ArrMetadata, res *Result) bool {
	if !meta.Found {
		return true
	}

	hay := r.arrHaystack(meta.Tags)
	required := trimAll(r.RequiredArrTags)
	for _, want := range required {
		if r.matches(want, hay) {
			res.ArrMatched = append(res.ArrMatched, want)
		} else {
			res.ArrMissing = append(res.ArrMissing, want)
		}
	}

	for _, excl := range trimAll(r.ExcludedArrTags) {
		if r.matches(excl, hay) {
			return true
		}
	}
	if roots := canonicalRoots(r.RootFolderPaths); len(roots) > 0 {
		if !rootAllowed(roots, meta.RootFolder) {
			return true
		}
	}
	if len(required) == 0 {
		return false
	}
	switch {
	case r.Mode == MatchAll && len(res.ArrMissing) > 0:
		return true
	case len(res.ArrMatched) == 0:
		return true
	}
	return false
}

// rootAllowed reports whether folder is one of the canonical allowlisted roots.
// Root folders are selected by path, never by name, and an empty cached root
// (no configured arr root matched the media folder) never satisfies the list.
func rootAllowed(roots []string, folder string) bool {
	if strings.TrimSpace(folder) == "" {
		return false
	}
	clean := filepath.Clean(folder)
	for _, root := range roots {
		if clean == root {
			return true
		}
	}
	return false
}

// canonicalRoots cleans a configured root list so comparisons are path-exact.
// Blank entries are dropped: settings validation rejects them on write, and a
// legacy row must not turn a blank into the filesystem root.
func canonicalRoots(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		clean := filepath.Clean(raw)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	return out
}

// haystack renders the searchable strings for a target: the base filename only.
// The entry is lower-cased unless the rule is case-sensitive.
func (r Rule) haystack(filename string) []string {
	base := filepath.Base(filename)
	if filename == "" || base == "." {
		return nil
	}
	if !r.CaseSensitive {
		base = strings.ToLower(base)
	}
	return []string{base}
}

// arrHaystack renders the arr tag labels as matchable strings, lower-cased
// unless the rule is case-sensitive. Labels are matched as labels (never tag
// ids): ADR-0009 settles that.
func (r Rule) arrHaystack(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if !r.CaseSensitive {
			tag = strings.ToLower(tag)
		}
		out = append(out, tag)
	}
	return out
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
