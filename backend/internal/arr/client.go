package arr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds every request when a constructor is given a
// non-positive timeout.
const DefaultTimeout = 30 * time.Second

// ErrKindMismatch means the instance reported an appName the client does not
// serve, for example a Radarr client configured with a Sonarr URL.
var ErrKindMismatch = errors.New("arr: instance app kind mismatch")

// Client is the read surface Dubarr needs from any *arr instance
// (ADR-0007). Credentials are supplied per client; this package stores none.
type Client interface {
	// Kind identifies the application this client talks to.
	Kind() Kind
	// Ping calls /api/v3/system/status as a connection test.
	Ping(ctx context.Context) error
	// QualityProfiles lists configured quality profiles.
	QualityProfiles(ctx context.Context) ([]QualityProfile, error)
	// RootFolders lists configured media roots.
	RootFolders(ctx context.Context) ([]RootFolder, error)
	// Tags lists configured tags (GET /api/v3/tag). Both products expose the
	// same endpoint and payload.
	Tags(ctx context.Context) ([]Tag, error)
}

// Radarr is the Radarr-specific client surface.
type Radarr interface {
	Client
	// Movies lists the Radarr library.
	Movies(ctx context.Context) ([]Movie, error)
	// MovieByID reads one movie (GET /api/v3/movie/{id}). It is the read
	// half of a per-item rescan: the command is accepted asynchronously, so
	// the caller re-reads the item to see what arr now holds.
	MovieByID(ctx context.Context, movieID int) (Movie, error)
	// RescanMovie asks Radarr to rescan one movie's files (command
	// RescanMovie). It is the movieId-scoped rescan: DownloadedMoviesScan
	// only accepts path/downloadClientId/importMode and cannot target a
	// single movie.
	RescanMovie(ctx context.Context, movieID int) error
	// Parse asks Radarr's own parser to identify one release name
	// (POST /api/v3/parse), normalised onto ParseResult.
	Parse(ctx context.Context, title string) (ParseResult, error)
}

// Sonarr is the Sonarr-specific client surface.
type Sonarr interface {
	Client
	// Series lists the Sonarr library.
	Series(ctx context.Context) ([]Series, error)
	// SeriesByID reads one series (GET /api/v3/series/{id}). It is the read
	// half of a per-item rescan for an episode row.
	SeriesByID(ctx context.Context, seriesID int) (Series, error)
	// RescanSeries asks Sonarr to rescan one series (command RescanSeries).
	RescanSeries(ctx context.Context, seriesID int) error
	// Parse asks Sonarr's own parser to identify one release name
	// (POST /api/v3/parse), normalised onto ParseResult.
	Parse(ctx context.Context, title string) (ParseResult, error)
}

// core performs authenticated JSON GETs against one base URL.
type core struct {
	kind    Kind
	baseURL string
	apiKey  string
	http    *http.Client
}

// RadarrClient is an HTTP client for the Radarr REST v3 API.
type RadarrClient struct{ core }

// SonarrClient is an HTTP client for the Sonarr REST v3 API.
type SonarrClient struct{ core }

var (
	_ Radarr = (*RadarrClient)(nil)
	_ Sonarr = (*SonarrClient)(nil)
)

// NewRadarrClient builds a Radarr client. baseURL is the instance root (for
// example http://radarr:7878); apiKey is sent as X-Api-Key. A non-positive
// timeout falls back to DefaultTimeout. No credentials are read from the
// environment or persisted here.
func NewRadarrClient(baseURL, apiKey string, timeout time.Duration) (*RadarrClient, error) {
	c, err := newCore(KindRadarr, baseURL, apiKey, timeout)
	if err != nil {
		return nil, err
	}
	return &RadarrClient{core: c}, nil
}

// NewSonarrClient builds a Sonarr client with the same semantics as
// NewRadarrClient.
func NewSonarrClient(baseURL, apiKey string, timeout time.Duration) (*SonarrClient, error) {
	c, err := newCore(KindSonarr, baseURL, apiKey, timeout)
	if err != nil {
		return nil, err
	}
	return &SonarrClient{core: c}, nil
}

func newCore(kind Kind, baseURL, apiKey string, timeout time.Duration) (core, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return core{}, fmt.Errorf("arr: invalid base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return core{}, fmt.Errorf("arr: invalid base URL %q: scheme must be http or https", baseURL)
	}
	if u.Host == "" {
		return core{}, fmt.Errorf("arr: invalid base URL %q: missing host", baseURL)
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return core{
		kind:    kind,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: timeout},
	}, nil
}

// Kind implements Client.
func (c *core) Kind() Kind { return c.kind }

// KindMismatchError reports a client pointed at the wrong *arr application:
// the instance answered with an appName the client does not serve. It carries
// only kind names, never credentials.
type KindMismatchError struct {
	Expected Kind
	Got      string
}

// Error implements error.
func (e *KindMismatchError) Error() string {
	return fmt.Sprintf("%s: expected %q, got %q", ErrKindMismatch, e.Expected, e.Got)
}

// Is lets errors.Is(err, ErrKindMismatch) match.
func (e *KindMismatchError) Is(target error) bool { return target == ErrKindMismatch }

// Ping implements Client. It validates the instance's appName so a Radarr
// client pointed at Sonarr (or vice versa) fails instead of pinging OK.
func (c *core) Ping(ctx context.Context) error {
	var status struct {
		AppName string `json:"appName"`
		Version string `json:"version"`
	}
	if err := c.get(ctx, "/api/v3/system/status", &status); err != nil {
		return err
	}
	if !strings.EqualFold(status.AppName, string(c.kind)) {
		return &KindMismatchError{Expected: c.kind, Got: status.AppName}
	}
	return nil
}

// QualityProfiles implements Client.
func (c *core) QualityProfiles(ctx context.Context) ([]QualityProfile, error) {
	out := []QualityProfile{}
	err := c.get(ctx, "/api/v3/qualityprofile", &out)
	return out, err
}

// RootFolders implements Client.
func (c *core) RootFolders(ctx context.Context) ([]RootFolder, error) {
	out := []RootFolder{}
	err := c.get(ctx, "/api/v3/rootfolder", &out)
	return out, err
}

// Tags implements Client. Radarr and Sonarr share GET /api/v3/tag, so one
// implementation serves both.
func (c *core) Tags(ctx context.Context) ([]Tag, error) {
	out := []Tag{}
	err := c.get(ctx, "/api/v3/tag", &out)
	return out, err
}

// Movies implements Radarr.
func (c *RadarrClient) Movies(ctx context.Context) ([]Movie, error) {
	out := []Movie{}
	err := c.get(ctx, "/api/v3/movie", &out)
	return out, err
}

// Series implements Sonarr.
func (c *SonarrClient) Series(ctx context.Context) ([]Series, error) {
	out := []Series{}
	err := c.get(ctx, "/api/v3/series", &out)
	return out, err
}

// MovieByID implements Radarr. A missing movie is a 404 from arr, which the
// shared error model reports as a StatusError.
func (c *RadarrClient) MovieByID(ctx context.Context, movieID int) (Movie, error) {
	var out Movie
	err := c.get(ctx, "/api/v3/movie/"+strconv.Itoa(movieID), &out)
	return out, err
}

// SeriesByID implements Sonarr.
func (c *SonarrClient) SeriesByID(ctx context.Context, seriesID int) (Series, error) {
	var out Series
	err := c.get(ctx, "/api/v3/series/"+strconv.Itoa(seriesID), &out)
	return out, err
}

// RescanMovie implements Radarr. It posts the RescanMovie command; Radarr's
// DownloadedMoviesScan command is path-based and takes no movieId.
func (c *RadarrClient) RescanMovie(ctx context.Context, movieID int) error {
	body := struct {
		Name    string `json:"name"`
		MovieID int    `json:"movieId"`
	}{Name: "RescanMovie", MovieID: movieID}
	return c.post(ctx, "/api/v3/command", body)
}

// RescanSeries implements Sonarr.
func (c *SonarrClient) RescanSeries(ctx context.Context, seriesID int) error {
	body := struct {
		Name     string `json:"name"`
		SeriesID int    `json:"seriesId"`
	}{Name: "RescanSeries", SeriesID: seriesID}
	return c.post(ctx, "/api/v3/command", body)
}

// get performs one authenticated GET and decodes a 2xx JSON body into out.
// Errors are scrubbed of the API key and response body.
func (c *core) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("arr: build request %s: %w", path, err)
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: GET %s: %w", ErrNetwork, path, scrub(err, c.apiKey))
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: GET %s: HTTP %d", ErrAuth, path, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return &StatusError{Code: resp.StatusCode, Method: http.MethodGet, Path: path}
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("arr: decode GET %s response: %w", path, err)
	}
	return nil
}

// post performs one authenticated JSON POST. A 2xx response is success; the
// body is discarded. Errors are scrubbed of the API key and response body.
func (c *core) post(ctx context.Context, path string, payload any) error {
	return c.postJSON(ctx, path, payload, nil)
}

// postJSON is post plus an optional 2xx response body decoded into out (nil
// discards it). Both products answer /api/v3/parse with a payload, so the
// decode shares this one request/error path instead of copying it.
func (c *core) postJSON(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("arr: encode POST %s body: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("arr: build request %s: %w", path, err)
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: POST %s: %w", ErrNetwork, path, scrub(err, c.apiKey))
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: POST %s: HTTP %d", ErrAuth, path, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return &StatusError{Code: resp.StatusCode, Method: http.MethodPost, Path: path}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("arr: decode POST %s response: %w", path, err)
	}
	return nil
}

// scrub removes the API key from error text should a transport error echo the
// request URL or headers, while preserving the original error for errors.Is.
func scrub(err error, apiKey string) error {
	if err == nil || apiKey == "" {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), apiKey, "[REDACTED]")
	if msg == err.Error() {
		return err
	}
	return &scrubbedError{msg: msg, err: err}
}

type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }
