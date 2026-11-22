package store

import (
	"context"
	"errors"
	"sort"
	"testing"
)

// extractionFixture opens a store and seeds import_queue rows with explicit
// statuses, returning the store and the row id of each seeded path. Rows are
// inserted in sorted path order, so ids are deterministic.
func extractionFixture(t *testing.T, statuses map[string]string) (*Store, context.Context, map[string]int64) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	paths := make([]string, 0, len(statuses))
	for path := range statuses {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	ids := make(map[string]int64, len(statuses))
	for _, path := range paths {
		status := statuses[path]
		created, err := s.RecordImportCandidate(ctx, path, path, 1024)
		if err != nil || !created {
			t.Fatalf("RecordImportCandidate(%q): created=%v err=%v", path, created, err)
		}
		if _, err := s.DB.ExecContext(ctx,
			`UPDATE import_queue SET status = ? WHERE source_file_path = ?`, status, path); err != nil {
			t.Fatalf("set status %q: %v", status, err)
		}
		var id int64
		if err := s.DB.QueryRowContext(ctx,
			`SELECT id FROM import_queue WHERE source_file_path = ?`, path).Scan(&id); err != nil {
			t.Fatalf("read id: %v", err)
		}
		ids[path] = id
	}
	return s, ctx, ids
}

// queueRow reads the persisted status and error_message for a queue id.
func queueRow(t *testing.T, s *Store, ctx context.Context, id int64) (status, message string) {
	t.Helper()
	if err := s.DB.QueryRowContext(ctx,
		`SELECT status, COALESCE(error_message, '') FROM import_queue WHERE id = ?`, id).
		Scan(&status, &message); err != nil {
		t.Fatalf("read row %d: %v", id, err)
	}
	return status, message
}

func TestSetImportCandidateDonor(t *testing.T) {
	s, ctx, ids := extractionFixture(t, map[string]string{"/w/candidate.mkv": "pending_approval"})
	id := ids["/w/candidate.mkv"]
	donor := "/w/Minions PL.DUB 1080p.mkv"

	if err := s.SetImportCandidateDonor(ctx, id, donor); err != nil {
		t.Fatalf("SetImportCandidateDonor: %v", err)
	}
	if status, _ := queueRow(t, s, ctx, id); status != "extracting" {
		t.Fatalf("status = %q, want extracting", status)
	}
	job, ok, err := s.NextImportExtraction(ctx)
	if err != nil || !ok {
		t.Fatalf("NextImportExtraction = (ok=%v, err=%v), want a job", ok, err)
	}
	if job.DonorPath != donor {
		t.Errorf("DonorPath = %q, want %q", job.DonorPath, donor)
	}

	// The transition is one-shot: a row that already left pending_approval
	// cannot be re-paired.
	if err := s.SetImportCandidateDonor(ctx, id, "/w/other.mkv"); !errors.Is(err, ErrImportCandidateState) {
		t.Errorf("second SetImportCandidateDonor error = %v, want ErrImportCandidateState", err)
	}
	if err := s.SetImportCandidateDonor(ctx, 4242, donor); !errors.Is(err, ErrImportCandidateNotFound) {
		t.Errorf("unknown id error = %v, want ErrImportCandidateNotFound", err)
	}
	if err := s.SetImportCandidateDonor(ctx, id, "  "); err == nil {
		t.Error("empty donor path accepted")
	}
}

// TestSetImportCandidateDonorPreservesGate pins the additive JSON contract: the
// donor member is written next to the existing gate verdict.
func TestSetImportCandidateDonorPreservesGate(t *testing.T) {
	s, ctx, ids := extractionFixture(t, map[string]string{"/w/candidate.mkv": "pending_approval"})
	id := ids["/w/candidate.mkv"]
	if err := s.SetImportCandidateGate(ctx, "/w/candidate.mkv", ImportGateStatus{Status: "eligible", TargetPath: "/media/target.mkv"}); err != nil {
		t.Fatalf("SetImportCandidateGate: %v", err)
	}
	if err := s.SetImportCandidateDonor(ctx, id, "/w/donor PL.DUB.mkv"); err != nil {
		t.Fatalf("SetImportCandidateDonor: %v", err)
	}
	gate, ok, err := s.ImportCandidateGate(ctx, "/w/candidate.mkv")
	if err != nil || !ok {
		t.Fatalf("ImportCandidateGate = (ok=%v, err=%v), want the preserved verdict", ok, err)
	}
	if gate.Status != "eligible" || gate.TargetPath != "/media/target.mkv" {
		t.Errorf("gate = %+v, want eligible for /media/target.mkv", gate)
	}
}

func TestNextImportExtraction(t *testing.T) {
	s, ctx, ids := extractionFixture(t, map[string]string{
		"/w/pending.mkv":     "pending_approval",
		"/w/first.mkv":       "extracting",
		"/w/second.mkv":      "extracting",
		"/w/done.mkv":        "completed",
		"/w/dead-letter.mkv": "failed",
	})

	tests := []struct {
		name string
		// drain marks the selected row completed so the next call advances.
		wantID       int64
		wantFilename string
		wantOK       bool
	}{
		{name: "oldest extracting row first", wantID: ids["/w/first.mkv"], wantFilename: "/w/first.mkv", wantOK: true},
		{name: "next extracting row after drain", wantID: ids["/w/second.mkv"], wantFilename: "/w/second.mkv", wantOK: true},
		{name: "empty queue reports nothing", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job, ok, err := s.NextImportExtraction(ctx)
			if err != nil {
				t.Fatalf("NextImportExtraction: %v", err)
			}
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				if job != (ImportExtractionJob{}) {
					t.Errorf("job = %+v, want the zero value when nothing is queued", job)
				}
				return
			}
			if job.ID != tt.wantID || job.SourceFilePath != tt.wantFilename {
				t.Errorf("job = {ID:%d path:%q}, want {ID:%d path:%q}",
					job.ID, job.SourceFilePath, tt.wantID, tt.wantFilename)
			}
			if job.FileSize != 1024 {
				t.Errorf("FileSize = %d, want 1024", job.FileSize)
			}
			if job.SelectedExports != "[]" {
				t.Errorf("SelectedExports = %q, want the schema default []", job.SelectedExports)
			}
			// Drain the row so the next case sees the following one.
			if err := s.FinishImportExtraction(ctx, job.ID, "completed", ""); err != nil {
				t.Fatalf("drain row %d: %v", job.ID, err)
			}
		})
	}
}

func TestClaimImportExtraction(t *testing.T) {
	tests := []struct {
		name        string
		status      string
		wantClaimed bool
	}{
		{name: "extracting row is claimable", status: "extracting", wantClaimed: true},
		{name: "pending approval row is not claimable", status: "pending_approval", wantClaimed: false},
		{name: "completed row is not claimable", status: "completed", wantClaimed: false},
		{name: "failed row is not claimable", status: "failed", wantClaimed: false},
		{name: "ignored row is not claimable", status: "ignored", wantClaimed: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, ctx, ids := extractionFixture(t, map[string]string{"/w/donor.mkv": tt.status})
			id := ids["/w/donor.mkv"]

			claimed, err := s.ClaimImportExtraction(ctx, id)
			if err != nil {
				t.Fatalf("ClaimImportExtraction: %v", err)
			}
			if claimed != tt.wantClaimed {
				t.Fatalf("claimed = %v, want %v", claimed, tt.wantClaimed)
			}
			if status, _ := queueRow(t, s, ctx, id); status != tt.status {
				t.Errorf("status = %q after claim, want %q (claim must not change state)", status, tt.status)
			}
		})
	}

	t.Run("unknown id reports not claimed", func(t *testing.T) {
		s, ctx, _ := extractionFixture(t, nil)
		claimed, err := s.ClaimImportExtraction(ctx, 4242)
		if err != nil {
			t.Fatalf("ClaimImportExtraction: %v", err)
		}
		if claimed {
			t.Error("claimed = true for an unknown id")
		}
	})
}

// TestClaimImportExtractionExclusivity pins the double-claim contract: once the
// first worker finishes the row it leaves the extracting state, so a second
// claim gets nothing and cannot overwrite the completed outcome.
func TestClaimImportExtractionExclusivity(t *testing.T) {
	s, ctx, ids := extractionFixture(t, map[string]string{"/w/donor.mkv": "extracting"})
	id := ids["/w/donor.mkv"]

	claimed, err := s.ClaimImportExtraction(ctx, id)
	if err != nil || !claimed {
		t.Fatalf("first claim = (%v, %v), want (true, nil)", claimed, err)
	}
	if err := s.FinishImportExtraction(ctx, id, "completed", ""); err != nil {
		t.Fatalf("FinishImportExtraction: %v", err)
	}

	claimed, err = s.ClaimImportExtraction(ctx, id)
	if err != nil {
		t.Fatalf("second ClaimImportExtraction: %v", err)
	}
	if claimed {
		t.Fatal("second claim = true for a row that already left extracting")
	}
	if status, message := queueRow(t, s, ctx, id); status != "completed" || message != "" {
		t.Errorf("row = %q/%q after losing claim, want completed with no error", status, message)
	}
}

func TestFinishImportExtraction(t *testing.T) {
	tests := []struct {
		name        string
		fromStatus  string
		finish      string
		message     string
		wantErr     error
		wantStatus  string
		wantMessage string
	}{
		{
			name: "completed from extracting", fromStatus: "extracting",
			finish: "completed", wantStatus: "completed",
		},
		{
			name: "failed from extracting stores reason", fromStatus: "extracting",
			finish: "failed", message: "mkvextract: boom",
			wantStatus: "failed", wantMessage: "mkvextract: boom",
		},
		{
			name: "pending approval cannot be finished", fromStatus: "pending_approval",
			finish: "completed", wantErr: ErrImportExtractionState, wantStatus: "pending_approval",
		},
		{
			name: "already completed row is a state conflict", fromStatus: "completed",
			finish: "failed", wantErr: ErrImportExtractionState, wantStatus: "completed",
		},
		{
			name: "already failed row is a state conflict", fromStatus: "failed",
			finish: "completed", wantErr: ErrImportExtractionState, wantStatus: "failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, ctx, ids := extractionFixture(t, map[string]string{"/w/donor.mkv": tt.fromStatus})
			id := ids["/w/donor.mkv"]

			err := s.FinishImportExtraction(ctx, id, tt.finish, tt.message)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("FinishImportExtraction: %v", err)
				}
			} else if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			status, message := queueRow(t, s, ctx, id)
			if status != tt.wantStatus || message != tt.wantMessage {
				t.Errorf("row = %q/%q, want %q/%q", status, message, tt.wantStatus, tt.wantMessage)
			}
		})
	}

	t.Run("unsupported status is rejected before touching the row", func(t *testing.T) {
		s, ctx, ids := extractionFixture(t, map[string]string{"/w/donor.mkv": "extracting"})
		id := ids["/w/donor.mkv"]
		for _, status := range []string{"", "extracting", "scanning", "ignored"} {
			if err := s.FinishImportExtraction(ctx, id, status, "x"); err == nil {
				t.Errorf("FinishImportExtraction(%q) = nil, want an unsupported-status error", status)
			}
		}
		if status, _ := queueRow(t, s, ctx, id); status != "extracting" {
			t.Errorf("status = %q after rejected finishes, want extracting", status)
		}
	})

	t.Run("unknown id is not found", func(t *testing.T) {
		s, ctx, _ := extractionFixture(t, nil)
		err := s.FinishImportExtraction(ctx, 4242, "completed", "")
		if !errors.Is(err, ErrImportCandidateNotFound) {
			t.Fatalf("error = %v, want ErrImportCandidateNotFound", err)
		}
	})

	t.Run("second finish is a state conflict and keeps the first outcome", func(t *testing.T) {
		s, ctx, ids := extractionFixture(t, map[string]string{"/w/donor.mkv": "extracting"})
		id := ids["/w/donor.mkv"]
		if err := s.FinishImportExtraction(ctx, id, "completed", ""); err != nil {
			t.Fatalf("first finish: %v", err)
		}
		err := s.FinishImportExtraction(ctx, id, "failed", "late failure")
		if !errors.Is(err, ErrImportExtractionState) {
			t.Fatalf("second finish error = %v, want ErrImportExtractionState", err)
		}
		if status, message := queueRow(t, s, ctx, id); status != "completed" || message != "" {
			t.Errorf("row = %q/%q after second finish, want the first outcome (completed, no error)", status, message)
		}
	})
}
