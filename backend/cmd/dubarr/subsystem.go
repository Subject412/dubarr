package main

// Post-boot subsystem activation (issue #53).
//
// Every pipeline subsystem is wired once at boot from settings the first-run
// wizard writes *after* boot. A setting that was empty at boot therefore left
// its subsystem nil forever: the library poller never existed, the watch folder
// was never scanned, and approvals stayed queued — until a restart re-ran the
// wiring. The wizard's connection test passed because it makes a live call and
// never touches the pipeline.
//
// The subsystemSupervisor below closes that gap without moving the boot wiring:
// after a successful settings write it re-reads the settings and constructs
// only the subsystems that are currently missing, reusing the same wire*
// constructors (and therefore the same validation) as boot.
//
// It never cancels or replaces a subsystem that is already running. A settings
// save must not kill a worker mid-mux, and swapping the vault, work or watch
// directory under a live worker is not safe, so a changed path setting is
// reported as "restart required" and the running worker is left alone.

import (
	"context"
	"strings"
	"sync"

	"github.com/Subject412/dubarr/backend/internal/arrsync"
	"github.com/Subject412/dubarr/backend/internal/extract"
	"github.com/Subject412/dubarr/backend/internal/library"
	"github.com/Subject412/dubarr/backend/internal/logs"
	"github.com/Subject412/dubarr/backend/internal/secrets"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
	"github.com/Subject412/dubarr/backend/internal/syncworker"
	"github.com/Subject412/dubarr/backend/internal/watch"
	"github.com/Subject412/dubarr/backend/internal/workdir"
)

// supervisorWiring is the collaborator set for the post-boot supervisor. It
// mirrors the boot wiring so the supervisor can build the same subsystems.
type supervisorWiring struct {
	Settings *settings.Store
	Store    *store.Store
	// Secrets backs the arr API key reads and rescans both workers need.
	Secrets *secrets.Service
	DataDir string
	Logger  *logs.Logger
	// AttachLibrary publishes a poller built after boot to the library API,
	// which captured its Refresher when the routes were registered. Nil is
	// allowed (the refresh route then keeps reporting "not configured").
	AttachLibrary func(library.Refresher)
}

// bootSubsystems is what boot managed to build. A nil member (and its nil
// cancel) means the subsystem stayed disabled at boot.
type bootSubsystems struct {
	Scanner     *watch.Scanner
	ScannerStop context.CancelFunc
	Extract     *extract.Worker
	ExtractStop context.CancelFunc
	Sync        *syncworker.Worker
	SyncStop    context.CancelFunc
	Library     *arrsync.Poller
	LibraryStop context.CancelFunc
}

// subsystemSupervisor starts pipeline subsystems whose settings became complete
// after boot, and owns the lifecycle of every subsystem (boot's and its own).
type subsystemSupervisor struct {
	settings *settings.Store
	store    *store.Store
	secrets  *secrets.Service
	dataDir  string
	logger   *logs.Logger

	attachLibrary func(library.Refresher)

	mu      sync.Mutex
	stopped bool

	scanner *watch.Scanner
	// scannerStop cancels the scanner's loop context; the scanner itself is
	// stopped through Stop so no run goroutine is left behind.
	scannerStop     context.CancelFunc
	scannerWatchDir string

	extract         *extract.Worker
	extractStop     context.CancelFunc
	extractVaultDir string

	sync         *syncworker.Worker
	syncStop     context.CancelFunc
	syncVaultDir string
	syncWorkDir  string

	library     *arrsync.Poller
	libraryStop context.CancelFunc
}

// newSubsystemSupervisor builds the supervisor. It starts nothing; adoptBoot
// records what boot built and HandleSettingsWrite runs the reconciliation.
func newSubsystemSupervisor(deps supervisorWiring) *subsystemSupervisor {
	return &subsystemSupervisor{
		settings:      deps.Settings,
		store:         deps.Store,
		secrets:       deps.Secrets,
		dataDir:       deps.DataDir,
		logger:        deps.Logger,
		attachLibrary: deps.AttachLibrary,
	}
}

// adoptBoot records the subsystems boot started, together with the storage
// paths they were built against, so a later path change can be reported as
// restart-only instead of being swapped silently.
func (s *subsystemSupervisor) adoptBoot(ctx context.Context, boot bootSubsystems) {
	s.mu.Lock()
	defer s.mu.Unlock()

	paths, err := loadStoragePaths(ctx, s.settings)
	if err != nil {
		// A snapshot is a diagnostic, not a gate: reconcile re-reads anyway.
		paths = settings.StoragePaths{}
	} else {
		s.ensureWorkRoot(paths)
	}
	if boot.Scanner != nil {
		s.scanner = boot.Scanner
		s.scannerStop = boot.ScannerStop
		s.scannerWatchDir = strings.TrimSpace(paths.WatchDir)
	}
	if boot.Extract != nil {
		s.extract = boot.Extract
		s.extractStop = boot.ExtractStop
		s.extractVaultDir = strings.TrimSpace(paths.VaultDir)
	}
	if boot.Sync != nil {
		s.sync = boot.Sync
		s.syncStop = boot.SyncStop
		s.syncVaultDir = strings.TrimSpace(paths.VaultDir)
		s.syncWorkDir = strings.TrimSpace(paths.WorkDir)
	}
	if boot.Library != nil {
		s.library = boot.Library
		s.libraryStop = boot.LibraryStop
	}
}

// ensureWorkRoot creates the temp folder at boot, so a missing folder, a
// read-only mount or a path that is a regular file is visible now instead of at
// the operator's first job. Every failure is a warning, not a boot error: the
// appliance keeps serving and the job that needs it fails with its own error.
//
// A temp folder inside the vault is refused outright. Staged copies are deleted
// per job, and the next vault pass would reorganise anything it finds there, so
// creating the folder inside the vault is the one outcome worth preventing.
func (s *subsystemSupervisor) ensureWorkRoot(paths settings.StoragePaths) {
	workDir := strings.TrimSpace(paths.WorkDir)
	err := settings.CheckWorkDirOutsideVault(workDir, strings.TrimSpace(paths.VaultDir))
	if err == nil {
		err = workdir.EnsureRoot(workDir)
	}
	if err == nil {
		return
	}
	s.logger.Logger().Warn("temp folder is not usable; jobs will fail until it is fixed",
		"component", "storage", "setting", "storage.paths.work_dir", "path", workDir, "error", err)
}

// HandleSettingsWrite is the settings.Store write hook: after a save commits it
// starts any subsystem whose settings are now complete. It never stops one.
// Both the settings editor and the first-run wizard save through the store, so
// this single hook covers both.
func (s *subsystemSupervisor) HandleSettingsWrite([]string) {
	s.reconcile(context.Background())
}

// reconcile re-reads the settings and starts every missing subsystem that is
// now configured. Already-running subsystems are left untouched.
func (s *subsystemSupervisor) reconcile(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}

	paths, err := loadStoragePaths(ctx, s.settings)
	if err != nil {
		s.logger.Logger().Warn("could not re-read storage.paths; subsystems left unchanged",
			"component", "supervisor", "error", err)
		return
	}

	s.warnPathChanges(paths)
	s.startScanner(paths)
	s.startExtract(paths)
	s.startSync(paths)
	s.startLibrary()
}

// warnPathChanges reports a path setting that changed while its subsystem is
// running. The worker keeps its old directory: swapping roots under a live
// worker is unsafe, so the operator has to restart.
func (s *subsystemSupervisor) warnPathChanges(paths settings.StoragePaths) {
	warn := func(component, setting, running, configured string) {
		if running == configured {
			return
		}
		s.logger.Logger().Warn("subsystem is running with its previous path; restart dubarr to apply the new one",
			"component", component, "setting", setting, "running", running, "configured", configured)
	}
	if s.scanner != nil {
		warn("watch", "storage.paths.watch_dir", s.scannerWatchDir, strings.TrimSpace(paths.WatchDir))
	}
	if s.extract != nil {
		warn("extract", "storage.paths.vault_dir", s.extractVaultDir, strings.TrimSpace(paths.VaultDir))
	}
	if s.sync != nil {
		warn("syncworker", "storage.paths.vault_dir", s.syncVaultDir, strings.TrimSpace(paths.VaultDir))
		warn("syncworker", "storage.paths.work_dir", s.syncWorkDir, strings.TrimSpace(paths.WorkDir))
	}
}

// startScanner builds and starts the watch scanner when none is running. The
// wire constructor decides eligibility (an empty watch_dir returns nil), so
// reconcile never repeats that validation.
func (s *subsystemSupervisor) startScanner(paths settings.StoragePaths) {
	if s.scanner != nil {
		return
	}
	runCtx, cancel := context.WithCancel(context.Background())
	scanner, err := wireScanner(runCtx, scannerWiring{
		Settings: s.settings,
		Store:    s.store,
		Logf:     s.logger.Logf("watch"),
		Debugf:   s.logger.Debugf("watch"),
	})
	if err != nil {
		cancel()
		s.logger.Logger().Warn("watch scanner could not start after a settings save",
			"component", "watch", "error", err)
		return
	}
	if scanner == nil {
		cancel()
		return
	}
	scanner.Start(runCtx)
	s.scanner = scanner
	s.scannerStop = cancel
	s.scannerWatchDir = strings.TrimSpace(paths.WatchDir)
	s.activated("watch", "storage.paths.watch_dir")
}

// startExtract builds and starts the approval-to-vault worker when none is
// running. wireExtractWorker returns nil until vault_dir is configured.
func (s *subsystemSupervisor) startExtract(paths settings.StoragePaths) {
	if s.extract != nil {
		return
	}
	runCtx, cancel := context.WithCancel(context.Background())
	worker, err := wireExtractWorker(runCtx, extractWiring{
		Settings: s.settings,
		Store:    s.store,
		DataDir:  s.dataDir,
		Logf:     s.logger.Logf("extract"),
		Debugf:   s.logger.Debugf("extract"),
	})
	if err != nil {
		cancel()
		s.logger.Logger().Warn("extraction worker could not start after a settings save",
			"component", "extract", "error", err)
		return
	}
	if worker == nil {
		cancel()
		return
	}
	worker.Start(runCtx)
	s.extract = worker
	s.extractStop = cancel
	s.extractVaultDir = strings.TrimSpace(paths.VaultDir)
	s.activated("extract", "storage.paths.vault_dir")
}

// startSync builds and starts the Execute→mux→rescan worker when none is
// running. wireSyncWorker returns nil until vault_dir, work_dir and
// sync.defaults are all present.
func (s *subsystemSupervisor) startSync(paths settings.StoragePaths) {
	if s.sync != nil {
		return
	}
	runCtx, cancel := context.WithCancel(context.Background())
	worker, err := wireSyncWorker(runCtx, syncWiring{
		Settings: s.settings,
		Store:    s.store,
		Secrets:  s.secrets,
		Logf:     s.logger.Logf("syncworker"),
		Errorf:   s.logger.Errorf("syncworker"),
	})
	if err != nil {
		cancel()
		s.logger.Logger().Warn("sync worker could not start after a settings save",
			"component", "syncworker", "error", err)
		return
	}
	if worker == nil {
		cancel()
		return
	}
	worker.Start(runCtx)
	s.sync = worker
	s.syncStop = cancel
	s.syncVaultDir = strings.TrimSpace(paths.VaultDir)
	s.syncWorkDir = strings.TrimSpace(paths.WorkDir)
	s.activated("syncworker", "storage.paths.vault_dir, storage.paths.work_dir and sync.defaults")
}

// startLibrary builds and starts the arr library poller when none is running,
// and publishes it to the library API so POST /api/library/refresh stops
// answering "not configured". wireArrSync returns nil until an enabled instance
// exists.
func (s *subsystemSupervisor) startLibrary() {
	if s.library != nil {
		return
	}
	runCtx, cancel := context.WithCancel(context.Background())
	poller, err := wireArrSync(runCtx, arrSyncWiring{
		Settings: s.settings,
		Store:    s.store,
		Secrets:  s.secrets,
		Logf:     s.logger.Logf("arrsync"),
		Debugf:   s.logger.Debugf("arrsync"),
	})
	if err != nil {
		cancel()
		s.logger.Logger().Warn("arr library sync could not start after a settings save",
			"component", "arrsync", "error", err)
		return
	}
	if poller == nil {
		cancel()
		return
	}
	poller.Start(runCtx)
	s.library = poller
	s.libraryStop = cancel
	if s.attachLibrary != nil {
		s.attachLibrary(poller)
	}
	s.activated("arrsync", "arr.connections")
}

// Post-boot subsystems get the same log seams as the boot ones: a worker
// started after a settings save must be as visible in /logs as one started at
// boot, otherwise the "why did nothing happen" question comes straight back
// for a subsystem the wizard activated. The constructors' "still disabled"
// messages are acceptable here because reconcile only calls a constructor for
// a subsystem it believes is missing.

// activated logs the activation exactly once per subsystem, naming the setting
// that made it eligible. Silence here was part of the field report's "no logs".
func (s *subsystemSupervisor) activated(component, setting string) {
	s.logger.Logger().Info("subsystem started after a settings save",
		"component", component, "setting", setting)
}

// Stop cancels and drains every subsystem the supervisor owns, including the
// ones boot started. It is safe to call more than once.
func (s *subsystemSupervisor) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	scanner, scannerStop := s.scanner, s.scannerStop
	extractWorker, extractStop := s.extract, s.extractStop
	syncWorker, syncStop := s.sync, s.syncStop
	poller, libraryStop := s.library, s.libraryStop
	s.mu.Unlock()

	if scannerStop != nil {
		scannerStop()
	}
	if scanner != nil {
		scanner.Stop()
	}
	if extractStop != nil {
		extractStop()
	}
	if extractWorker != nil {
		extractWorker.Stop()
	}
	if syncStop != nil {
		syncStop()
	}
	if syncWorker != nil {
		syncWorker.Stop()
	}
	if libraryStop != nil {
		libraryStop()
	}
	if poller != nil {
		poller.Stop()
	}
}
