package gate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/settings"
)

func TestRuleClassifyAny(t *testing.T) {
	r := Rule{
		Required: []string{"[ArcA310]", "[NVIDIA]"},
		Mode:     MatchAny,
		Enforce:  true,
	}
	got := r.Classify("Shrek (2001) [arcA310].mkv")
	if got.Status != StatusEligible {
		t.Fatalf("status = %q, want eligible", got.Status)
	}
	if len(got.Matched) != 1 || got.Matched[0] != "[ArcA310]" {
		t.Errorf("matched = %v, want [[ArcA310]]", got.Matched)
	}

	got = r.Classify("Shrek (2001).mkv")
	if got.Status != StatusWaitingForTags {
		t.Fatalf("status = %q, want waiting_for_tags", got.Status)
	}
	if len(got.Missing) != 2 {
		t.Errorf("missing = %v, want both required tags", got.Missing)
	}
}

func TestRuleClassifyAll(t *testing.T) {
	r := Rule{Required: []string{"[A]", "[B]"}, Mode: MatchAll, Enforce: true}
	if got := r.Classify("f [A].mkv"); got.Status != StatusWaitingForTags {
		t.Errorf("one of two present: status = %q, want waiting_for_tags", got.Status)
	}
	got := r.Classify("f [A] [B].mkv")
	if got.Status != StatusEligible {
		t.Errorf("both present: status = %q, want eligible", got.Status)
	}
}

func TestRuleClassifyCaseSensitivity(t *testing.T) {
	if got := (Rule{Required: []string{"[ArcA310]"}, Mode: MatchAny, Enforce: true}).Classify("f [arca310].mkv"); got.Status != StatusEligible {
		t.Errorf("case-insensitive default: status = %q, want eligible", got.Status)
	}
	if got := (Rule{Required: []string{"[ArcA310]"}, Mode: MatchAny, CaseSensitive: true, Enforce: true}).Classify("f [arca310].mkv"); got.Status != StatusWaitingForTags {
		t.Errorf("case-sensitive: status = %q, want waiting_for_tags", got.Status)
	}
}

func TestRuleClassifyRegex(t *testing.T) {
	r := Rule{Required: []string{`\[ArcA31\d\]`}, Mode: MatchRegex, Enforce: true}
	if got := r.Classify("f [ArcA310].mkv"); got.Status != StatusEligible {
		t.Errorf("regex match: status = %q, want eligible", got.Status)
	}
	if got := r.Classify("f [ArcA999].mkv"); got.Status != StatusWaitingForTags {
		t.Errorf("regex miss: status = %q, want waiting_for_tags", got.Status)
	}
	// An invalid pattern must never make an untagged file eligible.
	if got := (Rule{Required: []string{`[`}, Mode: MatchRegex, Enforce: true}).Classify("f.mkv"); got.Status != StatusWaitingForTags {
		t.Errorf("invalid regex: status = %q, want waiting_for_tags", got.Status)
	}
}

// TestGateReportsWhyAPatternDoesNotCompile pins #155's second half: a target
// waiting on a pattern the engine refuses must be distinguishable from one
// whose file simply lacks the tag. The status stays waiting_for_tags (the
// target is still waiting for a correct tag); Reason says why the wait cannot
// end, in the same words the settings form refuses the save with.
func TestGateReportsWhyAPatternDoesNotCompile(t *testing.T) {
	const pattern = `\d+\1`
	broken := Rule{Required: []string{pattern}, Mode: MatchRegex, Enforce: true}
	got := broken.Classify("Shrek (2001) [ArcA310].mkv")
	if got.Status != StatusWaitingForTags {
		t.Fatalf("status = %q, want waiting_for_tags", got.Status)
	}
	if !strings.Contains(got.Reason, "filename tag pattern "+fmt.Sprintf("%q", pattern)) {
		t.Errorf("reason = %q, want it to name the filename pattern %q", got.Reason, pattern)
	}
	if !strings.Contains(got.Reason, "invalid escape sequence") {
		t.Errorf("reason = %q, want the engine's own message", got.Reason)
	}
	// The pattern still appears in Missing: the card lists what it is short of
	// either way, and the reason is additive.
	if len(got.Missing) != 1 || got.Missing[0] != pattern {
		t.Errorf("missing = %v, want [%q]", got.Missing, pattern)
	}

	// A pattern that compiles leaves the reason empty, so a card has nothing to
	// add to its missing-tag line.
	fine := Rule{Required: []string{`\[ArcA310\]`}, Mode: MatchRegex, Enforce: true}
	if ok := fine.Classify("Shrek (2001) [ArcA310].mkv"); ok.Status != StatusEligible || ok.Reason != "" {
		t.Errorf("compiling pattern: status = %q reason = %q, want eligible with no reason", ok.Status, ok.Reason)
	}

	// REGEX treats every entry the same way, so one good and one broken pattern
	// can still be satisfied: the target is eligible and must not carry a
	// reason, because it is not stuck.
	mixed := Rule{Required: []string{`\[ArcA310\]`, pattern}, Mode: MatchRegex, Enforce: true}
	if mixedRes := mixed.Classify("Shrek (2001) [ArcA310].mkv"); mixedRes.Status != StatusEligible || mixedRes.Reason != "" {
		t.Errorf("satisfiable REGEX: status = %q reason = %q, want eligible with no reason", mixedRes.Status, mixedRes.Reason)
	}

	// ANY has no patterns at all: every entry is a literal substring, so a
	// regex-shaped entry such as `(?=x)` is an ordinary filename fragment that
	// compiles nothing and is accepted when the name carries it literally. There
	// is nothing for this mode to refuse, hence nothing to explain.
	literal := Rule{Required: []string{`(?=x)`}, Mode: MatchAny, Enforce: true}
	if got := literal.Classify("Shrek (2001) (?=x).mkv"); got.Status != StatusEligible || got.Reason != "" {
		t.Errorf("literal ANY: status = %q reason = %q, want eligible with no reason", got.Status, got.Reason)
	}
	if got := literal.Classify("Shrek (2001).mkv"); got.Status != StatusWaitingForTags || got.Reason != "" {
		t.Errorf("literal ANY miss: status = %q reason = %q, want waiting_for_tags with no reason", got.Status, got.Reason)
	}

	// The arr criterion uses the same compile and the same wording.
	arrBroken := Rule{
		RequiredArrTags: []string{pattern},
		Mode:            MatchRegex,
		SourceMode:      SourceArr,
		Enforce:         true,
	}
	arrRes := arrBroken.Classify("Shrek.mkv", ArrMetadata{Found: true, Tags: []string{"dub"}})
	if arrRes.Status != StatusWaitingForTags {
		t.Fatalf("arr status = %q, want waiting_for_tags", arrRes.Status)
	}
	if !strings.Contains(arrRes.Reason, "arr tag pattern") {
		t.Errorf("arr reason = %q, want it to name the arr pattern", arrRes.Reason)
	}
}

func TestRuleClassifyFilenameTags(t *testing.T) {
	r := Rule{Required: []string{"[ArcA310]"}, Mode: MatchAny, Enforce: true}
	got := r.Classify("Shrek (2001) [ArcA310].mkv")
	if got.Status != StatusEligible {
		t.Fatalf("filename tag: status = %q, want eligible", got.Status)
	}
}

// TestRuleClassifyIgnoresDirectoryMarkers pins ADR-0005: only the base filename
// decides eligibility, never another path component.
func TestRuleClassifyIgnoresDirectoryMarkers(t *testing.T) {
	r := Rule{Required: []string{"[ArcA310]"}, Mode: MatchAny, Enforce: true}
	if got := r.Classify("/media/[ArcA310]/Shrek (2001).mkv"); got.Status != StatusWaitingForTags {
		t.Errorf("marker in directory: status = %q, want waiting_for_tags", got.Status)
	}
}

func TestRuleClassifyExcluded(t *testing.T) {
	r := Rule{
		Required: []string{"[ArcA310]"},
		Excluded: []string{"[Broken]"},
		Mode:     MatchAny,
		Enforce:  true,
	}
	got := r.Classify("f [ArcA310] [Broken].mkv")
	if got.Status != StatusWaitingForTags {
		t.Errorf("excluded tag present: status = %q, want waiting_for_tags", got.Status)
	}
}

func TestRuleClassifyEnforceOff(t *testing.T) {
	r := Rule{Required: []string{"[ArcA310]"}, Mode: MatchAny}
	if got := r.Classify("f.mkv"); got.Status != StatusEligible {
		t.Errorf("enforce off: status = %q, want eligible", got.Status)
	}
	// No required tags means nothing to wait for.
	r = Rule{Mode: MatchAny, Enforce: true}
	if got := r.Classify("f.mkv"); got.Status != StatusEligible {
		t.Errorf("no required tags: status = %q, want eligible", got.Status)
	}
}

func TestRuleFromTagFilter(t *testing.T) {
	r := RuleFromTagFilter(settings.TargetTagFilter{
		RequiredFilenameTags: []string{"[ArcA310]"},
		ExcludedFilenameTags: []string{"[X]"},
		TagMatchMode:         "all",
		TagCaseSensitive:     true,
		EnforceTagFilter:     true,
	})
	if len(r.Required) != 1 || len(r.Excluded) != 1 {
		t.Fatalf("rule = %+v, want required/excluded copied", r)
	}
	if r.Mode != MatchAll {
		t.Errorf("mode = %q, want ALL", r.Mode)
	}
	if !r.CaseSensitive || !r.Enforce {
		t.Errorf("rule = %+v, want case-sensitive enforced", r)
	}
}

// TestRuleArrSource pins ADR-0009: with source_mode=arr the arr criterion is
// the gate, tags match by label and a target without a library row fails
// closed while the filter is enforced.
func TestRuleArrSource(t *testing.T) {
	r := Rule{
		SourceMode:      SourceArr,
		RequiredArrTags: []string{"dub"},
		Mode:            MatchAny,
		Enforce:         true,
	}
	if got := r.Classify("Shrek (2001).mkv", ArrMetadata{Found: true, Tags: []string{"Polish Dubbing", "DUB"}}); got.Status != StatusEligible {
		t.Errorf("label match: status = %q, want eligible", got.Status)
	}
	if got := r.Classify("Shrek (2001).mkv", ArrMetadata{Found: true, Tags: []string{"keep"}}); got.Status != StatusWaitingForTags {
		t.Errorf("missing arr tag: status = %q, want waiting_for_tags", got.Status)
	}
	// Fail closed: no arr library row at all.
	if got := r.Classify("Shrek (2001).mkv", ArrMetadata{}); got.Status != StatusWaitingForTags {
		t.Errorf("no arr row: status = %q, want waiting_for_tags", got.Status)
	}
	// The filename criterion is not consulted in arr mode.
	r.Required = []string{"[ArcA310]"}
	if got := r.Classify("Shrek (2001) [ArcA310].mkv", ArrMetadata{Found: true, Tags: []string{"dub"}}); got.Status != StatusEligible {
		t.Errorf("arr mode ignored filename tag: status = %q, want eligible", got.Status)
	}
}

// TestRuleBothSource pins AND-across-sources: adding an arr criterion can only
// narrow eligibility.
func TestRuleBothSource(t *testing.T) {
	r := Rule{
		SourceMode:      SourceBoth,
		Required:        []string{"[ArcA310]"},
		RequiredArrTags: []string{"dub"},
		Mode:            MatchAny,
		Enforce:         true,
	}
	if got := r.Classify("f [ArcA310].mkv", ArrMetadata{Found: true, Tags: []string{"dub"}}); got.Status != StatusEligible {
		t.Errorf("both pass: status = %q, want eligible", got.Status)
	}
	if got := r.Classify("f [ArcA310].mkv", ArrMetadata{Found: true, Tags: []string{"keep"}}); got.Status != StatusWaitingForTags {
		t.Errorf("arr fails: status = %q, want waiting_for_tags", got.Status)
	}
	if got := r.Classify("f.mkv", ArrMetadata{Found: true, Tags: []string{"dub"}}); got.Status != StatusWaitingForTags {
		t.Errorf("filename fails: status = %q, want waiting_for_tags", got.Status)
	}
	if got := r.Classify("f [ArcA310].mkv", ArrMetadata{}); got.Status != StatusWaitingForTags {
		t.Errorf("no arr row: status = %q, want waiting_for_tags", got.Status)
	}
}

func TestRuleArrMatchModesAndExcluded(t *testing.T) {
	all := Rule{SourceMode: SourceArr, RequiredArrTags: []string{"dub", "kids"}, Mode: MatchAll, Enforce: true}
	if got := all.Classify("f.mkv", ArrMetadata{Found: true, Tags: []string{"dub"}}); got.Status != StatusWaitingForTags {
		t.Errorf("ALL with one of two: status = %q, want waiting_for_tags", got.Status)
	}
	if got := all.Classify("f.mkv", ArrMetadata{Found: true, Tags: []string{"dub", "Kids"}}); got.Status != StatusEligible {
		t.Errorf("ALL with both: status = %q, want eligible", got.Status)
	}

	rex := Rule{SourceMode: SourceArr, RequiredArrTags: []string{`^dub`}, Mode: MatchRegex, Enforce: true}
	if got := rex.Classify("f.mkv", ArrMetadata{Found: true, Tags: []string{"dub-pl"}}); got.Status != StatusEligible {
		t.Errorf("REGEX match: status = %q, want eligible", got.Status)
	}

	// Excluded arr tags always win, even when the required tag matched.
	excl := Rule{
		SourceMode:      SourceArr,
		RequiredArrTags: []string{"dub"},
		ExcludedArrTags: []string{"broken"},
		Mode:            MatchAny,
		Enforce:         true,
	}
	if got := excl.Classify("f.mkv", ArrMetadata{Found: true, Tags: []string{"dub", "Broken"}}); got.Status != StatusWaitingForTags {
		t.Errorf("excluded arr tag: status = %q, want waiting_for_tags", got.Status)
	}
	// A regex that cannot compile must never let a target through.
	bad := Rule{SourceMode: SourceArr, RequiredArrTags: []string{"["}, Mode: MatchRegex, Enforce: true}
	if got := bad.Classify("f.mkv", ArrMetadata{Found: true, Tags: []string{"dub"}}); got.Status != StatusWaitingForTags {
		t.Errorf("invalid arr regex: status = %q, want waiting_for_tags", got.Status)
	}
}

// TestRuleArrRootFolderScoping pins that root folders are selected by
// canonical path: a cached root outside the allowlist is not eligible.
func TestRuleArrRootFolderScoping(t *testing.T) {
	r := Rule{
		SourceMode:      SourceArr,
		RootFolderPaths: []string{"/media/movies", "/media/tv"},
		Mode:            MatchAny,
		Enforce:         true,
	}
	if got := r.Classify("f.mkv", ArrMetadata{Found: true, RootFolder: "/media/movies"}); got.Status != StatusEligible {
		t.Errorf("allowed root: status = %q, want eligible", got.Status)
	}
	if got := r.Classify("f.mkv", ArrMetadata{Found: true, RootFolder: "/media/anime"}); got.Status != StatusWaitingForTags {
		t.Errorf("other root: status = %q, want waiting_for_tags", got.Status)
	}
	if got := r.Classify("f.mkv", ArrMetadata{Found: true}); got.Status != StatusWaitingForTags {
		t.Errorf("unknown root: status = %q, want waiting_for_tags", got.Status)
	}
}

// TestRuleFilenameSourceIgnoresArrMetadata keeps ADR-0005 behaviour for the
// default mode: legacy/zero-value rules never consult arr metadata.
func TestRuleFilenameSourceIgnoresArrMetadata(t *testing.T) {
	r := Rule{Required: []string{"[ArcA310]"}, Mode: MatchAny, Enforce: true}
	if got := r.Classify("f [ArcA310].mkv", ArrMetadata{Found: true, Tags: []string{"keep"}}); got.Status != StatusEligible {
		t.Errorf("filename mode: status = %q, want eligible", got.Status)
	}
	if got := r.Classify("f.mkv", ArrMetadata{Found: true, Tags: []string{"[ArcA310]"}}); got.Status != StatusWaitingForTags {
		t.Errorf("filename mode read arr tags: status = %q, want waiting_for_tags", got.Status)
	}
}

// TestRuleArrEmptyCriteriaFailsClosedWithoutRow pins fail-closed even when the
// arr source has no tag/root criterion: the row itself is the criterion.
func TestRuleArrEmptyCriteriaFailsClosedWithoutRow(t *testing.T) {
	r := Rule{SourceMode: SourceArr, Mode: MatchAny, Enforce: true}
	if got := r.Classify("f.mkv", ArrMetadata{}); got.Status != StatusWaitingForTags {
		t.Errorf("no row: status = %q, want waiting_for_tags", got.Status)
	}
	if got := r.Classify("f.mkv", ArrMetadata{Found: true}); got.Status != StatusEligible {
		t.Errorf("row present: status = %q, want eligible", got.Status)
	}
}

// TestRuleFromTagFilterLegacyDefaults pins that a stored section written before
// source_mode existed decodes as filename-only with empty arr criteria.
func TestRuleFromTagFilterLegacyDefaults(t *testing.T) {
	var legacy settings.TargetTagFilter
	if err := json.Unmarshal([]byte(`{"required_filename_tags":["[ArcA310]"],"tag_match_mode":"ANY","tag_case_sensitive":false,"excluded_filename_tags":[],"enforce_tag_filter":true}`), &legacy); err != nil {
		t.Fatalf("decode legacy section: %v", err)
	}
	if legacy.SourceMode != "filename" {
		t.Errorf("source_mode = %q, want filename", legacy.SourceMode)
	}
	if len(legacy.RequiredArrTags) != 0 || len(legacy.ExcludedArrTags) != 0 || len(legacy.RootFolderPaths) != 0 {
		t.Errorf("legacy arr criteria = %+v, want empty", legacy)
	}
	r := RuleFromTagFilter(legacy)
	if r.SourceMode != SourceFilename {
		t.Errorf("rule source = %q, want filename", r.SourceMode)
	}
	if got := r.Classify("f.mkv", ArrMetadata{Found: true, Tags: []string{"dub"}}); got.Status != StatusWaitingForTags {
		t.Errorf("legacy gate: status = %q, want waiting_for_tags", got.Status)
	}

	r = RuleFromTagFilter(settings.TargetTagFilter{
		SourceMode:       "arr",
		RequiredArrTags:  []string{"dub"},
		RootFolderPaths:  []string{"/media/movies"},
		EnforceTagFilter: true,
	})
	if r.SourceMode != SourceArr || len(r.RequiredArrTags) != 1 || len(r.RootFolderPaths) != 1 {
		t.Errorf("rule = %+v, want arr source with criteria", r)
	}
}

// TestEligiblePath pins the projection seam: it is Classify's verdict reduced to
// one boolean, so the eligibility projection asks one question with the same
// rules the mux re-check enforces. Force override is not modelled here — the
// caller owns the override — and a missing arr row fails closed exactly as
// Classify already does (ADR-0009).
func TestEligiblePath(t *testing.T) {
	filenameTagged := Rule{Required: []string{"[ArcA310]"}, Mode: MatchAny, Enforce: true}
	arrTagged := Rule{
		SourceMode: SourceArr, RequiredArrTags: []string{"dub"},
		Mode: MatchAny, Enforce: true,
	}
	arrRooted := Rule{
		SourceMode: SourceArr, RootFolderPaths: []string{"/media/movies"},
		Mode: MatchAny, Enforce: true,
	}

	tests := []struct {
		name string
		rule Rule
		path string
		meta ArrMetadata
		want bool
	}{
		{
			// The neutral default (ADR-0012): nothing is required, so every
			// target is eligible and no hold is ever recorded for it.
			name: "neutral rule is eligible",
			rule: Rule{Enforce: false},
			path: "/media/movies/Shrek (2001).mkv",
			want: true,
		},
		{
			name: "filename tag present",
			rule: filenameTagged,
			path: "/media/movies/Shrek (2001) [ArcA310].mkv",
			want: true,
		},
		{
			name: "filename tag missing",
			rule: filenameTagged,
			path: "/media/movies/Shrek (2001).mkv",
			want: false,
		},
		{
			name: "excluded filename tag blocks",
			rule: Rule{Excluded: []string{"[WIP]"}, Mode: MatchAny, Enforce: true},
			path: "/media/movies/Shrek [WIP].mkv",
			want: false,
		},
		{
			// Fail closed: no arr row means the arr criterion cannot be proven.
			name: "arr source without a cache row",
			rule: arrTagged,
			path: "/media/movies/Shrek (2001).mkv",
			meta: ArrMetadata{Found: false, Tags: []string{"dub"}},
			want: false,
		},
		{
			name: "arr source with the tag",
			rule: arrTagged,
			path: "/media/movies/Shrek (2001).mkv",
			meta: ArrMetadata{Found: true, Tags: []string{"dub"}},
			want: true,
		},
		{
			name: "arr source missing the tag",
			rule: arrTagged,
			path: "/media/movies/Shrek (2001).mkv",
			meta: ArrMetadata{Found: true, Tags: []string{"keep"}},
			want: false,
		},
		{
			name: "root folder outside the allowlist",
			rule: arrRooted,
			path: "/media/movies/Shrek (2001).mkv",
			meta: ArrMetadata{Found: true, RootFolder: "/media/anime"},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rule.EligiblePath(tc.path, tc.meta); got != tc.want {
				t.Fatalf("EligiblePath = %v, want %v", got, tc.want)
			}
			// One rule, one verdict: the wrapper must never disagree with the
			// classification the mux and the queue already read.
			classified := tc.rule.Classify(tc.path, tc.meta).Status == StatusEligible
			if classified != tc.want {
				t.Errorf("Classify disagrees with EligiblePath: %v vs %v", classified, tc.want)
			}
		})
	}
}
