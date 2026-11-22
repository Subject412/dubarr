package mux

import "errors"

// Typed failures. Every abort path returns one of these sentinels (wrapped with
// detail), so callers can map a failed mux onto a job status without string
// matching.
var (
	// ErrNoWorkDir reports an empty WORK_DIR; copy-on-work cannot run.
	ErrNoWorkDir = errors.New("mux: work directory is not configured")
	// ErrInvalidJob reports a job missing an id, target, track language or
	// staged audio outside {WORK_DIR}/jobs/{id}/.
	ErrInvalidJob = errors.New("mux: invalid job")
	// ErrVerdict reports a sync report whose verdict is not muxable. Only
	// ok|stable may proceed (PLAN.md M5-1); drifting, unreliable and
	// out_of_range abort loudly.
	ErrVerdict = errors.New("mux: sync report is not muxable")
	// ErrAmbiguous reports a target filename that resolves to more than one
	// queued vault track, or a language mapping that does not resolve to
	// exactly one track. It is checked before any write.
	ErrAmbiguous = errors.New("mux: ambiguous target/track pairing")
	// ErrIneligible reports a Tdarr Gate verdict of waiting_for_tags or
	// missing_target on re-check before muxing.
	ErrIneligible = errors.New("mux: target is not tag-eligible")
	// ErrTargetMissing reports an absent or non-regular target file.
	ErrTargetMissing = errors.New("mux: target file is missing")
	// ErrAudioMissing reports an absent staged .mka.
	ErrAudioMissing = errors.New("mux: staged audio is missing")
	// ErrTool reports a failed mkvmerge invocation.
	ErrTool = errors.New("mux: mkvmerge failed")
	// ErrPublish reports a filesystem failure while copying, backing up or
	// atomically renaming the muxed output.
	ErrPublish = errors.New("mux: atomic publish failed")
)
