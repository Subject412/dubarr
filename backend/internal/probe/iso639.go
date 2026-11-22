package probe

import "strings"

// langEntry is one row of the built-in ISO 639 table. Dubarr only needs a
// small, dependency-free subset (IDEA.md §3.3): enough to recognise the
// languages that appear on real media releases and to round-trip between the
// bibliographic ISO 639-2/B codes ffprobe/Matroska emit (pol, ger, fre, cze,
// chi, gre, ...), the terminological /T variants (deu, fra, ces, zho, ell),
// and the ISO 639-1 codes Dubarr normalises to (pl, de, fr, cs, zh, el, ...).
//
// aliases holds alternate spellings: the ISO 639-2/T code when it differs
// from /B, plus common English/self-language names.
type langEntry struct {
	name    string
	iso2B   string
	aliases []string
}

// iso639Table is keyed by ISO 639-1 code.
var iso639Table = map[string]langEntry{
	"af": {name: "afrikaans", iso2B: "afr"},
	"am": {name: "amharic", iso2B: "amh"},
	"ar": {name: "arabic", iso2B: "ara"},
	"az": {name: "azerbaijani", iso2B: "aze"},
	"be": {name: "belarusian", iso2B: "bel"},
	"bg": {name: "bulgarian", iso2B: "bul"},
	"bn": {name: "bengali", iso2B: "ben"},
	"bo": {name: "tibetan", iso2B: "tib", aliases: []string{"bod"}},
	"bs": {name: "bosnian", iso2B: "bos"},
	"ca": {name: "catalan", iso2B: "cat"},
	"cs": {name: "czech", iso2B: "cze", aliases: []string{"ces"}},
	"cy": {name: "welsh", iso2B: "wel", aliases: []string{"cym"}},
	"da": {name: "danish", iso2B: "dan"},
	"de": {name: "german", iso2B: "ger", aliases: []string{"deu", "deutsch"}},
	"el": {name: "greek", iso2B: "gre", aliases: []string{"ell", "modern greek"}},
	"en": {name: "english", iso2B: "eng"},
	"eo": {name: "esperanto", iso2B: "epo"},
	"es": {name: "spanish", iso2B: "spa", aliases: []string{"castilian"}},
	"et": {name: "estonian", iso2B: "est"},
	"eu": {name: "basque", iso2B: "baq", aliases: []string{"eus"}},
	"fa": {name: "persian", iso2B: "per", aliases: []string{"fas", "farsi"}},
	"fi": {name: "finnish", iso2B: "fin"},
	"fr": {name: "french", iso2B: "fre", aliases: []string{"fra", "francais"}},
	"ga": {name: "irish", iso2B: "gle"},
	"gl": {name: "galician", iso2B: "glg"},
	"gu": {name: "gujarati", iso2B: "guj", aliases: []string{"gud"}},
	"he": {name: "hebrew", iso2B: "heb", aliases: []string{"iw"}},
	"hi": {name: "hindi", iso2B: "hin"},
	"hr": {name: "croatian", iso2B: "hrv"},
	"hu": {name: "hungarian", iso2B: "hun", aliases: []string{"magyar"}},
	"hy": {name: "armenian", iso2B: "arm", aliases: []string{"hye"}},
	"id": {name: "indonesian", iso2B: "ind"},
	"is": {name: "icelandic", iso2B: "ice", aliases: []string{"isl"}},
	"it": {name: "italian", iso2B: "ita"},
	"ja": {name: "japanese", iso2B: "jpn"},
	"ka": {name: "georgian", iso2B: "geo", aliases: []string{"kat"}},
	"kk": {name: "kazakh", iso2B: "kaz"},
	"km": {name: "khmer", iso2B: "khm"},
	"kn": {name: "kannada", iso2B: "kan"},
	"ko": {name: "korean", iso2B: "kor"},
	"la": {name: "latin", iso2B: "lat"},
	"lo": {name: "lao", iso2B: "lao"},
	"lt": {name: "lithuanian", iso2B: "lit"},
	"lv": {name: "latvian", iso2B: "lav"},
	"mk": {name: "macedonian", iso2B: "mac", aliases: []string{"mkd"}},
	"ml": {name: "malayalam", iso2B: "mal"},
	"mn": {name: "mongolian", iso2B: "mon"},
	"mr": {name: "marathi", iso2B: "mar"},
	"ms": {name: "malay", iso2B: "may", aliases: []string{"msa"}},
	"my": {name: "burmese", iso2B: "bur", aliases: []string{"mya"}},
	"ne": {name: "nepali", iso2B: "nep"},
	"nl": {name: "dutch", iso2B: "dut", aliases: []string{"nld", "flemish"}},
	"no": {name: "norwegian", iso2B: "nor"},
	"pa": {name: "punjabi", iso2B: "pan"},
	"pl": {name: "polish", iso2B: "pol"},
	"pt": {name: "portuguese", iso2B: "por", aliases: []string{"brazilian portuguese"}},
	"ro": {name: "romanian", iso2B: "rum", aliases: []string{"ron"}},
	"ru": {name: "russian", iso2B: "rus"},
	"si": {name: "sinhala", iso2B: "sin"},
	"sk": {name: "slovak", iso2B: "slo", aliases: []string{"slk"}},
	"sl": {name: "slovenian", iso2B: "slv"},
	"sq": {name: "albanian", iso2B: "alb", aliases: []string{"sqi"}},
	"sr": {name: "serbian", iso2B: "srp"},
	"sv": {name: "swedish", iso2B: "swe"},
	"sw": {name: "swahili", iso2B: "swa"},
	"ta": {name: "tamil", iso2B: "tam"},
	"te": {name: "telugu", iso2B: "tel"},
	"th": {name: "thai", iso2B: "tha"},
	"tl": {name: "tagalog", iso2B: "tgl"},
	"tr": {name: "turkish", iso2B: "tur"},
	"uk": {name: "ukrainian", iso2B: "ukr"},
	"ur": {name: "urdu", iso2B: "urd"},
	"uz": {name: "uzbek", iso2B: "uzb"},
	"vi": {name: "vietnamese", iso2B: "vie"},
	"zh": {name: "chinese", iso2B: "chi", aliases: []string{"zho", "mandarin"}},
}

// undTokens are treated as "undetermined" rather than as a real language.
var undTokens = map[string]bool{
	"und":          true,
	"undetermined": true,
	"unknown":      true,
	"":             true,
}

var (
	iso1ByName map[string]string
	iso1ByCode map[string]string
)

func init() {
	n := len(iso639Table)
	iso1ByName = make(map[string]string, 2*n)
	iso1ByCode = make(map[string]string, 2*n)
	for iso1, e := range iso639Table {
		iso1ByName[e.name] = iso1
		iso1ByCode[iso1] = iso1
		iso1ByCode[e.iso2B] = iso1
		for _, alias := range e.aliases {
			iso1ByName[alias] = iso1
			if len(alias) == 3 {
				iso1ByCode[alias] = iso1
			}
		}
	}
}

// ISO639_1 normalises a language tag, ISO 639-2/B or /T code, or
// human-readable language name to the canonical ISO 639-1 two-letter code.
//
// It is case-insensitive, tolerates surrounding whitespace and BCP-47 style
// subtags ("en-US", "pt_BR"), and returns "" for absent, "und" or unknown
// languages — callers that need a always-present code use ISO639_2B instead.
// Unmapped but structurally valid two-letter codes pass through so rare
// languages are never silently dropped (IDEA.md §3.3).
func ISO639_1(raw string) string {
	tok := normalizeLangToken(raw)
	if undTokens[tok] {
		return ""
	}
	if iso1, ok := iso1ByCode[tok]; ok {
		return iso1
	}
	if iso1, ok := iso1ByName[tok]; ok {
		return iso1
	}
	if len(tok) == 2 && isAlpha(tok) {
		return tok
	}
	return ""
}

// ISO639_2B normalises the same inputs as ISO639_1 to the bibliographic
// ISO 639-2/B code (eng, pol, ger, fre, cze, chi, gre, ...). Absent or
// unrecognised languages become "und"; unmapped three-letter codes pass
// through lower-cased, mirroring the "fall back to ISO 639-2/3 or und" rule in
// IDEA.md §3.3.
func ISO639_2B(raw string) string {
	tok := normalizeLangToken(raw)
	if undTokens[tok] {
		return "und"
	}
	if iso1, ok := iso1ByCode[tok]; ok {
		return iso639Table[iso1].iso2B
	}
	if iso1, ok := iso1ByName[tok]; ok {
		return iso639Table[iso1].iso2B
	}
	if len(tok) == 3 && isAlpha(tok) {
		return tok
	}
	return "und"
}

// normalizeLangToken lower-cases a language token, trims padding and reduces
// a BCP-47/Matroska tag to its primary subtag ("en-US" -> "en").
func normalizeLangToken(raw string) string {
	tok := lowerASCII(strings.TrimSpace(raw))
	if tok == "" {
		return ""
	}
	for _, sep := range []string{"-", "_"} {
		if i := strings.Index(tok, sep); i > 0 {
			tok = tok[:i]
		}
	}
	return tok
}

func isAlpha(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return s != ""
}

// lowerASCII lower-cases ASCII letters only; language tags are ASCII.
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
