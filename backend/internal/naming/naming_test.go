package naming

import (
	"path/filepath"
	"strings"
	"testing"
)

// the three shipped patterns, written out here so a change to settings cannot
// silently redefine the grammar these tests pin.
const (
	defaultMovieTemplate   = "{Title} ({Year}) [{Variant} {Codec} {Channels}]"
	defaultEpisodeTemplate = "{Title} - S{Season:00}E{Episode:00} - {Episode Title} [{Variant} {Codec} {Channels}]"
	defaultSeasonFolder    = "Season {Season:00}"
)

func mustParse(t *testing.T, raw string) Template {
	t.Helper()
	tmpl, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	return tmpl
}

func TestRenderMovieDefault(t *testing.T) {
	tr := Track{
		Title:    "Spirited Away",
		Year:     2001,
		Variant:  "dubbing",
		Codec:    "eac3",
		Channels: "5.1",
		LangISO2: "pl",
	}
	stem, err := mustParse(t, defaultMovieTemplate).Render(tr)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if want := "Spirited Away (2001) [Dubbing EAC3 5.1]"; stem != want {
		t.Fatalf("stem = %q, want %q", stem, want)
	}
	path := VaultPath("/vault/Spirited Away (2001)", "", tr, stem)
	if want := "/vault/Spirited Away (2001)/Spirited Away (2001) [Dubbing EAC3 5.1].pl.mka"; path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
}

func TestRenderEpisodeDefault(t *testing.T) {
	tr := Track{
		Title:        "The Office",
		Year:         2005,
		Season:       2,
		Episode:      3,
		EpisodeTitle: "Office Olympics",
		Variant:      "lektor",
		Codec:        "eac3",
		Channels:     "2.0",
		LangISO2:     "pl",
	}
	stem, err := mustParse(t, defaultEpisodeTemplate).Render(tr)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if want := "The Office - S02E03 - Office Olympics [Lektor EAC3 2.0]"; stem != want {
		t.Fatalf("stem = %q, want %q", stem, want)
	}

	folder, err := mustParse(t, defaultSeasonFolder).RenderFolder(tr)
	if err != nil {
		t.Fatalf("RenderFolder: %v", err)
	}
	if folder != "Season 02" {
		t.Fatalf("season folder = %q, want %q", folder, "Season 02")
	}

	path := VaultPath("/vault/The Office (2005)", folder, tr, stem)
	want := "/vault/The Office (2005)/Season 02/The Office - S02E03 - Office Olympics [Lektor EAC3 2.0].pl.mka"
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
}

func TestRenderRemovesEmptyOptionalGroups(t *testing.T) {
	tr := Track{Title: "Spirited Away", LangISO2: "pl"}
	stem, err := mustParse(t, defaultMovieTemplate).Render(tr)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if stem != "Spirited Away" {
		t.Fatalf("stem = %q, want %q", stem, "Spirited Away")
	}
	for _, bad := range []string{"[]", "()", "  ", " -"} {
		if strings.Contains(stem, bad) {
			t.Fatalf("stem %q still contains %q", stem, bad)
		}
	}

	// A partially populated group keeps its content and its spacing.
	tr = Track{Title: "Spirited Away", Year: 2001, Channels: "5.1", LangISO2: "pl"}
	stem, err = mustParse(t, defaultMovieTemplate).Render(tr)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if want := "Spirited Away (2001) [5.1]"; stem != want {
		t.Fatalf("stem = %q, want %q", stem, want)
	}
}

func TestRenderUnmatchedEpisodeTitleLeavesCleanName(t *testing.T) {
	tr := Track{
		Title:    "The Office",
		Season:   2,
		Episode:  3,
		Variant:  "lektor",
		Codec:    "eac3",
		Channels: "2.0",
		LangISO2: "pl",
	}
	stem, err := mustParse(t, defaultEpisodeTemplate).Render(tr)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if want := "The Office - S02E03 [Lektor EAC3 2.0]"; stem != want {
		t.Fatalf("stem = %q, want %q", stem, want)
	}
}

func TestRenderRejectsLanguageToken(t *testing.T) {
	tmpl, err := Parse("{Title}.{Language}")
	if err == nil {
		_, err = tmpl.Render(Track{Title: "x"})
	}
	if err == nil {
		t.Fatal("a {Language} pattern was accepted")
	}
	want := "`{Language}` is not available: vault files always end in `.<language>.mka`."
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}

	for _, raw := range []string{"{Lang}", "{ISO2}", "{Ext}", "{Extension}", "{Original Title}"} {
		if _, err := Parse(raw); err == nil || !strings.Contains(err.Error(), "is not available") {
			t.Fatalf("Parse(%q) = %v, want an 'is not available' error", raw, err)
		}
	}
}

func TestRenderRejectsUnknownTokenAndBadModifier(t *testing.T) {
	if _, err := Parse("{Title} {Resolution}"); err == nil {
		t.Fatal("an unknown token was accepted")
	} else if !strings.Contains(err.Error(), "{Resolution}") || !strings.Contains(err.Error(), "{Title}") {
		t.Fatalf("error = %v, want it to name the bad token and list the token set", err)
	}
	if _, err := Parse("{Title:00}"); err == nil {
		t.Fatal("a modifier on {Title} was accepted")
	}
	if _, err := Parse("{Codec:dashed}"); err == nil {
		t.Fatal("a modifier on {Codec} was accepted")
	}
	for _, bad := range []string{"{Season:0x}", "{Episode:-1}", "{Season: 00}"} {
		if _, err := Parse(bad); err == nil || !strings.Contains(err.Error(), "zero padding") {
			t.Fatalf("Parse(%q) = %v, want a zero-padding error", bad, err)
		}
	}
	if _, err := Parse("{Title"); err == nil {
		t.Fatal("an unclosed token was accepted")
	}
	if _, err := Parse("Title}"); err == nil {
		t.Fatal("an unmatched '}' was accepted")
	}
	if _, err := Parse("{}"); err == nil {
		t.Fatal("an empty token was accepted")
	}
}

func TestSeasonEpisodePadding(t *testing.T) {
	tr := Track{Title: "Show", Season: 2, Episode: 12, LangISO2: "pl"}
	stem, err := mustParse(t, "S{Season:00}E{Episode:00}").Render(tr)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if stem != "S02E12" {
		t.Fatalf("stem = %q, want %q", stem, "S02E12")
	}
	stem, err = mustParse(t, "S{Season:000}E{Episode}").Render(tr)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if stem != "S002E12" {
		t.Fatalf("stem = %q, want %q", stem, "S002E12")
	}
}

func TestCleanTitleStripsForbiddenCharacters(t *testing.T) {
	cases := map[string]string{
		"A/B: C?":            "A B C",
		`a\b*c"d<e>f|g`:      "a b c d e f g",
		"  .Trailing dots. ": "Trailing dots",
		"Tab\there":          "Tab here",
		"CON":                "CON", // reserved names are a CleanFileName concern
	}
	for in, want := range cases {
		if got := CleanTitle(in); got != want {
			t.Fatalf("CleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanFileNameRejectsReservedAndDotNames(t *testing.T) {
	for _, bad := range []string{"CON", "con", "NUL", "COM1", "com9", "LPT9", ".", "..", "   "} {
		if _, err := CleanFileName(bad); err == nil {
			t.Fatalf("CleanFileName(%q) = nil error, want a refusal", bad)
		}
	}
	for _, good := range []string{"Spirited Away (2001)", "CON (2001)", "Contact", "Season 02"} {
		if _, err := CleanFileName(good); err != nil {
			t.Fatalf("CleanFileName(%q) = %v, want nil", good, err)
		}
	}
	// The device-name rule reaches past an extension, which is how an SMB
	// client resolves it.
	if _, err := CleanFileName("CON.pl"); err == nil {
		t.Fatal("CleanFileName(\"CON.pl\") = nil error, want a refusal")
	}
}

func TestRenderRejectsDoubleMkaSuffix(t *testing.T) {
	if _, err := Parse("{Title}.mka"); err == nil {
		t.Fatal("a pattern ending in .mka was accepted")
	} else if !strings.Contains(err.Error(), ".mka") {
		t.Fatalf("error = %v, want it to name .mka", err)
	}
	// The suffix is refused in any case: the writer appends a lowercase one.
	if _, err := Parse("{Title}.MKA"); err == nil {
		t.Fatal("a pattern ending in .MKA was accepted")
	}
}

func TestRenderRejectsOverlongStem(t *testing.T) {
	raw := strings.Repeat("a", MaxStemBytes+1)
	if _, err := Parse(raw); err == nil {
		t.Fatal("a 201-byte pattern was accepted")
	} else if !strings.Contains(err.Error(), "200") {
		t.Fatalf("error = %v, want it to name the 200-byte limit", err)
	}
	// A template that only gets too long once a value is substituted is a
	// per-file error, not a pattern error.
	if err := Validate("{Title}"); err != nil {
		t.Fatalf("Validate({Title}) = %v, want nil", err)
	}
	if _, err := mustParse(t, "{Title}").Render(Track{Title: strings.Repeat("b", 300)}); err == nil {
		t.Fatal("a 300-byte title rendered a legal stem")
	}
}

func TestRenderRejectsEmptyStemAndReservedName(t *testing.T) {
	if _, err := mustParse(t, "{Year}").Render(Track{Title: "x"}); err == nil {
		t.Fatal("a pattern rendering an empty name was accepted")
	}
	if _, err := mustParse(t, "{Title}").Render(Track{Title: "CON"}); err == nil {
		t.Fatal("a reserved name was accepted")
	}
}

func TestVaultPathAlwaysAppendsIso2Suffix(t *testing.T) {
	tr := Track{LangISO2: "pl"}
	if got, want := VaultPath("/vault/Show", "", tr, "Stem"), "/vault/Show/Stem.pl.mka"; got != want {
		t.Fatalf("VaultPath = %q, want %q", got, want)
	}
	if got, want := VaultPath("/vault/Show", "Season 02", tr, "Stem"), "/vault/Show/Season 02/Stem.pl.mka"; got != want {
		t.Fatalf("VaultPath = %q, want %q", got, want)
	}
	// Parity with vault.Writer, which keeps its own append this release:
	// filepath.Join(vaultDir, baseName+"."+iso2+".mka").
	iso2, base := "pl", "Spirited Away (2001)"
	want := filepath.Join("/vault", base+"."+iso2+".mka")
	if got := VaultPath("/vault", "", Track{LangISO2: iso2}, base); got != want {
		t.Fatalf("VaultPath = %q, vault.Writer would write %q", got, want)
	}
}

func TestSampleRendersForEveryPreset(t *testing.T) {
	presets := []string{
		defaultMovieTemplate,
		defaultEpisodeTemplate,
		"{Title} ({Year})",
		defaultSeasonFolder,
	}
	for _, raw := range presets {
		sample := mustParse(t, raw).Sample()
		if sample == "" {
			t.Fatalf("Sample(%q) is empty", raw)
		}
		// Braces are template syntax; square brackets are literal characters the
		// default templates use around the variant group, so only an unsubstituted
		// token would be residue.
		if strings.ContainsAny(sample, "{}") {
			t.Fatalf("Sample(%q) = %q, want no template residue", raw, sample)
		}
	}
	movie := mustParse(t, defaultMovieTemplate).Sample()
	if movie != "The Office (2005) [Lektor EAC3 2.0]" {
		t.Fatalf("movie sample = %q", movie)
	}
	episode := mustParse(t, defaultEpisodeTemplate).Sample()
	if episode != "The Office - S02E03 - Office Olympics [Lektor EAC3 2.0]" {
		t.Fatalf("episode sample = %q", episode)
	}
}

func TestValidateRejectsBlankAndAcceptsDefaults(t *testing.T) {
	if err := Validate(""); err == nil {
		t.Fatal("Validate(\"\") = nil, want an error")
	}
	if err := Validate("   {Season:00}   "); err != nil {
		t.Fatalf("Validate(season folder) = %v", err)
	}
	for _, raw := range []string{defaultMovieTemplate, defaultEpisodeTemplate, defaultSeasonFolder} {
		if err := Validate(raw); err != nil {
			t.Fatalf("Validate(%q) = %v, want nil", raw, err)
		}
	}
}

func TestRenderFolderBlankMeansNoSubfolder(t *testing.T) {
	tmpl := mustParse(t, "   ")
	folder, err := tmpl.RenderFolder(Track{Season: 2})
	if err != nil {
		t.Fatalf("RenderFolder: %v", err)
	}
	if folder != "" {
		t.Fatalf("folder = %q, want an empty folder", folder)
	}
}

func TestRenderRenamesNoLanguageIntoTheSuffix(t *testing.T) {
	// The suffix comes from the track, never from the pattern: two tracks that
	// differ only by language still get distinct names.
	tr := Track{Title: "Spirited Away", Year: 2001, Variant: "dubbing", Codec: "eac3", Channels: "5.1"}
	stem, err := mustParse(t, defaultMovieTemplate).Render(tr)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	pl := VaultPath("/vault", "", Track{LangISO2: "pl"}, stem)
	en := VaultPath("/vault", "", Track{LangISO2: "en"}, stem)
	if !strings.HasSuffix(pl, ".pl.mka") || !strings.HasSuffix(en, ".en.mka") || pl == en {
		t.Fatalf("paths = %q / %q, want distinct .<iso2>.mka names", pl, en)
	}
}
