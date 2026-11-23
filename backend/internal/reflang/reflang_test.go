package reflang

import (
	"strings"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/probe"
)

// track is a compact AudioTrack builder for table tests. lang is given in any
// spelling probe.ISO639_1 understands (eng, jpn, und, ...).
func track(index, channels int, lang string) probe.AudioTrack {
	return probe.AudioTrack{
		Index:        index,
		Channels:     channels,
		Language:     lang,
		LanguageISO1: probe.ISO639_1(lang),
	}
}

func commentary(t probe.AudioTrack) probe.AudioTrack {
	t.Disposition.Comment = true
	return t
}

func TestResolvePriorityOrder(t *testing.T) {
	tests := []struct {
		name       string
		target     []probe.AudioTrack
		vault      []probe.AudioTrack
		policy     Languages
		original   string
		wantPairs  []struct{ vault, target string } // language of each pair, in order
		wantReason bool                             // empty result must carry a reason
	}{
		{
			name: "priority 1 original language pair comes first, policy reference second",
			vault: []probe.AudioTrack{
				track(1, 2, "jpn"),
				track(2, 6, "eng"),
			},
			target: []probe.AudioTrack{
				track(1, 6, "jpn"),
				track(2, 2, "eng"),
			},
			policy:   Languages{Targets: []string{"pl"}, References: []string{"en"}, PreferOriginalLanguage: true, OriginalLanguageFromArr: true},
			original: "jpn",
			wantPairs: []struct{ vault, target string }{
				{"ja", "ja"},
				{"en", "en"},
			},
		},
		{
			name: "no original language falls back to policy reference order",
			vault: []probe.AudioTrack{
				track(1, 2, "eng"),
				track(2, 2, "fre"),
			},
			target: []probe.AudioTrack{
				track(1, 2, "eng"),
				track(2, 2, "fre"),
			},
			policy: Languages{Targets: []string{"pl"}, References: []string{"fr", "en"}},
			wantPairs: []struct{ vault, target string }{
				{"fr", "fr"},
				{"en", "en"},
			},
		},
		{
			name: "shared language outside policy is a priority 2 candidate",
			vault: []probe.AudioTrack{
				track(1, 2, "jpn"),
				track(2, 2, "ger"),
			},
			target: []probe.AudioTrack{
				track(1, 2, "jpn"),
			},
			policy: Languages{Targets: []string{"pl"}},
			wantPairs: []struct{ vault, target string }{
				{"ja", "ja"},
			},
		},
		{
			name: "target import languages never act as references",
			vault: []probe.AudioTrack{
				track(1, 2, "pol"),
				track(2, 2, "eng"),
			},
			target: []probe.AudioTrack{
				track(1, 2, "pol"),
				track(2, 2, "eng"),
			},
			policy: Languages{Targets: []string{"pl"}},
			wantPairs: []struct{ vault, target string }{
				{"en", "en"},
			},
		},
		{
			name: "original language absent from target skips priority 1",
			vault: []probe.AudioTrack{
				track(1, 2, "jpn"),
				track(2, 2, "eng"),
			},
			target: []probe.AudioTrack{
				track(1, 2, "eng"),
			},
			policy:   Languages{Targets: []string{"pl"}, References: []string{"en"}},
			original: "ja",
			wantPairs: []struct{ vault, target string }{
				{"en", "en"},
			},
		},
		{
			name: "und never matches, even when policy lists it or it is original",
			vault: []probe.AudioTrack{
				track(1, 2, "und"),
				track(2, 2, "pol"),
			},
			target: []probe.AudioTrack{
				track(1, 2, "und"),
				track(2, 2, "eng"),
			},
			policy:   Languages{Targets: []string{"pl"}, References: []string{"und"}},
			original: "und",
			wantPairs: []struct{ vault, target string }{
				{"pl", "en"}, // low-pass fallback; no usable shared reference
			},
		},
		{
			name: "no shared language falls back to low-pass against target original",
			vault: []probe.AudioTrack{
				track(1, 2, "pol"),
			},
			target: []probe.AudioTrack{
				track(1, 2, "eng"),
				track(2, 6, "jpn"),
			},
			policy:   Languages{Targets: []string{"pl"}},
			original: "jpn",
			wantPairs: []struct{ vault, target string }{
				{"pl", "ja"},
			},
		},
		{
			name: "no match at all yields empty result with reason",
			vault: []probe.AudioTrack{
				track(1, 2, "eng"),
			},
			target: []probe.AudioTrack{
				track(1, 2, "jpn"),
			},
			policy:     Languages{Targets: []string{"pl"}},
			wantReason: true,
		},
		{
			name: "empty target yields empty result with reason",
			vault: []probe.AudioTrack{
				track(1, 2, "pol"),
			},
			policy:     Languages{Targets: []string{"pl"}},
			wantReason: true,
		},
		{
			name: "empty vault yields empty result with reason",
			target: []probe.AudioTrack{
				track(1, 2, "eng"),
			},
			policy:     Languages{Targets: []string{"pl"}},
			wantReason: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve(tc.target, tc.vault, tc.policy, tc.original)
			if len(got.Pairs) != len(tc.wantPairs) {
				t.Fatalf("pairs = %d (%+v), want %d", len(got.Pairs), got.Pairs, len(tc.wantPairs))
			}
			for i, want := range tc.wantPairs {
				p := got.Pairs[i]
				if p.Vault.LanguageISO1 != want.vault || p.Target.LanguageISO1 != want.target {
					t.Errorf("pair %d = %s/%s, want %s/%s",
						i, p.Vault.LanguageISO1, p.Target.LanguageISO1, want.vault, want.target)
				}
			}
			if tc.wantReason && strings.TrimSpace(got.Reason) == "" {
				t.Error("empty result has no reason")
			}
			if !tc.wantReason && got.Reason != "" {
				t.Errorf("unexpected reason %q", got.Reason)
			}
		})
	}
}

func TestResolveLowpassMethodAndTargetPreference(t *testing.T) {
	vault := []probe.AudioTrack{track(1, 2, "pol")}

	t.Run("original language wins over English and first stream", func(t *testing.T) {
		target := []probe.AudioTrack{
			track(1, 2, "eng"),
			track(2, 6, "jpn"),
			track(3, 2, "fre"),
		}
		got := Resolve(target, vault, Languages{Targets: []string{"pl"}}, "jpn")
		if len(got.Pairs) != 1 {
			t.Fatalf("pairs = %+v", got.Pairs)
		}
		p := got.Pairs[0]
		if p.Method != LowpassFallback {
			t.Errorf("method = %q, want %q", p.Method, LowpassFallback)
		}
		if p.Target.LanguageISO1 != "ja" {
			t.Errorf("target = %q, want ja", p.Target.LanguageISO1)
		}
	})

	t.Run("English wins when original is absent", func(t *testing.T) {
		target := []probe.AudioTrack{
			track(1, 2, "fre"),
			track(2, 2, "eng"),
			track(3, 6, "und"),
		}
		got := Resolve(target, vault, Languages{Targets: []string{"pl"}}, "jpn")
		if len(got.Pairs) != 1 || got.Pairs[0].Target.LanguageISO1 != "en" {
			t.Fatalf("pairs = %+v", got.Pairs)
		}
	})

	t.Run("first non-commentary stream is the last resort", func(t *testing.T) {
		target := []probe.AudioTrack{
			commentary(track(1, 2, "fre")),
			track(2, 2, "und"),
			track(3, 2, "ger"),
		}
		got := Resolve(target, vault, Languages{Targets: []string{"pl"}}, "")
		if len(got.Pairs) != 1 || got.Pairs[0].Target.Index != 2 {
			t.Fatalf("pairs = %+v", got.Pairs)
		}
	})
}

func TestResolveTieBreaks(t *testing.T) {
	t.Run("higher channel count wins on both sides", func(t *testing.T) {
		vault := []probe.AudioTrack{
			track(1, 2, "jpn"),
			track(2, 6, "jpn"),
		}
		target := []probe.AudioTrack{
			track(1, 2, "jpn"),
			track(2, 6, "jpn"),
		}
		got := Resolve(target, vault, Languages{}, "ja")
		if len(got.Pairs) != 1 {
			t.Fatalf("pairs = %+v", got.Pairs)
		}
		p := got.Pairs[0]
		if p.Vault.Index != 2 || p.Target.Index != 2 {
			t.Errorf("picked vault[%d]/target[%d], want 2/2", p.Vault.Index, p.Target.Index)
		}
		if p.Method != SameLangReference {
			t.Errorf("method = %q", p.Method)
		}
	})

	t.Run("equal channels prefer the lower stream index", func(t *testing.T) {
		vault := []probe.AudioTrack{
			track(5, 2, "eng"),
			track(2, 2, "eng"),
		}
		target := []probe.AudioTrack{
			track(4, 2, "eng"),
			track(3, 2, "eng"),
		}
		got := Resolve(target, vault, Languages{}, "")
		if len(got.Pairs) != 1 {
			t.Fatalf("pairs = %+v", got.Pairs)
		}
		if got.Pairs[0].Vault.Index != 2 || got.Pairs[0].Target.Index != 3 {
			t.Errorf("picked %+v", got.Pairs[0])
		}
	})
}

func TestResolveSkipsCommentaryAndHandlesBadMetadata(t *testing.T) {
	t.Run("commentary tracks are never references", func(t *testing.T) {
		vault := []probe.AudioTrack{
			track(1, 2, "pol"),
			commentary(track(2, 6, "eng")),
		}
		target := []probe.AudioTrack{
			track(1, 2, "eng"),
		}
		got := Resolve(target, vault, Languages{Targets: []string{"pl"}}, "")
		if len(got.Pairs) != 1 || got.Pairs[0].Method != LowpassFallback {
			t.Fatalf("pairs = %+v", got.Pairs)
		}
	})

	t.Run("normalizes raw codes without LanguageISO1", func(t *testing.T) {
		vault := []probe.AudioTrack{{Index: 1, Channels: 2, Language: "jpn"}}
		target := []probe.AudioTrack{{Index: 1, Channels: 2, Language: "ja"}}
		got := Resolve(target, vault, Languages{}, "jpn")
		if len(got.Pairs) != 1 || got.Pairs[0].Method != SameLangReference {
			t.Fatalf("pairs = %+v", got.Pairs)
		}
	})
}

func TestResolveReferenceTokens(t *testing.T) {
	base := Languages{Targets: []string{"pl"}, PreferOriginalLanguage: true, OriginalLanguageFromArr: true}

	t.Run("org expands to the arr original with explicit entries first", func(t *testing.T) {
		vault := []probe.AudioTrack{track(1, 2, "jpn"), track(2, 2, "eng")}
		target := []probe.AudioTrack{track(1, 2, "jpn"), track(2, 2, "eng")}
		policy := base
		policy.References = []string{"en", "org"}
		policy.PreferOriginalLanguage = false // isolate the token from Priority 1
		got := Resolve(target, vault, policy, "jpn")
		want := []string{"en", "ja"}
		if !equal(gotLangs(got), want) {
			t.Fatalf("pairs = %v, want %v", gotLangs(got), want)
		}
	})

	t.Run("org without an arr original is skipped and resolution continues", func(t *testing.T) {
		vault := []probe.AudioTrack{track(1, 2, "jpn"), track(2, 2, "eng")}
		target := []probe.AudioTrack{track(1, 2, "jpn"), track(2, 2, "eng")}
		policy := base
		policy.References = []string{"org"}
		policy.PreferOriginalLanguage = false
		got := Resolve(target, vault, policy, "")
		want := []string{"en", "ja"} // implicit shared languages in lexical order
		if !equal(gotLangs(got), want) {
			t.Fatalf("pairs = %v, want %v", gotLangs(got), want)
		}
		if got.Reason != "" {
			t.Errorf("reason = %q, want empty (token is skipped, not fatal)", got.Reason)
		}
	})

	t.Run("org is skipped when the original equals a target language", func(t *testing.T) {
		vault := []probe.AudioTrack{track(1, 2, "pol"), track(2, 2, "eng")}
		target := []probe.AudioTrack{track(1, 2, "pol"), track(2, 2, "eng")}
		policy := base
		policy.References = []string{"org"}
		policy.PreferOriginalLanguage = false
		got := Resolve(target, vault, policy, "pol")
		want := []string{"en"}
		if !equal(gotLangs(got), want) {
			t.Fatalf("pairs = %v, want %v", gotLangs(got), want)
		}
	})

	t.Run("all is lexical after explicit entries and deduped", func(t *testing.T) {
		vault := []probe.AudioTrack{track(1, 2, "jpn"), track(2, 2, "eng"), track(3, 2, "fre")}
		target := []probe.AudioTrack{track(1, 2, "jpn"), track(2, 2, "eng"), track(3, 2, "fre")}
		policy := base
		policy.References = []string{"ja", "all", "ja"}
		policy.PreferOriginalLanguage = false
		got := Resolve(target, vault, policy, "")
		want := []string{"ja", "en", "fr"}
		if !equal(gotLangs(got), want) {
			t.Fatalf("pairs = %v, want %v", gotLangs(got), want)
		}
	})

	t.Run("prefer original on puts the original first", func(t *testing.T) {
		vault := []probe.AudioTrack{track(1, 2, "jpn"), track(2, 2, "eng")}
		target := []probe.AudioTrack{track(1, 2, "jpn"), track(2, 2, "eng")}
		policy := base
		policy.References = []string{"en"}
		got := Resolve(target, vault, policy, "jpn")
		want := []string{"ja", "en"}
		if !equal(gotLangs(got), want) {
			t.Fatalf("pairs = %v, want %v", gotLangs(got), want)
		}
	})

	t.Run("prefer original off leaves the original as a later candidate", func(t *testing.T) {
		vault := []probe.AudioTrack{track(1, 2, "jpn"), track(2, 2, "eng")}
		target := []probe.AudioTrack{track(1, 2, "jpn"), track(2, 2, "eng")}
		policy := base
		policy.References = []string{"en"}
		policy.PreferOriginalLanguage = false
		got := Resolve(target, vault, policy, "jpn")
		want := []string{"en", "ja"}
		if !equal(gotLangs(got), want) {
			t.Fatalf("pairs = %v, want %v", gotLangs(got), want)
		}
	})

	t.Run("original_language_from_arr off disables Priority 1 and the org token", func(t *testing.T) {
		vault := []probe.AudioTrack{track(1, 2, "jpn"), track(2, 2, "eng")}
		target := []probe.AudioTrack{track(1, 2, "jpn"), track(2, 2, "eng")}
		policy := base
		policy.References = []string{"org"}
		policy.OriginalLanguageFromArr = false
		got := Resolve(target, vault, policy, "jpn")
		want := []string{"en", "ja"} // no Priority 1, no org; implicit shared lexical
		if !equal(gotLangs(got), want) {
			t.Fatalf("pairs = %v, want %v", gotLangs(got), want)
		}
	})
}

func gotLangs(res Result) []string {
	out := make([]string, 0, len(res.Pairs))
	for _, p := range res.Pairs {
		out = append(out, p.Vault.LanguageISO1)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReportPredicates(t *testing.T) {
	target := []probe.AudioTrack{
		track(1, 2, "jpn"),
		commentary(track(2, 6, "eng")),
	}

	if !SameLang(target, "jpn") {
		t.Error("SameLang(jpn) = false, want true")
	}
	if !SameLang(target, "ja") {
		t.Error("SameLang(ja) = false, want true")
	}
	if SameLang(target, "eng") {
		t.Error("SameLang(eng) = true despite only a commentary track")
	}
	if SameLang(target, "pol") {
		t.Error("SameLang(pol) = true, want false")
	}
	if SameLang(target, "und") || SameLang(target, "") {
		t.Error("und/empty language must never be same-lang")
	}

	if Lowpass(target, "jpn") {
		t.Error("Lowpass(jpn) = true, want false")
	}
	if !Lowpass(target, "pol") {
		t.Error("Lowpass(pol) = false, want true")
	}
	// Classification follows the pair the resolver picked.
	got := Resolve([]probe.AudioTrack{track(1, 2, "eng")},
		[]probe.AudioTrack{track(1, 2, "pol")}, Languages{Targets: []string{"pl"}}, "eng")
	if len(got.Pairs) != 1 || got.Pairs[0].Method != LowpassFallback {
		t.Fatalf("pairs = %+v", got.Pairs)
	}
	if !Lowpass([]probe.AudioTrack{got.Pairs[0].Target}, got.Pairs[0].Vault.LanguageISO1) {
		t.Error("resolved low-pass pair is not classified lowpass")
	}
}

// The "all" token must not turn a title-only commentary track into a
// reference: export already excludes it, so the resolver must agree.
func TestResolveAllSkipsTitleOnlyCommentary(t *testing.T) {
	vault := []probe.AudioTrack{
		track(1, 2, "eng"),
		commentaryByTitle(track(2, 6, "jpn"), "Director commentary"),
	}
	target := []probe.AudioTrack{
		track(1, 2, "eng"),
		commentaryByTitle(track(2, 6, "jpn"), "Director commentary"),
	}
	policy := Languages{
		Targets:    []string{"pl"},
		References: []string{"all"},
	}
	got := Resolve(target, vault, policy, "")
	want := []string{"en"}
	if !equal(gotLangs(got), want) {
		t.Fatalf("pairs = %v, want %v (title-only commentary is never a reference)", gotLangs(got), want)
	}
}

func commentaryByTitle(t probe.AudioTrack, title string) probe.AudioTrack {
	t.Title = title
	return t
}
