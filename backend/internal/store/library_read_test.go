package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// libraryInstance inserts one arr instance and returns its id.
func libraryInstance(t *testing.T, s *Store, ctx context.Context, name, kind string) int64 {
	t.Helper()
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		 VALUES (?, ?, 'http://x.invalid', x'00', '0000')`, name, kind)
	if err != nil {
		t.Fatalf("insert arr instance %q: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("instance id: %v", err)
	}
	return id
}

// libraryRowOpts tunes one fixture row. The zero value is the common case: arr
// has imported the item, so target_file_path points at a video file and the
// tag gate matched.
type libraryRowOpts struct {
	// NoTargetFile models an item arr has no file for yet (a Sonarr episode
	// that was never downloaded): target_file_path is NULL and nothing could
	// have matched the target tags.
	NoTargetFile bool
}

// libraryRow inserts one cached row with only the columns the Library page
// reads, plus the JSON blobs the page must never return.
func libraryRow(t *testing.T, s *Store, ctx context.Context, instanceID int64, mediaType string, externalID int64, title, originalTitle, eligibility string, opts ...libraryRowOpts) int64 {
	t.Helper()
	var targetPath any = "/media/x/f.mkv"
	matchesTargetTags := 1
	if len(opts) > 0 && opts[0].NoTargetFile {
		targetPath = nil
		matchesTargetTags = 0
	}
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, title, original_title, year,
		  media_folder_path, target_file_path, matches_target_tags, eligibility_status,
		  video_codec, raw_metadata_json, audio_languages_json,
		  arr_tag_ids_json, arr_tag_labels_json)
		 VALUES (?, ?, ?, ?, ?, 2001, '/media/x', ?, ?, ?, 'h264',
		         '{"secret":"raw"}', '["ja"]', '[1]', '["dub"]')`,
		instanceID, mediaType, externalID, title, originalTitle, targetPath,
		matchesTargetTags, eligibility)
	if err != nil {
		t.Fatalf("insert library row %q: %v", title, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("library row id: %v", err)
	}
	return id
}

// libraryReadFixture seeds two instances and five rows: three eligible Radarr
// movies (two matching "shrek"), one waiting_for_tags movie, one Sonarr episode
// with no target file.
func libraryReadFixture(t *testing.T) (*Store, context.Context, int64, int64) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	radarr := libraryInstance(t, s, ctx, "Radarr", "radarr")
	sonarr := libraryInstance(t, s, ctx, "Sonarr", "sonarr")

	libraryRow(t, s, ctx, radarr, "movie", 1, "Shrek", "Shrek", "eligible")
	libraryRow(t, s, ctx, radarr, "movie", 2, "Shrek 2", "", "eligible")
	libraryRow(t, s, ctx, radarr, "movie", 3, "Arrival", "Przybycie", "eligible")
	libraryRow(t, s, ctx, radarr, "movie", 4, "Minions", "", "waiting_for_tags")
	libraryRow(t, s, ctx, sonarr, "episode", 10, "The Expanse", "", "missing_target",
		libraryRowOpts{NoTargetFile: true})
	return s, ctx, radarr, sonarr
}

// TestListLibraryPaginatesAndFilters pins that every filter applies to both the
// page and Total, that paging is stable and that the limit is bounded.
func TestListLibraryPaginatesAndFilters(t *testing.T) {
	s, ctx, radarr, sonarr := libraryReadFixture(t)

	// Filters compose: kind + eligibility + q + instance.
	page, err := s.ListLibrary(ctx, ListLibraryFilter{
		Kind: "radarr", Eligibility: "eligible", Q: "shrek", InstanceID: radarr,
	})
	if err != nil {
		t.Fatalf("ListLibrary (composed filters): %v", err)
	}
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("composed filter page = %d items / total %d, want 2/2", len(page.Items), page.Total)
	}
	if page.Items[0].Title != "Shrek" || page.Items[1].Title != "Shrek 2" {
		t.Fatalf("order = %q, %q; want title order Shrek, Shrek 2",
			page.Items[0].Title, page.Items[1].Title)
	}
	if page.Items[0].InstanceName != "Radarr" || page.Items[0].InstanceType != "radarr" {
		t.Errorf("instance columns = %q/%q, want Radarr/radarr",
			page.Items[0].InstanceName, page.Items[0].InstanceType)
	}

	// Page boundaries: Total counts the whole matching set, not the page.
	first, err := s.ListLibrary(ctx, ListLibraryFilter{
		Kind: "radarr", Eligibility: "eligible", Q: "shrek", Limit: 1, Offset: 0,
	})
	if err != nil {
		t.Fatalf("ListLibrary (page 1): %v", err)
	}
	if first.Total != 2 || len(first.Items) != 1 || first.Items[0].Title != "Shrek" {
		t.Fatalf("page 1 = %+v, want 1 item / total 2 starting at Shrek", first)
	}
	second, err := s.ListLibrary(ctx, ListLibraryFilter{
		Kind: "radarr", Eligibility: "eligible", Q: "shrek", Limit: 1, Offset: 1,
	})
	if err != nil {
		t.Fatalf("ListLibrary (page 2): %v", err)
	}
	if second.Total != 2 || len(second.Items) != 1 || second.Items[0].Title != "Shrek 2" {
		t.Fatalf("page 2 = %+v, want 1 item / total 2 at Shrek 2", second)
	}
	if second.Limit != 1 || second.Offset != 1 {
		t.Errorf("page metadata = limit %d offset %d, want 1/1", second.Limit, second.Offset)
	}
	if second.Items[0].ID == first.Items[0].ID {
		t.Error("pages 1 and 2 returned the same row; paging is not stable")
	}

	// Eligibility and instance alone.
	if page, err := s.ListLibrary(ctx, ListLibraryFilter{Eligibility: "waiting_for_tags"}); err != nil {
		t.Fatalf("ListLibrary (eligibility): %v", err)
	} else if page.Total != 1 || page.Items[0].Title != "Minions" {
		t.Errorf("eligibility filter = %+v, want only Minions", page.Items)
	}
	if page, err := s.ListLibrary(ctx, ListLibraryFilter{InstanceID: sonarr}); err != nil {
		t.Fatalf("ListLibrary (instance): %v", err)
	} else if page.Total != 1 || len(page.Items) != 1 || page.Items[0].MediaType != "episode" {
		t.Errorf("instance filter = %+v, want the Sonarr episode", page.Items)
	} else if page.Items[0].TargetFilePath != "" {
		// A fileless episode keeps its empty target path.
		t.Errorf("target path = %q, want empty", page.Items[0].TargetFilePath)
	}

	// A combination with no matches returns an empty page, never nil.
	if page, err := s.ListLibrary(ctx, ListLibraryFilter{Kind: "sonarr", Eligibility: "eligible"}); err != nil {
		t.Fatalf("ListLibrary (empty combination): %v", err)
	} else if page.Total != 0 || page.Items == nil || len(page.Items) != 0 {
		t.Errorf("empty combination = %+v, want an empty non-nil page", page)
	}

	// Limit defaults and is capped; negative offset clamps to 0.
	if page, err := s.ListLibrary(ctx, ListLibraryFilter{}); err != nil {
		t.Fatalf("ListLibrary (defaults): %v", err)
	} else if page.Limit != LibraryPageDefaultLimit || page.Offset != 0 {
		t.Errorf("defaults = limit %d offset %d, want %d/0", page.Limit, page.Offset, LibraryPageDefaultLimit)
	}
	if page, err := s.ListLibrary(ctx, ListLibraryFilter{Limit: 5000, Offset: -3}); err != nil {
		t.Fatalf("ListLibrary (bounds): %v", err)
	} else if page.Limit != LibraryPageMaxLimit || page.Offset != 0 {
		t.Errorf("bounds = limit %d offset %d, want %d/0", page.Limit, page.Offset, LibraryPageMaxLimit)
	}
}

// TestListLibraryBlankQueryReturnsAll is the difference from ListLibrarySearch:
// a blank Q browses the whole library instead of returning nothing.
func TestListLibraryBlankQueryReturnsAll(t *testing.T) {
	s, ctx, _, _ := libraryReadFixture(t)

	for _, q := range []string{"", "   "} {
		page, err := s.ListLibrary(ctx, ListLibraryFilter{Q: q})
		if err != nil {
			t.Fatalf("ListLibrary(q=%q): %v", q, err)
		}
		if page.Total != 5 || len(page.Items) != 5 {
			t.Fatalf("blank query q=%q = %d items / total %d, want 5/5", q, len(page.Items), page.Total)
		}
		// Deterministic order: title then id.
		want := []string{"Arrival", "Minions", "Shrek", "Shrek 2", "The Expanse"}
		got := make([]string, len(page.Items))
		for i, item := range page.Items {
			got[i] = item.Title
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("order = %v, want %v", got, want)
		}
	}

	// The query also matches original_title, not just title.
	page, err := s.ListLibrary(ctx, ListLibraryFilter{Q: "shrek"})
	if err != nil {
		t.Fatalf("ListLibrary(q=shrek): %v", err)
	}
	if page.Total != 2 {
		t.Errorf("q=shrek total = %d, want 2", page.Total)
	}
	page, err = s.ListLibrary(ctx, ListLibraryFilter{Q: "PRZYBYCIE"})
	if err != nil {
		t.Fatalf("ListLibrary(q=PRZYBYCIE): %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Title != "Arrival" {
		t.Errorf("original_title match = %+v, want Arrival (case-insensitive)", page.Items)
	}
}

// TestListLibraryExcludesJsonBlobs pins the column exclusion: the page struct
// carries none of raw_metadata_json, audio_languages_json, arr_tag_ids_json or
// arr_tag_labels_json, so a client can never receive them.
func TestListLibraryExcludesJsonBlobs(t *testing.T) {
	s, ctx, _, _ := libraryReadFixture(t)

	page, err := s.ListLibrary(ctx, ListLibraryFilter{})
	if err != nil {
		t.Fatalf("ListLibrary: %v", err)
	}
	if len(page.Items) == 0 {
		t.Fatal("no items to inspect")
	}

	// No struct field may carry a JSON blob column.
	typ := reflect.TypeOf(LibraryListItem{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		tag := strings.ToLower(typ.Field(i).Tag.Get("json"))
		for _, banned := range []string{"raw", "metadata", "audio_language", "arr_tag", "tag_ids", "tag_labels"} {
			if strings.Contains(name, banned) || strings.Contains(tag, banned) {
				t.Errorf("LibraryListItem field %q (json %q) exposes %q", typ.Field(i).Name, tag, banned)
			}
		}
	}

	// And the marshalled row must not contain the columns either.
	raw, err := json.Marshal(page.Items[0])
	if err != nil {
		t.Fatalf("marshal item: %v", err)
	}
	for _, banned := range []string{"raw_metadata", "audio_languages", "arr_tag_ids", "arr_tag_labels", "raw"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("marshalled item contains %q: %s", banned, raw)
		}
	}
	if strings.Contains(string(raw), "secret") {
		t.Errorf("marshalled item leaked the raw metadata payload: %s", raw)
	}
}

// TestListLibraryEscapesLikeWildcards guards the LIKE escaping: rows whose
// title contains a literal "%" or "_" must be matched as text, so q="%" cannot
// degenerate into "everything".
func TestListLibraryEscapesLikeWildcards(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	radarr := libraryInstance(t, s, ctx, "Radarr", "radarr")
	libraryRow(t, s, ctx, radarr, "movie", 51, "100% Wolf", "", "eligible")
	libraryRow(t, s, ctx, radarr, "movie", 52, "Foo_Bar", "", "eligible")
	libraryRow(t, s, ctx, radarr, "movie", 53, "Plain Title", "", "eligible")
	libraryRow(t, s, ctx, radarr, "movie", 54, "%Leading", "", "eligible")
	libraryRow(t, s, ctx, radarr, "movie", 55, "trailing_", "", "eligible")

	for _, tc := range []struct {
		q    string
		want []string
	}{
		// Order is title-then-id, so "%" (0x25) sorts before "1".
		{q: "%", want: []string{"%Leading", "100% Wolf"}},
		{q: "_", want: []string{"Foo_Bar", "trailing_"}},
		{q: "100%", want: []string{"100% Wolf"}},
	} {
		page, err := s.ListLibrary(ctx, ListLibraryFilter{Q: tc.q})
		if err != nil {
			t.Fatalf("ListLibrary(q=%q): %v", tc.q, err)
		}
		if int(page.Total) != len(tc.want) || len(page.Items) != len(tc.want) {
			t.Fatalf("q=%q = %d items / total %d, want %d/%d (a literal wildcard must not match everything)",
				tc.q, len(page.Items), page.Total, len(tc.want), len(tc.want))
		}
		got := make([]string, len(page.Items))
		for i, item := range page.Items {
			got[i] = item.Title
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("q=%q titles = %v, want %v", tc.q, got, tc.want)
		}
	}
}

// TestListLibraryPagesDuplicateTitles pins that rows sharing one title page
// without repeating or skipping: the id tie-break keeps the order total.
func TestListLibraryPagesDuplicateTitles(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	radarr := libraryInstance(t, s, ctx, "Radarr", "radarr")
	for i := int64(0); i < 4; i++ {
		libraryRow(t, s, ctx, radarr, "movie", 100+i, "Same Title", "", "eligible")
	}

	seen := make(map[int64]bool, 4)
	for offset := 0; offset < 4; offset++ {
		page, err := s.ListLibrary(ctx, ListLibraryFilter{Q: "same", Limit: 1, Offset: offset})
		if err != nil {
			t.Fatalf("ListLibrary(offset %d): %v", offset, err)
		}
		if page.Total != 4 {
			t.Fatalf("offset %d total = %d, want 4", offset, page.Total)
		}
		if len(page.Items) != 1 {
			t.Fatalf("offset %d returned %d items, want 1", offset, len(page.Items))
		}
		id := page.Items[0].ID
		if seen[id] {
			t.Fatalf("offset %d returned row %d again; paging repeats rows on duplicate titles", offset, id)
		}
		seen[id] = true
	}
	if len(seen) != 4 {
		t.Fatalf("paged %d distinct rows, want 4 (a duplicate title must not be skipped)", len(seen))
	}
}

// TestListLibraryRejectsUnknownKindAndEligibility keeps bad filter values a
// typed error instead of a silently empty page.
func TestListLibraryRejectsUnknownKindAndEligibility(t *testing.T) {
	s, ctx, _, _ := libraryReadFixture(t)

	if _, err := s.ListLibrary(ctx, ListLibraryFilter{Kind: "lidarr"}); err == nil {
		t.Fatal("ListLibrary accepted kind lidarr")
	} else if !errors.Is(err, ErrInvalidLibraryFilter) {
		t.Errorf("kind error = %v, want ErrInvalidLibraryFilter", err)
	}
	if _, err := s.ListLibrary(ctx, ListLibraryFilter{Eligibility: "maybe"}); err == nil {
		t.Fatal("ListLibrary accepted eligibility maybe")
	} else if !errors.Is(err, ErrInvalidLibraryFilter) {
		t.Errorf("eligibility error = %v, want ErrInvalidLibraryFilter", err)
	}
}
