package arrsync

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/logs"
	"github.com/Subject412/dubarr/backend/internal/settings"
)

// The fake credentials this test feeds the pipeline. They are distinctive so a
// leak is unambiguous, and shaped like the real things (an arr key and a bearer
// token) so a rerun of the redaction rules cannot miss them by accident.
const (
	testAPIKey    = "dub_TESTKEY_0123456789abcdef"
	testBearerTok = "dub_BEARER_0123456789abcdef"
)

// authHeader and authScheme are constants so the leak is assembled at runtime
// the way a real client writes its request, instead of sitting in this file as
// a literal header line.
const (
	authHeader = "Authorization"
	authScheme = "Bearer"
)

// credentialPattern is what a leaked line looks like: a secret keyword followed
// by a credential-shaped run. A redacted line ends in "***", which is too short
// to match, so this pattern is a real assertion rather than a tautology.
var credentialPattern = regexp.MustCompile(`(?i)\b(?:x-api-key|api[_-]?key|apikey|authorization|bearer|token)\b\s*[:=]?\s*[A-Za-z0-9._~+/=-]{12,}`)

// newRingLogger builds the real logger over a throwaway stderr buffer, so these
// tests read the ring exactly like internal/logs/logs_test.go does.
func newRingLogger(t *testing.T) *logs.Logger {
	t.Helper()
	var stderr bytes.Buffer
	lg, err := logs.New(logs.Options{Level: "debug", Stderr: &stderr})
	if err != nil {
		t.Fatalf("logs.New: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	return lg
}

// TestPassScrubsArrCredentialsFromEveryRingLine runs a real pass over two
// instances — one that caches, one whose upstream refuses the request and
// echoes the URL and the Authorization header back — with the debug level on,
// then walks the whole ring. No line may carry the API key or the bearer token,
// and no line may match the credential shape. The pass lines themselves must
// still be there and still be readable, so this cannot pass by logging nothing.
func TestPassScrubsArrCredentialsFromEveryRingLine(t *testing.T) {
	r := newRig(t)
	sonarr := sonarrServer(t)
	r.configure(
		settings.ArrConnection{Kind: "sonarr", BaseURL: sonarr.URL, Enabled: true},
		settings.ArrConnection{Kind: "radarr", BaseURL: "http://radarr.invalid", Enabled: true},
	)
	// The API key is a real input: it is stored in the secret store and handed
	// to the client builder below.
	r.key("sonarr", sonarr.APIKey())
	r.key("radarr", testAPIKey)

	// The exact shape a leaky transport error or a verbose upstream takes.
	leaky := fmt.Errorf("GET %s/api/v3/movie?apikey=%s rejected: %s: %s %s",
		"http://radarr.invalid", testAPIKey, authHeader, authScheme, testBearerTok)
	if !credentialPattern.MatchString(leaky.Error()) {
		t.Fatalf("the credential pattern is toothless: it does not match %q", leaky.Error())
	}

	lg := newRingLogger(t) // debug: per-cache-row lines are in the ring too
	p, err := New(Deps{
		Store:    r.store,
		Settings: r.settings,
		Secrets:  r.secrets,
		Interval: time.Minute,
		Timeout:  testHTTPTimeout,
		Logf:     lg.Logf("arrsync"),
		Debugf:   lg.Debugf("arrsync"),
		NewRadarr: func(baseURL, apiKey string) (RadarrSource, error) {
			// Echo both credentials the way a refusing upstream would.
			return nil, fmt.Errorf("%s?apikey=%s: authorization: bearer %s", baseURL, apiKey, testBearerTok)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stats, _ := p.Sync(context.Background())
	if stats.Instances != 2 || stats.Failed != 1 {
		t.Fatalf("stats = %+v, want 2 instances and 1 failed", stats)
	}

	entries := lg.Entries(logs.Query{})
	if len(entries) < 6 {
		t.Fatalf("ring holds %d entries, want the pass lines plus per-row debug: %+v", len(entries), entries)
	}

	var joined strings.Builder
	for _, e := range entries {
		joined.WriteString(e.Message)
		joined.WriteString("\n")
		if strings.Contains(e.Message, testAPIKey) {
			t.Errorf("%s line leaked the API key: %q", e.Level, e.Message)
		}
		if strings.Contains(e.Message, testBearerTok) {
			t.Errorf("%s line leaked the bearer token: %q", e.Level, e.Message)
		}
		if hit := credentialPattern.FindString(e.Message); hit != "" {
			t.Errorf("%s line matches the credential shape (%q): %q", e.Level, hit, e.Message)
		}
	}

	all := joined.String()
	// Positive controls: the ring really did record the pass and a debug row,
	// so "no leak" is not "no logging".
	for _, want := range []string{
		"arrsync: all refresh pass started: 2 instance(s)",
		"arrsync: sonarr: pass finished: 3 inserted, 0 updated, 0 unchanged, 0 deleted",
		"cache row episode/42",
		"arrsync: radarr:",
		"***",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("ring is missing %q:\n%s", want, all)
		}
	}
}
