package arrsync

// The release projection's first epoch (#48): a sync pass rewrites the arr
// columns the gate reads, so the pass is where a held job's old verdict is
// re-evaluated. These tests pin that a pass promotes exactly the job the pass
// just made eligible and leaves the rest held.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arr/arrmock"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// setTagFilter stores the target.tag_filter section the poller's projection
// reads.
func (r *rig) setTagFilter(tf settings.TargetTagFilter) {
	r.t.Helper()
	raw, err := json.Marshal(tf)
	if err != nil {
		r.t.Fatalf("marshal tag filter: %v", err)
	}
	if err := r.settings.Set(r.context(), settings.KeyTargetTagFilter, raw); err != nil {
		r.t.Fatalf("set tag filter: %v", err)
	}
}

// arrTagFilter makes the arr tag the criterion, so only the sync pass — which
// rewrites the arr tag snapshot — can turn a held job eligible.
func arrTagFilter(tag string) settings.TargetTagFilter {
	return settings.TargetTagFilter{
		RequiredArrTags:  []string{tag},
		TagMatchMode:     "ANY",
		SourceMode:       settings.SourceModeArr,
		EnforceTagFilter: true,
	}
}

// cacheRowID reads the cache row one arr item was cached as.
func cacheRowID(t *testing.T, st *store.Store, externalID int64) int64 {
	t.Helper()
	var id int64
	if err := st.DB.QueryRow(
		`SELECT id FROM arr_library_cache WHERE media_type = 'movie' AND external_item_id = ?`,
		externalID).Scan(&id); err != nil {
		t.Fatalf("cache row of movie %d: %v", externalID, err)
	}
	return id
}

// untagCacheRow strips the arr tag snapshot, so the job seeded on it is held for
// the reason a pass that ran before the tag existed would have written.
func untagCacheRow(t *testing.T, st *store.Store, rowID int64) {
	t.Helper()
	if _, err := st.DB.Exec(
		`UPDATE arr_library_cache SET arr_tag_labels_json = '[]', arr_tag_ids_json = '[]',
		        matches_target_tags = 0
		 WHERE id = ?`, rowID); err != nil {
		t.Fatalf("untag cache row %d: %v", rowID, err)
	}
}

// seedHeldJob inserts one held sync job on a real vault track, so the foreign
// keys hold and the projection has a row to decide. forced writes the row the
// constructor refuses to produce (a forced job is always born queued), which is
// how the projection's skip is pinned: it must ignore the operator's override
// instead of re-deriving eligibility for it.
func seedHeldJob(t *testing.T, st *store.Store, dir string, libID int64, target string, forced bool) int64 {
	t.Helper()
	ctx := context.Background()

	trackPath := filepath.Join(dir, fmt.Sprintf("track-%d-%t.pl.mka", libID, forced))
	if err := os.WriteFile(trackPath, []byte("mka"), 0o600); err != nil {
		t.Fatalf("write track: %v", err)
	}
	res, err := st.DB.ExecContext(ctx,
		`INSERT INTO vault_media (media_type, title, year, vault_folder_path)
		 VALUES ('movie', 'Shrek', 2001, ?)`, dir)
	if err != nil {
		t.Fatalf("insert vault media: %v", err)
	}
	mediaID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("vault media id: %v", err)
	}
	res, err = st.DB.ExecContext(ctx,
		`INSERT INTO audio_tracks
		 (vault_media_id, group_uuid, lang_iso2, lang_iso3, variant, file_path, sha256, codec, channels, channel_count)
		 VALUES (?, 'release-group', 'pl', 'pol', 'dubbing', ?, '00', 'opus', '2.0', 2)`,
		mediaID, trackPath)
	if err != nil {
		t.Fatalf("insert audio track: %v", err)
	}
	trackID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("audio track id: %v", err)
	}

	if !forced {
		rec, err := st.CreateSyncJob(ctx, store.SyncJobCreate{
			VaultTrackID:         trackID,
			TargetLibraryCacheID: libID,
			TargetVideoPath:      target,
			TargetLangISO2:       "pl",
			Status:               store.SyncJobHeld,
			HeldReason:           store.HeldReasonWaitingForTags,
		})
		if err != nil {
			t.Fatalf("CreateSyncJob(held): %v", err)
		}
		return rec.ID
	}
	res, err = st.DB.ExecContext(ctx,
		`INSERT INTO sync_mux_jobs
		 (target_track_id, target_library_cache_id, target_video_path, target_lang_iso2,
		  status, force_ignore_tag_filter, held_reason)
		 VALUES (?, ?, ?, 'pl', 'held', 1, ?)`,
		trackID, libID, target, store.HeldReasonWaitingForTags)
	if err != nil {
		t.Fatalf("insert forced held job: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("forced job id: %v", err)
	}
	return id
}

// jobStatus reads the persisted status and held reason of one job.
func jobStatus(t *testing.T, st *store.Store, id int64) (string, string) {
	t.Helper()
	rec, ok, err := st.SyncJobByID(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("SyncJobByID(%d) = (ok=%v, err=%v)", id, ok, err)
	}
	return rec.Status, rec.HeldReason
}

// TestSyncPassReleasesHeldJobWhenTagsAppear is the pass release point: the job
// held while the target carried no arr tag is queued after the pass that saw the
// tag. The job on the still-untagged item keeps its hold, and the forced row is
// invisible to the projection even though its path is eligible.
func TestSyncPassReleasesHeldJobWhenTagsAppear(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	dir := t.TempDir()
	taggedTarget := filepath.Join(dir, "Shrek (2001) [ArcA310].mkv")
	plainTarget := filepath.Join(dir, "Shrek 2 (2004).mkv")
	for _, path := range []string{taggedTarget, plainTarget} {
		if err := os.WriteFile(path, []byte("target"), 0o600); err != nil {
			t.Fatalf("write target %s: %v", path, err)
		}
	}
	r.setTagFilter(arrTagFilter("ArcA310"))

	srv := arrmock.New(arrmock.Radarr, "key-radarr", arrmock.Fixtures{
		Tags:        []arrmock.Tag{{ID: 4, Label: "ArcA310"}},
		RootFolders: []arrmock.RootFolder{{ID: 1, Path: "/media/movies"}},
		Movies: []arrmock.Movie{
			{
				ID: 7, Title: "Shrek", Year: 2001, HasFile: true,
				Path: "/media/movies/Shrek (2001)", Tags: []int{4},
				OriginalLanguage: arrmock.Language{ID: 1, Name: "English"},
				MovieFile:        &arrmock.MovieFile{Path: taggedTarget},
			},
			{
				ID: 8, Title: "Shrek 2", Year: 2004, HasFile: true,
				Path:             "/media/movies/Shrek 2 (2004)",
				OriginalLanguage: arrmock.Language{ID: 1, Name: "English"},
				MovieFile:        &arrmock.MovieFile{Path: plainTarget},
			},
		},
	})
	defer srv.Close()
	r.configure(settings.ArrConnection{Kind: "radarr", BaseURL: srv.URL, Enabled: true})
	r.key("radarr", srv.APIKey())

	p := r.newPoller(time.Minute)
	// First pass: caches both items, so the jobs can point at their rows.
	if _, err := p.Sync(ctx); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	taggedRow := cacheRowID(t, r.store, 7)
	plainRow := cacheRowID(t, r.store, 8)
	// The tag was not on the item when these jobs were born.
	untagCacheRow(t, r.store, taggedRow)

	taggedJob := seedHeldJob(t, r.store, dir, taggedRow, taggedTarget, false)
	plainJob := seedHeldJob(t, r.store, dir, plainRow, plainTarget, false)
	forcedJob := seedHeldJob(t, r.store, dir, taggedRow, taggedTarget, true)

	if status, _ := jobStatus(t, r.store, taggedJob); status != store.SyncJobHeld {
		t.Fatalf("held job before the pass = %q, want %q", status, store.SyncJobHeld)
	}

	if ran := p.syncPass(ctx); !ran {
		t.Fatal("syncPass did not run")
	}

	if status, reason := jobStatus(t, r.store, taggedJob); status != store.SyncJobQueued {
		t.Errorf("tagged job after the pass = %q (%s), want %q: the pass made it eligible",
			status, reason, store.SyncJobQueued)
	}
	if status, reason := jobStatus(t, r.store, plainJob); status != store.SyncJobHeld ||
		reason != store.HeldReasonWaitingForTags {
		t.Errorf("untagged job after the pass = %q (%s), want held/%s",
			status, reason, store.HeldReasonWaitingForTags)
	}
	if status, reason := jobStatus(t, r.store, forcedJob); status != store.SyncJobHeld {
		t.Errorf("forced job after the pass = %q (%s), want it untouched at %q",
			status, reason, store.SyncJobHeld)
	}
}

// TestSyncPassReleaseIsIdempotent pins that the projection is a projection: a
// second pass over the same rows promotes nothing, because the first already
// moved the only eligible job out of held.
func TestSyncPassReleaseIsIdempotent(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	dir := t.TempDir()
	target := filepath.Join(dir, "Shrek (2001) [ArcA310].mkv")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	r.setTagFilter(arrTagFilter("ArcA310"))

	srv := arrmock.New(arrmock.Radarr, "key-radarr", arrmock.Fixtures{
		Tags:        []arrmock.Tag{{ID: 4, Label: "ArcA310"}},
		RootFolders: []arrmock.RootFolder{{ID: 1, Path: "/media/movies"}},
		Movies: []arrmock.Movie{{
			ID: 7, Title: "Shrek", Year: 2001, HasFile: true,
			Path: "/media/movies/Shrek (2001)", Tags: []int{4},
			OriginalLanguage: arrmock.Language{ID: 1, Name: "English"},
			MovieFile:        &arrmock.MovieFile{Path: target},
		}},
	})
	defer srv.Close()
	r.configure(settings.ArrConnection{Kind: "radarr", BaseURL: srv.URL, Enabled: true})
	r.key("radarr", srv.APIKey())

	p := r.newPoller(time.Minute)
	if _, err := p.Sync(ctx); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	rowID := cacheRowID(t, r.store, 7)
	untagCacheRow(t, r.store, rowID)
	jobID := seedHeldJob(t, r.store, dir, rowID, target, false)

	if ran := p.syncPass(ctx); !ran {
		t.Fatal("first syncPass did not run")
	}
	if status, _ := jobStatus(t, r.store, jobID); status != store.SyncJobQueued {
		t.Fatalf("job after the first pass = %q, want %q", status, store.SyncJobQueued)
	}
	if ran := p.syncPass(ctx); !ran {
		t.Fatal("second syncPass did not run")
	}
	if status, _ := jobStatus(t, r.store, jobID); status != store.SyncJobQueued {
		t.Errorf("job after the second pass = %q, want it still %q (no re-hold, no duplicate work)",
			status, store.SyncJobQueued)
	}
}
