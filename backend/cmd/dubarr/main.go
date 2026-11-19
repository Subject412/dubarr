// Command dubarr is the Dubarr appliance entrypoint.
//
// At M1 this binary only resolves and reports its runtime configuration; the
// HTTP server, database bootstrap and feature wiring land in later tasks.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

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
	// Desktop marks a desktop (native) deployment, which selects the OS
	// keyring for the secrets master key instead of the 0600 keyfile.
	Desktop bool
}

// parseFlags parses CLI arguments into a Config. It returns an error rather
// than exiting so callers (and tests) stay in control of the process.
func parseFlags(args []string) (Config, error) {
	fs := flag.NewFlagSet("dubarr", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var cfg Config
	fs.StringVar(&cfg.DataDir, "data-dir", "./data", "appliance data directory")
	fs.StringVar(&cfg.Listen, "listen", "127.0.0.1:8080", "HTTP listen address")
	fs.BoolVar(&cfg.Desktop, "desktop", false, "run as a desktop (native) deployment")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	if cfg.DataDir == "" {
		return Config{}, errors.New("--data-dir must not be empty")
	}
	return cfg, nil
}

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "dubarr: %v\n", err)
		os.Exit(2)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "dubarr: create data dir %q: %v\n", cfg.DataDir, err)
		os.Exit(1)
	}

	fmt.Printf("dubarr config:\n  data-dir: %s\n  listen:   %s\n  desktop:  %t\n",
		cfg.DataDir, cfg.Listen, cfg.Desktop)
}
