package secrets

import (
	"errors"
	"fmt"
	"sync"

	"github.com/zalando/go-keyring"
)

const (
	// DefaultService is the keyring service name Dubarr registers secrets under.
	DefaultService = "dubarr"
	// DefaultKeyringUser is the entry holding the master key.
	DefaultKeyringUser = "master-key"
	// availabilityProbeUser is looked up (never written) by Available: an
	// ErrNotFound answer proves the Secret Service is reachable.
	availabilityProbeUser = "availability-probe"
)

// KeyringBackend hides the platform secret store so tests never need a live
// D-Bus session. Get must return an error wrapping ErrNotFound when the entry
// is absent; Delete of an absent entry is not an error.
type KeyringBackend interface {
	Get(service, user string) (string, error)
	Set(service, user, secret string) error
	Delete(service, user string) error
	// Available reports whether the backend can be used right now. A locked or
	// missing Secret Service must report false so resolution falls back to the
	// keyfile instead of failing startup.
	Available() bool
}

// OSKeyring is the production backend: Secret Service on Linux via D-Bus,
// Keychain on macOS and Credential Manager on Windows (go-keyring is pure Go,
// so CGO_ENABLED=0 builds keep working).
type OSKeyring struct{}

// NewOSKeyring returns the OS-backed keyring backend.
func NewOSKeyring() *OSKeyring { return &OSKeyring{} }

// Get reads a secret from the OS keyring.
func (k *OSKeyring) Get(service, user string) (string, error) {
	secret, err := keyring.Get(service, user)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("secrets: keyring get: %w", err)
	}
	return secret, nil
}

// Set stores a secret in the OS keyring, overwriting any previous value.
func (k *OSKeyring) Set(service, user, secret string) error {
	if err := keyring.Set(service, user, secret); err != nil {
		return fmt.Errorf("secrets: keyring set: %w", err)
	}
	return nil
}

// Delete removes a secret from the OS keyring; a missing entry is not an error.
func (k *OSKeyring) Delete(service, user string) error {
	err := keyring.Delete(service, user)
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return fmt.Errorf("secrets: keyring delete: %w", err)
}

// Available probes the Secret Service. Docker containers have no D-Bus session,
// so this returns false there and resolution proceeds to the 0600 keyfile.
// The result is not cached: a user can unlock the keyring between calls.
func (k *OSKeyring) Available() bool {
	_, err := keyring.Get(DefaultService, availabilityProbeUser)
	return err == nil || errors.Is(err, keyring.ErrNotFound)
}

// MemoryKeyring is an in-memory KeyringBackend for deterministic tests and for
// hosts where the keyring must be simulated. The zero value is not usable; use
// NewMemoryKeyring.
type MemoryKeyring struct {
	mu        sync.RWMutex
	entries   map[string]string
	available bool
}

// NewMemoryKeyring returns an empty, available in-memory keyring.
func NewMemoryKeyring() *MemoryKeyring {
	return &MemoryKeyring{entries: make(map[string]string), available: true}
}

// SetAvailable flips the reported availability, which lets tests exercise the
// keyfile fallback without touching the real Secret Service.
func (m *MemoryKeyring) SetAvailable(available bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.available = available
}

// Available reports the configured availability flag.
func (m *MemoryKeyring) Available() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.available
}

// Get returns the stored secret or ErrNotFound.
func (m *MemoryKeyring) Get(service, user string) (string, error) {
	if m == nil {
		return "", ErrNotFound
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	secret, ok := m.entries[service+"\x00"+user]
	if !ok {
		return "", ErrNotFound
	}
	return secret, nil
}

// Set stores a secret, overwriting any previous value.
func (m *MemoryKeyring) Set(service, user, secret string) error {
	if m == nil {
		return fmt.Errorf("%w: nil memory keyring", ErrUnavailable)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = make(map[string]string)
	}
	m.entries[service+"\x00"+user] = secret
	return nil
}

// Delete removes a secret; a missing entry is not an error.
func (m *MemoryKeyring) Delete(service, user string) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, service+"\x00"+user)
	return nil
}
