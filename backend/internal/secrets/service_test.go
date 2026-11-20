package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/store"
)

// newTestService opens a real store in a throwaway data dir and builds the
// secrets service on top of it with a deterministic in-memory keyring.
func newTestService(t *testing.T, dir string) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	svc, err := Open(st, Options{Keyring: NewMemoryKeyring()})
	if err != nil {
		st.Close()
		t.Fatalf("secrets.Open: %v", err)
	}
	return svc, st
}

func TestServiceSetGetDelete(t *testing.T) {
	dir := t.TempDir()
	svc, st := newTestService(t, dir)
	defer st.Close()

	const (
		key       = "radarr.api_key"
		plaintext = "0123456789abcdef0123456789abcdef"
	)
	if err := svc.Set(key, plaintext); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := svc.Get(key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != plaintext {
		t.Fatalf("Get = %q, want %q", got, plaintext)
	}

	// The row must be ciphertext: no plaintext substring may survive in the blob.
	var blob []byte
	if err := st.DB.QueryRow(`SELECT value_enc FROM secrets WHERE key = ?`, key).Scan(&blob); err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if len(blob) != NonceSize+len(plaintext)+TagSize {
		t.Fatalf("blob length = %d, want %d", len(blob), NonceSize+len(plaintext)+TagSize)
	}
	if bytes.Contains(blob, []byte(plaintext)) {
		t.Fatal("plaintext is visible in the stored blob")
	}

	// Rotating the value is an upsert, not a duplicate-key failure.
	if err := svc.Set(key, "rotated-secret-value"); err != nil {
		t.Fatalf("Set (rotate): %v", err)
	}
	var count int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM secrets WHERE key = ?`, key).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("row count = %d, want 1", count)
	}
	if got, err := svc.Get(key); err != nil || got != "rotated-secret-value" {
		t.Fatalf("Get after rotate = %q, %v", got, err)
	}

	if err := svc.Delete(key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete error = %v, want ErrNotFound", err)
	}
	// Deleting again is idempotent.
	if err := svc.Delete(key); err != nil {
		t.Fatalf("Delete(absent) = %v, want nil", err)
	}
}

func TestServiceGetMissing(t *testing.T) {
	svc, st := newTestService(t, t.TempDir())
	defer st.Close()
	if _, err := svc.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
	}
}

func TestServiceRejectsEmptyKey(t *testing.T) {
	svc, st := newTestService(t, t.TempDir())
	defer st.Close()
	if err := svc.Set("", "value"); err == nil {
		t.Fatal("Set accepted an empty key")
	}
	if _, err := svc.Get(""); err == nil {
		t.Fatal("Get accepted an empty key")
	}
	if err := svc.Delete(""); err == nil {
		t.Fatal("Delete accepted an empty key")
	}
}

func TestServicePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	// Keyfile mode (no keyring) so a second boot can resolve the same key.
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	svc, err := Open(st, Options{})
	if err != nil {
		st.Close()
		t.Fatalf("secrets.Open: %v", err)
	}
	const plaintext = "persist-me-across-restart"
	if err := svc.Set("jellyfin.token", plaintext); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Second boot: same DATA_DIR, no in-memory keyring (so the 0600 keyfile is
	// the source), and the blob must still decrypt.
	st2, err := store.Open(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st2.Close()
	svc2, err := Open(st2, Options{})
	if err != nil {
		t.Fatalf("secrets.Open (reopen): %v", err)
	}
	got, err := svc2.Get("jellyfin.token")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got != plaintext {
		t.Fatalf("Get after reopen = %q, want %q", got, plaintext)
	}
}

func TestServiceDetectsTamperedRow(t *testing.T) {
	dir := t.TempDir()
	svc, st := newTestService(t, dir)
	defer st.Close()

	if err := svc.Set("sonarr.api_key", "tamper-target"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	var blob []byte
	if err := st.DB.QueryRow(`SELECT value_enc FROM secrets WHERE key = ?`, "sonarr.api_key").Scan(&blob); err != nil {
		t.Fatalf("read blob: %v", err)
	}
	blob[NonceSize+1] ^= 0xff
	if _, err := st.DB.Exec(`UPDATE secrets SET value_enc = ? WHERE key = ?`, blob, "sonarr.api_key"); err != nil {
		t.Fatalf("tamper blob: %v", err)
	}
	if _, err := svc.Get("sonarr.api_key"); err == nil {
		t.Fatal("Get returned a tampered secret without error")
	}
}

func TestServiceEncryptDecryptAndMask(t *testing.T) {
	svc, st := newTestService(t, t.TempDir())
	defer st.Close()

	blob, err := svc.EncryptString("mask-me-please")
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}
	plain, err := svc.Decrypt(blob)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(plain) != "mask-me-please" {
		t.Fatalf("Decrypt = %q", plain)
	}

	masked := svc.Mask("mask-me-please")
	if strings.Contains(masked, "mask-me-please") {
		t.Fatalf("Mask leaks the secret: %q", masked)
	}
	if svc.KeySource() == "" {
		t.Fatal("KeySource is empty")
	}
}

func TestServiceOpenRequiresStore(t *testing.T) {
	if _, err := Open(nil, Options{}); err == nil {
		t.Fatal("Open(nil) must fail")
	}
}

func TestOpenCreatesKeyfileWith0600(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	svc, err := Open(st, Options{})
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	if svc == nil {
		t.Fatal("Open returned nil service")
	}
	keyPath := filepath.Join(dir, MasterKeyFile)
	info, statErr := os.Stat(keyPath)
	if statErr != nil {
		t.Fatalf("keyfile not created by service open: %v", statErr)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("keyfile mode = %04o, want 0600", perm)
	}
}
