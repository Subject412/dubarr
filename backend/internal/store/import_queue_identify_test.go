package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// identifyFixture seeds one arr instance, one arr_library_cache row and one
// pending import_queue row, and returns the ids. instanceType drives the queue
// row's kind (matched_arr_instance_id), cacheMediaType the library row's kind.
type identifyFixture struct {
	store    *Store
	ctx      context.Context
	instance int64
	queue    int64
	cache    int64
}

func newIdentifyFixture(t *testing.T, instanceType, cacheMediaType string) identifyFixture {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		 VALUES ('main', ?, 'http://radarr.invalid', x'00', '0000')`, instanceType)
	if err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	instanceID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("instance id: %v", err)
	}

	res, err = s.DB.ExecContext(ctx,
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, title, original_language_iso2, year,
		  target_file_path, audio_languages_json, eligibility_status)
		 VALUES (?, ?, 1, 'Kraina Lodu', 'en', 2013, '/media/Kraina Lodu (2013).mkv', '["en"]', 'eligible')`,
		instanceID, cacheMediaType)
	if err != nil {
		t.Fatalf("insert library cache: %v", err)
	}
	cacheID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("cache id: %v", err)
	}

	source := "/watch/Kraina.Lodu.2013.PL.mkv"
	if _, err := s.RecordImportCandidate(ctx, source, "Kraina.Lodu.2013.PL.mkv", 42); err != nil {
		t.Fatalf("RecordImportCandidate: %v", err)
	}
	var queueID int64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT id FROM import_queue WHERE source_file_path = ?`, source).Scan(&queueID); err != nil {
		t.Fatalf("queue id: %v", err)
	}
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE import_queue SET matched_arr_instance_id = ? WHERE id = ?`, instanceID, queueID); err != nil {
		t.Fatalf("link candidate instance: %v", err)
	}
	return identifyFixture{store: s, ctx: ctx, instance: instanceID, queue: queueID, cache: cacheID}
}

func (f identifyFixture) matchRow(t *testing.T) (matchedID sql.NullInt64, confidence sql.NullString, status string) {
	t.Helper()
	if err := f.store.DB.QueryRowContext(f.ctx,
		`SELECT matched_library_cache_id, parse_confidence, status FROM import_queue WHERE id = ?`,
		f.queue).Scan(&matchedID, &confidence, &status); err != nil {
		t.Fatalf("read queue row: %v", err)
	}
	return matchedID, confidence, status
}

// TestSetImportCandidateLibraryMatchPersists pins the operator pick: the match
// id and the parse confidence are written while the row stays pending_approval.
// A human pick is never an exact parse, so even an "exact" argument is stored as
// the highest non-exact label.
func TestSetImportCandidateLibraryMatchPersists(t *testing.T) {
	f := newIdentifyFixture(t, "radarr", "movie")

	if err := f.store.SetImportCandidateLibraryMatch(f.ctx, f.queue, f.cache, "exact"); err != nil {
		t.Fatalf("SetImportCandidateLibraryMatch: %v", err)
	}
	matched, confidence, status := f.matchRow(t)
	if !matched.Valid || matched.Int64 != f.cache {
		t.Errorf("matched_library_cache_id = %+v, want %d", matched, f.cache)
	}
	if !confidence.Valid || confidence.String != "ambiguous" {
		t.Errorf("parse_confidence = %+v, want ambiguous (a pick is not a parse)", confidence)
	}
	if status != "pending_approval" {
		t.Errorf("status = %q, want pending_approval", status)
	}

	// Re-picking the same id is idempotent, and an unrecognised label degrades
	// to the schema's "ambiguous" rather than inventing a new value.
	if err := f.store.SetImportCandidateLibraryMatch(f.ctx, f.queue, f.cache, "made-up"); err != nil {
		t.Fatalf("re-pick: %v", err)
	}
	_, confidence, _ = f.matchRow(t)
	if confidence.String != "ambiguous" {
		t.Errorf("parse_confidence after re-pick = %q, want ambiguous", confidence.String)
	}
}

// TestSetImportCandidateLibraryMatchKindMismatch pins that a movie row cannot be
// matched to an episode cache row, and that the refusal writes nothing.
func TestSetImportCandidateLibraryMatchKindMismatch(t *testing.T) {
	f := newIdentifyFixture(t, "radarr", "episode")

	err := f.store.SetImportCandidateLibraryMatch(f.ctx, f.queue, f.cache, "exact")
	if !errors.Is(err, ErrImportCandidateKindMismatch) {
		t.Fatalf("error = %v, want ErrImportCandidateKindMismatch", err)
	}
	matched, confidence, status := f.matchRow(t)
	if matched.Valid || confidence.Valid {
		t.Errorf("a refused pick wrote match=%+v confidence=%+v", matched, confidence)
	}
	if status != "pending_approval" {
		t.Errorf("status = %q, want pending_approval", status)
	}
}

// TestSetImportCandidateLibraryMatchUnknownQueueRow pins the 404 mapping.
func TestSetImportCandidateLibraryMatchUnknownQueueRow(t *testing.T) {
	f := newIdentifyFixture(t, "radarr", "movie")
	err := f.store.SetImportCandidateLibraryMatch(f.ctx, f.queue+999, f.cache, "exact")
	if !errors.Is(err, ErrImportCandidateNotFound) {
		t.Fatalf("error = %v, want ErrImportCandidateNotFound", err)
	}
}

// TestSetImportCandidateLibraryMatchUnknownCacheRow pins the other 404 mapping.
func TestSetImportCandidateLibraryMatchUnknownCacheRow(t *testing.T) {
	f := newIdentifyFixture(t, "radarr", "movie")
	err := f.store.SetImportCandidateLibraryMatch(f.ctx, f.queue, f.cache+999, "exact")
	if !errors.Is(err, ErrLibraryCacheNotFound) {
		t.Fatalf("error = %v, want ErrLibraryCacheNotFound", err)
	}
}

// TestSetImportCandidateLibraryMatchDoesNotChangeStatus pins that matching is
// not approving: a row that already left pending_approval is refused and its
// status is left alone.
func TestSetImportCandidateLibraryMatchDoesNotChangeStatus(t *testing.T) {
	f := newIdentifyFixture(t, "radarr", "movie")

	if err := f.store.TransitionImportCandidate(f.ctx, f.queue, "ignored"); err != nil {
		t.Fatalf("TransitionImportCandidate: %v", err)
	}
	err := f.store.SetImportCandidateLibraryMatch(f.ctx, f.queue, f.cache, "exact")
	if !errors.Is(err, ErrImportCandidateState) {
		t.Fatalf("error = %v, want ErrImportCandidateState", err)
	}
	matched, _, status := f.matchRow(t)
	if matched.Valid {
		t.Errorf("a non-pending row was matched: %+v", matched)
	}
	if status != "ignored" {
		t.Errorf("status = %q, want ignored (matching must not transition)", status)
	}
}

// TestImportCandidateSummaryByID pins the single-row read the match route uses to
// report the same match_state the list would show.
func TestImportCandidateSummaryByID(t *testing.T) {
	f := newIdentifyFixture(t, "radarr", "movie")
	if err := f.store.SetImportCandidateLibraryMatch(f.ctx, f.queue, f.cache, "exact"); err != nil {
		t.Fatalf("SetImportCandidateLibraryMatch: %v", err)
	}

	row, ok, err := f.store.ImportCandidateSummaryByID(f.ctx, f.queue)
	if err != nil {
		t.Fatalf("ImportCandidateSummaryByID: %v", err)
	}
	if !ok {
		t.Fatal("ImportCandidateSummaryByID reported the seeded row missing")
	}
	if !row.Matched || row.Title != "Kraina Lodu" || row.MediaType != "movie" {
		t.Errorf("summary = %+v, want the matched Kraina Lodu movie", row)
	}

	if _, ok, err := f.store.ImportCandidateSummaryByID(f.ctx, f.queue+999); err != nil || ok {
		t.Errorf("unknown id = ok %v err %v, want ok=false err=nil", ok, err)
	}
}

// TestIdentifyCandidatePool pins the pool narrowing: one media kind, a
// case-insensitive title substring, and an empty kind meaning both.
func TestIdentifyCandidatePool(t *testing.T) {
	f := newIdentifyFixture(t, "radarr", "movie")
	res, err := f.store.DB.ExecContext(f.ctx,
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, title, original_language_iso2, year,
		  target_file_path, audio_languages_json, eligibility_status)
		 VALUES (?, 'episode', 5, 'Kraina Lodu: The Series', 'en', 2015, '', '[]', 'missing_target')`,
		f.instance)
	if err != nil {
		t.Fatalf("insert episode cache row: %v", err)
	}
	if _, err := res.LastInsertId(); err != nil {
		t.Fatalf("episode cache id: %v", err)
	}

	pool, err := f.store.IdentifyCandidatePool(f.ctx, f.instance, "movie", "LODU")
	if err != nil {
		t.Fatalf("IdentifyCandidatePool: %v", err)
	}
	if len(pool) != 1 || pool[0].ID != f.cache || pool[0].Title != "Kraina Lodu" || pool[0].Year != 2013 {
		t.Fatalf("movie pool = %+v, want only the movie row", pool)
	}
	if pool[0].ExternalItemID != 1 || pool[0].MediaType != "movie" {
		t.Errorf("pool metadata = %+v", pool[0])
	}

	// A zero instance id means "no instance resolved", so both kinds stay in.
	both, err := f.store.IdentifyCandidatePool(f.ctx, 0, "", "lodu")
	if err != nil {
		t.Fatalf("IdentifyCandidatePool both kinds: %v", err)
	}
	if len(both) != 2 {
		t.Errorf("both-kind pool = %d rows, want 2", len(both))
	}

	none, err := f.store.IdentifyCandidatePool(f.ctx, f.instance, "movie", "no such title")
	if err != nil {
		t.Fatalf("IdentifyCandidatePool miss: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("miss pool = %+v, want none", none)
	}
}

// addInstance inserts one arr instance of the given type and returns its id.
func (f identifyFixture) addInstance(t *testing.T, instanceType string) int64 {
	t.Helper()
	res, err := f.store.DB.ExecContext(f.ctx,
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		 VALUES (?, ?, 'http://second.invalid', x'00', '0000')`, "second-"+instanceType, instanceType)
	if err != nil {
		t.Fatalf("insert second arr instance: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("second instance id: %v", err)
	}
	return id
}

// addCache inserts one arr_library_cache row owned by instanceID and returns it.
func (f identifyFixture) addCache(t *testing.T, instanceID int64, mediaType, title string, year int, externalID int64) int64 {
	t.Helper()
	res, err := f.store.DB.ExecContext(f.ctx,
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, title, original_language_iso2, year,
		  target_file_path, audio_languages_json, eligibility_status)
		 VALUES (?, ?, ?, ?, 'en', ?, '', '[]', 'eligible')`,
		instanceID, mediaType, externalID, title, year)
	if err != nil {
		t.Fatalf("insert library cache %q: %v", title, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("cache id %q: %v", title, err)
	}
	return id
}

// TestIdentifyCandidatePoolExcludesOtherInstance pins the instance half of the
// pool narrowing (defense in depth: two radarr rows can exist because
// arr_instances.type has no UNIQUE). A row synced by another instance of the
// same kind must not be offered, and a 0 instance id must still see it.
func TestIdentifyCandidatePoolExcludesOtherInstance(t *testing.T) {
	f := newIdentifyFixture(t, "radarr", "movie")
	other := f.addInstance(t, "radarr")
	otherCache := f.addCache(t, other, "movie", "Kraina Lodu", 2013, 1)

	pool, err := f.store.IdentifyCandidatePool(f.ctx, f.instance, "", "lodu")
	if err != nil {
		t.Fatalf("IdentifyCandidatePool: %v", err)
	}
	if len(pool) != 1 || pool[0].ID != f.cache {
		t.Fatalf("instance pool = %+v, want only the row of instance %d", pool, f.instance)
	}

	unscoped, err := f.store.IdentifyCandidatePool(f.ctx, 0, "", "lodu")
	if err != nil {
		t.Fatalf("IdentifyCandidatePool unscoped: %v", err)
	}
	if len(unscoped) != 2 {
		t.Fatalf("unscoped pool = %+v, want both instances' rows", unscoped)
	}
	seen := map[int64]bool{}
	for _, row := range unscoped {
		seen[row.ID] = true
	}
	if !seen[f.cache] || !seen[otherCache] {
		t.Errorf("unscoped pool ids = %+v, want %d and %d", seen, f.cache, otherCache)
	}
}

// TestSetImportCandidateLibraryMatchInstanceMismatch pins the instance guard: a
// cache row of another arr instance is refused even when the media kind agrees,
// and the refusal writes nothing.
func TestSetImportCandidateLibraryMatchInstanceMismatch(t *testing.T) {
	f := newIdentifyFixture(t, "radarr", "movie")
	other := f.addInstance(t, "radarr")
	otherCache := f.addCache(t, other, "movie", "Kraina Lodu", 2013, 1)

	err := f.store.SetImportCandidateLibraryMatch(f.ctx, f.queue, otherCache, "ambiguous")
	if !errors.Is(err, ErrImportCandidateInstanceMismatch) {
		t.Fatalf("error = %v, want ErrImportCandidateInstanceMismatch", err)
	}
	matched, confidence, status := f.matchRow(t)
	if matched.Valid || confidence.Valid {
		t.Errorf("a refused pick wrote match=%+v confidence=%+v", matched, confidence)
	}
	if status != "pending_approval" {
		t.Errorf("status = %q, want pending_approval", status)
	}

	// The row's own instance stays matchable.
	if err := f.store.SetImportCandidateLibraryMatch(f.ctx, f.queue, f.cache, "ambiguous"); err != nil {
		t.Fatalf("match on the row's own instance: %v", err)
	}
}

// TestSetImportCandidateLibraryMatchRejectsLostUpdate simulates the racing
// second pick: once the row is matched to one id, a pick of a different id loses
// the conditional UPDATE instead of silently overwriting the first pick.
func TestSetImportCandidateLibraryMatchRejectsLostUpdate(t *testing.T) {
	f := newIdentifyFixture(t, "radarr", "movie")
	second := f.addCache(t, f.instance, "movie", "Kraina Lodu 2", 2019, 2)

	if err := f.store.SetImportCandidateLibraryMatch(f.ctx, f.queue, f.cache, "ambiguous"); err != nil {
		t.Fatalf("first pick: %v", err)
	}

	err := f.store.SetImportCandidateLibraryMatch(f.ctx, f.queue, second, "ambiguous")
	if !errors.Is(err, ErrImportCandidateMatchMismatch) {
		t.Fatalf("second pick error = %v, want ErrImportCandidateMatchMismatch", err)
	}
	matched, confidence, status := f.matchRow(t)
	if !matched.Valid || matched.Int64 != f.cache {
		t.Errorf("matched_library_cache_id = %+v, want the first pick %d", matched, f.cache)
	}
	if !confidence.Valid || confidence.String != "ambiguous" {
		t.Errorf("parse_confidence = %+v, want ambiguous", confidence)
	}
	if status != "pending_approval" {
		t.Errorf("status = %q, want pending_approval", status)
	}

	// Re-picking the winning id is still idempotent.
	if err := f.store.SetImportCandidateLibraryMatch(f.ctx, f.queue, f.cache, "ambiguous"); err != nil {
		t.Errorf("re-pick of the winning id: %v", err)
	}
}
