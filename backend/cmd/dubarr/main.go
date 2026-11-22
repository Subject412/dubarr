// Command dubarr is the Dubarr appliance entrypoint: it opens DATA_DIR, wires
// every backend package, serves the embedded SPA and shuts down gracefully.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Subject412/dubarr/backend/internal/api"
	"github.com/Subject412/dubarr/backend/internal/auth"
	"github.com/Subject412/dubarr/backend/internal/health"
	"github.com/Subject412/dubarr/backend/internal/queue"
	"github.com/Subject412/dubarr/backend/internal/secrets"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
	"github.com/Subject412/dubarr/backend/web"
)

const (
	// dataDirPerm is the required mode for DATA_DIR.
	dataDirPerm = 0o700
	// sessionTTL mirrors the auth package's 7-day session lifetime.
	sessionTTL = 7 * 24 * time.Hour
	// shutdownTimeout bounds graceful shutdown on SIGINT/SIGTERM.
	shutdownTimeout = 10 * time.Second
)

// version is the build version reported by /api/health; overridable with
// -ldflags "-X main.version=...".
var version = "dev"

// newServerFn builds the wired server. Tests replace it to keep secrets on the
// 0600 keyfile path instead of touching the developer's OS keyring.
var newServerFn = newServer

// Config holds the resolved runtime configuration for the appliance.
//
// dubarr never reads settings or secrets from the environment: only these
// three flags (plus PUID/PGID in the Docker image) configure a process.
type Config struct {
	// DataDir is the appliance data root (database, keyfile, cache). Created
	// with 0700 when it does not exist yet.
	DataDir string
	// Listen is the host:port the HTTP server will bind to.
	Listen string
	// Desktop marks a desktop (native) deployment and opens the default
	// browser via xdg-open once the server is listening.
	Desktop bool
}

// newFlagSet binds cfg to a fresh flag set. Help is left to the caller: with
// ContinueOnError, -h/--help returns flag.ErrHelp.
func newFlagSet(cfg *Config) *flag.FlagSet {
	fs := flag.NewFlagSet("dubarr", flag.ContinueOnError)
	fs.StringVar(&cfg.DataDir, "data-dir", "./data", "appliance data directory")
	fs.StringVar(&cfg.Listen, "listen", "127.0.0.1:8080", "HTTP listen address")
	fs.BoolVar(&cfg.Desktop, "desktop", false, "run as a desktop (native) deployment")
	return fs
}

// parseFlags parses CLI arguments into a Config. It returns an error rather
// than exiting so callers (and tests) stay in control of the process. Help
// requests are reported as flag.ErrHelp.
func parseFlags(args []string) (Config, error) {
	var cfg Config
	fs := newFlagSet(&cfg)
	fs.SetOutput(io.Discard)

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	if cfg.DataDir == "" {
		return Config{}, errors.New("--data-dir must not be empty")
	}
	if cfg.Listen == "" {
		return Config{}, errors.New("--listen must not be empty")
	}
	return cfg, nil
}

// writeUsage prints the CLI help text to w.
func writeUsage(w io.Writer) {
	var cfg Config
	fs := newFlagSet(&cfg)
	fs.SetOutput(w)
	fmt.Fprintln(w, "Usage: dubarr [flags]")
	fmt.Fprintln(w, "       dubarr rekey --data-dir DIR --keyfile PATH")
	fmt.Fprintln(w)
	fs.PrintDefaults()
}

// runRekey implements the one-shot rekey subcommand: it re-encrypts every
// secret under the supplied keyfile and pins the keyfile source.
func runRekey(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dubarr rekey", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dataDir := fs.String("data-dir", "./data", "appliance data directory")
	keyfile := fs.String("keyfile", "", "path to the new master keyfile")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stdout, "Usage: dubarr rekey --data-dir DIR --keyfile PATH")
			return 0
		}
		fmt.Fprintf(stderr, "dubarr: %v\n", err)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "dubarr: unexpected positional arguments: %v\n", fs.Args())
		return 2
	}
	if *dataDir == "" || *keyfile == "" {
		fmt.Fprintln(stderr, "dubarr: rekey requires --data-dir and --keyfile")
		return 2
	}

	st, err := store.Open(*dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "dubarr: open store: %v\n", err)
		return 1
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			fmt.Fprintf(stderr, "dubarr: close: %v\n", cerr)
		}
	}()
	if err := secrets.Rekey(st, secrets.DefaultOptions(*dataDir), *keyfile); err != nil {
		fmt.Fprintf(stderr, "dubarr: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "dubarr: rekey complete: data-dir=%s\n", *dataDir)
	return 0
}

// server owns the opened stores and the HTTP server. It is the composition
// root: nothing outside this file constructs these packages.
type server struct {
	cfg      Config
	store    *store.Store
	secrets  *secrets.Service
	settings *settings.Store
	mux      *http.ServeMux
	http     *http.Server

	closeOnce sync.Once
	closeErr  error
}

// newServer opens DATA_DIR, wires every package onto one mux and returns a
// ready-to-serve server. secrets.Open uses the native production path (OS
// keyring with 0600 keyfile fallback).
func newServer(cfg Config) (*server, error) {
	return newServerWithSecrets(cfg, secrets.DefaultOptions(cfg.DataDir))
}

// newServerWithSecrets is newServer with injectable secrets options so tests
// never touch the developer's real keyring.
func newServerWithSecrets(cfg Config, secOpts secrets.Options) (*server, error) {
	if err := os.MkdirAll(cfg.DataDir, dataDirPerm); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	// MkdirAll keeps the mode of an existing directory; tighten it explicitly.
	if err := os.Chmod(cfg.DataDir, dataDirPerm); err != nil {
		return nil, fmt.Errorf("chmod data dir: %w", err)
	}

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	sec, err := secrets.Open(st, secOpts)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("open secrets: %w", err)
	}

	settingsStore := settings.New(st.DB)
	if err := settingsStore.Seed(context.Background(), cfg.DataDir); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("seed settings: %w", err)
	}

	mux := http.NewServeMux()

	authSvc := auth.RegisterRoutes(mux, auth.Dependencies{
		DB:               st.DB,
		SessionTTL:       sessionTTL,
		KeyringAvailable: sec.KeyringAvailable,
		KeySourceChanged: sec.KeySourceChanged,
	})

	api.RegisterRoutes(mux, api.Dependencies{
		DB:           st.DB,
		Settings:     settingsStore,
		Secrets:      sec,
		RequireAdmin: authSvc.RequireAdmin,
	})

	queue.RegisterRoutes(mux, queue.Dependencies{
		DB:           st.DB,
		Settings:     settingsStore,
		RequireAdmin: authSvc.RequireAdmin,
	})

	health.RegisterRoutes(mux, health.Dependencies{
		DB:               st.DB,
		Version:          version,
		KeyringAvailable: sec.KeyringAvailable,
		SecretsInitialized: func() bool {
			return sec != nil && sec.KeySource() != ""
		},
		AuthInitialized: func() bool {
			var users int
			if err := st.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
				return false
			}
			return users > 0
		},
	})

	spa, err := newSPAHandler(web.FS)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("mount SPA: %w", err)
	}
	mux.Handle("/", spa)

	s := &server{
		cfg:      cfg,
		store:    st,
		secrets:  sec,
		settings: settingsStore,
		mux:      mux,
	}
	s.http = &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	return s, nil
}

// Handler exposes the wired mux (used by tests and embed checks).
func (s *server) Handler() http.Handler { return s.mux }

// Close releases the database handle. It is safe to call more than once.
func (s *server) Close() error {
	s.closeOnce.Do(func() {
		if s.store != nil {
			s.closeErr = s.store.Close()
		}
	})
	return s.closeErr
}

// Serve binds cfg.Listen, reports the bound address through onListen and blocks
// until ctx is cancelled or the server fails. Cancellation triggers a bounded
// graceful shutdown; ErrServerClosed is reported as success.
func (s *server) Serve(ctx context.Context, onListen func(addr string)) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.Listen, err)
	}
	addr := ln.Addr().String()
	s.http.Addr = addr
	if onListen != nil {
		onListen(addr)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer shutCancel()
		_ = s.http.Shutdown(shutCtx)
	}()

	err = s.http.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	if ctx.Err() != nil {
		<-stopped
	}
	return err
}

// newSPAHandler serves the embedded SPA from root (backend/web). Unknown
// non-API paths fall back to index.html so client-side routes deep-link, while
// /api/* misses stay 404.
func newSPAHandler(root fs.FS) (http.Handler, error) {
	index, err := fs.ReadFile(root, "index.html")
	if err != nil {
		return nil, fmt.Errorf("embedded index.html: %w", err)
	}
	files := http.FileServerFS(root)

	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(index)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" || name == "." {
			serveIndex(w, r)
			return
		}
		if info, statErr := fs.Stat(root, name); statErr == nil && !info.IsDir() {
			files.ServeHTTP(w, r)
			return
		}
		serveIndex(w, r)
	}), nil
}

// browserURL turns a listen address into a URL a desktop browser can open.
func browserURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// openBrowser launches xdg-open detached. Failure is non-fatal: the URL is
// printed either way.
func openBrowser(url string) error {
	cmd := exec.Command("xdg-open", url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// run executes the command as the real CLI: SIGINT/SIGTERM cancel the server.
func run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runContext(ctx, args, stdout, stderr)
}

// runContext executes the command until ctx is cancelled. It returns the exit
// code so tests can drive the whole lifecycle without sending signals.
func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "rekey" {
		return runRekey(args[1:], stdout, stderr)
	}
	cfg, err := parseFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeUsage(stdout)
			return 0
		}
		fmt.Fprintf(stderr, "dubarr: %v\n", err)
		return 2
	}

	srv, err := newServerFn(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "dubarr: %v\n", err)
		return 1
	}
	defer func() {
		if cerr := srv.Close(); cerr != nil {
			fmt.Fprintf(stderr, "dubarr: close: %v\n", cerr)
		}
	}()

	fmt.Fprintf(stdout, "dubarr: data-dir=%s version=%s\n", cfg.DataDir, version)

	err = srv.Serve(ctx, func(addr string) {
		fmt.Fprintf(stdout, "dubarr: listening on http://%s\n", addr)
		if cfg.Desktop {
			url := browserURL(addr)
			if oerr := openBrowser(url); oerr != nil {
				fmt.Fprintf(stderr, "dubarr: could not open browser: %v\n", oerr)
			}
		}
	})
	if err != nil {
		fmt.Fprintf(stderr, "dubarr: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "dubarr: shut down")
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
