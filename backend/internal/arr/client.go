package arr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
}

// Radarr is the Radarr-specific client surface.
type Radarr interface {
	Client
	// Movies lists the Radarr library.
	Movies(ctx context.Context) ([]Movie, error)
}

// Sonarr is the Sonarr-specific client surface.
type Sonarr interface {
	Client
	// Series lists the Sonarr library.
	Series(ctx context.Context) ([]Series, error)
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
