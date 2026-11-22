// Package workdir implements the Copy-on-Work contract (ADR-0002): a sync job
// never reads a vault track in place. Stage copies the tracks a job needs —
// the target track first, then any timing-reference tracks — into
// {WORK_DIR}/jobs/{job_id}/ and verifies every copied byte against the SHA-256
// recorded in audio_tracks. A mismatch marks the vault copy suspect: staging is
// wiped and the vault file is never rewritten, repaired or removed.
//
// All writes happen under WORK_DIR. The only operation performed against a
// vault path is a read-only open of the source file.
package workdir

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Subject412/dubarr/backend/internal/store"
)

// TrackReader is the store surface staging needs. *store.Store satisfies it.
type TrackReader interface {
	VaultTrackByID(ctx context.Context, id int64) (store.VaultTrackRecord, bool, error)
}

// Job identifies the vault tracks one job must stage.
type Job struct {
	// ID is the sync job id; it names the per-job directory.
	ID int64
	// TargetTrackID is the archived track that will be muxed.
	TargetTrackID int64
	// ReferenceTrackIDs are sibling archived tracks used as timing
	// references. It may be empty for the low-pass fallback.
	ReferenceTrackIDs []int64
}

// StagedTrack is one verified copy inside the job directory.
type StagedTrack struct {
	ID         int64
	SourcePath string
	Path       string
	SHA256     string
	Size       int64
}

// Staging is the outcome of a successful Stage call.
type Staging struct {
	JobID      int64
	Dir        string
	Target     StagedTrack
	References []StagedTrack
}

// Errors returned by Stage and Cleanup.
var (
	// ErrNoWorkDir reports that the constructor received an empty WORK_DIR.
	ErrNoWorkDir = errors.New("workdir: work directory is not configured")
	// ErrInvalidJob reports a job with a non-positive id or track id.
	ErrInvalidJob = errors.New("workdir: invalid job")
	// ErrTrackNotFound reports that a job references an unknown vault track.
	ErrTrackNotFound = errors.New("workdir: vault track not found")
	// ErrVaultFileMissing reports that the vault file recorded in the
	// database is absent from disk.
	ErrVaultFileMissing = errors.New("workdir: vault file is missing")
	// ErrVaultFileRead reports a vault file that exists but cannot be read.
	ErrVaultFileRead = errors.New("workdir: vault file is unreadable")
	// ErrChecksumMismatch reports that the copied bytes do not match the
	// audio_tracks.sha256 record. A *ChecksumError matches this sentinel.
	ErrChecksumMismatch = errors.New("workdir: vault track checksum mismatch")
	// ErrStagingExists reports that the job directory already exists. Stage
	// never overwrites a directory that may belong to an active job.
	ErrStagingExists = errors.New("workdir: job staging directory already exists")
)

// ChecksumError describes a failed integrity check on a staged copy.
type ChecksumError struct {
	TrackID int64
	Path    string
	Want    string
	Got     string
}

func (e *ChecksumError) Error() string {
	return fmt.Sprintf("workdir: checksum mismatch for track %d (%s): want %s, got %s",
		e.TrackID, e.Path, e.Want, e.Got)
}

// Is lets callers branch with errors.Is(err, ErrChecksumMismatch).
func (e *ChecksumError) Is(target error) bool { return target == ErrChecksumMismatch }

// Stager copies vault tracks into per-job Work Area directories.
type Stager struct {
	workDir string
	tracks  TrackReader
}

// New validates deps and builds a Stager rooted at workDir. workDir is the
// storage.paths.work_dir setting; the package never guesses it.
func New(workDir string, tracks TrackReader) (*Stager, error) {
	if strings.TrimSpace(workDir) == "" {
		return nil, ErrNoWorkDir
	}
	if tracks == nil {
		return nil, errors.New("workdir: vault track reader is required")
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("workdir: resolve work dir %s: %w", workDir, err)
	}
	return &Stager{workDir: filepath.Clean(abs), tracks: tracks}, nil
}

func (j Job) validate() error {
	if j.ID <= 0 {
		return fmt.Errorf("%w: job id %d", ErrInvalidJob, j.ID)
	}
	if j.TargetTrackID <= 0 {
		return fmt.Errorf("%w: target track id %d", ErrInvalidJob, j.TargetTrackID)
	}
	for _, id := range j.ReferenceTrackIDs {
		if id <= 0 {
			return fmt.Errorf("%w: reference track id %d", ErrInvalidJob, id)
		}
	}
	return nil
}

// Stage copies and verifies every track in job under
// {WORK_DIR}/jobs/{job.ID}/. Any failure removes the job directory before
// returning, so a failed stage leaves no partial copies behind. On success the
// caller owns the directory and must call Cleanup when the job finishes.
func (s *Stager) Stage(ctx context.Context, job Job) (*Staging, error) {
	if err := job.validate(); err != nil {
		return nil, err
	}
	jobsRoot := s.jobsRoot()
	if err := os.MkdirAll(jobsRoot, 0o700); err != nil {
		return nil, fmt.Errorf("workdir: create jobs root %s: %w", jobsRoot, err)
	}
	dir := filepath.Join(jobsRoot, strconv.FormatInt(job.ID, 10))
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%w: %s", ErrStagingExists, dir)
		}
		return nil, fmt.Errorf("workdir: create staging dir %s: %w", dir, err)
	}

	staging, err := s.stageTracks(ctx, job, dir)
	if err != nil {
		if wipeErr := os.RemoveAll(dir); wipeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("workdir: wipe staging %s: %w", dir, wipeErr))
		}
		return nil, err
	}
	return staging, nil
}

func (s *Stager) stageTracks(ctx context.Context, job Job, dir string) (*Staging, error) {
	target, err := s.stageOne(ctx, "target", job.TargetTrackID, dir)
	if err != nil {
		return nil, err
	}
	staging := &Staging{JobID: job.ID, Dir: dir, Target: target}

	seen := map[int64]bool{job.TargetTrackID: true}
	for _, id := range job.ReferenceTrackIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		ref, err := s.stageOne(ctx, "ref", id, dir)
		if err != nil {
			return nil, err
		}
		staging.References = append(staging.References, ref)
	}
	return staging, nil
}

// stageOne looks up one vault track, copies it into dir and hashes the copy.
func (s *Stager) stageOne(ctx context.Context, kind string, id int64, dir string) (StagedTrack, error) {
	rec, found, err := s.tracks.VaultTrackByID(ctx, id)
	if err != nil {
		return StagedTrack{}, fmt.Errorf("workdir: read vault track %d: %w", id, err)
	}
	if !found {
		return StagedTrack{}, fmt.Errorf("%w: %d", ErrTrackNotFound, id)
	}
	if strings.TrimSpace(rec.FilePath) == "" {
		return StagedTrack{}, fmt.Errorf("%w: track %d has no file path", ErrTrackNotFound, id)
	}
	base := filepath.Base(rec.FilePath)
	if !strings.EqualFold(filepath.Ext(base), ".mka") {
		return StagedTrack{}, fmt.Errorf("%w: track %d is not a .mka file: %s", ErrInvalidJob, id, rec.FilePath)
	}
	dst := filepath.Join(dir, fmt.Sprintf("%s-%d-%s", kind, id, base))
	return copyVerified(id, rec.FilePath, dst, rec.SHA256)
}

// copyVerified streams src into dst (which must not exist), hashing the bytes
// actually written to the Work Area. The source is only ever opened read-only.
// A digest mismatch removes the copy and returns a *ChecksumError; the caller
// wipes the whole staging directory.
func copyVerified(id int64, src, dst, want string) (StagedTrack, error) {
	in, err := os.Open(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return StagedTrack{}, fmt.Errorf("%w: track %d: %s: %w", ErrVaultFileMissing, id, src, err)
		}
		return StagedTrack{}, fmt.Errorf("%w: track %d: %s: %w", ErrVaultFileRead, id, src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return StagedTrack{}, fmt.Errorf("workdir: create staged copy %s: %w", dst, err)
	}
	hasher := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(out, hasher), in)
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(dst)
		return StagedTrack{}, fmt.Errorf("workdir: copy %s -> %s: %w", src, dst, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return StagedTrack{}, fmt.Errorf("workdir: close staged copy %s: %w", dst, closeErr)
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	wantNorm := strings.ToLower(strings.TrimSpace(want))
	if wantNorm == "" || got != wantNorm {
		_ = os.Remove(dst)
		return StagedTrack{}, &ChecksumError{TrackID: id, Path: src, Want: wantNorm, Got: got}
	}
	return StagedTrack{ID: id, SourcePath: src, Path: dst, SHA256: got, Size: size}, nil
}

// Cleanup removes {WORK_DIR}/jobs/{jobID}/. A missing directory is not an
// error, so cleanup is safe to call on both success and failure paths.
func (s *Stager) Cleanup(jobID int64) error {
	if jobID <= 0 {
		return fmt.Errorf("%w: job id %d", ErrInvalidJob, jobID)
	}
	dir := filepath.Join(s.jobsRoot(), strconv.FormatInt(jobID, 10))
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("workdir: remove staging %s: %w", dir, err)
	}
	return nil
}

// ReapStale removes every job directory under {WORK_DIR}/jobs whose
// modification time is older than olderThan. It is the boot-time counterpart
// of the extraction worker's stale-staging reaper.
func (s *Stager) ReapStale(olderThan time.Time) error {
	entries, err := os.ReadDir(s.jobsRoot())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("workdir: read jobs root: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !info.ModTime().Before(olderThan) {
			continue
		}
		path := filepath.Join(s.jobsRoot(), entry.Name())
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Stager) jobsRoot() string { return filepath.Join(s.workDir, "jobs") }
