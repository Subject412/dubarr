package sync

import (
	"errors"
	"fmt"
)

// Kind classifies an engine failure.
type Kind string

const (
	// KindInvalidJob means the caller built a job the engine cannot accept;
	// it is rejected before any file is opened.
	KindInvalidJob Kind = "invalid_job"
	// KindAnalysis means the audio could not be analysed (unreadable,
	// mismatched or silent slices, no valid lag, ...).
	KindAnalysis Kind = "analysis"
	// KindTimeout means the job exceeded the runner deadline.
	KindTimeout Kind = "timeout"
	// KindExecution covers everything else, including a recovered panic from
	// inside the engine.
	KindExecution Kind = "execution"
)

// Sentinel errors for errors.Is. Every *Error matches its Kind sentinel.
var (
	ErrInvalidJob = errors.New("sync: invalid job")
	ErrAnalysis   = errors.New("sync: analysis failed")
	ErrTimeout    = errors.New("sync: job timed out")
	ErrExecution  = errors.New("sync: execution failed")
)

func sentinelFor(kind Kind) error {
	switch kind {
	case KindInvalidJob:
		return ErrInvalidJob
	case KindAnalysis:
		return ErrAnalysis
	case KindTimeout:
		return ErrTimeout
	default:
		return ErrExecution
	}
}

// Error is the typed failure returned by [Runner.Run].
//
// Stderr carries a one-line diagnostic for failures that have one — today only
// the recovered panic's stack, collapsed onto a single line so the message can
// be surfaced in a job status row without formatting surprises.
type Error struct {
	Kind   Kind
	Stderr string
	Err    error
}

// Error implements the error interface. The message stays one line so it can be
// surfaced in job status rows without formatting surprises.
func (e *Error) Error() string {
	msg := string(e.Kind)
	if e.Err != nil {
		msg = e.Err.Error()
	}
	if e.Stderr != "" {
		msg = msg + ": " + e.Stderr
	}
	return msg
}

// Unwrap exposes the underlying cause to errors.Is/As.
func (e *Error) Unwrap() error { return e.Err }

// Is matches the sentinel for this Error's Kind, so callers can branch with
// errors.Is(err, sync.ErrAnalysis) instead of type-switching.
func (e *Error) Is(target error) bool { return target == sentinelFor(e.Kind) }

// KindOf reports the Kind of an engine error, if err is one (possibly wrapped).
func KindOf(err error) (Kind, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind, true
	}
	return "", false
}

// SampleRateMismatch reports two staged WAVs sampled at different rates. The
// message is the engine's own wording (lib.rs), so log lines keep their shape.
type SampleRateMismatch struct {
	Target uint32
	Donor  uint32
}

// Error implements error.
func (e *SampleRateMismatch) Error() string {
	return fmt.Sprintf("sample rate mismatch: target %d Hz vs donor %d Hz (M4-1 requires identical rates)",
		e.Target, e.Donor)
}

// SliceOutOfRange reports a requested slice that starts past the end of a
// staged WAV.
type SliceOutOfRange struct {
	StartSec    float64
	DurationSec float64
}

// Error implements error, rendering the engine's `slice starts at … beyond
// audio duration …`.
func (e *SliceOutOfRange) Error() string {
	return fmt.Sprintf("slice starts at %.3fs beyond audio duration %.3fs", e.StartSec, e.DurationSec)
}
