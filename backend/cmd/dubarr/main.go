// Command dubarr is the Dubarr appliance entrypoint: it opens DATA_DIR, wires
// every backend package, serves the embedded SPA and shuts down gracefully.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Subject412/dubarr/backend/internal/api"
	"github.com/Subject412/dubarr/backend/internal/arrrescan"
	"github.com/Subject412/dubarr/backend/internal/arrsync"
	"github.com/Subject412/dubarr/backend/internal/auth"
	"github.com/Subject412/dubarr/backend/internal/extract"
	"github.com/Subject412/dubarr/backend/internal/gate"
	"github.com/Subject412/dubarr/backend/internal/health"
	"github.com/Subject412/dubarr/backend/internal/logs"
	"github.com/Subject412/dubarr/backend/internal/mux"
	"github.com/Subject412/dubarr/backend/internal/queue"
	"github.com/Subject412/dubarr/backend/internal/secrets"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
	"github.com/Subject412/dubarr/backend/internal/syncjob"
	"github.com/Subject412/dubarr/backend/internal/syncworker"
	"github.com/Subject412/dubarr/backend/internal/vault"
	"github.com/Subject412/dubarr/backend/internal/vaultapi"
	"github.com/Subject412/dubarr/backend/internal/watch"
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
	// LogLevel is the minimum severity written to any sink
	// (debug|info|warn|error, default info). A flag, never an env var
	// (ADR-0003); the Docker entrypoint forwards it via "$@".
	LogLevel string
	// LogFile enables the optional rotating file sink at
	// <data-dir>/logs/dubarr.log (10 MB x 3). Off by default.
	LogFile bool
}

// newFlagSet binds cfg to a fresh flag set. Help is left to the caller: with
// ContinueOnError, -h/--help returns flag.ErrHelp.
func newFlagSet(cfg *Config) *flag.FlagSet {
	fs := flag.NewFlagSet("dubarr", flag.ContinueOnError)
	fs.StringVar(&cfg.DataDir, "data-dir", "./data", "appliance data directory")
	fs.StringVar(&cfg.Listen, "listen", "127.0.0.1:8080", "HTTP listen address")
	fs.BoolVar(&cfg.Desktop, "desktop", false, "run as a desktop (native) deployment")
	fs.StringVar(&cfg.LogLevel, "log-level", "info", "minimum log level: debug, info, warn or error")
	fs.BoolVar(&cfg.LogFile, "log-file", false, "also write logs to <data-dir>/logs/dubarr.log (rotated)")
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
	if _, ok := logs.ParseLevel(cfg.LogLevel); !ok {
		return Config{}, fmt.Errorf("--log-level must be debug, info, warn or error (got %q)", cfg.LogLevel)
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
	scanner  *watch.Scanner
	extract  *extract.Worker
	sync     *syncworker.Worker
	library  *arrsync.Poller
	mux      *http.ServeMux
	http     *http.Server
	// logs owns the central slog logger, its ring and optional file sink.
	logs *logs.Logger

	// scannerCancel stops the watch loop; nil when no scanner was started.
	scannerCancel context.CancelFunc
	// extractCancel stops the approval-to-vault worker; nil when disabled.
	extractCancel context.CancelFunc
	// syncCancel stops the Execute→mux→rescan worker; nil when disabled.
	syncCancel context.CancelFunc
	// libraryCancel stops the arr library poller; nil when disabled.
	libraryCancel context.CancelFunc

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

	logOpts := logs.Options{Level: cfg.LogLevel, Stderr: os.Stderr}
	if cfg.LogFile {
		logOpts.FilePath = filepath.Join(cfg.DataDir, "logs", "dubarr.log")
	}
	logger, err := logs.New(logOpts)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("open log sink: %w", err)
	}
	logger.Logger().Info("dubarr starting", "component", "main", "version", version)

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

	syncjob.RegisterRoutes(mux, syncjob.Dependencies{
		DB:           st.DB,
		Settings:     settingsStore,
		RequireAdmin: authSvc.RequireAdmin,
		Logger:       logger.Logger(),
	})

	logs.RegisterRoutes(mux, logs.Dependencies{
		Logs:         logger,
		RequireAdmin: authSvc.RequireAdmin,
	})

	vaultapi.RegisterRoutes(mux, vaultapi.Dependencies{
		DB:           st.DB,
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
		_ = logger.Close()
		return nil, fmt.Errorf("mount SPA: %w", err)
	}
	mux.Handle("/", spa)

	scanCtx, scanCancel := context.WithCancel(context.Background())
	watchScanner, scanErr := wireScanner(scanCtx, scannerWiring{
		Settings: settingsStore,
		Store:    st,
		Logf:     logger.Logf("watch"),
	})
	if scanErr != nil {
		// A broken watch path must not take the HTTP API down: the operator
		// can still fix storage.paths in the UI and restart.
		logger.Logger().Warn("watch scanner disabled", "component", "watch", "error", scanErr)
		scanCancel()
	} else if watchScanner != nil {
		watchScanner.Start(scanCtx)
	}

	// M3-4: approval → extraction → vault. The worker consumes rows the user
	// approved (status extracting) and hands each validated staging output to
	// the vault writer. A broken storage/vault configuration must not take the
	// HTTP API down; approvals then stay queued until the next boot.
	extractCtx, extractCancel := context.WithCancel(context.Background())
	extractWorker, extractErr := wireExtractWorker(extractCtx, extractWiring{
		Settings: settingsStore,
		Store:    st,
		DataDir:  cfg.DataDir,
		Logf:     logger.Logf("extract"),
	})
	if extractErr != nil {
		logger.Logger().Warn("extraction worker disabled", "component", "extract", "error", extractErr)
		extractCancel()
		extractCancel = nil
	} else if extractWorker != nil {
		extractWorker.Start(extractCtx)
	} else {
		extractCancel()
		extractCancel = nil
	}

	// M5-3: queued sync job → Execute → mux → arr rescan. Disabled (rows stay
	// queued) when the storage or sync configuration is not complete yet; a
	// broken chain must not take the HTTP API down.
	syncCtx, syncCancel := context.WithCancel(context.Background())
	syncWorker, syncErr := wireSyncWorker(syncCtx, syncWiring{
		Settings: settingsStore,
		Store:    st,
		Secrets:  sec,
		Logf:     logger.Logf("syncworker"),
	})
	if syncErr != nil {
		logger.Logger().Warn("sync worker disabled", "component", "syncworker", "error", syncErr)
		syncCancel()
		syncCancel = nil
	} else if syncWorker != nil {
		syncWorker.Start(syncCtx)
	} else {
		syncCancel()
		syncCancel = nil
	}

	// M6-1: poll Radarr/Sonarr and refresh arr_library_cache so the manual
	// search bridge and approvals always see the current library. Disabled
	// (no instances, or a non-positive interval) rather than fatal.
	libraryCtx, libraryCancel := context.WithCancel(context.Background())
	libraryPoller, libraryErr := wireArrSync(libraryCtx, arrSyncWiring{
		Settings: settingsStore,
		Store:    st,
		Secrets:  sec,
		Logf:     logger.Logf("arrsync"),
	})
	if libraryErr != nil {
		logger.Logger().Warn("arr library sync disabled", "component", "arrsync", "error", libraryErr)
		libraryCancel()
		libraryCancel = nil
	} else if libraryPoller != nil {
		libraryPoller.Start(libraryCtx)
	} else {
		libraryCancel()
		libraryCancel = nil
	}

	s := &server{
		cfg:           cfg,
		store:         st,
		secrets:       sec,
		settings:      settingsStore,
		scanner:       watchScanner,
		extract:       extractWorker,
		sync:          syncWorker,
		library:       libraryPoller,
		scannerCancel: scanCancel,
		extractCancel: extractCancel,
		syncCancel:    syncCancel,
		libraryCancel: libraryCancel,
		mux:           mux,
		logs:          logger,
	}
	s.http = &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	return s, nil
}

// scannerWiring is the collaborator set the composition root needs to build
// the watch scanner. It is a struct rather than positional arguments so tests
// can substitute a logging sink without touching the production path.
type scannerWiring struct {
	Settings *settings.Store
	Store    *store.Store
	// Logf reports why the scanner stays disabled. Nil means log.Printf.
	Logf func(format string, args ...any)
}

// wireScanner builds the M2 ingest pipeline: watch scanner over the configured
// watch folder, with a post-record hook that classifies each new candidate via
// the Processing Gate and persists the verdict for GET /api/queue.
//
// The scanner watches storage.paths.watch_dir (IDEA.md §3.3), the dropzone for
// incoming donor media; media_roots are the target libraries and are not
// scanned for donors. With no watch_dir configured it returns (nil, nil) after
// logging once, so a fresh install can serve the setup wizard.
//
// watch_dir, tag_filter and scan_interval are read once at boot; changing them
// in the UI at runtime requires a restart to take effect.
func wireScanner(ctx context.Context, deps scannerWiring) (*watch.Scanner, error) {
	logf := deps.Logf
	if logf == nil {
		logf = log.Printf
	}
	if deps.Settings == nil || deps.Store == nil {
		return nil, errors.New("watch scanner needs the settings store and the database store")
	}

	paths, err := loadStoragePaths(ctx, deps.Settings)
	if err != nil {
		return nil, err
	}
	watchDir := strings.TrimSpace(paths.WatchDir)
	if watchDir == "" {
		logf("dubarr: watch scanner disabled: storage.paths.watch_dir is not configured")
		return nil, nil
	}

	rule, err := loadGateRule(ctx, deps.Settings)
	if err != nil {
		return nil, err
	}
	interval, err := loadWatchInterval(ctx, deps.Settings)
	if err != nil {
		return nil, err
	}

	scanner, err := watch.New(deps.Store, []string{watchDir}, interval)
	if err != nil {
		return nil, fmt.Errorf("watch scanner: %w", err)
	}

	gateSvc := gate.NewService(rule, deps.Store)
	scanner.SetCandidateHook(func(ctx context.Context, sourcePath, _ string, _ int64) error {
		// M2 has no arr target resolution on the watch path yet, so the
		// dropped file is classified by its own filename — exactly what
		// ADR-0005 makes authoritative for eligibility.
		if _, err := gateSvc.ClassifyCandidate(ctx, sourcePath, sourcePath); err != nil {
			return fmt.Errorf("gate classify %s: %w", sourcePath, err)
		}
		return nil
	})
	return scanner, nil
}

// extractWiring is the collaborator set for the approval-to-vault worker.
type extractWiring struct {
	Settings *settings.Store
	Store    *store.Store
	DataDir  string
	// Interval overrides the idle cadence; 0 keeps extract.DefaultInterval.
	Interval time.Duration
	// Logf reports why the worker stays disabled. Nil means log.Printf.
	Logf func(format string, args ...any)
}

// wireExtractWorker builds the M3-4 pipeline: the M3-1 extraction worker over
// approved queue rows, with a vault hook that publishes every validated
// staging output through the M3-2 writer.
//
// The vault directory is required. With none configured it returns (nil, nil)
// after logging once, leaving approved rows queued rather than failing them
// against a missing vault. storage.paths and languages.policy are read per job
// by the worker; the vault writer re-reads storage.paths per write.
func wireExtractWorker(ctx context.Context, deps extractWiring) (*extract.Worker, error) {
	logf := deps.Logf
	if logf == nil {
		logf = log.Printf
	}
	if deps.Settings == nil || deps.Store == nil {
		return nil, errors.New("extraction worker needs the settings store and the database store")
	}

	paths, err := loadStoragePaths(ctx, deps.Settings)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(paths.VaultDir) == "" {
		logf("dubarr: extraction worker disabled: storage.paths.vault_dir is not configured")
		return nil, nil
	}

	writer, err := vault.New(vault.Dependencies{
		Store:    deps.Store,
		Settings: deps.Settings,
		DataDir:  deps.DataDir,
	})
	if err != nil {
		return nil, fmt.Errorf("vault writer: %w", err)
	}

	worker, err := extract.New(extract.Dependencies{
		Store:     deps.Store,
		Settings:  deps.Settings,
		DataDir:   deps.DataDir,
		Interval:  deps.Interval,
		VaultHook: newVaultHook(writer),
		Logf:      logf,
	})
	if err != nil {
		return nil, fmt.Errorf("extraction worker: %w", err)
	}
	return worker, nil
}

// syncWiring is the collaborator set for the M5-3 sync job worker.
type syncWiring struct {
	Settings *settings.Store
	Store    *store.Store
	// Secrets decrypts arr_instances.api_key_enc for the M5-4 rescan.
	Secrets arrrescan.Decrypter
	// Interval overrides the idle cadence; 0 keeps syncworker.DefaultInterval.
	Interval time.Duration
	// Logf reports why the worker stays disabled. Nil means log.Printf.
	Logf func(format string, args ...any)
}

// wireSyncWorker builds the M5-3 chain: queued sync jobs are analysed by the
// M4-6 Execute seam, muxed by M5-1 and rescanned by M5-4.
//
// vault_dir, work_dir and sync.defaults are required. With any of them missing
// it returns (nil, nil) after logging once, leaving jobs queued rather than
// failing them against incomplete storage. The analysis settings and gate rule
// are re-read per job by the worker.
func wireSyncWorker(ctx context.Context, deps syncWiring) (*syncworker.Worker, error) {
	logf := deps.Logf
	if logf == nil {
		logf = log.Printf
	}
	if deps.Settings == nil || deps.Store == nil {
		return nil, errors.New("sync worker needs the settings store and the database store")
	}
	if deps.Secrets == nil {
		return nil, errors.New("sync worker needs the secret decrypter for arr rescans")
	}

	paths, err := loadStoragePaths(ctx, deps.Settings)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(paths.VaultDir) == "" {
		logf("dubarr: sync worker disabled: storage.paths.vault_dir is not configured")
		return nil, nil
	}
	if strings.TrimSpace(paths.WorkDir) == "" {
		logf("dubarr: sync worker disabled: storage.paths.work_dir is not configured")
		return nil, nil
	}
	if _, ok, err := deps.Settings.Get(ctx, settings.KeySyncDefaults); err != nil {
		return nil, fmt.Errorf("read sync.defaults: %w", err)
	} else if !ok {
		logf("dubarr: sync worker disabled: sync.defaults is not configured")
		return nil, nil
	}

	// Execute lives behind the same service the HTTP API registers, but the
	// worker only needs the method: mount it on a throwaway mux.
	executor := syncjob.RegisterRoutes(http.NewServeMux(), syncjob.Dependencies{
		DB:       deps.Store.DB,
		Settings: deps.Settings,
		WorkDir:  paths.WorkDir,
	})

	rescanner, err := arrrescan.New(arrrescan.Deps{
		DB:      deps.Store.DB,
		Secrets: deps.Secrets,
	})
	if err != nil {
		return nil, fmt.Errorf("arr rescanner: %w", err)
	}

	worker, err := syncworker.New(syncworker.Dependencies{
		Store:     deps.Store,
		Settings:  deps.Settings,
		Executor:  executor,
		Muxer:     mux.New(mux.Dependencies{}),
		Rescanner: rescanner,
		WorkDir:   paths.WorkDir,
		Interval:  deps.Interval,
		Logf:      logf,
	})
	if err != nil {
		return nil, fmt.Errorf("sync worker: %w", err)
	}
	return worker, nil
}

// arrSyncWiring is the collaborator set for the M6-1 library poller.
type arrSyncWiring struct {
	Settings *settings.Store
	Store    *store.Store
	// Secrets reads arr.<kind>.api_key and re-encrypts it for arr_instances.
	Secrets arrsync.SecretStore
	// Interval overrides settings arr.options.poll_interval_sec. 0 reads the
	// setting; a negative value disables the poller (test seam, like the other
	// workers' Interval field).
	Interval time.Duration
	// Logf reports why the poller stays disabled. Nil means log.Printf.
	Logf func(format string, args ...any)
}

// wireArrSync builds the M6-1 poller over the enabled arr.connections.
//
// It returns (nil, nil) — the documented "disabled" case — when no connection is
// enabled or the interval is not positive, so a fresh install serves the wizard
// and a deliberately disabled sync never starts a goroutine. The pass refreshes
// arr_library_cache; the manual search bridge and approvals read that table.
func wireArrSync(ctx context.Context, deps arrSyncWiring) (*arrsync.Poller, error) {
	logf := deps.Logf
	if logf == nil {
		logf = log.Printf
	}
	if deps.Settings == nil || deps.Store == nil {
		return nil, errors.New("arr library sync needs the settings store and the database store")
	}
	if deps.Secrets == nil {
		return nil, errors.New("arr library sync needs the secret store for arr API keys")
	}

	conns, err := loadArrConnections(ctx, deps.Settings)
	if err != nil {
		return nil, err
	}
	enabled := 0
	for _, conn := range conns.Instances {
		if conn.Enabled && strings.TrimSpace(conn.BaseURL) != "" {
			enabled++
		}
	}
	if enabled == 0 {
		logf("dubarr: arr library sync disabled: no enabled arr instances")
		return nil, nil
	}

	interval := deps.Interval
	if interval == 0 {
		interval, err = loadArrSyncInterval(ctx, deps.Settings)
		if err != nil {
			return nil, err
		}
	}
	if interval <= 0 {
		logf("dubarr: arr library sync disabled: arr.options.poll_interval_sec is not positive")
		return nil, nil
	}

	poller, err := arrsync.New(arrsync.Deps{
		Store:    deps.Store,
		Settings: deps.Settings,
		Secrets:  deps.Secrets,
		Interval: interval,
		Logf:     logf,
	})
	if err != nil {
		return nil, fmt.Errorf("arr library sync: %w", err)
	}
	return poller, nil
}

// loadArrConnections reads the seeded arr.connections section. A missing row
// means no instance is configured yet.
func loadArrConnections(ctx context.Context, st *settings.Store) (settings.ArrConnections, error) {
	raw, ok, err := st.Get(ctx, settings.KeyArrConnections)
	if err != nil {
		return settings.ArrConnections{}, fmt.Errorf("read arr.connections: %w", err)
	}
	if !ok {
		return settings.ArrConnections{}, nil
	}
	var conns settings.ArrConnections
	if err := json.Unmarshal(raw, &conns); err != nil {
		return settings.ArrConnections{}, fmt.Errorf("decode arr.connections: %w", err)
	}
	return conns, nil
}

// loadArrSyncInterval reads arr.options.poll_interval_sec, defaulting to the
// settled 300 s cadence when the section is missing. A non-positive stored value
// is returned as-is so the caller can treat it as "disabled".
func loadArrSyncInterval(ctx context.Context, st *settings.Store) (time.Duration, error) {
	raw, ok, err := st.Get(ctx, settings.KeyArrOptions)
	if err != nil {
		return 0, fmt.Errorf("read arr.options: %w", err)
	}
	if !ok {
		return arrsync.DefaultInterval, nil
	}
	var opts settings.ArrOptions
	if err := json.Unmarshal(raw, &opts); err != nil {
		return 0, fmt.Errorf("decode arr.options: %w", err)
	}
	if opts.PollIntervalSec <= 0 {
		return 0, nil
	}
	return time.Duration(opts.PollIntervalSec) * time.Second, nil
}

// newVaultHook adapts vault.Writer to extract.VaultHook: one Write per staged
// track, all sharing a Donor Group UUID so sibling tracks stay linked.
func newVaultHook(writer *vault.Writer) extract.VaultHook {
	return func(ctx context.Context, job store.ImportExtractionJob, tracks []extract.StagedTrack) error {
		media, baseName, err := vaultMediaFor(job)
		if err != nil {
			return err
		}
		groupUUID, err := newDonorGroupUUID()
		if err != nil {
			return err
		}
		for _, track := range tracks {
			if _, err := writer.Write(ctx, vault.Request{
				StagedPath:        track.Path,
				BaseName:          baseName,
				Language:          track.Language,
				Media:             media,
				SeasonNumber:      job.SeasonNumber,
				EpisodeNumber:     job.EpisodeNumber,
				GroupUUID:         groupUUID,
				Variant:           track.Variant,
				Codec:             track.Codec,
				Channels:          track.Channels,
				ChannelCount:      track.ChannelCount,
				BitrateBPS:        track.BitrateBPS,
				SampleRate:        track.SampleRate,
				DurationMS:        track.Duration.Milliseconds(),
				SourceReleaseName: job.Filename,
			}); err != nil {
				return fmt.Errorf("archive %s: %w", filepath.Base(track.Path), err)
			}
		}
		return nil
	}
}

// vaultMediaFor derives the vault catalogue row and archive stem from the
// queue row's matched arr target. An unmatched donor cannot be catalogued and
// fails the row with a reason rather than archiving anonymously.
func vaultMediaFor(job store.ImportExtractionJob) (store.VaultMediaRecord, string, error) {
	title := strings.TrimSpace(job.Title)
	if title == "" {
		return store.VaultMediaRecord{}, "", fmt.Errorf("queue row %d has no matched library target", job.ID)
	}
	var mediaType string
	switch job.MediaType {
	case "movie":
		mediaType = "movie"
	case "episode":
		mediaType = "series"
	default:
		return store.VaultMediaRecord{}, "", fmt.Errorf("queue row %d has unsupported media type %q", job.ID, job.MediaType)
	}
	baseName := title
	if job.Year > 0 {
		baseName = fmt.Sprintf("%s (%d)", title, job.Year)
	}
	media := store.VaultMediaRecord{
		MediaType:            mediaType,
		Title:                title,
		Year:                 job.Year,
		TMDBID:               job.TMDBID,
		TVDBID:               job.TVDBID,
		OriginalLanguageISO2: job.OriginalLanguage,
	}
	return media, baseName, nil
}

// newDonorGroupUUID returns a random hex id linking the tracks archived from
// one donor (GLOSSARY "Donor Group").
func newDonorGroupUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate donor group uuid: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// loadStoragePaths reads the seeded storage.paths section. A missing row means
// the wizard has not run yet and yields the zero value (no watch dir).
func loadStoragePaths(ctx context.Context, st *settings.Store) (settings.StoragePaths, error) {
	raw, ok, err := st.Get(ctx, settings.KeyStoragePaths)
	if err != nil {
		return settings.StoragePaths{}, fmt.Errorf("read storage.paths: %w", err)
	}
	if !ok {
		return settings.StoragePaths{}, nil
	}
	var paths settings.StoragePaths
	if err := json.Unmarshal(raw, &paths); err != nil {
		return settings.StoragePaths{}, fmt.Errorf("decode storage.paths: %w", err)
	}
	return paths, nil
}

// loadGateRule converts target.tag_filter into the gate rule, falling back to
// the seeded default when the section is missing or unreadable.
func loadGateRule(ctx context.Context, st *settings.Store) (gate.Rule, error) {
	raw, ok, err := st.Get(ctx, settings.KeyTargetTagFilter)
	if err != nil {
		return gate.Rule{}, fmt.Errorf("read target.tag_filter: %w", err)
	}
	if !ok {
		return gate.RuleFromTagFilter(defaultTagFilter()), nil
	}
	var tf settings.TargetTagFilter
	if err := json.Unmarshal(raw, &tf); err != nil {
		return gate.Rule{}, fmt.Errorf("decode target.tag_filter: %w", err)
	}
	return gate.RuleFromTagFilter(tf), nil
}

// defaultTagFilter is the seeded target.tag_filter, used when the row is
// absent (fresh DATA_DIR before Seed, e.g. a hand-built test store).
func defaultTagFilter() settings.TargetTagFilter {
	tf, _ := settings.Defaults("")[settings.KeyTargetTagFilter].(settings.TargetTagFilter)
	return tf
}

// loadWatchInterval reads watch.options.scan_interval_sec, defaulting to the
// settled 10 s cadence when the section is missing or not positive.
func loadWatchInterval(ctx context.Context, st *settings.Store) (time.Duration, error) {
	raw, ok, err := st.Get(ctx, settings.KeyWatchOptions)
	if err != nil {
		return 0, fmt.Errorf("read watch.options: %w", err)
	}
	if !ok {
		return watch.DefaultInterval, nil
	}
	var opts settings.WatchOptions
	if err := json.Unmarshal(raw, &opts); err != nil {
		return 0, fmt.Errorf("decode watch.options: %w", err)
	}
	if opts.ScanIntervalSec <= 0 {
		return watch.DefaultInterval, nil
	}
	return time.Duration(opts.ScanIntervalSec) * time.Second, nil
}

// Handler exposes the wired mux (used by tests and embed checks).
func (s *server) Handler() http.Handler { return s.mux }

// Close stops the watch loop and releases the database handle. It is safe to
// call more than once.
func (s *server) Close() error {
	s.closeOnce.Do(func() {
		if s.scannerCancel != nil {
			s.scannerCancel()
		}
		if s.scanner != nil {
			s.scanner.Stop()
		}
		if s.extractCancel != nil {
			s.extractCancel()
		}
		if s.extract != nil {
			s.extract.Stop()
		}
		if s.syncCancel != nil {
			s.syncCancel()
		}
		if s.sync != nil {
			s.sync.Stop()
		}
		if s.libraryCancel != nil {
			s.libraryCancel()
		}
		if s.library != nil {
			s.library.Stop()
		}
		if s.logs != nil {
			if err := s.logs.Close(); err != nil && s.closeErr == nil {
				s.closeErr = err
			}
		}
		if s.store != nil {
			if err := s.store.Close(); err != nil && s.closeErr == nil {
				s.closeErr = err
			}
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
