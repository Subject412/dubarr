package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"runtime"
	"strings"
	"time"

	"github.com/Subject412/dubarr/backend/internal/dsp/correlate"
	"github.com/Subject412/dubarr/backend/internal/dsp/wav"
)

const (
	// DefaultTimeout bounds one job unless the caller's context is tighter.
	DefaultTimeout = 30 * time.Second

	// DefaultWindowSec bounds the offset search when [Job.WindowSec] is nil,
	// as DEFAULT_WINDOW_SEC does in the reference engine.
	DefaultWindowSec = 5.0
)

// Correlator is the correlation seam: one answer for one pair of slices.
//
// [correlate.Analyze] is the waveform implementation; the mel-cepstral matcher
// (internal/dsp/mfcc) is the other, and it fills the same [correlate.Analysis].
// Sharing the seam is what makes a second matcher inherit everything the runner
// does around the call — validation, WAV reading, the analysed-slice echo,
// deadline classification, panic recovery — and the only thing that tells the
// two apart downstream is the precision each one reports.
type Correlator func(ctx context.Context, target, donor []float32, sampleRate uint32, windowSec float64) (correlate.Analysis, error)

// Runner runs one job at a time in-process against the Go DSP engine.
//
// A Runner holds no per-job state — no child process, no pipes, no buffers — so
// a failed or panicked Run leaves nothing behind for the next one. It is safe
// for concurrent use to the extent the engine is: a caller that wants several
// jobs at once builds one Runner per job (the queue runs one job anyway).
type Runner struct {
	timeout   time.Duration
	log       *slog.Logger
	correlate Correlator
}

// Option customises [New].
type Option func(*Runner)

// WithTimeout overrides how long a single job may run. The caller's context
// deadline still applies if it is earlier.
func WithTimeout(timeout time.Duration) Option {
	return func(r *Runner) {
		if timeout > 0 {
			r.timeout = timeout
		}
	}
}

// WithLogger sets the logger that records a recovered engine panic. The panic
// still fails the job; the log is for the operator.
func WithLogger(log *slog.Logger) Option {
	return func(r *Runner) {
		if log != nil {
			r.log = log
		}
	}
}

// WithCorrelator replaces the waveform correlator, which is how a second
// matcher runs behind the same runner: the mel-cepstral one is
// WithCorrelator(mfcc.Analyze) and nothing else about the job changes. It
// inherits the validation, the WAV reading, the analysed-slice echo, the
// deadline classification and the panic recovery, and its precision, z and
// sub-sample offset are reported as the engine scored them. A nil correlator
// keeps the default ([correlate.Analyze]).
func WithCorrelator(c Correlator) Option {
	return func(r *Runner) {
		if c != nil {
			r.correlate = c
		}
	}
}

// New returns a runner backed by the in-process engine.
//
// There is nothing to locate and nothing to handshake: the DSP code is compiled
// into this binary, so construction cannot fail.
func New(opts ...Option) *Runner {
	r := &Runner{
		timeout:   DefaultTimeout,
		log:       slog.Default(),
		correlate: correlate.Analyze,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// String renders the runner for logs.
func (r *Runner) String() string { return fmt.Sprintf("sync.Runner{timeout: %s}", r.timeout) }

// Run analyses job and returns its result.
//
// The job is validated first, then both WAV headers are read, then the slice is
// decoded from each and correlated. A sample-rate mismatch, an unreadable WAV, a
// slice past the end of a file, a slice shorter than 64 samples and a silent
// slice are all [ErrAnalysis]; a job the engine cannot accept is
// [ErrInvalidJob] and is rejected before any file is opened.
//
// A panic anywhere below — the fft package panics by contract on a bad plan
// size or an allocation it will not make — is recovered and returned as
// [ErrExecution] with a one-line stack in *Error.Stderr. In-process a panic
// would otherwise take the whole server down mid-job, with the SQLite WAL open;
// only the return value changes, a panic is never a success.
//
// Cancellation is cooperative: there is no process to kill. The context is
// checked after each open and each slice read, after each transform and every
// 4096 lags inside the correlation ([correlate.Analyze]). The worst case past a
// deadline is therefore one 2^22-point forward transform before the next check
// — sub-second at production sizes. An expired deadline is [ErrTimeout]; any
// other cancellation is [ErrExecution].
func (r *Runner) Run(ctx context.Context, job Job) (out Result, err error) {
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		if r.log != nil {
			r.log.Error("sync: dsp panic recovered", "panic", p)
		}
		out = Result{}
		err = &Error{
			Kind:   KindExecution,
			Stderr: panicTrace(),
			Err:    fmt.Errorf("dsp: %v", p),
		}
	}()

	if invalidErr := job.validate(); invalidErr != nil {
		return Result{}, &Error{Kind: KindInvalidJob, Err: invalidErr}
	}

	// Opening reads the header only, so an unsupported or unreadable file and a
	// rate mismatch fail before any window is decoded.
	target, openErr := wav.Open(job.TargetPath)
	if openErr != nil {
		return Result{}, &Error{Kind: KindAnalysis, Err: openErr}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Result{}, contextFailure(ctxErr, r.timeout)
	}
	donor, openErr := wav.Open(job.DonorPath)
	if openErr != nil {
		return Result{}, &Error{Kind: KindAnalysis, Err: openErr}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Result{}, contextFailure(ctxErr, r.timeout)
	}
	rate := target.SampleRate()
	if rate != donor.SampleRate() {
		return Result{}, &Error{Kind: KindAnalysis, Err: &SampleRateMismatch{Target: rate, Donor: donor.SampleRate()}}
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	// Seconds to samples exactly as job.rs:162-163 does, then truncate at each
	// file's end. Rounding both boundaries here (not in the engine) is what
	// keeps `analyzed.start` and the analysed samples in agreement.
	start := int64(math.Round(job.SliceStartSec * float64(rate)))
	length := int64(math.Round(job.SliceLenSec * float64(rate)))

	targetSlice, sliceErr := readSlice(target, start, length)
	if sliceErr != nil {
		return Result{}, &Error{Kind: KindAnalysis, Err: sliceErr}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Result{}, contextFailure(ctxErr, r.timeout)
	}
	donorSlice, sliceErr := readSlice(donor, start, length)
	if sliceErr != nil {
		return Result{}, &Error{Kind: KindAnalysis, Err: sliceErr}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Result{}, contextFailure(ctxErr, r.timeout)
	}

	samples := min(len(targetSlice), len(donorSlice))
	if samples < correlate.MinSliceSamples {
		return Result{}, &Error{
			Kind: KindAnalysis,
			Err:  &correlate.SliceTooShort{Samples: samples, Min: correlate.MinSliceSamples},
		}
	}

	window := DefaultWindowSec
	if job.WindowSec != nil {
		window = *job.WindowSec
	}
	analysis, correlateErr := r.correlate(ctx, targetSlice, donorSlice, rate, window)
	// The deadline is classified first, as the subprocess bridge did, so an
	// engine that gives up on a cancelled context is not mistaken for an
	// analysis failure.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Result{}, contextFailure(ctxErr, r.timeout)
	}
	if correlateErr != nil {
		return Result{}, &Error{Kind: KindAnalysis, Err: correlateErr}
	}

	result := Result{
		OffsetMS:           roundMillis(float64(analysis.OffsetSamples) * 1000 / float64(rate)),
		CorrelationQuality: roundMillis(float64(analysis.Quality)),
		SampleRate:         rate,
		// The interpolated offset is converted like the applied one but never
		// applied: it is the same measurement at sub-sample resolution, and z is
		// O(10), so three decimals is plenty for both.
		OffsetSubMS: roundMillis(analysis.OffsetSubSamples * 1000 / float64(rate)),
		MatchZ:      roundMillis(float64(analysis.MatchZ)),
		PrecisionMS: roundMillis(analysis.PrecisionMS),
	}
	result.Analyzed = Analyzed{
		Start: float64(start) / float64(rate),
		Len:   float64(samples) / float64(rate),
	}
	return result, nil
}

// readSlice decodes length frames from start, truncating at the end of the file.
// A start at or past the end is out of range, as job.rs:107-112 reports.
func readSlice(reader *wav.RangeReader, start, length int64) ([]float32, error) {
	frames := reader.Frames()
	rate := reader.SampleRate()
	if start >= frames {
		return nil, &SliceOutOfRange{
			StartSec:    float64(start) / float64(rate),
			DurationSec: float64(frames) / float64(rate),
		}
	}
	return reader.ReadMono(start, length)
}

// contextFailure classifies an expired or cancelled context: a deadline is a
// timeout, anything else (a parent cancel) is an execution failure.
func contextFailure(ctxErr error, timeout time.Duration) *Error {
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return &Error{Kind: KindTimeout, Err: fmt.Errorf("job exceeded %s", timeout)}
	}
	return &Error{Kind: KindExecution, Err: ctxErr}
}

// roundMillis rounds to three decimals, as round_millis does in job.rs:121-123.
func roundMillis(value float64) float64 { return math.Round(value*1000) / 1000 }

// panicTrace renders the current goroutine's stack as one line, truncated, so
// *Error.Stderr stays a single line in a job status row.
func panicTrace() string {
	buf := make([]byte, 4096)
	line := strings.Join(strings.Fields(string(buf[:runtime.Stack(buf, false)])), " ")
	const limit = 512
	if len(line) > limit {
		line = line[:limit] + "…"
	}
	return line
}
