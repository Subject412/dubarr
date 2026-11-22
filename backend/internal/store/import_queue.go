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

// SetImportCandidateGate records the gate classification for sourcePath,
// preserving any arr parse payload already stored in parsed_raw_json.
//
// It reports an error when the candidate is unknown rather than silently doing
// nothing: a classification that does not reach the queue is a wiring bug.
func (s *Store) SetImportCandidateGate(ctx context.Context, sourcePath string, status ImportGateStatus) error {
	var raw sql.NullString
	err := s.DB.QueryRowContext(ctx,
		`SELECT parsed_raw_json FROM import_queue WHERE source_file_path = ?`, sourcePath).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: import candidate %s not found", sourcePath)
	}
	if err != nil {
		return fmt.Errorf("store: read import candidate %s: %w", sourcePath, err)
	}

	doc := map[string]any{}
	if raw.Valid && strings.TrimSpace(raw.String) != "" {
		if err := json.Unmarshal([]byte(raw.String), &doc); err != nil {
			return fmt.Errorf("store: import candidate %s: parsed_raw_json is not a JSON object: %w", sourcePath, err)
		}
	}
	doc[gateJSONKey] = status
	encoded, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("store: encode import candidate gate: %w", err)
	}

	res, err := s.DB.ExecContext(ctx,
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
