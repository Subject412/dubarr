package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// libraryFixture seeds an arr instance plus cache rows and returns the ids in
// insertion order.
func libraryFixture(t *testing.T, s *Store, ctx context.Context, titles ...string) []int64 {
	t.Helper()
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		 VALUES ('main', 'radarr', 'http://radarr.invalid', x'00', '0000')`)
	if err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	instanceID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("instance id: %v", err)
	}
	ids := make([]int64, 0, len(titles))
	for i, title := range titles {
		res, err := s.DB.ExecContext(ctx,
			`INSERT INTO arr_library_cache
			 (arr_instance_id, media_type, external_item_id, title, original_language_iso2,
			  year, target_file_path, audio_languages_json, eligibility_status)
			 VALUES (?, 'movie', ?, ?, 'en', ?, ?, '["en"]', 'eligible')`,
			instanceID, i+1, title, 2000+i, fmt.Sprintf("/media/%d.mkv", i+1))
		if err != nil {
			t.Fatalf("insert library row %q: %v", title, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("library id: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestListLibrarySearchMatchesTitleSubstring(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	ids := libraryFixture(t, s, ctx, "Kraina Lodu", "Shrek", "Lodowy Król")

	got, err := s.ListLibrarySearch(ctx, "lod")
	if err != nil {
		t.Fatalf("ListLibrarySearch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("results = %d, want 2: %+v", len(got), got)
	}
	// Ordered by title (case-insensitive): Kraina Lodu before Lodowy Król.
	if got[0].ID != ids[0] || got[0].Title != "Kraina Lodu" {
		t.Errorf("first result = %+v, want id %d Kraina Lodu", got[0], ids[0])
	}
	if got[0].Year != 2000 || got[0].MediaType != "movie" || got[0].OriginalLanguage != "en" {
		t.Errorf("result metadata = %+v", got[0])
	}

	none, err := s.ListLibrarySearch(ctx, "no such title")
	if err != nil {
		t.Fatalf("ListLibrarySearch miss: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("miss results = %+v, want none", none)
	}

	empty, err := s.ListLibrarySearch(ctx, "   ")
	if err != nil {
		t.Fatalf("ListLibrarySearch empty: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty query results = %+v, want none", empty)
	}
}

func TestListLibrarySearchEscapesLikeWildcards(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	libraryFixture(t, s, ctx, "100% Wolf", "Kraina Lodu")

	got, err := s.ListLibrarySearch(ctx, "0%")
	if err != nil {
		t.Fatalf("ListLibrarySearch: %v", err)
	}
	if len(got) != 1 || got[0].Title != "100% Wolf" {
		t.Fatalf("results = %+v, want just 100%% Wolf", got)
	}
}

func TestListLibrarySearchLimitsToTwenty(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	titles := make([]string, 0, 25)
	for i := 0; i < 25; i++ {
		titles = append(titles, fmt.Sprintf("Match %02d", i))
	}
	libraryFixture(t, s, ctx, titles...)

	got, err := s.ListLibrarySearch(ctx, "match")
	if err != nil {
		t.Fatalf("ListLibrarySearch: %v", err)
	}
	if len(got) != LibrarySearchLimit {
		t.Fatalf("results = %d, want %d", len(got), LibrarySearchLimit)
	}
}

func TestSetImportCandidateApprovalRecordsMatch(t *testing.T) {
	s, ctx, ids := extractionFixture(t, map[string]string{"/w/candidate.mkv": "pending_approval"})
	id := ids["/w/candidate.mkv"]
	libraryID := libraryFixture(t, s, ctx, "Kraina Lodu")[0]
	if err := s.SetImportCandidateGate(ctx, "/w/candidate.mkv", ImportGateStatus{
		Status: "eligible", TargetPath: "/media/1.mkv",
	}); err != nil {
		t.Fatalf("SetImportCandidateGate: %v", err)
	}

	if err := s.SetImportCandidateApproval(ctx, id, "/w/donor PL.DUB.mkv", libraryID); err != nil {
		t.Fatalf("SetImportCandidateApproval: %v", err)
	}
	var status string
	var matchedID *int64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT status, matched_library_cache_id FROM import_queue WHERE id = ?`, id).
		Scan(&status, &matchedID); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != "extracting" {
		t.Errorf("status = %q, want extracting", status)
	}
	if matchedID == nil || *matchedID != libraryID {
		t.Errorf("matched_library_cache_id = %v, want %d", matchedID, libraryID)
	}
	job, ok, err := s.NextImportExtraction(ctx)
	if err != nil || !ok {
		t.Fatalf("NextImportExtraction = (ok=%v, err=%v)", ok, err)
	}
	if job.Title != "Kraina Lodu" || job.DonorPath != "/w/donor PL.DUB.mkv" {
		t.Errorf("extraction job = %+v", job)
	}
}

func TestSetImportCandidateApprovalRejectsBadMatch(t *testing.T) {
	s, ctx, ids := extractionFixture(t, map[string]string{
		"/w/a.mkv": "pending_approval",
		"/w/b.mkv": "pending_approval",
	})
	libraryIDs := libraryFixture(t, s, ctx, "Kraina Lodu", "Shrek")
	a, b := ids["/w/a.mkv"], ids["/w/b.mkv"]

	if err := s.SetImportCandidateApproval(ctx, a, "/w/donor PL.DUB.mkv", 99999); !errors.Is(err, ErrLibraryCacheNotFound) {
		t.Errorf("unknown library id error = %v, want ErrLibraryCacheNotFound", err)
	}
	if err := s.SetImportCandidateApproval(ctx, 4242, "/w/donor PL.DUB.mkv", libraryIDs[0]); !errors.Is(err, ErrImportCandidateNotFound) {
		t.Errorf("unknown candidate error = %v, want ErrImportCandidateNotFound", err)
	}

	// An already matched row only accepts its own library id.
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE import_queue SET matched_library_cache_id = ? WHERE id = ?`, libraryIDs[1], b); err != nil {
		t.Fatalf("pre-match row: %v", err)
	}
	if err := s.SetImportCandidateApproval(ctx, b, "/w/donor PL.DUB.mkv", libraryIDs[0]); !errors.Is(err, ErrImportCandidateMatchMismatch) {
		t.Errorf("mismatch error = %v, want ErrImportCandidateMatchMismatch", err)
	}
	if status, _ := queueRow(t, s, ctx, b); status != "pending_approval" {
		t.Errorf("status after rejected approve = %q, want pending_approval", status)
	}
}
