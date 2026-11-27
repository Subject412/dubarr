package extract

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Subject412/dubarr/backend/internal/gate"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// JobIntentStore is the slice of *store.Store the sync-job birth needs. It is
// declared here, not in the store, so the extraction owns the seam it calls.
type JobIntentStore interface {
	CreateSyncJob(ctx context.Context, c store.SyncJobCreate) (store.SyncJobRecord, error)
	LibraryCacheJobTargetByID(ctx context.Context, id int64) (store.LibraryCacheJobTarget, bool, error)
}

// BirthSyncJob creates the sync job an approval intended, at the only moment
// its target exists: right after the vault write archived the extracted track.
// Approval records intent and never creates the row, because
// sync_mux_jobs.target_track_id is NOT NULL and references the vault track this
// call has just been handed.
//
// The gate is consulted here and only here. An eligible target is born queued;
// a refused one is born held with the matching reason, so the job waits for the
// projection (sync pass, rescan, boot) instead of burning a mux. The approving
// row's force_ignore_tag_filter wins over the gate and the job starts queued
// even when the target is not tag-ready yet.
//
// The job is always tagged with origin_queue_id, which is how a later reject
// cancels exactly this intent.
func BirthSyncJob(
	ctx context.Context,
	st JobIntentStore,
	rule gate.Rule,
	job store.ImportExtractionJob,
	vaultTrackID int64,
	vaultLangISO2 string,
) (store.SyncJobRecord, error) {
	if job.ID <= 0 {
		return store.SyncJobRecord{}, fmt.Errorf("extract: birth sync job: queue row id must be positive")
	}
	if job.MatchedLibraryCacheID <= 0 {
		return store.SyncJobRecord{}, fmt.Errorf("extract: birth sync job for queue row %d: no matched library target", job.ID)
	}
	if vaultTrackID <= 0 {
		return store.SyncJobRecord{}, fmt.Errorf("extract: birth sync job for queue row %d: target track id must be positive", job.ID)
	}

	lib, hasLib, err := st.LibraryCacheJobTargetByID(ctx, job.MatchedLibraryCacheID)
	if err != nil {
		return store.SyncJobRecord{}, fmt.Errorf("extract: birth sync job for queue row %d: read library target: %w", job.ID, err)
	}

	targetPath := strings.TrimSpace(job.TargetVideoPath)
	if targetPath == "" && hasLib {
		targetPath = strings.TrimSpace(lib.TargetFilePath)
	}

	status, reason := store.SyncJobQueued, ""
	if !job.ForceIgnoreTagFilter {
		switch {
		case targetPath == "" || !fileExists(targetPath):
			status, reason = store.SyncJobHeld, store.HeldReasonMissingTarget
		case !rule.EligiblePath(targetPath, gate.ArrMetadata{
			Found:      hasLib,
			Tags:       lib.ArrTagLabels,
			RootFolder: lib.RootFolderPath,
		}):
			status, reason = store.SyncJobHeld, store.HeldReasonWaitingForTags
		}
	}

	origin := job.ID
	rec, err := st.CreateSyncJob(ctx, store.SyncJobCreate{
		VaultTrackID:         vaultTrackID,
		TargetLibraryCacheID: job.MatchedLibraryCacheID,
		TargetVideoPath:      targetPath,
		TargetLangISO2:       vaultLangISO2,
		Status:               status,
		ForceIgnoreTagFilter: job.ForceIgnoreTagFilter,
		HeldReason:           reason,
		OriginQueueID:        &origin,
	})
	if err != nil {
		return store.SyncJobRecord{}, fmt.Errorf("extract: birth sync job for queue row %d: %w", job.ID, err)
	}
	return rec, nil
}

// fileExists reports whether path is a regular, stat-able file. An unreadable
// path is treated as absent: either way there is nothing to mux now, and the
// projection retries on the next epoch.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}
