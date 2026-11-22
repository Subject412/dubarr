package store

import (
	"context"
	"fmt"
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
