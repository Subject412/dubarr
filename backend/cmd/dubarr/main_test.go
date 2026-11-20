package main

import (
	"bytes"
	"errors"
	"flag"
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
		"positional": {"serve"},
		"empty dir":  {"--data-dir", ""},
		"unknown":    {"--nope"},
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
