// The media-info normaliser: whichever shape an *arr instance sends, the cache
// stores canonical ISO 639-1 codes in stream order (U02).
package arr

import (
	"encoding/json"
	"slices"
	"testing"
)

// mediaInfoFromJSON decodes a raw `mediaInfo` object exactly as the client sees
// it, so the normaliser is tested against the wire shapes and not against a
// hand-built struct a parse bug could not reach.
func mediaInfoFromJSON(t *testing.T, raw string) *MediaInfo {
	t.Helper()
	var file MediaFile
	if err := json.Unmarshal([]byte(`{"mediaInfo":`+raw+`}`), &file); err != nil {
		t.Fatalf("decode mediaInfo %s: %v", raw, err)
	}
	return file.MediaInfo
}

// TestAudioLanguageCodesPrefersStreams pins that a v5 instance, which sends no
// audioLanguages string at all, is read from its stream list.
func TestAudioLanguageCodesPrefersStreams(t *testing.T) {
	info := mediaInfoFromJSON(t, `{
		"audioStreams": [{"language": "Polish"}, {"language": "English"}]
	}`)
	if got, want := info.AudioLanguageCodes(), []string{"pl", "en"}; !slices.Equal(got, want) {
		t.Fatalf("AudioLanguageCodes() = %v, want %v", got, want)
	}
}

// TestAudioLanguageCodesPrefersStreamsOverLegacyString pins the precedence: when
// both shapes are present (Radarr v3 sends both) the stream list wins, because
// it is the source the legacy string is derived from.
func TestAudioLanguageCodesPrefersStreamsOverLegacyString(t *testing.T) {
	info := mediaInfoFromJSON(t, `{
		"audioLanguages": "French/German",
		"audioStreams": [{"language": "Polish"}, {"language": "English"}]
	}`)
	if got, want := info.AudioLanguageCodes(), []string{"pl", "en"}; !slices.Equal(got, want) {
		t.Fatalf("AudioLanguageCodes() = %v, want the stream list %v", got, want)
	}
}

// TestAudioLanguageCodesFromLegacyString covers Sonarr v3 / Radarr v3, which
// join the display names with "/" and send no stream list.
func TestAudioLanguageCodesFromLegacyString(t *testing.T) {
	info := mediaInfoFromJSON(t, `{"audioLanguages": "Polish/English"}`)
	if got, want := info.AudioLanguageCodes(), []string{"pl", "en"}; !slices.Equal(got, want) {
		t.Fatalf("AudioLanguageCodes() = %v, want %v", got, want)
	}
}

// TestAudioLanguageCodesStripsQualifier pins that a media-info display name
// keeps its base language: the parenthesised qualifier is not an ISO concept.
func TestAudioLanguageCodesStripsQualifier(t *testing.T) {
	info := mediaInfoFromJSON(t, `{
		"audioStreams": [{"language": "Spanish (Latin America)"}, {"language": " English "}]
	}`)
	if got, want := info.AudioLanguageCodes(), []string{"es", "en"}; !slices.Equal(got, want) {
		t.Fatalf("AudioLanguageCodes() = %v, want %v", got, want)
	}
}

// TestAudioLanguageCodesDropsUnresolvable pins the honesty rule: a language
// Dubarr cannot resolve is omitted, never stored as "und" or guessed.
func TestAudioLanguageCodesDropsUnresolvable(t *testing.T) {
	info := mediaInfoFromJSON(t, `{
		"audioStreams": [
			{"language": "Undetermined"}, {"language": "und"}, {"language": ""},
			{"language": "zxx"}, {"language": "Klingon (Fictional)"}
		]
	}`)
	if got := info.AudioLanguageCodes(); len(got) != 0 {
		t.Fatalf("AudioLanguageCodes() = %v, want no codes", got)
	}
}

// TestAudioLanguageCodesDeduplicatesInStreamOrder pins that a file with the
// same language twice (commentary tracks) reports it once, in first position.
func TestAudioLanguageCodesDeduplicatesInStreamOrder(t *testing.T) {
	info := mediaInfoFromJSON(t, `{
		"audioStreams": [
			{"language": "Polish"}, {"language": "English"}, {"language": "Polish"},
			{"language": "en-US"}
		]
	}`)
	if got, want := info.AudioLanguageCodes(), []string{"pl", "en"}; !slices.Equal(got, want) {
		t.Fatalf("AudioLanguageCodes() = %v, want %v", got, want)
	}
}

// TestAudioLanguageCodesEmptyIsEmptySlice pins that both shapes absent yields an
// empty slice, not nil: a nil slice marshals as JSON null, and the column is
// documented as an array.
func TestAudioLanguageCodesEmptyIsEmptySlice(t *testing.T) {
	for name, raw := range map[string]string{
		"no mediaInfo":   `{}`,
		"both empty":     `{"audioLanguages": "", "audioStreams": []}`,
		"empty streams":  `{"audioStreams": [{"language": ""}]}`,
		"legacy slashes": `{"audioLanguages": "//"}`,
	} {
		info := mediaInfoFromJSON(t, raw)
		got := info.AudioLanguageCodes()
		if got == nil {
			t.Fatalf("%s: AudioLanguageCodes() is nil, want an empty slice", name)
		}
		if len(got) != 0 {
			t.Fatalf("%s: AudioLanguageCodes() = %v, want no codes", name, got)
		}
	}
}

// TestAudioLanguageCodesNilReceiver pins the nil-safety of a movie or episode
// arr has no file for.
func TestAudioLanguageCodesNilReceiver(t *testing.T) {
	var info *MediaInfo
	if got := info.AudioLanguageCodes(); got == nil || len(got) != 0 {
		t.Fatalf("AudioLanguageCodes() on nil = %v, want an empty slice", got)
	}
}
