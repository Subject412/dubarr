// M6-1 library sync persistence: the diff-safe upsert of arr_library_cache and
// the arr_instances rows the cache points at.
//
// The sync matches rows on (arr_instance_id, external_item_id, media_type), so
// a resync updates the row an operator already matched to a donor instead of
// replacing it: ids are stable for the lifetime of the arr item, and only rows
// that actually changed are written. Items that vanished from an instance's
// snapshot are deleted. Since migration 0006 the database enforces that
// identity with a unique index, and every write of a key goes through the
// upsert in insertLibraryCacheRow, so a stale read cannot create a twin row.
//
// Deletion is safe because no foreign key forbids it: import_queue and
// sync_jobs reference arr_library_cache with ON DELETE SET NULL, so a vanished
// arr item only unlinks a manual match (the donor row itself and its status
// survive). Deleting is the honest option — a target that arr no longer knows
// about must not stay eligible for muxing.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Subject412/dubarr/backend/internal/logs"
)

// ErrInvalidLibraryItem reports a cache item the schema would reject (unknown
// media type, missing external id). It is returned before any write, so a bad
// snapshot cannot half-apply.
var ErrInvalidLibraryItem = errors.New("store: invalid arr library cache item")

// LibraryCacheItem is one arr item as the sync wants it cached. Optional fields
// map to NULL when empty: SeasonNumber/EpisodeNumber are nil for movies,
// SeriesID/TMDBID/TVDBID/TargetFilePath are empty for items arr has no file
// for yet.
type LibraryCacheItem struct {
	// MediaType is "movie" (Radarr) or "episode" (Sonarr).
	MediaType string
	// ExternalItemID is the Radarr movieId or Sonarr episodeId.
	ExternalItemID int64
	// SeriesID is the Sonarr seriesId (0 for movies).
	SeriesID int64
	TMDBID   int64
	TVDBID   int64
	Title    string
	// OriginalTitle is arr's originalTitle when it differs; empty otherwise.
	OriginalTitle string
	// OriginalLanguageISO2 is the normalized ISO 639-1 code from arr.
	OriginalLanguageISO2 string
	Year                 int
	SeasonNumber         *int
	EpisodeNumber        *int
	EpisodeTitle         string
	// MediaFolderPath is the arr folder (movie.path / series.path).
	MediaFolderPath string
	// RootFolderPath is the canonical arr root folder containing
	// MediaFolderPath (ADR-0009); empty when no configured root matches.
	RootFolderPath string
	// ArrTagIDs/ArrTagLabels are the item's arr tag snapshot, paired by
	// index. Episodes carry their series' tags. Each sync replaces the pair,
	// so removed tags disappear instead of accumulating. Legacy rows decode
	// as empty.
	ArrTagIDs    []int64
	ArrTagLabels []string
	// TargetFilePath is the resolved on-disk video, empty when arr has none.
	TargetFilePath string
	// MatchesTargetTags is the Processing Gate verdict for the target filename.
	MatchesTargetTags bool
	// EligibilityStatus is "eligible", "waiting_for_tags" or "missing_target".
	EligibilityStatus string
}

// LibrarySyncStats reports one instance's diff result.
type LibrarySyncStats struct {
	Inserted  int
	Updated   int
	Unchanged int
	Deleted   int
}

// Add accumulates another instance's stats.
func (s *LibrarySyncStats) Add(other LibrarySyncStats) {
	s.Inserted += other.Inserted
	s.Updated += other.Updated
	s.Unchanged += other.Unchanged
	s.Deleted += other.Deleted
}

// ArrInstanceSyncRow is the arr_instances projection the library sync reads and
// writes. APIKeyEnc is the AES-256-GCM blob (never plaintext).
type ArrInstanceSyncRow struct {
	ID          int64
	Name        string
	Kind        string
	BaseURL     string
	APIKeyEnc   []byte
	APIKeyLast4 string
	Enabled     bool
}

// ArrInstanceByKind finds the instance row for one *arr application. The
// appliance keeps at most one instance per kind (settings arr.connections), so
// kind is the sync identity.
func (s *Store) ArrInstanceByKind(ctx context.Context, kind string) (ArrInstanceSyncRow, bool, error) {
	var row ArrInstanceSyncRow
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, name, type, base_url, api_key_enc, api_key_last4, enabled
		 FROM arr_instances WHERE type = ? ORDER BY id LIMIT 1`, kind).
		Scan(&row.ID, &row.Name, &row.Kind, &row.BaseURL, &row.APIKeyEnc, &row.APIKeyLast4, &row.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return ArrInstanceSyncRow{}, false, nil
	}
	if err != nil {
		return ArrInstanceSyncRow{}, false, fmt.Errorf("store: read arr instance %q: %w", kind, err)
	}
	return row, true, nil
}

// SaveArrInstance inserts a row when ID is 0 and updates it by id otherwise,
// returning the persisted id. The update never touches last_sync_at; use
// TouchArrInstanceSync for that.
func (s *Store) SaveArrInstance(ctx context.Context, row ArrInstanceSyncRow) (int64, error) {
	if row.Kind != "radarr" && row.Kind != "sonarr" {
		return 0, fmt.Errorf("store: arr instance kind %q is not radarr or sonarr", row.Kind)
	}
	if len(row.APIKeyEnc) == 0 {
		return 0, errors.New("store: arr instance needs an encrypted API key")
	}
	if row.ID <= 0 {
		res, err := s.DB.ExecContext(ctx,
			`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4, enabled)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			row.Name, row.Kind, row.BaseURL, row.APIKeyEnc, row.APIKeyLast4, row.Enabled)
		if err != nil {
			return 0, fmt.Errorf("store: insert arr instance %q: %w", row.Kind, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return 0, fmt.Errorf("store: arr instance id: %w", err)
		}
		return id, nil
	}
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE arr_instances
		 SET name = ?, base_url = ?, api_key_enc = ?, api_key_last4 = ?, enabled = ?
		 WHERE id = ?`,
		row.Name, row.BaseURL, row.APIKeyEnc, row.APIKeyLast4, row.Enabled, row.ID); err != nil {
		return 0, fmt.Errorf("store: update arr instance %d: %w", row.ID, err)
	}
	return row.ID, nil
}

// SyncHealthCauseLimit caps how much of a failure cause is stored in
// arr_instances.last_sync_error. A pathological error (a dumped response body,
// a stack trace) must not bloat the row or the Library payload.
const SyncHealthCauseLimit = 500

// TouchArrInstanceSync records a completed, successful sync pass on the
// instance: last_sync_at (last success) moves, last_sync_ok becomes 1,
// last_sync_error is cleared and last_attempt_at is stamped.
func (s *Store) TouchArrInstanceSync(ctx context.Context, id int64) error {
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE arr_instances
		 SET last_sync_at = ?, last_sync_ok = 1, last_sync_error = NULL, last_attempt_at = ?
		 WHERE id = ?`,
		now, now, id); err != nil {
		return fmt.Errorf("store: touch arr instance %d: %w", id, err)
	}
	return nil
}

// RecordArrInstanceSyncFailure records a failed sync attempt without touching
// the last-good snapshot: last_sync_ok becomes 0, last_sync_error carries the
// cause and last_attempt_at is stamped, while last_sync_at (the last SUCCESS)
// and every arr_library_cache row stay as they were. Without this a failing
// pass is indistinguishable from no pass at all (migration 0005).
//
// The cause must already be free of credentials: the arr clients scrub the API
// key from their errors, and callers pass err.Error() from those. As a second
// line of defence the text is run through logs.Redact and capped at
// SyncHealthCauseLimit bytes, so a stray "api_key=..." or an enormous driver
// message cannot reach the row.
func (s *Store) RecordArrInstanceSyncFailure(ctx context.Context, id int64, cause string) error {
	if id <= 0 {
		return errors.New("store: sync failure needs a positive arr instance id")
	}
	msg := truncateCause(logs.Redact(strings.TrimSpace(cause)))
	if msg == "" {
		msg = "sync failed"
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE arr_instances
		 SET last_sync_ok = 0, last_sync_error = ?, last_attempt_at = ?
		 WHERE id = ?`,
		msg, now, id); err != nil {
		return fmt.Errorf("store: record arr instance %d sync failure: %w", id, err)
	}
	return nil
}

// truncateCause caps cause at SyncHealthCauseLimit bytes without splitting a
// multi-byte rune.
func truncateCause(cause string) string {
	if len(cause) <= SyncHealthCauseLimit {
		return cause
	}
	cut := SyncHealthCauseLimit
	for cut > 0 && !utf8.RuneStart(cause[cut]) {
		cut--
	}
	return cause[:cut]
}

// ArrInstanceByID finds one instance row by its id, with the same projection
// ArrInstanceByKind returns. It is the lookup a row-scoped action uses, since
// arr_library_cache points at an instance by id rather than by kind.
func (s *Store) ArrInstanceByID(ctx context.Context, id int64) (ArrInstanceSyncRow, bool, error) {
	var row ArrInstanceSyncRow
	if id <= 0 {
		return ArrInstanceSyncRow{}, false, nil
	}
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, name, type, base_url, api_key_enc, api_key_last4, enabled
		 FROM arr_instances WHERE id = ?`, id).
		Scan(&row.ID, &row.Name, &row.Kind, &row.BaseURL, &row.APIKeyEnc, &row.APIKeyLast4, &row.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return ArrInstanceSyncRow{}, false, nil
	}
	if err != nil {
		return ArrInstanceSyncRow{}, false, fmt.Errorf("store: read arr instance %d: %w", id, err)
	}
	return row, true, nil
}

// SyncLibraryCache applies one complete library snapshot for one instance.
//
// The whole snapshot is written in a single transaction: either the cache
// matches arr or it keeps its previous contents. Rows are matched on
// (arr_instance_id, external_item_id, media_type); unchanged rows keep their id
// and updated_at so content-change history stays intact, while every row the
// pass observed gets last_seen_at stamped (observation, not change).
func (s *Store) SyncLibraryCache(ctx context.Context, instanceID int64, items []LibraryCacheItem) (LibrarySyncStats, error) {
	var stats LibrarySyncStats
	if instanceID <= 0 {
		return stats, errors.New("store: library sync needs an arr instance id")
	}

	existing, err := s.libraryCacheRows(ctx, instanceID)
	if err != nil {
		return LibrarySyncStats{}, err
	}
	seen := make(map[libraryKey]struct{}, len(items))
	validated := make([]LibraryCacheItem, 0, len(items))
	for _, item := range items {
		if err := validateLibraryItem(item); err != nil {
			return LibrarySyncStats{}, err
		}
		item = normalizeArrTagSnapshot(item)
		key := libraryKey{mediaType: item.MediaType, externalID: item.ExternalItemID}
		if _, dup := seen[key]; dup {
			return LibrarySyncStats{}, fmt.Errorf("%w: duplicate %s %d in snapshot",
				ErrInvalidLibraryItem, item.MediaType, item.ExternalItemID)
		}
		seen[key] = struct{}{}
		validated = append(validated, item)
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return LibrarySyncStats{}, fmt.Errorf("store: begin library sync: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	for _, item := range validated {
		key := libraryKey{mediaType: item.MediaType, externalID: item.ExternalItemID}
		current, ok := existing[key]
		switch {
		case !ok:
			if err := insertLibraryCacheRow(ctx, tx, instanceID, item, now); err != nil {
				return LibrarySyncStats{}, err
			}
			stats.Inserted++
		case !libraryItemEqual(current, item):
			if err := updateLibraryCacheRow(ctx, tx, instanceID, current.ID, item, now); err != nil {
				return LibrarySyncStats{}, err
			}
			stats.Updated++
		default:
			// Unchanged rows keep their id and updated_at (content did not
			// change) but are still observed by this pass, so last_seen_at
			// moves. That is what tells a dropped-but-cached row apart from
			// one that was never seen.
			if err := stampLibrarySeen(ctx, tx, current.ID, now); err != nil {
				return LibrarySyncStats{}, err
			}
			stats.Unchanged++
		}
	}

	for key, row := range existing {
		if _, keep := seen[key]; keep {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM arr_library_cache WHERE id = ?`, row.ID); err != nil {
			return LibrarySyncStats{}, fmt.Errorf("store: delete vanished library row %d: %w", row.ID, err)
		}
		stats.Deleted++
	}

	if err := tx.Commit(); err != nil {
		return LibrarySyncStats{}, fmt.Errorf("store: commit library sync: %w", err)
	}
	return stats, nil
}

// UpdateLibraryCacheItem upserts exactly ONE row of arr_library_cache, matched
// on (arr_instance_id, external_item_id, media_type), and never deletes or
// touches any other row.
//
// This is the deliberate difference from SyncLibraryCache: that method applies a
// complete snapshot, so a row it did not see is treated as vanished from arr and
// DELETED. Here the caller observed one item (a per-item re-read after a
// rescan), and the rest of the instance's cache is simply not part of the
// observation — deleting it would wipe the library.
//
// Change tracking matches the snapshot sync: a row whose synced fields changed
// gets updated_at and last_seen_at stamped, an unchanged row only moves
// last_seen_at (so "when did this actually change" survives a no-op re-read).
func (s *Store) UpdateLibraryCacheItem(ctx context.Context, instanceID int64, item LibraryCacheItem) error {
	if instanceID <= 0 {
		return errors.New("store: library item update needs an arr instance id")
	}
	if err := validateLibraryItem(item); err != nil {
		return err
	}
	item = normalizeArrTagSnapshot(item)

	current, ok, err := s.libraryCacheRowByKey(ctx, instanceID, item.MediaType, item.ExternalItemID)
	if err != nil {
		return err
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin library item update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	switch {
	case !ok:
		if err := insertLibraryCacheRow(ctx, tx, instanceID, item, now); err != nil {
			return err
		}
	case !libraryItemEqual(current, item):
		if err := updateLibraryCacheRow(ctx, tx, instanceID, current.ID, item, now); err != nil {
			return err
		}
	default:
		if err := stampLibrarySeen(ctx, tx, current.ID, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit library item update: %w", err)
	}
	return nil
}

// libraryKey is the cache identity: arr instance is implied by the query scope.
type libraryKey struct {
	mediaType  string
	externalID int64
}

// libraryCacheRow is one stored row read back for diffing.
type libraryCacheRow struct {
	ID int64
	LibraryCacheItem
}

func (s *Store) libraryCacheRows(ctx context.Context, instanceID int64) (map[libraryKey]libraryCacheRow, error) {
	rows, err := s.DB.QueryContext(ctx, libraryCacheRowColumns+` WHERE arr_instance_id = ?`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("store: read library cache: %w", err)
	}
	defer rows.Close()

	out := make(map[libraryKey]libraryCacheRow)
	for rows.Next() {
		row, err := scanLibraryCacheRow(rows)
		if err != nil {
			return nil, err
		}
		out[libraryKey{mediaType: row.MediaType, externalID: row.ExternalItemID}] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate library cache: %w", err)
	}
	return out, nil
}

// libraryCacheRowByKey reads the ONE row with this composite identity. A single
// item re-read does not need the instance's whole cache, and on a library of
// 100k+ rows the instance-wide scan is pure waste: the lookup goes straight to
// the key the update is matched on.
func (s *Store) libraryCacheRowByKey(ctx context.Context, instanceID int64, mediaType string, externalID int64) (libraryCacheRow, bool, error) {
	rows, err := s.DB.QueryContext(ctx, libraryCacheRowColumns+
		` WHERE arr_instance_id = ? AND external_item_id = ? AND media_type = ?`,
		instanceID, externalID, mediaType)
	if err != nil {
		return libraryCacheRow{}, false, fmt.Errorf("store: read library cache row: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return libraryCacheRow{}, false, fmt.Errorf("store: read library cache row: %w", err)
		}
		return libraryCacheRow{}, false, nil
	}
	row, err := scanLibraryCacheRow(rows)
	if err != nil {
		return libraryCacheRow{}, false, err
	}
	return row, true, nil
}

// libraryCacheRowColumns is the diff projection. The instance-wide scan and the
// single-key lookup share it, so the two can never compare against different
// data.
const libraryCacheRowColumns = `SELECT id, media_type, external_item_id, COALESCE(series_id, 0),
	        COALESCE(tmdb_id, 0), COALESCE(tvdb_id, 0), title,
	        COALESCE(original_title, ''), COALESCE(original_language_iso2, ''),
	        COALESCE(year, 0), season_number, episode_number,
	        COALESCE(episode_title, ''), COALESCE(media_folder_path, ''),
	        COALESCE(target_file_path, ''), matches_target_tags, eligibility_status,
	        COALESCE(arr_tag_ids_json, '[]'), COALESCE(arr_tag_labels_json, '[]'),
	        COALESCE(root_folder_path, '')
	 FROM arr_library_cache`

// rowScanner is what *sql.Rows and *sql.Row both implement.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanLibraryCacheRow reads one row of libraryCacheRowColumns.
func scanLibraryCacheRow(row rowScanner) (libraryCacheRow, error) {
	var (
		out      libraryCacheRow
		season   sql.NullInt64
		episode  sql.NullInt64
		media    string
		external int64
		idsJSON  string
		lblJSON  string
	)
	if err := row.Scan(&out.ID, &media, &external, &out.SeriesID, &out.TMDBID, &out.TVDBID,
		&out.Title, &out.OriginalTitle, &out.OriginalLanguageISO2, &out.Year,
		&season, &episode, &out.EpisodeTitle, &out.MediaFolderPath, &out.TargetFilePath,
		&out.MatchesTargetTags, &out.EligibilityStatus, &idsJSON, &lblJSON,
		&out.RootFolderPath); err != nil {
		return libraryCacheRow{}, fmt.Errorf("store: scan library cache row: %w", err)
	}
	if err := decodeArrTagSnapshot(idsJSON, lblJSON, &out.LibraryCacheItem); err != nil {
		return libraryCacheRow{}, err
	}
	out.MediaType = media
	out.ExternalItemID = external
	if season.Valid {
		v := int(season.Int64)
		out.SeasonNumber = &v
	}
	if episode.Valid {
		v := int(episode.Int64)
		out.EpisodeNumber = &v
	}
	return out, nil
}

// insertLibraryCacheRow writes one cache row for the composite identity
// (arr_instance_id, media_type, external_item_id).
//
// It is an upsert, not a plain INSERT, because both callers can reach here
// holding a read that is already stale: SyncLibraryCache inserts every key its
// earlier snapshot read did not contain, and the rescan's zero-row fallback in
// updateLibraryCacheRow inserts when the row it read was deleted before its
// UPDATE landed. Since migration 0006 the database owns the identity, so the
// honest outcome for such a write is "this key now holds what I just observed":
// ON CONFLICT rewrites the surviving row instead of failing the whole
// transaction. Without it the collision would surface as a 500 on a rescan, and
// before 0006 it would have created the permanent duplicate the index now
// forbids.
//
// The conflict target covers the key columns, which are never rewritten; every
// other synced column plus updated_at/last_seen_at is overwritten exactly like
// updateLibraryCacheRow does. Columns the sync does not own (imdb_id,
// audio_languages_json, video_codec, raw_metadata_json) keep their values.
func insertLibraryCacheRow(ctx context.Context, tx *sql.Tx, instanceID int64, item LibraryCacheItem, now string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, series_id, tmdb_id, tvdb_id,
		  title, original_title, original_language_iso2, year, season_number, episode_number,
		  episode_title, media_folder_path, root_folder_path, target_file_path,
		  arr_tag_ids_json, arr_tag_labels_json, matches_target_tags,
		  eligibility_status, updated_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(arr_instance_id, media_type, external_item_id) DO UPDATE SET
		     series_id = excluded.series_id,
		     tmdb_id = excluded.tmdb_id,
		     tvdb_id = excluded.tvdb_id,
		     title = excluded.title,
		     original_title = excluded.original_title,
		     original_language_iso2 = excluded.original_language_iso2,
		     year = excluded.year,
		     season_number = excluded.season_number,
		     episode_number = excluded.episode_number,
		     episode_title = excluded.episode_title,
		     media_folder_path = excluded.media_folder_path,
		     root_folder_path = excluded.root_folder_path,
		     target_file_path = excluded.target_file_path,
		     arr_tag_ids_json = excluded.arr_tag_ids_json,
		     arr_tag_labels_json = excluded.arr_tag_labels_json,
		     matches_target_tags = excluded.matches_target_tags,
		     eligibility_status = excluded.eligibility_status,
		     updated_at = excluded.updated_at,
		     last_seen_at = excluded.last_seen_at`,
		instanceID, item.MediaType, item.ExternalItemID, nullInt(item.SeriesID),
		nullInt(item.TMDBID), nullInt(item.TVDBID), item.Title, nullStr(item.OriginalTitle),
		nullStr(item.OriginalLanguageISO2), nullInt(int64(item.Year)),
		nullIntPtr(item.SeasonNumber), nullIntPtr(item.EpisodeNumber),
		nullStr(item.EpisodeTitle), nullStr(item.MediaFolderPath), nullStr(item.RootFolderPath),
		nullStr(item.TargetFilePath), encodeArrTagIDs(item.ArrTagIDs),
		encodeArrTagLabels(item.ArrTagLabels),
		item.MatchesTargetTags, item.EligibilityStatus, now, now); err != nil {
		return fmt.Errorf("store: insert library row %s %d: %w", item.MediaType, item.ExternalItemID, err)
	}
	return nil
}

// updateLibraryCacheRow overwrites the synced fields of one row and stamps it as
// changed.
//
// An UPDATE that matches no row is not success: the poller can delete the row
// between the caller's read and this write (a snapshot pass no longer saw the
// item), and a silently discarded observation would leave the route reporting
// "reread":"ok" having written nothing. When zero rows matched, the row is
// inserted instead, so the caller's observation always lands.
func updateLibraryCacheRow(ctx context.Context, tx *sql.Tx, instanceID, id int64, item LibraryCacheItem, now string) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE arr_library_cache
		 SET series_id = ?, tmdb_id = ?, tvdb_id = ?, title = ?, original_title = ?,
		     original_language_iso2 = ?, year = ?, season_number = ?, episode_number = ?,
		     episode_title = ?, media_folder_path = ?, root_folder_path = ?, target_file_path = ?,
		     arr_tag_ids_json = ?, arr_tag_labels_json = ?,
		     matches_target_tags = ?, eligibility_status = ?, updated_at = ?, last_seen_at = ?
		 WHERE id = ?`,
		nullInt(item.SeriesID), nullInt(item.TMDBID), nullInt(item.TVDBID), item.Title,
		nullStr(item.OriginalTitle), nullStr(item.OriginalLanguageISO2), nullInt(int64(item.Year)),
		nullIntPtr(item.SeasonNumber), nullIntPtr(item.EpisodeNumber),
		nullStr(item.EpisodeTitle), nullStr(item.MediaFolderPath), nullStr(item.RootFolderPath),
		nullStr(item.TargetFilePath), encodeArrTagIDs(item.ArrTagIDs),
		encodeArrTagLabels(item.ArrTagLabels),
		item.MatchesTargetTags, item.EligibilityStatus, now, now, id)
	if err != nil {
		return fmt.Errorf("store: update library row %d: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update library row %d rows affected: %w", id, err)
	}
	if affected == 0 {
		// The row vanished between the read and the write (a sync pass
		// deleted it, or it was never there): upsert rather than reporting
		// a success that changed nothing. The upsert matters because the
		// table's unique identity (migration 0006) may already be held by
		// a row this write never saw, e.g. one the poller inserted after
		// it deleted the old row.
		return insertLibraryCacheRow(ctx, tx, instanceID, item, now)
	}
	return nil
}

// stampLibrarySeen records that a pass observed an unchanged row. Only
// last_seen_at moves: updated_at tracks content changes, so rewriting it here
// would erase the "when did this row actually change" signal.
func stampLibrarySeen(ctx context.Context, tx *sql.Tx, id int64, now string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE arr_library_cache SET last_seen_at = ? WHERE id = ?`, now, id); err != nil {
		return fmt.Errorf("store: stamp library row %d last_seen_at: %w", id, err)
	}
	return nil
}

// validateLibraryItem rejects rows the schema CHECK constraints would refuse, so
// the caller sees a typed error instead of a half-applied transaction.
func validateLibraryItem(item LibraryCacheItem) error {
	switch item.MediaType {
	case "movie", "episode":
	default:
		return fmt.Errorf("%w: media_type %q must be movie or episode", ErrInvalidLibraryItem, item.MediaType)
	}
	if item.ExternalItemID <= 0 {
		return fmt.Errorf("%w: %s item needs a positive external_item_id", ErrInvalidLibraryItem, item.MediaType)
	}
	if strings.TrimSpace(item.Title) == "" {
		return fmt.Errorf("%w: %s %d has an empty title", ErrInvalidLibraryItem, item.MediaType, item.ExternalItemID)
	}
	switch item.EligibilityStatus {
	case "eligible", "waiting_for_tags", "missing_target":
	default:
		return fmt.Errorf("%w: eligibility_status %q is not a gate verdict",
			ErrInvalidLibraryItem, item.EligibilityStatus)
	}
	if len(item.ArrTagIDs) != len(item.ArrTagLabels) {
		return fmt.Errorf("%w: %s %d has %d arr tag ids but %d labels",
			ErrInvalidLibraryItem, item.MediaType, item.ExternalItemID,
			len(item.ArrTagIDs), len(item.ArrTagLabels))
	}
	return nil
}

// normalizeArrTagSnapshot canonicalises the tag snapshot: pairs sorted by id
// and duplicate ids dropped. A stable order keeps the diff from rewriting rows
// every pass, while the pairing between id and label is preserved.
func normalizeArrTagSnapshot(item LibraryCacheItem) LibraryCacheItem {
	if len(item.ArrTagIDs) == 0 {
		item.ArrTagIDs = nil
		item.ArrTagLabels = nil
		return item
	}
	type pair struct {
		id    int64
		label string
	}
	pairs := make([]pair, len(item.ArrTagIDs))
	for i := range item.ArrTagIDs {
		pairs[i] = pair{id: item.ArrTagIDs[i], label: item.ArrTagLabels[i]}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].id < pairs[j].id })

	ids := make([]int64, 0, len(pairs))
	labels := make([]string, 0, len(pairs))
	for i, p := range pairs {
		if i > 0 && p.id == pairs[i-1].id {
			continue
		}
		ids = append(ids, p.id)
		labels = append(labels, p.label)
	}
	item.ArrTagIDs = ids
	item.ArrTagLabels = labels
	return item
}

// encodeArrTagIDs renders the id snapshot as JSON for arr_tag_ids_json.
// Marshalling a []int64 cannot fail, so an error can only mean a future type
// change; the empty array is the safe fallback.
func encodeArrTagIDs(ids []int64) string {
	if len(ids) == 0 {
		return "[]"
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// encodeArrTagLabels renders the label snapshot as JSON. Labels are sorted by
// their id (see normalizeArrTagSnapshot), so JSON order is stable.
func encodeArrTagLabels(labels []string) string {
	if len(labels) == 0 {
		return "[]"
	}
	b, err := json.Marshal(labels)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// decodeArrTagSnapshot reads the JSON columns back. Legacy rows carry '[]' via
// the column default, so they decode as empty metadata; malformed JSON is
// reported instead of silently treated as "untagged" (which could change
// eligibility once ADR-0009 is wired into the gate).
func decodeArrTagSnapshot(idsJSON, labelsJSON string, out *LibraryCacheItem) error {
	out.ArrTagIDs = []int64{}
	out.ArrTagLabels = []string{}
	if idsJSON != "" {
		if err := json.Unmarshal([]byte(idsJSON), &out.ArrTagIDs); err != nil {
			return fmt.Errorf("store: decode arr tag ids %q: %w", idsJSON, err)
		}
	}
	if labelsJSON != "" {
		if err := json.Unmarshal([]byte(labelsJSON), &out.ArrTagLabels); err != nil {
			return fmt.Errorf("store: decode arr tag labels %q: %w", labelsJSON, err)
		}
	}
	return nil
}

// libraryItemEqual compares every synced field. updated_at, audio_languages_json
// and video_codec are deliberately not compared: the sync does not own them.
func libraryItemEqual(row libraryCacheRow, item LibraryCacheItem) bool {
	return row.MediaType == item.MediaType &&
		row.ExternalItemID == item.ExternalItemID &&
		row.SeriesID == item.SeriesID &&
		row.TMDBID == item.TMDBID &&
		row.TVDBID == item.TVDBID &&
		row.Title == item.Title &&
		row.OriginalTitle == item.OriginalTitle &&
		row.OriginalLanguageISO2 == item.OriginalLanguageISO2 &&
		row.Year == item.Year &&
		intPtrEqual(row.SeasonNumber, item.SeasonNumber) &&
		intPtrEqual(row.EpisodeNumber, item.EpisodeNumber) &&
		row.EpisodeTitle == item.EpisodeTitle &&
		row.MediaFolderPath == item.MediaFolderPath &&
		row.RootFolderPath == item.RootFolderPath &&
		slices.Equal(row.ArrTagIDs, item.ArrTagIDs) &&
		slices.Equal(row.ArrTagLabels, item.ArrTagLabels) &&
		row.TargetFilePath == item.TargetFilePath &&
		row.MatchesTargetTags == item.MatchesTargetTags &&
		row.EligibilityStatus == item.EligibilityStatus
}

func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullIntPtr(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullStr(v string) any {
	if v == "" {
		return nil
	}
	return v
}
