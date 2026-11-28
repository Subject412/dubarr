package probe

import "testing"

func TestISO639_1(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		// ISO 639-1 input, any case / padding.
		{"en", "en"},
		{"PL", "pl"},
		{" und ", ""},
		// ISO 639-2/B and /T input.
		{"pol", "pl"},
		{"eng", "en"},
		{"ger", "de"},
		{"deu", "de"},
		{"fre", "fr"},
		{"fra", "fr"},
		{"cze", "cs"},
		{"ces", "cs"},
		{"chi", "zh"},
		{"zho", "zh"},
		{"gre", "el"},
		{"ell", "el"},
		// BCP-47 style tags: primary subtag wins.
		{"en-US", "en"},
		{"pl_PL", "pl"},
		{"pt-BR", "pt"},
		// Human-readable names, case-insensitive.
		{"English", "en"},
		{"polish", "pl"},
		{"Japanese", "ja"},
		{"Brazilian Portuguese", "pt"},
		// Unknown / absent.
		{"", ""},
		{"und", ""},
		{"Undetermined", ""},
		{"Klingon", ""},
		{"zzz", ""},
		{"1", ""},
	}
	for _, tt := range tests {
		if got := ISO639_1(tt.in); got != tt.want {
			t.Errorf("ISO639_1(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestISO639_2B(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"en", "eng"},
		{"pl", "pol"},
		{"ja", "jpn"},
		{"de", "ger"}, // bibliographic form
		{"fr", "fre"},
		{"cs", "cze"},
		{"zh", "chi"},
		{"el", "gre"},
		{"pt-BR", "por"},
		// 639-2 input is normalised to the /B form.
		{"deu", "ger"},
		{"fra", "fre"},
		{"ces", "cze"},
		// Names.
		{"Polish", "pol"},
		{"English", "eng"},
		// 'und' passthrough, empty and unknown collapse to 'und'.
		{"und", "und"},
		{"UND", "und"},
		{"", "und"},
		{"Klingon", "und"},
		// Unmapped but structurally valid codes pass through lower-cased.
		{"tlh", "tlh"},
	}
	for _, tt := range tests {
		if got := ISO639_2B(tt.in); got != tt.want {
			t.Errorf("ISO639_2B(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestISO639_1To2BRoundTrip(t *testing.T) {
	for _, iso1 := range []string{"en", "pl", "ja", "de", "fr", "es", "it", "ru", "uk", "sv"} {
		iso2 := ISO639_2B(iso1)
		if iso2 == "und" {
			t.Errorf("ISO639_2B(%q) = und, want a mapped code", iso1)
			continue
		}
		if back := ISO639_1(iso2); back != iso1 {
			t.Errorf("round trip %q -> %q -> %q", iso1, iso2, back)
		}
	}
}

// TestLanguageName pins the track-title source: the table's English name with
// the first letter upper-cased, for both ISO 639-1 and 639-2 spellings, and ""
// for anything the table cannot resolve (so the muxer falls back, never an
// empty --track-name).
func TestLanguageName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"pl", "Polish"},
		{"pol", "Polish"},
		{"polish", "Polish"},
		{"PL", "Polish"},
		{"fr", "French"},
		{"fre", "French"},
		{"fra", "French"},
		{"pt-BR", "Portuguese"},
		// Unresolvable: absent, undetermined and unknown all fall back.
		{"", ""},
		{"und", ""},
		{"Undetermined", ""},
		{"zz", ""},
		{"Klingon", ""},
	}
	for _, tc := range tests {
		if got := LanguageName(tc.in); got != tc.want {
			t.Errorf("LanguageName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestISO639TableIntegrity(t *testing.T) {
	for iso1, entry := range iso639Table {
		if len(iso1) != 2 {
			t.Errorf("table key %q is not an ISO 639-1 code", iso1)
		}
		if entry.iso2B == "" {
			t.Errorf("%q has empty ISO 639-2/B code", iso1)
		}
		if entry.name == "" {
			t.Errorf("%q has empty language name", iso1)
		}
		for _, alias := range entry.aliases {
			if alias != lowerASCII(alias) {
				t.Errorf("%q alias %q is not lower-case", iso1, alias)
			}
		}
	}
}
