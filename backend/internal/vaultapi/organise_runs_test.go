package vaultapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/store"
)

// attentionRun writes one audit run with the given status and file rows.
func attentionRun(t *testing.T, h *organiseHarness, status string, files []store.OrganiseFileRecord) int64 {
	t.Helper()
	runID, err := h.st.InsertOrganiseRun(context.Background(), store.OrganiseRunRecord{
		PlanHash:  "sha256:" + status,
		FileCount: len(files),
		Status:    status,
		Detail:    "stage=sql: injected",
		Files:     files,
	})
	if err != nil {
		t.Fatalf("InsertOrganiseRun(%s): %v", status, err)
	}
	return runID
}

// rawRuns returns the history payload with its keys intact, so a test can tell
// an omitted "files" field from an empty one.
func rawRuns(t *testing.T, h *organiseHarness) []map[string]json.RawMessage {
	t.Helper()
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/vault/organise/runs?limit=50", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("runs = %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode runs: %v (%s)", err, rec.Body.String())
	}
	return resp.Items
}

// TestRunsExposeOnlyTheFilesToInspect pins #127: a needs_attention run carries
// its reconciled and failed rows and nothing else; renamed and renamed_back are
// the run's ordinary business and stay out of the payload.
func TestRunsExposeOnlyTheFilesToInspect(t *testing.T) {
	h := newOrganiseHarness(t)
	runID := attentionRun(t, h, store.OrganiseStatusNeedsAttention, []store.OrganiseFileRecord{
		// Inserted out of path order on purpose: the read orders by source path.
		{SourcePath: "/vault/Films/zeta.mka", DestinationPath: "/vault/Films/zeta new.mka", Result: store.OrganiseResultFailed},
		{SourcePath: "/vault/Films/alpha.mka", DestinationPath: "/vault/Films/alpha new.mka", Result: store.OrganiseResultReconciled},
		{SourcePath: "/vault/Films/beta.mka", DestinationPath: "/vault/Films/beta new.mka", Result: store.OrganiseResultRenamed},
		{SourcePath: "/vault/Films/gamma.mka", DestinationPath: "/vault/Films/gamma.mka", Result: store.OrganiseResultRenamedBack},
	})

	runs := h.runs()
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want the one seeded run", runs)
	}
	run := runs[0]
	if run.ID != runID || run.Status != store.OrganiseStatusNeedsAttention {
		t.Fatalf("run = %+v, want id %d needs_attention", run, runID)
	}
	// file_count is the whole run's count, not the inspectable subset.
	if run.FileCount != 4 {
		t.Fatalf("file_count = %d, want 4", run.FileCount)
	}
	if len(run.Files) != 2 {
		t.Fatalf("files = %+v, want exactly the reconciled and failed rows", run.Files)
	}

	want := []RunFileRow{
		{
			SourcePath:      "/vault/Films/alpha.mka",
			DestinationPath: "/vault/Films/alpha new.mka",
			Result:          store.OrganiseResultReconciled,
		},
		{
			SourcePath:      "/vault/Films/zeta.mka",
			DestinationPath: "/vault/Films/zeta new.mka",
			Result:          store.OrganiseResultFailed,
		},
	}
	for i, row := range want {
		if run.Files[i] != row {
			t.Fatalf("file %d = %+v, want %+v", i, run.Files[i], row)
		}
	}
}

// TestRunsOmitFilesForAppliedAndQuietRuns pins the other half of #127: the
// ordinary payload stays one row per run, so "files" is absent — not empty — for
// an applied run (even though its audit rows exist) and for a needs_attention
// run that recorded no file rows at all.
func TestRunsOmitFilesForAppliedAndQuietRuns(t *testing.T) {
	h := newOrganiseHarness(t)
	appliedID := attentionRun(t, h, store.OrganiseStatusApplied, []store.OrganiseFileRecord{
		{SourcePath: "/vault/Films/one.mka", DestinationPath: "/vault/Films/one new.mka", Result: store.OrganiseResultRenamed},
	})
	quietID := attentionRun(t, h, store.OrganiseStatusNeedsAttention, nil)

	items := rawRuns(t, h)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	seen := map[int64]map[string]json.RawMessage{}
	for _, item := range items {
		var id int64
		if err := json.Unmarshal(item["id"], &id); err != nil {
			t.Fatalf("decode id: %v", err)
		}
		seen[id] = item
	}

	for name, id := range map[string]int64{"applied": appliedID, "needs_attention without rows": quietID} {
		item, ok := seen[id]
		if !ok {
			t.Fatalf("%s run %d missing from the payload", name, id)
		}
		if _, present := item["files"]; present {
			t.Fatalf("%s run carries a files key: %s", name, item["files"])
		}
		if _, present := item["file_count"]; !present {
			t.Fatalf("%s run lost file_count, which every row keeps", name)
		}
	}
}
