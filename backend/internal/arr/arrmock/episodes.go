// M6-1 mock payloads: Radarr movieFile and Sonarr episodes / episodeFile, plus
// the {base}/episode route. Kept beside arrmock.go so the base fake stays
// readable; both files build the same package.
package arrmock

import (
	"net/http"
	"strconv"
	"strings"
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

// Episode is a Sonarr /api/v5/episode element.
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

// serveEpisodes answers GET {base}/episode?seriesId=N. Sonarr requires the
// filter, so a missing/blank seriesId is a 400 like the real instance.
//
// The file is embedded only when the caller asks for it with
// includeSubresources containing "episodeFile" (case-insensitive, comma
// separated or repeated) — exactly like Sonarr v5, which otherwise answers
// hasFile:true with no episodeFile at all. That omission is the whole point:
// a fake that always embeds the file cannot see the client bug it hides.
func (s *Server) serveEpisodes(episodes []Episode) http.HandlerFunc {
	return s.handle(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		raw := query.Get("seriesId")
		if raw == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "seriesId is required"})
			return
		}
		seriesID, err := strconv.Atoi(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "seriesId must be an integer"})
			return
		}
		withFile := asksForEpisodeFile(query)
		out := make([]Episode, 0, len(episodes))
		for _, ep := range episodes {
			if ep.SeriesID != seriesID {
				continue
			}
			if !withFile {
				ep.EpisodeFile = nil
			}
			out = append(out, ep)
		}
		writeJSON(w, http.StatusOK, out)
	})
}

// asksForEpisodeFile reports whether the request opted into the episodeFile
// sub-resource. The parameter is repeatable and comma separated in Sonarr v5
// (a []EpisodeSubresource binding), so both spellings are honoured.
func asksForEpisodeFile(query map[string][]string) bool {
	for _, raw := range query["includeSubresources"] {
		for _, part := range strings.Split(raw, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "episodeFile") {
				return true
			}
		}
	}
	return false
}
