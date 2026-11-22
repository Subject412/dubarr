package arrsync

import (
	"path/filepath"

	"github.com/Subject412/dubarr/backend/internal/gate"
)

// classifyTarget resolves one arr item's target video into a gate verdict.
//
// targetPath is the file *arr reports (movieFile.path / episodeFile.path). An
// item arr has no imported file for is missing_target and never eligible: there
// is nothing to mux. Otherwise eligibility is decided by the target's base
// filename through the shared Tdarr Gate rule (ADR-0005), exactly as the watch
// path and the approval queue decide it.
func classifyTarget(rule gate.Rule, targetPath string) (status string, matches bool) {
	if targetPath == "" {
		return string(gate.StatusMissingTarget), false
	}
	res := rule.Classify(filepath.Base(targetPath))
	return string(res.Status), res.Status == gate.StatusEligible
}
