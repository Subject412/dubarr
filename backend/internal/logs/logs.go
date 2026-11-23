// Package logs owns Dubarr's central structured logger: a stderr handler for
// `docker logs`, a bounded in-memory ring for the GUI Logs tab and an optional
// rotating file under DATA_DIR.
//
// Every sink is redacted at the boundary (see Redact): API keys, passwords,
// session/token values and secret key paths never reach stderr, the ring or
// the file. Levels are controlled by --log-level at boot and by
// PUT /api/logs/level at runtime.
package logs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultCapacity is the settled ring size (1000 entries).
	DefaultCapacity = 1000
	// DefaultMaxFileBytes is the settled rotation threshold (10 MB).
	DefaultMaxFileBytes int64 = 10 * 1024 * 1024
	// DefaultMaxBackups is the number of rotated files kept beside the current.
	DefaultMaxBackups = 3
)

// Entry is one log line as exposed to the GUI and the API.
type Entry struct {
	Time      time.Time `json:"ts"`
	Level     string    `json:"level"`
	Component string    `json:"component,omitempty"`
	Message   string    `json:"message"`
	JobID     string    `json:"job_id,omitempty"`
	MediaID   string    `json:"media_id,omitempty"`
}

// Query filters ring entries. The zero value returns everything currently kept.
type Query struct {
	// MinLevel keeps entries at or above this severity (debug|info|warn|error).
	MinLevel string
	// Component keeps entries whose component matches exactly.
	Component string
	// Limit keeps the newest N entries after filtering. 0 means no limit.
	Limit int
	// Since keeps entries strictly newer than this instant.
	Since time.Time
}

// Options configures New. Every field has a production default.
type Options struct {
	// Level is the minimum severity (debug|info|warn|error). Empty means info;
	// an unknown value also falls back to info.
	Level string
	// Capacity is the ring size. 0 means DefaultCapacity.
	Capacity int
	// FilePath enables the rotating file sink. Empty keeps it OFF.
	FilePath string
	// MaxFileBytes is the rotation threshold. 0 means DefaultMaxFileBytes.
	MaxFileBytes int64
	// MaxBackups is how many rotated files to keep. 0 means DefaultMaxBackups.
	MaxBackups int
	// Stderr is the handler output. Nil means os.Stderr.
	Stderr io.Writer
}

// Logger bundles the level switch, the ring and the optional file sink.
// Logger is safe for concurrent use.
type Logger struct {
	level  *slog.LevelVar
	ring   *ring
	file   *rotator
	logger *slog.Logger

	fileMu    sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

// New builds the logger and its sinks. An unreadable file sink is returned as
// an error; the caller may then boot without a file rather than fail.
func New(opts Options) (*Logger, error) {
	capacity := opts.Capacity
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	lvl, _ := ParseLevel(opts.Level)
	var level slog.LevelVar
	level.Set(lvl)

	l := &Logger{level: &level, ring: newRing(capacity)}

	base := slog.NewTextHandler(stderr, &slog.HandlerOptions{
		Level: &level,
		// Belt and braces: redaction already happens in our handler, but a
		// record forwarded by another handler still cannot echo a secret.
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Value.Kind() == slog.KindString {
				a.Value = slog.StringValue(Redact(a.Value.String()))
			}
			return a
		},
	})
	l.logger = slog.New(&handler{base: base, sink: l})

	if strings.TrimSpace(opts.FilePath) != "" {
		rot, err := newRotator(opts.FilePath, opts.MaxFileBytes, opts.MaxBackups)
		if err != nil {
			return nil, fmt.Errorf("logs: open file sink: %w", err)
		}
		l.file = rot
	}
	return l, nil
}

// Logger returns the slog.Logger callers should use.
func (l *Logger) Logger() *slog.Logger { return l.logger }

// Level reports the active minimum severity as a name.
func (l *Logger) Level() string { return levelName(l.level.Level()) }

// SetLevel changes the runtime threshold. Unknown names are rejected.
func (l *Logger) SetLevel(name string) error {
	lvl, ok := ParseLevel(name)
	if !ok {
		return fmt.Errorf("logs: unknown level %q (want debug, info, warn or error)", name)
	}
	l.level.Set(lvl)
	return nil
}

// Logf returns a printf-style adapter tagged with component, for the workers
// that still take a `Logf func(string, ...any)` seam.
func (l *Logger) Logf(component string) func(format string, args ...any) {
	return func(format string, args ...any) {
		l.logger.Info(fmt.Sprintf(format, args...), "component", component)
	}
}

// Entries returns a filtered copy of the ring, oldest first.
func (l *Logger) Entries(q Query) []Entry {
	all := l.ring.snapshot()

	minLevel := slog.LevelDebug
	if strings.TrimSpace(q.MinLevel) != "" {
		if lvl, ok := ParseLevel(q.MinLevel); ok {
			minLevel = lvl
		}
	}
	filtered := make([]Entry, 0, len(all))
	for _, e := range all {
		if levelValue(e.Level) < minLevel {
			continue
		}
		if q.Component != "" && e.Component != q.Component {
			continue
		}
		if !q.Since.IsZero() && !e.Time.After(q.Since) {
			continue
		}
		filtered = append(filtered, e)
	}
	if n := q.Limit; n > 0 && len(filtered) > n {
		filtered = filtered[len(filtered)-n:]
	}
	return filtered
}

// Close releases the file sink. It is safe to call more than once.
func (l *Logger) Close() error {
	l.closeOnce.Do(func() {
		l.fileMu.Lock()
		defer l.fileMu.Unlock()
		if l.file != nil {
			l.closeErr = l.file.Close()
		}
	})
	return l.closeErr
}

// append records one entry in the ring (and the file when enabled).
func (l *Logger) append(e Entry) {
	e.Message = Redact(e.Message)
	e.Component = Redact(e.Component)
	e.JobID = Redact(e.JobID)
	e.MediaID = Redact(e.MediaID)
	l.ring.add(e)

	l.fileMu.Lock()
	defer l.fileMu.Unlock()
	if l.file == nil {
		return
	}
	var b strings.Builder
	b.WriteString(e.Time.UTC().Format(time.RFC3339Nano))
	b.WriteString(" level=")
	b.WriteString(e.Level)
	if e.Component != "" {
		b.WriteString(" component=")
		b.WriteString(e.Component)
	}
	if e.JobID != "" {
		b.WriteString(" job_id=")
		b.WriteString(e.JobID)
	}
	if e.MediaID != "" {
		b.WriteString(" media_id=")
		b.WriteString(e.MediaID)
	}
	b.WriteString(" msg=")
	b.WriteString(e.Message)
	b.WriteString("\n")
	if _, err := l.file.Write([]byte(b.String())); err != nil {
		// A broken file sink must never break logging to stderr or the ring.
		_ = err
	}
}

// handler is the slog.Handler that mirrors every accepted record into the ring
// and the file, redacts it, and forwards the redacted record to the stderr
// handler.
type handler struct {
	base  slog.Handler
	sink  *Logger
	attrs []slog.Attr
}

func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	e := Entry{
		Time:    r.Time,
		Level:   levelName(r.Level),
		Message: Redact(r.Message),
	}

	redacted := slog.NewRecord(r.Time, r.Level, e.Message, r.PC)
	collect := func(a slog.Attr) bool {
		a = redactAttr(a)
		switch a.Key {
		case "component":
			e.Component = a.Value.String()
		case "job_id":
			e.JobID = a.Value.String()
		case "media_id":
			e.MediaID = a.Value.String()
		}
		redacted.AddAttrs(a)
		return true
	}
	for _, a := range h.attrs {
		collect(a)
	}
	r.Attrs(collect)

	h.sink.append(e)
	return h.base.Handle(ctx, redacted)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	next.base = h.base.WithAttrs(redactAttrs(attrs))
	return &next
}

func (h *handler) WithGroup(name string) slog.Handler {
	next := *h
	next.base = h.base.WithGroup(name)
	return &next
}

func redactAttrs(attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, redactAttr(a))
	}
	return out
}

func redactAttr(a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(Redact(a.Value.String()))
	case slog.KindGroup:
		a.Value = slog.GroupValue(redactAttrs(a.Value.Group())...)
	}
	return a
}

// ParseLevel maps a level name to a slog.Level. Unknown names return
// (slog.LevelInfo, false).
func ParseLevel(name string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "info":
		return slog.LevelInfo, true
	case "debug":
		return slog.LevelDebug, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

func levelName(l slog.Level) string {
	switch {
	case l <= slog.LevelDebug:
		return "debug"
	case l < slog.LevelWarn:
		return "info"
	case l < slog.LevelError:
		return "warn"
	default:
		return "error"
	}
}

func levelValue(name string) slog.Level {
	lvl, _ := ParseLevel(name)
	return lvl
}

// ring is a fixed-capacity FIFO of entries.
type ring struct {
	mu       sync.Mutex
	entries  []Entry
	capacity int
}

func newRing(capacity int) *ring {
	return &ring{entries: make([]Entry, 0, capacity), capacity: capacity}
}

func (r *ring) add(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) == r.capacity {
		copy(r.entries, r.entries[1:])
		r.entries[len(r.entries)-1] = e
		return
	}
	r.entries = append(r.entries, e)
}

func (r *ring) snapshot() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Entry, len(r.entries))
	copy(out, r.entries)
	return out
}
