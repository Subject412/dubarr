package secrets

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/Subject412/dubarr/backend/internal/store"
)

const (
	// KeySourceSetting is the settings row recording which Source is pinned to
	// a DATA_DIR. It stores the Source string and never key material.
	KeySourceSetting = "secrets.key_source"
	// KeyCheckRow is the sentinel secrets row proving that the resolved key is
	// the same one that encrypted the rest of the collection.
	KeyCheckRow = "__key_check"
	// KeyCheckPlaintext is the fixed plaintext sealed into KeyCheckRow.
	KeyCheckPlaintext = "dubarr-key-check-v1"
)

var (
	// ErrKeySourceMismatch reports a recorded source that can no longer supply
	// the master key while encrypted secrets exist. Callers use errors.Is and
	// a typed *SourceMismatchError for the operator-facing detail.
	ErrKeySourceMismatch = errors.New("secrets: master key source changed")
	// ErrKeyCheckMismatch reports a resolved key that cannot open the sentinel
	// row (wrong key, wiped keyring entry, or a corrupted/missing sentinel).
	ErrKeyCheckMismatch = errors.New("secrets: master key check failed")
)

// SourceMismatchError is the typed ErrKeySourceMismatch detail. It carries only
// source names and a count, never key material or plaintext.
type SourceMismatchError struct {
	Recorded Source
	Now      Source
	Secrets  int
}

func (e *SourceMismatchError) Error() string {
	return fmt.Sprintf(
		`master key source changed: recorded %q, now %q; %d encrypted secrets would be unreadable. Restore the original source or run: dubarr rekey --keyfile PATH`,
		e.Recorded, e.Now, e.Secrets)
}

// Is lets errors.Is(err, ErrKeySourceMismatch) match.
func (e *SourceMismatchError) Is(target error) bool { return target == ErrKeySourceMismatch }

// readKeySource returns the pinned source, or "" when no marker is recorded.
// A marker outside the documented Source set is an error: silently treating it
// as no marker would re-pin the DATA_DIR and strand every existing secret.
func readKeySource(db *sql.DB) (Source, error) {
	var value string
	err := db.QueryRow(`SELECT value FROM settings WHERE key = ?`, KeySourceSetting).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("secrets: read key source marker: %w", err)
	}
	source := Source(value)
	if !source.valid() {
		return "", fmt.Errorf("%w: unrecognised key source marker %q", ErrKeySourceMismatch, value)
	}
	return source, nil
}

// valid reports whether s is one of the documented Source constants.
func (s Source) valid() bool {
	switch s {
	case SourceExplicitKeyfile, SourcePassphrase, SourceKeyring, SourceKeyfile:
		return true
	default:
		return false
	}
}

// countUserSecrets counts secrets rows excluding the sentinel. This is the
// definition of a "non-empty collection" for re-pin decisions.
func countUserSecrets(db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM secrets WHERE key <> ?`, KeyCheckRow).Scan(&n); err != nil {
		return 0, fmt.Errorf("secrets: count secrets: %w", err)
	}
	return n, nil
}

// verifyKeyCheck decrypts the sentinel row with key. Missing, corrupt or
// foreign-key ciphertext all surface as ErrKeyCheckMismatch; the sentinel is
// never silently re-created.
func verifyKeyCheck(db *sql.DB, key []byte) error {
	var blob []byte
	err := db.QueryRow(`SELECT value_enc FROM secrets WHERE key = ?`, KeyCheckRow).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: sentinel row is missing", ErrKeyCheckMismatch)
	}
	if err != nil {
		return fmt.Errorf("secrets: read key check: %w", err)
	}
	c, err := NewCipher(key)
	if err != nil {
		return err
	}
	plain, err := c.DecryptString(blob)
	if err != nil {
		return fmt.Errorf("%w: sentinel does not authenticate", ErrKeyCheckMismatch)
	}
	if plain != KeyCheckPlaintext {
		return fmt.Errorf("%w: sentinel plaintext is corrupt", ErrKeyCheckMismatch)
	}
	return nil
}

// pinKeySource writes the marker and sentinel in one transaction. It is used on
// first boot, after an empty-collection re-pin, and by the M1 migration.
func pinKeySource(db *sql.DB, source Source, key []byte) error {
	c, err := NewCipher(key)
	if err != nil {
		return err
	}
	check, err := c.EncryptString(KeyCheckPlaintext)
	if err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("secrets: begin pin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
		KeySourceSetting, string(source),
	); err != nil {
		return fmt.Errorf("secrets: write key source marker: %w", err)
	}
	if err := upsertCheckRow(tx, check); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("secrets: commit pin: %w", err)
	}
	return nil
}

// upsertCheckRow writes the sentinel ciphertext inside an existing transaction.
func upsertCheckRow(tx *sql.Tx, ciphertext []byte) error {
	if _, err := tx.Exec(
		`INSERT INTO secrets (key, value_enc, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(key) DO UPDATE SET value_enc = excluded.value_enc, updated_at = CURRENT_TIMESTAMP`,
		KeyCheckRow, ciphertext,
	); err != nil {
		return fmt.Errorf("secrets: write key check: %w", err)
	}
	return nil
}

// probeExistingKey finds the key that can decrypt one existing secret row by
// trying the available candidates in resolution order. It never generates key
// material and never writes.
func probeExistingKey(db *sql.DB, opts Options) ([]byte, Source, error) {
	var blob []byte
	err := db.QueryRow(`SELECT value_enc FROM secrets WHERE key <> ? LIMIT 1`, KeyCheckRow).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", errors.New("secrets: no existing secret row to probe")
	}
	if err != nil {
		return nil, "", fmt.Errorf("secrets: read probe secret: %w", err)
	}

	for _, cand := range candidateKeys(opts) {
		c, err := NewCipher(cand.key)
		if err != nil {
			continue
		}
		if _, err := c.Decrypt(blob); err == nil {
			return cand.key, cand.source, nil
		}
	}
	return nil, "", fmt.Errorf("secrets: no available master key source can decrypt the existing secrets; run: dubarr rekey --keyfile PATH")
}

// candidateKeys returns the keys that already exist for opts, in the documented
// resolution order, without generating anything.
func candidateKeys(opts Options) []struct {
	source Source
	key    []byte
} {
	var out []struct {
		source Source
		key    []byte
	}
	add := func(source Source, key []byte, err error) {
		if err == nil && len(key) == KeySize {
			out = append(out, struct {
				source Source
				key    []byte
			}{source, key})
		}
	}
	if opts.KeyfilePath != "" {
		key, err := readKeyfile(opts.KeyfilePath)
		add(SourceExplicitKeyfile, key, err)
	}
	if opts.Passphrase != "" {
		salt, err := readSalt(filepath.Join(opts.DataDir, PassphraseSaltFile))
		if err == nil {
			add(SourcePassphrase, DerivePassphraseKey(opts.Passphrase, salt), nil)
		}
	}
	if opts.Keyring != nil && opts.Keyring.Available() {
		if encoded, err := opts.Keyring.Get(opts.serviceName(), opts.keyringUser()); err == nil {
			key, decErr := decodeKey(encoded)
			add(SourceKeyring, key, decErr)
		}
	}
	if key, err := readKeyfile(filepath.Join(opts.DataDir, MasterKeyFile)); err == nil {
		add(SourceKeyfile, key, nil)
	}
	return out
}

// migratePinning handles a pre-marker DATA_DIR with existing secrets: adopt the
// first candidate that authenticates one row, then record it.
func migratePinning(db *sql.DB, opts Options) ([]byte, Source, error) {
	key, source, err := probeExistingKey(db, opts)
	if err != nil {
		return nil, "", err
	}
	if err := pinKeySource(db, source, key); err != nil {
		return nil, "", err
	}
	log.Printf("INFO secrets: adopted existing master key source %q", source)
	return key, source, nil
}

// repinEmpty switches an empty collection to the source the current options
// resolve. It is the only automatic source change.
func repinEmpty(db *sql.DB, opts Options, previous Source) (*Service, error) {
	key, source, err := ResolveMasterKey(opts)
	if err != nil {
		return nil, err
	}
	if err := pinKeySource(db, source, key); err != nil {
		return nil, err
	}
	log.Printf("WARN secrets: master key source changed from %q to %q with no encrypted secrets; re-pinned", previous, source)

	svc, err := NewWithKey(db, key, opts.Keyring, source)
	if err != nil {
		return nil, err
	}
	svc.keySourceChanged = true
	return svc, nil
}

// Rekey decrypts every user secret with the currently recorded key and
// re-encrypts it under the key at keyfilePath in one transaction, then installs
// that key as the keyfile the next boot resolves (opts.KeyfilePath when set,
// DATA_DIR/.master.key otherwise) and pins the matching source. The previous
// keyfile is kept as <path>.prev until the commit succeeds. A single
// undecryptable row aborts the operation before any write.
func Rekey(st *store.Store, opts Options, keyfilePath string) error {
	if st == nil || st.DB == nil {
		return errors.New("secrets: store handle is required")
	}
	if keyfilePath == "" {
		return errors.New("secrets: rekey requires a keyfile path")
	}
	if opts.DataDir == "" {
		opts.DataDir = st.DataDir()
	}
	if opts.DataDir == "" {
		return errors.New("secrets: data dir must not be empty")
	}

	newKey, err := loadOrCreateKeyfile(keyfilePath)
	if err != nil {
		return fmt.Errorf("secrets: rekey: load new keyfile: %w", err)
	}
	newCipher, err := NewCipher(newKey)
	if err != nil {
		return err
	}

	recorded, err := readKeySource(st.DB)
	if err != nil {
		return err
	}
	count, err := countUserSecrets(st.DB)
	if err != nil {
		return err
	}

	var oldKey []byte
	switch {
	case recorded != "":
		oldKey, err = resolveRecorded(opts, recorded)
		if err != nil {
			return fmt.Errorf("secrets: rekey: recorded source %q is unavailable: %w", recorded, err)
		}
	case count > 0:
		oldKey, _, err = probeExistingKey(st.DB, opts)
		if err != nil {
			return fmt.Errorf("secrets: rekey: %w", err)
		}
	}

	// Decrypt everything before touching the database or the keyfile.
	type row struct {
		key   string
		plain []byte
	}
	var rows []row
	// Decrypted secret material must not outlive this call: overwrite every
	// plaintext buffer on both the success and the failure path.
	defer func() {
		plaintexts := make([][]byte, len(rows))
		for i := range rows {
			plaintexts[i] = rows[i].plain
		}
		if rekeyScrubHook != nil && len(plaintexts) > 0 {
			rekeyScrubHook(plaintexts)
		}
		for _, plain := range plaintexts {
			zeroBytes(plain)
		}
	}()
	if count > 0 {
		oldCipher, err := NewCipher(oldKey)
		if err != nil {
			return err
		}
		result, err := st.DB.Query(`SELECT key, value_enc FROM secrets WHERE key <> ?`, KeyCheckRow)
		if err != nil {
			return fmt.Errorf("secrets: rekey: list secrets: %w", err)
		}
		defer result.Close()
		for result.Next() {
			var (
				name string
				blob []byte
			)
			if err := result.Scan(&name, &blob); err != nil {
				return fmt.Errorf("secrets: rekey: scan secret: %w", err)
			}
			plain, err := oldCipher.Decrypt(blob)
			if err != nil {
				return fmt.Errorf("secrets: rekey: cannot decrypt %q: %w", name, err)
			}
			rows = append(rows, row{key: name, plain: plain})
		}
		if err := result.Err(); err != nil {
			return fmt.Errorf("secrets: rekey: iterate secrets: %w", err)
		}
		if err := result.Close(); err != nil {
			return fmt.Errorf("secrets: rekey: close secrets: %w", err)
		}
	}

	// Install the new key where the next boot will look: the operator's
	// explicit keyfile when one is configured, DATA_DIR/.master.key otherwise.
	// A recorded explicit-keyfile source always implies a configured keyfile,
	// because resolving the old key above needs it.
	installedPath := filepath.Join(opts.DataDir, MasterKeyFile)
	pinSource := SourceKeyfile
	if opts.KeyfilePath != "" || recorded == SourceExplicitKeyfile {
		pinSource = SourceExplicitKeyfile
	}
	if opts.KeyfilePath != "" {
		installedPath = opts.KeyfilePath
	}

	backup, existed, readErr := readFileMaybe(installedPath)
	if readErr != nil {
		return fmt.Errorf("secrets: rekey: read existing keyfile: %w", readErr)
	}
	// Persist the old keyfile before the new one replaces it so a crash before
	// the commit leaves .prev behind and the old key recoverable.
	backupPath := installedPath + ".prev"
	if existed {
		if err := writeFileAtomic(backupPath, backup); err != nil {
			return fmt.Errorf("secrets: rekey: back up existing keyfile: %w", err)
		}
	}

	tx, err := st.DB.Begin()
	if err != nil {
		return fmt.Errorf("secrets: rekey: begin: %w", err)
	}
	fail := func(cause error) error {
		_ = tx.Rollback()
		if restoreErr := restoreKeyfile(installedPath, backupPath, existed); restoreErr != nil {
			return errors.Join(cause, restoreErr)
		}
		return cause
	}

	for _, r := range rows {
		blob, err := newCipher.Encrypt(r.plain)
		if err != nil {
			return fail(err)
		}
		if _, err := tx.Exec(
			`UPDATE secrets SET value_enc = ?, updated_at = CURRENT_TIMESTAMP WHERE key = ?`,
			blob, r.key,
		); err != nil {
			return fail(fmt.Errorf("secrets: rekey: update %q: %w", r.key, err))
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
		KeySourceSetting, string(pinSource),
	); err != nil {
		return fail(fmt.Errorf("secrets: rekey: write marker: %w", err))
	}
	check, err := newCipher.EncryptString(KeyCheckPlaintext)
	if err != nil {
		return fail(err)
	}
	if err := upsertCheckRow(tx, check); err != nil {
		return fail(err)
	}

	// Make the new key durable before the database commit so a successful
	// commit can always be reopened. The old key stays in .prev until then.
	if err := installKeyfileNow(installedPath, newKey); err != nil {
		return fail(fmt.Errorf("secrets: rekey: install keyfile: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return fail(fmt.Errorf("secrets: rekey: commit: %w", err))
	}
	if err := os.Remove(backupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("WARN secrets: rekey: remove keyfile backup %q: %v", backupPath, err)
	}
	return nil
}

// zeroBytes overwrites b with zeroes.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// rekeyScrubHook, when set (tests only), observes the plaintext buffers Rekey
// is about to zeroize so a test can assert they do not survive the call.
var rekeyScrubHook func(plaintexts [][]byte)

// readFileMaybe returns the file content and whether it existed.
func readFileMaybe(path string) ([]byte, bool, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return content, true, nil
}

// restoreKeyfile puts back the pre-rekey keyfile from its .prev backup, or
// removes a keyfile that did not exist before, then drops the backup. A failed
// rekey must leave no stray key material behind.
func restoreKeyfile(path, backupPath string, existed bool) error {
	if !existed {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("secrets: rekey: remove new keyfile: %w", err)
		}
	} else {
		content, err := os.ReadFile(backupPath)
		if err != nil {
			return fmt.Errorf("secrets: rekey: read keyfile backup: %w", err)
		}
		if err := writeFileAtomic(path, content); err != nil {
			return fmt.Errorf("secrets: rekey: restore keyfile backup: %w", err)
		}
	}
	if err := os.Remove(backupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("secrets: rekey: remove keyfile backup: %w", err)
	}
	return nil
}

// installKeyfile writes key material as DATA_DIR/.master.key, 0600.
func installKeyfile(path string, key []byte) error {
	return writeFileAtomic(path, []byte(encodeKey(key)+"\n"))
}

// installKeyfileNow is the install step used by Rekey. Tests swap it to force
// the crash-window failure (install done, commit not yet) and prove rollback.
var installKeyfileNow = installKeyfile

// writeFileAtomic writes content 0600 via a temporary sibling and a rename.
func writeFileAtomic(path string, content []byte) error {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".master.key.tmp-*")
	if err != nil {
		return fmt.Errorf("secrets: create temporary keyfile: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("secrets: write temporary keyfile: %w", err)
	}
	if err := tmp.Chmod(filePerm); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("secrets: chmod temporary keyfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("secrets: close temporary keyfile: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("secrets: install keyfile: %w", err)
	}
	return nil
}
