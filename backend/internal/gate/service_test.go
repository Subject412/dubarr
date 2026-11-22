package gate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

func newTestService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	rule := RuleFromTagFilter(settings.TargetTagFilter{
		RequiredFilenameTags: []string{"[ArcA310]"},
		TagMatchMode:         "ANY",
		EnforceTagFilter:     true,
	})
	tagger := New(Dependencies{
		MKVMergePath:   mkvTool(t, "mkvmerge"),
		MKVExtractPath: mkvTool(t, "mkvextract"),
	})
	return NewService(rule, tagger, st), st
}

func TestServiceClassifiesAndPersists(t *testing.T) {
	ctx := context.Background()
	svc, st := newTestService(t)

	dir := t.TempDir()
	source := filepath.Join(dir, "donor.mp3")
	if err := os.WriteFile(source, []byte("donor"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if _, err := st.RecordImportCandidate(ctx, source, filepath.Base(source), 5); err != nil {
		t.Fatalf("RecordImportCandidate: %v", err)
	}

	tagged := buildTestMKV(t, filepath.Join(dir), "tagged.mkv", "[ArcA310]")
	targetPath := filepath.Join(filepath.Dir(tagged), "target.mkv")
	if err := os.Rename(tagged, targetPath); err != nil {
		t.Fatalf("rename target: %v", err)
	}

	res, err := svc.ClassifyCandidate(ctx, source, targetPath)
	if err != nil {
		t.Fatalf("ClassifyCandidate: %v", err)
	}
	if res.Status != StatusEligible {
		t.Errorf("status = %q, want eligible", res.Status)
	}
	if res.TargetPath != targetPath {
		t.Errorf("target path = %q, want %q", res.TargetPath, targetPath)
	}

	persisted, ok, err := st.ImportCandidateGate(ctx, source)
	if err != nil {
		t.Fatalf("ImportCandidateGate: %v", err)
	}
	if !ok || persisted.Status != string(StatusEligible) || persisted.TargetPath != targetPath {
		t.Errorf("persisted = %+v ok=%v, want eligible at %q", persisted, ok, targetPath)
	}
}

func TestServiceWaitingForTags(t *testing.T) {
	ctx := context.Background()
	svc, st := newTestService(t)

	dir := t.TempDir()
	source := filepath.Join(dir, "donor.mp3")
	if err := os.WriteFile(source, []byte("donor"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if _, err := st.RecordImportCandidate(ctx, source, filepath.Base(source), 5); err != nil {
		t.Fatalf("RecordImportCandidate: %v", err)
	}
	target := buildTestMKV(t, dir, "untagged.mkv", "")

	res, err := svc.ClassifyCandidate(ctx, source, target)
	if err != nil {
		t.Fatalf("ClassifyCandidate: %v", err)
	}
	if res.Status != StatusWaitingForTags {
		t.Errorf("status = %q, want waiting_for_tags", res.Status)
	}
	persisted, ok, err := st.ImportCandidateGate(ctx, source)
	if err != nil || !ok {
		t.Fatalf("ImportCandidateGate: %v ok=%v", err, ok)
	}
	if persisted.Status != string(StatusWaitingForTags) {
		t.Errorf("persisted status = %q, want waiting_for_tags", persisted.Status)
	}
}

func TestServiceMissingTarget(t *testing.T) {
	ctx := context.Background()
	svc, st := newTestService(t)

	dir := t.TempDir()
	source := filepath.Join(dir, "donor.mp3")
	if err := os.WriteFile(source, []byte("donor"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if _, err := st.RecordImportCandidate(ctx, source, filepath.Base(source), 5); err != nil {
		t.Fatalf("RecordImportCandidate: %v", err)
	}

	for _, target := range []string{"", filepath.Join(dir, "gone.mkv")} {
		res, err := svc.ClassifyCandidate(ctx, source, target)
		if err != nil {
			t.Fatalf("ClassifyCandidate(%q): %v", target, err)
		}
		if res.Status != StatusMissingTarget {
			t.Errorf("target %q: status = %q, want missing_target", target, res.Status)
		}
		persisted, ok, err := st.ImportCandidateGate(ctx, source)
		if err != nil || !ok {
			t.Fatalf("ImportCandidateGate: %v ok=%v", err, ok)
		}
		if persisted.Status != string(StatusMissingTarget) {
			t.Errorf("persisted status = %q, want missing_target", persisted.Status)
		}
	}
}

func TestServiceUnknownCandidate(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	if _, err := svc.ClassifyCandidate(ctx, filepath.Join(t.TempDir(), "nope.mp3"), ""); err == nil {
		t.Fatal("classifying an unknown candidate returned no error")
	}
}
