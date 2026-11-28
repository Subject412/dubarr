// Package arrmock provides in-process fake Radarr and Sonarr HTTP servers for
// tests. It reproduces the subset of the *arr REST contract Dubarr reads
// (ADR-0007): system status, library, quality profiles and root folders.
//
// Each fake serves one pinned API prefix per product — /api/v5 for Sonarr,
// /api/v3 for Radarr — and answers 404 on the other one. That strictness is
// deliberate and is not a claim about the real servers (a real Sonarr 5 still
// serves /api/v3): dubarr pins one base path per product, and a fake that
// answered both versions would hide exactly the wrong-prefix bug the pin
// exists to catch. The Sonarr episode route embeds `episodeFile` only when the
// caller asks for it with includeSubresources=EpisodeFile, which is what the
// real instance does.
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
	"strconv"
	"sync"
	"time"
)

// Kind selects which fake application is served.
type Kind string

const (
	Radarr Kind = "radarr"
	Sonarr Kind = "sonarr"
)

// API bases. Each product serves exactly one: Sonarr its newest v5, Radarr its
// only v3.
const (
	apiV5 = "/api/v5"
	apiV3 = "/api/v3"
)

// APIBase is the single API prefix dubarr pins for the product — the newest
// one — and therefore the only one its fake answers.
func APIBase(kind Kind) string {
	if kind == Sonarr {
		return apiV5
	}
	return apiV3
}

// DefaultAPIKey is used when New is called with an empty key.
const DefaultAPIKey = "mock-api-key"

// Language mirrors the *arr nested language object.
type Language struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Movie is a Radarr /api/v3/movie element (v3 is Radarr's only API).
type Movie struct {
	ID               int        `json:"id"`
	Title            string     `json:"title"`
	OriginalTitle    string     `json:"originalTitle,omitempty"`
	Year             int        `json:"year"`
	TmdbID           int        `json:"tmdbId"`
	Path             string     `json:"path"`
	FolderName       string     `json:"folderName"`
	QualityProfileID int        `json:"qualityProfileId"`
	Monitored        bool       `json:"monitored"`
	HasFile          bool       `json:"hasFile"`
	OriginalLanguage Language   `json:"originalLanguage"`
	Tags             []int      `json:"tags"`
	MovieFile        *MovieFile `json:"movieFile,omitempty"`
}

// Series is a Sonarr /api/v5/series element.
type Series struct {
	ID               int      `json:"id"`
	Title            string   `json:"title"`
	OriginalTitle    string   `json:"originalTitle,omitempty"`
	Year             int      `json:"year"`
	TvdbID           int      `json:"tvdbId"`
	Path             string   `json:"path"`
	FolderName       string   `json:"folderName"`
	QualityProfileID int      `json:"qualityProfileId"`
	Monitored        bool     `json:"monitored"`
	OriginalLanguage Language `json:"originalLanguage"`
	Tags             []int    `json:"tags"`
}

// QualityProfile is a {base}/qualityprofile element.
type QualityProfile struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// RootFolder is a {base}/rootfolder element.
type RootFolder struct {
	ID         int    `json:"id"`
	Path       string `json:"path"`
	Accessible bool   `json:"accessible"`
	FreeSpace  int64  `json:"freeSpace"`
}

// Tag is a {base}/tag element. Both products serve the same shape.
type Tag struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
}

// Fixtures is the content the fake serves. Empty slices produce empty arrays,
// exactly like a configured-but-empty instance.
type Fixtures struct {
	Movies          []Movie
	Series          []Series
	Episodes        []Episode
	QualityProfiles []QualityProfile
	RootFolders     []RootFolder
	Tags            []Tag
	// Parse is the verdict GET {base}/parse?title=... answers with. A zero
	// value is an instance that parsed nothing.
	Parse ParseResponse
}

// RecordedRequest is one request observed by the fake.
type RecordedRequest struct {
	Method string
	Path   string
	// Query is the raw query string without its leading "?", so a test can
	// assert the exact parameters a client sent (for example
	// includeSubresources=episodeFile).
	Query  string
	APIKey string
}

// URL is the recorded path with its query, i.e. what the client requested
// below the instance root.
func (r RecordedRequest) URL() string {
	if r.Query == "" {
		return r.Path
	}
	return r.Path + "?" + r.Query
}

// CommandRequest is one decoded POST {base}/command body. Only the fields
// Dubarr sends are modelled; a real instance accepts more.
type CommandRequest struct {
	Name     string `json:"name"`
	MovieID  int    `json:"movieId"`
	SeriesID int    `json:"seriesId"`
}

// Server is a running fake *arr instance.
type Server struct {
	*httptest.Server

	apiKey string

	mu         sync.Mutex
	failStatus int
	delay      time.Duration
	requests   []RecordedRequest
	commands   []CommandRequest
	parses     []string
}

// New starts a fake of the given kind. An empty apiKey falls back to
// DefaultAPIKey.
func New(kind Kind, apiKey string, fx Fixtures) *Server {
	if apiKey == "" {
		apiKey = DefaultAPIKey
	}
	s := &Server{apiKey: apiKey}

	// Real instances report the product name in canonical casing.
	appName := "Radarr"
	if kind == Sonarr {
		appName = "Sonarr"
	}
	base := APIBase(kind)

	mux := http.NewServeMux()
	mux.HandleFunc(base+"/system/status", s.handle(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"appName": appName,
			"version": "5.0.0",
		})
	}))
	mux.HandleFunc(base+"/qualityprofile", s.handle(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, nonNil(fx.QualityProfiles))
	}))
	mux.HandleFunc(base+"/rootfolder", s.handle(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, nonNil(fx.RootFolders))
	}))
	// {base}/tag exists on both products with an identical payload.
	mux.HandleFunc(base+"/tag", s.handle(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, nonNil(fx.Tags))
	}))
	// {base}/parse exists on both products but answers a product-shaped body.
	mux.HandleFunc(base+"/parse", s.serveParse(kind, fx.Parse))
	// Commands are shared by both products; the fake records the decoded body
	// so tests can assert the exact command name and id.
	mux.HandleFunc(base+"/command", s.handle(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "method not allowed"})
			return
		}
		var cmd CommandRequest
		if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid command body"})
			return
		}
		s.mu.Lock()
		s.commands = append(s.commands, cmd)
		s.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": 1, "name": cmd.Name, "status": "queued",
		})
	}))
	switch kind {
	case Radarr:
		mux.HandleFunc(base+"/movie", s.handle(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, nonNil(fx.Movies))
		}))
		mux.HandleFunc(base+"/movie/{id}", s.handle(serveByID("movie", fx.Movies, func(m Movie) int { return m.ID })))
	case Sonarr:
		mux.HandleFunc(base+"/series", s.handle(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, nonNil(fx.Series))
		}))
		mux.HandleFunc(base+"/series/{id}", s.handle(serveByID("series", fx.Series, func(sv Series) int { return sv.ID })))
		mux.HandleFunc(base+"/episode", s.serveEpisodes(fx.Episodes))
	}

	// The prefix this product does not serve is rejected on purpose. A real
	// Sonarr 5 answers /api/v3 as well, so this 404 is the fake being stricter
	// than the server: dubarr pins one base path per product, and a fake that
	// answered both versions would hide exactly the wrong-prefix bug that pin
	// exists to catch. These routes record the request, which lets a test prove
	// a client never asked the wrong prefix.
	for _, other := range []string{apiV3, apiV5} {
		if other == base {
			continue
		}
		mux.HandleFunc(other+"/", s.rejectAPIVersion(other, base))
	}
	mux.HandleFunc("/", s.handle(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "not found"})
	}))

	s.Server = httptest.NewServer(mux)
	return s
}

// rejectAPIVersion answers every request under the API prefix this product does
// not serve.
func (s *Server) rejectAPIVersion(other, served string) http.HandlerFunc {
	return s.handle(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": "this instance serves " + served + ", not " + other,
		})
	})
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

// Commands returns a copy of the decoded command bodies seen so far, oldest
// first.
func (s *Server) Commands() []CommandRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]CommandRequest(nil), s.commands...)
}

func (s *Server) handle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, RecordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
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

// serveByID answers GET {base}/movie/{id} or {base}/series/{id} from the
// fixture slice: the matching element, or a 404 like a real instance.
func serveByID[T any](kind string, items []T, id func(T) int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "method not allowed"})
			return
		}
		wanted, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": kind + " id must be an integer"})
			return
		}
		for _, item := range items {
			if id(item) == wanted {
				writeJSON(w, http.StatusOK, item)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"message": kind + " not found"})
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
