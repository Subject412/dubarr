package syncjob

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/dsp/wav"
)

const (
	// WindowPrerollSec is decoded ahead of the window and excluded from the
	// correlation so the band filter is in steady state at the first analysed
	// sample. 2 s is ~400x the settling time of an 80 Hz 2nd-order highpass.
	WindowPrerollSec = 2.0
	// WindowRetryPrerollSec is the longer pre-roll used when one window is
	// re-decoded after the fallback policy flags it.
	WindowRetryPrerollSec = 10.0

	// windowSampleRate is the rate every analysis decode is produced at
	// (-ar 8000 in [decodeArgs]) and the rate a window's frame count is
	// checked against. A window file at another rate is refused rather than
	// resampled: a resample would hand the engine samples the plan never
	// promised, and the shorts would show up as a quality drop, not as an
	// error.
	windowSampleRate = 8000

	// minWindowFrames is the correlator's floor (correlate.MinSliceSamples):
	// fewer frames than this is not a short window, it is not a slice at all.
	minWindowFrames = 64
)

// Side names which of a window's two planned seeks one decode uses: the target
// (the arr file whose soundtrack is being vouched for) or the donor (the vault
// track that gets muxed in).
type Side int

const (
	// SideTarget is the pair's target input.
	SideTarget Side = iota
	// SideDonor is the pair's donor input.
	SideDonor
)

// String is the single letter a window's per-side file is named after and the
// word its errors use.
func (s Side) String() string {
	if s == SideDonor {
		return "d"
	}
	return "t"
}

// seek is the container timestamp this side's decode must reach to land on the
// window's film position.
//
// The two sides differ only by the container/track start_time correction the
// planner applied (analyze.Window's Seek fields): an uncorrected seek reads
// audio a constant 500 ms away from the planned position in a muxed MKV whose
// reference stream starts late, which is a plausible-looking offset on every
// checkpoint rather than an error. TestWindowStartTimeCorrection is what keeps
// that correction from being deleted.
func (s Side) seek(w analyze.Window) float64 {
	if s == SideDonor {
		return w.DonorSeekSec
	}
	return w.TargetSeekSec
}

// windowRequest projects one side of one planned window onto the shared decode
// builder.
//
// The input seek lands preroll seconds before the planned position, never
// before 0: a window at the head of a film simply has less pre-roll, and
// [windowPreroll] is what the caller must then tell the engine. The output is
// limited to the pre-roll actually present plus the window itself, so nothing
// about this decode is unbounded.
//
// The pre-roll stays in the file: the caller tells the engine to start reading
// at the pre-roll present (SliceStartSec), so there is no second encode and no
// WAV rewrite, and the samples the engine sees are the ones ffmpeg produced in
// one pass.
func windowRequest(input string, stream int, filter string, w analyze.Window, side Side, preroll float64, out string) decodeRequest {
	start, present := windowPreroll(side.seek(w), preroll)
	duration := present + w.LenSec
	return decodeRequest{
		Input:       input,
		Stream:      stream,
		Filter:      filter,
		Output:      out,
		SeekSec:     &start,
		DurationSec: &duration,
	}
}

// windowPreroll splits a planned seek into the container timestamp to seek to
// and the pre-roll actually present in the resulting file. Near t=0 the second
// number is smaller than preroll and the engine must be told the real one, or
// it would skip audio that is not there.
func windowPreroll(seek, preroll float64) (start, present float64) {
	if !(preroll > 0) { // also catches NaN: a pre-roll must be a real, positive number
		preroll = 0
	}
	start = math.Max(0, seek-preroll)
	return start, seek - start
}

// decodeWindow turns one side of one planned window into a WAV holding exactly
// the planned audio plus its pre-roll, and refuses to return a file that does
// not.
//
// The argv is the shared [decodeArgs] one, so these samples are the samples the
// whole-track decode would have produced for the same span. A failed decode or
// a failed frame gate removes out: a checkpoint must never find a truncated
// window and mistake it for data.
func decodeWindow(ctx context.Context, ffmpeg, input string, stream int, filter string, w analyze.Window, side Side, preroll float64, out string) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return fmt.Errorf("syncjob: create window directory: %w", err)
	}
	req := windowRequest(input, stream, filter, w, side, preroll, out)
	if err := runDecode(ctx, ffmpeg, req); err != nil {
		removeWindow(out)
		return err
	}
	// want is computed from the request, not from the file: the gate's job is
	// to compare what ffmpeg produced with what the plan asked for.
	want := int64(math.Round(*req.DurationSec * windowSampleRate))
	if err := verifyWindow(out, want, windowSampleRate); err != nil {
		removeWindow(out)
		return err
	}
	return nil
}

// ErrShortWindow reports a window file that does not hold the audio the plan
// asked for: fewer than [minWindowFrames] frames, less than 99% of the planned
// frames, or a sample rate that is not the analysis rate.
//
// It is a distinct type on purpose. A window that came back short is a *decode*
// suspect — the fallback policy re-decodes or falls back to a whole-track
// decode — whereas a correlation failure would be reported as an analysis
// failure. Both frames and want are in the message, so a log line says which.
type ErrShortWindow struct {
	Path   string
	Frames int64
	Want   int64
	// Rate and WantRate are set only when the file's sample rate is not the
	// planned one; Frames and Want then count frames at two different rates.
	Rate     uint32
	WantRate uint32
}

// Error implements error, naming the file and both frame counts.
func (e *ErrShortWindow) Error() string {
	if e.WantRate != 0 && e.Rate != e.WantRate {
		return fmt.Sprintf("window %s: %d frames at %d Hz, want %d frames at %d Hz",
			filepath.Base(e.Path), e.Frames, e.Rate, e.Want, e.WantRate)
	}
	return fmt.Sprintf("window %s: %d frames, want %d", filepath.Base(e.Path), e.Frames, e.Want)
}

// verifyWindow checks that the WAV at out is the window the caller planned:
// exactly [rate] Hz and at least 99% of wantFrames long, with a hard floor of
// [minWindowFrames] (the capture tool's rule, verbatim).
//
// The 1% slack is not a tolerance on the audio: it absorbs the frame count of
// an encoder boundary, while a window that lost a second fails by a factor the
// slack cannot hide. wantFrames is round((preroll' + LenSec) * rate) as the
// caller computed it — verifyWindow does not re-derive it, because doing so
// would hide a caller that got the pre-roll wrong.
func verifyWindow(out string, wantFrames, rate int64) error {
	reader, err := wav.Open(out)
	if err != nil {
		return err
	}
	defer reader.Close()
	frames := reader.Frames()
	if reader.SampleRate() != uint32(rate) {
		return &ErrShortWindow{
			Path:     out,
			Frames:   frames,
			Want:     wantFrames,
			Rate:     reader.SampleRate(),
			WantRate: uint32(rate),
		}
	}
	if frames < minWindowFrames || frames*100 < wantFrames*99 {
		return &ErrShortWindow{Path: out, Frames: frames, Want: wantFrames}
	}
	return nil
}

// windowFilePath is where one side of one window is written:
// {jobDir}/win/{pct}/{t,d}.wav.
//
// The name must never match the mux stage's `target-*.mka` glob
// (syncworker's stagedAudio): a second match there fails the job as ambiguous,
// so a window file is never named like a staged track, and it never carries the
// .mka extension.
func windowFilePath(jobDir string, w analyze.Window, side Side) string {
	return filepath.Join(jobDir, "win", strconv.Itoa(w.PositionPct), side.String()+".wav")
}

// removeWindow drops a half-written or rejected window file. Its error is
// deliberately ignored: the caller is already returning the failure that
// matters, and a leftover file is worse than a second log line only if it is
// mistaken for a window — which the frame gate prevents on the next read.
func removeWindow(out string) {
	_ = os.Remove(out)
}
