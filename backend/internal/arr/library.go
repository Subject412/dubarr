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
