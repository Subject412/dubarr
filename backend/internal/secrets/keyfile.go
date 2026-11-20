package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	// MasterKeyFile is the default keyfile name inside DATA_DIR.
	MasterKeyFile = ".master.key"
	// PassphraseSaltFile is the Argon2id salt file, stored next to the master
	// keyfile with the same 0600 permissions.
	PassphraseSaltFile = ".master.salt"

	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
	saltLen              = 16

	// Argon2id parameters for the passphrase KEK. They mirror internal/auth's
	// password hashing cost and stay well inside default container memory.
	argonTime    uint32 = 1
	argonMemory  uint32 = 64 * 1024
	argonThreads uint8  = 4
)

// DerivePassphraseKey stretches a passphrase into a 32-byte master key with
// Argon2id. The salt is random per DATA_DIR and persisted, so the same
// passphrase always yields the same key; a wrong passphrase yields a different
// key and every Decrypt then fails authentication.
func DerivePassphraseKey(passphrase string, salt []byte) []byte {
	return argon2.IDKey([]byte(passphrase), salt, argonTime, argonMemory, argonThreads, KeySize)
}

// encodeKey renders key material for text storage (keyfile, keyring value).
func encodeKey(key []byte) string {
	return base64.StdEncoding.EncodeToString(key)
}

// decodeKey parses key material without ever echoing it in an error.
func decodeKey(encoded string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("%w: keyfile is not valid base64", ErrInvalidKey)
	}
	if len(raw) != KeySize {
		return nil, fmt.Errorf("%w: decoded %d bytes, want %d", ErrInvalidKey, len(raw), KeySize)
	}
	return raw, nil
}

// ensureDir creates dir with 0700 and tightens it if it already exists with
// wider permissions, so a copied DATA_DIR cannot leave the keyfile exposed.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("secrets: create data dir: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("secrets: stat data dir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("secrets: data dir %q is not a directory", dir)
	}
	if info.Mode().Perm() != dirPerm {
		if err := os.Chmod(dir, dirPerm); err != nil {
			return fmt.Errorf("secrets: tighten data dir permissions: %w", err)
		}
	}
	return nil
}

// readKeyfile loads and validates an existing keyfile, tightening its
// permissions first so a pre-existing 0644 file is repaired on every boot.
func readKeyfile(path string) ([]byte, error) {
	if err := os.Chmod(path, filePerm); err != nil {
		return nil, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodeKey(string(content))
}

// loadOrCreateKeyfile returns the key stored at path, generating and persisting
// a fresh CSPRNG key with O_EXCL 0600 when the file does not exist.
func loadOrCreateKeyfile(path string) ([]byte, error) {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	key, err := readKeyfile(path)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("secrets: read keyfile: %w", err)
	}

	key = make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("secrets: generate master key: %w", err)
	}
	// O_EXCL guarantees two concurrent first boots cannot both install a key
	// and silently diverge; the loser re-reads the winner's key.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return readKeyfile(path)
		}
		return nil, fmt.Errorf("secrets: create keyfile: %w", err)
	}
	if _, err := file.WriteString(encodeKey(key) + "\n"); err != nil {
		file.Close()
		os.Remove(path)
		return nil, fmt.Errorf("secrets: write keyfile: %w", err)
	}
	if err := file.Chmod(filePerm); err != nil {
		file.Close()
		os.Remove(path)
		return nil, fmt.Errorf("secrets: chmod keyfile: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("secrets: close keyfile: %w", err)
	}
	return key, nil
}

// loadOrCreateSalt returns the persisted Argon2id salt, creating it 0600 the
// first time a passphrase is configured. The salt is not secret, but it lives
// under the same permissions so DATA_DIR has one uniform access policy.
func loadOrCreateSalt(path string) ([]byte, error) {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	salt, err := readSalt(path)
	if err == nil {
		return salt, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("secrets: read passphrase salt: %w", err)
	}

	salt = make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("secrets: generate passphrase salt: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return readSalt(path)
		}
		return nil, fmt.Errorf("secrets: create passphrase salt: %w", err)
	}
	if _, err := file.WriteString(base64.StdEncoding.EncodeToString(salt) + "\n"); err != nil {
		file.Close()
		os.Remove(path)
		return nil, fmt.Errorf("secrets: write passphrase salt: %w", err)
	}
	if err := file.Chmod(filePerm); err != nil {
		file.Close()
		os.Remove(path)
		return nil, fmt.Errorf("secrets: chmod passphrase salt: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("secrets: close passphrase salt: %w", err)
	}
	return salt, nil
}

// readSalt loads the persisted Argon2id salt, tightening permissions first.
func readSalt(path string) ([]byte, error) {
	if err := os.Chmod(path, filePerm); err != nil {
		return nil, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(content)))
	if err != nil || len(raw) != saltLen {
		return nil, fmt.Errorf("%w: passphrase salt is malformed", ErrInvalidKey)
	}
	return raw, nil
}
