// The library mirror's language and monitored columns (U02): what the arrs
// report about a file's audio has to reach arr_library_cache in canonical
// ISO 639-1 codes, and has to disappear with the file.
package arrsync

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arr"
	"github.com/Subject412/dubarr/backend/internal/settings"
)

// languageRig is a rig with one enabled instance of kind whose client is a
// fake, so a test can change what the instance reports between passes.
func languageRig(t *testing.T, kind string) *rig {
	t.Helper()
	r := newRig(t)
	r.configure(settings.ArrConnection{Kind: kind, BaseURL: "http://" + kind + ".invalid", Enabled: true})
	r.key(kind, "key")
	return r
}

// radarrLanguagePoller wires a poller to the swappable Radarr fake.
func radarrLanguagePoller(t *testing.T, r *rig, src *fakeRadarr) *Poller {
	t.Helper()
	p, err := New(Deps{
		Store:     r.store,
		Settings:  r.settings,
		Secrets:   r.secrets,
		Interval:  time.Minute,
		Timeout:   testHTTPTimeout,
		Logf:      t.Logf,
		NewRadarr: func(string, string) (RadarrSource, error) { return src, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// fakeSonarrLang is the Sonarr side of the same seam: series and their episodes
// are replaced between passes.
type fakeSonarrLang struct {
	series   []arr.Series
	episodes map[int][]arr.Episode
}

func (f *fakeSonarrLang) Series(context.Context) ([]arr.Series, error) { return f.series, nil }

func (f *fakeSonarrLang) Episodes(_ context.Context, seriesID int) ([]arr.Episode, error) {
	return f.episodes[seriesID], nil
}

func (f *fakeSonarrLang) Tags(context.Context) ([]arr.Tag, error) { return nil, nil }

func (f *fakeSonarrLang) RootFolders(context.Context) ([]arr.RootFolder, error) { return nil, nil }

// sonarrLanguagePoller wires a poller to the swappable Sonarr fake.
func sonarrLanguagePoller(t *testing.T, r *rig, src *fakeSonarrLang) *Poller {
	t.Helper()
	p, err := New(Deps{
		Store:     r.store,
		Settings:  r.settings,
		Secrets:   r.secrets,
		Interval:  time.Minute,
		Timeout:   testHTTPTimeout,
		Logf:      t.Logf,
		NewSonarr: func(string, string) (SonarrSource, error) { return src, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// rawAudioLanguages reads the stored column exactly as SQLite holds it, so a
// test can prove the row carries codes and not the arrs' display names.
func rawAudioLanguages(t *testing.T, r *rig, mediaType string, externalID int64) string {
	t.Helper()
	var raw string
	if err := r.store.DB.QueryRow(
		`SELECT audio_languages_json FROM arr_library_cache
		 WHERE media_type = ? AND external_item_id = ?`, mediaType, externalID).Scan(&raw); err != nil {
		t.Fatalf("read audio_languages_json of %s %d: %v", mediaType, externalID, err)
	}
	return raw
}

// TestMovieAudioLanguagesAreMirrored is the owner's case: Radarr reports
// "English/Polish" for Shrek, the mirror stores ["en","pl"] and the row records
// that Radarr still monitors the film.
func TestMovieAudioLanguagesAreMirrored(t *testing.T) {
	r := languageRig(t, "radarr")
	src := &fakeRadarr{movies: []arr.Movie{{
		ID: 7, Title: "Shrek", Year: 2001, HasFile: true, Monitored: true,
		OriginalLanguage: arr.Language{ID: 1, Name: "English"},
		MovieFile: &arr.MediaFile{
			Path:      "/media/movies/Shrek (2001)/Shrek (2001).mkv",
			MediaInfo: &arr.MediaInfo{AudioLanguages: "English/Polish"},
		},
	}}}
	p := radarrLanguagePoller(t, r, src)
	if _, err := p.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	row := readCache(t, r.store)["movie|7"]
	if want := []string{"en", "pl"}; !slices.Equal(row.AudioLanguages, want) {
		t.Errorf("AudioLanguages = %v, want %v", row.AudioLanguages, want)
	}
	if got, want := rawAudioLanguages(t, r, "movie", 7), `["en","pl"]`; got != want {
		t.Errorf("audio_languages_json = %s, want %s", got, want)
	}
	if !row.Monitored {
		t.Error("monitored = false, want true (Radarr monitors the movie)")
	}
}

// TestEpisodeAudioLanguagesFromV3String covers the v3 shape: a "/"-joined
// display string, no stream list.
func TestEpisodeAudioLanguagesFromV3String(t *testing.T) {
	r := languageRig(t, "sonarr")
	src := &fakeSonarrLang{
		series: []arr.Series{{ID: 3, Title: "The Expanse", Year: 2015,
			OriginalLanguage: arr.Language{ID: 2, Name: "English"}}},
		episodes: map[int][]arr.Episode{3: {{
			ID: 42, SeriesID: 3, SeasonNumber: 1, EpisodeNumber: 1, Title: "Dulcinea", HasFile: true,
			EpisodeFile: &arr.MediaFile{
				Path:      "/media/tv/The Expanse/S01E01.mkv",
				MediaInfo: &arr.MediaInfo{AudioLanguages: "Polish/English"},
			},
		}}},
	}
	p := sonarrLanguagePoller(t, r, src)
	if _, err := p.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	row := readCache(t, r.store)["episode|42"]
	if want := []string{"pl", "en"}; !slices.Equal(row.AudioLanguages, want) {
		t.Errorf("AudioLanguages = %v, want %v", row.AudioLanguages, want)
	}
	if got, want := rawAudioLanguages(t, r, "episode", 42), `["pl","en"]`; got != want {
		t.Errorf("audio_languages_json = %s, want %s", got, want)
	}
}

// TestEpisodeAudioLanguagesFromV5Streams covers Sonarr v5, which sends no
// audioLanguages at all: the stream list is the only source.
func TestEpisodeAudioLanguagesFromV5Streams(t *testing.T) {
	r := languageRig(t, "sonarr")
	src := &fakeSonarrLang{
		series: []arr.Series{{ID: 3, Title: "The Expanse", Year: 2015,
			OriginalLanguage: arr.Language{ID: 2, Name: "English"}}},
		episodes: map[int][]arr.Episode{3: {{
			ID: 42, SeriesID: 3, SeasonNumber: 1, EpisodeNumber: 1, Title: "Dulcinea", HasFile: true,
			EpisodeFile: &arr.MediaFile{
				Path: "/media/tv/The Expanse/S01E01.mkv",
				MediaInfo: &arr.MediaInfo{AudioStreams: []arr.MediaInfoAudio{
					{Language: "Polish"}, {Language: "English"},
				}},
			},
		}}},
	}
	p := sonarrLanguagePoller(t, r, src)
	if _, err := p.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	row := readCache(t, r.store)["episode|42"]
	if want := []string{"pl", "en"}; !slices.Equal(row.AudioLanguages, want) {
		t.Errorf("AudioLanguages = %v, want %v (stream order)", row.AudioLanguages, want)
	}
}

// TestAudioLanguagesAreReplacedNotAppended pins that a file arr replaced leaves
// exactly the new set: there is no merge with the previous file's languages.
func TestAudioLanguagesAreReplacedNotAppended(t *testing.T) {
	r := languageRig(t, "radarr")
	src := &fakeRadarr{movies: []arr.Movie{{
		ID: 7, Title: "Shrek", Year: 2001, HasFile: true,
		OriginalLanguage: arr.Language{ID: 1, Name: "English"},
		MovieFile: &arr.MediaFile{
			Path:      "/media/movies/Shrek/Shrek.mkv",
			MediaInfo: &arr.MediaInfo{AudioLanguages: "English/Polish/French"},
		},
	}}}
	p := radarrLanguagePoller(t, r, src)
	if _, err := p.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got, want := rawAudioLanguages(t, r, "movie", 7), `["en","pl","fr"]`; got != want {
		t.Fatalf("first pass audio_languages_json = %s, want %s", got, want)
	}

	// The operator replaced the file with a single-track one.
	src.movies = []arr.Movie{{
		ID: 7, Title: "Shrek", Year: 2001, HasFile: true,
		OriginalLanguage: arr.Language{ID: 1, Name: "English"},
		MovieFile: &arr.MediaFile{
			Path:      "/media/movies/Shrek/Shrek.remux.mkv",
			MediaInfo: &arr.MediaInfo{AudioStreams: []arr.MediaInfoAudio{{Language: "English"}}},
		},
	}}
	stats, err := p.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync (replaced): %v", err)
	}
	if stats.Updated != 1 {
		t.Errorf("replaced stats = %+v, want the row updated", stats)
	}
	if got, want := rawAudioLanguages(t, r, "movie", 7), `["en"]`; got != want {
		t.Errorf("audio_languages_json after the replace = %s, want %s", got, want)
	}
}

// TestFilelessItemClearsAudioLanguages pins that the languages go with the
// file: an item arr no longer has a file for keeps no stale codes.
func TestFilelessItemClearsAudioLanguages(t *testing.T) {
	r := languageRig(t, "sonarr")
	src := &fakeSonarrLang{
		series: []arr.Series{{ID: 3, Title: "The Expanse", Year: 2015,
			OriginalLanguage: arr.Language{ID: 2, Name: "English"}}},
		episodes: map[int][]arr.Episode{3: {{
			ID: 42, SeriesID: 3, SeasonNumber: 1, EpisodeNumber: 1, Title: "Dulcinea", HasFile: true,
			EpisodeFile: &arr.MediaFile{
				Path:      "/media/tv/The Expanse/S01E01.mkv",
				MediaInfo: &arr.MediaInfo{AudioLanguages: "Polish"},
			},
		}}},
	}
	p := sonarrLanguagePoller(t, r, src)
	if _, err := p.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got, want := rawAudioLanguages(t, r, "episode", 42), `["pl"]`; got != want {
		t.Fatalf("first pass audio_languages_json = %s, want %s", got, want)
	}

	// Sonarr lost the file (deleted, or a failed import).
	fileless := src.episodes[3][0]
	fileless.HasFile = false
	fileless.EpisodeFile = nil
	src.episodes[3] = []arr.Episode{fileless}
	if _, err := p.Sync(context.Background()); err != nil {
		t.Fatalf("Sync (fileless): %v", err)
	}

	row := readCache(t, r.store)["episode|42"]
	if row.TargetFilePath != "" {
		t.Errorf("target path = %q, want none", row.TargetFilePath)
	}
	if got, want := rawAudioLanguages(t, r, "episode", 42), `[]`; got != want {
		t.Errorf("audio_languages_json after the file vanished = %s, want %s", got, want)
	}
	if len(row.AudioLanguages) != 0 {
		t.Errorf("AudioLanguages = %v, want none", row.AudioLanguages)
	}
}

// TestUnresolvableAudioLanguageIsDropped pins the honesty rule at the row
// level: a name Dubarr cannot resolve is omitted, never stored as "und" or a
// raw display name, while the resolvable ones still land.
func TestUnresolvableAudioLanguageIsDropped(t *testing.T) {
	r := languageRig(t, "radarr")
	src := &fakeRadarr{movies: []arr.Movie{{
		ID: 7, Title: "Shrek", Year: 2001, HasFile: true,
		OriginalLanguage: arr.Language{ID: 1, Name: "English"},
		MovieFile: &arr.MediaFile{
			Path: "/media/movies/Shrek/Shrek.mkv",
			MediaInfo: &arr.MediaInfo{AudioStreams: []arr.MediaInfoAudio{
				{Language: "Undetermined"}, {Language: "English"}, {Language: "und"},
				{Language: ""}, {Language: "zxx"}, {Language: "Klingon"},
			}},
		},
	}}}
	p := radarrLanguagePoller(t, r, src)
	if _, err := p.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	row := readCache(t, r.store)["movie|7"]
	if want := []string{"en"}; !slices.Equal(row.AudioLanguages, want) {
		t.Errorf("AudioLanguages = %v, want %v", row.AudioLanguages, want)
	}
	if got, want := rawAudioLanguages(t, r, "movie", 7), `["en"]`; got != want {
		t.Errorf("audio_languages_json = %s, want %s", got, want)
	}
}
