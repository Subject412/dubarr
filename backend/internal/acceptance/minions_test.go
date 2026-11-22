package acceptance

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/arrrescan"
	"github.com/Subject412/dubarr/backend/internal/mux"
	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/store"
	syncengine "github.com/Subject412/dubarr/backend/internal/sync"
	"github.com/Subject412/dubarr/backend/internal/syncjob"
	"github.com/Subject412/dubarr/backend/internal/syncworker"
	"github.com/Subject412/dubarr/backend/internal/vault"
)

// TestMinionsLoudFail is the M5-5 ambiguity path over copies of the Minions
// fixtures. The donor is a Polish dub whose only audio track is tagged `und`,
// so it cannot be auto-exported and is archived manually (M3 manual pick), and
// two Polish vault tracks (dubbing + lektor) are queued against the same AV1
// target. The target is English-only, so no shared reference language exists:
// the analysis falls back to lowpass and/or the pairing is ambiguous. Either
// way the worker must fail loudly with a typed reason *before* the first
// write: the target, its directory and the vault stay byte-identical.
func TestMinionsLoudFail(t *testing.T) {
	requireAcceptanceTools(t)
	h := newHarness(t)
	const secretMarker = "sk-acceptance-DO-NOT-LOG-m1nions"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	target := h.copyFixture(t, minionsTargetName, h.media)
	donor := h.copyFixture(t, minionsDonorName, h.donors)
	instanceID := h.insertInstance(t, "http://127.0.0.1:1", false)
	libraryID := h.insertLibraryRow(t, instanceID, "Minions and Monsters", 2026, "en", target)

	targetSHA := sha256File(t, target)

	// Manual pick: stage the untagged AC3 as Polish, then archive it via the
	// real vault writer. Two variants reproduce the dubbing/lektor ambiguity.
	dubbing := h.stageManualPick(t, donor, "dubbing.mka")
	lektor := h.stageManualPick(t, donor, "lektor.mka")
	polDubbing := h.archive(t, vault.Request{
		StagedPath: dubbing.StagedPath, BaseName: "Minions and Monsters (2026)",
		Language: "pl", Variant: "dubbing",
		Media:     store.VaultMediaRecord{MediaType: "movie", Title: "Minions and Monsters", Year: 2026, OriginalLanguageISO2: "en"},
		GroupUUID: "minions-donor", Codec: dubbing.Track.Codec, Channels: "2.0", ChannelCount: dubbing.Track.Channels,
		DurationMS: dubbing.DurationMS, SourceReleaseName: filepath.Base(donor),
	})
	polLektor := h.archive(t, vault.Request{
		StagedPath: lektor.StagedPath, BaseName: "Minions and Monsters (2026) lektor",
		Language: "pl", Variant: "lektor",
		Media:     store.VaultMediaRecord{MediaType: "movie", Title: "Minions and Monsters", Year: 2026, OriginalLanguageISO2: "en"},
		GroupUUID: "minions-donor", Codec: lektor.Track.Codec, Channels: "2.0", ChannelCount: lektor.Track.Channels,
		DurationMS: lektor.DurationMS, SourceReleaseName: filepath.Base(donor),
	})
	vaultSHAs := map[string]string{
		polDubbing.Path: sha256File(t, polDubbing.Path),
		polLektor.Path:  sha256File(t, polLektor.Path),
	}

	// Two queued tracks for one target: an ambiguous pairing by construction.
	if _, err := h.st.CreateSyncJob(ctx, store.SyncJobCreate{
		VaultTrackID: polDubbing.ID, TargetLibraryCacheID: libraryID,
		TargetVideoPath: target, TargetLangISO2: "pl",
	}); err != nil {
		t.Fatalf("CreateSyncJob(dubbing): %v", err)
	}
	second, err := h.st.CreateSyncJob(ctx, store.SyncJobCreate{
		VaultTrackID: polLektor.ID, TargetLibraryCacheID: libraryID,
		TargetVideoPath: target, TargetLangISO2: "pl",
	})
	if err != nil {
		t.Fatalf("CreateSyncJob(lektor): %v", err)
	}

	engine, err := syncengine.New(syncBinary, syncengine.WithTimeout(analyze.JobTimeout(analyze.DefaultConfig())))
	if err != nil {
		t.Fatalf("sync.New: %v", err)
	}
	jobSvc := syncjob.RegisterRoutes(http.NewServeMux(), syncjob.Dependencies{
		DB: h.st.DB, Settings: h.ss, WorkDir: h.work, Engine: engine,
	})
	rescanner, err := arrrescan.New(arrrescan.Deps{DB: h.st.DB, Secrets: fakeDecrypter{marker: secretMarker}})
	if err != nil {
		t.Fatalf("arrrescan.New: %v", err)
	}
	worker, err := syncworker.New(syncworker.Dependencies{
		Store: h.st, Settings: h.ss, Executor: jobSvc,
		Muxer: mux.New(mux.Dependencies{}), Rescanner: rescanner,
		WorkDir: h.work, Interval: time.Millisecond, Logf: h.logs.printf,
	})
	if err != nil {
		t.Fatalf("syncworker.New: %v", err)
	}
	if processed, err := worker.ProcessOnce(ctx); err != nil || !processed {
		t.Fatalf("syncworker.ProcessOnce = %v/%v, want true/nil", processed, err)
	}

	// The job failed loudly with a typed reason and nothing was published.
	firstID := 1
	job, ok, err := h.st.SyncJobByID(ctx, int64(firstID))
	if err != nil || !ok {
		t.Fatalf("SyncJobByID = %v/%v", ok, err)
	}
	if job.Status != "failed" {
		t.Fatalf("job status = %q (%s), want failed", job.Status, job.ErrorMessage)
	}
	reason := job.ErrorMessage
	t.Logf("minions job failed with typed reason: %s", reason)
	typed := strings.Contains(reason, syncworker.ErrUnmuxableVerdict.Error()) ||
		strings.Contains(reason, mux.ErrAmbiguous.Error())
	if !typed {
		t.Fatalf("failure %q is not a typed unmuxable/ambiguous reason; logs:\n%s", reason, h.logs.String())
	}

	// Zero target writes: same bytes, no backup, no temp sibling in the folder.
	if got := sha256File(t, target); got != targetSHA {
		t.Fatalf("target bytes changed on a loud-fail path: %s → %s", targetSHA, got)
	}
	if _, err := os.Stat(target + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("loud-fail produced a .bak (stat err = %v)", err)
	}
	entries, err := os.ReadDir(h.media)
	if err != nil {
		t.Fatalf("read media dir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("media dir has %v, want only the target (no partial mux)", names)
	}
	if got := audioStreamTags(t, target); len(got) != 1 {
		t.Fatalf("target audio streams = %v, want the untouched single eng track", got)
	}

	// Zero vault modification and no leftover staging for the failed job.
	for path, want := range vaultSHAs {
		if got := sha256File(t, path); got != want {
			t.Fatalf("vault track %s changed: %s → %s", path, want, got)
		}
	}
	if _, err := os.Stat(filepath.Join(h.work, "jobs", "1")); !os.IsNotExist(err) {
		t.Fatalf("failed job left staging (stat err = %v)", err)
	}

	// The second job is untouched: no partial chain, still queued.
	other, ok, err := h.st.SyncJobByID(ctx, second.ID)
	if err != nil || !ok {
		t.Fatalf("SyncJobByID(second) = %v/%v", ok, err)
	}
	if other.Status != "queued" {
		t.Fatalf("second job status = %q, want queued (no partial mux)", other.Status)
	}

	assertNoSecrets(t, secretMarker, h.logs.String())
}

type stagedPick struct {
	StagedPath string
	DurationMS int64
	Track      probe.AudioTrack
}

// stageManualPick extracts the donor's only (und) audio stream into a staged
// .mka under WORK_DIR/extract, tagged Polish, as the M3 manual pick would.
func (h *harness) stageManualPick(t *testing.T, donor, name string) stagedPick {
	t.Helper()
	dir := filepath.Join(h.work, "extract", "manual")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	out := filepath.Join(dir, name)
	cmd := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-y", "-i", donor,
		"-map", "0:a:0", "-c:a", "copy", "-metadata:s:a:0", "language=pol", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg extract %s: %v\n%s", name, err, b)
	}
	info, err := probe.New(probe.Dependencies{}).Probe(context.Background(), out)
	if err != nil {
		t.Fatalf("probe %s: %v", name, err)
	}
	if len(info.Tracks) != 1 {
		t.Fatalf("staged %s audio tracks = %d, want 1", name, len(info.Tracks))
	}
	return stagedPick{StagedPath: out, DurationMS: info.DurationMS, Track: info.Tracks[0]}
}

// archive runs the real vault writer and returns the archived row.
func (h *harness) archive(t *testing.T, req vault.Request) vaultTrack {
	t.Helper()
	entry, err := h.writer.Write(context.Background(), req)
	if err != nil {
		t.Fatalf("vault.Write(%s): %v", req.BaseName, err)
	}
	return vaultTrack{ID: entry.ID, LangISO2: entry.LanguageISO2, Path: entry.Path, SHA256: entry.SHA256}
}
