package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// BinaryName is the executable the bridge locates on PATH. /api/health probes
// the same name (internal/health defaultTools).
const BinaryName = "dubarr-sync"

const (
	// DefaultTimeout bounds one job unless the caller's context is tighter.
	DefaultTimeout = 30 * time.Second
	// versionProbeTimeout is a hang guard for the construction handshake.
	versionProbeTimeout = 5 * time.Second
)

// versionToken matches the clap banner "dubarr-sync 0.1.0".
var versionToken = regexp.MustCompile(`\b([0-9]+\.[0-9][0-9A-Za-z.+-]*)`)

// Runner invokes the dubarr-sync binary for one job at a time.
//
// A Runner is immutable after [New] and safe for concurrent use; each Run owns
// its own process, stdin pipe and buffers.
type Runner struct {
	path    string
	timeout time.Duration
	log     *slog.Logger
	version string
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

// WithLogger sets the logger used for the log-only version handshake.
func WithLogger(log *slog.Logger) Option {
	return func(r *Runner) {
		if log != nil {
			r.log = log
		}
	}
}

// New locates the engine and performs the version handshake.
//
// binaryPath is an optional explicit override; empty means "search PATH with
// exec.LookPath". Per ADR-0003 there is deliberately no environment override.
// A missing binary is reported as [ErrUnavailable] so callers can mirror
// /api/health's dubarr-sync:false. A failing --version is logged but never
// fatal: the engine may still run jobs.
func New(binaryPath string, opts ...Option) (*Runner, error) {
	r := &Runner{
		path:    binaryPath,
		timeout: DefaultTimeout,
		log:     slog.Default(),
	}
	for _, opt := range opts {
		opt(r)
	}
	if r.path == "" {
		path, err := exec.LookPath(BinaryName)
		if err != nil {
			return nil, &Error{
				Kind: KindUnavailable,
				Err:  fmt.Errorf("%s not found on PATH: %w", BinaryName, err),
			}
		}
		r.path = path
	} else if err := checkExecutable(r.path); err != nil {
		return nil, &Error{Kind: KindUnavailable, Err: err}
	}
	r.version = r.handshake()
	return r, nil
}

// checkExecutable validates an explicit binary override so a bad path fails at
// construction with ErrUnavailable instead of on the first job.
func checkExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not an executable file", path)
	}
	return nil
}

// handshake asks the engine for its version. Failures are logged at debug/warn
// level and yield an empty version; they never fail construction.
func (r *Runner) handshake() string {
	ctx, cancel := context.WithTimeout(context.Background(), versionProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.path, "--version").Output()
	if err != nil {
		r.log.Warn("sync: dubarr-sync version handshake failed", "path", r.path, "error", err)
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	version := ""
	if m := versionToken.FindStringSubmatch(line); len(m) == 2 {
		version = m[1]
	}
	r.log.Info("sync: dubarr-sync ready", "path", r.path, "version", version)
	return version
}

// Path reports the resolved binary path.
func (r *Runner) Path() string { return r.path }

// Version reports the version parsed at construction, or "" if the handshake
// failed.
func (r *Runner) Version() string { return r.version }

// String renders the runner for logs.
func (r *Runner) String() string { return fmt.Sprintf("sync.Runner{%s %s}", r.path, r.version) }

// Run sends job to the engine and returns its result.
//
// The job is JSON-encoded with the exact sync-engine/src/job.rs field names and
// written to the child's stdin; stdout must hold exactly one result object.
// Failures are typed: ErrInvalidJob (rejected locally), ErrTimeout,
// ErrUsage / ErrAnalysis (exit codes 2/3), ErrMalformedOutput and
// ErrExecution/ErrUnavailable for everything else.
func (r *Runner) Run(ctx context.Context, job Job) (Result, error) {
	if err := job.validate(); err != nil {
		return Result{}, &Error{Kind: KindInvalidJob, Err: err}
	}
	payload, err := json.Marshal(job)
	if err != nil {
		return Result{}, &Error{Kind: KindInvalidJob, Err: fmt.Errorf("encode job: %w", err)}
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, r.path)
	isolateProcessGroup(cmd)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	stderrText := strings.TrimSpace(stderr.String())

	// A context expiry kills the child, which also surfaces as a non-nil
	// runErr; classify it first so a timeout is not mistaken for a signal.
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return Result{}, &Error{
				Kind:   KindTimeout,
				Stderr: stderrText,
				Err:    fmt.Errorf("job exceeded %s", r.timeout),
			}
		}
		return Result{}, &Error{Kind: KindExecution, Stderr: stderrText, Err: ctxErr}
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			code := exitErr.ExitCode()
			return Result{}, &Error{
				Kind:     kindForExit(code),
				ExitCode: code,
				Stderr:   stderrText,
				Err:      fmt.Errorf("%s exited %d", BinaryName, code),
			}
		}
		return Result{}, &Error{
			Kind:   KindUnavailable,
			Stderr: stderrText,
			Err:    fmt.Errorf("start %s: %w", BinaryName, runErr),
		}
	}

	raw := bytes.TrimSpace(stdout.Bytes())
	if len(raw) == 0 {
		return Result{}, &Error{
			Kind:   KindMalformedOutput,
			Stderr: stderrText,
			Err:    errors.New("empty stdout"),
		}
	}
	var result Result
	if err := json.Unmarshal(raw, &result); err != nil {
		return Result{}, &Error{
			Kind:   KindMalformedOutput,
			Stderr: stderrText,
			Err:    fmt.Errorf("decode result: %w", err),
		}
	}
	if result.SampleRate == 0 {
		return Result{}, &Error{
			Kind:   KindMalformedOutput,
			Stderr: stderrText,
			Err:    errors.New("result has no sample rate"),
		}
	}
	return result, nil
}

// kindForExit maps the documented engine exit codes onto typed kinds: 2 is a
// usage error, 3 an analysis failure; anything else is an execution failure.
func kindForExit(code int) Kind {
	switch code {
	case 2:
		return KindUsage
	case 3:
		return KindAnalysis
	default:
		return KindExecution
	}
}
