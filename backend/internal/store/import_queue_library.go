package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// LibrarySearchLimit caps how many arr_library_cache rows the manual matching
// search returns (IDEA.md §4 GET /api/imports/search-library).
const LibrarySearchLimit = 20

// likeEscapeChar is the single-character ESCAPE used by every LIKE in the
// store. It is a backslash, so the escaping below and the SQL clause must
// always be used together (likeContainsPattern + likeEscapeSQL).
const likeEscapeChar = `\`

// likeEscapeSQL is appended to a LIKE predicate so the backslash escapes
// rendered by likeContainsPattern are interpreted instead of compared.
const likeEscapeSQL = ` ESCAPE '\'`

// likeEscaper turns LIKE metacharacters in operator input into literals.
var likeEscaper = strings.NewReplacer(
	likeEscapeChar, likeEscapeChar+likeEscapeChar,
	"%", likeEscapeChar+"%",
	"_", likeEscapeChar+"_",
)

// escapeLikeTerm escapes %, _ and the escape character itself so a literal
// "%" typed by an operator is matched as text, never as "everything".
func escapeLikeTerm(term string) string { return likeEscaper.Replace(term) }

// likeContainsPattern renders the "%term%" pattern for a case-insensitive
// substring match. It is the single place both library searches escape their
// input, so ListLibrarySearch and ListLibrary cannot drift apart; pair it with
// likeEscapeSQL in the query.
func likeContainsPattern(term string) string { return "%" + escapeLikeTerm(term) + "%" }

// LibrarySearchItem is one cached arr library row offered to the operator when
// the automatic parse could not identify a dropped file (e.g. a localized
// release name such as Kraina.Lodu.2013.PL.mkv).
type LibrarySearchItem struct {
	ID               int64  `json:"id"`
	Title            string `json:"title"`
	Year             int    `json:"year"`
	MediaType        string `json:"media_type"`
	SeasonNumber     *int   `json:"season_number,omitempty"`
	EpisodeNumber    *int   `json:"episode_number,omitempty"`
	OriginalLanguage string `json:"original_language_iso2"`
}

// ListLibrarySearch returns up to LibrarySearchLimit arr_library_cache rows
// whose title contains q (case-insensitive substring), ordered by title, year
// and episode so repeated searches are stable.
//
// LIKE wildcards in q are escaped: a literal "%" is matched as text, never as
// "everything". An empty/blank query returns no rows instead of the whole
// library.
func (s *Store) ListLibrarySearch(ctx context.Context, q string) ([]LibrarySearchItem, error) {
	term := strings.TrimSpace(q)
	if term == "" {
		return []LibrarySearchItem{}, nil
	}
	pattern := likeContainsPattern(term)
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, title, COALESCE(year, 0), media_type, season_number, episode_number,
		       COALESCE(original_language_iso2, '')
		FROM arr_library_cache
		WHERE title LIKE ?`+likeEscapeSQL+`
		ORDER BY title COLLATE NOCASE, year, season_number, episode_number, id
		LIMIT ?`, pattern, LibrarySearchLimit)
	if err != nil {
		return nil, fmt.Errorf("store: search library: %w", err)
	}
	defer rows.Close()

	out := []LibrarySearchItem{}
	for rows.Next() {
		var (
			item             LibrarySearchItem
			year             sql.NullInt64
			season, episode  sql.NullInt64
			title, mediaType string
			origLang         string
		)
		if err := rows.Scan(&item.ID, &title, &year, &mediaType, &season, &episode, &origLang); err != nil {
			return nil, fmt.Errorf("store: scan library search row: %w", err)
		}
		item.Title = title
		item.Year = int(year.Int64)
		item.MediaType = mediaType
		item.OriginalLanguage = origLang
		if season.Valid {
			v := int(season.Int64)
			item.SeasonNumber = &v
		}
		if episode.Valid {
			v := int(episode.Int64)
			item.EpisodeNumber = &v
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate library search: %w", err)
	}
	return out, nil
}

// LibraryCacheTarget reports whether the arr_library_cache row exists and, when
// it does, its target file path (possibly empty: an arr item whose file is not
// on disk yet). It lets approve validate a manually supplied library id before
// persisting the match.
func (s *Store) LibraryCacheTarget(ctx context.Context, id int64) (string, bool, error) {
	var target sql.NullString
	err := s.DB.QueryRowContext(ctx,
		`SELECT target_file_path FROM arr_library_cache WHERE id = ?`, id).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read library cache %d: %w", id, err)
	}
	return target.String, true, nil
}
