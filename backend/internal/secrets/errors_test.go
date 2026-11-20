package secrets

import (
	"errors"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/store"
)

// failingKeyring is available but every operation fails, modelling a locked or
// broken Secret Service. Resolution must surface the error, not silently fall
// back to a fresh keyfile that would strand already-encrypted secrets.
type failingKeyring struct{ err error }

func (f failingKeyring) Available() bool { return true }
func (f failingKeyring) Get(string, string) (string, error) {
	return "", f.err
}
func (f failingKeyring) Set(string, string, string) error { return f.err }
func (f failingKeyring) Delete(string, string) error      { return f.err }

func TestResolveMasterKeySurfacesKeyringErrors(t *testing.T) {
	boom := errors.New("dbus: connection closed")
	_, _, err := ResolveMasterKey(Options{DataDir: t.TempDir(), Keyring: failingKeyring{err: boom}})
	if !errors.Is(err, boom) {
		t.Fatalf("ResolveMasterKey error = %v, want the keyring failure", err)
	}
}

func TestResolveMasterKeyRejectsCorruptKeyringValue(t *testing.T) {
	kr := NewMemoryKeyring()
	if err := kr.Set(DefaultService, DefaultKeyringUser, "not-base64!"); err != nil {
		t.Fatalf("seed keyring: %v", err)
	}
	_, _, err := ResolveMasterKey(Options{DataDir: t.TempDir(), Keyring: kr})
	if !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("error = %v, want ErrInvalidKey", err)
	}
}

func TestServiceUninitialisedReturnsErrUnavailable(t *testing.T) {
	var svc *Service
	if _, err := svc.Encrypt([]byte("x")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Encrypt error = %v, want ErrUnavailable", err)
	}
	if _, err := svc.Decrypt([]byte("x")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Decrypt error = %v, want ErrUnavailable", err)
	}
	if err := svc.Set("k", "v"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Set error = %v, want ErrUnavailable", err)
	}
	if _, err := svc.Get("k"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Get error = %v, want ErrUnavailable", err)
	}
	if err := svc.Delete("k"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Delete error = %v, want ErrUnavailable", err)
	}
	if svc.KeyringAvailable() {
		t.Fatal("nil service reports a keyring")
	}
	if svc.KeySource() != "" {
		t.Fatal("nil service reports a key source")
	}
}

func TestServiceKeyringAvailableReflectsBackend(t *testing.T) {
	kr := NewMemoryKeyring()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	plain, err := Open(st, Options{})
	if err != nil {
		t.Fatalf("Open without keyring: %v", err)
	}
	if plain.KeyringAvailable() {
		t.Fatal("KeyringAvailable = true with a nil backend")
	}

	svc, err := Open(st, Options{DataDir: st.DataDir(), Keyring: kr})
	if err != nil {
		t.Fatalf("Open with keyring: %v", err)
	}
	if !svc.KeyringAvailable() {
		t.Fatal("KeyringAvailable = false with an available backend")
	}
	kr.SetAvailable(false)
	if svc.KeyringAvailable() {
		t.Fatal("KeyringAvailable = true after the backend went away")
	}
}

func TestNewWithKeyRejectsBadInput(t *testing.T) {
	if _, err := NewWithKey(nil, testKey(t), nil, SourceKeyfile); err == nil {
		t.Fatal("NewWithKey accepted a nil database")
	}
	svc, st := newTestService(t, t.TempDir())
	defer st.Close()
	if svc == nil {
		t.Fatal("newTestService returned nil")
	}
	if _, err := NewWithKey(st.DB, make([]byte, 16), nil, SourceKeyfile); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("NewWithKey(16-byte key) error = %v, want ErrInvalidKey", err)
	}
}
