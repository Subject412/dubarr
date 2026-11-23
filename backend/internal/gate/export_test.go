package gate

import (
	"testing"

	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/settings"
)

func track(index int, lang, title string, comment bool) probe.AudioTrack {
	return probe.AudioTrack{
		Index:         index,
		Language:      lang,
		LanguageISO1:  probe.ISO639_1(lang),
		LanguageISO2B: probe.ISO639_2B(lang),
		Title:         title,
		Disposition:   probe.Disposition{Comment: comment},
	}
}

func TestSplitExportTracksRuleBased(t *testing.T) {
	tracks := []probe.AudioTrack{
		track(1, "pol", "Polski Dubbing", false),
		track(2, "eng", "", false),
		track(3, "jpn", "", false),
		track(4, "und", "", false),
		track(5, "eng", "Commentary with director", true),
	}
	pol := ExportPolicy{
		TargetLanguages:    []string{"pl"},
		ReferenceLanguages: []string{"en"},
		OriginalLanguage:   "eng",
		ExcludeCommentary:  true,
	}
	got := SplitExportTracks(tracks, pol)

	if len(got.Policy) != 2 || got.Policy[0].Index != 1 || got.Policy[1].Index != 2 {
		t.Errorf("policy = %v, want tracks 1 and 2", indexes(got.Policy))
	}
	if len(got.Extras) != 2 || got.Extras[0].Index != 3 || got.Extras[1].Index != 4 {
		t.Errorf("extras = %v, want tracks 3 and 4", indexes(got.Extras))
	}
}

func TestSplitExportTracksCommentaryOptIn(t *testing.T) {
	tracks := []probe.AudioTrack{
		track(1, "pol", "", false),
		track(2, "eng", "Director commentary", false),
	}
	got := SplitExportTracks(tracks, ExportPolicy{TargetLanguages: []string{"pl"}, ExcludeCommentary: false})
	if len(got.Policy) != 1 || got.Policy[0].Index != 1 {
		t.Fatalf("policy = %v, want track 1 only", indexes(got.Policy))
	}
	if len(got.Extras) != 1 || got.Extras[0].Index != 2 {
		t.Errorf("extras = %v, want commentary track 2 opt-in", indexes(got.Extras))
	}
}

func TestSplitExportTracksConfiguredLanguageSynonyms(t *testing.T) {
	tracks := []probe.AudioTrack{
		track(1, "pol", "", false),
		track(2, "pol", "", false),
	}
	got := SplitExportTracks(tracks, ExportPolicy{TargetLanguages: []string{"pl", "pol"}})
	if len(got.Policy) != 2 {
		t.Errorf("policy = %v, want both Polish tracks", indexes(got.Policy))
	}
}

func TestSplitExportTracksKeepsOrderAndUnknownLanguages(t *testing.T) {
	tracks := []probe.AudioTrack{
		track(2, "jpn", "", false),
		track(1, "pol", "", false),
		track(3, "", "", false),
	}
	got := SplitExportTracks(tracks, ExportPolicy{TargetLanguages: []string{"pl"}})
	if len(got.Policy) != 1 || got.Policy[0].Index != 1 {
		t.Fatalf("policy = %v, want track 1", indexes(got.Policy))
	}
	if len(got.Extras) != 2 || got.Extras[0].Index != 2 || got.Extras[1].Index != 3 {
		t.Errorf("extras = %v, want tracks 2 then 3", indexes(got.Extras))
	}
}

func TestPolicyFromLanguages(t *testing.T) {
	pol := PolicyFromLanguages(settings.LanguagesPolicy{
		TargetImportLanguages:   []string{"pl"},
		ExtraReferenceLanguages: []string{"en"},
		OriginalLanguageFromArr: true,
		ExcludeCommentary:       true,
	}, "eng")
	if len(pol.TargetLanguages) != 1 || pol.TargetLanguages[0] != "pl" {
		t.Errorf("target languages = %v, want [pl]", pol.TargetLanguages)
	}
	if len(pol.ReferenceLanguages) != 1 || pol.ReferenceLanguages[0] != "en" {
		t.Errorf("reference languages = %v, want [en]", pol.ReferenceLanguages)
	}
	if pol.OriginalLanguage != "en" {
		t.Errorf("original language = %q, want en (ISO-639-1)", pol.OriginalLanguage)
	}
	if !pol.ExcludeCommentary {
		t.Error("ExcludeCommentary = false, want true")
	}
}

func TestSplitExportTracksReferenceTokens(t *testing.T) {
	tracks := []probe.AudioTrack{
		track(1, "pol", "", false),
		track(2, "eng", "", false),
		track(3, "jpn", "", false),
		track(4, "und", "", false),
		track(5, "ger", "Director commentary", true),
	}

	t.Run("all selects every usable donor language", func(t *testing.T) {
		got := SplitExportTracks(tracks, ExportPolicy{
			TargetLanguages:    []string{"pl"},
			ReferenceLanguages: []string{"all"},
			ExcludeCommentary:  true,
		})
		if len(got.Policy) != 3 || got.Policy[0].Index != 1 || got.Policy[1].Index != 2 || got.Policy[2].Index != 3 {
			t.Fatalf("policy = %v, want tracks 1,2,3", indexes(got.Policy))
		}
		if len(got.Extras) != 1 || got.Extras[0].Index != 4 {
			t.Errorf("extras = %v, want track 4 only (und)", indexes(got.Extras))
		}
	})

	t.Run("org selects the arr original", func(t *testing.T) {
		got := SplitExportTracks(tracks, ExportPolicy{
			TargetLanguages:    []string{"pl"},
			ReferenceLanguages: []string{"org"},
			OriginalLanguage:   "jpn",
			ExcludeCommentary:  true,
		})
		if len(got.Policy) != 2 || got.Policy[0].Index != 1 || got.Policy[1].Index != 3 {
			t.Fatalf("policy = %v, want tracks 1,3", indexes(got.Policy))
		}
	})

	t.Run("org without an arr original is skipped", func(t *testing.T) {
		got := SplitExportTracks(tracks, ExportPolicy{
			TargetLanguages:    []string{"pl"},
			ReferenceLanguages: []string{"org"},
			ExcludeCommentary:  true,
		})
		if len(got.Policy) != 1 || got.Policy[0].Index != 1 {
			t.Fatalf("policy = %v, want track 1 only", indexes(got.Policy))
		}
	})

	t.Run("explicit entries and all dedupe to the same selection", func(t *testing.T) {
		got := SplitExportTracks(tracks, ExportPolicy{
			TargetLanguages:    []string{"pl"},
			ReferenceLanguages: []string{"en", "all"},
			OriginalLanguage:   "eng",
			ExcludeCommentary:  true,
		})
		if len(got.Policy) != 3 {
			t.Fatalf("policy = %v, want tracks 1,2,3 exactly once", indexes(got.Policy))
		}
	})
}

func TestPolicyFromLanguagesHonoursOriginalLanguageFromArr(t *testing.T) {
	tracks := []probe.AudioTrack{
		track(1, "pol", "", false),
		track(2, "jpn", "", false),
	}

	lp := settings.LanguagesPolicy{
		TargetImportLanguages:   []string{"pl"},
		ExtraReferenceLanguages: []string{"org"},
		OriginalLanguageFromArr: false,
		ExcludeCommentary:       true,
	}
	pol := PolicyFromLanguages(lp, "jpn")
	if pol.OriginalLanguage != "" {
		t.Fatalf("original language = %q, want empty when arr metadata is disabled", pol.OriginalLanguage)
	}
	if got := SplitExportTracks(tracks, pol); len(got.Policy) != 1 || got.Policy[0].Index != 1 {
		t.Errorf("policy = %v, want track 1 only (no arr original, no org)", indexes(got.Policy))
	}

	lp.OriginalLanguageFromArr = true
	pol = PolicyFromLanguages(lp, "jpn")
	if got := SplitExportTracks(tracks, pol); len(got.Policy) != 2 {
		t.Errorf("policy = %v, want both tracks once arr metadata is enabled", indexes(got.Policy))
	}
}

func indexes(tracks []probe.AudioTrack) []int {
	out := make([]int, len(tracks))
	for i, tr := range tracks {
		out[i] = tr.Index
	}
	return out
}
