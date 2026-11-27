package mux

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/gate"
	"github.com/Subject412/dubarr/backend/internal/probe"
)

// Synthetic fixtures are built once per test binary with the tools the
// appliance requires anyway (ffmpeg/ffprobe/mkvmerge). They are tiny: a 6 s
// MPEG-4 video plus one PCM English audio track, and a matching 6 s PCM track
// used as the "synced" vault audio.
var (
	fixtureDir    string
	fixtureTarget string
	fixtureAudio  string
	fixtureErr    error
)

func TestMain(m *testing.M) {
	for _, tool := range []string{"ffmpeg", "ffprobe", "mkvmerge"} {
		if _, err := exec.LookPath(tool); err != nil {
			fixtureErr = fmt.Errorf("%s not on PATH: %w", tool, err)
		}
	}
	if fixtureErr == nil {
		fixtureDir, fixtureErr = buildFixtures()
	}
	code := m.Run()
	if fixtureDir != "" {
		_ = os.RemoveAll(fixtureDir)
	}
	os.Exit(code)
}

func buildFixtures() (string, error) {
	dir, err := os.MkdirTemp("", "dubarr-mux-fixture-")
	if err != nil {
		return "", err
	}
	video := filepath.Join(dir, "video.mkv")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=10:duration=6",
		"-c:v", "mpeg4", "-b:v", "200k", "-an", video).CombinedOutput(); err != nil {
		return dir, fmt.Errorf("ffmpeg video fixture: %w: %s", err, out)
	}
	audio := filepath.Join(dir, "audio.wav")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=6",
		"-c:a", "pcm_s16le", audio).CombinedOutput(); err != nil {
		return dir, fmt.Errorf("ffmpeg audio fixture: %w: %s", err, out)
	}
	target := filepath.Join(dir, "target.mkv")
	if out, err := exec.Command("mkvmerge", "-o", target, "--quiet",
		video, "--language", "0:eng", audio).CombinedOutput(); err != nil {
		return dir, fmt.Errorf("mkvmerge target fixture: %w: %s", err, out)
	}
	synced := filepath.Join(dir, "synced.mka")
	if out, err := exec.Command("mkvmerge", "-o", synced, "--quiet",
		"--language", "0:pl", audio).CombinedOutput(); err != nil {
		return dir, fmt.Errorf("mkvmerge audio fixture: %w: %s", err, out)
	}
	fixtureTarget = target
	fixtureAudio = synced
	return dir, nil
}

func requireFixtures(t *testing.T) {
	t.Helper()
	if fixtureErr != nil {
		t.Skipf("media tools unavailable: %v", fixtureErr)
	}
}

// copyFixture copies one synthetic fixture into dir under name.
func copyFixture(t *testing.T, src, dir, name string) string {
	t.Helper()
	dst := filepath.Join(dir, name)
	if err := copyFile(src, dst, 0o644); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	return dst
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// videoStreamHash hashes the compressed video packets only (no container
// metadata), so it proves the video bitstream survived the mux untouched.
func videoStreamHash(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", path, "-map", "0:v", "-c", "copy", "-f", "hash", "-hash", "md5", "-").Output()
	if err != nil {
		t.Fatalf("hash video stream of %s: %v", path, err)
	}
	got := strings.TrimSpace(string(out))
	const emptyMD5 = "MD5=d41d8cd98f00b204e9800998ecf8427e"
	if !strings.HasPrefix(got, "MD5=") || got == emptyMD5 {
		t.Fatalf("video stream hash of %s is empty: %q", path, got)
	}
	return got
}

// tempLeftovers lists scratch files of the form <target>.<tag>-<rand>.tmp still
// sitting beside target. The match is a plain prefix/suffix comparison on the
// directory entry names, never a glob: the target base legitimately contains
// glob metacharacters (e.g. the Tdarr marker "[TDARR]"), so a Glob pattern
// built from it matches nothing and the check silently passes.
func tempLeftovers(target string) ([]string, error) {
	dir, base := filepath.Dir(target), filepath.Base(target)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	prefix, suffix := base+".", ".tmp"
	var matches []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if name := e.Name(); strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) {
			matches = append(matches, filepath.Join(dir, name))
		}
	}
	return matches, nil
}

// noTempsLeft fails when any *.dubarr*.tmp scratch file survived beside target.
func noTempsLeft(t *testing.T, target string) {
	t.Helper()
	matches, err := tempLeftovers(target)
	if err != nil {
		t.Fatalf("list temps: %v", err)
	}
	if len(matches) > 0 {
		t.Errorf("leftover temp files: %v", matches)
	}
}

// TestTempLeftoversCatchesSeededScratch guards the check above from going dead
// again: a seeded scratch file next to a "[TDARR]" target must be reported.
func TestTempLeftoversCatchesSeededScratch(t *testing.T) {
	h := newHarness(t)
	if got, err := tempLeftovers(h.target); err != nil || len(got) != 0 {
		t.Fatalf("tempLeftovers on clean dir = %v, %v; want none", got, err)
	}

	leftover := h.target + ".dubarr-0123456789abcdef.tmp"
	if err := os.WriteFile(leftover, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed leftover temp: %v", err)
	}
	got, err := tempLeftovers(h.target)
	if err != nil {
		t.Fatalf("tempLeftovers: %v", err)
	}
	if len(got) != 1 || got[0] != leftover {
		t.Fatalf("tempLeftovers = %v, want [%s]", got, leftover)
	}
}

// TestStaleTempBesideTargetIsFlagged pins the production cleanup contract: a
// Mux call removes only the temp it created (mux.go:219-235), never a stale
// scratch file left beside the target by an earlier crash. The verification
// helper must nevertheless report that file under its real
// "<target>.dubarr-<hex>.tmp" name, so a leak is visible rather than silently
// passing.
func TestStaleTempBesideTargetIsFlagged(t *testing.T) {
	h := newHarness(t)
	stale := h.target + ".dubarr-0123456789abcdef.tmp"
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed stale temp: %v", err)
	}

	// A failing mux still creates and removes its own temp, leaving the
	// seeded one untouched.
	if _, err := New(Dependencies{MKVMergePath: "/bin/false"}).Mux(context.Background(), h.job()); !errors.Is(err, ErrTool) {
		t.Fatalf("Mux error = %v, want ErrTool", err)
	}

	got, err := tempLeftovers(h.target)
	if err != nil {
		t.Fatalf("tempLeftovers: %v", err)
	}
	if len(got) != 1 || got[0] != stale {
		t.Fatalf("tempLeftovers = %v, want the seeded stale temp [%s]", got, stale)
	}
}

// TestMuxCopyOnWorkStagesVerbatimTarget restores the Copy-on-Work integrity
// assertion lost when issue #23 made Mux remove its own staging: the target is
// copied byte-for-byte into {WORK_DIR}/jobs/{id}/ before mkvmerge reads it.
// The copy only exists between MkdirAll and the deferred RemoveAll, so it is
// observed through the Run hook at the moment mkvmerge is invoked: argv is
// "-o <temp> --quiet <workCopy> ...", which is the only read of those bytes.
func TestMuxCopyOnWorkStagesVerbatimTarget(t *testing.T) {
	h := newHarness(t)
	originalHash := sha256File(t, h.target)
	wantCopy := filepath.Join(h.work, "jobs", "42", "target.mkv")

	var observedPath, observedHash string
	var observedErr error
	run := func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		if filepath.Base(bin) == "mkvmerge" {
			if len(args) < 4 {
				t.Fatalf("mkvmerge argv too short: %v", args)
			}
			observedPath = args[3]
			observedErr = nil
			if _, err := os.Stat(observedPath); err != nil {
				observedErr = err
			} else {
				observedHash = sha256File(t, observedPath)
			}
		}
		return execRun(ctx, bin, args...)
	}

	if _, err := New(Dependencies{Run: run}).Mux(context.Background(), h.job()); err != nil {
		t.Fatalf("Mux: %v", err)
	}

	if observedPath == "" {
		t.Fatal("mkvmerge was never invoked; the staged copy was not observed")
	}
	if observedPath != wantCopy {
		t.Errorf("mkvmerge read %q, want the staged copy at %q", observedPath, wantCopy)
	}
	if observedErr != nil {
		t.Fatalf("staged copy %s at mkvmerge time: %v", observedPath, observedErr)
	}
	if observedHash != originalHash {
		t.Errorf("staged copy sha256 = %s, want the target's %s (Copy-on-Work not verbatim)", observedHash, originalHash)
	}

	// The observed copy was the transient staging, not a surviving artifact.
	if _, err := os.Stat(wantCopy); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staged copy %s still present after Mux returned: %v", wantCopy, err)
	}
}

// snapshot records every regular file under dir with its hash.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out[rel] = sha256File(t, path)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", dir, err)
	}
	return out
}

type probeStream struct {
	Index     int               `json:"index"`
	CodecType string            `json:"codec_type"`
	CodecName string            `json:"codec_name"`
	Language  string            `json:"language"`
	Title     string            `json:"-"`
	Tags      map[string]string `json:"tags"`
	Duration  string            `json:"duration"`
	BitRate   string            `json:"bit_rate"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	NbFrames  string            `json:"nb_frames"`
	Default   int               `json:"-"`
}

type probeDoc struct {
	Streams []probeStream `json:"streams"`
}

func probeFile(t *testing.T, path string) probeDoc {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "stream=index,codec_type,codec_name,width,height,nb_frames,duration,bit_rate:stream_tags=language,title",
		"-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var doc probeDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode ffprobe %s: %v", path, err)
	}
	for i := range doc.Streams {
		doc.Streams[i].Language = doc.Streams[i].Tags["language"]
		doc.Streams[i].Title = doc.Streams[i].Tags["title"]
	}
	return doc
}

func videoOnly(doc probeDoc) []probeStream {
	out := make([]probeStream, 0, 1)
	for _, s := range doc.Streams {
		if s.CodecType == "video" {
			s.Index = 0
			out = append(out, s)
		}
	}
	return out
}

func audioStreams(doc probeDoc) []probeStream {
	out := make([]probeStream, 0, len(doc.Streams))
	for _, s := range doc.Streams {
		if s.CodecType == "audio" {
			s.Index = 0
			out = append(out, s)
		}
	}
	return out
}

func findAudio(doc probeDoc, lang, title string) (probeStream, bool) {
	for _, s := range audioStreams(doc) {
		if probe.ISO639_1(s.Language) == lang && s.Title == title {
			return s, true
		}
	}
	return probeStream{}, false
}

// audioTrackIETF returns the LanguageIETF of the last audio track. mkvmerge
// stores both the legacy code (pol) and the IETF code (pl) from --language 0:pl;
// ffprobe only exposes the legacy tag.
func audioTrackIETF(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("mkvmerge", "-J", path).Output()
	if err != nil {
		t.Fatalf("mkvmerge -J %s: %v", path, err)
	}
	var doc struct {
		Tracks []struct {
			Type       string         `json:"type"`
			Properties map[string]any `json:"properties"`
		} `json:"tracks"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode mkvmerge -J: %v", err)
	}
	ietf := ""
	for _, tr := range doc.Tracks {
		if tr.Type == "audio" {
			if v, ok := tr.Properties["language_ietf"].(string); ok {
				ietf = v
			}
		}
	}
	return ietf
}

func eligibleRule() gate.Rule {
	return gate.Rule{Required: []string{"[TDARR]"}, Mode: gate.MatchAny, Enforce: true}
}

type harness struct {
	t      *testing.T
	dir    string
	work   string
	target string
	audio  string
	id     int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	requireFixtures(t)
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	h := &harness{t: t, dir: dir, work: work, id: 42}
	h.target = copyFixture(t, fixtureTarget, dir, "Movie (2021) [TDARR].mkv")
	jobDir := filepath.Join(work, "jobs", "42")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("mkdir job: %v", err)
	}
	h.audio = copyFixture(t, fixtureAudio, jobDir, "synced.pl.mka")
	return h
}

func (h *harness) job() Job {
	return Job{
		ID:             h.id,
		WorkDir:        h.work,
		TargetPath:     h.target,
		TargetLangISO2: "pl",
		AudioPath:      h.audio,
		SyncOffsetMS:   -750,
		Gate:           eligibleRule(),
		Report:         analyze.Report{Verdict: analyze.VerdictOK, Classification: analyze.ClassificationStable},
		Candidates:     []Candidate{{TrackID: 1, LangISO2: "pl", Variant: "dubbing"}},
	}
}

// TestMuxPublishesAtomically: a successful replace-mode publish is a single
// rename of the validated temp over the target — new inode, no temp and no
// sidecar left beside it. There is no .bak: safety comes from the pre-rename
// validation (ADR-0018), which the failure tests below exercise.
func TestMuxPublishesAtomically(t *testing.T) {
	h := newHarness(t)
	beforeTargetHash := sha256File(t, h.target)
	beforeInfo, err := os.Stat(h.target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	beforeProbe := probeFile(t, h.target)
	beforeVideo := videoStreamHash(t, h.target)

	res, err := New(Dependencies{}).Mux(context.Background(), h.job())
	if err != nil {
		t.Fatalf("Mux: %v", err)
	}

	if res.OutputPath != h.target {
		t.Errorf("OutputPath = %q, want %q", res.OutputPath, h.target)
	}
	if leftovers, err := tempLeftovers(h.target); err != nil {
		t.Fatalf("list temps: %v", err)
	} else if len(leftovers) > 0 {
		t.Errorf("temp files still present beside target: %v", leftovers)
	}
	if _, err := os.Stat(h.target + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".bak created beside the target (err = %v), want none", err)
	}

	// The content at the original path is new; the path was replaced, not
	// edited in place (the old inode is gone).
	if got := sha256File(t, h.target); got == beforeTargetHash {
		t.Error("target content unchanged, mux did not publish")
	}
	afterInfo, err := os.Stat(h.target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if os.SameFile(beforeInfo, afterInfo) {
		t.Error("target inode unchanged, want atomic replacement")
	}

	// Video stream is copied byte-for-byte: the compressed bitstream hash is
	// identical before and after (ffprobe reports N/A nb_frames/bit_rate for
	// these tiny fixtures, so a probe comparison alone is nearly vacuous).
	afterProbe := probeFile(t, h.target)
	if want, got := videoOnly(beforeProbe), videoOnly(afterProbe); !reflect.DeepEqual(got, want) {
		t.Errorf("video stream changed:\n got %+v\nwant %+v", got, want)
	}
	if got, want := videoStreamHash(t, h.target), beforeVideo; got != want {
		t.Errorf("video stream hash = %s, want %s (video was re-encoded)", got, want)
	}

	// Exactly one audio track was appended, tagged with the vault track's ISO2
	// language and the dub marker ("PL DUB").
	if got, want := len(audioStreams(afterProbe)), len(audioStreams(beforeProbe))+1; got != want {
		t.Fatalf("audio tracks = %d, want %d", got, want)
	}
	added, ok := findAudio(afterProbe, "pl", "PL DUB")
	if !ok {
		t.Fatalf("no pl/PL DUB audio track in %+v", audioStreams(afterProbe))
	}
	if ietf := audioTrackIETF(t, h.target); ietf != "pl" {
		t.Errorf("LanguageIETF = %q, want pl", ietf)
	}
	if added.CodecName != "pcm_s16le" {
		t.Errorf("added codec = %q, want pcm_s16le (copied, never transcoded)", added.CodecName)
	}

	// Issue #23: a completed mux leaves no staging behind. The target was
	// copied into the job's temp folder (Copy-on-Work) and that copy lived in
	// the job directory Mux has now removed.
	if _, err := os.Stat(filepath.Join(h.work, "jobs", "42")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("job staging still present after a publish: %v", err)
	}

	// Command shape (ADR-0004): target as input, then the
	// appended audio with language, title, sync and default-flag options.
	joined := strings.Join(res.Command, " ")
	for _, want := range []string{"--language 0:pl", "--track-name 0:PL DUB", "--sync 0:-750", "--default-track-flag 0:no"} {
		if !strings.Contains(joined, want) {
			t.Errorf("command %q missing %q", joined, want)
		}
	}
}

func TestExplicitTrackTitleOverridesDubMarker(t *testing.T) {
	h := newHarness(t)
	job := h.job()
	job.TrackTitle = "Polski Dubbing"
	if _, err := New(Dependencies{}).Mux(context.Background(), job); err != nil {
		t.Fatalf("Mux: %v", err)
	}
	doc := probeFile(t, h.target)
	if _, ok := findAudio(doc, "pl", "Polski Dubbing"); !ok {
		t.Errorf("no pl/Polski Dubbing track in %+v", audioStreams(doc))
	}
}

func TestAmbiguousQueuedTracksAbortBeforeWrites(t *testing.T) {
	h := newHarness(t)
	targetDirBefore := snapshot(t, h.dir)
	workBefore := snapshot(t, h.work)

	job := h.job()
	job.Candidates = []Candidate{
		{TrackID: 1, LangISO2: "pl", Variant: "dubbing"},
		{TrackID: 2, LangISO2: "pol", Variant: "lektor"},
		// Queued against a different file: ignored by the filename match.
		{TrackID: 3, LangISO2: "pl", Variant: "dubbing", TargetFilename: "Other Movie (2020) [TDARR].mkv"},
	}
	if _, err := New(Dependencies{}).Mux(context.Background(), job); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("Mux error = %v, want ErrAmbiguous", err)
	}

	if got, want := snapshot(t, h.dir), targetDirBefore; !reflect.DeepEqual(got, want) {
		t.Errorf("target directory changed on ambiguity: %v", diffKeys(want, got))
	}
	if got, want := snapshot(t, h.work), workBefore; !reflect.DeepEqual(got, want) {
		t.Errorf("work directory changed on ambiguity: %v", diffKeys(want, got))
	}
	if _, err := os.Stat(filepath.Join(h.work, "jobs", "42", "target.mkv")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("work copy written before ambiguity abort: %v", err)
	}
}

func TestAmbiguousLanguageMappingAborts(t *testing.T) {
	h := newHarness(t)
	job := h.job()
	job.Candidates = []Candidate{{TrackID: 1, LangISO2: "und"}}
	if _, err := New(Dependencies{}).Mux(context.Background(), job); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("Mux error = %v, want ErrAmbiguous", err)
	}
	if sha256File(t, h.target) == "" {
		t.Fatal("unreachable")
	}
}

func TestIneligibleTargetAbortsBeforeWrites(t *testing.T) {
	h := newHarness(t)
	// Rename the target so the gate marker is gone.
	untagged := filepath.Join(h.dir, "Movie (2021).mkv")
	if err := os.Rename(h.target, untagged); err != nil {
		t.Fatalf("rename: %v", err)
	}
	hash := sha256File(t, untagged)
	workBefore := snapshot(t, h.work)

	job := h.job()
	job.TargetPath = untagged
	if _, err := New(Dependencies{}).Mux(context.Background(), job); !errors.Is(err, ErrIneligible) {
		t.Fatalf("Mux error = %v, want ErrIneligible", err)
	}

	if got := sha256File(t, untagged); got != hash {
		t.Error("untagged target modified")
	}
	if _, err := os.Stat(untagged + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Error(".bak created for an ineligible target")
	}
	if got, want := snapshot(t, h.work), workBefore; !reflect.DeepEqual(got, want) {
		t.Errorf("work directory changed: %v", diffKeys(want, got))
	}
}

func TestForceBypassesGateButNotAmbiguity(t *testing.T) {
	h := newHarness(t)
	untagged := filepath.Join(h.dir, "Movie (2021).mkv")
	if err := os.Rename(h.target, untagged); err != nil {
		t.Fatalf("rename: %v", err)
	}
	job := h.job()
	job.TargetPath = untagged
	job.Force = true
	if _, err := New(Dependencies{}).Mux(context.Background(), job); err != nil {
		t.Fatalf("forced Mux: %v", err)
	}
	if _, err := os.Stat(untagged + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("forced mux created a .bak: %v", err)
	}
}

func TestUnmuxableVerdictsAbort(t *testing.T) {
	for _, verdict := range []string{analyze.VerdictDrifting, analyze.VerdictUnreliable, analyze.VerdictOutOfRange, ""} {
		t.Run("verdict="+verdict, func(t *testing.T) {
			h := newHarness(t)
			hash := sha256File(t, h.target)
			job := h.job()
			job.Report = analyze.Report{Verdict: verdict}
			if _, err := New(Dependencies{}).Mux(context.Background(), job); !errors.Is(err, ErrVerdict) {
				t.Fatalf("Mux error = %v, want ErrVerdict", err)
			}
			if got := sha256File(t, h.target); got != hash {
				t.Error("target modified on bad verdict")
			}
			if _, err := os.Stat(h.target + ".bak"); !errors.Is(err, os.ErrNotExist) {
				t.Error(".bak created on bad verdict")
			}
		})
	}
}

// TestCheckVerdictAcceptsACorrectedDrift pins the one widening (#86): a
// drifting timeline the fitter classified as a clean drift, and whose tempo
// factor the worker folded into the job's single encode, is aligned audio, so
// the guard behind ErrVerdict must let it through.
func TestCheckVerdictAcceptsACorrectedDrift(t *testing.T) {
	r := analyze.Report{
		Verdict:        analyze.VerdictDrifting,
		Classification: analyze.ClassificationDrifting,
		Fit: &analyze.LineFit{
			Shape:         analyze.ShapeDrift,
			SlopeMSPerSec: 42.7,
			TempoFactor:   1.0427,
			TempoApplied:  true,
		},
		Application: &analyze.Application{Mode: "re-encoded", Note: "re-encoded (tempo 1.0427)"},
	}
	if err := checkVerdict(r); err != nil {
		t.Fatalf("checkVerdict(corrected drift) = %v, want nil", err)
	}
}

// TestCheckVerdictStillRefusesADriftingCut keeps the other side pinned: a
// re-cut is drifting too, and no tempo factor can align it, so the same
// verdict with a cut shape must still be refused.
func TestCheckVerdictStillRefusesADriftingCut(t *testing.T) {
	r := analyze.Report{
		Verdict:        analyze.VerdictDrifting,
		Classification: analyze.ClassificationDrifting,
		Fit: &analyze.LineFit{
			Shape:        analyze.ShapeCut,
			CutAtSec:     1800,
			TempoFactor:  1.0427,
			TempoApplied: true,
		},
		Application: &analyze.Application{Mode: "re-encoded", Note: "re-encoded (tempo 1.0427)"},
	}
	err := checkVerdict(r)
	if !errors.Is(err, ErrVerdict) {
		t.Fatalf("checkVerdict(drifting cut) = %v, want ErrVerdict", err)
	}
}

// TestCheckVerdictStillRefusesAnUncorrectedDrift covers the trap: a report that
// merely looks like a drift is not a corrected track. Neither an encode that
// never happened nor a missing application record may satisfy the widening.
func TestCheckVerdictStillRefusesAnUncorrectedDrift(t *testing.T) {
	cases := map[string]analyze.Report{
		"tempo not applied": {
			Verdict:        analyze.VerdictDrifting,
			Classification: analyze.ClassificationDrifting,
			Fit: &analyze.LineFit{
				Shape:        analyze.ShapeDrift,
				TempoFactor:  1.0427,
				TempoApplied: false,
			},
			Application: &analyze.Application{Mode: "copy", Note: "copied untouched"},
		},
		"application missing": {
			Verdict:        analyze.VerdictDrifting,
			Classification: analyze.ClassificationDrifting,
			Fit: &analyze.LineFit{
				Shape:        analyze.ShapeDrift,
				TempoFactor:  1.0427,
				TempoApplied: true,
			},
		},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			if err := checkVerdict(r); !errors.Is(err, ErrVerdict) {
				t.Fatalf("checkVerdict(%s) = %v, want ErrVerdict", name, err)
			}
		})
	}
}

func TestMkvmergeFailureLeavesOriginalUntouched(t *testing.T) {
	h := newHarness(t)
	hash := sha256File(t, h.target)
	job := h.job()
	_, err := New(Dependencies{MKVMergePath: "/bin/false"}).Mux(context.Background(), job)
	if !errors.Is(err, ErrTool) {
		t.Fatalf("Mux error = %v, want ErrTool", err)
	}
	if got := sha256File(t, h.target); got != hash {
		t.Error("target modified on mkvmerge failure")
	}
	if leftovers, err := tempLeftovers(h.target); err != nil {
		t.Fatalf("list temps: %v", err)
	} else if len(leftovers) > 0 {
		t.Errorf("temp not cleaned up: %v", leftovers)
	}
	noTempsLeft(t, h.target)
	if _, err := os.Stat(h.target + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Error(".bak created despite failed mux")
	}
}

func TestStagedAudioOutsideJobDirAborts(t *testing.T) {
	h := newHarness(t)
	hash := sha256File(t, h.target)
	// A perfectly valid .mka, but staged outside {WORK_DIR}/jobs/{id}/.
	outside := copyFixture(t, fixtureAudio, h.dir, "vault-track.mka")

	job := h.job()
	job.AudioPath = outside
	if _, err := New(Dependencies{}).Mux(context.Background(), job); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("Mux error = %v, want ErrInvalidJob", err)
	}
	if got := sha256File(t, h.target); got != hash {
		t.Error("target modified for out-of-jobDir audio")
	}
	if _, err := os.Stat(h.target + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Error(".bak created despite invalid audio path")
	}
	noTempsLeft(t, h.target)
}

func TestTargetMustBeAnExistingRegularFile(t *testing.T) {
	h := newHarness(t)
	dirTarget := filepath.Join(h.dir, "Folder (2021) [TDARR].mkv")
	if err := os.Mkdir(dirTarget, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	cases := map[string]string{
		"missing":     filepath.Join(h.dir, "Nope (2021) [TDARR].mkv"),
		"non-regular": dirTarget,
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			job := h.job()
			job.TargetPath = path
			if _, err := New(Dependencies{}).Mux(context.Background(), job); !errors.Is(err, ErrTargetMissing) {
				t.Fatalf("Mux error = %v, want ErrTargetMissing", err)
			}
			if _, err := os.Stat(filepath.Join(h.work, "jobs", "42", "target.mkv")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("work copy written for unusable target: %v", err)
			}
		})
	}
}

func TestBasicJobShapeValidation(t *testing.T) {
	h := newHarness(t)
	hash := sha256File(t, h.target)
	cases := []struct {
		name   string
		mutate func(*Job)
		want   error
	}{
		{"no work dir", func(j *Job) { j.WorkDir = "  " }, ErrNoWorkDir},
		{"no job id", func(j *Job) { j.ID = 0 }, ErrInvalidJob},
		{"no target path", func(j *Job) { j.TargetPath = "" }, ErrInvalidJob},
		{"no audio path", func(j *Job) { j.AudioPath = "" }, ErrInvalidJob},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := h.job()
			tc.mutate(&job)
			if _, err := New(Dependencies{}).Mux(context.Background(), job); !errors.Is(err, tc.want) {
				t.Fatalf("Mux error = %v, want %v", err, tc.want)
			}
			if got := sha256File(t, h.target); got != hash {
				t.Error("target modified on invalid job")
			}
		})
	}
}

// TestPostMuxProbeFailureAborts covers ADR-0004: the muxed temp is probed
// before the rename, so a broken probe or a missing track leaves the original
// untouched and no .bak behind.
func TestPostMuxProbeFailureAborts(t *testing.T) {
	cases := map[string]string{
		"probe error":     "not json at all",
		"no audio tracks": `{"streams":[{"codec_type":"video"}]}`,
	}
	for name, probeOut := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			hash := sha256File(t, h.target)
			run := func(ctx context.Context, bin string, args ...string) ([]byte, error) {
				if filepath.Base(bin) == "ffprobe" {
					// The Copy-on-Work baseline (basename target.mkv) must
					// succeed; only the post-mux temp probe fails.
					if len(args) > 0 && filepath.Base(args[len(args)-1]) == "target.mkv" {
						return []byte(`{"streams":[{"codec_type":"video"},{"codec_type":"audio"}]}`), nil
					}
					return []byte(probeOut), nil
				}
				return execRun(ctx, bin, args...)
			}
			_, err := New(Dependencies{Run: run}).Mux(context.Background(), h.job())
			if !errors.Is(err, ErrPublish) {
				t.Fatalf("Mux error = %v, want ErrPublish", err)
			}
			if got := sha256File(t, h.target); got != hash {
				t.Error("target modified after failed validation")
			}
			if _, err := os.Stat(h.target + ".bak"); !errors.Is(err, os.ErrNotExist) {
				t.Error(".bak created despite failed validation")
			}
			noTempsLeft(t, h.target)
		})
	}
}

// isMkvmergeWrite reports the argv of the publishing mkvmerge call (as opposed
// to a probe): "-o <temp> --quiet <workCopy> ...". It is the seam the tests use
// to act mid-mux.
func isMkvmergeWrite(bin string, args []string) bool {
	return filepath.Base(bin) == "mkvmerge" && len(args) > 0 && args[0] == "-o"
}

// TestConcurrentMuxesOnDistinctTargetsPublishIndependently keeps this test's
// original point — each mux owns its temp file, so neither can delete the
// other's output — on the topology where concurrency actually happens: two
// different films drained at once. Both must publish, and each result must be a
// valid Matroska with exactly one appended track and no temp left beside it.
func TestConcurrentMuxesOnDistinctTargetsPublishIndependently(t *testing.T) {
	h := newHarness(t)
	second := *h
	second.id = 43
	second.target = copyFixture(t, fixtureTarget, h.dir, "Other Movie (2022) [TDARR].mkv")
	secondJobDir := filepath.Join(h.work, "jobs", "43")
	if err := os.MkdirAll(secondJobDir, 0o755); err != nil {
		t.Fatalf("mkdir second job: %v", err)
	}
	second.audio = copyFixture(t, fixtureAudio, secondJobDir, "synced.pl.mka")

	targets := []string{h.target, second.target}
	before := make([]int, len(targets))
	for i, target := range targets {
		before[i] = len(audioStreams(probeFile(t, target)))
	}

	jobs := []Job{h.job(), second.job()}
	errs := make([]error, len(jobs))
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = New(Dependencies{}).Mux(context.Background(), jobs[i])
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Mux %d on its own target: %v", i, err)
		}
	}

	for i, target := range targets {
		noTempsLeft(t, target)
		if _, err := os.Stat(target + ".bak"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("target %d left a .bak: %v", i, err)
		}
		doc := probeFile(t, target)
		if len(videoOnly(doc)) == 0 {
			t.Errorf("target %d has no video stream", i)
		}
		if got, want := len(audioStreams(doc)), before[i]+1; got != want {
			t.Errorf("target %d audio tracks = %d, want %d", i, got, want)
		}
		if _, ok := findAudio(doc, "pl", "PL DUB"); !ok {
			t.Errorf("target %d has no pl/PL DUB track", i)
		}
	}
}

// TestSequentialMuxesComposeTwoAudioTracks pins the real workflow: dubs are
// muxed one at a time into the same target, so the second publish copies a file
// that already carries the first dub and must append its own without losing it.
// The drain is sequential, so this — not a concurrent merge — is what
// production relies on, and it must not regress.
func TestSequentialMuxesComposeTwoAudioTracks(t *testing.T) {
	h := newHarness(t)
	first := h.job()
	first.TrackTitle = "FIRST DUB"

	second := *h
	second.id = 43
	secondJobDir := filepath.Join(h.work, "jobs", "43")
	if err := os.MkdirAll(secondJobDir, 0o755); err != nil {
		t.Fatalf("mkdir second job: %v", err)
	}
	second.audio = copyFixture(t, fixtureAudio, secondJobDir, "synced.pl.mka")
	secondJob := second.job()
	secondJob.TrackTitle = "SECOND DUB"

	before := len(audioStreams(probeFile(t, h.target)))

	if _, err := New(Dependencies{}).Mux(context.Background(), first); err != nil {
		t.Fatalf("first Mux: %v", err)
	}
	if _, err := New(Dependencies{}).Mux(context.Background(), secondJob); err != nil {
		t.Fatalf("second Mux: %v", err)
	}

	doc := probeFile(t, h.target)
	if len(videoOnly(doc)) == 0 {
		t.Error("composed target has no video stream")
	}
	if got, want := len(audioStreams(doc)), before+2; got != want {
		t.Fatalf("audio tracks = %d, want %d (the original plus both dubs)", got, want)
	}
	for _, title := range []string{"FIRST DUB", "SECOND DUB"} {
		if _, ok := findAudio(doc, "pl", title); !ok {
			t.Errorf("no pl/%s track in %+v; the earlier dub was lost", title, audioStreams(doc))
		}
	}
	noTempsLeft(t, h.target)
}

// TestConcurrentMuxesOnTheSameTargetPublishExactlyOnce pins the rule the
// staleness check introduced for replace mode. A mux is a read-copy-publish
// cycle over a file the media manager may replace at any time, so two muxes
// that both copied the same old target cannot both publish: the second rename
// would overwrite the first and drop its track. Refusing beats last-writer-wins
// here because publishing a copy taken before the other mux ran silently loses
// an audio track while looking like a successful merge.
//
// The race is orchestrated rather than hoped for: both calls are held at their
// mkvmerge step until both have arrived (so both copied the original), and the
// loser is then held until the winner's publish has landed, which makes the
// identity check deterministic.
func TestConcurrentMuxesOnTheSameTargetPublishExactlyOnce(t *testing.T) {
	h := newHarness(t)
	second := *h
	second.id = 43
	secondJobDir := filepath.Join(h.work, "jobs", "43")
	if err := os.MkdirAll(secondJobDir, 0o755); err != nil {
		t.Fatalf("mkdir second job: %v", err)
	}
	second.audio = copyFixture(t, fixtureAudio, secondJobDir, "synced.pl.mka")

	before := len(audioStreams(probeFile(t, h.target)))

	winnerJob := h.job()
	winnerJob.TrackTitle = "RACE WINNER"
	loserJob := second.job()
	loserJob.TrackTitle = "RACE LOSER"

	var mu sync.Mutex
	arrived := 0
	bothCopied := make(chan struct{})
	winnerPublished := make(chan struct{})
	gate := func() {
		mu.Lock()
		arrived++
		if arrived == 2 {
			close(bothCopied)
		}
		mu.Unlock()
		<-bothCopied
	}
	runWinner := func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		if isMkvmergeWrite(bin, args) {
			gate()
		}
		return execRun(ctx, bin, args...)
	}
	runLoser := func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		if isMkvmergeWrite(bin, args) {
			gate()
			<-winnerPublished
		}
		return execRun(ctx, bin, args...)
	}

	winner := New(Dependencies{Run: runWinner})
	winner.rename = func(oldpath, newpath string) error {
		defer close(winnerPublished)
		return os.Rename(oldpath, newpath)
	}
	loser := New(Dependencies{Run: runLoser})

	var (
		wg     sync.WaitGroup
		errWin error
		errLos error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errWin = winner.Mux(context.Background(), winnerJob)
	}()
	go func() {
		defer wg.Done()
		_, errLos = loser.Mux(context.Background(), loserJob)
	}()
	wg.Wait()

	if errWin != nil {
		t.Fatalf("the winning mux failed: %v", errWin)
	}
	if !errors.Is(errLos, ErrTargetChanged) {
		t.Fatalf("the losing mux error = %v, want ErrTargetChanged", errLos)
	}

	// The published target is a whole, valid Matroska — not truncated and not
	// half-written — carrying the winner's track only.
	doc := probeFile(t, h.target)
	if len(videoOnly(doc)) == 0 {
		t.Error("published target has no video stream")
	}
	if got, want := len(audioStreams(doc)), before+1; got != want {
		t.Errorf("audio tracks = %d, want %d (the winner's only, never a merged pair)", got, want)
	}
	if _, ok := findAudio(doc, "pl", "RACE WINNER"); !ok {
		t.Errorf("no pl/RACE WINNER track in %+v", audioStreams(doc))
	}
	if _, ok := findAudio(doc, "pl", "RACE LOSER"); ok {
		t.Errorf("the loser's track is in the target: a stale copy was published as a merge")
	}

	noTempsLeft(t, h.target)
	if _, err := os.Stat(h.target + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".bak left behind by the race: %v", err)
	}
}

// TestMuxLeavesPreExistingBackupUntouched: the operator (or an older dubarr
// build) may have a <target>.bak beside the library file. A mux must publish
// but leave that file byte-identical with its mode intact in both modes:
// dubarr never deletes or rewrites a file it did not create in this run.
func TestMuxLeavesPreExistingBackupUntouched(t *testing.T) {
	seeded := []byte("operator backup from an older dubarr build")
	for _, tc := range []struct {
		name     string
		separate bool
	}{
		{name: "replace mode"},
		{name: "separate mode", separate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			bak := h.target + ".bak"
			if err := os.WriteFile(bak, seeded, 0o600); err != nil {
				t.Fatalf("seed pre-existing .bak: %v", err)
			}
			wantHash := sha256File(t, bak)
			targetBefore := sha256File(t, h.target)

			job := h.job()
			if tc.separate {
				job, _, _ = h.separateHarness(t)
			}
			if _, err := New(Dependencies{}).Mux(context.Background(), job); err != nil {
				t.Fatalf("Mux: %v", err)
			}

			if got := sha256File(t, bak); got != wantHash {
				t.Errorf(".bak hash = %s, want it untouched at %s", got, wantHash)
			}
			info, err := os.Stat(bak)
			if err != nil {
				t.Fatalf("stat .bak: %v", err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf(".bak mode = %v, want 0600 untouched", info.Mode().Perm())
			}
			if tc.separate {
				if got := sha256File(t, h.target); got != targetBefore {
					t.Error("separate mode modified the library target")
				}
			} else if got := sha256File(t, h.target); got == targetBefore {
				t.Error("replace mode did not publish the muxed file")
			}
			noTempsLeft(t, h.target)
		})
	}
}

// diffKeys reports keys whose presence or hash differs.
func diffKeys(want, got map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for k, v := range want {
		seen[k] = true
		if g, ok := got[k]; !ok || g != v {
			out = append(out, k)
		}
	}
	for k := range got {
		if !seen[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// separateHarness returns a job in separate mode plus the destination the
// deterministic naming rule must produce.
func (h *harness) separateHarness(t *testing.T) (Job, string, string) {
	t.Helper()
	outDir := filepath.Join(h.dir, "output")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}
	job := h.job()
	job.OutputMode = OutputModeSeparate
	job.OutputDir = outDir
	return job, outDir, filepath.Join(outDir, "Movie (2021) [TDARR].dubarr.mkv")
}

// TestSeparateModeWritesDeterministicOutputAndKeepsTarget covers ADR-0011:
// separate mode publishes <basename>.dubarr.mkv into the output directory,
// leaves the library Target byte-identical and never creates a .bak.
func TestSeparateModeWritesDeterministicOutputAndKeepsTarget(t *testing.T) {
	h := newHarness(t)
	job, outDir, wantDest := h.separateHarness(t)
	targetHash := sha256File(t, h.target)
	beforeAudio := len(audioStreams(probeFile(t, h.target)))

	res, err := New(Dependencies{}).Mux(context.Background(), job)
	if err != nil {
		t.Fatalf("Mux: %v", err)
	}
	if res.OutputPath != wantDest {
		t.Fatalf("OutputPath = %q, want %q", res.OutputPath, wantDest)
	}
	if got, want := sha256File(t, h.target), targetHash; got != want {
		t.Errorf("target hash = %s, want unchanged %s", got, want)
	}
	if _, err := os.Stat(h.target + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".bak created in separate mode: %v", err)
	}
	if leftovers, err := tempLeftovers(wantDest); err != nil {
		t.Fatalf("list temps: %v", err)
	} else if len(leftovers) > 0 {
		t.Errorf("temp files left in output dir: %v", leftovers)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read output dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(wantDest) {
		t.Fatalf("output dir entries = %v, want only %s", entries, filepath.Base(wantDest))
	}
	doc := probeFile(t, wantDest)
	if got, want := len(audioStreams(doc)), beforeAudio+1; got != want {
		t.Fatalf("output audio tracks = %d, want %d", got, want)
	}
	if _, ok := findAudio(doc, "pl", "PL DUB"); !ok {
		t.Errorf("no pl/PL DUB audio track in %+v", audioStreams(doc))
	}
}

// TestSeparateModeTempIsCreatedInDestinationDir proves the temp lives beside
// the final file, so the publishing rename never crosses filesystems.
func TestSeparateModeTempIsCreatedInDestinationDir(t *testing.T) {
	h := newHarness(t)
	job, outDir, _ := h.separateHarness(t)

	var tempDir string
	m := New(Dependencies{})
	m.rename = func(oldpath, newpath string) error {
		tempDir = filepath.Dir(oldpath)
		return os.Rename(oldpath, newpath)
	}
	if _, err := m.Mux(context.Background(), job); err != nil {
		t.Fatalf("Mux: %v", err)
	}
	if tempDir != outDir {
		t.Fatalf("temp dir = %q, want %q", tempDir, outDir)
	}
}

// TestSeparateModeRefusesExistingDestination pins the collision policy: never
// overwrite and never auto-suffix.
func TestSeparateModeRefusesExistingDestination(t *testing.T) {
	h := newHarness(t)
	job, _, dest := h.separateHarness(t)
	if err := os.WriteFile(dest, []byte("previous deliverable"), 0o644); err != nil {
		t.Fatalf("seed destination: %v", err)
	}
	targetHash := sha256File(t, h.target)

	if _, err := New(Dependencies{}).Mux(context.Background(), job); !errors.Is(err, ErrOutputExists) {
		t.Fatalf("Mux error = %v, want ErrOutputExists", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "previous deliverable" {
		t.Errorf("existing destination was modified: %q", got)
	}
	if got := sha256File(t, h.target); got != targetHash {
		t.Error("target modified on collision")
	}
	if leftovers, err := tempLeftovers(dest); err != nil {
		t.Fatalf("list temps: %v", err)
	} else if len(leftovers) > 0 {
		t.Errorf("temp files left after collision: %v", leftovers)
	}
}

// TestSeparateModeCrossDeviceRenameFailsLoudly: a different-filesystem rename
// is a clear error, never a copy+delete fallback (ADR-0011).
func TestSeparateModeCrossDeviceRenameFailsLoudly(t *testing.T) {
	h := newHarness(t)
	job, _, dest := h.separateHarness(t)
	targetHash := sha256File(t, h.target)

	m := New(Dependencies{})
	m.rename = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
	}
	_, err := m.Mux(context.Background(), job)
	if !errors.Is(err, ErrCrossDevice) {
		t.Fatalf("Mux error = %v, want ErrCrossDevice", err)
	}
	if !strings.Contains(err.Error(), "different filesystems") {
		t.Errorf("error %q should explain the cross-filesystem failure", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("destination created despite cross-device failure: %v", err)
	}
	if got := sha256File(t, h.target); got != targetHash {
		t.Error("target modified on cross-device failure")
	}
	if leftovers, err := tempLeftovers(dest); err != nil {
		t.Fatalf("list temps: %v", err)
	} else if len(leftovers) > 0 {
		t.Errorf("temp files left after cross-device failure: %v", leftovers)
	}
}

// TestSeparateModeValidatesBeforePublish keeps ADR-0004 in separate mode: a
// failed post-mux probe leaves no destination file.
func TestSeparateModeValidatesBeforePublish(t *testing.T) {
	h := newHarness(t)
	job, outDir, _ := h.separateHarness(t)
	targetHash := sha256File(t, h.target)

	run := func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		if filepath.Base(bin) == "ffprobe" {
			if len(args) > 0 && filepath.Base(args[len(args)-1]) == "target.mkv" {
				return []byte(`{"streams":[{"codec_type":"video"},{"codec_type":"audio"}]}`), nil
			}
			return []byte(`{"streams":[{"codec_type":"video"}]}`), nil
		}
		return execRun(ctx, bin, args...)
	}
	_, err := New(Dependencies{Run: run}).Mux(context.Background(), job)
	if !errors.Is(err, ErrPublish) {
		t.Fatalf("Mux error = %v, want ErrPublish", err)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read output dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("output dir not empty after failed validation: %v", entries)
	}
	if got := sha256File(t, h.target); got != targetHash {
		t.Error("target modified after failed validation")
	}
}

// TestSeparateModePublishesWhenTargetChangesMidMux pins the deliberate
// asymmetry of the staleness check: separate mode only reads the target and
// publishes beside it, so a target the media manager replaced mid-run cannot
// corrupt the deliverable and must not fail the job. Replace mode refuses the
// same race (ADR-0018); this half proves the check did not leak into the mode
// where it would be a false failure.
func TestSeparateModePublishesWhenTargetChangesMidMux(t *testing.T) {
	h := newHarness(t)
	job, outDir, dest := h.separateHarness(t)
	before := len(audioStreams(probeFile(t, h.target)))

	upgraded := []byte("a brand-new release installed by the media manager")
	sum := sha256.Sum256(upgraded)
	upgradedHash := hex.EncodeToString(sum[:])

	run := func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		if isMkvmergeWrite(bin, args) {
			staged := h.target + ".upgrade"
			if err := os.WriteFile(staged, upgraded, 0o644); err != nil {
				t.Fatalf("write upgrade: %v", err)
			}
			if err := os.Rename(staged, h.target); err != nil {
				t.Fatalf("install upgrade: %v", err)
			}
		}
		return execRun(ctx, bin, args...)
	}

	res, err := New(Dependencies{Run: run}).Mux(context.Background(), job)
	if err != nil {
		t.Fatalf("separate-mode Mux: %v, want a publish despite the changed target", err)
	}
	if res.OutputPath != dest {
		t.Fatalf("OutputPath = %q, want %q", res.OutputPath, dest)
	}
	// The target read for the copy is gone; nothing was written into it.
	if got := sha256File(t, h.target); got != upgradedHash {
		t.Errorf("target hash = %s, want the upgraded bytes %s", got, upgradedHash)
	}
	if _, err := os.Stat(h.target + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".bak created in separate mode: %v", err)
	}

	doc := probeFile(t, dest)
	if len(videoOnly(doc)) == 0 {
		t.Error("separate deliverable has no video stream")
	}
	if got, want := len(audioStreams(doc)), before+1; got != want {
		t.Fatalf("deliverable audio tracks = %d, want %d", got, want)
	}
	if _, ok := findAudio(doc, "pl", "PL DUB"); !ok {
		t.Errorf("no pl/PL DUB track in %+v", audioStreams(doc))
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read output dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(dest) {
		t.Fatalf("output dir entries = %v, want only %s", entries, filepath.Base(dest))
	}
	noTempsLeft(t, dest)
}

// TestSeparateModeStillEnforcesTheTagGate: the gate is evaluated against the
// source Target in both modes.
func TestSeparateModeStillEnforcesTheTagGate(t *testing.T) {
	h := newHarness(t)
	job, outDir, _ := h.separateHarness(t)
	untagged := filepath.Join(h.dir, "Movie (2021).mkv")
	if err := os.Rename(h.target, untagged); err != nil {
		t.Fatalf("rename: %v", err)
	}
	job.TargetPath = untagged
	if _, err := New(Dependencies{}).Mux(context.Background(), job); !errors.Is(err, ErrIneligible) {
		t.Fatalf("Mux error = %v, want ErrIneligible", err)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read output dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("output dir not empty after an ineligible target: %v", entries)
	}
}

// TestOutputDestinationValidation covers unusable separate-mode destinations.
func TestOutputDestinationValidation(t *testing.T) {
	h := newHarness(t)
	outDir := filepath.Join(h.dir, "output")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}
	file := filepath.Join(h.dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*Job)
		want   error
	}{
		{"unknown mode", func(j *Job) { j.OutputMode = "elsewhere" }, ErrInvalidJob},
		{"missing dir", func(j *Job) { j.OutputMode = OutputModeSeparate; j.OutputDir = filepath.Join(h.dir, "nope") }, ErrOutputDir},
		{"file not dir", func(j *Job) { j.OutputMode = OutputModeSeparate; j.OutputDir = file }, ErrOutputDir},
		{"empty dir", func(j *Job) { j.OutputMode = OutputModeSeparate; j.OutputDir = "  " }, ErrInvalidJob},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			targetHash := sha256File(t, h.target)
			job := h.job()
			tc.mutate(&job)
			if _, err := New(Dependencies{}).Mux(context.Background(), job); !errors.Is(err, tc.want) {
				t.Fatalf("Mux error = %v, want %v", err, tc.want)
			}
			if got := sha256File(t, h.target); got != targetHash {
				t.Error("target modified on invalid destination")
			}
			if _, err := os.Stat(filepath.Join(h.work, "jobs", "42", "target.mkv")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("work copy written for an invalid destination: %v", err)
			}
		})
	}
}

// TestMuxRemovesJobStagingOnReturn pins issue #23: a standalone Mux call owns
// its staging. Once Mux returns — on success and on a failure that happened
// after the job directory was created — {WORK_DIR}/jobs/{id} is gone, so a
// caller that never runs the sync worker leaves no directory behind. Removal
// is idempotent, so the worker's own cleanup of the same job stays harmless.
func TestMuxRemovesJobStagingOnReturn(t *testing.T) {
	staging := func(h *harness) string { return filepath.Join(h.work, "jobs", "42") }

	t.Run("success", func(t *testing.T) {
		h := newHarness(t)
		if _, err := New(Dependencies{}).Mux(context.Background(), h.job()); err != nil {
			t.Fatalf("Mux: %v", err)
		}
		if _, err := os.Stat(staging(h)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("job staging %s still present after a successful mux: %v", staging(h), err)
		}
	})

	// A muxer failure happens after the job directory and the work copy exist
	// (mkvmerge cannot write its output), so this exercises the failure path
	// that used to leave the directory behind.
	t.Run("tool failure", func(t *testing.T) {
		h := newHarness(t)
		if _, err := New(Dependencies{MKVMergePath: "/bin/false"}).Mux(context.Background(), h.job()); !errors.Is(err, ErrTool) {
			t.Fatalf("Mux error = %v, want ErrTool", err)
		}
		if _, err := os.Stat(staging(h)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("job staging %s still present after a failed mux: %v", staging(h), err)
		}
		if _, err := os.Stat(h.target + ".bak"); !errors.Is(err, os.ErrNotExist) {
			t.Error(".bak created despite failed mux")
		}
	})
}

// TestArrGateCriteriaBlockAndForceOverride wires ADR-0009 into the mux call
// site: the verdict uses the arr metadata carried by the job, and force
// override still bypasses the whole gate.
func TestArrGateCriteriaBlockAndForceOverride(t *testing.T) {
	h := newHarness(t)
	job := h.job()
	job.Gate = gate.Rule{
		SourceMode:      gate.SourceArr,
		RequiredArrTags: []string{"dub"},
		Mode:            gate.MatchAny,
		Enforce:         true,
	}
	job.Arr = gate.ArrMetadata{Found: true, Tags: []string{"keep"}}
	if _, err := New(Dependencies{}).Mux(context.Background(), job); !errors.Is(err, ErrIneligible) {
		t.Fatalf("Mux error = %v, want ErrIneligible for a non-matching arr tag", err)
	}
	if _, err := os.Stat(h.target + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Error(".bak created for an arr-ineligible target")
	}

	// A library row that satisfies the criterion lets the mux proceed.
	job.Arr = gate.ArrMetadata{Found: true, Tags: []string{"Polish Dubbing", "dub"}, RootFolder: "/media/movies"}
	if _, err := New(Dependencies{}).Mux(context.Background(), job); err != nil {
		t.Fatalf("arr-eligible Mux: %v", err)
	}

	// Force bypasses the arr criterion too (the caller owns the switch).
	h2 := newHarness(t)
	job2 := h2.job()
	job2.Gate = job.Gate
	job2.Arr = gate.ArrMetadata{Found: false}
	job2.Force = true
	if _, err := New(Dependencies{}).Mux(context.Background(), job2); err != nil {
		t.Fatalf("forced Mux with no arr row: %v", err)
	}
}

// TestMuxContainerGuardRefusesNonMatroska pins the container guard: mkvmerge
// only ever writes Matroska, so a target (replace) or destination (separate)
// that is not .mkv/.mka is refused before the work copy exists. A refusal must
// leave the target byte-identical, no temp and no .bak behind, and the
// caller-owned staging untouched. The check is extension-based and
// case-insensitive on purpose.
func TestMuxContainerGuardRefusesNonMatroska(t *testing.T) {
	for _, mode := range []string{OutputModeReplace, OutputModeSeparate} {
		for _, ext := range []string{".mp4", ".MP4", ".avi", ".m2ts"} {
			t.Run(mode+"/"+ext, func(t *testing.T) {
				h := newHarness(t)
				// The fixture bytes are Matroska; only the name is foreign, which
				// is exactly what a mux into an .mp4 target would publish over.
				h.target = copyFixture(t, fixtureTarget, h.dir, "Movie (2021) [TDARR]"+ext)
				job := h.job()
				if mode == OutputModeSeparate {
					job.OutputMode = OutputModeSeparate
					job.OutputDir = filepath.Join(h.dir, "output")
					if err := os.MkdirAll(job.OutputDir, 0o755); err != nil {
						t.Fatalf("mkdir output: %v", err)
					}
				}

				targetHash := sha256File(t, h.target)
				jobDir := filepath.Join(h.work, "jobs", "42")
				stagingBefore := snapshot(t, jobDir)

				_, err := New(Dependencies{}).Mux(context.Background(), job)
				if !errors.Is(err, ErrUnsupportedContainer) {
					t.Fatalf("Mux error = %v, want ErrUnsupportedContainer", err)
				}
				if !strings.Contains(err.Error(), filepath.Base(h.target)) {
					t.Errorf("error %q should name the offending path %s", err, h.target)
				}

				if got := sha256File(t, h.target); got != targetHash {
					t.Errorf("target hash = %s, want unchanged %s", got, targetHash)
				}
				if leftovers, err := tempLeftovers(h.target); err != nil {
					t.Fatalf("list temps: %v", err)
				} else if len(leftovers) > 0 {
					t.Errorf("temp files left after a container refusal: %v", leftovers)
				}
				if _, err := os.Stat(h.target + ".bak"); !errors.Is(err, os.ErrNotExist) {
					t.Errorf(".bak created by a refused mux: %v", err)
				}
				// No work copy: the refusal happens before Copy-on-Work.
				if _, err := os.Stat(filepath.Join(jobDir, "target"+filepath.Ext(h.target))); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("work copy staged for a refused mux: %v", err)
				}
				// The staging directory belongs to the caller (the sync worker
				// removes it); a pre-write refusal leaves it byte-identical.
				if got := snapshot(t, jobDir); !reflect.DeepEqual(got, stagingBefore) {
					t.Errorf("caller staging changed on a refused mux:\n got %v\nwant %v", got, stagingBefore)
				}
				if mode == OutputModeSeparate {
					entries, err := os.ReadDir(job.OutputDir)
					if err != nil {
						t.Fatalf("read output dir: %v", err)
					}
					if len(entries) != 0 {
						t.Errorf("output dir entries = %v, want none", entries)
					}
				}
			})
		}
	}
}

// TestMuxContainerGuardAcceptsMatroska is the positive half: .mkv, .MKV and
// .mka targets still publish, and the guard never blocks a legitimate mux.
func TestMuxContainerGuardAcceptsMatroska(t *testing.T) {
	for _, ext := range []string{".mkv", ".MKV", ".mka"} {
		t.Run(ext, func(t *testing.T) {
			h := newHarness(t)
			h.target = copyFixture(t, fixtureTarget, h.dir, "Movie (2021) [TDARR]"+ext)
			beforeHash := sha256File(t, h.target)

			res, err := New(Dependencies{}).Mux(context.Background(), h.job())
			if err != nil {
				t.Fatalf("Mux: %v", err)
			}
			if res.OutputPath != h.target {
				t.Fatalf("OutputPath = %q, want %q", res.OutputPath, h.target)
			}
			if got := sha256File(t, h.target); got == beforeHash {
				t.Error("target content unchanged, mux did not publish")
			}
			if _, err := os.Stat(h.target + ".bak"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf(".bak created by a replace-mode publish: %v", err)
			}
			noTempsLeft(t, h.target)
		})
	}
}

// TestMuxStalenessRefusesToPublishOverAChangedTarget pins ADR-0018's staleness
// check: the mux copies the target, the media manager then replaces it while
// mkvmerge runs, and the publish must be refused rather than silently
// overwriting the upgrade with output built from the old file. The upgraded
// bytes survive untouched, no temp and no .bak are left behind, and the job
// staging is removed as on every other return path.
func TestMuxStalenessRefusesToPublishOverAChangedTarget(t *testing.T) {
	h := newHarness(t)

	upgraded := []byte("a brand-new release installed by the media manager")
	sum := sha256.Sum256(upgraded)
	upgradedHash := hex.EncodeToString(sum[:])

	// The Run seam is the injection point the other tests use: rewrite the
	// target exactly when mkvmerge starts, which is mid-mux, after the work
	// copy was taken and before the publish.
	run := func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		if filepath.Base(bin) == "mkvmerge" && len(args) > 0 && args[0] == "-o" {
			staged := h.target + ".upgrade"
			if err := os.WriteFile(staged, upgraded, 0o644); err != nil {
				t.Fatalf("write upgrade: %v", err)
			}
			// Rename so the inode changes too, as an arr upgrade would.
			if err := os.Rename(staged, h.target); err != nil {
				t.Fatalf("install upgrade: %v", err)
			}
		}
		return execRun(ctx, bin, args...)
	}

	_, err := New(Dependencies{Run: run}).Mux(context.Background(), h.job())
	if !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("Mux error = %v, want ErrTargetChanged", err)
	}
	if !strings.Contains(err.Error(), h.target) {
		t.Errorf("error %q should name the target %s", err, h.target)
	}

	if got := sha256File(t, h.target); got != upgradedHash {
		t.Errorf("target hash = %s, want the upgraded bytes %s", got, upgradedHash)
	}
	noTempsLeft(t, h.target)
	if _, err := os.Stat(h.target + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".bak created by a refused publish: %v", err)
	}
	jobDir := filepath.Join(h.work, "jobs", "42")
	if _, err := os.Stat(jobDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("job staging %s survived the refusal: %v", jobDir, err)
	}
}

// TestMuxStalenessControlPublishesWhenTargetUnchanged is the control half: an
// untouched target still publishes, so the check cannot pass by refusing every
// mux.
func TestMuxStalenessControlPublishesWhenTargetUnchanged(t *testing.T) {
	h := newHarness(t)
	beforeHash := sha256File(t, h.target)

	res, err := New(Dependencies{}).Mux(context.Background(), h.job())
	if err != nil {
		t.Fatalf("Mux: %v", err)
	}
	if res.OutputPath != h.target {
		t.Fatalf("OutputPath = %q, want %q", res.OutputPath, h.target)
	}
	if got := sha256File(t, h.target); got == beforeHash {
		t.Error("target content unchanged, mux did not publish")
	}
	if _, err := os.Stat(h.target + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".bak created by a replace-mode publish: %v", err)
	}
	noTempsLeft(t, h.target)
}
