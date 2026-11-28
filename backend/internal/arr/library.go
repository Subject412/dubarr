// Library file payloads for the M6-1 library sync.
//
// Radarr's movie list embeds `movieFile` unconditionally, so the parse of
// GET {apiBase}/movie needs no extra parameter. Sonarr's episode list does not:
// it answers every episode with `hasFile: true` and no `episodeFile` unless the
// caller asks for that sub-resource, so Episodes() sends
// includeSubresources=episodeFile (Sonarr 5 spelling; v3's includeEpisodeFile
// is gone with the v3 API). Reading the file from the same response keeps the
// sync at one request per instance plus one per series instead of a fan-out
// over the file endpoints.
package arr

import (
	"context"
	"strconv"
	"strings"

	"github.com/Subject412/dubarr/backend/internal/probe"
)

// MediaFile is the downloaded file *arr attached to a movie or an episode. Path
// is the absolute path *arr reports; RelativePath is relative to the item
// folder and is kept for diagnostics.
type MediaFile struct {
	ID           int    `json:"id"`
	MovieID      int    `json:"movieId"`
	SeriesID     int    `json:"seriesId"`
	SeasonNumber int    `json:"seasonNumber"`
	RelativePath string `json:"relativePath"`
	Path         string `json:"path"`
	Size         int64  `json:"size"`
	// MediaInfo is the file's media-info block, nil when the instance sent
	// none (a file arr has not scanned, or no file at all).
	MediaInfo *MediaInfo `json:"mediaInfo"`
}

// MediaInfo is the audit of one media file. Both shapes the arrs emit are
// modelled here, because which one arrives depends on the product and its API
// version:
//
//   - AudioLanguages is the display string the v3 APIs join with "/"
//     ("English/Polish"). Radarr v3 still sends it, marked obsolete in its own
//     resource.
//   - AudioStreams is the per-stream list Radarr v3 and Sonarr v5 send. Sonarr
//     v5 sends *only* this one.
//
// AudioLanguageCodes is the single place that reconciles them.
type MediaInfo struct {
	AudioLanguages string           `json:"audioLanguages"`
	AudioStreams   []MediaInfoAudio `json:"audioStreams"`
}

// MediaInfoAudio is one entry of MediaInfo.AudioStreams. Language is the
// media-info *name* ("Polish", "Spanish (Latin America)"), not a code.
type MediaInfoAudio struct {
	Language string `json:"language"`
}

// AudioLanguageCodes flattens whichever shape the instance sent into canonical
// ISO 639-1 codes, in stream order, de-duplicated.
//
// A language Dubarr cannot resolve is omitted rather than guessed: an "und", an
// unknown name or an empty entry is not a language the operator can act on, and
// the columns it feeds are documented as two-letter codes.
func (m *MediaInfo) AudioLanguageCodes() []string {
	out := []string{}
	if m == nil {
		return out
	}
	raw := make([]string, 0, len(m.AudioStreams))
	for _, stream := range m.AudioStreams {
		raw = append(raw, stream.Language)
	}
	if len(raw) == 0 {
		// The v3 display string: the same languages, joined with "/".
		raw = strings.Split(m.AudioLanguages, "/")
	}
	seen := make(map[string]bool, len(raw))
	for _, name := range raw {
		code := probe.ISO639_1(trimLanguageQualifier(name))
		if code == "" || seen[code] {
			continue
		}
		seen[code] = true
		out = append(out, code)
	}
	return out
}

// trimLanguageQualifier drops the parenthesised qualifier media-info names
// carry ("Spanish (Latin America)" -> "Spanish"). It is never an ISO concept, so
// it must not reach the lookup. A name that *starts* with the parenthesis keeps
// its text and simply fails the lookup instead of becoming a prefix.
func trimLanguageQualifier(raw string) string {
	name := strings.TrimSpace(raw)
	if i := strings.Index(name, "("); i > 0 {
		return strings.TrimSpace(name[:i])
	}
	return name
}

// Episode is the subset of a Sonarr /api/v5/episode record Dubarr needs. The
// episode id is the arr_library_cache external_item_id for media_type
// "episode"; EpisodeFile is nil until Sonarr has imported the file, and the
// list only carries it because Episodes() asks for the sub-resource.
type Episode struct {
	ID            int        `json:"id"`
	SeriesID      int        `json:"seriesId"`
	SeasonNumber  int        `json:"seasonNumber"`
	EpisodeNumber int        `json:"episodeNumber"`
	Title         string     `json:"title"`
	AirDateUTC    string     `json:"airDateUtc"`
	Monitored     bool       `json:"monitored"`
	HasFile       bool       `json:"hasFile"`
	EpisodeFile   *MediaFile `json:"episodeFile"`
}

// Episodes lists one series' episodes together with their file
// (GET {apiBase}/episode?seriesId=N&includeSubresources=episodeFile).
//
// The sub-resource parameter is not optional. Without it Sonarr answers
// `hasFile: true, episodeFile: null` for every episode of a series that has
// files on disk, so no row would get a target path — which is exactly the bug
// this call exists to avoid. The name and the value are fixed literals, never
// built from user data.
//
// Sonarr requires the seriesId filter; the whole-library episode list is
// deliberately not exposed.
func (c *SonarrClient) Episodes(ctx context.Context, seriesID int) ([]Episode, error) {
	out := []Episode{}
	path := c.apiBase + "/episode?seriesId=" + strconv.Itoa(seriesID) + "&includeSubresources=episodeFile"
	err := c.apiVersionError(c.get(ctx, path, &out))
	return out, err
}
