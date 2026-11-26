package dsp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fftModule and fftPinnedVersion are the pin this package's DSP work is built
// and validated against. The version is exact on purpose: the library shipped
// releases between v0.25.0 and v0.26.2 that returned silently wrong results,
// and the golden vectors in testdata were captured for this exact version.
const (
	fftModule        = "github.com/go-fft/fft"
	fftPinnedVersion = "v0.49.0"
)

// TestFFTPinned reads backend/go.mod and fails when the dependency has moved.
// A `go get -u` / `go mod tidy -go=...` in some future change is the way this
// pin slides without anyone noticing, so the failure message says what to do
// instead of just reporting a mismatch.
func TestFFTPinned(t *testing.T) {
	path := filepath.Join("..", "..", "go.mod")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	got, ok := requiredVersion(string(data), fftModule)
	if !ok {
		t.Fatalf("%s is not required in backend/go.mod; it must be a direct requirement at exactly %s.\n"+
			"Restore the line `require %s %s` (no `// indirect`, no version range).",
			fftModule, fftPinnedVersion, fftModule, fftPinnedVersion)
	}
	if got != fftPinnedVersion {
		t.Fatalf("backend/go.mod requires %s %s, want exactly %s.\n"+
			"Do not take this bump silently: the pin is the guard against the v0.25.0-v0.26.2 "+
			"wrong-result releases, and every vector under backend/internal/dsp/testdata was "+
			"captured against %s. To move it deliberately, update fftPinnedVersion in this file, "+
			"bump the requirement on purpose, and re-capture the goldens in "+
			"backend/internal/dsp/testdata against the reference engine before the change lands.",
			fftModule, got, fftPinnedVersion, fftPinnedVersion)
	}
}

// requiredVersion returns the version required for mod in a go.mod body, or
// ok=false when the module is not listed. Both `require (...)` blocks and
// single-line `require m v` directives are handled; the first match wins.
func requiredVersion(gomod, mod string) (version string, ok bool) {
	inBlock := false
	for _, line := range strings.Split(gomod, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		switch {
		case len(fields) >= 2 && fields[0] == "require" && fields[1] == "(":
			inBlock = true
			continue
		case inBlock && len(fields) == 1 && fields[0] == ")":
			inBlock = false
			continue
		case len(fields) == 3 && fields[0] == "require":
			fields = fields[1:]
		case !inBlock:
			continue
		}
		if len(fields) >= 2 && fields[0] == mod {
			return fields[1], true
		}
	}
	return "", false
}
