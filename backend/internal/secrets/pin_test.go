package secrets

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/store"
)

// bootStore opens a fresh store handle for dir. The caller closes it.
func bootStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	return st
}

// writeKeyfile persists key material at path with 0600, mirroring what a
// pre-existing operator keyfile looks like.
func writeKeyfile(t *testing.T, path string, key []byte) {
	t.Helper()
	if err := os.WriteFile(path, []byte(encodeKey(key)+"\n"), 0o600); err != nil {
		t.Fatalf("write keyfile: %v", err)
	}
}

// secondKey returns a deterministic 32-byte key distinct from testKey.
func secondKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = byte(255 - i*3)
	}
	return key
}

// seedSecret writes a ciphertext row directly, so tests can model pre-pinning
// databases without going through Open.
func seedSecret(t *testing.T, st *store.Store, key, plaintext string, encryptionKey []byte) {
	t.Helper()
	c, err := NewCipher(encryptionKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	blob, err := c.EncryptString(plaintext)
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO secrets (key, value_enc) VALUES (?, ?)`, key, blob); err != nil {
		t.Fatalf("insert secret: %v", err)
	}
}

// recordedSource reads the settings marker, or "" when absent.
func recordedSource(t *testing.T, st *store.Store) Source {
	t.Helper()
	var value string
	err := st.DB.QueryRow(`SELECT value FROM settings WHERE key = ?`, KeySourceSetting).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	return Source(value)
}

// TestRecordedKeyringUnavailableRefusesBoot pins the first half of the fix:
// once keyring-sourced secrets exist, a locked keyring must abort startup
// instead of falling back to a freshly generated keyfile.
func TestRecordedKeyringUnavailableRefusesBoot(t *testing.T) {
	dir := t.TempDir()
	kr := NewMemoryKeyring()

	st := bootStore(t, dir)
	svc, err := Open(st, Options{DataDir: dir, Keyring: kr})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if svc.KeySource() != SourceKeyring {
		t.Fatalf("first boot source = %q, want keyring", svc.KeySource())
	}
	if err := svc.Set("radarr.api_key", "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	// The keyring locks between boots. Nothing may be written to the keyfile.
	kr.SetAvailable(false)
	st2 := bootStore(t, dir)
	defer st2.Close()
	if _, err := Open(st2, Options{DataDir: dir, Keyring: kr}); err == nil {
		t.Fatal("Open succeeded with a locked keyring and existing secrets")
	} else if !errors.Is(err, ErrKeySourceMismatch) {
		t.Fatalf("Open error = %v, want ErrKeySourceMismatch", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, MasterKeyFile)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("keyfile was created after a keyring failure (stat err = %v)", statErr)
	}
	if _, gerr := kr.Get(DefaultService, DefaultKeyringUser); gerr != nil {
		t.Fatalf("recorded keyring entry was disturbed: %v", gerr)
	}
}

// TestRecordedKeyfileIgnoresAvailableKeyring pins the second half: a recorded
// keyfile keeps winning even when a keyring becomes available later.
func TestRecordedKeyfileIgnoresAvailableKeyring(t *testing.T) {
	dir := t.TempDir()
	st := bootStore(t, dir)
	svc, err := Open(st, Options{DataDir: dir})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := svc.Set("sonarr.api_key", "keep-me"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	kr := NewMemoryKeyring()
	st2 := bootStore(t, dir)
	defer st2.Close()
	svc2, err := Open(st2, Options{DataDir: dir, Keyring: kr})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if svc2.KeySource() != SourceKeyfile {
		t.Fatalf("source = %q, want keyfile even with a keyring available", svc2.KeySource())
	}
	got, err := svc2.Get("sonarr.api_key")
	if err != nil || got != "keep-me" {
		t.Fatalf("Get = %q, %v", got, err)
	}
}

// TestWipedKeyringDoesNotRegenerate covers an available keyring whose entry was
// removed: that is never a reason to mint a new master key.
func TestWipedKeyringDoesNotRegenerate(t *testing.T) {
	dir := t.TempDir()
	kr := NewMemoryKeyring()
	st := bootStore(t, dir)
	svc, err := Open(st, Options{DataDir: dir, Keyring: kr})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := svc.Set("jellyfin.token", "token"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := kr.Delete(DefaultService, DefaultKeyringUser); err != nil {
		t.Fatalf("wipe keyring: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	st2 := bootStore(t, dir)
	defer st2.Close()
	if _, err := Open(st2, Options{DataDir: dir, Keyring: kr}); !errors.Is(err, ErrKeySourceMismatch) {
		t.Fatalf("Open error = %v, want ErrKeySourceMismatch", err)
	}
	if _, gerr := kr.Get(DefaultService, DefaultKeyringUser); !errors.Is(gerr, ErrNotFound) {
		t.Fatalf("keyring entry was regenerated (Get err = %v)", gerr)
	}
}

// TestEmptyCollectionRepinsOnSourceChange covers the only safe automatic
// switch: with zero user secrets, a changed source is re-pinned and flagged.
func TestEmptyCollectionRepinsOnSourceChange(t *testing.T) {
	dir := t.TempDir()
	kr := NewMemoryKeyring()
	st := bootStore(t, dir)
	if _, err := Open(st, Options{DataDir: dir, Keyring: kr}); err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	kr.SetAvailable(false)
	st2 := bootStore(t, dir)
	svc, err := Open(st2, Options{DataDir: dir, Keyring: kr})
	if err != nil {
		t.Fatalf("re-pin Open: %v", err)
	}
	if svc.KeySource() != SourceKeyfile {
		t.Fatalf("re-pinned source = %q, want keyfile", svc.KeySource())
	}
	if !svc.KeySourceChanged() {
		t.Fatal("KeySourceChanged = false after an automatic re-pin")
	}
	if got := recordedSource(t, st2); got != SourceKeyfile {
		t.Fatalf("marker = %q, want keyfile", got)
	}
	if err := st2.Close(); err != nil {
		t.Fatalf("close second store: %v", err)
	}

	// The re-pin is persistent: the next boot is silent and unflagged.
	st3 := bootStore(t, dir)
	defer st3.Close()
	svc3, err := Open(st3, Options{DataDir: dir, Keyring: kr})
	if err != nil {
		t.Fatalf("third Open: %v", err)
	}
	if svc3.KeySourceChanged() {
		t.Fatal("KeySourceChanged = true on a normal boot")
	}
}

// TestMigrationProbePicksDecryptingSource covers the M1 upgrade path: no marker
// with existing secrets must adopt whichever candidate actually decrypts.
func TestMigrationProbePicksDecryptingSource(t *testing.T) {
	dir := t.TempDir()
	key := testKey(t)
	writeKeyfile(t, filepath.Join(dir, MasterKeyFile), key)

	st := bootStore(t, dir)
	defer st.Close()
	seedSecret(t, st, "old.secret", "migration-secret", key)

	// The keyring holds a valid but different key; probing must reject it and
	// fall through to the keyfile.
	kr := NewMemoryKeyring()
	if err := kr.Set(DefaultService, DefaultKeyringUser, encodeKey(secondKey(t))); err != nil {
		t.Fatalf("seed keyring: %v", err)
	}

	svc, err := Open(st, Options{DataDir: dir, Keyring: kr})
	if err != nil {
		t.Fatalf("migration Open: %v", err)
	}
	if svc.KeySource() != SourceKeyfile {
		t.Fatalf("migrated source = %q, want keyfile", svc.KeySource())
	}
	if got := recordedSource(t, st); got != SourceKeyfile {
		t.Fatalf("marker = %q, want keyfile", got)
	}
	if got, err := svc.Get("old.secret"); err != nil || got != "migration-secret" {
		t.Fatalf("Get = %q, %v", got, err)
	}
}

// TestMigrationProbeWithoutMatchPointsAtRekey covers the unrecoverable upgrade:
// nothing decrypts, so startup fails with a rekey hint and writes nothing.
func TestMigrationProbeWithoutMatchPointsAtRekey(t *testing.T) {
	dir := t.TempDir()
	st := bootStore(t, dir)
	defer st.Close()
	seedSecret(t, st, "old.secret", "migration-secret", secondKey(t))

	kr := NewMemoryKeyring()
	if err := kr.Set(DefaultService, DefaultKeyringUser, encodeKey(testKey(t))); err != nil {
		t.Fatalf("seed keyring: %v", err)
	}

	_, err := Open(st, Options{DataDir: dir, Keyring: kr})
	if err == nil {
		t.Fatal("Open succeeded although no candidate decrypts the existing secrets")
	}
	if !strings.Contains(err.Error(), "dubarr rekey --keyfile PATH") {
		t.Fatalf("error %q does not point at the rekey command", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, MasterKeyFile)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("keyfile was created during a failed migration (stat err = %v)", statErr)
	}
	if got := recordedSource(t, st); got != "" {
		t.Fatalf("marker was written during a failed migration: %q", got)
	}
}

// TestRekeyRoundTrip re-encrypts every row under a new keyfile and pins it so a
// plain boot keeps working.
func TestRekeyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := bootStore(t, dir)
	svc, err := Open(st, Options{DataDir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for k, v := range map[string]string{"a": "alpha", "b": "beta"} {
		if err := svc.Set(k, v); err != nil {
			t.Fatalf("Set %q: %v", k, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	newKey := secondKey(t)
	newPath := filepath.Join(t.TempDir(), "new-master.key")
	writeKeyfile(t, newPath, newKey)

	st2 := bootStore(t, dir)
	if err := Rekey(st2, Options{DataDir: dir}, newPath); err != nil {
		t.Fatalf("Rekey: %v", err)
	}
	if got := recordedSource(t, st2); got != SourceKeyfile {
		t.Fatalf("marker after rekey = %q, want keyfile", got)
	}
	if err := st2.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// The new key was installed as DATA_DIR/.master.key.
	installed, err := readKeyfile(filepath.Join(dir, MasterKeyFile))
	if err != nil {
		t.Fatalf("read installed keyfile: %v", err)
	}
	if !bytes.Equal(installed, newKey) {
		t.Fatal("rekey did not install the new keyfile")
	}

	st3 := bootStore(t, dir)
	defer st3.Close()
	svc3, err := Open(st3, Options{DataDir: dir})
	if err != nil {
		t.Fatalf("Open after rekey: %v", err)
	}
	for k, want := range map[string]string{"a": "alpha", "b": "beta"} {
		if got, err := svc3.Get(k); err != nil || got != want {
			t.Fatalf("Get %q = %q, %v; want %q", k, got, err, want)
		}
	}
}

// TestRekeyFailsBeforeAnyWrite keeps the operation atomic: one undecryptable
// row aborts the whole rekey with rows, marker and keyfile untouched.
func TestRekeyFailsBeforeAnyWrite(t *testing.T) {
	dir := t.TempDir()
	st := bootStore(t, dir)
	svc, err := Open(st, Options{DataDir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := svc.Set("a", "alpha"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	var before []byte
	if err := st.DB.QueryRow(`SELECT value_enc FROM secrets WHERE key = 'a'`).Scan(&before); err != nil {
		t.Fatalf("read blob: %v", err)
	}
	keyfileBefore, err := os.ReadFile(filepath.Join(dir, MasterKeyFile))
	if err != nil {
		t.Fatalf("read keyfile: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Corrupt the only user row so the old key cannot open it.
	st2 := bootStore(t, dir)
	defer st2.Close()
	corrupt := append([]byte(nil), before...)
	corrupt[NonceSize] ^= 0xff
	if _, err := st2.DB.Exec(`UPDATE secrets SET value_enc = ? WHERE key = 'a'`, corrupt); err != nil {
		t.Fatalf("corrupt row: %v", err)
	}
	markerBefore := recordedSource(t, st2)

	newPath := filepath.Join(t.TempDir(), "new-master.key")
	writeKeyfile(t, newPath, secondKey(t))

	if err := Rekey(st2, Options{DataDir: dir}, newPath); err == nil {
		t.Fatal("Rekey succeeded although a row does not decrypt")
	}
	var after []byte
	if err := st2.DB.QueryRow(`SELECT value_enc FROM secrets WHERE key = 'a'`).Scan(&after); err != nil {
		t.Fatalf("read blob after rekey: %v", err)
	}
	if !bytes.Equal(corrupt, after) {
		t.Fatal("a row was rewritten despite the rekey failure")
	}
	if got := recordedSource(t, st2); got != markerBefore {
		t.Fatalf("marker changed from %q to %q", markerBefore, got)
	}
	keyfileAfter, err := os.ReadFile(filepath.Join(dir, MasterKeyFile))
	if err != nil {
		t.Fatalf("read keyfile: %v", err)
	}
	if !bytes.Equal(keyfileBefore, keyfileAfter) {
		t.Fatal("the installed keyfile was replaced despite the rekey failure")
	}
}

// TestCorruptOrMissingKeyCheckIsFatal: a marker plus a broken sentinel row is
// never re-initialised silently.
func TestCorruptOrMissingKeyCheckIsFatal(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		dir := t.TempDir()
		st := bootStore(t, dir)
		svc, err := Open(st, Options{DataDir: dir})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := svc.Set("a", "alpha"); err != nil {
			t.Fatalf("Set: %v", err)
		}
		if _, err := st.DB.Exec(`DELETE FROM secrets WHERE key = ?`, KeyCheckRow); err != nil {
			t.Fatalf("delete check row: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}

		st2 := bootStore(t, dir)
		defer st2.Close()
		if _, err := Open(st2, Options{DataDir: dir}); !errors.Is(err, ErrKeyCheckMismatch) {
			t.Fatalf("Open error = %v, want ErrKeyCheckMismatch", err)
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		dir := t.TempDir()
		st := bootStore(t, dir)
		svc, err := Open(st, Options{DataDir: dir})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := svc.Set("a", "alpha"); err != nil {
			t.Fatalf("Set: %v", err)
		}
		var blob []byte
		if err := st.DB.QueryRow(`SELECT value_enc FROM secrets WHERE key = ?`, KeyCheckRow).Scan(&blob); err != nil {
			t.Fatalf("read check row: %v", err)
		}
		blob[NonceSize] ^= 0xff
		if _, err := st.DB.Exec(`UPDATE secrets SET value_enc = ? WHERE key = ?`, blob, KeyCheckRow); err != nil {
			t.Fatalf("tamper check row: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}

		st2 := bootStore(t, dir)
		defer st2.Close()
		if _, err := Open(st2, Options{DataDir: dir}); !errors.Is(err, ErrKeyCheckMismatch) {
			t.Fatalf("Open error = %v, want ErrKeyCheckMismatch", err)
		}
	})
}

// TestMismatchErrorHasNoKeyMaterial guards the "never print secrets" rule while
// checking the operator-facing wording.
func TestMismatchErrorHasNoKeyMaterial(t *testing.T) {
	dir := t.TempDir()
	kr := NewMemoryKeyring()
	st := bootStore(t, dir)
	svc, err := Open(st, Options{DataDir: dir, Keyring: kr})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := svc.Set("a", "alpha"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	encoded, err := kr.Get(DefaultService, DefaultKeyringUser)
	if err != nil {
		t.Fatalf("read keyring: %v", err)
	}

	kr.SetAvailable(false)
	st2 := bootStore(t, dir)
	defer st2.Close()
	_, openErr := Open(st2, Options{DataDir: dir, Keyring: kr})
	if !errors.Is(openErr, ErrKeySourceMismatch) {
		t.Fatalf("Open error = %v, want ErrKeySourceMismatch", openErr)
	}
	msg := openErr.Error()
	for _, leak := range []string{encoded, strings.TrimRight(encoded, "=")} {
		if leak != "" && strings.Contains(msg, leak) {
			t.Fatalf("error message leaks key material: %q", msg)
		}
	}
	for _, want := range []string{
		`recorded "keyring"`,
		`now "keyfile"`,
		"1 encrypted secrets would be unreadable",
		"dubarr rekey --keyfile PATH",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q is missing %q", msg, want)
		}
	}
}

// TestRekeyRestoresOldKeyfileOnInstallFailure covers the crash window between
// installing the new keyfile and committing: when the install fails, the old
// keyfile must come back from the .prev backup so the old key stays usable.
func TestRekeyRestoresOldKeyfileOnInstallFailure(t *testing.T) {
	dir := t.TempDir()
	st := bootStore(t, dir)
	svc, err := Open(st, Options{DataDir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := svc.Set("api.key", "keep-me"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	installedPath := filepath.Join(dir, MasterKeyFile)
	oldKeyfile, err := os.ReadFile(installedPath)
	if err != nil {
		t.Fatalf("read old keyfile: %v", err)
	}

	newPath := filepath.Join(t.TempDir(), "new-master.key")
	writeKeyfile(t, newPath, secondKey(t))

	origInstall := installKeyfileNow
	installKeyfileNow = func(string, []byte) error { return errors.New("injected install failure") }
	t.Cleanup(func() { installKeyfileNow = origInstall })

	st2 := bootStore(t, dir)
	defer st2.Close()
	if err := Rekey(st2, Options{DataDir: dir}, newPath); err == nil {
		t.Fatal("Rekey succeeded although installing the keyfile failed")
	}

	restored, err := os.ReadFile(installedPath)
	if err != nil {
		t.Fatalf("read restored keyfile: %v", err)
	}
	if !bytes.Equal(restored, oldKeyfile) {
		t.Fatal("the old keyfile was not restored after a failed install")
	}
	if _, err := os.Stat(installedPath + ".prev"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("keyfile backup was left behind (stat err = %v)", err)
	}
	if got := recordedSource(t, st2); got != SourceKeyfile {
		t.Fatalf("marker after failed rekey = %q, want keyfile", got)
	}
	if err := st2.Close(); err != nil {
		t.Fatalf("close second store: %v", err)
	}

	// The old keyfile still opens the collection.
	st3 := bootStore(t, dir)
	defer st3.Close()
	svc3, err := Open(st3, Options{DataDir: dir})
	if err != nil {
		t.Fatalf("Open after failed rekey: %v", err)
	}
	if got, err := svc3.Get("api.key"); err != nil || got != "keep-me" {
		t.Fatalf("Get after failed rekey = %q, %v; want keep-me", got, err)
	}
}

// TestRekeyInstallsExplicitKeyfile: with an explicit keyfile configured, rekey
// must rotate that keyfile and keep pinning the explicit-keyfile source.
func TestRekeyInstallsExplicitKeyfile(t *testing.T) {
	dir := t.TempDir()
	explicitPath := filepath.Join(dir, "operator.key")
	writeKeyfile(t, explicitPath, testKey(t))

	st := bootStore(t, dir)
	svc, err := Open(st, Options{DataDir: dir, KeyfilePath: explicitPath})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if svc.KeySource() != SourceExplicitKeyfile {
		t.Fatalf("initial source = %q, want explicit-keyfile", svc.KeySource())
	}
	if err := svc.Set("b", "beta"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	newPath := filepath.Join(t.TempDir(), "rotated.key")
	newKey := secondKey(t)
	writeKeyfile(t, newPath, newKey)

	st2 := bootStore(t, dir)
	if err := Rekey(st2, Options{DataDir: dir, KeyfilePath: explicitPath}, newPath); err != nil {
		t.Fatalf("Rekey: %v", err)
	}
	if got := recordedSource(t, st2); got != SourceExplicitKeyfile {
		t.Fatalf("marker after rekey = %q, want explicit-keyfile", got)
	}
	installed, err := readKeyfile(explicitPath)
	if err != nil {
		t.Fatalf("read explicit keyfile: %v", err)
	}
	if !bytes.Equal(installed, newKey) {
		t.Fatal("rekey did not install the new key at the explicit keyfile path")
	}
	if _, err := os.Stat(filepath.Join(dir, MasterKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rekey created DATA_DIR/.master.key although an explicit keyfile is pinned (stat err = %v)", err)
	}
	if err := st2.Close(); err != nil {
		t.Fatalf("close second store: %v", err)
	}

	st3 := bootStore(t, dir)
	defer st3.Close()
	svc3, err := Open(st3, Options{DataDir: dir, KeyfilePath: explicitPath})
	if err != nil {
		t.Fatalf("Open after rekey: %v", err)
	}
	if got, err := svc3.Get("b"); err != nil || got != "beta" {
		t.Fatalf("Get after rekey = %q, %v; want beta", got, err)
	}
}

// TestCorruptKeySourceMarkerIsFatal: an unrecognised marker is never treated as
// a source and never silently re-pinned, even with an empty collection.
func TestCorruptKeySourceMarkerIsFatal(t *testing.T) {
	t.Run("with secrets", func(t *testing.T) {
		dir := t.TempDir()
		st := bootStore(t, dir)
		svc, err := Open(st, Options{DataDir: dir})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := svc.Set("a", "alpha"); err != nil {
			t.Fatalf("Set: %v", err)
		}
		if _, err := st.DB.Exec(`UPDATE settings SET value = 'garbage' WHERE key = ?`, KeySourceSetting); err != nil {
			t.Fatalf("corrupt marker: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}

		st2 := bootStore(t, dir)
		defer st2.Close()
		if _, err := Open(st2, Options{DataDir: dir}); !errors.Is(err, ErrKeySourceMismatch) {
			t.Fatalf("Open error = %v, want ErrKeySourceMismatch", err)
		}
		if got := recordedSource(t, st2); got != "garbage" {
			t.Fatalf("marker was rewritten to %q", got)
		}
	})

	t.Run("without secrets", func(t *testing.T) {
		dir := t.TempDir()
		st := bootStore(t, dir)
		if _, err := Open(st, Options{DataDir: dir}); err != nil {
			t.Fatalf("Open: %v", err)
		}
		if _, err := st.DB.Exec(`UPDATE settings SET value = '' WHERE key = ?`, KeySourceSetting); err != nil {
			t.Fatalf("corrupt marker: %v", err)
		}
		if _, err := Open(st, Options{DataDir: dir}); !errors.Is(err, ErrKeySourceMismatch) {
			t.Fatalf("Open error = %v, want ErrKeySourceMismatch", err)
		}
		if got := recordedSource(t, st); got != "" {
			t.Fatalf("empty marker was re-pinned to %q", got)
		}
	})
}
