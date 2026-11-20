package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFlagsDefaults(t *testing.T) {
	cfg, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags(nil) returned error: %v", err)
	}
	if cfg.DataDir != "./data" {
		t.Errorf("default DataDir = %q, want %q", cfg.DataDir, "./data")
	}
	if cfg.Listen != "127.0.0.1:8080" {
		t.Errorf("default Listen = %q, want %q", cfg.Listen, "127.0.0.1:8080")
	}
	if cfg.Desktop {
		t.Errorf("default Desktop = true, want false")
	}
}

func TestParseFlagsOverrides(t *testing.T) {
	cfg, err := parseFlags([]string{"--data-dir", "/tmp/dd", "--listen", "0.0.0.0:9090", "--desktop"})
	if err != nil {
		t.Fatalf("parseFlags returned error: %v", err)
	}
	if cfg.DataDir != "/tmp/dd" {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, "/tmp/dd")
	}
	if cfg.Listen != "0.0.0.0:9090" {
		t.Errorf("Listen = %q, want %q", cfg.Listen, "0.0.0.0:9090")
	}
	if !cfg.Desktop {
		t.Errorf("Desktop = false, want true")
	}
}

func TestParseFlagsRejectsPositionalArgs(t *testing.T) {
	if _, err := parseFlags([]string{"serve"}); err == nil {
		t.Fatal("parseFlags accepted an unexpected positional argument")
	}
}

func TestParseFlagsRejectsEmptyDataDir(t *testing.T) {
	if _, err := parseFlags([]string{"--data-dir", ""}); err == nil {
		t.Fatal("parseFlags accepted an empty --data-dir")
	}
}

func TestParseFlagsRejectsUnknownFlag(t *testing.T) {
	if _, err := parseFlags([]string{"--nope"}); err == nil {
		t.Fatal("parseFlags accepted an unknown flag")
	}
}

// TestParseFlagsHelpSignalsHelp checks that help is surfaced as flag.ErrHelp
// so main can print usage and exit 0 instead of treating it as a usage error.
func TestParseFlagsHelpSignalsHelp(t *testing.T) {
	_, err := parseFlags([]string{"--help"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parseFlags(--help) error = %v, want flag.ErrHelp", err)
	}
}

func TestRunHelpPrintsUsageAndExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(--help) = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"Usage: dubarr", "data-dir", "listen", "desktop"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage output missing %q; got:\n%s", want, out)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("run(--help) wrote to stderr: %q", stderr.String())
	}
}

func TestRunReportsUsageErrorsOnStderr(t *testing.T) {
	cases := map[string][]string{
		"positional":   {"serve"},
		"empty dir":    {"--data-dir", ""},
		"empty listen": {"--listen", ""},
		"unknown":      {"--nope"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 2 {
				t.Fatalf("run(%v) = %d, want 2", args, code)
			}
			if stderr.Len() == 0 {
				t.Errorf("run(%v) wrote no error to stderr", args)
			}
			if stdout.Len() != 0 {
				t.Errorf("run(%v) wrote to stdout: %q", args, stdout.String())
			}
		})
	}
}

func TestParseFlagsRejectsEmptyListen(t *testing.T) {
	if _, err := parseFlags([]string{"--listen", ""}); err == nil {
		t.Fatal("parseFlags accepted an empty --listen")
	}
}

// TestRunSuccess exercises the happy path: DATA_DIR is created 0700, the server
// binds, reports its address, and a cancelled context shuts it down cleanly.
func TestRunSuccess(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "data")
	useKeyfileSecrets(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // request shutdown as soon as the listener is up

	var stdout, stderr bytes.Buffer
	code := runContext(ctx, []string{"--data-dir", dataDir, "--listen", "127.0.0.1:0"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runContext() = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("runContext() wrote to stderr: %q", stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{dataDir, "listening on http://127.0.0.1:", "shut down"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}

	info, err := os.Stat(dataDir)
	if err != nil {
		t.Fatalf("stat data dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s was not created as a directory", dataDir)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("data dir perm = %o, want 700", perm)
	}
}

// TestRunMkdirFailure checks that an unusable data-dir is reported on stderr
// with exit code 1. The parent of the data dir is a regular file, so
// os.MkdirAll fails with ENOTDIR.
func TestRunMkdirFailure(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	useKeyfileSecrets(t)

	var stdout, stderr bytes.Buffer
	code := runContext(context.Background(), []string{"--data-dir", filepath.Join(blocker, "data")}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("runContext() = %d, want 1", code)
	}
	if stderr.Len() == 0 {
		t.Error("runContext() wrote no error to stderr")
	}
	if stdout.Len() != 0 {
		t.Errorf("runContext() wrote to stdout: %q", stdout.String())
	}
}
