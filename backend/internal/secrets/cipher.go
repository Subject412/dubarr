// Package secrets implements the Dubarr secret store: AES-256-GCM blobs
// (12-byte nonce || ciphertext || tag) whose master key comes from an explicit
// keyfile, an Argon2id passphrase KEK, the OS keyring, or a 0600 keyfile under
// DATA_DIR. See masterkey.go for the resolution order and service.go for the
// SQLite-backed API used by internal/arr and internal/auth.
//
// No secret (master key, passphrase, plaintext) is ever written to a log, an
// error message or an HTTP response; Mask/Last4 exist for display, and
// KeyringAvailable reports whether /api/auth/status should advertise keyring
// support. There are no HTTP routes in this package: the arr endpoints that
// carry secrets belong to internal/arr.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
)

const (
	// KeySize is the AES-256 master key length in bytes.
	KeySize = 32
	// NonceSize is the GCM nonce prefix length: crypto/cipher.NewGCM's default
	// nonce size, and the first field of every stored blob.
	NonceSize = 12
	// TagSize is the GCM authentication tag length appended by Seal.
	TagSize = 16
	// maskPrefix hides everything but the last four characters of a secret.
	maskPrefix = "********"
)

var (
	// ErrInvalidKey reports a key that is not exactly KeySize bytes, or keyfile
	// content that does not decode to one.
	ErrInvalidKey = errors.New("secrets: invalid key")
	// ErrInvalidCiphertext reports a blob shorter than a nonce plus a tag.
	ErrInvalidCiphertext = errors.New("secrets: invalid ciphertext")
	// ErrDecryptFailed reports a GCM authentication failure: tampered data,
	// truncated data, or the wrong key.
	ErrDecryptFailed = errors.New("secrets: decryption failed")
	// ErrNotFound reports a secret (or keyring entry) that does not exist.
	ErrNotFound = errors.New("secrets: not found")
	// ErrUnavailable reports use of a Service that was not constructed by Open.
	ErrUnavailable = errors.New("secrets: service not initialised")
)

// Cipher is an AES-256-GCM primitive bound to one master key. It is safe for
// concurrent use: the AEAD is read-only and every Encrypt draws a fresh nonce.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher validates key and returns an AES-256-GCM Cipher. Only 32-byte keys
// are accepted, so a mis-sized keyfile fails loudly instead of silently using
// AES-128.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("%w: got %d bytes, want %d", ErrInvalidKey, len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidKey, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: new gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt seals plaintext with a fresh random nonce and returns
// nonce || ciphertext || tag. A nil plaintext yields a valid empty ciphertext
// rather than an error, which keeps the roundtrip contract total.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, ErrUnavailable
	}
	nonce := make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: generate nonce: %w", err)
	}
	// Seal with the nonce already in dst appends ciphertext||tag directly
	// after it, so the layout needs no second allocation or copy.
	out := make([]byte, NonceSize, NonceSize+len(plaintext)+TagSize)
	copy(out, nonce)
	return c.aead.Seal(out, nonce, plaintext, nil), nil
}

// EncryptString is Encrypt for string secrets.
func (c *Cipher) EncryptString(plaintext string) ([]byte, error) {
	return c.Encrypt([]byte(plaintext))
}

// Decrypt opens a nonce || ciphertext || tag blob. Tampering, truncation and a
// wrong key all surface as ErrDecryptFailed; nothing about the key or the
// recovered bytes is included in the error.
func (c *Cipher) Decrypt(blob []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, ErrUnavailable
	}
	if len(blob) < NonceSize+TagSize {
		return nil, fmt.Errorf("%w: %d bytes, want at least %d", ErrInvalidCiphertext, len(blob), NonceSize+TagSize)
	}
	nonce, ciphertext := blob[:NonceSize], blob[NonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: authentication failed", ErrDecryptFailed)
	}
	return plaintext, nil
}

// DecryptString is Decrypt for string secrets.
func (c *Cipher) DecryptString(blob []byte) (string, error) {
	plaintext, err := c.Decrypt(blob)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// Mask renders a secret for display: eight fixed characters plus the last four
// characters, or eight characters for secrets short enough that the suffix
// would reveal them. The plaintext is never part of the output.
func Mask(secret string) string {
	if secret == "" {
		return ""
	}
	if len(secret) <= 4 {
		return strings.Repeat("*", len(maskPrefix))
	}
	return maskPrefix + secret[len(secret)-4:]
}

// Last4 returns the display suffix stored in arr_instances.api_key_last4, or ""
// for secrets too short to have a meaningful suffix.
func Last4(secret string) string {
	if len(secret) <= 4 {
		return ""
	}
	return secret[len(secret)-4:]
}
