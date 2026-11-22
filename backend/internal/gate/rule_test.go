package gate

import (
	"testing"

	"github.com/Subject412/dubarr/backend/internal/settings"
)

func TestRuleClassifyAny(t *testing.T) {
	r := Rule{
		Required: []string{"[ArcA310]", "[NVIDIA]"},
		Mode:     MatchAny,
		Enforce:  true,
	}
	got := r.Classify("Shrek (2001).mkv", []Tag{{Name: "DUBARR_GATE", Value: "[arcA310]"}})
	if got.Status != StatusEligible {
		t.Fatalf("status = %q, want eligible", got.Status)
	}
	if len(got.Matched) != 1 || got.Matched[0] != "[ArcA310]" {
		t.Errorf("matched = %v, want [[ArcA310]]", got.Matched)
	}

	got = r.Classify("Shrek (2001).mkv", []Tag{{Name: "TITLE", Value: "Shrek"}})
	if got.Status != StatusWaitingForTags {
		t.Fatalf("status = %q, want waiting_for_tags", got.Status)
	}
	if len(got.Missing) != 2 {
		t.Errorf("missing = %v, want both required tags", got.Missing)
	}
}

func TestRuleClassifyAll(t *testing.T) {
	r := Rule{Required: []string{"[A]", "[B]"}, Mode: MatchAll, Enforce: true}
	if got := r.Classify("f.mkv", []Tag{{Name: "X", Value: "[A]"}}); got.Status != StatusWaitingForTags {
		t.Errorf("one of two present: status = %q, want waiting_for_tags", got.Status)
	}
	got := r.Classify("f.mkv", []Tag{{Name: "X", Value: "[A] [B]"}})
	if got.Status != StatusEligible {
		t.Errorf("both present: status = %q, want eligible", got.Status)
	}
}

func TestRuleClassifyCaseSensitivity(t *testing.T) {
	tags := []Tag{{Name: "GATE", Value: "[arca310]"}}
	if got := (Rule{Required: []string{"[ArcA310]"}, Mode: MatchAny, Enforce: true}).Classify("f.mkv", tags); got.Status != StatusEligible {
		t.Errorf("case-insensitive default: status = %q, want eligible", got.Status)
	}
	if got := (Rule{Required: []string{"[ArcA310]"}, Mode: MatchAny, CaseSensitive: true, Enforce: true}).Classify("f.mkv", tags); got.Status != StatusWaitingForTags {
		t.Errorf("case-sensitive: status = %q, want waiting_for_tags", got.Status)
	}
}

func TestRuleClassifyRegex(t *testing.T) {
	r := Rule{Required: []string{`\[ArcA31\d\]`}, Mode: MatchRegex, Enforce: true}
	if got := r.Classify("f.mkv", []Tag{{Name: "GATE", Value: "[ArcA310]"}}); got.Status != StatusEligible {
		t.Errorf("regex match: status = %q, want eligible", got.Status)
	}
	if got := r.Classify("f.mkv", []Tag{{Name: "GATE", Value: "[ArcA999]"}}); got.Status != StatusWaitingForTags {
		t.Errorf("regex miss: status = %q, want waiting_for_tags", got.Status)
	}
	// An invalid pattern must never make an untagged file eligible.
	if got := (Rule{Required: []string{`[`}, Mode: MatchRegex, Enforce: true}).Classify("f.mkv", nil); got.Status != StatusWaitingForTags {
		t.Errorf("invalid regex: status = %q, want waiting_for_tags", got.Status)
	}
}

func TestRuleClassifyFilenameTags(t *testing.T) {
	r := Rule{Required: []string{"[ArcA310]"}, Mode: MatchAny, Enforce: true}
	got := r.Classify("Shrek (2001) [ArcA310].mkv", nil)
	if got.Status != StatusEligible {
		t.Fatalf("filename tag: status = %q, want eligible", got.Status)
	}
}

func TestRuleClassifyExcluded(t *testing.T) {
	r := Rule{
		Required: []string{"[ArcA310]"},
		Excluded: []string{"[Broken]"},
		Mode:     MatchAny,
		Enforce:  true,
	}
	got := r.Classify("f.mkv", []Tag{{Name: "GATE", Value: "[ArcA310] [Broken]"}})
	if got.Status != StatusWaitingForTags {
		t.Errorf("excluded tag present: status = %q, want waiting_for_tags", got.Status)
	}
}

func TestRuleClassifyEnforceOff(t *testing.T) {
	r := Rule{Required: []string{"[ArcA310]"}, Mode: MatchAny}
	if got := r.Classify("f.mkv", nil); got.Status != StatusEligible {
		t.Errorf("enforce off: status = %q, want eligible", got.Status)
	}
	// No required tags means nothing to wait for.
	r = Rule{Mode: MatchAny, Enforce: true}
	if got := r.Classify("f.mkv", nil); got.Status != StatusEligible {
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
