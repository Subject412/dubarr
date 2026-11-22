package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
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

func TestImportCandidateGateRoundTrip(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	const source = "/media/watch/Movie (2021).mkv"
	if _, err := s.RecordImportCandidate(ctx, source, "Movie (2021).mkv", 1234); err != nil {
		t.Fatalf("RecordImportCandidate: %v", err)
	}

	checked := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	want := ImportGateStatus{
		Status:      "waiting_for_tags",
		TargetPath:  "/media/movies/Movie (2021)/Movie (2021).mkv",
		MatchedTags: []string{},
		MissingTags: []string{"[ArcA310]"},
		CheckedAt:   checked,
	}
	if err := s.SetImportCandidateGate(ctx, source, want); err != nil {
		t.Fatalf("SetImportCandidateGate: %v", err)
	}

	got, ok, err := s.ImportCandidateGate(ctx, source)
	if err != nil {
		t.Fatalf("ImportCandidateGate: %v", err)
	}
	if !ok {
		t.Fatal("ImportCandidateGate: ok = false, want true")
	}
	if got.Status != want.Status || got.TargetPath != want.TargetPath || !got.CheckedAt.Equal(checked) {
		t.Errorf("gate = %+v, want %+v", got, want)
	}
	if len(got.MissingTags) != 1 || got.MissingTags[0] != "[ArcA310]" {
		t.Errorf("missing tags = %v, want [[ArcA310]]", got.MissingTags)
	}
}

func TestImportCandidateGateMergesParsedJSON(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	const source = "/media/watch/Shrek.mkv"
	if _, err := s.RecordImportCandidate(ctx, source, "Shrek.mkv", 10); err != nil {
		t.Fatalf("RecordImportCandidate: %v", err)
	}
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE import_queue SET parsed_raw_json = ? WHERE source_file_path = ?`,
		`{"title":"Shrek","year":2001}`, source); err != nil {
		t.Fatalf("seed parsed_raw_json: %v", err)
	}

	if err := s.SetImportCandidateGate(ctx, source, ImportGateStatus{Status: "eligible", TargetPath: "/media/Shrek.mkv"}); err != nil {
		t.Fatalf("SetImportCandidateGate: %v", err)
	}

	var raw string
	if err := s.DB.QueryRowContext(ctx,
		`SELECT parsed_raw_json FROM import_queue WHERE source_file_path = ?`, source).Scan(&raw); err != nil {
		t.Fatalf("select parsed_raw_json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("parsed_raw_json is not an object: %v (%s)", err, raw)
	}
	if doc["title"] != "Shrek" {
		t.Errorf("parse data lost: %v", doc)
	}
	if _, ok := doc["gate"]; !ok {
		t.Errorf("gate key missing: %v", doc)
	}
}

func TestImportCandidateGateUnknownCandidate(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	if err := s.SetImportCandidateGate(ctx, "/nope.mkv", ImportGateStatus{Status: "eligible"}); err == nil {
		t.Fatal("SetImportCandidateGate for an unknown candidate returned no error")
	}
	if _, ok, err := s.ImportCandidateGate(ctx, "/nope.mkv"); err != nil {
		t.Fatalf("ImportCandidateGate: %v", err)
	} else if ok {
		t.Fatal("ImportCandidateGate: ok = true for an unknown candidate")
	}
}

