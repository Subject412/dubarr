package gate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Subject412/dubarr/backend/internal/store"
)

// CandidateStore is the persistence seam Service needs: recording the gate
// verdict on an import-queue candidate.
type CandidateStore interface {
	SetImportCandidateGate(ctx context.Context, sourcePath string, status store.ImportGateStatus) error
}

// RuleSource returns the gate rule to use for one classification.
//
// It is read per candidate rather than bound once at construction, so an
// operator editing target.tag_filter changes the verdict of the next
// classification without a restart (the #53 class of bug).
type RuleSource func(ctx context.Context) (Rule, error)

// Service classifies import-queue candidates and records the verdict.
//
// The import queue carries donor drops, and donor ingestion is never gated by
// arr metadata (ADR-0005): a donor file has no arr library row, so Service
// evaluates the rule with its arr source disabled. Target eligibility (arr
// tags/root folders, ADR-0009) is decided by the arr poller and re-checked by
// the muxer, which both carry the target's ArrMetadata.
//
// The rule comes from a RuleSource on every call. When the source fails, the
// last known-good rule keeps classifying: failing open (treating the gate as
// unenforced) would mux an ineligible target, the one outcome this gate exists
// to prevent. With no known-good rule yet the error is returned instead.
type Service struct {
	source RuleSource
	store  CandidateStore
	// logf reports a rule-source failure. Nil means log.Printf.
	logf func(format string, args ...any)

	// mu guards the last-known-good rule and the last logged error signature;
	// ClassifyCandidate can run concurrently.
	mu         sync.Mutex
	lastRule   Rule
	hasRule    bool
	lastErrSig string
}

// NewService wires the live gate rule source and the import-queue store.
func NewService(source RuleSource, st CandidateStore) *Service {
	return &Service{source: source, store: st, logf: log.Printf}
}

// NewServiceWithRule wires a fixed rule. It keeps callers that hold a settled
// rule (and every test) free of a closure while sharing one implementation.
func NewServiceWithRule(rule Rule, st CandidateStore) *Service {
	return NewService(func(context.Context) (Rule, error) { return rule, nil }, st)
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
			rule, err := s.currentRule(ctx)
			if err != nil {
				return Result{}, err
			}
			res = rule.FilenameOnly().Classify(filepath.Base(targetPath))
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

// currentRule reads the rule for this classification.
//
// A source failure is never fatal once a rule is known: the last known-good
// rule is reused so the gate stays enforced, and the failure is logged once
// per distinct error signature rather than on every candidate. Before any rule
// is known there is nothing safe to classify with, so the error is returned.
func (s *Service) currentRule(ctx context.Context) (Rule, error) {
	if s.source == nil {
		return Rule{}, errors.New("gate: rule source is not configured")
	}
	rule, err := s.source(ctx)
	if err == nil {
		s.mu.Lock()
		s.lastRule, s.hasRule, s.lastErrSig = rule, true, ""
		s.mu.Unlock()
		return rule, nil
	}

	s.mu.Lock()
	last, known, sig := s.lastRule, s.hasRule, s.lastErrSig
	report := known && sig != err.Error()
	if report {
		s.lastErrSig = err.Error()
	}
	s.mu.Unlock()

	if !known {
		return Rule{}, fmt.Errorf("gate: read tag filter: %w", err)
	}
	if report {
		if s.logf != nil {
			s.logf("gate: %v — keeping the last known-good tag filter", err)
		}
	}
	return last, nil
}
