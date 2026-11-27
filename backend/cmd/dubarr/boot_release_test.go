package main

// The boot release point (#48): a job held when the appliance last stopped is
// re-projected once before the sync worker starts, so a target that is already
// tagged runs on the first drain instead of waiting for the next arr pass.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/secrets"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// TestBootReleasesHeldJobsBeforeWorkerStart pins that wiring the sync worker
// promotes the held jobs whose target already passes the gate, and does it
// before the worker owns the queue (the worker it returns has not started).
func TestBootReleasesHeldJobsBeforeWorkerStart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	settingsStore := settings.New(st.DB)
	if err := settingsStore.Seed(ctx, filepath.Join(dir, "data")); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	sec, err := secrets.Open(st, secrets.Options{DataDir: filepath.Join(dir, "data")})
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}

	vaultDir := filepath.Join(dir, "vault")
	workDir := filepath.Join(dir, "work")
	for _, path := range []string{vaultDir, workDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}
	storage, err := json.Marshal(settings.StoragePaths{VaultDir: vaultDir, WorkDir: workDir})
	if err != nil {
		t.Fatalf("marshal storage: %v", err)
	}
	if err := settingsStore.Set(ctx, settings.KeyStoragePaths, storage); err != nil {
		t.Fatalf("set storage: %v", err)
	}

	// The seeded tag filter requires no tag (ADR-0012), so this target is
	// already eligible: the hold it carries is a stale verdict from before.
	target := filepath.Join(dir, "library", "Movie (2020) [ArcA310].mkv")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatalf("mkdir library: %v", err)
	}
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	jobID := seedBootHeldJob(t, st, dir, target)

	w, err := wireSyncWorker(ctx, syncWiring{
		Settings: settingsStore,
		Store:    st,
		Secrets:  sec,
		Logf:     t.Logf,
	})
	if err != nil {
		t.Fatalf("wireSyncWorker: %v", err)
	}
	if w == nil {
		t.Fatal("wireSyncWorker returned no worker for configured storage")
	}

	rec, ok, err := st.SyncJobByID(ctx, jobID)
	if err != nil || !ok {
		t.Fatalf("SyncJobByID(%d) = (ok=%v, err=%v)", jobID, ok, err)
	}
	if rec.Status != store.SyncJobQueued {
		t.Errorf("held job at boot = %q (%s), want %q before the worker starts",
			rec.Status, rec.HeldReason, store.SyncJobQueued)
	}
	if rec.HeldReason != "" {
		t.Errorf("held_reason = %q, want it cleared with the release", rec.HeldReason)
	}
}

// seedBootHeldJob inserts one held sync job on a real vault track: the rig has
// the arr row the job points at and a target file that already passes the gate.
func seedBootHeldJob(t *testing.T, st *store.Store, dir, target string) int64 {
	t.Helper()
	ctx := context.Background()

	res, err := st.DB.ExecContext(ctx,
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4)
		 VALUES ('main', 'radarr', 'http://radarr.invalid', x'00', '0000')`)
	if err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	instanceID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("instance id: %v", err)
	}
	res, err = st.DB.ExecContext(ctx,
		`INSERT INTO arr_library_cache
		 (arr_instance_id, media_type, external_item_id, title, original_language_iso2,
		  year, target_file_path, eligibility_status)
		 VALUES (?, 'movie', 7, 'Movie', 'en', 2020, ?, 'eligible')`,
		instanceID, target)
	if err != nil {
		t.Fatalf("insert library cache: %v", err)
	}
	cacheID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("cache id: %v", err)
	}

	mediaID := insertBootVaultMedia(t, st, dir)
	res, err = st.DB.ExecContext(ctx,
		`INSERT INTO audio_tracks
		 (vault_media_id, group_uuid, lang_iso2, lang_iso3, variant, file_path, sha256, codec, channels, channel_count)
		 VALUES (?, 'boot-group', 'pl', 'pol', 'dubbing', ?, '00', 'opus', '2.0', 2)`,
		mediaID, filepath.Join(dir, "track.pl.mka"))
	if err != nil {
		t.Fatalf("insert audio track: %v", err)
	}
	trackID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("audio track id: %v", err)
	}

	rec, err := st.CreateSyncJob(ctx, store.SyncJobCreate{
		VaultTrackID:         trackID,
		TargetLibraryCacheID: cacheID,
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

// insertBootVaultMedia writes the vault catalogue row the track belongs to.
func insertBootVaultMedia(t *testing.T, st *store.Store, dir string) int64 {
	t.Helper()
	res, err := st.DB.Exec(
		`INSERT INTO vault_media (media_type, title, year, vault_folder_path)
		 VALUES ('movie', 'Movie', 2020, ?)`, dir)
	if err != nil {
		t.Fatalf("insert vault media: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("vault media id: %v", err)
	}
	return id
}
