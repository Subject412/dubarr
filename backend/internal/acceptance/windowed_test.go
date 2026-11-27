package acceptance

// U-WIN.8 — parity proof: windowed decoding changes only the cost.
//
// TestWindowedParity runs one real sync job per mode on each film fixture —
// once with analysis_source=full (both tracks decoded whole, Config.Slicer ==
// nil) and once with analysis_source=windows — through the same Execute the
// worker calls, and asserts the two reports are the same report:
//
//   - identical verdict and classification;
//   - per checkpoint |Δoffset| ≤ windowedCrossPathOffsetMS (1.5 ms, one
//     container timestamp tick) and |Δquality| ≤ 0.02;
//   - |Δmax_drift| ≤ 1 ms;
//   - the same checkpoint plan (position, slice start within one sample, slice
//     length exactly), which is what "identical checkpoints_json apart from
//     float noise" means once the noise is bounded instead of hand-waved.
//
// The step is printed as a table — fixture, checkpoint, full, windowed, delta —
// so a human can read every number without a debugger.
//
// Two further parity claims live where the unexported primitives are:
// TestWindowedGoldenParityOnBothFixtures reproduces all 12 committed goldens
// through the windowed path, and TestWindowedFallbackReproducesFullReport
// asserts a forced fallback returns the full-decode report exactly (both in
// internal/syncjob/window_parity_test.go).
//
// # Mutations this is meant to catch
//
// Forcing the seek correction to +0 (dropping start_time from the plan) turns
// the start_time fixture red and this Shrek comparison red: every Shrek window
// then reads audio from the wrong place and the offsets move by hundreds of
// samples. Shortening the pre-roll to 0.05 s turns the quality bound red on at
// least one checkpoint, because the first samples of each window still carry
// the band filter's settling transient. Both are recorded, not applied: this
// unit must not touch production code.
//
// # Skipping
//
// testfiles/ is tens of GiB of real media and is not in the repository, so this
// file skips — naming testfiles/ — when the fixtures are absent. The full
// comparison *needs* the media and is allowed to skip; it must never be
// "fixed" by deleting the comparison. The gate that cannot skip is
// internal/dsp/correlate's golden test over the committed excerpts (see
// requireAcceptanceTools).

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/reflang"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
	syncengine "github.com/Subject412/dubarr/backend/internal/sync"
	"github.com/Subject412/dubarr/backend/internal/syncjob"
	"github.com/Subject412/dubarr/backend/internal/vault"
)

// windowedCrossPathOffsetMS bounds the offset difference between the two modes
// on one pair of sources.
//
// It is not one sample, and the reason is the container rather than the code:
// Matroska timestamps are quantised to a millisecond, so a seek can only name
// a position to that resolution while a whole-track decode is sample-continuous
// from the first sample. Measured on the Minions pair (4K MKV target, mp4
// donor): the windowed report sits 1.000 ms high at every one of the six
// checkpoints, with |Δquality| 0.000, |Δmax_drift| 0.000 and the same verdict —
// a constant bias, not drift, which is why only this bound moves. The Shrek
// pair, whose target is also MKV, agrees to 0.000 ms, so the effect is
// per-source and bounded by one container tick, not a general seek error.
//
// Everything that can stay exact does stay exact: the slice start keeps the
// one-sample bound, and quality, drift and verdict keep theirs.
const windowedCrossPathOffsetMS = 1.5

// windowedFixture names one film's analysis inputs and what the reference
// resolver must make of them.
type windowedFixture struct {
	name         string
	target       string
	donor        string
	title        string
	year         int
	originalLang string
	// dubSelector picks the donor-group track the job synchronises (the track
	// that gets muxed); refSelector picks the sibling it is correlated
	// against. Empty refSelector documents the low-pass pair: the donor carries
	// one Polish track and the target is English-only, so no shared reference
	// language exists.
	dubSelector string
	refSelector string
	method      reflang.Method
}

func windowedFixtures() []windowedFixture {
	return []windowedFixture{
		{
			name: "shrek", target: shrekTargetName, donor: shrekDonorName,
			title: "Shrek", year: 2001, originalLang: "en",
			dubSelector: "0:a:0", refSelector: "0:a:1", method: reflang.SameLangReference,
		},
		{
			name: "minions", target: minionsTargetName, donor: minionsDonorName,
			title: "Minions and Monsters", year: 2026, originalLang: "en",
			dubSelector: "0:a:0", method: reflang.LowpassFallback,
		},
	}
}

// windowedRun is one mode's job: what it recorded, what it reported, and what
// it cost.
type windowedRun struct {
	jobID      int64
	source     string
	fallback   string
	report     analyze.Report
	elapsed    time.Duration
	decodedSec float64
	seeks      int
}

// TestWindowedParity is proof 2: whole-job equivalence on both fixtures.
//
// It is opt-in (DUBARR_PARITY=1) because it runs four whole jobs over the real
// films and takes about five minutes, which does not belong in `go test ./...`.
func TestWindowedParity(t *testing.T) {
	requireAcceptanceTools(t)
	if os.Getenv("DUBARR_PARITY") != "1" {
		t.Skip("real-film parity proof: set DUBARR_PARITY=1 to run it")
	}
	for _, fx := range windowedFixtures() {
		fx := fx
		t.Run(fx.name, func(t *testing.T) {
			runWindowedParity(t, fx)
		})
	}
}

func runWindowedParity(t *testing.T, fx windowedFixture) {
	t.Helper()
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	target := h.copyFixture(t, fx.target, h.media)
	donor := h.copyFixture(t, fx.donor, h.donors)
	targetSHA := sha256File(t, target)
	instanceID := h.insertInstance(t, "http://127.0.0.1:1", false)
	libraryID := h.insertLibraryRow(t, instanceID, fx.title, fx.year, fx.originalLang, target)

	// The donor group, archived exactly as a manual pick plus the vault writer
	// leaves it. The dub is the track the job synchronises; the reference is
	// the same-language sibling when the fixture has one.
	baseName := fmt.Sprintf("%s (%d)", fx.title, fx.year)
	media := store.VaultMediaRecord{
		MediaType: "movie", Title: fx.title, Year: fx.year, OriginalLanguageISO2: fx.originalLang,
	}
	group := "parity-" + fx.name
	dubPick := h.stageDonorStream(t, donor, fx.dubSelector, "pl", fx.name+"-dub.mka")
	dub := h.archive(t, vault.Request{
		StagedPath: dubPick.StagedPath, BaseName: baseName, Language: "pl", Variant: "dubbing",
		Media: media, GroupUUID: group, Codec: dubPick.Track.Codec, Channels: channelsLabel(dubPick.Track),
		ChannelCount: dubPick.Track.Channels, DurationMS: dubPick.DurationMS,
		SourceReleaseName: filepath.Base(donor),
	})
	vaultTracks := []probe.AudioTrack{{Index: 1, Channels: dubPick.Track.Channels, Language: "pl", LanguageISO1: "pl"}}
	donorDur := time.Duration(dubPick.DurationMS) * time.Millisecond
	if fx.refSelector != "" {
		refPick := h.stageDonorStream(t, donor, fx.refSelector, "eng", fx.name+"-ref.mka")
		h.archive(t, vault.Request{
			StagedPath: refPick.StagedPath, BaseName: baseName, Language: "en", Variant: "original",
			Media: media, GroupUUID: group, Codec: refPick.Track.Codec, Channels: channelsLabel(refPick.Track),
			ChannelCount: refPick.Track.Channels, DurationMS: refPick.DurationMS,
			SourceReleaseName: filepath.Base(donor),
		})
		vaultTracks = append(vaultTracks, probe.AudioTrack{Index: 2, Channels: refPick.Track.Channels, Language: "eng", LanguageISO1: "en"})
		// The reference track is the analysis input on the donor side; the dub
		// is only muxed.
		donorDur = time.Duration(refPick.DurationMS) * time.Millisecond
	}

	// The pair the job will resolve, resolved here from the same persisted
	// policy so the table states the band and the target stream whose duration
	// the whole-track decode reads.
	policy := h.languagesPolicy(t, ctx)
	targetInfo, err := probe.New(probe.Dependencies{}).Probe(ctx, target)
	if err != nil {
		t.Fatalf("probe target: %v", err)
	}
	resolved := reflang.Resolve(targetInfo.Tracks, vaultTracks, policy, fx.originalLang)
	if len(resolved.Pairs) == 0 {
		t.Fatalf("reference resolution: %s", resolved.Reason)
	}
	if got := resolved.Pairs[0].Method; got != fx.method {
		t.Fatalf("fixture resolves to %s, this test documents %s (%s)", got, fx.method, resolved.Pairs[0].Reason)
	}
	targetDur, ok := targetInfo.TrackDuration(resolved.Pairs[0].Target.Index)
	if !ok {
		t.Fatalf("target stream %d states no duration, so the whole-track cost cannot be stated", resolved.Pairs[0].Target.Index)
	}

	engine := syncengine.New(syncengine.WithTimeout(testJobTimeout))
	jobSvc := syncjob.RegisterRoutes(http.NewServeMux(), syncjob.Dependencies{
		DB: h.st.DB, Settings: h.ss, WorkDir: h.work,
		Engine: budgetEngine{inner: engine},
		// The mode decision's own log line is part of the measurement
		// protocol ("which mode did this job use?"), so route it to the
		// capture the assertions read.
		Logger: slog.New(slog.NewTextHandler(h.logs, nil)),
	})

	full := h.runParityJob(t, ctx, jobSvc, target, libraryID, dub.ID, analyze.AnalysisSourceFull)
	full.decodedSec = (targetDur + donorDur).Seconds()
	windows := h.runParityJob(t, ctx, jobSvc, target, libraryID, dub.ID, analyze.AnalysisSourceWindows)
	// The slicer's counter: both sides, every window, its pre-roll and its
	// slice. TestDecodedSecondsCounter pins that it equals exactly this
	// arithmetic for a six-checkpoint plan.
	for _, cp := range windows.report.Checkpoints {
		windows.decodedSec += 2 * (syncjob.WindowPrerollSec + cp.SliceLenSec)
	}
	windows.seeks = 2 * len(windows.report.Checkpoints)

	// The mode the job recorded is the mode the setting asked for, and the
	// windowed job did not have to fall back: no suspect window was forced.
	if full.source != analyze.AnalysisSourceFull || full.fallback != "" {
		t.Fatalf("full job recorded analysis_source=%q fallback=%q, want full/\"\"", full.source, full.fallback)
	}
	if windows.source != analyze.AnalysisSourceWindows || windows.fallback != "" {
		t.Fatalf("windowed job recorded analysis_source=%q fallback=%q, want windows/\"\" (no forced fallback); logs:\n%s",
			windows.source, windows.fallback, h.logs.String())
	}
	if !strings.Contains(h.logs.String(), "analysis_source=windows") {
		t.Fatalf("the job never logged its windowed mode decision; logs:\n%s", h.logs.String())
	}
	if got := windows.report.SyncMethod; got != fx.method {
		t.Fatalf("windowed sync_method = %q, want %q", got, fx.method)
	}

	// Same report: verdict, classification and spread first, then the rows.
	if full.report.Verdict != windows.report.Verdict {
		t.Fatalf("verdict full=%q (%s) windows=%q (%s)", full.report.Verdict, full.report.Reason,
			windows.report.Verdict, windows.report.Reason)
	}
	if full.report.Classification != windows.report.Classification {
		t.Fatalf("classification full=%q windows=%q", full.report.Classification, windows.report.Classification)
	}
	if d := math.Abs(full.report.MaxDriftMS - windows.report.MaxDriftMS); d > 1 {
		t.Fatalf("max_drift full=%v ms windows=%v ms, |Δ|=%v ms > 1 ms",
			full.report.MaxDriftMS, windows.report.MaxDriftMS, d)
	}
	if len(full.report.Checkpoints) != len(windows.report.Checkpoints) {
		t.Fatalf("checkpoint rows full=%d windows=%d", len(full.report.Checkpoints), len(windows.report.Checkpoints))
	}

	// One 8 kHz sample. The plan's duration base is derived from the containers
	// in windows mode and from the staged WAV headers in full mode, so a slice
	// start may land one sample apart; the offsets they produce may not.
	const startBoundSec = 1.0 / 8000.0
	table := []string{fmt.Sprintf("%-8s %4s %11s %11s %9s %7s %7s %7s",
		"fixture", "pos", "full_ms", "win_ms", "d_ms", "full_q", "win_q", "d_q")}
	var worstOffset, worstQuality, worstStart float64
	for i := range full.report.Checkpoints {
		f, w := full.report.Checkpoints[i], windows.report.Checkpoints[i]
		if f.PositionPct != w.PositionPct {
			t.Fatalf("row %d position full=%d%% windows=%d%% (report order changed)", i, f.PositionPct, w.PositionPct)
		}
		if d := math.Abs(f.SliceLenSec - w.SliceLenSec); d != 0 {
			t.Fatalf("checkpoint %d%% slice length full=%v s windows=%v s (the plan must be shared)", f.PositionPct, f.SliceLenSec, w.SliceLenSec)
		}
		dStart := math.Abs(f.SliceStartSec - w.SliceStartSec)
		dOffset := w.OffsetMS - f.OffsetMS
		dQuality := w.Quality - f.Quality
		if dStart > worstStart {
			worstStart = dStart
		}
		if math.Abs(dOffset) > worstOffset {
			worstOffset = math.Abs(dOffset)
		}
		if math.Abs(dQuality) > worstQuality {
			worstQuality = math.Abs(dQuality)
		}
		table = append(table, fmt.Sprintf("%-8s %4d %11.3f %11.3f %+9.3f %7.3f %7.3f %+7.3f",
			fx.name, f.PositionPct, f.OffsetMS, w.OffsetMS, dOffset, f.Quality, w.Quality, dQuality))

		if dStart > startBoundSec {
			t.Errorf("checkpoint %d%%: slice start full=%.6f s windows=%.6f s, |Δ|=%.6f s > one sample",
				f.PositionPct, f.SliceStartSec, w.SliceStartSec, dStart)
		}
		if math.Abs(dOffset) > windowedCrossPathOffsetMS {
			t.Errorf("checkpoint %d%%: offset full=%.3f ms windows=%.3f ms, |Δ|=%.3f ms > cross-path bound (%.3f ms, container timestamp granularity)",
				f.PositionPct, f.OffsetMS, w.OffsetMS, math.Abs(dOffset), windowedCrossPathOffsetMS)
		}
		if math.Abs(dQuality) > 0.02 {
			t.Errorf("checkpoint %d%%: quality full=%.3f windows=%.3f, |Δ|=%.3f > 0.02",
				f.PositionPct, f.Quality, w.Quality, math.Abs(dQuality))
		}
	}
	t.Logf("windowed parity (%s, %s band, verdict %q, %d checkpoints, pre-roll %.1fs):\n%s\nworst |Δoffset| %.3f ms (bound %.3f), worst |Δquality| %.3f (bound 0.02), worst |Δstart| %.6f s (bound %.6f), |Δmax_drift| %.3f ms (bound 1.000)\ncost: full %s over %.0f decoded s / 0 seeks; windows %s over %.0f decoded s / %d seeks",
		fx.name, fx.method, full.report.Verdict, len(full.report.Checkpoints), syncjob.WindowPrerollSec,
		strings.Join(table, "\n"),
		worstOffset, windowedCrossPathOffsetMS, worstQuality, worstStart, startBoundSec,
		math.Abs(full.report.MaxDriftMS-windows.report.MaxDriftMS),
		full.elapsed.Round(time.Millisecond), full.decodedSec,
		windows.elapsed.Round(time.Millisecond), windows.decodedSec, windows.seeks)

	// Analysis only: nothing was muxed, so the target is byte-identical and no
	// backup/temp sibling appeared beside it.
	if got := sha256File(t, target); got != targetSHA {
		t.Fatalf("target bytes changed during analysis-only runs: %s → %s", targetSHA, got)
	}
	if _, err := os.Stat(target + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("analysis-only runs produced a .bak beside the target (stat err = %v)", err)
	}
	assertNoSecrets(t, "", h.logs.String())
}

// runParityJob flips the setting, queues one job and executes its analysis
// phase, returning what the job recorded and reported. Execute never muxes, so
// the two runs see the same target bytes.
func (h *harness) runParityJob(t *testing.T, ctx context.Context, svc *syncjob.Service,
	target string, libraryID, vaultTrackID int64, source string) windowedRun {
	t.Helper()
	h.setAnalysisSource(t, source)
	rec, err := h.st.CreateSyncJob(ctx, store.SyncJobCreate{
		VaultTrackID: vaultTrackID, TargetLibraryCacheID: libraryID,
		TargetVideoPath: target, TargetLangISO2: "pl",
	})
	if err != nil {
		t.Fatalf("CreateSyncJob(%s): %v", source, err)
	}
	start := time.Now()
	if _, err := svc.Execute(ctx, rec.ID); err != nil {
		t.Fatalf("%s analysis of job %d: %v; logs:\n%s", source, rec.ID, err, h.logs.String())
	}
	elapsed := time.Since(start)
	stored, ok, err := h.st.SyncJobByID(ctx, rec.ID)
	if err != nil || !ok {
		t.Fatalf("SyncJobByID(%d) = %v/%v", rec.ID, ok, err)
	}
	if stored.Status != "completed" {
		t.Fatalf("%s job %d status = %q (%s); logs:\n%s", source, rec.ID, stored.Status, stored.ErrorMessage, h.logs.String())
	}
	var report analyze.Report
	if err := json.Unmarshal([]byte(stored.CheckpointsJSON), &report); err != nil {
		t.Fatalf("decode persisted report of job %d: %v", rec.ID, err)
	}
	if len(report.Checkpoints) == 0 {
		t.Fatalf("%s job %d persisted no checkpoints", source, rec.ID)
	}
	return windowedRun{
		jobID: rec.ID, source: stored.AnalysisSource, fallback: stored.AnalysisFallback,
		report: report, elapsed: elapsed,
	}
}

// setAnalysisSource writes the setting the job reads, through the validated
// store call the API uses.
func (h *harness) setAnalysisSource(t *testing.T, source string) {
	t.Helper()
	ctx := context.Background()
	raw, ok, err := h.ss.Get(ctx, settings.KeySyncDefaults)
	if err != nil {
		t.Fatalf("read sync defaults: %v", err)
	}
	var sd settings.SyncDefaults
	if ok {
		if err := json.Unmarshal(raw, &sd); err != nil {
			t.Fatalf("decode sync defaults: %v", err)
		}
	} else {
		sd = settings.Defaults("")[settings.KeySyncDefaults].(settings.SyncDefaults)
	}
	sd.AnalysisSource = source
	out, err := json.Marshal(sd)
	if err != nil {
		t.Fatalf("encode sync defaults: %v", err)
	}
	if err := h.ss.Set(ctx, settings.KeySyncDefaults, out); err != nil {
		t.Fatalf("set analysis_source=%s: %v", source, err)
	}
}

// languagesPolicy projects the persisted policy the way syncjob does, so the
// pair this test documents is the pair the job resolves.
func (h *harness) languagesPolicy(t *testing.T, ctx context.Context) reflang.Languages {
	t.Helper()
	raw, ok, err := h.ss.Get(ctx, settings.KeyLanguagesPolicy)
	if err != nil || !ok {
		t.Fatalf("read languages policy = %v/%v", ok, err)
	}
	var lp settings.LanguagesPolicy
	if err := json.Unmarshal(raw, &lp); err != nil {
		t.Fatalf("decode languages policy: %v", err)
	}
	return reflang.Languages{
		Targets:                 append([]string(nil), lp.TargetImportLanguages...),
		References:              append([]string(nil), lp.ExtraReferenceLanguages...),
		PreferOriginalLanguage:  lp.PreferOriginalLanguage,
		OriginalLanguageFromArr: lp.OriginalLanguageFromArr,
	}
}

// stageDonorStream extracts one audio stream of a donor fixture with a stream
// copy and tags it, which is what a manual pick records (the language is the
// operator's choice, not the container's).
func (h *harness) stageDonorStream(t *testing.T, donor, selector, lang, name string) stagedPick {
	t.Helper()
	dir := filepath.Join(h.work, "extract", "manual")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	out := filepath.Join(dir, name)
	cmd := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-y", "-i", donor,
		"-map", selector, "-c:a", "copy", "-metadata:s:a:0", "language="+lang, out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg extract %s (%s): %v\n%s", name, selector, err, b)
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

// channelsLabel is the "2.0"/"5.1" label the vault writer requires; the
// fixtures' dubs are stereo, the remuxes' references take the channel count
// from the stream.
func channelsLabel(track probe.AudioTrack) string {
	switch track.Channels {
	case 1:
		return "1.0"
	case 2:
		return "2.0"
	case 6:
		return "5.1"
	case 8:
		return "7.1"
	default:
		return fmt.Sprintf("%d.0", track.Channels)
	}
}
