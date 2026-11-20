package secrets

import (
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
)

// Source records which mechanism supplied the master key, for logs and for
// /api/auth/status. It never contains key material.
type Source string

const (
	// SourceExplicitKeyfile is a keyfile path supplied by the operator.
	SourceExplicitKeyfile Source = "explicit-keyfile"
	// SourcePassphrase is an Argon2id key derived from an unlock passphrase.
	SourcePassphrase Source = "passphrase"
	// SourceKeyring is the OS keyring (native desktop installs).
	SourceKeyring Source = "keyring"
	// SourceKeyfile is DATA_DIR/.master.key (Docker, headless, keyring-less).
	SourceKeyfile Source = "keyfile"
)

// Options configures master-key resolution. The resolution order is:
//
//  1. KeyfilePath, when non-empty (created 0600 if absent);
//  2. Passphrase, when non-empty (Argon2id with a persisted random salt);
//  3. Keyring, when non-nil and Available();
//  4. DATA_DIR/.master.key (created 0600 on first boot).
//
// A nil Keyring disables step 3, which is what tests and the keyfile-only
// Docker path want; use DefaultOptions for the native desktop behaviour.
type Options struct {
	// DataDir is DATA_DIR, owned 0700. Required.
	DataDir string
	// KeyfilePath is an explicit operator-supplied keyfile. It wins over every
	// other source.
	KeyfilePath string
	// Passphrase enables unlock-passphrase mode. The passphrase itself is never
	// persisted; only its random salt is.
	Passphrase string
	// Keyring is the OS secret store backend. nil disables keyring resolution.
	Keyring KeyringBackend
	// KeyringService overrides DefaultService.
	KeyringService string
	// KeyringUser overrides DefaultKeyringUser.
	KeyringUser string
}

// DefaultOptions returns production options for dataDir: the OS keyring is
// tried first and the 0600 keyfile is used when no Secret Service is reachable
// (Docker, headless servers, locked keyrings). Callers that need a different
// source (passphrase, explicit keyfile) override the matching field.
func DefaultOptions(dataDir string) Options {
	return Options{DataDir: dataDir, Keyring: NewOSKeyring()}
}

func (o Options) serviceName() string {
	if o.KeyringService != "" {
		return o.KeyringService
	}
	return DefaultService
}

func (o Options) keyringUser() string {
	if o.KeyringUser != "" {
		return o.KeyringUser
	}
	return DefaultKeyringUser
}

// ResolveMasterKey returns the 32-byte master key and the source it came from,
// generating and persisting one on first boot. The key is never logged.
func ResolveMasterKey(opts Options) ([]byte, Source, error) {
	if opts.DataDir == "" {
		return nil, "", errors.New("secrets: data dir must not be empty")
	}
	if err := ensureDir(opts.DataDir); err != nil {
		return nil, "", err
	}

	if opts.KeyfilePath != "" {
		key, err := loadOrCreateKeyfile(opts.KeyfilePath)
		if err != nil {
			return nil, "", err
		}
		return key, SourceExplicitKeyfile, nil
	}

	if opts.Passphrase != "" {
		salt, err := loadOrCreateSalt(filepath.Join(opts.DataDir, PassphraseSaltFile))
		if err != nil {
			return nil, "", err
		}
		return DerivePassphraseKey(opts.Passphrase, salt), SourcePassphrase, nil
	}

	if opts.Keyring != nil && opts.Keyring.Available() {
		key, err := resolveFromKeyring(opts)
		if err != nil {
			return nil, "", err
		}
		return key, SourceKeyring, nil
	}

	key, err := loadOrCreateKeyfile(filepath.Join(opts.DataDir, MasterKeyFile))
	if err != nil {
		return nil, "", err
	}
	return key, SourceKeyfile, nil
}

// resolveFromKeyring reads the master key from the keyring, generating and
// storing one on first run. A keyring that is present but unusable is an error
// rather than a silent fallback: falling back would strand every secret already
// encrypted under the keyring key.
func resolveFromKeyring(opts Options) ([]byte, error) {
	service, user := opts.serviceName(), opts.keyringUser()

	encoded, err := opts.Keyring.Get(service, user)
	switch {
	case err == nil:
		key, decodeErr := decodeKey(encoded)
		if decodeErr != nil {
			return nil, fmt.Errorf("secrets: keyring master key is corrupt: %w", decodeErr)
		}
		return key, nil
	case !errors.Is(err, ErrNotFound):
		return nil, err
	}

	key := make([]byte, KeySize)
	if _, randErr := rand.Read(key); randErr != nil {
		return nil, fmt.Errorf("secrets: generate master key: %w", randErr)
	}
	if err := opts.Keyring.Set(service, user, encodeKey(key)); err != nil {
		return nil, err
	}
	return key, nil
}
