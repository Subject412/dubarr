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

// API bases. Dubarr speaks only the newest API each product ships — Sonarr 5,
// Radarr 3 — and never probes for an older one or falls back to it (owner
// decision 2026-11-28). The old prefix is not "still supported": Sonarr's v3
// episode list answers hasFile:true with no episodeFile unless the caller asks
// for the sub-resource, which is the bug that left every episode without a
// target path.
const (
	apiBaseSonarr = "/api/v5"
	apiBaseRadarr = "/api/v3"
)

// pinnedRelease is the dubarr release that pinned the API versions above, and
// the only place that release is named. It is prose for the operator reading
// the API-version error, not a build version: the binary's own version lives in
// cmd/dubarr/main.go behind -ldflags and is not importable from internal/arr.
// Nothing enforces it, so bump it in the same commit that changes the accepted
// versions — otherwise the error keeps naming a release that never required the
// version the code now asks for.
const pinnedRelease = "0.6.0"

// ErrKindMismatch means the instance reported an appName the client does not
// serve, for example a Radarr client configured with a Sonarr URL.
var ErrKindMismatch = errors.New("arr: instance app kind mismatch")

// Client is the read surface Dubarr needs from any *arr instance
// (ADR-0007). Credentials are supplied per client; this package stores none.
//
// Every route below is requested under the product's own API base:
// /api/v5 for Sonarr, /api/v3 for Radarr.
type Client interface {
	// Kind identifies the application this client talks to.
	Kind() Kind
	// Ping calls {apiBase}/system/status as a connection test. A 404 there is
	// not a transient error: the instance does not serve the API version
	// dubarr requires, and Ping reports that as an *APIVersionError naming
	// the product, the expected version and the path tried.
	Ping(ctx context.Context) error
	// QualityProfiles lists configured quality profiles.
	QualityProfiles(ctx context.Context) ([]QualityProfile, error)
	// RootFolders lists configured media roots.
	RootFolders(ctx context.Context) ([]RootFolder, error)
	// Tags lists configured tags (GET {apiBase}/tag). Both products expose the
	// route under their own base with the same payload.
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
	// SeriesByID reads one series (GET /api/v5/series/{id}). It is the read
	// half of a per-item rescan for an episode row.
	SeriesByID(ctx context.Context, seriesID int) (Series, error)
	// RescanSeries asks Sonarr to rescan one series (command RescanSeries).
	RescanSeries(ctx context.Context, seriesID int) error
	// Parse asks Sonarr's own parser to identify one release name
	// (POST /api/v5/parse), normalised onto ParseResult.
	Parse(ctx context.Context, title string) (ParseResult, error)
}

// core performs authenticated JSON GETs against one base URL.
type core struct {
	kind    Kind
	baseURL string
	// apiBase is the product's API prefix, set once by the constructor that
	// knows the product: "/api/v5" for Sonarr, "/api/v3" for Radarr. No
	// request path may hardcode a version; that is how the two products are
	// kept apart without a probe.
	apiBase string
	apiKey  string
	http    *http.Client
}

// RadarrClient is an HTTP client for the Radarr REST v3 API.
type RadarrClient struct{ core }

// SonarrClient is an HTTP client for the Sonarr REST v5 API.
type SonarrClient struct{ core }

var (
	_ Radarr = (*RadarrClient)(nil)
	_ Sonarr = (*SonarrClient)(nil)
)

// NewRadarrClient builds a Radarr client. baseURL is the instance root (for
// example http://radarr:7878); apiKey is sent as X-Api-Key. Radarr ships only
// v3, so every request goes to /api/v3 — the newest API Radarr has. A
// non-positive timeout falls back to DefaultTimeout. No credentials are read
// from the environment or persisted here.
func NewRadarrClient(baseURL, apiKey string, timeout time.Duration) (*RadarrClient, error) {
	c, err := newCore(KindRadarr, baseURL, apiKey, timeout)
	if err != nil {
		return nil, err
	}
	// The constructor is the only place that knows which product this is, so
	// it is the only place that picks the prefix.
	c.apiBase = apiBaseRadarr
	return &RadarrClient{core: c}, nil
}

// NewSonarrClient builds a Sonarr client with the same semantics as
// NewRadarrClient, pinned to Sonarr's v5 API. v3 is not a fallback: it answers
// the episode list without the file sub-resource, so dubarr never asks it.
func NewSonarrClient(baseURL, apiKey string, timeout time.Duration) (*SonarrClient, error) {
	c, err := newCore(KindSonarr, baseURL, apiKey, timeout)
	if err != nil {
		return nil, err
	}
	c.apiBase = apiBaseSonarr
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
// client pointed at Sonarr (or vice versa) fails instead of pinging OK, and it
// treats a 404 on the API base as "this instance does not serve the version
// dubarr requires" — a loud *APIVersionError, never a reason to try an older
// prefix.
func (c *core) Ping(ctx context.Context) error {
	var status struct {
		AppName string `json:"appName"`
		Version string `json:"version"`
	}
	if err := c.get(ctx, c.apiBase+"/system/status", &status); err != nil {
		return c.apiVersionError(err)
	}
	if !strings.EqualFold(status.AppName, string(c.kind)) {
		return &KindMismatchError{Expected: c.kind, Got: status.AppName}
	}
	return nil
}

// apiVersionError maps a 404 from a route that always exists on a working
// instance — the system status, the library lists, tags, profiles, root folders
// and commands — onto the loud "this instance does not serve /api/v5" failure.
// A by-id read keeps its plain StatusError, because a missing movie or series is
// a real 404 and says nothing about the API version.
//
// This is a classification, not a probe: nothing is retried and no prefix is
// remembered. It is what turns the first failing call of a poll pass into one
// specific line instead of one 404 per series.
func (c *core) apiVersionError(err error) error {
	var se *StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		return &APIVersionError{Kind: c.kind, APIBase: c.apiBase, Code: se.Code, Err: se}
	}
	return err
}

// QualityProfiles implements Client.
func (c *core) QualityProfiles(ctx context.Context) ([]QualityProfile, error) {
	out := []QualityProfile{}
	err := c.apiVersionError(c.get(ctx, c.apiBase+"/qualityprofile", &out))
	return out, err
}

// RootFolders implements Client.
func (c *core) RootFolders(ctx context.Context) ([]RootFolder, error) {
	out := []RootFolder{}
	err := c.apiVersionError(c.get(ctx, c.apiBase+"/rootfolder", &out))
	return out, err
}

// Tags implements Client. Radarr and Sonarr both serve GET {apiBase}/tag with
// the same payload, so one implementation serves both.
func (c *core) Tags(ctx context.Context) ([]Tag, error) {
	out := []Tag{}
	err := c.apiVersionError(c.get(ctx, c.apiBase+"/tag", &out))
	return out, err
}

// Movies implements Radarr.
func (c *RadarrClient) Movies(ctx context.Context) ([]Movie, error) {
	out := []Movie{}
	err := c.apiVersionError(c.get(ctx, c.apiBase+"/movie", &out))
	return out, err
}

// Series implements Sonarr.
func (c *SonarrClient) Series(ctx context.Context) ([]Series, error) {
	out := []Series{}
	err := c.apiVersionError(c.get(ctx, c.apiBase+"/series", &out))
	return out, err
}

// MovieByID implements Radarr. A missing movie is a 404 from arr, which the
// shared error model reports as a StatusError.
func (c *RadarrClient) MovieByID(ctx context.Context, movieID int) (Movie, error) {
	var out Movie
	err := c.get(ctx, c.apiBase+"/movie/"+strconv.Itoa(movieID), &out)
	return out, err
}

// SeriesByID implements Sonarr.
func (c *SonarrClient) SeriesByID(ctx context.Context, seriesID int) (Series, error) {
	var out Series
	err := c.get(ctx, c.apiBase+"/series/"+strconv.Itoa(seriesID), &out)
	return out, err
}

// RescanMovie implements Radarr. It posts the RescanMovie command; Radarr's
// DownloadedMoviesScan command is path-based and takes no movieId.
func (c *RadarrClient) RescanMovie(ctx context.Context, movieID int) error {
	body := struct {
		Name    string `json:"name"`
		MovieID int    `json:"movieId"`
	}{Name: "RescanMovie", MovieID: movieID}
	return c.apiVersionError(c.post(ctx, c.apiBase+"/command", body))
}

// RescanSeries implements Sonarr.
func (c *SonarrClient) RescanSeries(ctx context.Context, seriesID int) error {
	body := struct {
		Name     string `json:"name"`
		SeriesID int    `json:"seriesId"`
	}{Name: "RescanSeries", SeriesID: seriesID}
	return c.apiVersionError(c.post(ctx, c.apiBase+"/command", body))
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
		return &AuthError{Code: resp.StatusCode, Method: http.MethodGet, Path: path}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return &StatusError{Code: resp.StatusCode, Method: http.MethodGet, Path: path}
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%w: GET %s: %w", ErrDecode, path, err)
	}
	return nil
}

// post performs one authenticated JSON POST. A 2xx response is success; the
// body is discarded. Errors are scrubbed of the API key and response body.
func (c *core) post(ctx context.Context, path string, payload any) error {
	return c.postJSON(ctx, path, payload, nil)
}

// postJSON is post plus an optional 2xx response body decoded into out (nil
// discards it). Both products answer {apiBase}/parse with a payload, so the
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
		return &AuthError{Code: resp.StatusCode, Method: http.MethodPost, Path: path}
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
