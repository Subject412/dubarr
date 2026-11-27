package extract

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/gate"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// birthRig is a store holding one approved import plus the vault track its
// extraction would have archived, so BirthSyncJob sees exactly what production
// hands it: the queue row's intent and a real target_track_id.
type birthRig struct {
	t       *testing.T
	store   *store.Store
	job     store.ImportExtractionJob
	trackID int64
	dir     string
}

func newBirthRig(t *testing.T, targetPath string, force bool) *birthRig {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

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
		 (arr_instance_id, media_type, external_item_id, title, original_language_iso2, year,
		  target_file_path, eligibility_status)
		 VALUES (?, 'movie', 7, 'Shrek', 'en', 2001, ?, 'eligible')`,
		instanceID, targetPath)
	if err != nil {
		t.Fatalf("insert library cache: %v", err)
	}
	cacheID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("cache id: %v", err)
	}

	donor := filepath.Join(dir, "Shrek 2001 PL.DUB.mkv")
	if err := os.WriteFile(donor, []byte("donor"), 0o644); err != nil {
		t.Fatalf("write donor: %v", err)
	}
	source := filepath.Join(dir, "donor.mp4")
	created, err := st.RecordImportCandidate(ctx, source, "donor.mp4", 11)
	if err != nil || !created {
		t.Fatalf("RecordImportCandidate: created=%v err=%v", created, err)
	}
	var queueID int64
	if err := st.DB.QueryRowContext(ctx, `SELECT id FROM import_queue WHERE source_file_path = ?`, source).Scan(&queueID); err != nil {
		t.Fatalf("queue id: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE import_queue SET matched_arr_instance_id = ?, matched_library_cache_id = ? WHERE id = ?`,
		instanceID, cacheID, queueID); err != nil {
		t.Fatalf("link candidate: %v", err)
	}
	if err := st.ApproveImportCandidate(ctx, queueID, store.ImportApproval{
		DonorPath:            donor,
		ForceIgnoreTagFilter: force,
	}); err != nil {
		t.Fatalf("ApproveImportCandidate: %v", err)
	}

	job, ok, err := st.NextImportExtraction(ctx)
	if err != nil || !ok {
		t.Fatalf("NextImportExtraction = (ok=%v, err=%v)", ok, err)
	}

	// The vault track the extraction would have just archived.
	res, err = st.DB.ExecContext(ctx,
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
		 VALUES (?, 'birth-group', 'pl', 'pol', 'dubbing', ?, '00', 'opus', '2.0', 2)`,
		mediaID, filepath.Join(dir, "Shrek (2001).pl.mka"))
	if err != nil {
		t.Fatalf("insert audio track: %v", err)
	}
	trackID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("audio track id: %v", err)
	}
	return &birthRig{t: t, store: st, job: job, trackID: trackID, dir: dir}
}

func taggedRule() gate.Rule {
	return gate.Rule{Required: []string{"[ArcA310]"}, Mode: gate.MatchAny, Enforce: true}
}

// TestBirthSyncJobHoldsOrQueuesAtCreation is the ruling's heart: the job is
// born where the track exists, queued when the target passes the gate and held
// with the matching reason when it does not. The override the operator recorded
// at approval wins over the gate; a target that vanished is missing_target
// rather than a tag verdict.
func TestBirthSyncJobHoldsOrQueuesAtCreation(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name       string
		targetName string
		missing    bool
		force      bool
		wantStatus string
		wantReason string
	}{
		{
			name:       "eligible target queues the job",
			targetName: "Shrek (2001) [ArcA310].mkv",
			wantStatus: store.SyncJobQueued,
		},
		{
			name:       "untagged target is held for its tags",
			targetName: "Shrek (2001).mkv",
			wantStatus: store.SyncJobHeld,
			wantReason: store.HeldReasonWaitingForTags,
		},
		{
			name:       "vanished target is held as missing",
			targetName: "Shrek (2001) [ArcA310].mkv",
			missing:    true,
			wantStatus: store.SyncJobHeld,
			wantReason: store.HeldReasonMissingTarget,
		},
		{
			name:       "the approval's override starts the job now",
			targetName: "Shrek (2001).mkv",
			force:      true,
			wantStatus: store.SyncJobQueued,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, tc.targetName)
			if !tc.missing {
				if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
					t.Fatalf("write target: %v", err)
				}
			}
			rig := newBirthRig(t, target, tc.force)
			if rig.job.TargetVideoPath != target {
				t.Fatalf("target_video_path = %q, want %q", rig.job.TargetVideoPath, target)
			}
			if rig.job.ForceIgnoreTagFilter != tc.force {
				t.Fatalf("recorded force = %v, want %v", rig.job.ForceIgnoreTagFilter, tc.force)
			}

			rec, err := BirthSyncJob(ctx, rig.store, taggedRule(), rig.job, rig.trackID, "pl")
			if err != nil {
				t.Fatalf("BirthSyncJob: %v", err)
			}
			if rec.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", rec.Status, tc.wantStatus)
			}
			if rec.HeldReason != tc.wantReason {
				t.Errorf("held_reason = %q, want %q", rec.HeldReason, tc.wantReason)
			}
			if rec.ForceIgnoreTagFilter != tc.force {
				t.Errorf("persisted force = %v, want %v", rec.ForceIgnoreTagFilter, tc.force)
			}
			if rec.OriginQueueID != rig.job.ID {
				t.Errorf("origin_queue_id = %d, want %d", rec.OriginQueueID, rig.job.ID)
			}
			if rec.TargetTrackID != rig.trackID || rec.TargetLangISO2 != "pl" {
				t.Errorf("track = %d/%q, want %d/pl", rec.TargetTrackID, rec.TargetLangISO2, rig.trackID)
			}
			if rec.TargetLibraryCacheID != rig.job.MatchedLibraryCacheID {
				t.Errorf("library target = %d, want %d", rec.TargetLibraryCacheID, rig.job.MatchedLibraryCacheID)
			}
		})
	}
}

// TestBirthSyncJobRefusesWithoutALibraryTarget pins the boundary: a job cannot
// exist without the arr identity that names its target, so the birth fails
// loudly instead of inserting a half-keyed row.
func TestBirthSyncJobRefusesWithoutALibraryTarget(t *testing.T) {
	rig := newBirthRig(t, filepath.Join(t.TempDir(), "Shrek (2001) [ArcA310].mkv"), false)
	job := rig.job
	job.MatchedLibraryCacheID = 0

	if _, err := BirthSyncJob(context.Background(), rig.store, taggedRule(), job, rig.trackID, "pl"); err == nil {
		t.Fatal("BirthSyncJob without a library target = nil error, want a refusal")
	}
}
