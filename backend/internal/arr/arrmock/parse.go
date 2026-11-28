// M13 identify mock: POST {base}/parse for both products.
//
// The real instances answer the same route with different payloads (Radarr:
// parsedMovieInfo/movie, Sonarr: parsedEpisodeInfo/series), so the fake builds
// the product-shaped body from one fixture. The release name the client posted
// is echoed back as the top-level title, exactly like a real instance, and
// recorded so a test can assert what was asked.
package arrmock

import (
	"encoding/json"
	"net/http"
)

// ParseResponse is the parsed verdict the fake reports. Radarr reads Title/Year/
// Quality; Sonarr reads SeriesTitle/SeriesYear/Quality. An all-zero fixture is
// an instance that could not parse anything: it answers 200 with an empty
// verdict, which is what a real instance does.
type ParseResponse struct {
	Title   string `json:"title,omitempty"`
	Year    int    `json:"year,omitempty"`
	Quality string `json:"quality,omitempty"`
	// SeriesTitle and SeriesYear feed Sonarr's parsedEpisodeInfo/series.
	SeriesTitle string `json:"series_title,omitempty"`
	SeriesYear  int    `json:"series_year,omitempty"`
}

// parseRequest is the body Dubarr sends: {"title": "<release name>"}.
type parseRequest struct {
	Title string `json:"title"`
}

// serveParse answers POST {base}/parse in the shape of the given product.
func (s *Server) serveParse(kind Kind, resp ParseResponse) http.HandlerFunc {
	return s.handle(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "method not allowed"})
			return
		}
		var req parseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid parse body"})
			return
		}
		s.mu.Lock()
		s.parses = append(s.parses, req.Title)
		s.mu.Unlock()

		quality := map[string]any{"quality": map[string]any{"name": resp.Quality}}
		if kind == Radarr {
			body := map[string]any{
				"title":   req.Title,
				"quality": quality,
			}
			if resp.Year > 0 {
				body["year"] = resp.Year
			}
			if resp.Title != "" {
				body["parsedMovieInfo"] = map[string]any{
					"movieTitles": []string{resp.Title},
					"year":        resp.Year,
				}
				body["movie"] = map[string]any{"title": resp.Title, "year": resp.Year}
			}
			writeJSON(w, http.StatusOK, body)
			return
		}
		body := map[string]any{
			"title":   req.Title,
			"quality": quality,
		}
		if resp.SeriesTitle != "" {
			body["parsedEpisodeInfo"] = map[string]any{
				"seriesTitle": resp.SeriesTitle,
				"year":        resp.SeriesYear,
			}
			body["series"] = map[string]any{"title": resp.SeriesTitle, "year": resp.SeriesYear}
		}
		writeJSON(w, http.StatusOK, body)
	})
}

// ParseRequests returns a copy of the release names posted to {base}/parse,
// oldest first.
func (s *Server) ParseRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.parses...)
}
