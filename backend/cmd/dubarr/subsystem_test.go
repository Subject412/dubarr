package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/library"
	"github.com/Subject412/dubarr/backend/internal/logs"
	"github.com/Subject412/dubarr/backend/internal/secrets"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// supervisorRig is a seeded store plus a supervisor whose settings are still
// the empty defaults the first-run wizard has not overwritten yet.
type supervisorRig struct {
	t        *testing.T
	dir      string
	store    *store.Store
	settings *settings.Store
	logger   *logs.Logger
	sup      *subsystemSupervisor
	attached []library.Refresher
}

func newSupervisorRig(t *testing.T) *supervisorRig {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	settingsStore := settings.New(st.DB)
	if err := settingsStore.Seed(context.Background(), dir); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	sec, err := secrets.Open(st, secrets.Options{DataDir: dir})
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	logger, err := logs.New(logs.Options{Level: "debug", Stderr: io.Discard})
	if err != nil {
		t.Fatalf("logs.New: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	rig := &supervisorRig{t: t, dir: dir, store: st, settings: settingsStore, logger: logger}
	rig.sup = newSubsystemSupervisor(supervisorWiring{
		Settings: settingsStore,
		Store:    st,
		Secrets:  sec,
		DataDir:  dir,
		Logger:   logger,
		AttachLibrary: func(r library.Refresher) {
			rig.attached = append(rig.attached, r)
		},
	})
	t.Cleanup(rig.sup.Stop)
	return rig
}

// enableArrInstance writes an enabled arr.connections section, which is what a
// finished wizard leaves behind. The base URL is a closed local port: the
// poller's first pass fails fast and never leaves the machine.
func (r *supervisorRig) enableArrInstance() {
	r.t.Helper()
	conns, err := json.Marshal(settings.ArrConnections{Instances: []settings.ArrConnection{{
		Kind:    "radarr",
		BaseURL: "http://127.0.0.1:1",
		Enabled: true,
	}}})
	if err != nil {
		r.t.Fatalf("marshal arr connections: %v", err)
	}
	if err := r.settings.Set(context.Background(), settings.KeyArrConnections, conns); err != nil {
		r.t.Fatalf("set arr connections: %v", err)
	}
}

// setStoragePaths writes the wizard's storage.paths section.
func (r *supervisorRig) setStoragePaths(paths settings.StoragePaths) {
	r.t.Helper()
	raw, err := json.Marshal(paths)
	if err != nil {
		r.t.Fatalf("marshal storage paths: %v", err)
	}
	if err := r.settings.Set(context.Background(), settings.KeyStoragePaths, raw); err != nil {
		r.t.Fatalf("set storage paths: %v", err)
	}
}

// mkdir makes a directory under the rig root and returns it.
func (r *supervisorRig) mkdir(name string) string {
	r.t.Helper()
	dir := filepath.Join(r.dir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		r.t.Fatalf("mkdir %s: %v", name, err)
	}
	return dir
}

// hasLog reports whether the ring holds a message at or above minLevel for the
// component containing substr.
func (r *supervisorRig) hasLog(minLevel, component, substr string) bool {
	r.t.Helper()
	for _, entry := range r.logger.Entries(logs.Query{MinLevel: minLevel, Component: component}) {
		if strings.Contains(entry.Message, substr) {
			return true
		}
	}
	return false
}

// TestSupervisorStartsPollerWhenInstanceAppears pins the #53 path: a fresh
// install has no poller, the wizard saves one instance, and the settings write
// hook starts the poller (and publishes it to the library API) with an info
// log naming arr.connections.
func TestSupervisorStartsPollerWhenInstanceAppears(t *testing.T) {
	rig := newSupervisorRig(t)
	rig.settings.OnWrite(rig.sup.HandleSettingsWrite)

	// Boot-equivalent: no instance configured, so reconcile starts nothing.
	rig.sup.reconcile(context.Background())
	if rig.sup.library != nil {
		t.Fatal("supervisor built a poller with no arr instance configured")
	}

	rig.enableArrInstance()

	if rig.sup.library == nil {
		t.Fatal("supervisor did not start a poller after an enabled instance appeared")
	}
	if len(rig.attached) != 1 || rig.attached[0] != rig.sup.library {
		t.Fatalf("library API got %v, want the poller the supervisor started", rig.attached)
	}
	if !rig.hasLog("info", "arrsync", "subsystem started after a settings save") {
		t.Error("no info log naming the arr.connections activation")
	}
}

// TestSupervisorDoesNotStartPollerTwice pins idempotency: a second settings save
// must not build (and start) a second poller.
func TestSupervisorDoesNotStartPollerTwice(t *testing.T) {
	rig := newSupervisorRig(t)
	rig.enableArrInstance()

	rig.sup.reconcile(context.Background())
	first := rig.sup.library
	if first == nil {
		t.Fatal("supervisor did not start a poller for an enabled instance")
	}

	rig.sup.reconcile(context.Background())
	if rig.sup.library != first {
		t.Error("second reconcile replaced the running poller")
	}
	if len(rig.attached) != 1 {
		t.Errorf("library API received %d pollers, want 1", len(rig.attached))
	}
}

// canonicalRoot resolves a directory the way watch.New does, so the assertion
// survives a symlinked temp root.
func canonicalRoot(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("eval symlinks %s: %v", dir, err)
	}
	return resolved
}

// TestSupervisorWarnsInsteadOfSwappingPaths pins the safety rule: a save that
// changes watch_dir while the scanner runs must not cancel or replace it, and
// must log a restart warning instead.
func TestSupervisorWarnsInsteadOfSwappingPaths(t *testing.T) {
	rig := newSupervisorRig(t)
	rig.settings.OnWrite(rig.sup.HandleSettingsWrite)

	firstWatch := rig.mkdir("watch-a")
	rig.setStoragePaths(settings.StoragePaths{WatchDir: firstWatch, WorkDir: filepath.Join(rig.dir, "work")})
	if rig.sup.scanner == nil {
		t.Fatal("supervisor did not start a scanner for a configured watch_dir")
	}
	running := rig.sup.scanner

	secondWatch := rig.mkdir("watch-b")
	rig.setStoragePaths(settings.StoragePaths{WatchDir: secondWatch, WorkDir: filepath.Join(rig.dir, "work")})

	if rig.sup.scanner != running {
		t.Error("a path change replaced the running scanner")
	}
	if roots := running.Roots(); len(roots) != 1 || roots[0] != canonicalRoot(t, firstWatch) {
		t.Errorf("running scanner roots = %v, want the original %v", roots, firstWatch)
	}
	if !rig.hasLog("warn", "watch", "restart") {
		t.Error("no restart-required warning logged for the changed watch_dir")
	}
}
