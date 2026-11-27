package syncworker

// The claim-time re-check (#48): a job is only ever promoted by a projection
// that ran on an earlier snapshot, and the selection and this load are not one
// transaction. These tests pin that a job whose target no longer passes the gate
// goes back to held before any work is done — never muxed into a file the
// upstream pipeline may still replace — and that the operator's force override
// is the one thing that survives the re-check.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// requireTagFilter makes target.tag_filter enforce a filename tag the rig's
// target does not carry, so the gate refuses it once the job is claimed.
func requireTagFilter(t *testing.T, ss *settings.Store, tags ...string) {
	t.Helper()
	raw, err := json.Marshal(settings.TargetTagFilter{
		RequiredFilenameTags: tags,
		TagMatchMode:         "ANY",
		EnforceTagFilter:     true,
	})
	if err != nil {
		t.Fatalf("marshal tag filter: %v", err)
	}
	if err := ss.Set(context.Background(), settings.KeyTargetTagFilter, raw); err != nil {
		t.Fatalf("set tag filter: %v", err)
	}
}

// syncJobStatus reads the persisted status and held reason of one job.
func syncJobStatus(t *testing.T, st *store.Store, id int64) (string, string) {
	t.Helper()
	rec, ok, err := st.SyncJobByID(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("SyncJobByID(%d) = (ok=%v, err=%v)", id, ok, err)
	}
	return rec.Status, rec.HeldReason
}

// TestProcessOnceReholdsJobWhoseTargetLostEligibility is the re-check: the job
// is queued, the target stopped passing the gate between the projection and the
// claim, and the worker puts it back to held instead of analysing and muxing.
func TestProcessOnceReholdsJobWhoseTargetLostEligibility(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name       string
		removeFile bool
		wantReason string
	}{
		{
			name:       "the target lost its tag",
			wantReason: store.HeldReasonWaitingForTags,
		},
		{
			name:       "the target file vanished",
			removeFile: true,
			wantReason: store.HeldReasonMissingTarget,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			jobID := r.newJob(t)
			// The claim-time projection reads settings live, so the rule that
			// refuses the target is the one the worker sees right now.
			requireTagFilter(t, r.settings, "[NotOnTheTarget]")
			if tc.removeFile {
				if err := os.Remove(r.target); err != nil {
					t.Fatalf("remove target: %v", err)
				}
			}

			ex := &fakeExecutor{report: stableReport()}
			mx := &fakeMuxer{}
			w := newWorker(t, r, ex, mx, &fakeRescanner{})
			if _, err := w.ProcessOnce(ctx); err != nil {
				t.Fatalf("ProcessOnce: %v", err)
			}

			if len(ex.calls) != 0 {
				t.Errorf("Execute calls = %v, want none: the job must be re-held before any analysis", ex.calls)
			}
			if len(mx.jobs) != 0 {
				t.Errorf("Mux calls = %d, want none", len(mx.jobs))
			}
			status, reason := syncJobStatus(t, r.store, jobID)
			if status != store.SyncJobHeld || reason != tc.wantReason {
				t.Errorf("job = %q (%s), want %q/%s", status, reason, store.SyncJobHeld, tc.wantReason)
			}
			if _, err := os.Stat(filepath.Join(r.workDir, "jobs", strconv.FormatInt(jobID, 10))); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("staging directory exists for a re-held job: %v", err)
			}
		})
	}
}

// TestProcessOnceNeverReholdsAForcedJob pins the override: force_ignore_tag_filter
// is the operator's persisted "start it now", so the claim-time re-check lets it
// through even though the target still fails the gate.
func TestProcessOnceNeverReholdsAForcedJob(t *testing.T) {
	r := newRig(t)
	jobID := r.newJob(t)
	requireTagFilter(t, r.settings, "[NotOnTheTarget]")
	if _, err := r.store.DB.Exec(
		`UPDATE sync_mux_jobs SET force_ignore_tag_filter = 1 WHERE id = ?`, jobID); err != nil {
		t.Fatalf("force the job: %v", err)
	}

	ex := &fakeExecutor{report: stableReport()}
	w := newWorker(t, r, ex, &fakeMuxer{}, &fakeRescanner{})
	if _, err := w.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}

	// The fake executor stages nothing, so the run fails after Execute; what the
	// override guarantees is only that the job was never sent back to held.
	if len(ex.calls) != 1 || ex.calls[0] != jobID {
		t.Fatalf("Execute calls = %v, want [%d]: a forced job must not be re-held", ex.calls, jobID)
	}
	if status, reason := syncJobStatus(t, r.store, jobID); status == store.SyncJobHeld {
		t.Errorf("forced job = %q (%s), want it never re-held", status, reason)
	}
}
