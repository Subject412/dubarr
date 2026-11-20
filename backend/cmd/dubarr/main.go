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
	// Desktop marks a desktop (native) deployment. This flag itself only opens
	// the default browser via xdg-open (T6); it does not pick the secrets
	// backend. Keyring-vs-keyfile is chosen by deployment detection — native
	// installs use the OS keyring, the Docker image uses the 0600 keyfile (T4).
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
	fmt.Fprintln(w)
	fs.PrintDefaults()
}

// run executes the command and returns its exit code so tests can drive it.
func run(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeUsage(stdout)
			return 0
		}
		fmt.Fprintf(stderr, "dubarr: %v\n", err)
		return 2
	}

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "dubarr: create data dir %q: %v\n", cfg.DataDir, err)
		return 1
	}

	fmt.Fprintf(stdout, "dubarr config:\n  data-dir: %s\n  listen:   %s\n  desktop:  %t\n",
		cfg.DataDir, cfg.Listen, cfg.Desktop)
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
