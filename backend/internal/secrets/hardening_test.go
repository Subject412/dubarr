package secrets

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// setFailingKeyring is reachable (Available and Get work) but cannot store a
// newly generated master key, e.g. a locked Secret Service or a full keyring.
type setFailingKeyring struct{ err error }

func (f setFailingKeyring) Available() bool { return true }
func (f setFailingKeyring) Get(string, string) (string, error) {
	return "", ErrNotFound
}
func (f setFailingKeyring) Set(string, string, string) error { return f.err }
func (f setFailingKeyring) Delete(string, string) error      { return nil }

// A keyring that accepts reads but rejects writes must abort key resolution.
// Falling back to a freshly generated keyfile would strand every secret that
// was already encrypted under the keyring key.
func TestKeyringSetFailureDoesNotFallBackToKeyfile(t *testing.T) {
	dir := t.TempDir()
	boom := errors.New("keyring: secret service is locked")

	_, src, err := ResolveMasterKey(Options{DataDir: dir, Keyring: setFailingKeyring{err: boom}})
	if err == nil {
		t.Fatalf("ResolveMasterKey succeeded (source %q) although the keyring Set failed", src)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("ResolveMasterKey error = %v, want the keyring Set failure", err)
	}

	if _, statErr := os.Stat(filepath.Join(dir, MasterKeyFile)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("keyfile was created after a keyring Set failure (stat err = %v)", statErr)
	}
}

// A corrupt .master.salt means the existing ciphertext can never be recovered
// with the configured passphrase; regenerating the salt would silently destroy
// that chance, so resolution must fail and leave the file untouched.
func TestMalformedSaltErrorsInsteadOfRegenerating(t *testing.T) {
	cases := map[string]string{
		"not base64":   "this is not a salt\n",
		"wrong length": base64.StdEncoding.EncodeToString([]byte("too-short")),
		"empty":        "",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			saltPath := filepath.Join(dir, PassphraseSaltFile)
			if err := os.WriteFile(saltPath, []byte(content), 0o600); err != nil {
				t.Fatalf("seed salt: %v", err)
			}
			before, err := os.ReadFile(saltPath)
			if err != nil {
				t.Fatalf("read seeded salt: %v", err)
			}

			_, _, resolveErr := ResolveMasterKey(Options{DataDir: dir, Passphrase: "correct horse battery staple"})
			if !errors.Is(resolveErr, ErrInvalidKey) {
				t.Fatalf("ResolveMasterKey error = %v, want ErrInvalidKey", resolveErr)
			}

			after, err := os.ReadFile(saltPath)
			if err != nil {
				t.Fatalf("salt file is gone after a failed resolve: %v", err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("salt file was rewritten: before %q, after %q", before, after)
			}
		})
	}
}
