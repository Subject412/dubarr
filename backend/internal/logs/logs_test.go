package logs

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestLogger(t *testing.T, opts Options) (*Logger, *bytes.Buffer) {
	t.Helper()
	var stderr bytes.Buffer
	opts.Stderr = &stderr
	if opts.Level == "" {
		opts.Level = "info"
	}
	l, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, &stderr
}

func TestLevelFiltering(t *testing.T) {
	l, _ := newTestLogger(t, Options{})
	log := l.Logger()

	log.Debug("debug hidden")
	log.Info("info kept")
	log.Warn("warn kept")

	entries := l.Entries(Query{})
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (debug must be filtered at info): %+v", len(entries), entries)
	}
	if entries[0].Message != "info kept" || entries[1].Message != "warn kept" {
		t.Fatalf("unexpected entries: %+v", entries)
	}

	if err := l.SetLevel("debug"); err != nil {
		t.Fatalf("SetLevel: %v", err)
	}
	if got := l.Level(); got != "debug" {
		t.Fatalf("Level() = %q, want debug", got)
	}
	log.Debug("debug now kept")
	entries = l.Entries(Query{})
	if len(entries) != 3 || entries[2].Message != "debug now kept" {
		t.Fatalf("debug entry missing after SetLevel: %+v", entries)
	}
}

func TestInvalidLevelRejected(t *testing.T) {
	l, _ := newTestLogger(t, Options{Level: "trace"})
	if l.Level() != "info" {
		t.Fatalf("invalid level should fall back to info, got %q", l.Level())
	}
	if err := l.SetLevel("loud"); err == nil {
		t.Fatal("SetLevel(loud) accepted an unknown level")
	}
}

func TestRingEviction(t *testing.T) {
	l, _ := newTestLogger(t, Options{Capacity: 3})
	log := l.Logger()
	for i := 0; i < 5; i++ {
		log.Info(fmt.Sprintf("entry %d", i))
	}
	entries := l.Entries(Query{})
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	if entries[0].Message != "entry 2" || entries[2].Message != "entry 4" {
		t.Fatalf("eviction kept the wrong entries: %+v", entries)
	}
}

func TestStructuredFields(t *testing.T) {
	l, _ := newTestLogger(t, Options{})
	l.Logger().Info("job started", "component", "syncworker", "job_id", "7", "media_id", "42")
	entries := l.Entries(Query{})
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Component != "syncworker" || e.JobID != "7" || e.MediaID != "42" {
		t.Fatalf("structured fields lost: %+v", e)
	}
	if e.Level != "info" || e.Time.IsZero() || e.Message != "job started" {
		t.Fatalf("basic fields wrong: %+v", e)
	}
}

func TestQueryFilters(t *testing.T) {
	l, _ := newTestLogger(t, Options{Level: "debug"})
	log := l.Logger()
	log.Debug("dbg", "component", "a")
	log.Info("info a", "component", "a")
	log.Warn("warn b", "component", "b")
	log.Error("error b", "component", "b")

	if got := l.Entries(Query{MinLevel: "warn"}); len(got) != 2 {
		t.Fatalf("MinLevel=warn returned %d entries, want 2: %+v", len(got), got)
	}
	if got := l.Entries(Query{MinLevel: "debug"}); len(got) != 4 {
		t.Fatalf("MinLevel=debug returned %d entries, want 4", len(got))
	}
	if got := l.Entries(Query{Component: "b"}); len(got) != 2 {
		t.Fatalf("Component=b returned %d entries, want 2", len(got))
	}
	if got := l.Entries(Query{Limit: 2}); len(got) != 2 || got[1].Message != "error b" {
		t.Fatalf("Limit=2 should keep the newest two: %+v", got)
	}
	cutoff := l.Entries(Query{})[2].Time
	if got := l.Entries(Query{Since: cutoff}); len(got) != 1 || got[0].Message != "error b" {
		t.Fatalf("Since returned %+v, want only the last entry", got)
	}
}

func TestFileRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs", "dubarr.log")
	l, _ := newTestLogger(t, Options{
		FilePath:     path,
		MaxFileBytes: 128,
		MaxBackups:   2,
	})
	log := l.Logger()
	for i := 0; i < 20; i++ {
		log.Info(fmt.Sprintf("line %02d padding padding", i))
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, name := range []string{"dubarr.log", "dubarr.log.1", "dubarr.log.2"} {
		info, err := os.Stat(filepath.Join(dir, "logs", name))
		if err != nil {
			t.Fatalf("%s missing after rotation: %v", name, err)
		}
		if info.Size() > 128 {
			t.Errorf("%s is %d bytes, want <= 128", name, info.Size())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", "dubarr.log.3")); !os.IsNotExist(err) {
		t.Fatalf("dubarr.log.3 exists, MaxBackups=2 should drop it (err=%v)", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(body), "line 19") {
		t.Fatalf("current log does not contain the newest line: %q", body)
	}
}

func TestFileSinkOffByDefault(t *testing.T) {
	dir := t.TempDir()
	l, _ := newTestLogger(t, Options{})
	l.Logger().Info("hello")
	if _, err := os.Stat(filepath.Join(dir, "logs")); !os.IsNotExist(err) {
		t.Fatalf("file sink wrote without FilePath: %v", err)
	}
}

func TestRedaction(t *testing.T) {
	l, stderr := newTestLogger(t, Options{})
	log := l.Logger()
	log.Info("api call with X-Api-Key: supersecretvalue123")
	log.Warn("login failed for password=hunter2 and token=abc123def456")
	log.Error("GET https://radarr.local/api/v3/movie?apikey=deadbeef&x=1 failed")
	log.Info("Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig")
	log.Info("reading master key from /config/.master.key")
	log.Info("session=dub_session_abcdef0123456789")

	entries := l.Entries(Query{})
	if len(entries) != 6 {
		t.Fatalf("entries = %d, want 6", len(entries))
	}
	joined := ""
	for _, e := range entries {
		joined += e.Message + "\n"
	}
	for _, secret := range []string{
		"supersecretvalue123",
		"hunter2",
		"abc123def456",
		"deadbeef",
		"eyJhbGciOiJIUzI1NiJ9",
		"dub_session_abcdef0123456789",
		".master.key",
	} {
		if strings.Contains(joined, secret) {
			t.Errorf("ring leaked %q: %q", secret, joined)
		}
		if strings.Contains(stderr.String(), secret) {
			t.Errorf("stderr leaked %q: %q", secret, stderr.String())
		}
	}
	if !strings.Contains(joined, "***") {
		t.Errorf("redaction marker missing: %q", joined)
	}
	if !strings.Contains(joined, "https://radarr.local/api/v3/movie") {
		t.Errorf("redaction removed too much: %q", joined)
	}
}

// TestStderrRedactsErrorAndAnyAttrs covers finding 1: `error` and slog.Any
// values arrive as slog.KindAny, which the stderr handler used to print
// verbatim (main.go logs "error", err on every wiring failure).
func TestStderrRedactsErrorAndAnyAttrs(t *testing.T) {
	l, stderr := newTestLogger(t, Options{})
	log := l.Logger()

	log.Error("radarr probe failed", "error", errors.New("auth failed api_key=supersecret123"))
	log.Warn("keyfile unreadable", "error", fmt.Errorf("open /config/.master.key: %w", fs.ErrPermission))
	log.Info("upstream body", "response", slog.Any("response", secretPayload{Body: "access_token=s3cr3t-token"}))
	log.Info("counters", "n", 3, "ok", false)
	// With(...) becomes preformatted attrs on the stderr handler, a path
	// ReplaceAttr does not reach, so redactAttrs must cover it too.
	log.With("error", errors.New("with_attr api_key=wistleak")).Info("scoped record")

	out := stderr.String()
	for _, leaked := range []string{"supersecret123", "s3cr3t-token", ".master.key", "wistleak"} {
		if strings.Contains(out, leaked) {
			t.Errorf("stderr leaked %q: %q", leaked, out)
		}
	}
	for _, want := range []string{"api_key=***", "access_token=***", "n=3", "ok=false"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr missing %q: %q", want, out)
		}
	}
}

// secretPayload is a non-string slog.Any value (a fmt.Stringer), the shape
// that reaches the handlers as slog.KindAny.
type secretPayload struct{ Body string }

func (p secretPayload) String() string { return p.Body }

// TestRedactSensitiveKeys covers finding 2: `_`-compounded keys and encoded
// Authorization credentials used to slip past the \b-anchored key rules.
func TestRedactSensitiveKeys(t *testing.T) {
	cases := []struct{ name, in, leak string }{
		{"access_token", "GET /api?access_token=leaked1", "leaked1"},
		{"auth_token", "auth_token=leaked2", "leaked2"},
		{"secret_key", "secret_key=leaked3", "leaked3"},
		{"session_id", "session_id=leaked4", "leaked4"},
		{"db_password", "db_password=leaked5", "leaked5"},
		{"client_secret", "client_secret: leaked6", "leaked6"},
		{"pwd", "pwd=leaked7", "leaked7"},
		{"pass", "pass=leaked8", "leaked8"},
		{"credential", "credential=leaked9", "leaked9"},
		{"basic auth", "Authorization: Basic dXNlcjpwYXNzd29yZA==", "dXNlcjpwYXNzd29yZA=="},
		{"bearer auth", "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.sig", "eyJhbGciOiJIUzI1NiJ9.sig"},
		{"query password", "https://radarr.local/api?password=leaked10", "leaked10"},
		{"query session", "https://radarr.local/api?a=1&session=leaked11", "leaked11"},
	}
	for _, tc := range cases {
		got := Redact(tc.in)
		if strings.Contains(got, tc.leak) {
			t.Errorf("%s: Redact(%q) = %q, leaked the credential", tc.name, tc.in, got)
		}
		if !strings.Contains(got, "***") {
			t.Errorf("%s: Redact(%q) = %q, want a *** marker", tc.name, tc.in, got)
		}
	}
}

// TestFileSinkRedacts covers the third sink: the rotating file must receive
// the same redacted message the ring and stderr got.
func TestFileSinkRedacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dubarr.log")
	l, _ := newTestLogger(t, Options{FilePath: path})
	l.Logger().Warn("vault unlock failed client_secret=filesecret access_token=filetoken")
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	for _, leaked := range []string{"filesecret", "filetoken"} {
		if strings.Contains(string(body), leaked) {
			t.Errorf("file sink leaked %q: %q", leaked, body)
		}
	}
	if !strings.Contains(string(body), "client_secret=***") {
		t.Errorf("file sink missing the redaction marker: %q", body)
	}
}

func TestRedactKeepsOrdinaryText(t *testing.T) {
	cases := map[string]string{
		"sync job 7 finished":          "sync job 7 finished",
		"muxing /media/Film/Shrek.mkv": "muxing /media/Film/Shrek.mkv",
		"level=info component=watch":   "level=info component=watch",
		"token budget exhausted":       "token budget exhausted",
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLogfAdapter(t *testing.T) {
	l, _ := newTestLogger(t, Options{})
	l.Logf("watch")("scanner disabled: %v", "no watch dir")
	entries := l.Entries(Query{})
	if len(entries) != 1 || entries[0].Component != "watch" {
		t.Fatalf("Logf adapter did not tag the component: %+v", entries)
	}
	if !strings.Contains(entries[0].Message, "scanner disabled") {
		t.Fatalf("Logf message wrong: %q", entries[0].Message)
	}
}

func TestConcurrentWrites(t *testing.T) {
	l, _ := newTestLogger(t, Options{Capacity: 50})
	var done = make(chan struct{})
	for i := 0; i < 4; i++ {
		go func(id int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 25; j++ {
				l.Logger().Info(fmt.Sprintf("worker %d entry %d", id, j))
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	if got := len(l.Entries(Query{})); got != 50 {
		t.Fatalf("entries = %d, want the 50-entry capacity", got)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	l, _ := newTestLogger(t, Options{FilePath: filepath.Join(t.TempDir(), "dubarr.log")})
	if err := l.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
