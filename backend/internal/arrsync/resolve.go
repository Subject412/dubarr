package arrsync

import (
	"path/filepath"

	"github.com/Subject412/dubarr/backend/internal/gate"
)

// classifyTarget resolves one arr item's target video into a gate verdict.
//
// targetPath is the file *arr reports (movieFile.path / episodeFile.path). An
// item arr has no imported file for is missing_target and never eligible: there
// is nothing to mux. Otherwise the shared Processing Gate rule decides, with the
// item's own cached arr metadata (labels and canonical root folder, ADR-0009)
// so source_mode=arr/both narrows exactly this target. The metadata is the
// snapshot of the current poll, so an arr tag edit is visible on the next pass
// (at most 300 s later).
func classifyTarget(rule gate.Rule, targetPath string, arr gate.ArrMetadata) (status string, matches bool) {
	if targetPath == "" {
		return string(gate.StatusMissingTarget), false
	}
	res := rule.Classify(filepath.Base(targetPath), arr)
	return string(res.Status), res.Status == gate.StatusEligible
}
