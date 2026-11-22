package vault

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// Pipeline errors. Collision also matches fs.ErrExist so callers can test
// either.
var (
	// ErrCollision reports that <base>.<iso2>.mka already exists in the vault.
	// The existing file is never overwritten.
	ErrCollision = errors.New("vault: destination already exists")
	// ErrNotConfigured reports that storage.paths.vault_dir is unset.
	ErrNotConfigured = errors.New("vault: vault directory is not configured")
	// ErrInvalidRequest reports a malformed write request (bad base name,
	// language, media metadata, ...).
	ErrInvalidRequest = errors.New("vault: invalid request")
	// ErrStagingCleanup reports that the vault write and database record
	// succeeded but the staged file could not be removed. A non-nil Entry is
	// returned alongside it: the archive is complete and durable.
	ErrStagingCleanup = errors.New("vault: staging cleanup failed")
)

// Recorder persists vault entries. *store.Store satisfies it.
type Recorder interface {
	EnsureVaultMedia(ctx context.Context, media store.VaultMediaRecord) (int64, error)
	RecordVaultTrack(ctx context.Context, track store.VaultTrackRecord) (int64, error)
}

// SettingsReader reads the storage section. *settings.Store satisfies it.
type SettingsReader interface {
	Get(ctx context.Context, key string) (json.RawMessage, bool, error)
}

// Dependencies configures a Writer.
type Dependencies struct {
	// Store records vault entries. Required.
	Store Recorder
	// Settings supplies storage.paths. Required: the vault directory is never
	// guessed.
	Settings SettingsReader
	// DataDir is the fallback base for WORK_DIR when storage.paths.work_dir is
	// unset, mirroring package extract.
	DataDir string
}

// Request describes one staged track to archive.
type Request struct {
	// StagedPath is the M3-1 output file under WORK_DIR/extract/<id>/.
	StagedPath string
	// BaseName is the archived file's stem, e.g. "Spirited Away (2001)".
	// The suffix ".<iso2>.mka" is appended by the writer.
	BaseName string
	// Language is the extracted track's language as ISO 639-1 or ISO 639-2/B
	// (pol, pl, eng, en, ...). It is normalized to the ISO 639-1 filename
	// suffix; a rare 3-letter code is kept when no 2-letter form exists.
	Language string

	// Media identifies the vault_media parent row.
	Media store.VaultMediaRecord
	// SeasonNumber/EpisodeNumber are set for series episodes.
	SeasonNumber  *int
	EpisodeNumber *int
	// GroupUUID links tracks extracted from the same donor; generated when
	// empty.
	GroupUUID string
	// Variant is dubbing, lektor, original, standard or commentary; empty
	// means standard.
	Variant        string
	IsOriginalLang bool

	Codec             string
	Channels          string
	ChannelCount      int
	BitrateBPS        int
	SampleRate        int
	SourceFPS         float64
	DurationMS        int64
	SourceReleaseName string
	Notes             string
}

// Entry is the outcome of a successful vault write.
type Entry struct {
	ID           int64
	Path         string
	SHA256       string
	Size         int64
	LanguageISO2 string
	LanguageISO3 string
	CreatedAt    time.Time
}

// Writer publishes staged tracks into VAULT_DIR. It is safe for concurrent use
// and does not start goroutines.
type Writer struct {
	store    Recorder
	settings SettingsReader
	dataDir  string

	// Test seams.
	renameFile func(oldPath, newPath string) error

	mu sync.Mutex
}

// New validates deps and builds a Writer.
func New(deps Dependencies) (*Writer, error) {
	if deps.Store == nil {
		return nil, errors.New("vault: recorder is required")
	}
	if deps.Settings == nil {
		return nil, errors.New("vault: settings reader is required")
	}
	return &Writer{
		store:      deps.Store,
		settings:   deps.Settings,
		dataDir:    deps.DataDir,
		renameFile: os.Rename,
	}, nil
}

// Write archives one staged file. On success the staged file (and its import
// directory when empty) is removed. On failure before the rename the vault is
// untouched and the staged file remains; after the rename the returned error
// names the durable final path and its SHA-256.
func (w *Writer) Write(ctx context.Context, req Request) (*Entry, error) {
	media := req.Media
	if err := validateRequest(req, media); err != nil {
		return nil, err
	}
	iso2, iso3, err := normalizeLanguage(req.Language)
	if err != nil {
		return nil, err
	}

	vaultDir, workDir, err := w.storagePaths(ctx)
	if err != nil {
		return nil, err
	}
	extractRoot := filepath.Join(workDir, "extract")

	staged, err := canonicalStaged(req.StagedPath, extractRoot)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(staged)
	if err != nil {
		return nil, fmt.Errorf("vault: staged file %s: %w", staged, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: staged path %s is not a regular file", ErrInvalidRequest, staged)
	}

	finalPath := filepath.Join(vaultDir, req.BaseName+"."+iso2+".mka")
	if media.FolderPath == "" {
		media.FolderPath = vaultDir
	}

	// Write-once: refuse an existing destination before copying anything, so a
	// refused overwrite reports ErrCollision rather than surfacing a copy error
	// (ENOSPC, EACCES) and never wastes a full copy of the staged file. The
	// mutex closes the check/rename race inside this process; see the package
	// comment for the cross-process TOCTOU note.
	w.mu.Lock()
	err = checkDestinationFree(finalPath)
	w.mu.Unlock()
	if err != nil {
		return nil, err
	}

	digest, size, tempPath, err := w.copyToTemp(staged, vaultDir, info.Size())
	if err != nil {
		return nil, err
	}
	// Until published, every failure removes the temp file: the vault then
	// holds only previously archived tracks.
	published := false
	defer func() {
		if !published {
			_ = os.Remove(tempPath)
		}
	}()

	// Belt-and-braces: re-check under the same mutex immediately before the
	// rename, in case the destination appeared since the check above.
	w.mu.Lock()
	if err := checkDestinationFree(finalPath); err != nil {
		w.mu.Unlock()
		return nil, err
	}
	if err := w.renameFile(tempPath, finalPath); err != nil {
		w.mu.Unlock()
		return nil, fmt.Errorf("vault: rename %s -> %s: %w", tempPath, finalPath, err)
	}
	published = true
	w.mu.Unlock()

	if err := syncDir(vaultDir); err != nil {
		return nil, fmt.Errorf("vault: fsync %s after publishing %s (sha256 %s): %w",
			vaultDir, finalPath, digest, err)
	}
	if err := os.Chmod(finalPath, 0o444); err != nil {
		return nil, fmt.Errorf("vault: chmod 0444 %s (sha256 %s): %w", finalPath, digest, err)
	}

	mediaID, err := w.store.EnsureVaultMedia(ctx, media)
	if err != nil {
		return nil, fmt.Errorf("vault: record media for %s (file durable, sha256 %s): %w",
			finalPath, digest, err)
	}
	groupUUID := req.GroupUUID
	if groupUUID == "" {
		groupUUID, err = newGroupUUID()
		if err != nil {
			return nil, fmt.Errorf("vault: generate group uuid for %s (file durable, sha256 %s): %w",
				finalPath, digest, err)
		}
	}
	id, err := w.store.RecordVaultTrack(ctx, store.VaultTrackRecord{
		VaultMediaID:      mediaID,
		SeasonNumber:      req.SeasonNumber,
		EpisodeNumber:     req.EpisodeNumber,
		GroupUUID:         groupUUID,
		LangISO2:          iso2,
		LangISO3:          iso3,
		Variant:           variantOrDefault(req.Variant),
		IsOriginalLang:    req.IsOriginalLang,
		FilePath:          finalPath,
		SHA256:            digest,
		Codec:             req.Codec,
		Channels:          req.Channels,
		ChannelCount:      req.ChannelCount,
		BitrateBPS:        req.BitrateBPS,
		SampleRate:        req.SampleRate,
		SourceFPS:         req.SourceFPS,
		DurationMS:        req.DurationMS,
		SourceReleaseName: req.SourceReleaseName,
		Notes:             req.Notes,
	})
	if err != nil {
		return nil, fmt.Errorf("vault: record track %s (file durable, sha256 %s): %w",
			finalPath, digest, err)
	}

	entry := &Entry{
		ID:           id,
		Path:         finalPath,
		SHA256:       digest,
		Size:         size,
		LanguageISO2: iso2,
		LanguageISO3: iso3,
		CreatedAt:    time.Now().UTC(),
	}

	// Success: no staging leftovers. The import directory is removed only when
	// this was its last track (os.Remove fails harmlessly on a non-empty dir).
	if err := os.Remove(staged); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return entry, fmt.Errorf("%w: %s (vault entry %d is complete): %w",
			ErrStagingCleanup, staged, id, err)
	}
	_ = os.Remove(filepath.Dir(staged))
	return entry, nil
}

// checkDestinationFree enforces write-once: it returns ErrCollision (matching
// fs.ErrExist) when finalPath already exists. Callers hold w.mu so the check
// and the rename that follows it are atomic within this process.
func checkDestinationFree(finalPath string) error {
	if _, err := os.Lstat(finalPath); err == nil {
		return fmt.Errorf("%w: %s: %w", ErrCollision, finalPath, fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("vault: check destination %s: %w", finalPath, err)
	}
	return nil
}

// copyToTemp streams src into a hidden temp file inside vaultDir, hashing the
// archived bytes, and fsyncs the file. It returns the digest, size and temp
// path; the caller owns removal of the temp file.
func (w *Writer) copyToTemp(src, vaultDir string, wantSize int64) (digest string, size int64, tempPath string, err error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, "", fmt.Errorf("vault: open staged file %s: %w", src, err)
	}
	defer in.Close()

	tempPath, out, err := createTemp(vaultDir)
	if err != nil {
		return "", 0, "", err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = out.Close()
			_ = os.Remove(tempPath)
		}
	}()

	hasher := sha256.New()
	size, err = io.Copy(io.MultiWriter(out, hasher), in)
	if err != nil {
		return "", 0, "", fmt.Errorf("vault: write %s: %w", tempPath, err)
	}
	if size != wantSize {
		return "", 0, "", fmt.Errorf("vault: staged file %s changed while copying (size %d, want %d)", src, size, wantSize)
	}
	if err := out.Sync(); err != nil {
		return "", 0, "", fmt.Errorf("vault: fsync %s: %w", tempPath, err)
	}
	if err := out.Close(); err != nil {
		return "", 0, "", fmt.Errorf("vault: close %s: %w", tempPath, err)
	}
	cleanup = false
	return hex.EncodeToString(hasher.Sum(nil)), size, tempPath, nil
}

// createTemp opens a fresh hidden temp file in dir with 0600, retrying on the
// (astronomically unlikely) name collision.
func createTemp(dir string) (string, *os.File, error) {
	for attempt := 0; attempt < 4; attempt++ {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", nil, fmt.Errorf("vault: random temp name: %w", err)
		}
		path := filepath.Join(dir, ".dubarr-"+hex.EncodeToString(raw[:])+".tmp")
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("vault: create temp in %s: %w", dir, err)
		}
		return path, f, nil
	}
	return "", nil, fmt.Errorf("vault: create temp in %s: no unique name after retries", dir)
}

// syncDir flushes a directory entry so the rename survives a crash.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// storagePaths resolves and canonicalizes VAULT_DIR and WORK_DIR. The vault
// directory is created 0755 when missing; it is never made read-only so later
// atomic renames keep working (see package comment).
func (w *Writer) storagePaths(ctx context.Context) (vaultDir, workDir string, err error) {
	paths := settings.StoragePaths{}
	raw, ok, err := w.settings.Get(ctx, settings.KeyStoragePaths)
	if err != nil {
		return "", "", fmt.Errorf("vault: read storage.paths: %w", err)
	}
	if ok {
		if err := json.Unmarshal(raw, &paths); err != nil {
			return "", "", fmt.Errorf("vault: decode storage.paths: %w", err)
		}
	}

	vaultDir = strings.TrimSpace(paths.VaultDir)
	if vaultDir == "" {
		return "", "", ErrNotConfigured
	}
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		return "", "", fmt.Errorf("vault: create vault dir %s: %w", vaultDir, err)
	}
	vaultDir, err = filepath.EvalSymlinks(vaultDir)
	if err != nil {
		return "", "", fmt.Errorf("vault: canonicalize vault dir: %w", err)
	}

	workDir = strings.TrimSpace(paths.WorkDir)
	if workDir == "" {
		if strings.TrimSpace(w.dataDir) == "" {
			return "", "", errors.New("vault: work directory is not configured")
		}
		workDir = filepath.Join(w.dataDir, "work")
	}
	workDir, err = filepath.EvalSymlinks(workDir)
	if err != nil {
		return "", "", fmt.Errorf("vault: canonicalize work dir %s: %w", workDir, err)
	}
	return vaultDir, workDir, nil
}

// canonicalStaged resolves the staged file and confines it to the extraction
// root (IDEA.md §3.2 path guard).
func canonicalStaged(path, extractRoot string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%w: staged path is required", ErrInvalidRequest)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("vault: resolve staged file %s: %w", path, err)
	}
	if !within(extractRoot, canonical) {
		return "", fmt.Errorf("%w: staged file %s is outside %s", ErrInvalidRequest, canonical, extractRoot)
	}
	return canonical, nil
}

// within reports whether path is root or below it.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func validateRequest(req Request, media store.VaultMediaRecord) error {
	if err := validateBaseName(req.BaseName); err != nil {
		return err
	}
	if strings.TrimSpace(req.Language) == "" {
		return fmt.Errorf("%w: language is required", ErrInvalidRequest)
	}
	if media.MediaType != "movie" && media.MediaType != "series" {
		return fmt.Errorf("%w: media type %q is not movie/series", ErrInvalidRequest, media.MediaType)
	}
	if strings.TrimSpace(media.Title) == "" {
		return fmt.Errorf("%w: media title is required", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.Codec) == "" {
		return fmt.Errorf("%w: codec is required", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.Channels) == "" {
		return fmt.Errorf("%w: channel layout is required", ErrInvalidRequest)
	}
	if req.ChannelCount <= 0 {
		return fmt.Errorf("%w: channel count must be positive, got %d", ErrInvalidRequest, req.ChannelCount)
	}
	switch variantOrDefault(req.Variant) {
	case "dubbing", "lektor", "original", "standard", "commentary":
	default:
		return fmt.Errorf("%w: variant %q", ErrInvalidRequest, req.Variant)
	}
	return nil
}

func validateBaseName(base string) error {
	if base == "" {
		return fmt.Errorf("%w: base name is required", ErrInvalidRequest)
	}
	if base == "." || base == ".." {
		return fmt.Errorf("%w: base name %q", ErrInvalidRequest, base)
	}
	if strings.ContainsAny(base, `/\`+"\x00") {
		return fmt.Errorf("%w: base name %q contains a path separator", ErrInvalidRequest, base)
	}
	for _, r := range base {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: base name contains a control character", ErrInvalidRequest)
		}
	}
	return nil
}

// normalizeLanguage maps an ISO 639-1/2 language token to the filename suffix
// (2-letter when the table knows the language) and its ISO 639-2/B code.
func normalizeLanguage(raw string) (iso2, iso3 string, err error) {
	token := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.IndexAny(token, "-_"); i > 0 {
		token = token[:i]
	}
	if token == "" || token == "und" || token == "unknown" {
		return "", "", fmt.Errorf("%w: language %q has no ISO 639 code", ErrInvalidRequest, raw)
	}
	if iso1 := probe.ISO639_1(token); iso1 != "" {
		return iso1, probe.ISO639_2B(iso1), nil
	}
	// Rare languages without an ISO 639-1 code keep their 639-2/3 code
	// (IDEA.md §3.3 fallback).
	if len(token) == 3 && isAlpha(token) {
		return token, probe.ISO639_2B(token), nil
	}
	return "", "", fmt.Errorf("%w: language %q has no ISO 639 code", ErrInvalidRequest, raw)
}

func isAlpha(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return s != ""
}

func variantOrDefault(v string) string {
	if strings.TrimSpace(v) == "" {
		return "standard"
	}
	return strings.ToLower(strings.TrimSpace(v))
}

func newGroupUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
