package main

import "testing"

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
