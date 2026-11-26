package gate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// tagRule builds a filename-only rule that requires one marker, so a target
// without it is waiting_for_tags and a target with it is eligible.
func tagRule(required ...string) Rule {
	return RuleFromTagFilter(settings.TargetTagFilter{
		RequiredFilenameTags: required,
		TagMatchMode:         "ANY",
		EnforceTagFilter:     true,
	})
}

func newTestService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	st := openTestStore(t)
	return NewServiceWithRule(tagRule("[ArcA310]"), st), st
}

// candidate writes a donor file and registers it in the import queue, so a
// classification has a row to persist its verdict onto.
func candidate(t *testing.T, st *store.Store, dir string) string {
	t.Helper()
	source := filepath.Join(dir, "donor.mp3")
	if err := os.WriteFile(source, []byte("donor"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if _, err := st.RecordImportCandidate(context.Background(), source, filepath.Base(source), 5); err != nil {
		t.Fatalf("RecordImportCandidate: %v", err)
	}
	return source
}

func targetFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("target"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	return path
}

func TestServiceClassifiesAndPersists(t *testing.T) {
	ctx := context.Background()
	svc, st := newTestService(t)
	dir := t.TempDir()
	source := candidate(t, st, dir)

	// ADR-0005: only the filename carries eligibility, so no container tag is
	// written — the marker lives in the name.
	targetPath := targetFile(t, dir, "target [ArcA310].mkv")

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
	source := candidate(t, st, dir)

	res, err := svc.ClassifyCandidate(ctx, source, targetFile(t, dir, "untagged.mkv"))
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
	source := candidate(t, st, dir)

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

// TestServiceRuleSourceAppliesToNextClassification is U1.5: a target.tag_filter
// edit changes the verdict of the very next candidate, with no restart.
func TestServiceRuleSourceAppliesToNextClassification(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	dir := t.TempDir()
	source := candidate(t, st, dir)
	target := targetFile(t, dir, "untagged.mkv")

	var mu sync.Mutex
	current := tagRule("[ArcA310]")
	svc := NewService(func(context.Context) (Rule, error) {
		mu.Lock()
		defer mu.Unlock()
		return current, nil
	}, st)

	res, err := svc.ClassifyCandidate(ctx, source, target)
	if err != nil {
		t.Fatalf("ClassifyCandidate: %v", err)
	}
	if res.Status != StatusWaitingForTags {
		t.Fatalf("status = %q, want waiting_for_tags before the edit", res.Status)
	}

	// The operator clears the required list; nothing is re-wired or restarted.
	mu.Lock()
	current = tagRule()
	mu.Unlock()

	res, err = svc.ClassifyCandidate(ctx, source, target)
	if err != nil {
		t.Fatalf("ClassifyCandidate after edit: %v", err)
	}
	if res.Status != StatusEligible {
		t.Errorf("status = %q, want eligible on the next classification after the edit", res.Status)
	}
	persisted, ok, err := st.ImportCandidateGate(ctx, source)
	if err != nil || !ok {
		t.Fatalf("ImportCandidateGate: %v ok=%v", err, ok)
	}
	if persisted.Status != string(StatusEligible) {
		t.Errorf("persisted status = %q, want eligible", persisted.Status)
	}
}

// TestServiceRuleSourceErrorKeepsLastKnownGood pins fail-closed behaviour: a
// failing rule source must not classify the target as eligible.
func TestServiceRuleSourceErrorKeepsLastKnownGood(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	dir := t.TempDir()
	source := candidate(t, st, dir)
	target := targetFile(t, dir, "untagged.mkv")

	var mu sync.Mutex
	current := tagRule("[ArcA310]")
	var fail bool
	svc := NewService(func(context.Context) (Rule, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return Rule{}, errors.New("settings: database is locked")
		}
		return current, nil
	}, st)
	svc.logf = func(string, ...any) {}

	res, err := svc.ClassifyCandidate(ctx, source, target)
	if err != nil {
		t.Fatalf("ClassifyCandidate: %v", err)
	}
	if res.Status != StatusWaitingForTags {
		t.Fatalf("status = %q, want waiting_for_tags before the failure", res.Status)
	}

	mu.Lock()
	fail = true
	mu.Unlock()

	res, err = svc.ClassifyCandidate(ctx, source, target)
	if err != nil {
		t.Fatalf("ClassifyCandidate with a failing source: %v", err)
	}
	if res.Status != StatusWaitingForTags {
		t.Errorf("status = %q, want waiting_for_tags (the last known-good rule), never eligible", res.Status)
	}
	persisted, ok, err := st.ImportCandidateGate(ctx, source)
	if err != nil || !ok {
		t.Fatalf("ImportCandidateGate: %v ok=%v", err, ok)
	}
	if persisted.Status != string(StatusWaitingForTags) {
		t.Errorf("persisted status = %q, want waiting_for_tags", persisted.Status)
	}
}

// TestServiceRuleSourceErrorWithoutKnownRuleFails covers a source that has
// never produced a rule: there is nothing safe to classify with, so the error
// surfaces instead of a guessed verdict.
func TestServiceRuleSourceErrorWithoutKnownRuleFails(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	dir := t.TempDir()
	source := candidate(t, st, dir)
	target := targetFile(t, dir, "target [ArcA310].mkv")

	svc := NewService(func(context.Context) (Rule, error) {
		return Rule{}, errors.New("settings: database is locked")
	}, st)
	svc.logf = func(string, ...any) {}

	if _, err := svc.ClassifyCandidate(ctx, source, target); err == nil {
		t.Fatal("classifying without any rule returned no error")
	}
	if persisted, ok, err := st.ImportCandidateGate(ctx, source); err != nil || ok {
		t.Errorf("gate verdict persisted without a rule: %+v ok=%v err=%v", persisted, ok, err)
	}
}

// TestServiceRuleSourceErrorLogsOnce checks the log is throttled by error
// signature rather than once per candidate.
func TestServiceRuleSourceErrorLogsOnce(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	dir := t.TempDir()
	source := candidate(t, st, dir)
	target := targetFile(t, dir, "untagged.mkv")

	var mu sync.Mutex
	current := tagRule("[ArcA310]")
	errText := "settings: database is locked"
	var fail bool
	svc := NewService(func(context.Context) (Rule, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return Rule{}, errors.New(errText)
		}
		return current, nil
	}, st)

	var logsMu sync.Mutex
	var logs []string
	svc.logf = func(format string, args ...any) {
		logsMu.Lock()
		defer logsMu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	if _, err := svc.ClassifyCandidate(ctx, source, target); err != nil {
		t.Fatalf("ClassifyCandidate: %v", err)
	}

	mu.Lock()
	fail = true
	mu.Unlock()
	for i := 0; i < 3; i++ {
		if _, err := svc.ClassifyCandidate(ctx, source, target); err != nil {
			t.Fatalf("ClassifyCandidate #%d: %v", i, err)
		}
	}

	logsMu.Lock()
	got := len(logs)
	logsMu.Unlock()
	if got != 1 {
		t.Fatalf("logged %d times across 3 failures, want 1", got)
	}

	// A different failure is a different signature, so it is reported again.
	mu.Lock()
	errText = "settings: no such table"
	mu.Unlock()
	if _, err := svc.ClassifyCandidate(ctx, source, target); err != nil {
		t.Fatalf("ClassifyCandidate with a new failure: %v", err)
	}
	logsMu.Lock()
	got = len(logs)
	logsMu.Unlock()
	if got != 2 {
		t.Errorf("logged %d times after a distinct failure, want 2", got)
	}
}
