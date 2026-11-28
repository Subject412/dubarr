package gate

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The UI shows exactly one REGEX-mode example, and it is kept in a plain text
// file rather than in a Go or a TS literal. This test compiles the same bytes
// an operator copies, so the hint cannot drift into a pattern the gate rejects.
//
// Go runs a test with its package directory as the working directory, so this
// path is relative to backend/internal/gate and climbs three levels to the repo
// root. The helper below reports the absolute path it actually tried, so a move
// of the fixture fails with an explanation instead of a bare stat error.
const tagFilterHintExamplePath = "../../../frontend/src/lib/tag-filter-regex-example.txt"

func tagFilterHintExample(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(tagFilterHintExamplePath)
	if err != nil {
		t.Fatalf("resolve the displayed tag-filter example path %q: %v", tagFilterHintExamplePath, err)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read the displayed tag-filter example %q: %v\n"+
			"the example the operator sees must be the bytes this test compiles, so this test reads the frontend-owned fixture from a full repo checkout "+
			"(relative to backend/internal/gate it is %q; if the fixture moved, update tagFilterHintExamplePath rather than shrinking the test)",
			abs, err, tagFilterHintExamplePath)
	}
	return strings.TrimSpace(string(raw))
}

// TestTagFilterHintExampleCompiles feeds the displayed example through the
// gate's own path — Rule.Classify -> matches -> regexp.Compile (RE2), the very
// call that performs the match — instead of trusting a copy of the string or
// the help text's claim about it.
func TestTagFilterHintExampleCompiles(t *testing.T) {
	example := tagFilterHintExample(t)
	if example == "" {
		t.Fatal("the displayed tag-filter example is empty")
	}
	if _, err := regexp.Compile(example); err != nil {
		t.Fatalf("displayed example %q does not compile: %v", example, err)
	}

	rule := Rule{Required: []string{example}, Mode: MatchRegex, Enforce: true}
	if got := rule.Classify("Shrek.2001.PL.DUB.1080p.WEB-DL.mkv"); got.Status != StatusEligible {
		t.Errorf("displayed example did not match a tagged file: status = %q, want eligible", got.Status)
	}
	if got := rule.Classify("Shrek.2001.ENG.1080p.WEB-DL.mkv"); got.Status != StatusWaitingForTags {
		t.Errorf("displayed example matched an untagged file: status = %q, want waiting_for_tags", got.Status)
	}

	// Case-insensitive is the shipped default: matches() prefixes "(?i)" to the
	// entry and haystack lower-cases the filename before that compile. A rule
	// that merely repeats the one above proves nothing about the rewrite, so this
	// sub-case feeds a lowercase ".pl." filename — it can only match because of
	// the rewrite — and its case-sensitive twin pins that the uppercase "PL" in
	// the example is load-bearing.
	const lowercasePL = "shrek.pl.dub.mkv"
	t.Run("case_insensitive_default_matches_lowercase_pl", func(t *testing.T) {
		defaultRule := Rule{Required: []string{example}, Mode: MatchRegex, Enforce: true}
		if got := defaultRule.Classify(lowercasePL); got.Status != StatusEligible {
			t.Errorf("displayed example %q did not match %q under the default case-insensitive rule: status = %q, want eligible (the (?i) rewrite must keep the example usable)", example, lowercasePL, got.Status)
		}
	})
	t.Run("case_sensitive_rule_refuses_lowercase_pl", func(t *testing.T) {
		caseSensitive := Rule{Required: []string{example}, Mode: MatchRegex, Enforce: true, CaseSensitive: true}
		if got := caseSensitive.Classify(lowercasePL); got.Status != StatusWaitingForTags {
			t.Errorf("displayed example %q matched %q under a case-sensitive rule: status = %q, want waiting_for_tags (case sensitivity must not be applied incidentally)", example, lowercasePL, got.Status)
		}
		// The same strict rule still matches the example's own spelling, so the
		// miss above is the case switch and not a pattern that matches nothing.
		if got := caseSensitive.Classify("Shrek.2001.PL.DUB.mkv"); got.Status != StatusEligible {
			t.Errorf("case-sensitive rule failed to match the example's own spelling: status = %q, want eligible", got.Status)
		}
	})
}

// TestTagFilterRegexRefusesPCREOnlyConstructs pins what the help text claims:
// REGEX entries are Go RE2, so the two PCRE features operators reach for most
// often are refused by the compiler with a message that names the construct,
// and the gate fails closed instead of matching something else.
func TestTagFilterRegexRefusesPCREOnlyConstructs(t *testing.T) {
	cases := []struct {
		name      string
		entry     string
		construct string
	}{
		{"backreference", `(PL)\.DUB\1`, `\1`},
		{"lookahead", `PL(?=\.DUB)`, `(?=`},
		{"lookbehind", `(?<=\.)PL`, `(?<=`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := regexp.Compile(tc.entry)
			if err == nil {
				t.Fatalf("%s entry %q compiled; RE2 does not support it", tc.name, tc.entry)
			}
			if !strings.Contains(err.Error(), tc.construct) {
				t.Errorf("compile error %q does not name the construct %s", err, tc.construct)
			}
			rule := Rule{Required: []string{tc.entry}, Mode: MatchRegex, Enforce: true}
			if got := rule.Classify("Shrek.2001.PL.DUB.mkv"); got.Status != StatusWaitingForTags {
				t.Errorf("%s entry let a file through: status = %q, want waiting_for_tags", tc.name, got.Status)
			}
		})
	}
}

// TestStoredTagPatternsKeepMatching guards the upgrade path: patterns an
// operator already stored under REGEX mode must keep compiling and matching
// through the same path the displayed example is validated against.
func TestStoredTagPatternsKeepMatching(t *testing.T) {
	cases := []struct {
		entry    string
		filename string
	}{
		{`\[ArcA310\]`, "Shrek (2001) [ArcA310].mkv"},
		{`[ArcA310]`, "Shrek (2001) [ArcA310].mkv"},
		{`^.*\.PL\..*$`, "Shrek.2001.PL.DUB.mkv"},
		{`(?i)pl\.dub`, "Shrek.PL.DUB.mkv"},
		{`ArcA31\d`, "Shrek [ArcA310].mkv"},
	}
	for _, tc := range cases {
		if _, err := regexp.Compile(tc.entry); err != nil {
			t.Errorf("stored pattern %q no longer compiles: %v", tc.entry, err)
			continue
		}
		rule := Rule{Required: []string{tc.entry}, Mode: MatchRegex, Enforce: true}
		if got := rule.Classify(tc.filename); got.Status != StatusEligible {
			t.Errorf("stored pattern %q stopped matching %q: status = %q, want eligible", tc.entry, tc.filename, got.Status)
		}
	}
}
