package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	// sessionCookieName is the cookie carrying the signed session value.
	sessionCookieName = "dubarr_session"
	// sessionKeySetting is the settings-table key holding the hex signing key.
	// T5's sanitized settings export MUST skip this key.
	sessionKeySetting = "auth.session_key"

	sessionKeyLen     = 32
	sessionPayloadLen = 32
)

// errNoSessionKey reports that no signing key exists yet, i.e. setup has not
// run. Requests are simply unauthenticated in that state.
var errNoSessionKey = errors.New("auth: session signing key not initialised")

// randomBytes returns n bytes from the operating system CSPRNG.
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("auth: read random bytes: %w", err)
	}
	return b, nil
}

// hashToken is the single digest used for both session cookie values and API
// tokens: the hex-encoded SHA-256 of the bearer secret. The plaintext is never
// stored.
func hashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// ensureSessionKey returns the persisted 32-byte session signing key,
// generating and storing it on first use. It is safe to call repeatedly and
// concurrently: the first insert wins (INSERT OR IGNORE) and every caller reads
// the stored value back.
func ensureSessionKey(ctx context.Context, db *sql.DB) ([]byte, error) {
	key, err := loadSessionKey(ctx, db)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, errNoSessionKey) {
		return nil, err
	}

	fresh, err := randomBytes(sessionKeyLen)
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`,
		sessionKeySetting, hex.EncodeToString(fresh),
	); err != nil {
		return nil, fmt.Errorf("auth: store session key: %w", err)
	}
	// Another writer may have won the race; read back whatever is persisted.
	return loadSessionKey(ctx, db)
}

// loadSessionKey reads the persisted signing key without creating one.
func loadSessionKey(ctx context.Context, db *sql.DB) ([]byte, error) {
	var stored string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, sessionKeySetting).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNoSessionKey
	}
	if err != nil {
		return nil, fmt.Errorf("auth: read session key: %w", err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(stored))
	if err != nil {
		return nil, fmt.Errorf("auth: decode session key: %w", err)
	}
	if len(key) != sessionKeyLen {
		return nil, fmt.Errorf("auth: session key has %d bytes, want %d", len(key), sessionKeyLen)
	}
	return key, nil
}

// encodeSessionValue builds "<b64url payload>.<b64url HMAC-SHA256(payload)>".
func encodeSessionValue(key, payload []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// decodeSessionValue verifies the HMAC in constant time and returns the opaque
// payload. A malformed value and a forged signature are indistinguishable.
func decodeSessionValue(key []byte, value string) ([]byte, bool) {
	encPayload, encMAC, ok := strings.Cut(value, ".")
	if !ok || encPayload == "" || encMAC == "" {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encPayload)
	if err != nil || len(payload) != sessionPayloadLen {
		return nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(encMAC)
	if err != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), sig) {
		return nil, false
	}
	return payload, true
}
