package store

import (
	"context"
	"testing"
)

func TestRecordImportCandidateDedupes(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	created, err := s.RecordImportCandidate(ctx, "/media/watch/Movie (2021).mkv", "Movie (2021).mkv", 1234)
	if err != nil {
		t.Fatalf("RecordImportCandidate: %v", err)
	}
	if !created {
		t.Fatal("first insert: created = false, want true")
	}

	created, err = s.RecordImportCandidate(ctx, "/media/watch/Movie (2021).mkv", "Movie (2021).mkv", 1234)
	if err != nil {
		t.Fatalf("RecordImportCandidate duplicate: %v", err)
	}
	if created {
		t.Error("duplicate insert: created = true, want false")
	}

	var count int
	var status, filename string
	var size int64
	if err := s.DB.QueryRow(
		`SELECT count(*) FROM import_queue WHERE source_file_path = ?`,
		"/media/watch/Movie (2021).mkv",
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("import_queue rows = %d, want 1", count)
	}
	if err := s.DB.QueryRow(
		`SELECT status, filename, file_size FROM import_queue WHERE source_file_path = ?`,
		"/media/watch/Movie (2021).mkv",
	).Scan(&status, &filename, &size); err != nil {
		t.Fatalf("select row: %v", err)
	}
	if status != "pending_approval" {
		t.Errorf("status = %q, want pending_approval", status)
	}
	if filename != "Movie (2021).mkv" || size != 1234 {
		t.Errorf("row = (%q, %d), want (Movie (2021).mkv, 1234)", filename, size)
	}
}
