package secrets

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMemoryKeyringSetGetDelete(t *testing.T) {
	kr := NewMemoryKeyring()
	if !kr.Available() {
		t.Fatal("a fresh memory keyring must report Available() == true")
	}
	if _, err := kr.Get(DefaultService, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
	}
	if err := kr.Set(DefaultService, "k1", "v1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := kr.Get(DefaultService, "k1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "v1" {
		t.Fatalf("Get = %q, want v1", got)
	}
	// Overwrite is allowed and visible.
	if err := kr.Set(DefaultService, "k1", "v2"); err != nil {
		t.Fatalf("Set overwrite: %v", err)
	}
	if got, _ := kr.Get(DefaultService, "k1"); got != "v2" {
		t.Fatalf("after overwrite Get = %q, want v2", got)
	}
	if err := kr.Delete(DefaultService, "k1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := kr.Get(DefaultService, "k1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete error = %v, want ErrNotFound", err)
	}
	// Deleting an absent entry is not an error.
	if err := kr.Delete(DefaultService, "k1"); err != nil {
		t.Fatalf("Delete(absent) = %v, want nil", err)
	}
}

func TestKeyringAvailabilityFlag(t *testing.T) {
	kr := NewMemoryKeyring()
	kr.SetAvailable(false)
	if kr.Available() {
		t.Fatal("Available() = true after SetAvailable(false)")
	}
	kr.SetAvailable(true)
	if !kr.Available() {
		t.Fatal("Available() = false after SetAvailable(true)")
	}
}

func TestResolveMasterKeyUsesKeyringWhenAvailable(t *testing.T) {
	dir := t.TempDir()
	kr := NewMemoryKeyring()

	key1, src, err := ResolveMasterKey(Options{DataDir: dir, Keyring: kr})
	if err != nil {
		t.Fatalf("ResolveMasterKey: %v", err)
	}
	if src != SourceKeyring {
		t.Fatalf("source = %q, want %q", src, SourceKeyring)
	}
	if len(key1) != KeySize {
		t.Fatalf("key length = %d, want %d", len(key1), KeySize)
	}

	stored, err := kr.Get(DefaultService, DefaultKeyringUser)
	if err != nil {
		t.Fatalf("keyring Get: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		t.Fatalf("keyring value is not base64: %v", err)
	}
	if !bytes.Equal(raw, key1) {
		t.Fatal("keyring does not hold the resolved master key")
	}

	// No keyfile may be written when the keyring is the source.
	if _, err := os.Stat(filepath.Join(dir, MasterKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("keyfile created although keyring was used (err=%v)", err)
	}

	key2, src2, err := ResolveMasterKey(Options{DataDir: dir, Keyring: kr})
	if err != nil {
		t.Fatalf("second ResolveMasterKey: %v", err)
	}
	if src2 != SourceKeyring || !bytes.Equal(key1, key2) {
		t.Fatal("second resolve did not reuse the keyring master key")
	}
}

func TestResolveMasterKeyFallsBackWhenKeyringUnavailable(t *testing.T) {
	dir := t.TempDir()
	kr := NewMemoryKeyring()
	kr.SetAvailable(false)

	key, src, err := ResolveMasterKey(Options{DataDir: dir, Keyring: kr})
	if err != nil {
		t.Fatalf("ResolveMasterKey: %v", err)
	}
	if src != SourceKeyfile {
		t.Fatalf("source = %q, want keyfile fallback", src)
	}
	if len(key) != KeySize {
		t.Fatalf("key length = %d", len(key))
	}
	if _, err := os.Stat(filepath.Join(dir, MasterKeyFile)); err != nil {
		t.Fatalf("fallback keyfile missing: %v", err)
	}
}

// TestOSKeyringIntegration exercises the real Secret Service backend. It skips
// (never fails) on hosts without a usable keyring, e.g. CI containers or Docker.
func TestOSKeyringIntegration(t *testing.T) {
	kr := NewOSKeyring()
	if !kr.Available() {
		t.Skip("Secret Service keyring unavailable on this host")
	}
	const (
		service = "dubarr-test"
		user    = "integration-probe"
		secret  = "probe-value-9f3a"
	)
	t.Cleanup(func() { _ = kr.Delete(service, user) })

	if err := kr.Set(service, user, secret); err != nil {
		t.Fatalf("OS keyring Set: %v", err)
	}
	got, err := kr.Get(service, user)
	if err != nil {
		t.Fatalf("OS keyring Get: %v", err)
	}
	if got != secret {
		t.Fatalf("OS keyring Get = %q, want %q", got, secret)
	}
	if err := kr.Delete(service, user); err != nil {
		t.Fatalf("OS keyring Delete: %v", err)
	}
	if _, err := kr.Get(service, user); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete error = %v, want ErrNotFound", err)
	}
}
