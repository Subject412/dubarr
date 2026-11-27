// Vault reorganisation persistence (#47): the plan projection, the guarded path
// rewrite, the audit runs and the job-in-flight probe.
//
// Nothing here moves a file. The filesystem half lives in internal/vaultapi;
// this half owns the two statements that must be true together — rewrite
// audio_tracks.file_path and write the audit row — plus the read that builds a
// plan from what the vault already knows.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Audit vocabulary. The CHECK constraints in migration 0010 make both closed, so
// the UI translates a code and never parses prose.
const (
	OrganiseStatusApplied        = "applied"
	OrganiseStatusRolledBack     = "rolled_back"
	OrganiseStatusNeedsAttention = "needs_attention"

	OrganiseResultRenamed     = "renamed"
	OrganiseResultRenamedBack = "renamed_back"
	OrganiseResultFailed      = "failed"
	OrganiseResultReconciled  = "reconciled"
)

// Errors the reorganisation path maps onto an HTTP status.
var (
	// ErrTrackPathChanged means a guarded UPDATE matched no row: the row is no
	// longer the one the plan read, so the plan is stale.
	ErrTrackPathChanged = errors.New("store: track path changed since the plan was built")
	// ErrTrackPathNotFound means the track id does not exist.
	ErrTrackPathNotFound = errors.New("store: track not found")
)

const organiseTimeLayout = "2006-01-02 15:04:05"

// VaultTrackPlanRow is one archived track as the organiser sees it: the track
// row, its vault_media catalog row, and the episode title the arr mirror knows
// for (season, episode).
//
// EpisodeTitle is empty when the mirror has no row — a name is never invented
// from the release name (ADR-0014), so the token renders empty and the cleanup
// removes the separator it leaves behind.
type VaultTrackPlanRow struct {
	TrackID   int64
	MediaID   int64
	MediaType string
	Title     string
	Year      int
	Season    *int
	Episode   *int
	Variant   string
	Codec     string
	Channels  string
	LangISO2  string
	FilePath  string
	// MediaFolderPath is vault_media.vault_folder_path: the folder Dubarr
	// already owns, never re-derived from a template.
	MediaFolderPath string
	EpisodeTitle    string
}

// VaultTrackPlanRows returns every archived track in scope, ordered by track id
// so a plan renders the same table twice for the same input.
//
// mediaType is "movie", "series" or "all"; ids, when non-empty, narrows the
// scope to those tracks (the UI's row selection). An unknown media type is an
// error rather than "all": silently planning more files than asked for is the
// one mistake this feature must not make.
func (s *Store) VaultTrackPlanRows(ctx context.Context, mediaType string, ids []int64) ([]VaultTrackPlanRow, error) {
	if mediaType == "" {
		mediaType = "all"
	}
	switch mediaType {
	case "movie", "series", "all":
	default:
		return nil, fmt.Errorf("store: vault organise media type %q is not movie/series/all", mediaType)
	}

	var (
		sb   strings.Builder
		args []any
	)
	sb.WriteString(`
		SELECT t.id, m.id, m.media_type, m.title, COALESCE(m.year, 0),
		       t.season_number, t.episode_number, t.variant, t.codec, t.channels,
		       t.lang_iso2, t.file_path, m.vault_folder_path,
		       COALESCE((
		           SELECT c.episode_title
		             FROM arr_library_cache c
		            WHERE c.media_type = 'episode'
		              AND c.season_number = t.season_number
		              AND c.episode_number = t.episode_number
		              AND c.episode_title IS NOT NULL
		              AND c.episode_title <> ''
		              AND ((m.tvdb_id IS NOT NULL AND c.tvdb_id = m.tvdb_id)
		                OR (m.tmdb_id IS NOT NULL AND c.tmdb_id = m.tmdb_id))
		            ORDER BY c.id DESC
		            LIMIT 1), '') AS episode_title
		  FROM audio_tracks t
		  JOIN vault_media m ON m.id = t.vault_media_id`)
	if mediaType != "all" {
		sb.WriteString(` WHERE m.media_type = ?`)
		args = append(args, mediaType)
	}
	if len(ids) > 0 {
		if mediaType == "all" {
			sb.WriteString(` WHERE`)
		} else {
			sb.WriteString(` AND`)
		}
		sb.WriteString(` t.id IN (`)
		sb.WriteString(placeholders(len(ids)))
		sb.WriteString(`)`)
		for _, id := range ids {
			args = append(args, id)
		}
	}
	sb.WriteString(` ORDER BY t.id`)

	rows, err := s.DB.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("store: vault organise plan rows: %w", err)
	}
	defer rows.Close()

	out := make([]VaultTrackPlanRow, 0, len(ids))
	for rows.Next() {
		var (
			row             VaultTrackPlanRow
			season, episode sql.NullInt64
		)
		if err := rows.Scan(&row.TrackID, &row.MediaID, &row.MediaType, &row.Title,
			&row.Year, &season, &episode, &row.Variant, &row.Codec, &row.Channels,
			&row.LangISO2, &row.FilePath, &row.MediaFolderPath, &row.EpisodeTitle); err != nil {
			return nil, fmt.Errorf("store: scan vault organise plan row: %w", err)
		}
		if season.Valid {
			v := int(season.Int64)
			row.Season = &v
		}
		if episode.Valid {
			v := int(episode.Int64)
			row.Episode = &v
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate vault organise plan rows: %w", err)
	}
	return out, nil
}

// TrackPathUpdate is one guarded file_path rewrite: To applies only while the
// row still holds From.
type TrackPathUpdate struct {
	TrackID int64
	From    string
	To      string
}

// UpdateTrackPathsTx rewrites file_path for every update inside the caller's
// transaction and aborts on the first guard miss.
//
// The WHERE file_path = :source guard is the whole concurrency story: a second
// apply, or a row an operator changed between preview and apply, matches zero
// rows and the transaction is rolled back instead of overwriting a newer value.
func UpdateTrackPathsTx(ctx context.Context, tx *sql.Tx, updates []TrackPathUpdate) error {
	for _, u := range updates {
		res, err := tx.ExecContext(ctx,
			`UPDATE audio_tracks SET file_path = ? WHERE id = ? AND file_path = ?`,
			u.To, u.TrackID, u.From)
		if err != nil {
			return fmt.Errorf("store: rewrite track %d path: %w", u.TrackID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: rewrite track %d path: %w", u.TrackID, err)
		}
		if n != 1 {
			return fmt.Errorf("%w: track %d", ErrTrackPathChanged, u.TrackID)
		}
	}
	return nil
}

// TrackPath returns the current file_path of one track.
func (s *Store) TrackPath(ctx context.Context, trackID int64) (string, error) {
	var path string
	err := s.DB.QueryRowContext(ctx, `SELECT file_path FROM audio_tracks WHERE id = ?`, trackID).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %d", ErrTrackPathNotFound, trackID)
	}
	if err != nil {
		return "", fmt.Errorf("store: read track %d path: %w", trackID, err)
	}
	return path, nil
}

// OrganiseFileRecord is one audit file row.
type OrganiseFileRecord struct {
	TrackID         int64
	SourcePath      string
	DestinationPath string
	// Result is one of OrganiseResult{Renamed,RenamedBack,Failed,Reconciled}.
	Result string
}

// OrganiseRunRecord is one audit run plus the files it touched. Files are
// written in the same transaction as the run, so an apply's audit trail is
// either complete or absent — never half-written.
type OrganiseRunRecord struct {
	PlanHash  string
	FileCount int
	// Status is one of OrganiseStatus{Applied,RolledBack,NeedsAttention}.
	Status     string
	Actor      string
	Detail     string
	StartedAt  time.Time
	FinishedAt time.Time
	Files      []OrganiseFileRecord
}

// InsertOrganiseRunTx writes a run and its file rows inside the caller's
// transaction. Phase C of an apply uses this so the path rewrites and their
// audit rows commit together.
func InsertOrganiseRunTx(ctx context.Context, tx *sql.Tx, rec OrganiseRunRecord) (int64, error) {
	started := rec.StartedAt
	if started.IsZero() {
		started = time.Now()
	}
	finished := rec.FinishedAt
	if finished.IsZero() {
		finished = time.Now()
	}
	if !validOrganiseStatus(rec.Status) {
		return 0, fmt.Errorf("store: organise run status %q is not applied/rolled_back/needs_attention", rec.Status)
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO vault_organise_runs
		   (started_at, finished_at, status, plan_hash, file_count, actor, detail)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		started.UTC().Format(organiseTimeLayout),
		finished.UTC().Format(organiseTimeLayout),
		rec.Status, rec.PlanHash, rec.FileCount,
		nullableText(rec.Actor), nullableText(rec.Detail))
	if err != nil {
		return 0, fmt.Errorf("store: insert organise run: %w", err)
	}
	runID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: organise run id: %w", err)
	}
	for _, f := range rec.Files {
		if !validOrganiseResult(f.Result) {
			return 0, fmt.Errorf("store: organise file result %q is not renamed/renamed_back/failed/reconciled", f.Result)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO vault_organise_files
			   (run_id, track_id, source_path, destination_path, result)
			 VALUES (?, ?, ?, ?, ?)`,
			runID, nullableID(f.TrackID), f.SourcePath, f.DestinationPath, f.Result); err != nil {
			return 0, fmt.Errorf("store: insert organise file %s: %w", f.SourcePath, err)
		}
	}
	return runID, nil
}

// InsertOrganiseRun writes a run in its own transaction: the rolled_back and
// needs_attention outcomes are recorded after the failed core transaction has
// already been rolled back.
func (s *Store) InsertOrganiseRun(ctx context.Context, rec OrganiseRunRecord) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin organise run: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	runID, err := InsertOrganiseRunTx(ctx, tx, rec)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit organise run: %w", err)
	}
	return runID, nil
}

// OrganiseRunRow is one audit run as the history list serves it.
type OrganiseRunRow struct {
	ID         int64
	StartedAt  string
	FinishedAt string
	Status     string
	PlanHash   string
	FileCount  int
	Actor      string
	Detail     string
}

// ListOrganiseRuns returns the newest runs first, at most limit of them.
func (s *Store) ListOrganiseRuns(ctx context.Context, limit int) ([]OrganiseRunRow, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, CAST(started_at AS TEXT), COALESCE(CAST(finished_at AS TEXT), ''),
		       status, plan_hash, file_count, COALESCE(actor, ''), COALESCE(detail, '')
		  FROM vault_organise_runs
		 ORDER BY started_at DESC, id DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list organise runs: %w", err)
	}
	defer rows.Close()

	out := []OrganiseRunRow{}
	for rows.Next() {
		var row OrganiseRunRow
		if err := rows.Scan(&row.ID, &row.StartedAt, &row.FinishedAt, &row.Status,
			&row.PlanHash, &row.FileCount, &row.Actor, &row.Detail); err != nil {
			return nil, fmt.Errorf("store: scan organise run: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate organise runs: %w", err)
	}
	return out, nil
}

// OrganiseFileRow is one audited file of a run as the history read serves it.
type OrganiseFileRow struct {
	RunID           int64
	SourcePath      string
	DestinationPath string
	// Result is one of OrganiseResult{Failed,Reconciled}: the two outcomes a
	// human has to look at. renamed and renamed_back never reach this read.
	Result string
}

// ListOrganiseFilesForRuns returns, per run id, the audited files an operator
// has to inspect: exactly the reconciled and failed rows (#127).
//
// renamed and renamed_back are left out on purpose: a file that moved, or one
// that was put back, needs no inspection, and the run's file_count already says
// how many files the run touched. The filter is server-side so a caller cannot
// accidentally ship the whole audit table to the UI.
//
// Rows are ordered by run id then source path (then id, so two rows with the
// same path still come out in a stable order) — the same input renders the same
// list twice.
func (s *Store) ListOrganiseFilesForRuns(ctx context.Context, runIDs []int64) (map[int64][]OrganiseFileRow, error) {
	out := make(map[int64][]OrganiseFileRow, len(runIDs))
	ids := uniqueIDs(runIDs)
	if len(ids) == 0 {
		// No runs, no query: an empty IN () is a syntax error waiting to happen.
		return out, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT run_id, source_path, destination_path, result
		  FROM vault_organise_files
		 WHERE run_id IN (`+placeholders(len(ids))+`)
		   AND result IN ('reconciled', 'failed')
		 ORDER BY run_id, source_path, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list organise files: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var row OrganiseFileRow
		if err := rows.Scan(&row.RunID, &row.SourcePath, &row.DestinationPath, &row.Result); err != nil {
			return nil, fmt.Errorf("store: scan organise file: %w", err)
		}
		out[row.RunID] = append(out[row.RunID], row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate organise files: %w", err)
	}
	return out, nil
}

// JobsInFlight counts sync_mux_jobs rows that reference any of trackIDs.
//
// Referencing counts a job whose target or reference track is planned: those
// reference tracks by id, so a path rewrite does not break them — the number is
// reported so the operator is not surprised. Blocking counts only analyzing and
// muxing, because those are the two states that cannot be running safely while
// the vault moves; held/queued/ready_for_review cannot be mid-mux by
// definition (#48).
type JobsInFlight struct {
	Referencing int
	Blocking    int
}

// JobsInFlight counts the jobs that reference the given tracks.
func (s *Store) JobsInFlight(ctx context.Context, trackIDs []int64) (JobsInFlight, error) {
	var out JobsInFlight
	ids := uniqueIDs(trackIDs)
	if len(ids) == 0 {
		return out, nil
	}
	// Chunked so a 20k-track plan never approaches SQLite's variable limit.
	const chunk = 300
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		part := ids[start:end]
		args := make([]any, 0, len(part)*2)
		for _, id := range part {
			args = append(args, id)
		}
		for _, id := range part {
			args = append(args, id)
		}
		list := placeholders(len(part))
		var referencing, blocking int
		err := s.DB.QueryRowContext(ctx, `
			SELECT COUNT(*),
			       COALESCE(SUM(CASE WHEN status IN ('analyzing', 'muxing') THEN 1 ELSE 0 END), 0)
			  FROM sync_mux_jobs
			 WHERE target_track_id IN (`+list+`) OR ref_track_id IN (`+list+`)`, args...).
			Scan(&referencing, &blocking)
		if err != nil {
			return out, fmt.Errorf("store: count jobs in flight: %w", err)
		}
		out.Referencing += referencing
		out.Blocking += blocking
	}
	return out, nil
}

func validOrganiseStatus(status string) bool {
	switch status {
	case OrganiseStatusApplied, OrganiseStatusRolledBack, OrganiseStatusNeedsAttention:
		return true
	}
	return false
}

func validOrganiseResult(result string) bool {
	switch result {
	case OrganiseResultRenamed, OrganiseResultRenamedBack, OrganiseResultFailed, OrganiseResultReconciled:
		return true
	}
	return false
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func uniqueIDs(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
