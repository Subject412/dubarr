// Library page read surface (#29): the paginated, filterable listing the
// Library screen renders, plus the joins it needs for instance identity.
//
// This is deliberately separate from ListLibrarySearch, which serves manual
// matching and therefore requires a search term and returns at most
// LibrarySearchLimit rows. The page read must also work with a blank query
// (browse the whole library), paginate deterministically and count the full
// matching set before limit/offset.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Library page defaults and bounds.
const (
	// LibraryPageDefaultLimit is the page size when the filter asks for none.
	LibraryPageDefaultLimit = 50
	// LibraryPageMaxLimit caps the page size so one request cannot pull the
	// whole library into memory.
	LibraryPageMaxLimit = 200
)

// ErrInvalidLibraryFilter reports a filter value the read surface does not
// understand (an unknown kind, an unknown eligibility verdict). It is returned
// before the query runs, so callers can map it to a 400.
var ErrInvalidLibraryFilter = errors.New("store: invalid library filter")

// ListLibraryFilter selects one page of arr_library_cache rows. The zero value
// lists the whole library: blank Q, no kind/eligibility/instance restriction,
// first page of LibraryPageDefaultLimit rows.
type ListLibraryFilter struct {
	// Kind restricts to one arr application: "radarr" or "sonarr". Empty
	// means both.
	Kind string
	// InstanceID restricts to one arr instance; <= 0 means every instance.
	InstanceID int64
	// Eligibility restricts to one gate verdict: "eligible",
	// "waiting_for_tags" or "missing_target". Empty means all.
	Eligibility string
	// Q matches title or original_title as a case-insensitive substring.
	// Blank matches everything (unlike ListLibrarySearch).
	Q string
	// Limit defaults to LibraryPageDefaultLimit and is capped at
	// LibraryPageMaxLimit; values below 1 take the default.
	Limit int
	// Offset skips that many rows; negative values are treated as 0.
	Offset int
}

// LibraryListItem is one row of the Library page. It carries only the columns
// the page needs: the JSON blob columns (raw_metadata_json,
// audio_languages_json, arr_tag_ids_json, arr_tag_labels_json) are never
// selected, so a page cannot accidentally ship them to a client.
type LibraryListItem struct {
	ID             int64  `json:"id"`
	ArrInstanceID  int64  `json:"arr_instance_id"`
	InstanceName   string `json:"instance_name"`
	InstanceType   string `json:"instance_type"`
	MediaType      string `json:"media_type"`
	ExternalItemID int64  `json:"external_item_id"`
	SeriesID       int64  `json:"series_id"` // 0 for movies
	TMDBID         int64  `json:"tmdb_id"`
	TVDBID         int64  `json:"tvdb_id"`
	IMDBID         string `json:"imdb_id"`
	Title          string `json:"title"`
	OriginalTitle  string `json:"original_title"`
	Year           int    `json:"year"`
	SeasonNumber   *int   `json:"season_number,omitempty"`
	EpisodeNumber  *int   `json:"episode_number,omitempty"`
	EpisodeTitle   string `json:"episode_title"`
	// MediaFolderPath is the arr folder; TargetFilePath is the resolved video
	// (empty while arr has no file).
	MediaFolderPath string `json:"media_folder_path"`
	TargetFilePath  string `json:"target_file_path"`
	// MatchesTargetTags is the Processing Gate verdict for the target.
	MatchesTargetTags bool   `json:"matches_target_tags"`
	EligibilityStatus string `json:"eligibility_status"`
	VideoCodec        string `json:"video_codec"`
	// UpdatedAt is when the row's content last changed; LastSeenAt is when a
	// sync pass last observed it (empty = never observed, NULL in the row).
	UpdatedAt  string `json:"updated_at"`
	LastSeenAt string `json:"last_seen_at"`
}

// LibraryPage is one page plus the total for the same filters.
type LibraryPage struct {
	Items  []LibraryListItem `json:"items"`
	Total  int64             `json:"total"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
}

// ListLibrary returns one page of cached arr items for the Library screen.
//
// Filters compose: Kind, InstanceID, Eligibility and Q are all applied to both
// the page and the Total count, and the ordering is deterministic (title,
// then id) so paging cannot repeat or skip a row between requests. A blank Q
// returns everything, which is the difference from ListLibrarySearch.
func (s *Store) ListLibrary(ctx context.Context, f ListLibraryFilter) (LibraryPage, error) {
	where, args, err := libraryFilterSQL(f)
	if err != nil {
		return LibraryPage{}, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = LibraryPageDefaultLimit
	}
	if limit > LibraryPageMaxLimit {
		limit = LibraryPageMaxLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	page := LibraryPage{Items: []LibraryListItem{}, Limit: limit, Offset: offset}

	countArgs := append([]any(nil), args...)
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM arr_library_cache c
		 JOIN arr_instances i ON i.id = c.arr_instance_id`+where,
		countArgs...).Scan(&page.Total); err != nil {
		return LibraryPage{}, fmt.Errorf("store: count library rows: %w", err)
	}
	if page.Total == 0 {
		return page, nil
	}

	pageArgs := append(append([]any(nil), args...), limit, offset)
	rows, err := s.DB.QueryContext(ctx, libraryPageColumns+where+
		` ORDER BY c.title COLLATE NOCASE, c.id LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		return LibraryPage{}, fmt.Errorf("store: list library rows: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			item     LibraryListItem
			season   sql.NullInt64
			episode  sql.NullInt64
			lastSeen sql.NullString
		)
		if err := rows.Scan(&item.ID, &item.ArrInstanceID, &item.InstanceName, &item.InstanceType,
			&item.MediaType, &item.ExternalItemID, &item.SeriesID, &item.TMDBID, &item.TVDBID,
			&item.IMDBID, &item.Title, &item.OriginalTitle, &item.Year, &season, &episode,
			&item.EpisodeTitle, &item.MediaFolderPath, &item.TargetFilePath,
			&item.MatchesTargetTags, &item.EligibilityStatus, &item.VideoCodec,
			&item.UpdatedAt, &lastSeen); err != nil {
			return LibraryPage{}, fmt.Errorf("store: scan library row: %w", err)
		}
		if season.Valid {
			v := int(season.Int64)
			item.SeasonNumber = &v
		}
		if episode.Valid {
			v := int(episode.Int64)
			item.EpisodeNumber = &v
		}
		item.LastSeenAt = lastSeen.String
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return LibraryPage{}, fmt.Errorf("store: iterate library rows: %w", err)
	}
	return page, nil
}

// libraryPageColumns is the page projection. The JSON blob columns are
// intentionally absent: raw_metadata_json and audio_languages_json are
// diagnostics for other screens, and the arr tag snapshots are internal to
// eligibility.
const libraryPageColumns = `
	SELECT c.id, c.arr_instance_id, i.name, i.type,
	       c.media_type, c.external_item_id, COALESCE(c.series_id, 0),
	       COALESCE(c.tmdb_id, 0), COALESCE(c.tvdb_id, 0), COALESCE(c.imdb_id, ''),
	       c.title, COALESCE(c.original_title, ''), COALESCE(c.year, 0),
	       c.season_number, c.episode_number, COALESCE(c.episode_title, ''),
	       COALESCE(c.media_folder_path, ''), COALESCE(c.target_file_path, ''),
	       c.matches_target_tags, c.eligibility_status, COALESCE(c.video_codec, ''),
	       COALESCE(c.updated_at, ''), c.last_seen_at
	FROM arr_library_cache c
	JOIN arr_instances i ON i.id = c.arr_instance_id`

// libraryFilterSQL validates the filter and renders the shared WHERE clause so
// the page and the count can never disagree.
func libraryFilterSQL(f ListLibraryFilter) (string, []any, error) {
	var clauses []string
	var args []any

	switch f.Kind {
	case "":
	case "radarr", "sonarr":
		clauses = append(clauses, "i.type = ?")
		args = append(args, f.Kind)
	default:
		return "", nil, fmt.Errorf("%w: kind %q is not radarr or sonarr", ErrInvalidLibraryFilter, f.Kind)
	}

	switch f.Eligibility {
	case "":
	case "eligible", "waiting_for_tags", "missing_target":
		clauses = append(clauses, "c.eligibility_status = ?")
		args = append(args, f.Eligibility)
	default:
		return "", nil, fmt.Errorf("%w: eligibility %q is not a gate verdict",
			ErrInvalidLibraryFilter, f.Eligibility)
	}

	if f.InstanceID > 0 {
		clauses = append(clauses, "c.arr_instance_id = ?")
		args = append(args, f.InstanceID)
	}

	if term := strings.TrimSpace(f.Q); term != "" {
		// likeContainsPattern escapes a literal "%" or "_" in the operator's
		// text so it is matched as text, never as a wildcard. The pattern and
		// the ESCAPE clause come from the shared helpers so this read and
		// ListLibrarySearch cannot drift apart.
		pattern := likeContainsPattern(term)
		clauses = append(clauses, `(c.title LIKE ?`+likeEscapeSQL+
			` OR COALESCE(c.original_title, '') LIKE ?`+likeEscapeSQL+`)`)
		args = append(args, pattern, pattern)
	}

	if len(clauses) == 0 {
		return "", args, nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args, nil
}
