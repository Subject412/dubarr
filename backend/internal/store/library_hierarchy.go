// Library hierarchy read surface (#56): Radarr as one flat paged movie list,
// and Sonarr as a series -> seasons -> episodes tree where every level is a
// separate read against arr_library_cache.
//
// The surface is read-only by construction: every method here issues a SELECT
// and nothing else, because the mirror's contents are owned by arrsync. All
// aggregation happens in SQLite (GROUP BY series_id for a series, GROUP BY
// season_number for its seasons); episode rows are never loaded into Go to be
// counted, which is what keeps a 12 000-episode library inside the p95 budget
// the acceptance criteria name.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Hierarchy read bounds. The page bounds are the ones the flat listing already
// enforces (LibraryPageDefaultLimit / LibraryPageMaxLimit in library_read.go),
// reused rather than duplicated.
const (
	// LibrarySeasonMaxRows is the hard cap for one season payload. A season
	// above it is a data error, not a paging problem: the handler reports
	// truncated instead of silently dropping episodes.
	LibrarySeasonMaxRows = 2000

	// LibraryEpisodePageDefaultLimit is the episode page size when the request
	// asks for none. A season is small enough that the default returns it
	// whole for the common case.
	LibraryEpisodePageDefaultLimit = 200

	// LibraryEpisodePageMaxLimit caps the episode page size.
	LibraryEpisodePageMaxLimit = 500
)

// HierarchyFilter is the whole query surface of the movie and series lists.
// Every field is applied on the server: nothing here is a hint the client is
// expected to re-apply.
type HierarchyFilter struct {
	// Q matches title / original_title (movies) or the series title (series)
	// as a case-insensitive substring. Blank matches everything.
	Q string
	// InstanceID restricts to one arr instance; <= 0 means every instance.
	InstanceID int64
	// Page is 1-based; values below 1 are treated as 1.
	Page int
	// PageSize is clamped to [1, LibraryPageMaxLimit]; <= 0 takes
	// LibraryPageDefaultLimit.
	PageSize int
	// Sort is "title" (default) | "year" | "updated".
	Sort string
	// Dir is "asc" (default) | "desc".
	Dir string
}

// ValidateOrder reports whether Sort and Dir name a documented ordering. The
// HTTP layer maps its error to 400 invalid_sort and the read methods refuse to
// guess, so a query-string typo can never silently reorder a page.
func (f HierarchyFilter) ValidateOrder() error {
	switch f.Sort {
	case "", "title", "year", "updated":
	default:
		return fmt.Errorf("%w: sort %q", ErrInvalidLibraryFilter, f.Sort)
	}
	switch f.Dir {
	case "", "asc", "desc":
	default:
		return fmt.Errorf("%w: dir %q", ErrInvalidLibraryFilter, f.Dir)
	}
	return nil
}

// MovieRow is one Radarr row of the flat movie list.
//
// ID is arr_library_cache.id — the id the rest of the API (and /rescan) takes,
// not the Radarr movieId (ExternalItemID). HasFile is derived from
// TargetFilePath: the mirror records "no file" as NULL or an empty path, never
// as a separate flag. SizeOnDisk ships as 0 because the mirror does not record
// it, and stat-ing one row per page would be N syscalls that lie under
// concurrent writes.
type MovieRow struct {
	ID              int64
	ExternalItemID  int64
	ArrInstanceID   int64
	InstanceName    string
	Title           string
	OriginalTitle   string
	Year            int
	MediaFolderPath string
	TargetFilePath  string
	HasFile         bool
	SizeOnDisk      int64
	Eligibility     string
	VideoCodec      string
	UpdatedAt       string
}

// SeriesRow is one Sonarr series, aggregated from its episode rows.
//
// SeriesID is arr_library_cache.series_id (the Sonarr seriesId). Title is the
// series title, identical on every episode row. Eligibility is the worst
// verdict across the series: waiting_for_tags > missing_target > eligible.
type SeriesRow struct {
	SeriesID         int64
	ArrInstanceID    int64
	InstanceName     string
	Title            string
	Year             int
	MediaFolderPath  string
	SeasonCount      int
	EpisodeCount     int
	EpisodeFileCount int
	Eligibility      string
	UpdatedAt        string
}

// SeasonRow is one season of a series, aggregated in SQL. PercentEpisodes is
// 100 * file/episode, 0 when the season has no rows.
type SeasonRow struct {
	SeasonNumber     int
	EpisodeCount     int
	EpisodeFileCount int
	PercentEpisodes  float64
}

// EpisodeRow is one Sonarr episode of one season. ExternalItemID is the
// Sonarr episodeId.
type EpisodeRow struct {
	ID             int64
	SeriesID       int64
	ExternalItemID int64
	SeasonNumber   int
	EpisodeNumber  int
	EpisodeTitle   string
	TargetFilePath string
	HasFile        bool
	Eligibility    string
	UpdatedAt      string
}

// ListMovies returns one page of movies plus the unpaged total for the same
// filters. Both queries share hierarchyMovieWhere, so the page and the count
// can never disagree about what matched.
func (s *Store) ListMovies(ctx context.Context, f HierarchyFilter) (int64, []MovieRow, error) {
	if err := f.ValidateOrder(); err != nil {
		return 0, nil, err
	}
	where, args := hierarchyMovieWhere(f)

	var total int64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM arr_library_cache c`+where, args...).Scan(&total); err != nil {
		return 0, nil, fmt.Errorf("store: count movies: %w", err)
	}
	if total == 0 {
		return 0, []MovieRow{}, nil
	}

	limit, offset := hierarchyPageBounds(f.Page, f.PageSize, LibraryPageDefaultLimit, LibraryPageMaxLimit)
	pageArgs := append(append([]any(nil), args...), limit, offset)
	rows, err := s.DB.QueryContext(ctx,
		hierarchyMovieColumns+where+hierarchyMovieOrder(f)+` LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		return 0, nil, fmt.Errorf("store: list movies: %w", err)
	}
	defer rows.Close()

	out := make([]MovieRow, 0, limit)
	for rows.Next() {
		var row MovieRow
		if err := rows.Scan(&row.ID, &row.ExternalItemID, &row.ArrInstanceID, &row.InstanceName,
			&row.Title, &row.OriginalTitle, &row.Year, &row.MediaFolderPath,
			&row.TargetFilePath, &row.Eligibility, &row.VideoCodec, &row.UpdatedAt); err != nil {
			return 0, nil, fmt.Errorf("store: scan movie: %w", err)
		}
		row.HasFile = row.TargetFilePath != ""
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("store: iterate movies: %w", err)
	}
	return total, out, nil
}

// ListSeries returns one page of series plus the total number of grouped
// series for the same filters. The aggregation is one SQL statement
// (GROUP BY series_id); the total is a second aggregate over the same grouped
// set, never a read of every episode row into Go.
func (s *Store) ListSeries(ctx context.Context, f HierarchyFilter) (int64, []SeriesRow, error) {
	if err := f.ValidateOrder(); err != nil {
		return 0, nil, err
	}
	where, args := hierarchySeriesWhere(f)

	var total int64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (SELECT c.series_id FROM arr_library_cache c`+
			where+` GROUP BY c.series_id)`, args...).Scan(&total); err != nil {
		return 0, nil, fmt.Errorf("store: count series: %w", err)
	}
	if total == 0 {
		return 0, []SeriesRow{}, nil
	}

	limit, offset := hierarchyPageBounds(f.Page, f.PageSize, LibraryPageDefaultLimit, LibraryPageMaxLimit)
	pageArgs := append(append([]any(nil), args...), limit, offset)
	rows, err := s.DB.QueryContext(ctx,
		hierarchySeriesAggregate+where+` GROUP BY c.series_id`+hierarchySeriesOrder(f)+
			` LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		return 0, nil, fmt.Errorf("store: list series: %w", err)
	}
	defer rows.Close()

	out := make([]SeriesRow, 0, limit)
	for rows.Next() {
		row, err := hierarchySeriesScan(rows.Scan)
		if err != nil {
			return 0, nil, fmt.Errorf("store: scan series: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("store: iterate series: %w", err)
	}
	return total, out, nil
}

// SeriesByID is the header for the season view. found=false when no cached
// episode row names that series (a series the last snapshot deleted, or an id
// that never existed); the handler turns that into a 404.
func (s *Store) SeriesByID(ctx context.Context, seriesID int64) (SeriesRow, bool, error) {
	if seriesID <= 0 {
		return SeriesRow{}, false, nil
	}
	row, err := hierarchySeriesScan(s.DB.QueryRowContext(ctx,
		hierarchySeriesAggregate+` WHERE c.media_type = 'episode' AND c.series_id = ?
		 GROUP BY c.series_id`, seriesID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return SeriesRow{}, false, nil
	}
	if err != nil {
		return SeriesRow{}, false, fmt.Errorf("store: read series %d: %w", seriesID, err)
	}
	return row, true, nil
}

// SeasonsForSeries aggregates one series' seasons in SQL, ordered numerically
// (season 10 after season 2, which a lexical TEXT sort would get wrong).
func (s *Store) SeasonsForSeries(ctx context.Context, seriesID int64) ([]SeasonRow, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT c.season_number,
		        COUNT(*),
		        SUM(CASE WHEN c.target_file_path IS NOT NULL AND c.target_file_path <> ''
		                 THEN 1 ELSE 0 END),
		        100.0 * SUM(CASE WHEN c.target_file_path IS NOT NULL AND c.target_file_path <> ''
		                         THEN 1 ELSE 0 END) / COUNT(*)
		   FROM arr_library_cache c
		  WHERE c.media_type = 'episode' AND c.series_id = ?
		    AND c.season_number IS NOT NULL
		  GROUP BY c.season_number
		  ORDER BY c.season_number`, seriesID)
	if err != nil {
		return nil, fmt.Errorf("store: read seasons of series %d: %w", seriesID, err)
	}
	defer rows.Close()

	out := []SeasonRow{}
	for rows.Next() {
		var row SeasonRow
		if err := rows.Scan(&row.SeasonNumber, &row.EpisodeCount,
			&row.EpisodeFileCount, &row.PercentEpisodes); err != nil {
			return nil, fmt.Errorf("store: scan season of series %d: %w", seriesID, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate seasons of series %d: %w", seriesID, err)
	}
	return out, nil
}

// EpisodesForSeason returns one season ordered by episode number, capped at
// LibrarySeasonMaxRows. The cap is a data guard, not paging: the handler pairs
// it with SeasonEpisodeCount so a season above the cap is reported truncated
// with its real size instead of appearing short.
func (s *Store) EpisodesForSeason(ctx context.Context, seriesID int64, season int) ([]EpisodeRow, error) {
	return s.episodesForSeason(ctx, seriesID, season, LibrarySeasonMaxRows, 0)
}

// EpisodesForSeasonPage returns one page of one season. page is 1-based and
// pageSize is clamped to [1, LibraryEpisodePageMaxLimit], so a page is always
// a slice of a single season and never spills into the next one.
func (s *Store) EpisodesForSeasonPage(ctx context.Context, seriesID int64, season, page, pageSize int) ([]EpisodeRow, error) {
	limit, offset := hierarchyPageBounds(page, pageSize,
		LibraryEpisodePageDefaultLimit, LibraryEpisodePageMaxLimit)
	return s.episodesForSeason(ctx, seriesID, season, limit, offset)
}

func (s *Store) episodesForSeason(ctx context.Context, seriesID int64, season, limit, offset int) ([]EpisodeRow, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT c.id, COALESCE(c.series_id, 0), c.external_item_id,
		        COALESCE(c.season_number, 0), COALESCE(c.episode_number, 0),
		        COALESCE(c.episode_title, ''), COALESCE(c.target_file_path, ''),
		        c.eligibility_status, COALESCE(c.updated_at, '')
		   FROM arr_library_cache c
		  WHERE c.media_type = 'episode' AND c.series_id = ? AND c.season_number = ?
		  ORDER BY c.episode_number, c.id
		  LIMIT ? OFFSET ?`, seriesID, season, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: read episodes of series %d season %d: %w", seriesID, season, err)
	}
	defer rows.Close()

	out := []EpisodeRow{}
	for rows.Next() {
		var row EpisodeRow
		if err := rows.Scan(&row.ID, &row.SeriesID, &row.ExternalItemID, &row.SeasonNumber,
			&row.EpisodeNumber, &row.EpisodeTitle, &row.TargetFilePath,
			&row.Eligibility, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan episode of series %d season %d: %w", seriesID, season, err)
		}
		row.HasFile = row.TargetFilePath != ""
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate episodes of series %d season %d: %w", seriesID, season, err)
	}
	return out, nil
}

// SeasonEpisodeCount is the whole season's episode count, so the handler can
// report the real total even when the cap shortens the payload.
func (s *Store) SeasonEpisodeCount(ctx context.Context, seriesID int64, season int) (int64, error) {
	var total int64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM arr_library_cache c
		  WHERE c.media_type = 'episode' AND c.series_id = ? AND c.season_number = ?`,
		seriesID, season).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: count episodes of series %d season %d: %w", seriesID, season, err)
	}
	return total, nil
}

// hierarchyPageBounds normalises a 1-based page into LIMIT/OFFSET, clamping the
// page size the same way the flat listing does.
func hierarchyPageBounds(page, pageSize, defaultSize, maxSize int) (limit, offset int) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = defaultSize
	}
	if pageSize > maxSize {
		pageSize = maxSize
	}
	return pageSize, (page - 1) * pageSize
}

// hierarchyMovieColumns is the movie projection: the JSON blob columns never
// appear, so a page cannot ship diagnostics to the client.
const hierarchyMovieColumns = `
	SELECT c.id, c.external_item_id, c.arr_instance_id, i.name,
	       c.title, COALESCE(c.original_title, ''), COALESCE(c.year, 0),
	       COALESCE(c.media_folder_path, ''), COALESCE(c.target_file_path, ''),
	       c.eligibility_status, COALESCE(c.video_codec, ''), COALESCE(c.updated_at, '')
	FROM arr_library_cache c
	JOIN arr_instances i ON i.id = c.arr_instance_id`

// hierarchyMovieWhere renders the movie filters. Every predicate is on the c
// alias, so the count query can drop the instance join.
func hierarchyMovieWhere(f HierarchyFilter) (string, []any) {
	clauses := []string{"c.media_type = 'movie'"}
	var args []any
	if f.InstanceID > 0 {
		clauses = append(clauses, "c.arr_instance_id = ?")
		args = append(args, f.InstanceID)
	}
	if term := strings.TrimSpace(f.Q); term != "" {
		pattern := likeContainsPattern(term)
		clauses = append(clauses, `(c.title LIKE ?`+likeEscapeSQL+
			` OR COALESCE(c.original_title, '') LIKE ?`+likeEscapeSQL+`)`)
		args = append(args, pattern, pattern)
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// hierarchyMovieOrder always ends on c.id so a page boundary cannot repeat or
// skip a row when several movies share a title.
func hierarchyMovieOrder(f HierarchyFilter) string {
	dir := "ASC"
	if f.Dir == "desc" {
		dir = "DESC"
	}
	switch f.Sort {
	case "year":
		return " ORDER BY COALESCE(c.year, 0) " + dir + ", c.title COLLATE NOCASE ASC, c.id ASC"
	case "updated":
		return " ORDER BY COALESCE(c.updated_at, '') " + dir + ", c.id " + dir
	default:
		return " ORDER BY c.title COLLATE NOCASE " + dir + ", c.id " + dir
	}
}

// hierarchySeriesAggregate is the series projection: one row per series_id,
// with the season count, episode count, file count and worst eligibility
// verdict computed by SQLite. MIN(title) is deliberate — the cache repeats the
// series title on every episode row, and the aggregate must not depend on
// which row the planner visits first.
const hierarchySeriesAggregate = `
	SELECT c.series_id,
	       MIN(c.arr_instance_id), MIN(i.name),
	       MIN(c.title), COALESCE(MIN(c.year), 0), COALESCE(MIN(c.media_folder_path), ''),
	       COUNT(DISTINCT c.season_number),
	       COUNT(*),
	       SUM(CASE WHEN c.target_file_path IS NOT NULL AND c.target_file_path <> ''
	                THEN 1 ELSE 0 END),
	       CASE MIN(CASE c.eligibility_status
	                 WHEN 'waiting_for_tags' THEN 1
	                 WHEN 'missing_target' THEN 2
	                 ELSE 3 END)
	            WHEN 1 THEN 'waiting_for_tags'
	            WHEN 2 THEN 'missing_target'
	            ELSE 'eligible' END,
	       COALESCE(MAX(c.updated_at), '')
	FROM arr_library_cache c
	JOIN arr_instances i ON i.id = c.arr_instance_id`

// hierarchySeriesWhere renders the series filters. Search stays on the c alias
// so the grouped-count subquery needs no join; every episode row of a series
// carries the same title, so a WHERE match is the same set as matching the
// aggregate.
func hierarchySeriesWhere(f HierarchyFilter) (string, []any) {
	clauses := []string{"c.media_type = 'episode'", "c.series_id IS NOT NULL"}
	var args []any
	if f.InstanceID > 0 {
		clauses = append(clauses, "c.arr_instance_id = ?")
		args = append(args, f.InstanceID)
	}
	if term := strings.TrimSpace(f.Q); term != "" {
		clauses = append(clauses, `c.title LIKE ?`+likeEscapeSQL)
		args = append(args, likeContainsPattern(term))
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// hierarchySeriesOrder orders the grouped rows; the tiebreaker is series_id so
// two series with the same title page deterministically.
func hierarchySeriesOrder(f HierarchyFilter) string {
	dir := "ASC"
	if f.Dir == "desc" {
		dir = "DESC"
	}
	switch f.Sort {
	case "year":
		return " ORDER BY COALESCE(MIN(c.year), 0) " + dir +
			", MIN(c.title) COLLATE NOCASE ASC, c.series_id ASC"
	case "updated":
		return " ORDER BY COALESCE(MAX(c.updated_at), '') " + dir + ", c.series_id " + dir
	default:
		return " ORDER BY MIN(c.title) COLLATE NOCASE " + dir + ", c.series_id " + dir
	}
}

// hierarchySeriesScan reads the hierarchySeriesAggregate projection from
// either *sql.Rows or *sql.Row.
func hierarchySeriesScan(scan func(...any) error) (SeriesRow, error) {
	var row SeriesRow
	err := scan(&row.SeriesID, &row.ArrInstanceID, &row.InstanceName, &row.Title,
		&row.Year, &row.MediaFolderPath, &row.SeasonCount, &row.EpisodeCount,
		&row.EpisodeFileCount, &row.Eligibility, &row.UpdatedAt)
	return row, err
}
