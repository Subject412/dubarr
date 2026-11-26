// The Library items endpoint: one page of the arr cache with the columns the
// Library screen renders, plus the total for the same filters.
package library

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/Subject412/dubarr/backend/internal/store"
)

// libraryItem is one library row as the screen consumes it. Season/Episode are
// null for movies. The keys are fixed (no omitempty) so the client can rely on
// every field being present.
type libraryItem struct {
	ID        int64  `json:"id"`
	MediaType string `json:"media_type"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
	// Season and Episode are the row's numbers; null for movies.
	Season  *int `json:"season"`
	Episode *int `json:"episode"`
	// Kind is the arr application ("radarr"/"sonarr") that owns the row.
	Kind              string `json:"kind"`
	InstanceID        int64  `json:"instance_id"`
	InstanceName      string `json:"instance_name"`
	EligibilityStatus string `json:"eligibility_status"`
	MatchesTargetTags bool   `json:"matches_target_tags"`
	RootFolderPath    string `json:"root_folder_path"`
	TargetFilePath    string `json:"target_file_path"`
	LastSeenAt        string `json:"last_seen_at"`
	UpdatedAt         string `json:"updated_at"`
}

type itemsResponse struct {
	Items  []libraryItem `json:"items"`
	Total  int64         `json:"total"`
	Limit  int           `json:"limit"`
	Offset int           `json:"offset"`
}

// handleItems maps the query string onto store.ListLibraryFilter. Unknown
// kind/eligibility values come back from the store as
// ErrInvalidLibraryFilter (400); limit/offset clamping is the store's job, and
// the clamped values are echoed so the client pages with what was applied.
func (s *Service) handleItems(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "library store is not configured")
		return
	}
	q := r.URL.Query()
	filter := store.ListLibraryFilter{
		Kind:        q.Get("kind"),
		Eligibility: q.Get("eligibility"),
		Q:           q.Get("q"),
	}
	// The store treats <= 0 as "every instance", so a client typo like -1 or 0
	// would silently widen the page to the whole library. Reject it instead.
	if raw := q.Get("instance_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 1 {
			writeError(w, http.StatusBadRequest, "invalid instance_id")
			return
		}
		filter.InstanceID = id
	}
	if raw := q.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		filter.Limit = limit
	}
	if raw := q.Get("offset"); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid offset")
			return
		}
		filter.Offset = offset
	}

	page, err := s.store.ListLibrary(r.Context(), filter)
	if err != nil {
		if errors.Is(err, store.ErrInvalidLibraryFilter) {
			writeError(w, http.StatusBadRequest, "invalid library filter")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not read the library")
		return
	}

	items := make([]libraryItem, 0, len(page.Items))
	for _, row := range page.Items {
		items = append(items, libraryItem{
			ID:                row.ID,
			MediaType:         row.MediaType,
			Title:             row.Title,
			Year:              row.Year,
			Season:            row.SeasonNumber,
			Episode:           row.EpisodeNumber,
			Kind:              row.InstanceType,
			InstanceID:        row.ArrInstanceID,
			InstanceName:      row.InstanceName,
			EligibilityStatus: row.EligibilityStatus,
			MatchesTargetTags: row.MatchesTargetTags,
			RootFolderPath:    row.RootFolderPath,
			TargetFilePath:    row.TargetFilePath,
			LastSeenAt:        row.LastSeenAt,
			UpdatedAt:         row.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, itemsResponse{
		Items:  items,
		Total:  page.Total,
		Limit:  page.Limit,
		Offset: page.Offset,
	})
}
