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

// IdentifyCandidate is what the identify route needs to know about one queue row
// before it calls arr: the release name to parse, the media kind the row
// already proves, and the arr kind behind it.
//
// MediaType is the matched cache row's media_type, so it is empty for the very
// row identify exists for (an unmatched release). Kind is the arr_instances type
// the row's matched_arr_instance_id points at, empty when the row was never
// linked to an instance. InstanceID is that same matched_arr_instance_id, 0
// when the row has no resolved instance; the candidate pool and the match write
// both narrow on it.
type IdentifyCandidate struct {
	ID                    int64
	Filename              string
	MediaType             string
	Kind                  string
	InstanceID            int64
	MatchedLibraryCacheID int64
}

// ImportCandidateIdentify reads the identify inputs of one queue row. An
// unknown id is ErrImportCandidateNotFound.
func (s *Store) ImportCandidateIdentify(ctx context.Context, id int64) (IdentifyCandidate, error) {
	var (
		row      IdentifyCandidate
		media    sql.NullString
		kind     sql.NullString
		cacheID  sql.NullInt64
		instance sql.NullInt64
	)
	err := s.DB.QueryRowContext(ctx, `
		SELECT iq.id, iq.filename, alc.media_type, ai.type, iq.matched_arr_instance_id, iq.matched_library_cache_id
		FROM import_queue iq
		LEFT JOIN arr_library_cache alc ON alc.id = iq.matched_library_cache_id
		LEFT JOIN arr_instances ai ON ai.id = iq.matched_arr_instance_id
		WHERE iq.id = ?`, id).Scan(&row.ID, &row.Filename, &media, &kind, &instance, &cacheID)
	if errors.Is(err, sql.ErrNoRows) {
		return IdentifyCandidate{}, ErrImportCandidateNotFound
	}
	if err != nil {
		return IdentifyCandidate{}, fmt.Errorf("store: read import candidate %d identify inputs: %w", id, err)
	}
	row.MediaType = media.String
	row.Kind = kind.String
	row.InstanceID = instance.Int64
	row.MatchedLibraryCacheID = cacheID.Int64
	return row, nil
}

// IdentifyPoolLimit caps how many arr_library_cache rows one identify pass
// scores before the route ranks and trims the list.
const IdentifyPoolLimit = 200

// LibraryCandidate is one arr_library_cache row the identify route offers as a
// possible match for a parsed release.
type LibraryCandidate struct {
	ID             int64
	Title          string
	Year           int
	MediaType      string
	ExternalItemID int64
}

// IdentifyCandidatePool returns the cache rows a parsed title can be scored
// against: the rows of one media kind (both when mediaType is empty) whose title
// contains term, case-insensitively, capped at IdentifyPoolLimit.
//
// instanceID narrows the pool to one arr instance when the queue row has a
// resolved instance (nonzero); 0 means "no instance resolved" and keeps both
// kinds' rows, exactly as an empty mediaType does.
//
// Scoring stays in the caller: the pool is a deterministic LIKE narrowing, not a
// verdict, and a blank term yields the kind's rows rather than nothing so a
// parser that produced no title can still be inspected.
func (s *Store) IdentifyCandidatePool(ctx context.Context, instanceID int64, mediaType, term string) ([]LibraryCandidate, error) {
	pattern := "%"
	if strings.TrimSpace(term) != "" {
		pattern = likeContainsPattern(term)
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, title, COALESCE(year, 0), media_type, external_item_id
		FROM arr_library_cache
		WHERE (? = '' OR media_type = ?)
		  AND (? = 0 OR arr_instance_id = ?)
		  AND title LIKE ?`+likeEscapeSQL+`
		ORDER BY title COLLATE NOCASE, year, id
		LIMIT ?`, mediaType, mediaType, instanceID, instanceID, pattern, IdentifyPoolLimit)
	if err != nil {
		return nil, fmt.Errorf("store: identify candidate pool: %w", err)
	}
	defer rows.Close()

	out := []LibraryCandidate{}
	for rows.Next() {
		var item LibraryCandidate
		if err := rows.Scan(&item.ID, &item.Title, &item.Year, &item.MediaType, &item.ExternalItemID); err != nil {
			return nil, fmt.Errorf("store: scan identify candidate: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate identify candidates: %w", err)
	}
	return out, nil
}

// mediaTypeForArrKind maps an arr instance type to the arr_library_cache media
// type it serves.
func mediaTypeForArrKind(kind string) string {
	switch kind {
	case "radarr":
		return "movie"
	case "sonarr":
		return "episode"
	default:
		return ""
	}
}

// SetImportCandidateLibraryMatch persists an operator's manual arr library pick
// for one queue row. It sets matched_library_cache_id and parse_confidence and
// performs NO status transition: approve owns the pending_approval -> extracting
// move, and matching a row must not imply approval.
//
// Validation, in order:
//
//	unknown queue id                      -> ErrImportCandidateNotFound
//	row not pending_approval              -> ErrImportCandidateState
//	unknown cache id                      -> ErrLibraryCacheNotFound
//	cache row serves another kind         -> ErrImportCandidateKindMismatch
//	cache row belongs to another instance -> ErrImportCandidateInstanceMismatch
//	row matched to another id meanwhile   -> ErrImportCandidateMatchMismatch
//
// The last check is not a read-then-write: the UPDATE itself requires the row to
// still be pending_approval and still unmatched-or-matched-to-this-id, so two
// concurrent picks of different ids cannot both succeed with the later one
// silently winning. A zero row count is classified by re-reading the row.
//
// Re-picking the id the row is already matched to is idempotent (it rewrites the
// same values), so a retried identify does not have to fail.
//
// confidence is accepted for call-site compatibility and deliberately ignored: a
// human pick is never an exact parse, so the row is always stored with the
// schema's highest non-exact label, "ambiguous" (the parse_confidence
// vocabulary is exact | ambiguous | unmatched).
func (s *Store) SetImportCandidateLibraryMatch(ctx context.Context, queueID, cacheID int64, confidence string) error {
	if cacheID <= 0 {
		return fmt.Errorf("store: import candidate %d: library cache id must be positive", queueID)
	}
	_ = confidence
	const label = "ambiguous"

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin import candidate match: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	var (
		status      string
		cacheMedia  sql.NullString
		rowKind     sql.NullString
		rowInstance sql.NullInt64
	)
	err = tx.QueryRowContext(ctx, `
		SELECT iq.status,
		       (SELECT media_type FROM arr_library_cache WHERE id = iq.matched_library_cache_id),
		       (SELECT type FROM arr_instances WHERE id = iq.matched_arr_instance_id),
		       iq.matched_arr_instance_id
		FROM import_queue iq WHERE iq.id = ?`, queueID).Scan(&status, &cacheMedia, &rowKind, &rowInstance)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrImportCandidateNotFound
	}
	if err != nil {
		return fmt.Errorf("store: read import candidate %d: %w", queueID, err)
	}
	if status != "pending_approval" {
		return ErrImportCandidateState
	}

	var (
		targetMedia    sql.NullString
		targetInstance sql.NullInt64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT media_type, arr_instance_id FROM arr_library_cache WHERE id = ?`, cacheID).
		Scan(&targetMedia, &targetInstance)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLibraryCacheNotFound
	}
	if err != nil {
		return fmt.Errorf("store: read library cache %d: %w", cacheID, err)
	}

	// The row's kind comes from what the row already proves: the matched cache
	// row's media type, else the instance it was matched on. A row with neither
	// has no kind to disagree with.
	expected := cacheMedia.String
	if expected == "" {
		expected = mediaTypeForArrKind(rowKind.String)
	}
	if expected != "" && expected != targetMedia.String {
		return ErrImportCandidateKindMismatch
	}
	// Kind alone is not instance agreement: arr_instances.type has no UNIQUE, so
	// two radarr rows could exist. A row with no resolved instance stays
	// unfiltered, matching the pool's "no instance resolved means both kinds".
	if rowInstance.Valid && targetInstance.Valid && targetInstance.Int64 != rowInstance.Int64 {
		return ErrImportCandidateInstanceMismatch
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE import_queue
		 SET matched_library_cache_id = ?, parse_confidence = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND status = 'pending_approval'
		   AND (matched_library_cache_id IS NULL OR matched_library_cache_id = ?)`,
		cacheID, label, queueID, cacheID)
	if err != nil {
		return fmt.Errorf("store: record import candidate %d match: %w", queueID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: record import candidate %d match rows: %w", queueID, err)
	}
	if n != 1 {
		return classifyMatchWriteMiss(ctx, tx, queueID)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit import candidate match: %w", err)
	}
	return nil
}

// classifyMatchWriteMiss turns a zero-row conditional UPDATE into the sentinel
// the route maps: the row left pending_approval (422) or another pick won the
// race (409). The re-read happens inside the same transaction that owns the
// failed write, so it sees the row as the UPDATE saw it.
func classifyMatchWriteMiss(ctx context.Context, tx *sql.Tx, queueID int64) error {
	var (
		status    string
		matchedID sql.NullInt64
	)
	err := tx.QueryRowContext(ctx,
		`SELECT status, matched_library_cache_id FROM import_queue WHERE id = ?`, queueID).
		Scan(&status, &matchedID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrImportCandidateNotFound
	}
	if err != nil {
		return fmt.Errorf("store: re-read import candidate %d after match miss: %w", queueID, err)
	}
	if status != "pending_approval" {
		return ErrImportCandidateState
	}
	return ErrImportCandidateMatchMismatch
}
