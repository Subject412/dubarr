// Library file payloads for the M6-1 library sync.
//
// Both list calls Dubarr already makes embed the downloaded file: Radarr's
// GET /api/v3/movie returns `movieFile`, and Sonarr's GET /api/v3/episode
// returns `episodeFile`. Reading them from the same response keeps the sync at
// one request per instance plus one per series instead of a fan-out over
// /api/v3/moviefile and /api/v3/episodefile.
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

// Episode is the subset of a Sonarr /api/v3/episode record Dubarr needs. The
// episode id is the arr_library_cache external_item_id for media_type
// "episode"; EpisodeFile is nil until Sonarr has imported the file.
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

// Episodes lists one series' episodes (GET /api/v3/episode?seriesId=N). Sonarr
// requires the seriesId filter; the whole-library episode list is deliberately
// not exposed.
func (c *SonarrClient) Episodes(ctx context.Context, seriesID int) ([]Episode, error) {
	out := []Episode{}
	err := c.get(ctx, "/api/v3/episode?seriesId="+strconv.Itoa(seriesID), &out)
	return out, err
}
