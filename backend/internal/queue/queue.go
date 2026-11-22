// Package queue exposes the M2 approval queue: pending donor candidates with
// their matched arr target, donor pairing and Tdarr Gate verdict, plus the
// approve/reject status transitions.
//
// M2 is approval-only: approve marks the row extracting for the M3 extraction
// worker; no ffmpeg/mkvmerge work happens here.
package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// defaultDonorPatterns pairs a target with sibling releases whose filename
// carries the configured dubbing marker. It matches the fixture drop
// ("Minions ... PL.DUB ... mp4" next to the Tdarr-tagged target).
var defaultDonorPatterns = []string{"PL.DUB"}

// Dependencies are the collaborators the queue API needs.
type Dependencies struct {
	DB       *sql.DB
	Settings *settings.Store
	// RequireAdmin wraps every route. When nil the API answers 503 rather than
	// running unauthenticated.
	RequireAdmin func(http.Handler) http.Handler
}

// Service holds the resolved dependencies for the registered handlers.
type Service struct {
	store        *store.Store
	settings     *settings.Store
	requireAdmin func(http.Handler) http.Handler
}

// RegisterRoutes mounts:
//
//	GET  /api/queue                 pending_approval candidates
//	POST /api/queue/{id}/approve    pending_approval -> extracting
//	POST /api/queue/{id}/reject     pending_approval -> ignored
func RegisterRoutes(mux *http.ServeMux, deps Dependencies) *Service {
	svc := &Service{
		store:        &store.Store{DB: deps.DB},
		settings:     deps.Settings,
		requireAdmin: deps.RequireAdmin,
	}
	admin := func(h http.HandlerFunc) http.Handler {
		if svc.requireAdmin == nil {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeError(w, http.StatusServiceUnavailable, "authorization is not configured")
			})
		}
		return svc.requireAdmin(h)
	}
	mux.Handle("GET /api/queue", admin(svc.handleList))
	mux.Handle("POST /api/queue/{id}/approve", admin(svc.handleApprove))
	mux.Handle("POST /api/queue/{id}/reject", admin(svc.handleReject))
	return svc
}

// DonorCandidate is a sibling file that can serve as the donor for a target.
type DonorCandidate struct {
	Filename string `json:"filename"`
	Path     string `json:"path"`
	Size     int64  `json:"file_size"`
	// Selected marks the first matched donor: the card arrives pre-selected.
	Selected bool `json:"selected"`
}

// QueueItem is one approval card.
type QueueItem struct {
	ID               int64            `json:"id"`
	SourceFile       string           `json:"source_file_path"`
	Filename         string           `json:"filename"`
	FileSize         int64            `json:"file_size"`
	Status           string           `json:"status"`
	Title            string           `json:"title"`
	Year             int              `json:"year,omitempty"`
	MediaType        string           `json:"media_type,omitempty"`
	SeasonNumber     *int             `json:"season_number,omitempty"`
	EpisodeNumber    *int             `json:"episode_number,omitempty"`
	TargetPath       string           `json:"target_path"`
	OriginalLanguage string           `json:"original_language,omitempty"`
	AudioLanguages   []string         `json:"audio_languages"`
	Eligibility      string           `json:"eligibility"`
	MatchedTags      []string         `json:"matched_tags"`
	MissingTags      []string         `json:"missing_tags"`
	DonorCandidates  []DonorCandidate `json:"donor_candidates"`
}

type listResponse struct {
	Items []QueueItem `json:"items"`
}

type transitionResponse struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "queue store is not configured")
		return
	}
	rows, err := s.store.ListPendingImportCandidates(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the approval queue")
		return
	}
	patterns := s.donorPatterns(r.Context())
	items := make([]QueueItem, 0, len(rows))
	for _, row := range rows {
		item := QueueItem{
			ID:               row.ID,
			SourceFile:       row.SourceFilePath,
			Filename:         row.Filename,
			FileSize:         row.FileSize,
			Status:           row.Status,
			Title:            row.Title,
			Year:             row.Year,
			MediaType:        row.MediaType,
			SeasonNumber:     row.SeasonNumber,
			EpisodeNumber:    row.EpisodeNumber,
			TargetPath:       row.TargetPath,
			OriginalLanguage: row.OriginalLanguage,
			AudioLanguages:   decodeLanguages(row.AudioLanguages),
			Eligibility:      row.Eligibility,
			MatchedTags:      row.Gate.MatchedTags,
			MissingTags:      row.Gate.MissingTags,
			DonorCandidates:  pairDonors(row.TargetPath, patterns),
		}
		if item.MatchedTags == nil {
			item.MatchedTags = []string{}
		}
		if item.MissingTags == nil {
			item.MissingTags = []string{}
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, listResponse{Items: items})
}

func (s *Service) handleApprove(w http.ResponseWriter, r *http.Request) {
	s.transition(w, r, "extracting")
}

func (s *Service) handleReject(w http.ResponseWriter, r *http.Request) {
	s.transition(w, r, "ignored")
}

func (s *Service) transition(w http.ResponseWriter, r *http.Request, to string) {
	if s.store == nil || s.store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "queue store is not configured")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid queue id")
		return
	}
	if err := s.store.TransitionImportCandidate(r.Context(), id, to); err != nil {
		switch {
		case errors.Is(err, store.ErrImportCandidateNotFound):
			writeError(w, http.StatusNotFound, "queue item not found")
		case errors.Is(err, store.ErrImportCandidateState):
			writeError(w, http.StatusConflict, "queue item is no longer pending approval")
		default:
			writeError(w, http.StatusInternalServerError, "could not update the queue item")
		}
		return
	}
	writeJSON(w, http.StatusOK, transitionResponse{ID: id, Status: to})
}

// donorPatterns reads imports.options from the settings kv, falling back to the
// fixture-compatible default when the section is missing or unset.
func (s *Service) donorPatterns(ctx context.Context) []string {
	if s.settings == nil {
		return defaultDonorPatterns
	}
	raw, ok, err := s.settings.Get(ctx, settings.KeyImportsOptions)
	if err != nil || !ok {
		return defaultDonorPatterns
	}
	var opts settings.ImportsOptions
	if err := json.Unmarshal(raw, &opts); err != nil || len(opts.DonorPatterns) == 0 {
		return defaultDonorPatterns
	}
	return opts.DonorPatterns
}

// pairDonors lists regular sibling files of targetPath whose basename matches
// one of patterns. The first match (name order) is marked selected so the
// approval card arrives pre-selected.
func pairDonors(targetPath string, patterns []string) []DonorCandidate {
	out := []DonorCandidate{}
	if strings.TrimSpace(targetPath) == "" || len(patterns) == 0 {
		return out
	}
	dir := filepath.Dir(targetPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	targetName := filepath.Base(targetPath)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == targetName {
			continue
		}
		if matchesDonorPattern(entry.Name(), patterns) {
			names = append(names, entry.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool {
		return strings.ToLower(names[i]) < strings.ToLower(names[j])
	})
	for i, name := range names {
		full := filepath.Join(dir, name)
		var size int64
		if info, err := os.Stat(full); err == nil {
			size = info.Size()
		}
		out = append(out, DonorCandidate{
			Filename: name,
			Path:     full,
			Size:     size,
			Selected: i == 0,
		})
	}
	return out
}

// matchesDonorPattern reports whether filename matches any pattern. Matching is
// case-insensitive. A pattern containing '*' is treated as a glob, otherwise it
// is a substring match, so the default "PL.DUB" finds release names reliably.
func matchesDonorPattern(filename string, patterns []string) bool {
	lower := strings.ToLower(filename)
	for _, pattern := range patterns {
		p := strings.ToLower(strings.TrimSpace(pattern))
		if p == "" {
			continue
		}
		if strings.Contains(p, "*") {
			if ok, err := path.Match(p, lower); err == nil && ok {
				return true
			}
			continue
		}
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func decodeLanguages(raw string) []string {
	out := []string{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []string{}
	}
	return out
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
