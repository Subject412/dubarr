package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyfileCreated0600AndPersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	key1, src, err := ResolveMasterKey(Options{DataDir: dir})
	if err != nil {
		t.Fatalf("ResolveMasterKey: %v", err)
	}
	if src != SourceKeyfile {
		t.Fatalf("source = %q, want %q", src, SourceKeyfile)
	}
	if len(key1) != KeySize {
		t.Fatalf("master key length = %d, want %d", len(key1), KeySize)
	}
	if bytes.Equal(key1, make([]byte, KeySize)) {
		t.Fatal("master key is all zeroes")
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat data dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("data dir mode = %04o, want 0700", perm)
	}

	keyPath := filepath.Join(dir, MasterKeyFile)
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat keyfile: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("keyfile mode = %04o, want 0600", perm)
	}

	key2, src2, err := ResolveMasterKey(Options{DataDir: dir})
	if err != nil {
		t.Fatalf("second ResolveMasterKey: %v", err)
	}
	if src2 != SourceKeyfile {
		t.Fatalf("second source = %q, want %q", src2, SourceKeyfile)
	}
	if !bytes.Equal(key1, key2) {
		t.Fatal("master key changed across reopen")
	}
}

func TestKeyfileLoosePermissionsTightened(t *testing.T) {
	dir := t.TempDir()
	// Pre-seed a valid key with world-readable permissions.
	key := testKey(t)
	keyPath := filepath.Join(dir, MasterKeyFile)
	if err := os.WriteFile(keyPath, []byte(encodeKey(key)), 0o644); err != nil {
		t.Fatalf("seed keyfile: %v", err)
	}
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatalf("chmod seed: %v", err)
	}

	got, src, err := ResolveMasterKey(Options{DataDir: dir})
	if err != nil {
		t.Fatalf("ResolveMasterKey: %v", err)
	}
	if src != SourceKeyfile {
		t.Fatalf("source = %q", src)
	}
	if !bytes.Equal(got, key) {
		t.Fatal("existing keyfile content was not used")
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat keyfile: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("keyfile mode = %04o after resolve, want 0600", perm)
	}
}

func TestKeyfileMalformedRejectedWithoutLeaking(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, MasterKeyFile)
	const bogus = "definitely-not-a-32-byte-key-material"
	if err := os.WriteFile(keyPath, []byte(bogus), 0o600); err != nil {
		t.Fatalf("write keyfile: %v", err)
	}

	_, _, err := ResolveMasterKey(Options{DataDir: dir})
	if err == nil {
		t.Fatal("ResolveMasterKey accepted a malformed keyfile")
	}
	if strings.Contains(err.Error(), bogus) {
		t.Fatalf("error leaks keyfile content: %v", err)
	}
}

func TestExplicitKeyfilePathWins(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(t.TempDir(), "custom.key")
	kr := NewMemoryKeyring()
	if err := kr.Set(DefaultService, DefaultKeyringUser, encodeKey(testKey(t))); err != nil {
		t.Fatalf("seed keyring: %v", err)
	}

	if err := os.WriteFile(explicit, []byte(encodeKey(testKey(t))), 0o600); err != nil {
		t.Fatalf("seed explicit keyfile: %v", err)
	}

	key, src, err := ResolveMasterKey(Options{DataDir: dir, KeyfilePath: explicit, Keyring: kr})
	if err != nil {
		t.Fatalf("ResolveMasterKey: %v", err)
	}
	if src != SourceExplicitKeyfile {
		t.Fatalf("source = %q, want %q", src, SourceExplicitKeyfile)
	}
	if !bytes.Equal(key, testKey(t)) {
		t.Fatal("explicit keyfile did not win over the keyring")
	}
	info, err := os.Stat(explicit)
	if err != nil {
		t.Fatalf("stat explicit keyfile: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("explicit keyfile mode = %04o, want 0600", perm)
	}
	// The default keyfile must stay absent: only the chosen source is written.
	if _, err := os.Stat(filepath.Join(dir, MasterKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default keyfile was created although an explicit path was given (err=%v)", err)
	}
}

func TestPassphraseKeyDeterministicAndSaltPersisted(t *testing.T) {
	dir := t.TempDir()

	key1, src, err := ResolveMasterKey(Options{DataDir: dir, Passphrase: "correct horse battery staple"})
	if err != nil {
		t.Fatalf("ResolveMasterKey: %v", err)
	}
	if src != SourcePassphrase {
		t.Fatalf("source = %q, want %q", src, SourcePassphrase)
	}
	if len(key1) != KeySize {
		t.Fatalf("passphrase key length = %d, want %d", len(key1), KeySize)
	}

	saltPath := filepath.Join(dir, PassphraseSaltFile)
	info, err := os.Stat(saltPath)
	if err != nil {
		t.Fatalf("stat salt file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("salt file mode = %04o, want 0600", perm)
	}

	key2, _, err := ResolveMasterKey(Options{DataDir: dir, Passphrase: "correct horse battery staple"})
	if err != nil {
		t.Fatalf("second ResolveMasterKey: %v", err)
	}
	if !bytes.Equal(key1, key2) {
		t.Fatal("same passphrase + salt produced different keys")
	}

	c, err := NewCipher(key1)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	blob, err := c.Encrypt([]byte("vault secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	wrongKey, _, err := ResolveMasterKey(Options{DataDir: dir, Passphrase: "wrong passphrase"})
	if err != nil {
		t.Fatalf("wrong-passphrase resolve: %v", err)
	}
	wrongCipher, err := NewCipher(wrongKey)
	if err != nil {
		t.Fatalf("NewCipher(wrong): %v", err)
	}
	if _, err := wrongCipher.Decrypt(blob); err == nil {
		t.Fatal("wrong passphrase must not decrypt existing blobs")
	}
	// A wrong passphrase must not scribble over the persisted salt.
	saltAfter, err := os.ReadFile(saltPath)
	if err != nil {
		t.Fatalf("read salt: %v", err)
	}
	if len(saltAfter) == 0 {
		t.Fatal("salt file was emptied")
	}
}

func TestDerivePassphraseKeyIsDeterministic(t *testing.T) {
	salt := []byte("0123456789abcdef")
	a := DerivePassphraseKey("hunter2", salt)
	b := DerivePassphraseKey("hunter2", salt)
	if !bytes.Equal(a, b) {
		t.Fatal("DerivePassphraseKey is not deterministic")
	}
	if len(a) != KeySize {
		t.Fatalf("derived length = %d, want %d", len(a), KeySize)
	}
	if bytes.Equal(a, DerivePassphraseKey("hunter3", salt)) {
		t.Fatal("different passphrases derived the same key")
	}
	if bytes.Equal(a, DerivePassphraseKey("hunter2", []byte("fedcba9876543210"))) {
		t.Fatal("different salts derived the same key")
	}
}

func TestResolveMasterKeyRequiresDataDir(t *testing.T) {
	if _, _, err := ResolveMasterKey(Options{}); err == nil {
		t.Fatal("ResolveMasterKey accepted an empty data dir")
	}
}
