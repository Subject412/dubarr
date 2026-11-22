package sync

import (
	"errors"
	"fmt"
)

// Kind classifies a bridge failure. The values map onto the engine's exit
// codes (2 usage, 3 analysis) plus the failures the bridge detects itself.
type Kind string

const (
	// KindInvalidJob means the caller built a job the engine could not accept;
	// the bridge rejects it before spawning the binary.
	KindInvalidJob Kind = "invalid_job"
	// KindUnavailable means the dubarr-sync binary could not be found or
	// executed. This is the condition /api/health reports as
	// dubarr-sync.available=false.
	KindUnavailable Kind = "unavailable"
	// KindUsage is exit code 2: invalid job JSON or semantically invalid job.
	KindUsage Kind = "usage"
	// KindAnalysis is exit code 3: the engine could not analyse the audio
	// (unreadable/mismatched/silent slices, no valid lag, ...).
	KindAnalysis Kind = "analysis"
	// KindMalformedOutput means the engine exited 0 but its stdout was not a
	// single valid result object.
	KindMalformedOutput Kind = "malformed_output"
	// KindTimeout means the job exceeded the bridge deadline and was killed.
	KindTimeout Kind = "timeout"
	// KindExecution covers other process failures (signals, start errors,
	// unexpected exit codes) that fit none of the above.
	KindExecution Kind = "execution"
)

// Sentinel errors for errors.Is. Every *Error matches its Kind sentinel.
var (
	ErrInvalidJob      = errors.New("sync: invalid job")
	ErrUnavailable     = errors.New("sync: dubarr-sync unavailable")
	ErrUsage           = errors.New("sync: dubarr-sync usage error")
	ErrAnalysis        = errors.New("sync: dubarr-sync analysis failed")
	ErrMalformedOutput = errors.New("sync: malformed dubarr-sync output")
	ErrTimeout         = errors.New("sync: dubarr-sync timed out")
	ErrExecution       = errors.New("sync: dubarr-sync execution failed")
)

func sentinelFor(kind Kind) error {
	switch kind {
	case KindInvalidJob:
		return ErrInvalidJob
	case KindUnavailable:
		return ErrUnavailable
	case KindUsage:
		return ErrUsage
	case KindAnalysis:
		return ErrAnalysis
	case KindMalformedOutput:
		return ErrMalformedOutput
	case KindTimeout:
		return ErrTimeout
	default:
		return ErrExecution
	}
}

// Error is the typed failure returned by [Runner.Run] and [New].
//
// Stderr carries whatever the engine wrote to stderr (already trimmed); it is
// safe to log, as the engine never echoes secret material.
type Error struct {
	Kind     Kind
	ExitCode int
	Stderr   string
	Err      error
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
	if e.ExitCode != 0 {
		msg = fmt.Sprintf("%s (exit %d)", msg, e.ExitCode)
	}
	return msg
}

// Unwrap exposes the underlying cause to errors.Is/As.
func (e *Error) Unwrap() error { return e.Err }

// Is matches the sentinel for this Error's Kind, so callers can branch with
// errors.Is(err, sync.ErrAnalysis) instead of type-switching.
func (e *Error) Is(target error) bool { return target == sentinelFor(e.Kind) }

// KindOf reports the Kind of a bridge error, if err is one (possibly wrapped).
func KindOf(err error) (Kind, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind, true
	}
	return "", false
}
