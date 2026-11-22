// Package arrmock provides in-process fake Radarr and Sonarr HTTP servers for
// tests. It reproduces the subset of the *arr REST v3 contract Dubarr reads
// (ADR-0007): system status, library, quality profiles and root folders.
//
// The fakes enforce the X-Api-Key header so tests exercise the real client's
// auth-failure path, and can be told to fail every request with a chosen HTTP
// status to exercise the unexpected-status path. No live instance or key is
// ever involved.
package arrmock

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// Kind selects which fake application is served.
type Kind string

const (
	Radarr Kind = "radarr"
	Sonarr Kind = "sonarr"
)

// DefaultAPIKey is used when New is called with an empty key.
const DefaultAPIKey = "mock-api-key"

// Language mirrors the *arr nested language object.
type Language struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Movie is a Radarr /api/v3/movie element.
type Movie struct {
	ID               int      `json:"id"`
	Title            string   `json:"title"`
	Year             int      `json:"year"`
	TmdbID           int      `json:"tmdbId"`
	Path             string   `json:"path"`
	FolderName       string   `json:"folderName"`
	QualityProfileID int      `json:"qualityProfileId"`
	Monitored        bool     `json:"monitored"`
	HasFile          bool     `json:"hasFile"`
	OriginalLanguage Language `json:"originalLanguage"`
}

// Series is a Sonarr /api/v3/series element.
type Series struct {
	ID               int    `json:"id"`
	Title            string `json:"title"`
	Year             int    `json:"year"`
	TvdbID           int    `json:"tvdbId"`
	Path             string `json:"path"`
	FolderName       string `json:"folderName"`
	QualityProfileID int    `json:"qualityProfileId"`
	Monitored        bool   `json:"monitored"`
}

// QualityProfile is an /api/v3/qualityprofile element.
type QualityProfile struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// RootFolder is an /api/v3/rootfolder element.
type RootFolder struct {
	ID         int    `json:"id"`
	Path       string `json:"path"`
	Accessible bool   `json:"accessible"`
	FreeSpace  int64  `json:"freeSpace"`
}

// Fixtures is the content the fake serves. Empty slices produce empty arrays,
// exactly like a configured-but-empty instance.
type Fixtures struct {
	Movies          []Movie
	Series          []Series
	QualityProfiles []QualityProfile
	RootFolders     []RootFolder
}

// RecordedRequest is one request observed by the fake.
type RecordedRequest struct {
	Method string
	Path   string
	APIKey string
}

// Server is a running fake *arr instance.
type Server struct {
	*httptest.Server

	apiKey string

	mu         sync.Mutex
	failStatus int
	delay      time.Duration
	requests   []RecordedRequest
}

// New starts a fake of the given kind. An empty apiKey falls back to
// DefaultAPIKey.
func New(kind Kind, apiKey string, fx Fixtures) *Server {
	if apiKey == "" {
		apiKey = DefaultAPIKey
	}
	s := &Server{apiKey: apiKey}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/system/status", s.handle(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"appName": string(kind),
			"version": "5.0.0",
		})
	}))
	mux.HandleFunc("/api/v3/qualityprofile", s.handle(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, nonNil(fx.QualityProfiles))
	}))
	mux.HandleFunc("/api/v3/rootfolder", s.handle(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, nonNil(fx.RootFolders))
	}))
	switch kind {
	case Radarr:
		mux.HandleFunc("/api/v3/movie", s.handle(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, nonNil(fx.Movies))
		}))
	case Sonarr:
		mux.HandleFunc("/api/v3/series", s.handle(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, nonNil(fx.Series))
		}))
	}

	s.Server = httptest.NewServer(mux)
	return s
}

// APIKey is the key clients must send in X-Api-Key.
func (s *Server) APIKey() string { return s.apiKey }

// FailAll makes every subsequent request return the given HTTP status.
// Passing 0 restores normal behaviour.
func (s *Server) FailAll(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failStatus = status
}

// SetDelay makes every subsequent request sleep before responding, so tests
// can exercise client timeouts.
func (s *Server) SetDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = d
}

// Requests returns a copy of the requests seen so far, oldest first.
func (s *Server) Requests() []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RecordedRequest(nil), s.requests...)
}

func (s *Server) handle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, RecordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			APIKey: r.Header.Get("X-Api-Key"),
		})
		status, delay := s.failStatus, s.delay
		s.mu.Unlock()

		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		if status != 0 {
			writeJSON(w, status, map[string]string{"message": "mock failure"})
			return
		}
		if r.Header.Get("X-Api-Key") != s.apiKey {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Unauthorized"})
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func nonNil[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}
