package secrets

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/Subject412/dubarr/backend/internal/store"
)

// Service is the encrypted secret store used by the rest of the appliance: it
// resolves the master key on Open, owns the AES-256-GCM Cipher, and reads and
// writes the existing `secrets (key TEXT PRIMARY KEY, value_enc BLOB)` table.
// It never opens the database itself, so lifecycle stays with the store.
type Service struct {
	db     *sql.DB
	cipher *Cipher
	source Source
	// keyring is retained only so KeyringAvailable can answer /api/auth/status
	// and the health payload without re-probing the platform.
	keyring KeyringBackend
	// keySourceChanged is true when this boot auto re-pinned an empty
	// collection to a different source; /api/auth/status surfaces it.
	keySourceChanged bool
}

// Open resolves the master key for st.DataDir under the pinned-source state
// machine (ADR-0008) and returns a ready Service. On first boot it writes the
// marker and sentinel in one transaction; on later boots it resolves only from
// the recorded source and refuses a mismatch when user secrets exist.
func Open(st *store.Store, opts Options) (*Service, error) {
	if st == nil || st.DB == nil {
		return nil, errors.New("secrets: store handle is required")
	}
	if opts.DataDir == "" {
		opts.DataDir = st.DataDir()
	}

	recorded, err := readKeySource(st.DB)
	if err != nil {
		return nil, err
	}
	userSecrets, err := countUserSecrets(st.DB)
	if err != nil {
		return nil, err
	}

	if recorded == "" {
		if userSecrets > 0 {
			// M1 upgrade: adopt whichever existing source authenticates.
			key, source, err := migratePinning(st.DB, opts)
			if err != nil {
				return nil, err
			}
			return NewWithKey(st.DB, key, opts.Keyring, source)
		}
		// First boot: generation is allowed exactly here.
		key, source, err := ResolveMasterKey(opts)
		if err != nil {
			return nil, err
		}
		if err := pinKeySource(st.DB, source, key); err != nil {
			return nil, err
		}
		return NewWithKey(st.DB, key, opts.Keyring, source)
	}

	key, resolveErr := resolveRecorded(opts, recorded)
	if resolveErr != nil {
		if userSecrets == 0 {
			return repinEmpty(st.DB, opts, recorded)
		}
		return nil, &SourceMismatchError{
			Recorded: recorded,
			Now:      candidateSource(opts),
			Secrets:  userSecrets,
		}
	}

	// The source matched; the sentinel must also authenticate. A wiped keyring
	// or corrupt check row is fatal, never a silent re-initialisation.
	if err := verifyKeyCheck(st.DB, key); err != nil {
		return nil, fmt.Errorf("%w (recorded source %q)", err, recorded)
	}
	return NewWithKey(st.DB, key, opts.Keyring, recorded)
}

// NewWithKey builds a Service from an already-resolved key. It bypasses the
// pinned-source state machine by design (tests, embedders, an explicit unlock
// flow); production startup goes through Open.
func NewWithKey(db *sql.DB, key []byte, keyring KeyringBackend, source Source) (*Service, error) {
	if db == nil {
		return nil, errors.New("secrets: database handle is required")
	}
	c, err := NewCipher(key)
	if err != nil {
		return nil, err
	}
	return &Service{db: db, cipher: c, source: source, keyring: keyring}, nil
}

// KeySource reports which mechanism supplied the master key.
func (s *Service) KeySource() Source {
	if s == nil {
		return ""
	}
	return s.source
}

// KeySourceChanged reports whether this boot auto re-pinned an empty secrets
// collection to a different master-key source. It is false on normal boots.
func (s *Service) KeySourceChanged() bool {
	return s != nil && s.keySourceChanged
}

// KeyringAvailable reports whether an OS keyring is usable right now, so
// /api/auth/status can advertise it. It is false when the service was built
// without a keyring backend (Docker/keyfile-only installs).
func (s *Service) KeyringAvailable() bool {
	if s == nil || s.keyring == nil {
		return false
	}
	return s.keyring.Available()
}

// Encrypt seals plaintext into a nonce || ciphertext || tag blob.
func (s *Service) Encrypt(plaintext []byte) ([]byte, error) {
	if s == nil || s.cipher == nil {
		return nil, ErrUnavailable
	}
	return s.cipher.Encrypt(plaintext)
}

// EncryptString seals a string secret.
func (s *Service) EncryptString(plaintext string) ([]byte, error) {
	if s == nil || s.cipher == nil {
		return nil, ErrUnavailable
	}
	return s.cipher.EncryptString(plaintext)
}

// Decrypt opens a blob produced by Encrypt/EncryptString.
func (s *Service) Decrypt(blob []byte) ([]byte, error) {
	if s == nil || s.cipher == nil {
		return nil, ErrUnavailable
	}
	return s.cipher.Decrypt(blob)
}

// DecryptString opens a blob into a string.
func (s *Service) DecryptString(blob []byte) (string, error) {
	if s == nil || s.cipher == nil {
		return "", ErrUnavailable
	}
	return s.cipher.DecryptString(blob)
}

// Mask renders a secret for API responses (last four characters only).
func (s *Service) Mask(secret string) string { return Mask(secret) }

// Last4 returns the display suffix persisted in arr_instances.api_key_last4.
func (s *Service) Last4(secret string) string { return Last4(secret) }

// Set encrypts plaintext and upserts it under key. The plaintext only exists in
// memory; the row holds ciphertext and the current timestamp.
func (s *Service) Set(key, plaintext string) error {
	if err := s.ready(key); err != nil {
		return err
	}
	blob, err := s.cipher.EncryptString(plaintext)
	if err != nil {
		return err
	}
	const q = `INSERT INTO secrets (key, value_enc, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET
			value_enc = excluded.value_enc,
			updated_at = CURRENT_TIMESTAMP`
	if _, err := s.db.Exec(q, key, blob); err != nil {
		return fmt.Errorf("secrets: store %q: %w", key, err)
	}
	return nil
}

// Get decrypts the secret stored under key, returning ErrNotFound when absent.
func (s *Service) Get(key string) (string, error) {
	if err := s.ready(key); err != nil {
		return "", err
	}
	var blob []byte
	const q = `SELECT value_enc FROM secrets WHERE key = ?`
	if err := s.db.QueryRow(q, key).Scan(&blob); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: %q", ErrNotFound, key)
		}
		return "", fmt.Errorf("secrets: load %q: %w", key, err)
	}
	plaintext, err := s.cipher.DecryptString(blob)
	if err != nil {
		return "", fmt.Errorf("secrets: decrypt %q: %w", key, err)
	}
	return plaintext, nil
}

// Delete removes the secret stored under key. Deleting an absent key succeeds,
// so callers can clean up unconditionally.
func (s *Service) Delete(key string) error {
	if err := s.ready(key); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM secrets WHERE key = ?`, key); err != nil {
		return fmt.Errorf("secrets: delete %q: %w", key, err)
	}
	return nil
}

// ready validates the receiver and the key name so an empty key can never be a
// row that silently collides with another empty-key write.
func (s *Service) ready(key string) error {
	if s == nil || s.db == nil || s.cipher == nil {
		return ErrUnavailable
	}
	if key == "" {
		return errors.New("secrets: key must not be empty")
	}
	return nil
}
