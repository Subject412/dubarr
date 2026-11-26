package store

import (
	"context"
	"errors"
	"testing"
)

// libraryItemFixture seeds one instance with three movie cache rows and returns
// the store, the context and the instance id.
func libraryItemFixture(t *testing.T) (*Store, context.Context, int64) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	instanceID := libraryInstance(t, s, ctx, "Radarr", "radarr")
	libraryRow(t, s, ctx, instanceID, "movie", 1, "Shrek", "Shrek", "eligible")
	libraryRow(t, s, ctx, instanceID, "movie", 2, "Shrek 2", "", "eligible")
	libraryRow(t, s, ctx, instanceID, "movie", 3, "Arrival", "Przybycie", "eligible")
	return s, ctx, instanceID
}

// TestLibraryItemByIDFoundAndMissing pins the single-row read: the same
// projection ListLibrary returns, and ErrLibraryItemNotFound for a row that does
// not exist (rather than a zero row).
func TestLibraryItemByIDFoundAndMissing(t *testing.T) {
	s, ctx, instanceID := libraryItemFixture(t)

	page, err := s.ListLibrary(ctx, ListLibraryFilter{Q: "Arrival"})
	if err != nil {
		t.Fatalf("ListLibrary: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("filtered rows = %d, want 1", len(page.Items))
	}
	want := page.Items[0]

	got, err := s.LibraryItemByID(ctx, want.ID)
	if err != nil {
		t.Fatalf("LibraryItemByID(%d): %v", want.ID, err)
	}
	if got.ID != want.ID || got.Title != "Arrival" || got.OriginalTitle != "Przybycie" {
		t.Errorf("item = %+v, want the Arrival row", got)
	}
	if got.MediaType != "movie" || got.InstanceType != "radarr" ||
		got.ArrInstanceID != instanceID || got.InstanceName != "Radarr" {
		t.Errorf("item identity = %+v", got)
	}
	if got.EligibilityStatus != "eligible" || !got.MatchesTargetTags {
		t.Errorf("eligibility = %q matches=%v", got.EligibilityStatus, got.MatchesTargetTags)
	}
	if got.TargetFilePath != "/media/x/f.mkv" || got.MediaFolderPath != "/media/x" {
		t.Errorf("paths = %q/%q", got.TargetFilePath, got.MediaFolderPath)
	}
	if got.LastSeenAt != want.LastSeenAt || got.UpdatedAt != want.UpdatedAt {
		t.Errorf("timestamps %q/%q differ from the page read %q/%q",
			got.LastSeenAt, got.UpdatedAt, want.LastSeenAt, want.UpdatedAt)
	}

	if _, err := s.LibraryItemByID(ctx, want.ID+999); !errors.Is(err, ErrLibraryItemNotFound) {
		t.Fatalf("LibraryItemByID(missing) error = %v, want ErrLibraryItemNotFound", err)
	}
	if _, err := s.LibraryItemByID(ctx, 0); !errors.Is(err, ErrLibraryItemNotFound) {
		t.Fatalf("LibraryItemByID(0) error = %v, want ErrLibraryItemNotFound", err)
	}
}

// TestUpdateLibraryCacheItemDoesNotDeleteOthers is the load-bearing test against
// snapshot semantics: updating one row must leave the rows the caller never
// observed untouched (SyncLibraryCache would delete them).
func TestUpdateLibraryCacheItemDoesNotDeleteOthers(t *testing.T) {
	s, ctx, instanceID := libraryItemFixture(t)

	if err := s.UpdateLibraryCacheItem(ctx, instanceID, LibraryCacheItem{
		MediaType:         "movie",
		ExternalItemID:    2,
		Title:             "Shrek 2 (rescan)",
		Year:              2004,
		MediaFolderPath:   "/media/movies/Shrek 2",
		RootFolderPath:    "/media/movies",
		TargetFilePath:    "/media/movies/Shrek 2/Shrek.2.mkv",
		MatchesTargetTags: true,
		EligibilityStatus: "eligible",
	}); err != nil {
		t.Fatalf("UpdateLibraryCacheItem: %v", err)
	}

	page, err := s.ListLibrary(ctx, ListLibraryFilter{})
	if err != nil {
		t.Fatalf("ListLibrary: %v", err)
	}
	if page.Total != 3 || len(page.Items) != 3 {
		t.Fatalf("rows = %d (total %d), want 3: the other rows were deleted", len(page.Items), page.Total)
	}

	titles := map[int64]string{}
	for _, row := range page.Items {
		titles[row.ExternalItemID] = row.Title
	}
	if titles[2] != "Shrek 2 (rescan)" {
		t.Errorf("updated row title = %q, want the re-read title", titles[2])
	}
	if titles[1] != "Shrek" || titles[3] != "Arrival" {
		t.Errorf("untouched rows = %q/%q, want Shrek/Arrival", titles[1], titles[3])
	}
}

// TestUpdateLibraryCacheItemBumpsUpdatedAtOnlyOnChange pins the change tracking:
// an unchanged re-read only moves last_seen_at, a changed one moves updated_at
// too.
func TestUpdateLibraryCacheItemBumpsUpdatedAtOnlyOnChange(t *testing.T) {
	s, ctx, instanceID := libraryItemFixture(t)

	// Insert through the same method so the comparison is against exactly what
	// this method writes (no hand-built fixture to drift out of sync).
	item := LibraryCacheItem{
		MediaType:         "movie",
		ExternalItemID:    42,
		Title:             "Dune",
		Year:              2021,
		MediaFolderPath:   "/media/movies/Dune (2021)",
		RootFolderPath:    "/media/movies",
		TargetFilePath:    "/media/movies/Dune (2021)/Dune.2021.mkv",
		MatchesTargetTags: true,
		EligibilityStatus: "eligible",
	}
	if err := s.UpdateLibraryCacheItem(ctx, instanceID, item); err != nil {
		t.Fatalf("insert via UpdateLibraryCacheItem: %v", err)
	}

	// Freeze both stamps in the past so "did it move?" is a SQL comparison
	// instead of a string-format guess.
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE arr_library_cache SET updated_at = '2000-01-01 00:00:00',
		     last_seen_at = '2000-01-01 00:00:00'
		 WHERE arr_instance_id = ? AND external_item_id = 42`, instanceID); err != nil {
		t.Fatalf("freeze timestamps: %v", err)
	}
	stampIsOld := func(column string) bool {
		t.Helper()
		var old bool
		if err := s.DB.QueryRowContext(ctx,
			`SELECT `+column+` < '2020-01-01 00:00:00' FROM arr_library_cache
			 WHERE arr_instance_id = ? AND external_item_id = 42`, instanceID).Scan(&old); err != nil {
			t.Fatalf("read %s: %v", column, err)
		}
		return old
	}

	// Same content again: only the observation moves.
	if err := s.UpdateLibraryCacheItem(ctx, instanceID, item); err != nil {
		t.Fatalf("unchanged UpdateLibraryCacheItem: %v", err)
	}
	if !stampIsOld("updated_at") {
		t.Error("updated_at moved on an unchanged re-read; change history is lost")
	}
	if stampIsOld("last_seen_at") {
		t.Error("last_seen_at did not move on a re-read; the observation is not recorded")
	}

	// A real change moves both.
	changed := item
	changed.Title = "Dune (remastered)"
	if err := s.UpdateLibraryCacheItem(ctx, instanceID, changed); err != nil {
		t.Fatalf("changed UpdateLibraryCacheItem: %v", err)
	}
	if stampIsOld("updated_at") {
		t.Error("updated_at did not move on a changed re-read")
	}
	var title string
	if err := s.DB.QueryRowContext(ctx,
		`SELECT title FROM arr_library_cache WHERE arr_instance_id = ? AND external_item_id = 42`,
		instanceID).Scan(&title); err != nil {
		t.Fatalf("read title: %v", err)
	}
	if title != "Dune (remastered)" {
		t.Errorf("title = %q, want the re-read title", title)
	}
}

// TestUpdateLibraryCacheRowFallsBackToInsert covers the race a re-read cannot
// see: the row is deleted between the caller's read and the UPDATE (a sync pass
// no longer saw the item), so the statement matches nothing. That must not be a
// silent no-op — the observation is inserted instead of thrown away.
func TestUpdateLibraryCacheRowFallsBackToInsert(t *testing.T) {
	s, ctx, instanceID := libraryItemFixture(t)

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	item := LibraryCacheItem{
		MediaType:         "movie",
		ExternalItemID:    77,
		Title:             "Heat",
		Year:              1995,
		MediaFolderPath:   "/media/movies/Heat (1995)",
		RootFolderPath:    "/media/movies",
		TargetFilePath:    "/media/movies/Heat (1995)/Heat.1995.mkv",
		MatchesTargetTags: true,
		EligibilityStatus: "eligible",
	}
	// id 999999 matches no row, exactly like a row the poller deleted after the
	// caller read it.
	if err := updateLibraryCacheRow(ctx, tx, instanceID, 999999, item, "2020-01-01 00:00:00"); err != nil {
		t.Fatalf("updateLibraryCacheRow with a vanished row: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var (
		id    int64
		title string
	)
	if err := s.DB.QueryRowContext(ctx,
		`SELECT id, title FROM arr_library_cache
		 WHERE arr_instance_id = ? AND external_item_id = 77`, instanceID).Scan(&id, &title); err != nil {
		t.Fatalf("read the row the update was supposed to insert: %v", err)
	}
	if id <= 0 || title != "Heat" {
		t.Errorf("row = %d/%q, want the inserted re-read row", id, title)
	}

	page, err := s.ListLibrary(ctx, ListLibraryFilter{})
	if err != nil {
		t.Fatalf("ListLibrary: %v", err)
	}
	if page.Total != 4 {
		t.Errorf("rows = %d, want 4 (3 fixtures + the re-read row)", page.Total)
	}
}
