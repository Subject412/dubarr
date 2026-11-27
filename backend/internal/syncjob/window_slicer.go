package syncjob

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/Subject412/dubarr/backend/internal/analyze"
)

// windowDecodeFunc is one side of one planned window: the decode plus its
// frame gate. [decodeWindow] is the production implementation; tests replace
// it so the windowed path runs without ffmpeg and without media.
type windowDecodeFunc func(ctx context.Context, ffmpeg, input string, stream int, filter string,
	w analyze.Window, side Side, preroll float64, out string) error

// windowSlicerSpec is one job's window decode: the two analysis inputs, the
// directory the window files live in, and the two timeline corrections the
// plan cannot carry.
type windowSlicerSpec struct {
	ffmpeg       string
	jobDir       string
	targetPath   string
	targetStream int
	donorPath    string
	filter       string
	// targetCorrection and donorCorrection are TrackStart − ContainerStart per
	// input: how far a container seek must be shifted to land on the analysed
	// track's own timeline. They are applied by [windowSlicer] because the
	// plan it receives from analyze.Analyze is built from a single duration
	// base with no origins (Request carries no origin fields), so its
	// Window.TargetSeekSec/DonorSeekSec are the film position uncorrected.
	// Applying a correction of 0 for an unknown origin is safe here: the job
	// only runs windowed when both origins are known.
	targetCorrection float64
	donorCorrection  float64
	// decode is the window decode and frame gate. Nil selects [decodeWindow].
	decode windowDecodeFunc
}

// windowSlicer is the production [analyze.Slicer]: it cuts both sides of one
// planned window out of the sources, verifies each with the frame gate, and
// removes the pair as soon as the checkpoint that asked for it is done.
//
// It is safe for concurrent use: analyze.Analyze calls Slice once per
// checkpoint from that checkpoint's worker (and Release from the same worker),
// each window has its own directory ({jobDir}/win/{pct}), and the only shared
// state — the decoded-seconds counter and the failed-window record — is behind
// one mutex. Nothing is shared between two windows' files.
//
// The decode itself is stateless apart from that job directory, and the whole
// fallback/retry phase runs single-threaded after every worker has joined.
type windowSlicer struct {
	spec windowSlicerSpec

	mu         sync.Mutex
	decodedSec float64
	failed     map[int]windowFailure
}

// windowFailure is one window whose decode or frame gate failed, kept so the
// job can name and retry exactly the window the pass lost.
type windowFailure struct {
	window analyze.Window
	err    error
}

// newWindowSlicer builds the slicer for one job.
func newWindowSlicer(spec windowSlicerSpec) *windowSlicer {
	if spec.decode == nil {
		spec.decode = decodeWindow
	}
	return &windowSlicer{spec: spec, failed: map[int]windowFailure{}}
}

// Slice decodes both sides of w with the settled pre-roll and returns the pair
// the engine reads, starting at the pre-roll that is actually present.
func (s *windowSlicer) Slice(ctx context.Context, w analyze.Window) (analyze.Slice, error) {
	return s.slice(ctx, w, WindowPrerollSec)
}

// Retry re-decodes one window with the longer retry pre-roll. It exists as its
// own call because the fallback policy asks for exactly one window to be
// decoded again, not for the plan to be re-cut.
func (s *windowSlicer) Retry(ctx context.Context, w analyze.Window) (analyze.Slice, error) {
	return s.slice(ctx, w, WindowRetryPrerollSec)
}

// Release removes this window's files. It is called once per Slice attempt,
// from the worker that made it, so at most one checkpoint's pair exists per
// worker at a time.
func (s *windowSlicer) Release(w analyze.Window) {
	s.release(w)
}

// DecodedSeconds is everything this slicer has decoded, both sides, both the
// window itself and its pre-roll. It is the job's structural speed gate: a
// windowed job may decode no more than the plan it was given.
func (s *windowSlicer) DecodedSeconds() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.decodedSec
}

// Failure returns the lowest-position window whose decode or frame gate failed
// on this job, and why. A window cancelled by a sibling's failure is not a
// failure of its own and is never recorded: its decode ends with the context,
// which is what the ctx.Err check in slice filters out.
func (s *windowSlicer) Failure() (analyze.Window, error, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	best := -1
	var found windowFailure
	for pct, failure := range s.failed {
		if best < 0 || pct < best {
			best, found = pct, failure
		}
	}
	if best < 0 {
		return analyze.Window{}, nil, false
	}
	return found.window, found.err, true
}

// slice is Slice and Retry with the pre-roll as an argument.
func (s *windowSlicer) slice(ctx context.Context, w analyze.Window, preroll float64) (analyze.Slice, error) {
	window := s.corrected(w)
	// A single pre-roll both files can honour, so the one number the engine is
	// given (Slice.EngineStartSec) is the truth for both sides: the two seeks
	// differ only by a start-time correction, and near t=0 the available
	// pre-roll is the shorter seek's.
	common := commonPreroll(preroll, window.TargetSeekSec, window.DonorSeekSec)

	sides := []struct {
		side   Side
		input  string
		stream int
		out    string
	}{
		{SideTarget, s.spec.targetPath, s.spec.targetStream, windowFilePath(s.spec.jobDir, w, SideTarget)},
		{SideDonor, s.spec.donorPath, 0, windowFilePath(s.spec.jobDir, w, SideDonor)},
	}
	for _, side := range sides {
		if err := s.spec.decode(ctx, s.spec.ffmpeg, side.input, side.stream, s.spec.filter,
			window, side.side, common, side.out); err != nil {
			s.release(w)
			s.recordFailure(ctx, w, err)
			return analyze.Slice{}, err
		}
		_, present := windowPreroll(side.side.seek(window), common)
		s.addDecoded(present + window.LenSec)
	}
	s.clearFailure(w)

	// Both files were cut with the same pre-roll, so their analysed audio
	// starts at the same offset inside them. The clamps are the head-of-file
	// case: a negative seek cannot pre-roll, and the engine may not be told a
	// negative start.
	_, targetPresent := windowPreroll(SideTarget.seek(window), common)
	_, donorPresent := windowPreroll(SideDonor.seek(window), common)
	return analyze.Slice{
		TargetPath:     sides[0].out,
		DonorPath:      sides[1].out,
		EngineStartSec: math.Max(0, math.Min(targetPresent, donorPresent)),
		LenSec:         window.LenSec,
	}, nil
}

// corrected returns w with each side's seek carrying this input's timeline
// correction, which is how a window planned on the film timeline becomes two
// container seeks.
func (s *windowSlicer) corrected(w analyze.Window) analyze.Window {
	w.TargetSeekSec = w.FilmStartSec + s.spec.targetCorrection
	w.DonorSeekSec = w.FilmStartSec + s.spec.donorCorrection
	w.UnknownOrigin = analyze.UnknownOriginNone
	return w
}

func (s *windowSlicer) release(w analyze.Window) {
	_ = os.RemoveAll(filepath.Join(s.spec.jobDir, "win", strconv.Itoa(w.PositionPct)))
}

func (s *windowSlicer) addDecoded(sec float64) {
	s.mu.Lock()
	s.decodedSec += sec
	s.mu.Unlock()
}

// recordFailure keeps a failed window for the job, unless the failure is only
// the context ending under it (a cancelled sibling, a shutdown).
func (s *windowSlicer) recordFailure(ctx context.Context, w analyze.Window, err error) {
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	s.failed[w.PositionPct] = windowFailure{window: w, err: err}
	s.mu.Unlock()
}

// clearFailure forgets a window that has now decoded: a successful retry
// replaces the failure the first attempt recorded.
func (s *windowSlicer) clearFailure(w analyze.Window) {
	s.mu.Lock()
	delete(s.failed, w.PositionPct)
	s.mu.Unlock()
}

// commonPreroll is the pre-roll both sides can have: the requested one, capped
// by each side's own seek so the two files hold the same amount of audio
// before the window. Without the cap a window near the head of a film would
// give one file more pre-roll than the other, and the single engine start
// cannot describe both.
func commonPreroll(preroll, targetSeek, donorSeek float64) float64 {
	if !(preroll > 0) { // also catches NaN: a pre-roll must be a real, positive number
		return 0
	}
	return math.Max(0, math.Min(preroll, math.Min(targetSeek, donorSeek)))
}
