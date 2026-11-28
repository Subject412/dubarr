// Package library exposes the read-only Library screen API: the paged,
// filterable arr cache listing and the per-instance sync health panel (#29),
// plus the manual refresh trigger that shares the poller's single pass slot.
//
// The GET routes are admin-only and read from arr_library_cache /
// arr_instances through the store's Library read surface; health additionally
// reads the arr.connections settings section to report configured-but-never-
// synced instances, and the #56 hierarchy routes each read one level of the
// series -> seasons -> episodes tree. Only POST /api/library/refresh acts, and
// it only asks the poller to run a pass — it never writes the cache itself. POST
// /api/library/items/{id}/rescan is the one route that both commands arr and
// writes a single cache row (see rescan.go).
package library

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// Dependencies are the collaborators the library API needs.
type Dependencies struct {
	DB *sql.DB
	// Settings supplies the arr.connections section, so a connection saved but
	// not yet polled (no arr_instances row) still appears in health. When nil
	// the endpoint serves the store-derived instances only.
	Settings *settings.Store
	// Refresher starts a manual sync pass. Nil (e.g. no arr instance is
	// configured, so no poller was built) makes the refresh route answer 409
	// with code "not_configured" instead of panicking.
	Refresher Refresher
	// Secrets decrypts arr_instances.api_key_enc for the per-item rescan. Nil
	// makes that route answer 409 "not_configured" rather than rescanning
	// unauthenticated.
	Secrets Decrypter
	// RequireAdmin wraps every route. When nil the API answers 503 rather than
	// running unauthenticated (same contract as the queue API).
	RequireAdmin func(http.Handler) http.Handler
	// Logf reports the refresh decision at info (accepted or refused, with the
	// kind) so /logs?component=library answers "what did the click do". Nil
	// keeps the API silent.
	Logf func(format string, args ...any)
}

// Service holds the resolved dependencies for the registered handlers.
type Service struct {
	store    *store.Store
	settings *settings.Store
	// refMu guards refresher: cmd/dubarr publishes a poller built after boot
	// (#53) while handlers read it.
	refMu        sync.RWMutex
	refresher    Refresher
	secrets      Decrypter
	requireAdmin func(http.Handler) http.Handler
	logf         func(format string, args ...any)
}

// RegisterRoutes mounts:
//
//	GET  /api/library/items                                   paged, filtered arr_library_cache listing
//	GET  /api/library/health                                  per-instance arr sync health + cache size
//	POST /api/library/refresh                                 start one manual sync pass for one kind or all
//	POST /api/library/items/{id}/rescan                       rescan one arr item and re-read it
//	GET  /api/library/movies                                  paged Radarr movie list
//	GET  /api/library/series                                  paged Sonarr series aggregate
//	GET  /api/library/series/{id}                             one series header
//	GET  /api/library/series/{id}/seasons                     one series' seasons, aggregated
//	GET  /api/library/series/{id}/seasons/{season}/episodes   one page of one season
func RegisterRoutes(mux *http.ServeMux, deps Dependencies) *Service {
	svc := &Service{
		store:        &store.Store{DB: deps.DB},
		settings:     deps.Settings,
		refresher:    deps.Refresher,
		secrets:      deps.Secrets,
		requireAdmin: deps.RequireAdmin,
		logf:         deps.Logf,
	}
	admin := func(h http.HandlerFunc) http.Handler {
		if svc.requireAdmin == nil {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeError(w, http.StatusServiceUnavailable, "authorization is not configured")
			})
		}
		return svc.requireAdmin(h)
	}
	mux.Handle("GET /api/library/items", admin(svc.handleItems))
	mux.Handle("GET /api/library/health", admin(svc.handleHealth))
	mux.Handle("POST /api/library/refresh", admin(svc.handleRefresh))
	mux.Handle("POST /api/library/items/{id}/rescan", admin(svc.handleRescan))

	// #56 hierarchy routes: Radarr is flat, Sonarr is series -> seasons ->
	// episodes. Each level has its own read; none of them writes the mirror.
	mux.Handle("GET /api/library/movies", admin(svc.handleMovies))
	mux.Handle("GET /api/library/series", admin(svc.handleSeries))
	mux.Handle("GET /api/library/series/{id}", admin(svc.handleSeriesByID))
	mux.Handle("GET /api/library/series/{id}/seasons", admin(svc.handleSeriesSeasons))
	mux.Handle("GET /api/library/series/{id}/seasons/{season}/episodes", admin(svc.handleSeasonEpisodes))
	return svc
}

// SetRefresher publishes a poller built after boot (#53), when no poller
// existed at route registration. An already-wired refresher is never replaced,
// so a settings save cannot swap the poller under a live request. A nil
// argument is ignored.
func (s *Service) SetRefresher(r Refresher) {
	if r == nil {
		return
	}
	s.refMu.Lock()
	if s.refresher == nil {
		s.refresher = r
	}
	s.refMu.Unlock()
}

// refresherRef returns the currently wired refresher, nil when none exists.
func (s *Service) refresherRef() Refresher {
	s.refMu.RLock()
	defer s.refMu.RUnlock()
	return s.refresher
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// --- Library hierarchy (#56) -------------------------------------------------
//
// Sonarr's library is a tree, so each level gets its own read: the movie list
// and the series list are paged collections, a series header is one resource,
// the seasons of a series are an aggregate, and one season's episodes are a
// page. Every route below is a pure read of arr_library_cache — the mirror is
// written by arrsync, never by the Library screen.

// The hierarchy query parser's failures. Each maps to one documented 400 code;
// a bad page/size/value is never silently clamped into something else.
var (
	errInvalidPage     = errors.New("invalid page")
	errInvalidSort     = errors.New("invalid sort")
	errInvalidInstance = errors.New("invalid instance id")
)

// movieResource is one Radarr movie as the SPA consumes it. Field names follow
// Radarr's resource where they overlap and the legacy item shape where it
// already had a name, so the UI's types do not fork.
type movieResource struct {
	ID            int64  `json:"id"`
	ArrID         int64  `json:"arr_id"`
	InstanceID    int64  `json:"instance_id"`
	Instance      string `json:"instance"`
	Title         string `json:"title"`
	OriginalTitle string `json:"original_title"`
	Year          int    `json:"year"`
	// OriginalLanguage is the movie's language as an ISO 639-1 code;
	// AudioLanguages are the codes of the file's audio tracks in arr's
	// order. Both are never null: an absent value ships as "" / [].
	OriginalLanguage string   `json:"original_language"`
	AudioLanguages   []string `json:"audio_languages"`
	// Monitored is arr's monitored flag; Available means the mirror holds a
	// target path for the movie (the same fact has_file reports).
	Monitored      bool   `json:"monitored"`
	Available      bool   `json:"available"`
	Path           string `json:"path"`
	TargetFilePath string `json:"target_file_path"`
	HasFile        bool   `json:"has_file"`
	SizeOnDisk     int64  `json:"size_on_disk"`
	Eligibility    string `json:"eligibility"`
	VideoCodec     string `json:"video_codec"`
	UpdatedAt      string `json:"updated_at"`
}

// seriesResource is one aggregated Sonarr series. AudioLanguages are absent on
// purpose: they are per file, and a series row must not invent one.
type seriesResource struct {
	ID               int64  `json:"id"`
	InstanceID       int64  `json:"instance_id"`
	Instance         string `json:"instance"`
	Title            string `json:"title"`
	Year             int    `json:"year"`
	OriginalLanguage string `json:"original_language"`
	// Monitored is arr's monitored flag for the series; Available means at
	// least one episode of it has a file in the mirror.
	Monitored        bool   `json:"monitored"`
	Available        bool   `json:"available"`
	Path             string `json:"path"`
	SeasonCount      int    `json:"season_count"`
	EpisodeCount     int    `json:"episode_count"`
	EpisodeFileCount int    `json:"episode_file_count"`
	Eligibility      string `json:"eligibility"`
	UpdatedAt        string `json:"updated_at"`
}

// seasonResource is one season with its statistics nested, mirroring Sonarr's
// SeasonResource.
type seasonResource struct {
	SeasonNumber int              `json:"season_number"`
	Statistics   seasonStatistics `json:"statistics"`
}

// seasonStatistics is the per-season aggregate computed in SQL.
type seasonStatistics struct {
	EpisodeCount     int     `json:"episode_count"`
	EpisodeFileCount int     `json:"episode_file_count"`
	PercentEpisodes  float64 `json:"percent_of_episodes"`
}

// episodeResource is one episode of one season.
type episodeResource struct {
	ID             int64  `json:"id"`
	SeriesID       int64  `json:"series_id"`
	ArrID          int64  `json:"arr_id"`
	SeasonNumber   int    `json:"season_number"`
	EpisodeNumber  int    `json:"episode_number"`
	Title          string `json:"title"`
	HasFile        bool   `json:"has_file"`
	TargetFilePath string `json:"target_file_path"`
	Eligibility    string `json:"eligibility"`
}

// moviesResponse and seriesResponse are the paged envelopes; pageSize echoes
// the size actually applied after clamping.
type moviesResponse struct {
	Page     int             `json:"page"`
	PageSize int             `json:"pageSize"`
	Total    int64           `json:"total"`
	Items    []movieResource `json:"items"`
}

type seriesResponse struct {
	Page     int              `json:"page"`
	PageSize int              `json:"pageSize"`
	Total    int64            `json:"total"`
	Items    []seriesResource `json:"items"`
}

// seasonsResponse is a plain {items} envelope: a series has few seasons, so
// they are never paged.
type seasonsResponse struct {
	Items []seasonResource `json:"items"`
}

// seasonEpisodesResponse reports the whole season's total beside the page, so
// the UI can say "showing 500 of 2001" instead of pretending the season is
// short. Truncated is set only when the season exceeds the hard cap.
type seasonEpisodesResponse struct {
	SeasonNumber int               `json:"seasonNumber"`
	Total        int64             `json:"total"`
	Truncated    bool              `json:"truncated"`
	Items        []episodeResource `json:"items"`
}

// handleMovies serves the flat Radarr list: q / instanceId / page / pageSize /
// sort / dir, all applied on the server.
func (s *Service) handleMovies(w http.ResponseWriter, r *http.Request) {
	if !s.hierarchyStoreReady(w) {
		return
	}
	filter, err := parseHierarchyFilter(r.URL.Query())
	if err != nil {
		writeHierarchyError(w, err)
		return
	}
	total, rows, err := s.store.ListMovies(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the library")
		return
	}
	items := make([]movieResource, 0, len(rows))
	for _, row := range rows {
		items = append(items, movieResource{
			ID:               row.ID,
			ArrID:            row.ExternalItemID,
			InstanceID:       row.ArrInstanceID,
			Instance:         row.InstanceName,
			Title:            row.Title,
			OriginalTitle:    row.OriginalTitle,
			Year:             row.Year,
			OriginalLanguage: row.OriginalLanguageISO2,
			AudioLanguages:   nonNilLanguages(row.AudioLanguages),
			Monitored:        row.Monitored,
			Available:        row.Available,
			Path:             row.MediaFolderPath,
			TargetFilePath:   row.TargetFilePath,
			HasFile:          row.HasFile,
			SizeOnDisk:       row.SizeOnDisk,
			Eligibility:      row.Eligibility,
			VideoCodec:       row.VideoCodec,
			UpdatedAt:        row.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, moviesResponse{
		Page:     hierarchyPageNumber(filter.Page),
		PageSize: clampHierarchyPageSize(filter.PageSize),
		Total:    total,
		Items:    items,
	})
}

// handleSeries serves the paged series aggregate (one row per series_id).
func (s *Service) handleSeries(w http.ResponseWriter, r *http.Request) {
	if !s.hierarchyStoreReady(w) {
		return
	}
	filter, err := parseHierarchyFilter(r.URL.Query())
	if err != nil {
		writeHierarchyError(w, err)
		return
	}
	total, rows, err := s.store.ListSeries(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the library")
		return
	}
	items := make([]seriesResource, 0, len(rows))
	for _, row := range rows {
		items = append(items, seriesResourceOf(row))
	}
	writeJSON(w, http.StatusOK, seriesResponse{
		Page:     hierarchyPageNumber(filter.Page),
		PageSize: clampHierarchyPageSize(filter.PageSize),
		Total:    total,
		Items:    items,
	})
}

// handleSeriesByID serves the header above the season view.
func (s *Service) handleSeriesByID(w http.ResponseWriter, r *http.Request) {
	if !s.hierarchyStoreReady(w) {
		return
	}
	seriesID, ok := hierarchySeriesID(w, r)
	if !ok {
		return
	}
	row, found, err := s.store.SeriesByID(r.Context(), seriesID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the library")
		return
	}
	if !found {
		writeSeriesNotFound(w)
		return
	}
	writeJSON(w, http.StatusOK, seriesResourceOf(row))
}

// handleSeriesSeasons serves every season of one series, aggregated in SQL.
func (s *Service) handleSeriesSeasons(w http.ResponseWriter, r *http.Request) {
	if !s.hierarchyStoreReady(w) {
		return
	}
	seriesID, ok := hierarchySeriesID(w, r)
	if !ok {
		return
	}
	// Only an unknown series is a 404: a known series with no cached seasons
	// answers an empty list, because the series does exist.
	if _, found, err := s.store.SeriesByID(r.Context(), seriesID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the library")
		return
	} else if !found {
		writeSeriesNotFound(w)
		return
	}
	seasons, err := s.store.SeasonsForSeries(r.Context(), seriesID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the library")
		return
	}
	items := make([]seasonResource, 0, len(seasons))
	for _, row := range seasons {
		items = append(items, seasonResource{
			SeasonNumber: row.SeasonNumber,
			Statistics: seasonStatistics{
				EpisodeCount:     row.EpisodeCount,
				EpisodeFileCount: row.EpisodeFileCount,
				PercentEpisodes:  row.PercentEpisodes,
			},
		})
	}
	writeJSON(w, http.StatusOK, seasonsResponse{Items: items})
}

// handleSeasonEpisodes serves one page of one season: pageSize defaults to 200
// and is clamped to 500, and a season above store.LibrarySeasonMaxRows is
// reported truncated with its real total rather than silently shortened.
func (s *Service) handleSeasonEpisodes(w http.ResponseWriter, r *http.Request) {
	if !s.hierarchyStoreReady(w) {
		return
	}
	seriesID, ok := hierarchySeriesID(w, r)
	if !ok {
		return
	}
	season, err := strconv.Atoi(r.PathValue("season"))
	if err != nil || season < 0 {
		writeSeriesNotFound(w)
		return
	}
	if _, found, err := s.store.SeriesByID(r.Context(), seriesID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the library")
		return
	} else if !found {
		writeSeriesNotFound(w)
		return
	}
	page, pageSize, err := parseHierarchyPaging(r.URL.Query())
	if err != nil {
		writeHierarchyError(w, err)
		return
	}
	total, err := s.store.SeasonEpisodeCount(r.Context(), seriesID, season)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the library")
		return
	}
	rows, err := s.store.EpisodesForSeasonPage(r.Context(), seriesID, season, page, pageSize)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the library")
		return
	}
	items := make([]episodeResource, 0, len(rows))
	for _, row := range rows {
		items = append(items, episodeResource{
			ID:             row.ID,
			SeriesID:       row.SeriesID,
			ArrID:          row.ExternalItemID,
			SeasonNumber:   row.SeasonNumber,
			EpisodeNumber:  row.EpisodeNumber,
			Title:          row.EpisodeTitle,
			HasFile:        row.HasFile,
			TargetFilePath: row.TargetFilePath,
			Eligibility:    row.Eligibility,
		})
	}
	writeJSON(w, http.StatusOK, seasonEpisodesResponse{
		SeasonNumber: season,
		Total:        total,
		Truncated:    total > int64(store.LibrarySeasonMaxRows),
		Items:        items,
	})
}

// parseHierarchyFilter maps the movie/series query string onto the store
// filter. Absent page/pageSize mean 1 / the store default; a present value
// below 1 or a non-number is a 400, never a silent clamp, because a typo must
// not quietly widen or narrow what the operator asked for.
func parseHierarchyFilter(q url.Values) (store.HierarchyFilter, error) {
	filter := store.HierarchyFilter{Q: q.Get("q"), Sort: q.Get("sort"), Dir: q.Get("dir")}
	page, pageSize, err := parseHierarchyPaging(q)
	if err != nil {
		return filter, err
	}
	filter.Page, filter.PageSize = page, pageSize
	if raw := q.Get("instanceId"); raw != "" {
		instanceID, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || instanceID < 1 {
			return filter, errInvalidInstance
		}
		filter.InstanceID = instanceID
	}
	if err := filter.ValidateOrder(); err != nil {
		return filter, errInvalidSort
	}
	return filter, nil
}

// parseHierarchyPaging reads page/pageSize. A pageSize of 0 is treated like
// the store's default only when the parameter is absent.
func parseHierarchyPaging(q url.Values) (page, pageSize int, err error) {
	page = 1
	if raw := q.Get("page"); raw != "" {
		value, convErr := strconv.Atoi(raw)
		if convErr != nil || value < 1 {
			return 0, 0, errInvalidPage
		}
		page = value
	}
	if raw := q.Get("pageSize"); raw != "" {
		value, convErr := strconv.Atoi(raw)
		if convErr != nil || value < 1 {
			return 0, 0, errInvalidPage
		}
		pageSize = value
	}
	return page, pageSize, nil
}

// hierarchyPageNumber echoes the page the store actually applied.
func hierarchyPageNumber(page int) int {
	if page < 1 {
		return 1
	}
	return page
}

// clampHierarchyPageSize mirrors the store's clamp so the response echoes the
// size that was applied, not the one that was asked for.
func clampHierarchyPageSize(pageSize int) int {
	if pageSize <= 0 {
		return store.LibraryPageDefaultLimit
	}
	if pageSize > store.LibraryPageMaxLimit {
		return store.LibraryPageMaxLimit
	}
	return pageSize
}

// hierarchySeriesID reads the {id} path segment. A malformed id is a 404, not
// a 400: no such series exists, which is the only thing these routes say.
func hierarchySeriesID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeSeriesNotFound(w)
		return 0, false
	}
	return id, true
}

// hierarchyStoreReady answers 503 when the service has no database, matching
// the rest of the Library API.
func (s *Service) hierarchyStoreReady(w http.ResponseWriter) bool {
	if s.store == nil || s.store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "library store is not configured")
		return false
	}
	return true
}

// seriesResourceOf maps one aggregated store row onto the wire shape.
func seriesResourceOf(row store.SeriesRow) seriesResource {
	return seriesResource{
		ID:               row.SeriesID,
		InstanceID:       row.ArrInstanceID,
		Instance:         row.InstanceName,
		Title:            row.Title,
		Year:             row.Year,
		OriginalLanguage: row.OriginalLanguageISO2,
		Monitored:        row.Monitored,
		Available:        row.Available,
		Path:             row.MediaFolderPath,
		SeasonCount:      row.SeasonCount,
		EpisodeCount:     row.EpisodeCount,
		EpisodeFileCount: row.EpisodeFileCount,
		Eligibility:      row.Eligibility,
		UpdatedAt:        row.UpdatedAt,
	}
}

// nonNilLanguages guarantees the wire shape of a language list: the API
// promises an array, and a nil slice would marshal as null (which the UI would
// have to special-case).
func nonNilLanguages(codes []string) []string {
	if codes == nil {
		return []string{}
	}
	return codes
}

// writeHierarchyError maps a query-parse failure onto the documented 400 code.
func writeHierarchyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInvalidSort):
		writeErrorCode(w, http.StatusBadRequest,
			"sort must be title, year or updated and dir asc or desc", "invalid_sort")
	case errors.Is(err, errInvalidInstance):
		writeErrorCode(w, http.StatusBadRequest,
			"instanceId must be a positive integer", "invalid_instance")
	default:
		writeErrorCode(w, http.StatusBadRequest,
			"page and pageSize must be integers of at least 1", "invalid_page")
	}
}

// writeSeriesNotFound is the one 404 these routes answer with.
func writeSeriesNotFound(w http.ResponseWriter) {
	writeErrorCode(w, http.StatusNotFound, "series not found", "not_found")
}

// writeErrorCode is writeError's machine-readable sibling: the UI branches on
// code, never on the message text.
func writeErrorCode(w http.ResponseWriter, status int, message, code string) {
	writeJSON(w, status, errorBody{Error: message, Code: code})
}
