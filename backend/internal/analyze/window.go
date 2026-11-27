package analyze

import (
	"context"
	"errors"
)

// Analysis source modes: which audio a checkpoint's engine job reads.
const (
	// AnalysisSourceWindows analyses windows cut from the sources at the
	// planned positions. It needs a probed duration, because every
	// percentage is taken of it (see [ErrNoDuration]).
	AnalysisSourceWindows = "windows"
	// AnalysisSourceFull analyses the two fully decoded sources whole: the
	// audio the engine reads is the track, frame 0 is the film start, and
	// the duration can be read from the staged WAV headers.
	AnalysisSourceFull = "full"
)

// ErrNoDuration is what a windowed analysis returns when the request carries
// no duration. Every checkpoint percentage is taken of that duration, so
// without it the plan would clamp every position to the start of the file and
// six identical windows would answer as one — the silent finding-1 signature.
// It is returned before any slicer is asked for audio, so a missing duration
// is a loud configuration failure rather than a suspiciously stable report.
//
// Like [ErrShortWindow], it carries no "analyze:" prefix: both are always
// surfaced through Analyze's own wrapping, which does.
var ErrNoDuration = errors.New("windowed analysis needs a positive duration")

// ErrShortWindow is what a checkpoint returns when the engine reports having
// analysed fewer samples than the plan asked for. The echo (sync.Analyzed.Len)
// is used for exactly this and nothing else: a slice that lost audio scores
// its peak on less material, which is a weak checkpoint masquerading as data.
// The caller treats it as "window suspect" (re-decode or fall back), never as
// a real offset.
var ErrShortWindow = errors.New("engine analysed less audio than planned")

// Slice is what the engine is given for one window: the files to read and the
// offset of the analysed audio inside them (the pre-roll in windowed mode, the
// film start in whole-file mode), plus the length to analyse.
//
// EngineStartSec is a position inside TargetPath/DonorPath, never a position
// in the movie: the engine addresses the files it is handed, and reporting a
// film start as an engine offset is what made a windowed job read past the end
// of its window WAV (SliceOutOfRange). The film position travels separately,
// in [Window.FilmStartSec], and is what the report states.
type Slice struct {
	TargetPath, DonorPath  string
	EngineStartSec, LenSec float64
}

// Slicer produces the audio one checkpoint's engine job reads.
//
// Implementations must be safe for concurrent use: [Analyze] runs up to
// Config.ParallelCheckpoints checkpoints at once, each calling Slice exactly
// once per checkpoint from its own goroutine. Results are indexed by
// checkpoint and aggregated single-threaded after the join, so no ordering
// between calls is assumed or required.
//
// Release is called once for every Window a Slice was attempted for, whether
// or not it succeeded, so an implementation that writes files can remove them
// as soon as that checkpoint's job is done. It too may run concurrently.
type Slicer interface {
	Slice(ctx context.Context, w Window) (Slice, error)
	Release(w Window)
}

// wholeFileSlicer is the identity slicer: the engine reads the two fully
// decoded WAVs from the top and the analysed audio starts at the planned film
// position, because a whole-file decode's frame 0 *is* the film start. It
// keeps no state, so it is trivially safe for concurrent use.
type wholeFileSlicer struct {
	TargetPath, DonorPath string
}

// Slice returns the whole-file job for w.
func (s wholeFileSlicer) Slice(_ context.Context, w Window) (Slice, error) {
	return Slice{
		TargetPath:     s.TargetPath,
		DonorPath:      s.DonorPath,
		EngineStartSec: w.FilmStartSec,
		LenSec:         w.LenSec,
	}, nil
}

// Release is a no-op: a whole-file job owns its files.
func (wholeFileSlicer) Release(Window) {}
