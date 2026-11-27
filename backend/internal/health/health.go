// Package health exposes GET /api/health, the appliance's readiness probe.
//
// The payload reports the backend version, database reachability, and the
// availability and version of every external tool Dubarr shells out to
// (ffmpeg, ffprobe, mkvmerge). It is public so the SPA can render
// pre-login diagnostics, and it therefore never exposes secrets, key material
// or user data.
package health

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// defaultTools are the external binaries /api/health must report.
var defaultTools = []string{"ffmpeg", "ffprobe", "mkvmerge"}

// versionProbeTimeout bounds how long a single `<tool> -version` call may take.
//
// The bound is a hang guard, not a performance budget: a wedged binary must
// cost at most this long. It is deliberately generous because appliances run
// under load (docker builds, transcodes) and a cold `ffmpeg -version` start can
// take a couple of seconds on a loaded box.
const versionProbeTimeout = 5 * time.Second

// dbPingTimeout bounds the database liveness check.
const dbPingTimeout = 2 * time.Second

var (
	// versionAfterWord matches "ffmpeg version n9.0.2" / "version 1.2".
	versionAfterWord = regexp.MustCompile(`(?i)\bversion\s+([^\s,()]+)`)
	// versionVToken matches "mkvmerge v103.0 ('...')".
	versionVToken = regexp.MustCompile(`(?i)\bv([0-9][0-9A-Za-z.+-]*)`)
	// versionBareNumber matches a bare "tool 1.2.3" banner.
	versionBareNumber = regexp.MustCompile(`\b([0-9]+\.[0-9][0-9A-Za-z.+-]*)`)
)

// Dependencies is everything the health endpoint needs from the process.
//
// All func fields are optional: nil means "feature not available in this
// deployment" and is reported as false. LookPath and VersionCommand are seams
// so tests can simulate installed and missing tools deterministically.
type Dependencies struct {
	// DB is the live SQLite handle; nil reports db_ok=false.
	DB *sql.DB
	// Version is the backend build version reported to the UI.
	Version string
	// KeyringAvailable reports whether an OS keyring backend is usable.
	KeyringAvailable func() bool
	// SecretsInitialized reports whether the encrypted store is ready.
	SecretsInitialized func() bool
	// AuthInitialized reports whether at least one user exists.
	AuthInitialized func() bool
	// LookPath resolves a binary on PATH (defaults to exec.LookPath).
	LookPath func(string) (string, error)
	// VersionCommand returns the first line of `<path> -version`. The context
	// carries the probe deadline, so implementations must abort on ctx.Done()
	// rather than block on a wedged binary.
	VersionCommand func(ctx context.Context, name, path string) (string, error)
	// Tools overrides the reported tool set (defaults to defaultTools).
	Tools []string
}

// Service holds the resolved probes for the endpoint.
type Service struct {
	db                 *sql.DB
	version            string
	keyringAvailable   func() bool
	secretsInitialized func() bool
	authInitialized    func() bool
	lookPath           func(string) (string, error)
	versionCommand     func(ctx context.Context, name, path string) (string, error)
	probeTimeout       time.Duration
	tools              []string
}

// New resolves the optional dependencies into a Service.
func New(deps Dependencies) *Service {
	s := &Service{
		db:                 deps.DB,
		version:            deps.Version,
		keyringAvailable:   deps.KeyringAvailable,
		secretsInitialized: deps.SecretsInitialized,
		authInitialized:    deps.AuthInitialized,
		lookPath:           deps.LookPath,
		versionCommand:     deps.VersionCommand,
		probeTimeout:       versionProbeTimeout,
		tools:              deps.Tools,
	}
	if s.lookPath == nil {
		s.lookPath = exec.LookPath
	}
	if s.versionCommand == nil {
		s.versionCommand = probeVersion
	}
	if len(s.tools) == 0 {
		s.tools = defaultTools
	}
	if s.version == "" {
		s.version = "dev"
	}
	return s
}

// RegisterRoutes mounts GET /api/health on mux and returns the service.
func RegisterRoutes(mux *http.ServeMux, deps Dependencies) *Service {
	s := New(deps)
	s.RegisterRoutes(mux)
	return s
}

// RegisterRoutes mounts the health endpoint on mux.
func (s *Service) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", s.handleHealth)
}

// toolStatus is one external binary's probe result. A missing tool reports
// available=false with empty version and path.
type toolStatus struct {
	Available bool   `json:"available"`
	Version   string `json:"version"`
	Path      string `json:"path"`
}

// response is the public /api/health payload. Field names are the frontend
// contract and deliberately contain no secret material.
type response struct {
	Status             string                `json:"status"`
	Version            string                `json:"version"`
	DBOK               bool                  `json:"db_ok"`
	Tools              map[string]toolStatus `json:"tools"`
	KeyringAvailable   bool                  `json:"keyring_available"`
	SecretsInitialized bool                  `json:"secrets_initialized"`
	AuthInitialized    bool                  `json:"auth_initialized"`
}

// handleHealth answers GET /api/health. It always returns 200: the `status`
// field ("ok" or "degraded") carries readiness so probes can distinguish a
// degraded backend from an unreachable one.
func (s *Service) handleHealth(w http.ResponseWriter, r *http.Request) {
	body := response{
		Status:             "ok",
		Version:            s.version,
		DBOK:               s.dbReachable(r.Context()),
		Tools:              s.probeTools(r.Context()),
		KeyringAvailable:   s.flag(s.keyringAvailable),
		SecretsInitialized: s.flag(s.secretsInitialized),
		AuthInitialized:    s.flag(s.authInitialized),
	}
	if !body.DBOK {
		body.Status = "degraded"
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Service) dbReachable(ctx context.Context) bool {
	if s.db == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, dbPingTimeout)
	defer cancel()
	return s.db.PingContext(ctx) == nil
}

func (s *Service) probeTools(ctx context.Context) map[string]toolStatus {
	out := make(map[string]toolStatus, len(s.tools))
	for _, name := range s.tools {
		path, err := s.lookPath(name)
		if err != nil || path == "" {
			out[name] = toolStatus{}
			continue
		}
		status := toolStatus{Available: true, Path: path}
		// Bound each probe individually: a tool that hangs must only cost
		// versionProbeTimeout and must not stall the public endpoint.
		tctx, cancel := context.WithTimeout(ctx, s.probeTimeout)
		line, verr := s.versionCommand(tctx, name, path)
		cancel()
		if verr == nil {
			status.Version = parseVersion(line)
		}
		out[name] = status
	}
	return out
}

func (s *Service) flag(fn func() bool) bool { return fn != nil && fn() }

// probeVersion runs `<path> -version` (falling back to --version) and returns
// the first non-empty output line. Failures are non-fatal.
//
// The context bounds the whole probe: exec.CommandContext kills the child when
// the deadline expires (or the request is cancelled), so a wedged binary can
// never make the caller wait indefinitely. The timeout is also applied here so
// direct callers get the same guarantee as the HTTP path.
func probeVersion(ctx context.Context, name, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionProbeTimeout)
	defer cancel()

	var lastErr error
	for _, flag := range []string{"-version", "--version"} {
		cmd := exec.CommandContext(ctx, path, flag)
		combined, err := cmd.CombinedOutput()
		if err != nil {
			lastErr = err
			continue
		}
		for _, line := range strings.Split(string(combined), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				return line, nil
			}
		}
	}
	return "", lastErr
}

// parseVersion extracts a version token from a tool's banner line. It handles
// "ffmpeg version n9.0.2", "mkvmerge v103.0 ('...')" and bare "name 1.2.3"
// banners, returning "" when nothing looks like a version.
func parseVersion(line string) string {
	for _, re := range []*regexp.Regexp{versionAfterWord, versionVToken, versionBareNumber} {
		if m := re.FindStringSubmatch(line); len(m) == 2 {
			return strings.Trim(m[1], " \t,;")
		}
	}
	return ""
}
