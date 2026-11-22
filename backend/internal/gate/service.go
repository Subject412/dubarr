package gate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Subject412/dubarr/backend/internal/store"
)

// CandidateStore is the persistence seam Service needs: recording the gate
// verdict on an import-queue candidate.
type CandidateStore interface {
	SetImportCandidateGate(ctx context.Context, sourcePath string, status store.ImportGateStatus) error
}

// Service classifies import-queue candidates and records the verdict.
//
// Classification is filename-only (ADR-0005); container tags are informational
// and are read through Tagger, not consulted here.
type Service struct {
	rule  Rule
	store CandidateStore
}

// NewService wires the gate rule and the import-queue store.
func NewService(rule Rule, st CandidateStore) *Service {
	return &Service{rule: rule, store: st}
}

// ClassifyCandidate evaluates sourcePath's target filename and persists the
// result on the candidate.
//
// An empty or missing target yields missing_target without reading the file
// beyond a stat. A target that cannot be stat'ed is an error, not a verdict:
// silently marking it waiting_for_tags would hide a broken path from the
// operator.
func (s *Service) ClassifyCandidate(ctx context.Context, sourcePath, targetPath string) (Result, error) {
	res := Result{TargetPath: targetPath, Matched: []string{}, Missing: []string{}}

	switch {
	case targetPath == "":
		res.Status = StatusMissingTarget
	default:
		info, err := os.Stat(targetPath)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			res = Result{Status: StatusMissingTarget, TargetPath: targetPath, Matched: []string{}, Missing: []string{}}
		case err != nil:
			return Result{}, fmt.Errorf("gate: stat target %s: %w", targetPath, err)
		case info.IsDir():
			res.Status = StatusMissingTarget
		default:
			res = s.rule.Classify(filepath.Base(targetPath))
			res.TargetPath = targetPath
		}
	}

	if err := s.store.SetImportCandidateGate(ctx, sourcePath, store.ImportGateStatus{
		Status:      string(res.Status),
		TargetPath:  res.TargetPath,
		MatchedTags: res.Matched,
		MissingTags: res.Missing,
		CheckedAt:   time.Now().UTC(),
	}); err != nil {
		return Result{}, err
	}
	return res, nil
}
