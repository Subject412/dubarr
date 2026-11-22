package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Errors reported by the approval-queue transitions.
var (
	// ErrImportCandidateNotFound reports an unknown import_queue id.
	ErrImportCandidateNotFound = errors.New("store: import candidate not found")
	// ErrImportCandidateState reports a transition requested from a status
	// other than pending_approval.
	ErrImportCandidateState = errors.New("store: import candidate is not pending approval")
	// ErrImportExtractionState reports an extraction result recorded for a row
	// that is no longer extracting (already finished or never approved).
	ErrImportExtractionState = errors.New("store: import candidate is not extracting")
	// ErrLibraryCacheNotFound reports a manual library match naming an
	// arr_library_cache row that does not exist.
	ErrLibraryCacheNotFound = errors.New("store: library cache target not found")
	// ErrImportCandidateMatchMismatch reports an approve that supplies a
	// library id different from the one already matched on the row.
	ErrImportCandidateMatchMismatch = errors.New("store: import candidate is matched to a different library target")
)

// RecordImportCandidate inserts a newly discovered donor file into the
// approval queue (IDEA.md §4 table 5) and reports whether the row was created.
//
// import_queue.source_file_path is UNIQUE, so the scanner can rescan the same
// tree every cycle: an already-known path is a no-op and created is false.
// Files start as pending_approval; probe results and user choices are filled in
// by later milestones.
func (s *Store) RecordImportCandidate(ctx context.Context, sourcePath, filename string, size int64) (created bool, err error) {
	res, err := s.DB.ExecContext(ctx,
		`INSERT OR IGNORE INTO import_queue (source_file_path, filename, file_size, status)
		 VALUES (?, ?, ?, 'pending_approval')`,
		sourcePath, filename, size)
	if err != nil {
		return false, fmt.Errorf("store: record import candidate: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: record import candidate rows affected: %w", err)
	}
	return n == 1, nil
}

// ImportGateStatus is the Tdarr Gate classification recorded for one import
// candidate (PLAN.md M2-5).
//
// import_queue has no dedicated gate column, and M2-5 is deliberately additive
// (no migration), so the classification is stored as a "gate" member inside the
// candidate's parsed_raw_json text column. A dedicated gate_status /
// target_file_path column pair is the proper shape once a migration is in
// scope; until then the JSON blob keeps the row self-describing without
// inventing a schema.
type ImportGateStatus struct {
	// Status is one of eligible, waiting_for_tags or missing_target.
	Status string `json:"status"`
	// TargetPath is the target file the gate inspected ("" when unknown).
	TargetPath string `json:"target_path"`
	// MatchedTags lists the required tags found on the target.
	MatchedTags []string `json:"matched_tags"`
	// MissingTags lists the required tags not found (waiting_for_tags).
	MissingTags []string `json:"missing_tags"`
	// CheckedAt is when the classification was made (UTC).
	CheckedAt time.Time `json:"checked_at"`
}

// gateJSONKey is the parsed_raw_json member holding ImportGateStatus. The
// underscore-free name keeps it distinct from arr /api/v3/parse payload keys.
const gateJSONKey = "gate"

// donorJSONKey is the parsed_raw_json member holding the donor file path
// selected at approval time. Like gateJSONKey it needs no migration.
const donorJSONKey = "donor"

// SetImportCandidateGate records the gate classification for sourcePath,
// preserving any arr parse payload already stored in parsed_raw_json.
//
// The payload is decoded as raw JSON members and the gate member is replaced by
// its raw encoding, so every other member — including integers beyond 2^53 that
// float64 would round — is preserved byte-exact. A JSON null payload is treated
// as an empty object.
//
// The read and the write run in one transaction so a concurrent classification
// cannot overwrite a gate decision made between the two statements.
//
// It reports an error when the candidate is unknown rather than silently doing
// nothing: a classification that does not reach the queue is a wiring bug.
func (s *Store) SetImportCandidateGate(ctx context.Context, sourcePath string, status ImportGateStatus) error {
	gateRaw, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("store: encode import candidate gate: %w", err)
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin import candidate gate: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	var raw sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT parsed_raw_json FROM import_queue WHERE source_file_path = ?`, sourcePath).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: import candidate %s not found", sourcePath)
	}
	if err != nil {
		return fmt.Errorf("store: read import candidate %s: %w", sourcePath, err)
	}

	doc := map[string]json.RawMessage{}
	if payload := strings.TrimSpace(raw.String); raw.Valid && payload != "" && payload != "null" {
		if err := json.Unmarshal([]byte(payload), &doc); err != nil {
			return fmt.Errorf("store: import candidate %s: parsed_raw_json is not a JSON object: %w", sourcePath, err)
		}
		if doc == nil {
			doc = map[string]json.RawMessage{}
		}
	}
	doc[gateJSONKey] = gateRaw
	encoded, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("store: encode import candidate gate: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE import_queue SET parsed_raw_json = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE source_file_path = ?`, string(encoded), sourcePath)
	if err != nil {
		return fmt.Errorf("store: record import candidate gate: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: record import candidate gate rows affected: %w", err)
	} else if n != 1 {
		return fmt.Errorf("store: import candidate %s not found", sourcePath)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit import candidate gate: %w", err)
	}
	return nil
}

// ImportCandidateGate returns the stored gate classification for sourcePath.
// ok is false when the candidate exists but has never been classified; an
// unknown candidate is also reported as ok=false with no error.
func (s *Store) ImportCandidateGate(ctx context.Context, sourcePath string) (ImportGateStatus, bool, error) {
	var raw sql.NullString
	err := s.DB.QueryRowContext(ctx,
		`SELECT parsed_raw_json FROM import_queue WHERE source_file_path = ?`, sourcePath).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportGateStatus{}, false, nil
	}
	if err != nil {
		return ImportGateStatus{}, false, fmt.Errorf("store: read import candidate %s: %w", sourcePath, err)
	}
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return ImportGateStatus{}, false, nil
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw.String), &doc); err != nil {
		return ImportGateStatus{}, false, fmt.Errorf("store: import candidate %s: parsed_raw_json is not a JSON object: %w", sourcePath, err)
	}
	blob, ok := doc[gateJSONKey]
	if !ok {
		return ImportGateStatus{}, false, nil
	}
	var status ImportGateStatus
	if err := json.Unmarshal(blob, &status); err != nil {
		return ImportGateStatus{}, false, fmt.Errorf("store: decode import candidate gate: %w", err)
	}
	return status, true, nil
}

// ImportCandidateTarget returns the target path paired with a pending_approval
// candidate: the gate verdict's target when one was recorded, otherwise the
// matched arr library row's target. An unknown id reports
// ErrImportCandidateNotFound and a row that already left pending_approval
// reports ErrImportCandidateState, so approve cannot pair against a stale row.
func (s *Store) ImportCandidateTarget(ctx context.Context, id int64) (string, error) {
	target, _, err := s.ImportCandidateMatchTarget(ctx, id)
	return target, err
}

// ImportCandidateMatchTarget is ImportCandidateTarget plus the candidate's
// matched_library_cache_id (0 when the row has no matched arr library row).
// Approve needs both: the path to pair the donor with and the identity the
// extraction→vault hand-off requires.
func (s *Store) ImportCandidateMatchTarget(ctx context.Context, id int64) (string, int64, error) {
	var (
		status      string
		raw         sql.NullString
		cacheTarget sql.NullString
		cacheID     sql.NullInt64
	)
	err := s.DB.QueryRowContext(ctx, `
		SELECT iq.status, iq.parsed_raw_json, alc.target_file_path, iq.matched_library_cache_id
		FROM import_queue iq
		LEFT JOIN arr_library_cache alc ON alc.id = iq.matched_library_cache_id
		WHERE iq.id = ?`, id).Scan(&status, &raw, &cacheTarget, &cacheID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, ErrImportCandidateNotFound
	}
	if err != nil {
		return "", 0, fmt.Errorf("store: read import candidate %d target: %w", id, err)
	}
	if status != "pending_approval" {
		return "", 0, ErrImportCandidateState
	}
	target := cacheTarget.String
	if payload := strings.TrimSpace(raw.String); raw.Valid && payload != "" && payload != "null" {
		var doc map[string]json.RawMessage
		if err := json.Unmarshal([]byte(payload), &doc); err != nil {
			return "", 0, fmt.Errorf("store: import candidate %d: parsed_raw_json is not a JSON object: %w", id, err)
		}
		if blob, ok := doc[gateJSONKey]; ok {
			var gate ImportGateStatus
			if err := json.Unmarshal(blob, &gate); err != nil {
				return "", 0, fmt.Errorf("store: decode import candidate %d gate: %w", id, err)
			}
			if gate.TargetPath != "" {
				target = gate.TargetPath
			}
		}
	}
	return target, cacheID.Int64, nil
}

// SetImportCandidateDonor persists the donor selection and moves the candidate
// from pending_approval to extracting in one transaction.
//
// The donor path is stored as a "donor" member inside parsed_raw_json (see
// gateJSONKey for why no migration is involved), so the extraction worker reads
// exactly the donor the approval card showed instead of falling back to the
// candidate file. The conditional status guard means a racing approve/reject
// cannot attach a donor to a row that already left pending_approval.
func (s *Store) SetImportCandidateDonor(ctx context.Context, id int64, donorPath string) error {
	return s.SetImportCandidateApproval(ctx, id, donorPath, 0)
}

// SetImportCandidateApproval is SetImportCandidateDonor plus an optional manual
// library match: when libraryCacheID is positive it is validated against
// arr_library_cache and persisted into import_queue.matched_library_cache_id in
// the same transaction as the donor and the status transition, so the
// extraction→vault hand-off always sees a media identity.
//
// A supplied id that contradicts a match already on the row is
// ErrImportCandidateMatchMismatch; an unknown id is ErrLibraryCacheNotFound.
// Both leave the row pending_approval.
func (s *Store) SetImportCandidateApproval(ctx context.Context, id int64, donorPath string, libraryCacheID int64) error {
	if strings.TrimSpace(donorPath) == "" {
		return fmt.Errorf("store: import candidate %d: donor path is empty", id)
	}
	if libraryCacheID < 0 {
		return fmt.Errorf("store: import candidate %d: library cache id must be positive", id)
	}
	donorRaw, err := json.Marshal(donorPath)
	if err != nil {
		return fmt.Errorf("store: encode import candidate donor: %w", err)
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin import candidate donor: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	var (
		raw       sql.NullString
		currentID sql.NullInt64
		status    string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT parsed_raw_json, matched_library_cache_id, status FROM import_queue WHERE id = ?`,
		id).Scan(&raw, &currentID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrImportCandidateNotFound
	}
	if err != nil {
		return fmt.Errorf("store: read import candidate %d: %w", id, err)
	}
	if status != "pending_approval" {
		return ErrImportCandidateState
	}

	var match any
	if libraryCacheID > 0 {
		var exists int
		err = tx.QueryRowContext(ctx,
			`SELECT 1 FROM arr_library_cache WHERE id = ?`, libraryCacheID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLibraryCacheNotFound
		}
		if err != nil {
			return fmt.Errorf("store: read library cache %d: %w", libraryCacheID, err)
		}
		if currentID.Valid && currentID.Int64 != libraryCacheID {
			return ErrImportCandidateMatchMismatch
		}
		match = libraryCacheID
	}

	doc := map[string]json.RawMessage{}
	if payload := strings.TrimSpace(raw.String); raw.Valid && payload != "" && payload != "null" {
		if err := json.Unmarshal([]byte(payload), &doc); err != nil {
			return fmt.Errorf("store: import candidate %d: parsed_raw_json is not a JSON object: %w", id, err)
		}
		if doc == nil {
			doc = map[string]json.RawMessage{}
		}
	}
	doc[donorJSONKey] = donorRaw
	encoded, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("store: encode import candidate donor: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE import_queue
		 SET parsed_raw_json = ?, matched_library_cache_id = COALESCE(?, matched_library_cache_id),
		     status = 'extracting', updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND status = 'pending_approval'`, string(encoded), match, id)
	if err != nil {
		return fmt.Errorf("store: record import candidate %d donor: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: record import candidate %d donor rows: %w", id, err)
	}
	if n != 1 {
		var current string
		qerr := tx.QueryRowContext(ctx, `SELECT status FROM import_queue WHERE id = ?`, id).Scan(&current)
		if errors.Is(qerr, sql.ErrNoRows) {
			return ErrImportCandidateNotFound
		}
		if qerr != nil {
			return fmt.Errorf("store: read import candidate %d: %w", id, qerr)
		}
		return ErrImportCandidateState
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit import candidate donor: %w", err)
	}
	return nil
}

// ImportCandidateSummary is one pending approval-queue row joined with the
// matched arr library target and the stored gate verdict. String fields are
// empty when the candidate has no match or classification yet.
type ImportCandidateSummary struct {
	ID               int64
	SourceFilePath   string
	Filename         string
	FileSize         int64
	Status           string
	Title            string
	MediaType        string
	Year             int
	SeasonNumber     *int
	EpisodeNumber    *int
	TargetPath       string
	OriginalLanguage string
	AudioLanguages   string
	Eligibility      string
	Gate             ImportGateStatus
	HasGate          bool
	// DonorPath is the donor persisted at approval time (empty for rows that
	// never stored a selection).
	DonorPath string
	// Matched reports whether the row is linked to an arr_library_cache row;
	// LibraryTitle is that row's title. An unmatched row needs a manual match
	// before approve can hand media identity to extraction.
	Matched      bool
	LibraryTitle string
}

// ListPendingImportCandidates returns every donor awaiting approval, newest
// queue id last, with the matched target and gate verdict attached.
//
// The gate verdict lives in parsed_raw_json (see SetImportCandidateGate); when a
// candidate was never classified, Eligibility falls back to the arr library
// row's eligibility_status, and finally to "missing_target".
func (s *Store) ListPendingImportCandidates(ctx context.Context) ([]ImportCandidateSummary, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT iq.id, iq.source_file_path, iq.filename, iq.file_size, iq.status,
		       iq.parsed_raw_json, iq.matched_library_cache_id,
		       alc.title, alc.media_type, alc.year, alc.season_number, alc.episode_number,
		       alc.target_file_path, alc.original_language_iso2,
		       alc.audio_languages_json, alc.eligibility_status
		FROM import_queue iq
		LEFT JOIN arr_library_cache alc ON alc.id = iq.matched_library_cache_id
		WHERE iq.status = 'pending_approval'
		ORDER BY iq.created_at, iq.id`)
	if err != nil {
		return nil, fmt.Errorf("store: list pending import candidates: %w", err)
	}
	defer rows.Close()

	out := []ImportCandidateSummary{}
	for rows.Next() {
		var (
			c                  ImportCandidateSummary
			raw                sql.NullString
			matchedID          sql.NullInt64
			title, mediaType   sql.NullString
			year               sql.NullInt64
			season, episode    sql.NullInt64
			target, origLang   sql.NullString
			audioLangs, eligib sql.NullString
		)
		if err := rows.Scan(&c.ID, &c.SourceFilePath, &c.Filename, &c.FileSize, &c.Status,
			&raw, &matchedID, &title, &mediaType, &year, &season, &episode,
			&target, &origLang, &audioLangs, &eligib); err != nil {
			return nil, fmt.Errorf("store: scan pending import candidate: %w", err)
		}
		c.Matched = matchedID.Valid
		c.LibraryTitle = title.String
		c.Title = title.String
		c.MediaType = mediaType.String
		c.Year = int(year.Int64)
		if season.Valid {
			v := int(season.Int64)
			c.SeasonNumber = &v
		}
		if episode.Valid {
			v := int(episode.Int64)
			c.EpisodeNumber = &v
		}
		c.TargetPath = target.String
		c.OriginalLanguage = origLang.String
		c.AudioLanguages = audioLangs.String
		c.Eligibility = eligib.String
		if raw.Valid && strings.TrimSpace(raw.String) != "" {
			var doc map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw.String), &doc); err != nil {
				return nil, fmt.Errorf("store: import candidate %d: parsed_raw_json is not an object: %w", c.ID, err)
			}
			if blob, ok := doc[gateJSONKey]; ok {
				if err := json.Unmarshal(blob, &c.Gate); err != nil {
					return nil, fmt.Errorf("store: decode import candidate %d gate: %w", c.ID, err)
				}
				c.HasGate = true
				if c.Gate.TargetPath != "" {
					c.TargetPath = c.Gate.TargetPath
				}
				if c.Gate.Status != "" {
					c.Eligibility = c.Gate.Status
				}
			}
			if blob, ok := doc[donorJSONKey]; ok {
				if err := json.Unmarshal(blob, &c.DonorPath); err != nil {
					return nil, fmt.Errorf("store: decode import candidate %d donor: %w", c.ID, err)
				}
			}
		}
		if c.Eligibility == "" {
			c.Eligibility = "missing_target"
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate pending import candidates: %w", err)
	}
	return out, nil
}

// TransitionImportCandidate moves a pending_approval candidate to status to.
//
// M2 has no extraction worker: approve marks the row extracting (M3 consumes
// that state) and reject marks it ignored. Any other source status is a
// conflict, so a double click cannot silently overwrite a completed row.
func (s *Store) TransitionImportCandidate(ctx context.Context, id int64, to string) error {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE import_queue SET status = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND status = 'pending_approval'`, to, id)
	if err != nil {
		return fmt.Errorf("store: transition import candidate %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: transition import candidate %d rows: %w", id, err)
	}
	if n == 1 {
		return nil
	}

	var current string
	err = s.DB.QueryRowContext(ctx, `SELECT status FROM import_queue WHERE id = ?`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrImportCandidateNotFound
	}
	if err != nil {
		return fmt.Errorf("store: read import candidate %d: %w", id, err)
	}
	return ErrImportCandidateState
}

// ImportExtractionJob is the extraction worker's view of an approved row: the
// donor file to extract from and the arr original language used to build the
// export policy. SelectedExportsJSON is the persisted approval choice (empty
// in M2; the worker falls back to the export policy when it is empty).
//
// Match metadata is joined from arr_library_cache so the vault hand-off can
// name and catalogue the published track; those fields are empty/zero when the
// candidate is unmatched.
type ImportExtractionJob struct {
	ID               int64
	SourceFilePath   string
	Filename         string
	FileSize         int64
	SelectedExports  string
	OriginalLanguage string
	// DonorPath is the donor file persisted by the approval step. The worker
	// extracts from it; an empty value is a row that was never paired and must
	// not fall back to SourceFilePath.
	DonorPath string
	// Match metadata from arr_library_cache (media_type is "movie" or
	// "episode"; empty when the donor has no matched target).
	Title         string
	MediaType     string
	Year          int
	TMDBID        int64
	TVDBID        int64
	SeasonNumber  *int
	EpisodeNumber *int
}

// NextImportExtraction returns the oldest row awaiting extraction, or ok=false
// when no row is in the extracting state. The worker consumes rows one at a
// time (concurrency 1), so a single-row read is enough.
func (s *Store) NextImportExtraction(ctx context.Context) (ImportExtractionJob, bool, error) {
	var (
		job                               ImportExtractionJob
		orig, title, mediaType, raw       sql.NullString
		year, tmdb, tvdb, season, episode sql.NullInt64
	)
	err := s.DB.QueryRowContext(ctx, `
		SELECT iq.id, iq.source_file_path, iq.filename, iq.file_size,
		       iq.selected_exports_json, iq.parsed_raw_json, alc.original_language_iso2,
		       alc.title, alc.media_type, alc.year, alc.tmdb_id, alc.tvdb_id,
		       alc.season_number, alc.episode_number
		FROM import_queue iq
		LEFT JOIN arr_library_cache alc ON alc.id = iq.matched_library_cache_id
		WHERE iq.status = 'extracting'
		ORDER BY iq.created_at, iq.id
		LIMIT 1`).Scan(&job.ID, &job.SourceFilePath, &job.Filename, &job.FileSize,
		&job.SelectedExports, &raw, &orig, &title, &mediaType, &year, &tmdb, &tvdb,
		&season, &episode)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportExtractionJob{}, false, nil
	}
	if err != nil {
		return ImportExtractionJob{}, false, fmt.Errorf("store: next import extraction: %w", err)
	}
	if payload := strings.TrimSpace(raw.String); raw.Valid && payload != "" && payload != "null" {
		var doc map[string]json.RawMessage
		if err := json.Unmarshal([]byte(payload), &doc); err != nil {
			return ImportExtractionJob{}, false, fmt.Errorf("store: next import extraction %d: parsed_raw_json is not a JSON object: %w", job.ID, err)
		}
		if blob, ok := doc[donorJSONKey]; ok {
			if err := json.Unmarshal(blob, &job.DonorPath); err != nil {
				return ImportExtractionJob{}, false, fmt.Errorf("store: next import extraction %d: decode donor: %w", job.ID, err)
			}
		}
	}
	job.OriginalLanguage = orig.String
	job.Title = title.String
	job.MediaType = mediaType.String
	job.Year = int(year.Int64)
	job.TMDBID = tmdb.Int64
	job.TVDBID = tvdb.Int64
	if season.Valid {
		v := int(season.Int64)
		job.SeasonNumber = &v
	}
	if episode.Valid {
		v := int(episode.Int64)
		job.EpisodeNumber = &v
	}
	return job, true, nil
}

// ClaimImportExtraction atomically claims an extracting row for this worker.
//
// M3-1 is additive and migration-free, and import_queue's status CHECK only
// permits the M1 status vocabulary, so no separate 'processing' state can be
// introduced. The claim is therefore a conditional touch of the extracting row:
// the UPDATE only succeeds while the status is still 'extracting', which gives
// one writer exclusive ownership for the lifetime of the claim. A crash leaves
// the row extracting, and the next boot re-processes it after reaping its
// staging directory — the desired crash-safe behaviour for concurrency 1.
func (s *Store) ClaimImportExtraction(ctx context.Context, id int64) (bool, error) {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE import_queue SET status = 'extracting', updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND status = 'extracting'`, id)
	if err != nil {
		return false, fmt.Errorf("store: claim import extraction %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: claim import extraction %d rows: %w", id, err)
	}
	return n == 1, nil
}

// FinishImportExtraction records the extraction outcome for an extracting row.
//
// status is 'completed' when the validated output is staged in WORK_DIR for
// M3-2 (the status CHECK has no 'extracted' member and M3-1 adds no migration,
// so 'completed' carries "extracted, awaiting vault" until M3-2 advances it)
// and 'failed' when the row is dead-lettered with errorMessage. Recording a
// result for a row that is no longer extracting is a state conflict.
func (s *Store) FinishImportExtraction(ctx context.Context, id int64, status, errorMessage string) error {
	if status != "completed" && status != "failed" {
		return fmt.Errorf("store: finish import extraction %d: unsupported status %q", id, status)
	}
	res, err := s.DB.ExecContext(ctx,
		`UPDATE import_queue SET status = ?, error_message = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND status = 'extracting'`, status, errorMessage, id)
	if err != nil {
		return fmt.Errorf("store: finish import extraction %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: finish import extraction %d rows: %w", id, err)
	}
	if n == 1 {
		return nil
	}
	var current string
	err = s.DB.QueryRowContext(ctx, `SELECT status FROM import_queue WHERE id = ?`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrImportCandidateNotFound
	}
	if err != nil {
		return fmt.Errorf("store: read import candidate %d: %w", id, err)
	}
	return fmt.Errorf("%w (status %q)", ErrImportExtractionState, current)
}
