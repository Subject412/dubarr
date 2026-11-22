// M6-1 mock payloads: Radarr movieFile and Sonarr episodes / episodeFile, plus
// the /api/v3/episode route. Kept beside arrmock.go so the base fake stays
// readable; both files build the same package.
package arrmock

import (
	"net/http"
	"strconv"
)

// MovieFile is a Radarr movieFile element (nested in Movie).
type MovieFile struct {
	ID           int    `json:"id"`
	MovieID      int    `json:"movieId"`
	RelativePath string `json:"relativePath"`
	Path         string `json:"path"`
	Size         int64  `json:"size"`
}

// EpisodeFile is a Sonarr episodeFile element (nested in Episode).
type EpisodeFile struct {
	ID           int    `json:"id"`
	SeriesID     int    `json:"seriesId"`
	SeasonNumber int    `json:"seasonNumber"`
	RelativePath string `json:"relativePath"`
	Path         string `json:"path"`
	Size         int64  `json:"size"`
}

// Episode is a Sonarr /api/v3/episode element.
type Episode struct {
	ID            int          `json:"id"`
	SeriesID      int          `json:"seriesId"`
	SeasonNumber  int          `json:"seasonNumber"`
	EpisodeNumber int          `json:"episodeNumber"`
	Title         string       `json:"title"`
	AirDateUTC    string       `json:"airDateUtc"`
	Monitored     bool         `json:"monitored"`
	HasFile       bool         `json:"hasFile"`
	EpisodeFile   *EpisodeFile `json:"episodeFile,omitempty"`
}

// serveEpisodes answers GET /api/v3/episode?seriesId=N. Sonarr requires the
// filter, so a missing/blank seriesId is a 400 like the real instance.
func (s *Server) serveEpisodes(episodes []Episode) http.HandlerFunc {
	return s.handle(func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.Query().Get("seriesId")
		if raw == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "seriesId is required"})
			return
		}
		seriesID, err := strconv.Atoi(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "seriesId must be an integer"})
			return
		}
		out := make([]Episode, 0, len(episodes))
		for _, ep := range episodes {
			if ep.SeriesID == seriesID {
				out = append(out, ep)
			}
		}
		writeJSON(w, http.StatusOK, out)
	})
}
