package workdir

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/store"
)

type vaultFileSig struct {
	data  string
	mode  fs.FileMode
	mtime time.Time
	size  int64
}

// snapshotVault records every entry directly under dir, so a test can prove the
// vault was not created in, written to, deleted from or touched.
func snapshotVault(t *testing.T, dir string) map[string]vaultFileSig {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read vault dir: %v", err)
	}
	out := make(map[string]vaultFileSig, len(entries))
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read vault file %s: %v", path, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat vault file %s: %v", path, err)
		}
		out[entry.Name()] = vaultFileSig{data: string(data), mode: info.Mode(), mtime: info.ModTime(), size: info.Size()}
	}
	return out
}

func assertVaultUnchanged(t *testing.T, before, after map[string]vaultFileSig) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("vault entry count changed: before %d, after %d", len(before), len(after))
	}
	for name, want := range before {
		got, ok := after[name]
		if !ok {
			t.Fatalf("vault entry %s disappeared", name)
		}
		if got.data != want.data {
			t.Fatalf("vault file %s bytes changed", name)
		}
		if !got.mtime.Equal(want.mtime) {
			t.Fatalf("vault file %s mtime changed: %v -> %v", name, want.mtime, got.mtime)
		}
		if got.mode != want.mode || got.size != want.size {
			t.Fatalf("vault file %s metadata changed", name)
		}
	}
}

func sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// vaultFixture creates a vault file with a fixed mtime and returns its path and
// digest.
func writeVaultFile(t *testing.T, vaultDir, name, data string) (string, string) {
	t.Helper()
	path := filepath.Join(vaultDir, name)
	if err := os.WriteFile(path, []byte(data), 0o444); err != nil {
		t.Fatalf("write vault file: %v", err)
	}
	stamp := time.Unix(1_700_000_000, 0).UTC()
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatalf("chtimes vault file: %v", err)
	}
	return path, sha256Hex(data)
}

// recordTrack seeds a vault_media row and returns the audio_tracks id.
func recordTrack(t *testing.T, s *store.Store, ctx context.Context, path, digest string) int64 {
	t.Helper()
	mediaID, err := s.EnsureVaultMedia(ctx, store.VaultMediaRecord{
		MediaType:  "movie",
		Title:      "Fixture",
		Year:       2001,
		FolderPath: filepath.Dir(path),
	})
	if err != nil {
		t.Fatalf("EnsureVaultMedia: %v", err)
	}
	id, err := s.RecordVaultTrack(ctx, store.VaultTrackRecord{
		VaultMediaID: mediaID,
		GroupUUID:    "group-fixture",
		LangISO2:     "pl",
		LangISO3:     "pol",
		Variant:      "dubbing",
		FilePath:     path,
		SHA256:       digest,
		Codec:        "eac3",
		Channels:     "5.1",
		ChannelCount: 6,
	})
	if err != nil {
		t.Fatalf("RecordVaultTrack: %v", err)
	}
	return id
}

type fixture struct {
	store    *store.Store
	ctx      context.Context
	stager   *Stager
	vaultDir string
	workDir  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	vaultDir := filepath.Join(root, "vault")
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		t.Fatalf("create vault dir: %v", err)
	}
	s, err := store.Open(filepath.Join(root, "data"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	stager, err := New(workDir, s)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &fixture{store: s, ctx: context.Background(), stager: stager, vaultDir: vaultDir, workDir: workDir}
}

func jobDir(workDir string, id int64) string {
	return filepath.Join(workDir, "jobs", strconv.FormatInt(id, 10))
}

func TestStageCopiesAndVerifiesTracks(t *testing.T) {
	f := newFixture(t)
	targetPath, targetSHA := writeVaultFile(t, f.vaultDir, "Movie (2001).pl.mka", "target-audio-bytes")
	refPath, refSHA := writeVaultFile(t, f.vaultDir, "Movie (2001).en.mka", "reference-bytes")
	targetID := recordTrack(t, f.store, f.ctx, targetPath, targetSHA)
	refID := recordTrack(t, f.store, f.ctx, refPath, refSHA)

	before := snapshotVault(t, f.vaultDir)
	staging, err := f.stager.Stage(f.ctx, Job{ID: 42, TargetTrackID: targetID, ReferenceTrackIDs: []int64{refID}})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if staging.JobID != 42 || staging.Dir != jobDir(f.workDir, 42) {
		t.Fatalf("staging dir = %q (job %d), want %q", staging.Dir, staging.JobID, jobDir(f.workDir, 42))
	}
	for _, tc := range []struct {
		name string
		got  StagedTrack
		src  string
		sha  string
		data string
	}{
		{"target", staging.Target, targetPath, targetSHA, "target-audio-bytes"},
		{"reference", staging.References[0], refPath, refSHA, "reference-bytes"},
	} {
		if tc.got.SourcePath != tc.src {
			t.Errorf("%s source = %q, want %q", tc.name, tc.got.SourcePath, tc.src)
		}
		if tc.got.SHA256 != tc.sha {
			t.Errorf("%s sha = %q, want %q", tc.name, tc.got.SHA256, tc.sha)
		}
		data, err := os.ReadFile(tc.got.Path)
		if err != nil {
			t.Fatalf("read staged %s: %v", tc.name, err)
		}
		if string(data) != tc.data {
			t.Errorf("%s staged bytes = %q, want %q", tc.name, data, tc.data)
		}
		if tc.got.Size != int64(len(tc.data)) {
			t.Errorf("%s size = %d, want %d", tc.name, tc.got.Size, len(tc.data))
		}
		info, err := os.Stat(tc.got.Path)
		if err != nil {
			t.Fatalf("stat staged %s: %v", tc.name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", tc.name, info.Mode().Perm())
		}
	}
	assertVaultUnchanged(t, before, snapshotVault(t, f.vaultDir))

	if err := f.stager.Cleanup(42); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(staging.Dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("job dir still present after Cleanup: %v", err)
	}
	if err := f.stager.Cleanup(42); err != nil {
		t.Fatalf("second Cleanup = %v, want nil", err)
	}
	assertVaultUnchanged(t, before, snapshotVault(t, f.vaultDir))
}

func TestStageChecksumMismatchWipesStagingAndLeavesVaultAlone(t *testing.T) {
	f := newFixture(t)
	path, _ := writeVaultFile(t, f.vaultDir, "Movie (2001).pl.mka", "corrupt-me")
	id := recordTrack(t, f.store, f.ctx, path, sha256Hex("different-bytes"))

	before := snapshotVault(t, f.vaultDir)
	_, err := f.stager.Stage(f.ctx, Job{ID: 7, TargetTrackID: id})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Stage error = %v, want ErrChecksumMismatch", err)
	}
	var checksum *ChecksumError
	if !errors.As(err, &checksum) {
		t.Fatalf("error %v is not *ChecksumError", err)
	}
	if checksum.TrackID != id || checksum.Want != sha256Hex("different-bytes") || checksum.Got != sha256Hex("corrupt-me") {
		t.Errorf("ChecksumError = %+v", checksum)
	}
	if _, err := os.Stat(jobDir(f.workDir, 7)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("failed staging was not wiped: %v", err)
	}
	assertVaultUnchanged(t, before, snapshotVault(t, f.vaultDir))
}

func TestStageMissingVaultFileWipesStaging(t *testing.T) {
	f := newFixture(t)
	missing := filepath.Join(f.vaultDir, "Absent.pl.mka")
	id := recordTrack(t, f.store, f.ctx, missing, sha256Hex("whatever"))

	before := snapshotVault(t, f.vaultDir)
	_, err := f.stager.Stage(f.ctx, Job{ID: 9, TargetTrackID: id})
	if !errors.Is(err, ErrVaultFileMissing) {
		t.Fatalf("Stage error = %v, want ErrVaultFileMissing", err)
	}
	if _, err := os.Stat(jobDir(f.workDir, 9)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("failed staging was not wiped: %v", err)
	}
	assertVaultUnchanged(t, before, snapshotVault(t, f.vaultDir))
}

func TestStageRejectsUnknownTrack(t *testing.T) {
	f := newFixture(t)
	_, err := f.stager.Stage(f.ctx, Job{ID: 11, TargetTrackID: 4242})
	if !errors.Is(err, ErrTrackNotFound) {
		t.Fatalf("Stage error = %v, want ErrTrackNotFound", err)
	}
	if _, err := os.Stat(jobDir(f.workDir, 11)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("failed staging was not wiped: %v", err)
	}
}

func TestStageValidatesJob(t *testing.T) {
	f := newFixture(t)
	for _, job := range []Job{
		{ID: 0, TargetTrackID: 1},
		{ID: -1, TargetTrackID: 1},
		{ID: 1, TargetTrackID: 0},
		{ID: 1, TargetTrackID: 1, ReferenceTrackIDs: []int64{-2}},
	} {
		if _, err := f.stager.Stage(f.ctx, job); !errors.Is(err, ErrInvalidJob) {
			t.Errorf("Stage(%+v) error = %v, want ErrInvalidJob", job, err)
		}
	}
	if err := f.stager.Cleanup(0); !errors.Is(err, ErrInvalidJob) {
		t.Errorf("Cleanup(0) = %v, want ErrInvalidJob", err)
	}
}

func TestNewValidatesWorkDirAndStore(t *testing.T) {
	if _, err := New("   ", nil); !errors.Is(err, ErrNoWorkDir) {
		t.Fatalf("New(empty) = %v, want ErrNoWorkDir", err)
	}
	if _, err := New(t.TempDir(), nil); err == nil {
		t.Fatal("New with nil track reader succeeded")
	}
}

func TestStageRefusesExistingJobDir(t *testing.T) {
	f := newFixture(t)
	path, digest := writeVaultFile(t, f.vaultDir, "Movie (2001).pl.mka", "target")
	id := recordTrack(t, f.store, f.ctx, path, digest)
	dir := jobDir(f.workDir, 5)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("seed job dir: %v", err)
	}
	marker := filepath.Join(dir, "active-job")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	if _, err := f.stager.Stage(f.ctx, Job{ID: 5, TargetTrackID: id}); !errors.Is(err, ErrStagingExists) {
		t.Fatalf("Stage error = %v, want ErrStagingExists", err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
		t.Fatalf("existing job dir was disturbed: data=%q err=%v", data, err)
	}
}

func TestReapStaleRemovesOldJobsOnly(t *testing.T) {
	f := newFixture(t)
	path, digest := writeVaultFile(t, f.vaultDir, "Movie (2001).pl.mka", "target")
	id := recordTrack(t, f.store, f.ctx, path, digest)

	for _, jobID := range []int64{1, 2} {
		if _, err := f.stager.Stage(f.ctx, Job{ID: jobID, TargetTrackID: id}); err != nil {
			t.Fatalf("Stage(%d): %v", jobID, err)
		}
	}
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(jobDir(f.workDir, 1), old, old); err != nil {
		t.Fatalf("age job 1: %v", err)
	}

	if err := f.stager.ReapStale(now.Add(-time.Hour)); err != nil {
		t.Fatalf("ReapStale: %v", err)
	}
	if _, err := os.Stat(jobDir(f.workDir, 1)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale job 1 remains: %v", err)
	}
	if _, err := os.Stat(jobDir(f.workDir, 2)); err != nil {
		t.Fatalf("recent job 2 was removed: %v", err)
	}
}
