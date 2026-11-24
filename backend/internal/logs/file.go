package logs

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// The redaction rules run in order: authorization schemes first (so the
// keyword rule cannot swallow just the scheme word and leave the credential),
// then key=value pairs, URL query secrets and finally paths to key material.
// Every match becomes "***".
var (
	// Authorization: Basic <base64(user:pass)> / Bearer <token>. The scheme
	// word alone is not a secret, so the credential after it must go too. A
	// scheme is only treated as such when a >=8-char token follows it.
	reBearer = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	// Authorization[:=] [scheme] <credential>: keeps the header name (and the
	// scheme, when present) so the line stays readable, drops the credential.
	// Runs after reBearer so a bare scheme can never survive unredacted.
	reAuth = regexp.MustCompile(`(?i)\b(authorization)([ 	]*[:=][ 	]*)((?:basic|digest|bearer|negotiate|ntlm|token)[ 	]+)?("[^"]*"|'[^']*'|[^\s,;&]+)`)
	// key=value / key: value forms (api keys, passwords, tokens, sessions).
	// There is deliberately no \b anchor: `_` is a word character, so
	// access_token, secret_key, session_id and client_secret never matched.
	// The keyword only has to be part of the identifier before the separator.
	reKeyValue = regexp.MustCompile(`(?i)([A-Za-z0-9_.-]*(?:x-api-key|api[_-]?key|apikey|password|passwd|pwd|pass|secret|token|session|credential)[A-Za-z0-9_.-]*)([ 	]*[:=][ 	]*)(("[^"]*"|'[^']*'|[^\s,;&]+))`)
	// Secret material in URL query strings.
	reQuery = regexp.MustCompile(`(?i)([?&](?:apikey|api_key|access_token|token|key|password|secret|session|credential)=)[^&\s]+`)
	// Paths to key material (DATA_DIR/.master.key, .master.salt, keyfiles).
	reSecretPath = regexp.MustCompile(`(?i)[^\s"']*/[^\s"']*(?:\.master\.key|\.master\.salt|keyfile)[^\s"']*`)
)

// Redact strips credentials and secret paths from one log string. It is the
// single sink-boundary guard used for messages, attrs and error text.
func Redact(s string) string {
	if s == "" {
		return s
	}
	s = reBearer.ReplaceAllStringFunc(s, func(match string) string {
		m := reBearer.FindStringSubmatch(match)
		return m[1] + " ***"
	})
	s = reAuth.ReplaceAllStringFunc(s, func(match string) string {
		m := reAuth.FindStringSubmatch(match)
		return m[1] + m[2] + m[3] + "***"
	})
	s = reKeyValue.ReplaceAllStringFunc(s, func(match string) string {
		m := reKeyValue.FindStringSubmatch(match)
		return m[1] + m[2] + "***"
	})
	s = reQuery.ReplaceAllStringFunc(s, func(match string) string {
		m := reQuery.FindStringSubmatch(match)
		return m[1] + "***"
	})
	s = reSecretPath.ReplaceAllString(s, "***")
	return s
}

// rotator is a size-bounded append-only file writer with numbered backups.
//
//	dubarr.log      current
//	dubarr.log.1    previous
//	dubarr.log.N    oldest retained (N == maxBackups)
type rotator struct {
	mu         sync.Mutex
	path       string
	maxBytes   int64
	maxBackups int
	file       *os.File
	size       int64
}

func newRotator(path string, maxBytes int64, maxBackups int) (*rotator, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFileBytes
	}
	if maxBackups <= 0 {
		maxBackups = DefaultMaxBackups
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat log file: %w", err)
	}
	return &rotator{path: path, maxBytes: maxBytes, maxBackups: maxBackups, file: f, size: info.Size()}, nil
}

// Write appends p, rotating first when the current file would exceed maxBytes.
func (r *rotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return 0, os.ErrClosed
	}
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := r.file.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rotator) rotateLocked() error {
	if err := r.file.Close(); err != nil {
		return fmt.Errorf("close for rotation: %w", err)
	}
	// Drop the oldest, then shift every backup one slot down.
	_ = os.Remove(fmt.Sprintf("%s.%d", r.path, r.maxBackups))
	for i := r.maxBackups - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", r.path, i)
		to := fmt.Sprintf("%s.%d", r.path, i+1)
		if _, err := os.Stat(from); err == nil {
			if renameErr := os.Rename(from, to); renameErr != nil {
				return fmt.Errorf("rotate %s: %w", from, renameErr)
			}
		}
	}
	if err := os.Rename(r.path, r.path+".1"); err != nil {
		return fmt.Errorf("rotate current log: %w", err)
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("reopen log file: %w", err)
	}
	r.file = f
	r.size = 0
	return nil
}

// Close closes the current file.
func (r *rotator) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}
