package langtoken

import (
	"reflect"
	"testing"
)

func TestToken(t *testing.T) {
	for _, raw := range []string{"org", "ORG", " org ", "All"} {
		tok, ok := Token(raw)
		if !ok {
			t.Errorf("Token(%q) = not reserved, want reserved", raw)
		}
		if tok != Org && tok != All {
			t.Errorf("Token(%q) = %q, want canonical lowercase token", raw, tok)
		}
	}
	for _, raw := range []string{"en", "", "organization", "everything"} {
		if _, ok := Token(raw); ok {
			t.Errorf("Token(%q) = reserved, want not reserved", raw)
		}
	}
}

func TestExpandExplicitFirstThenOrgThenAll(t *testing.T) {
	got := Expand(
		[]string{"ja", "org", "all", "ja"},
		"jpn",
		[]string{"fre", "eng", "jpn"},
		[]string{"pl"},
	)
	want := []string{"ja", "en", "fr"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Expand = %v, want %v", got, want)
	}
}

func TestExpandSkipsOrgWithoutOriginal(t *testing.T) {
	got := Expand([]string{"org"}, "", []string{"eng"}, nil)
	if len(got) != 0 {
		t.Fatalf("Expand = %v, want no candidates", got)
	}
}

func TestExpandSkipsOriginalThatIsATarget(t *testing.T) {
	got := Expand([]string{"org", "all"}, "pol", []string{"eng", "pol"}, []string{"pl"})
	want := []string{"en"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Expand = %v, want %v", got, want)
	}
}

func TestExpandWithoutTokensKeepsConfiguredOrder(t *testing.T) {
	got := Expand([]string{"de", "en"}, "jpn", []string{"en", "fr"}, []string{"pl"})
	want := []string{"de", "en"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Expand = %v, want %v", got, want)
	}
}
