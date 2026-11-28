// arr {apiBase}/parse: the instance's own release-name parser.
//
// The approval queue identifies an unmatched release with the arr instance that
// will hold its target (ADR-0007: mock-first, no live instance in tests). Radarr
// and Sonarr answer the same route under their own API base with different
// payloads, so the client normalises both into one kind-agnostic ParseResult
// and keeps the original body as Raw for diagnostics.
package arr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// parsePath is the parse route relative to the client's API base ("/api/v5"
// for Sonarr, "/api/v3" for Radarr).
const parsePath = "/parse"

// ParseResult is an instance's parse verdict, normalised across Radarr and
// Sonarr. Title is the media title the parser recognised (the movie title for
// Radarr, the series title for Sonarr); it is empty when the parser matched
// nothing. Year is 0 when the release name carried none. Quality is arr's
// quality name ("Bluray-1080p"), also 0-value when unparsed.
//
// Confidence is a deterministic hint derived from what was parsed (see
// parseConfidence); it is never random and never an arr-reported score.
//
// Parsed states explicitly whether the instance recognised a media title at
// all. It is false when Title is empty, which is what an instance that could
// not parse the release name answers (200 with an empty verdict). Callers must
// read it instead of inferring "nothing matched" from an empty Title, and must
// never substitute a title of their own: there is no fake title here.
type ParseResult struct {
	Title      string
	Year       int
	Quality    string
	Confidence float64
	Parsed     bool
	// Raw is the untouched response body, so a diagnostic can show exactly
	// what arr thought without a second request.
	Raw json.RawMessage
}

// parseWire is the union of the fields Dubarr reads from either product's parse
// payload. Radarr reports parsedMovieInfo/movie, Sonarr parsedEpisodeInfo/series;
// the normaliser picks the ones its kind populates.
type parseWire struct {
	Title   string `json:"title"`
	Year    int    `json:"year"`
	Quality struct {
		Quality struct {
			Name string `json:"name"`
		} `json:"quality"`
	} `json:"quality"`
	ParsedMovieInfo *struct {
		MovieTitles []string `json:"movieTitles"`
		Year        int      `json:"year"`
	} `json:"parsedMovieInfo"`
	ParsedEpisodeInfo *struct {
		SeriesTitle string `json:"seriesTitle"`
		Year        int    `json:"year"`
	} `json:"parsedEpisodeInfo"`
	Movie *struct {
		Title string `json:"title"`
		Year  int    `json:"year"`
	} `json:"movie"`
	Series *struct {
		Title string `json:"title"`
		Year  int    `json:"year"`
	} `json:"series"`
}

// Parse asks the instance to parse one release name — GET {apiBase}/parse with
// the name as the title query parameter. Both products expose the route as
// [HttpGet] Parse(string title) and answer 405 to a POST, so the verb is not a
// choice. The title is escaped as one query value, so a name carrying "&", "#"
// or "+" reaches arr intact.
//
// Errors go through the shared error model, so an operator diagnostic can
// classify them: ErrAuth, ErrNetwork or a *StatusError. The API key is sent as
// X-Api-Key and never appears in an error.
func (c *core) Parse(ctx context.Context, title string) (ParseResult, error) {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return ParseResult{}, fmt.Errorf("arr: parse: title is empty")
	}
	path := c.apiBase + parsePath + "?" + url.Values{"title": {trimmed}}.Encode()

	// Decoding into a RawMessage keeps the payload verbatim: the normalised
	// view is derived from these bytes, so Raw and the result cannot disagree.
	var raw json.RawMessage
	if err := c.get(ctx, path, &raw); err != nil {
		return ParseResult{}, err
	}
	var wire parseWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return ParseResult{}, fmt.Errorf("arr: decode GET %s response: %w", errorPath(path), err)
	}
	return normaliseParse(c.kind, wire, raw), nil
}

// normaliseParse maps either product's payload onto ParseResult.
//
// The media title comes from the product's parsed-info block, never from the
// top-level "title": a real instance echoes the release name Dubarr just sent
// there, which is not an identification. A payload the instance could not parse
// therefore normalises to an empty title (confidence 0), and the operator sees
// that instead of their own input bounced back as a match.
func normaliseParse(kind Kind, wire parseWire, raw json.RawMessage) ParseResult {
	out := ParseResult{
		Quality: strings.TrimSpace(wire.Quality.Quality.Name),
		Raw:     raw,
	}
	switch kind {
	case KindRadarr:
		if info := wire.ParsedMovieInfo; info != nil && len(info.MovieTitles) > 0 {
			out.Title = strings.TrimSpace(info.MovieTitles[0])
			out.Year = info.Year
		}
		if out.Title == "" && wire.Movie != nil {
			out.Title = strings.TrimSpace(wire.Movie.Title)
			out.Year = wire.Movie.Year
		}
	default:
		if info := wire.ParsedEpisodeInfo; info != nil && info.SeriesTitle != "" {
			out.Title = strings.TrimSpace(info.SeriesTitle)
			out.Year = info.Year
		}
		if out.Title == "" && wire.Series != nil {
			out.Title = strings.TrimSpace(wire.Series.Title)
			out.Year = wire.Series.Year
		}
	}
	out.Confidence = parseConfidence(out)
	out.Parsed = out.Title != ""
	return out
}

// parseConfidence scores what the parser actually produced: a recognised title
// is the only thing worth anything, and a year and a quality each confirm the
// parse. Deterministic by design — the operator sees the same score for the
// same payload, and no candidate is ever pre-picked on it.
func parseConfidence(res ParseResult) float64 {
	if res.Title == "" {
		return 0
	}
	score := 0.5
	if res.Year > 0 {
		score += 0.3
	}
	if res.Quality != "" {
		score += 0.2
	}
	return score
}
