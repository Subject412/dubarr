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
// stores both the legacy code (pol) and the IETF code (pl) from --language 0:pl
// (IDEA.md:393); ffprobe only exposes the legacy tag.
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

func boolPtr(v bool) *bool { return &v }

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

func TestMuxPublishesAtomicallyWithBackup(t *testing.T) {
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

	if res.BackupPath != h.target+".bak" {
		t.Errorf("BackupPath = %q, want %q", res.BackupPath, h.target+".bak")
	}
	if res.OutputPath != h.target {
		t.Errorf("OutputPath = %q, want %q", res.OutputPath, h.target)
	}
	if leftovers, err := tempLeftovers(h.target); err != nil {
		t.Fatalf("list temps: %v", err)
	} else if len(leftovers) > 0 {
		t.Errorf("temp files still present beside target: %v", leftovers)
	}

	// The original bytes survive verbatim in the sidecar backup.
	if got := sha256File(t, h.target+".bak"); got != beforeTargetHash {
		t.Errorf(".bak hash = %s, want %s", got, beforeTargetHash)
	}
	bakInfo, err := os.Stat(h.target + ".bak")
	if err != nil {
		t.Fatalf("stat .bak: %v", err)
	}
	if bakInfo.Mode().Perm() != beforeInfo.Mode().Perm() {
		t.Errorf(".bak mode = %v, want %v", bakInfo.Mode().Perm(), beforeInfo.Mode().Perm())
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
	// language and the dub marker (IDEA.md:506 "PL DUB").
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

	// The target was copied into the job's Work Area first (Copy-on-Work).
	workCopy := filepath.Join(h.work, "jobs", "42", "target.mkv")
	if got := sha256File(t, workCopy); got != beforeTargetHash {
		t.Errorf("work copy hash = %s, want staged original %s", got, beforeTargetHash)
	}

	// Command shape follows IDEA.md:397-403: target as input, then the
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
	if _, err := os.Stat(untagged + ".bak"); err != nil {
		t.Errorf("forced mux did not keep .bak: %v", err)
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

// TestConcurrentMuxesUseDistinctTemps runs two publishes on one target: each
// job must own its temp file, so neither can delete the other's output.
func TestConcurrentMuxesUseDistinctTemps(t *testing.T) {
	h := newHarness(t)
	second := *h
	second.id = 43
	secondJobDir := filepath.Join(h.work, "jobs", "43")
	if err := os.MkdirAll(secondJobDir, 0o755); err != nil {
		t.Fatalf("mkdir second job: %v", err)
	}
	second.audio = copyFixture(t, fixtureAudio, secondJobDir, "synced.pl.mka")

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
			t.Fatalf("concurrent Mux %d: %v", i, err)
		}
	}
	noTempsLeft(t, h.target)
	doc := probeFile(t, h.target)
	if got := len(audioStreams(doc)); got != 2 {
		t.Errorf("audio tracks = %d, want 2 (one per concurrent job)", got)
	}
	if _, err := os.Stat(h.target + ".bak"); err != nil {
		t.Errorf("concurrent mux left no .bak: %v", err)
	}
}

// TestMuxDiscardsBackupWhenKeepBakFalse covers settings mux.options.keep_bak=
// false: the validated publish stands, but no <target>.bak is left behind.
func TestMuxDiscardsBackupWhenKeepBakFalse(t *testing.T) {
	h := newHarness(t)
	beforeAudio := len(audioStreams(probeFile(t, h.target)))

	job := h.job()
	job.KeepBak = boolPtr(false)
	res, err := New(Dependencies{}).Mux(context.Background(), job)
	if err != nil {
		t.Fatalf("Mux: %v", err)
	}
	if _, err := os.Stat(h.target + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".bak still present (err = %v), want removed", err)
	}
	if res.BackupPath != "" {
		t.Errorf("BackupPath = %q, want empty after removal", res.BackupPath)
	}
	if res.OutputPath != h.target {
		t.Errorf("OutputPath = %q, want %q", res.OutputPath, h.target)
	}
	afterProbe := probeFile(t, h.target)
	if got, want := len(audioStreams(afterProbe)), beforeAudio+1; got != want {
		t.Errorf("audio tracks = %d, want %d", got, want)
	}
	if _, ok := findAudio(afterProbe, "pl", "PL DUB"); !ok {
		t.Errorf("no pl/PL DUB audio track in %+v", audioStreams(afterProbe))
	}
	if leftovers, err := tempLeftovers(h.target); err != nil {
		t.Fatalf("list temps: %v", err)
	} else if len(leftovers) > 0 {
		t.Errorf("temp files still present beside target: %v", leftovers)
	}
}

// TestMuxBackupRemovalFailureWarnsAndPublishes: a failing .bak removal is a
// WARN, never a mux failure. The backup simply stays on disk.
func TestMuxBackupRemovalFailureWarnsAndPublishes(t *testing.T) {
	h := newHarness(t)
	var warnings []string
	merr := New(Dependencies{Warnf: func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}})
	merr.remove = func(string) error { return errors.New("remove denied") }

	job := h.job()
	job.KeepBak = boolPtr(false)
	res, err := merr.Mux(context.Background(), job)
	if err != nil {
		t.Fatalf("Mux: %v", err)
	}
	if res.BackupPath != h.target+".bak" {
		t.Errorf("BackupPath = %q, want %q (removal failed, backup stays)", res.BackupPath, h.target+".bak")
	}
	if _, err := os.Stat(h.target + ".bak"); err != nil {
		t.Errorf("stat .bak: %v, want the backup left in place", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warnings)
	}
	if !strings.Contains(warnings[0], ".bak") || !strings.Contains(warnings[0], "remove denied") {
		t.Errorf("warning %q should name the backup and the reason", warnings[0])
	}
	if _, ok := findAudio(probeFile(t, h.target), "pl", "PL DUB"); !ok {
		t.Errorf("publish did not survive the backup removal failure")
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
	if res.BackupPath != "" {
		t.Errorf("BackupPath = %q, want empty in separate mode", res.BackupPath)
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

// TestSeparateModeIgnoresKeepBak: nothing was replaced, so keep_bak is inert
// and the .bak removal hook is never called.
func TestSeparateModeIgnoresKeepBak(t *testing.T) {
	h := newHarness(t)
	job, _, dest := h.separateHarness(t)
	discard := false
	job.KeepBak = &discard

	removed := false
	m := New(Dependencies{})
	m.remove = func(string) error {
		removed = true
		return errors.New("should not be called")
	}
	res, err := m.Mux(context.Background(), job)
	if err != nil {
		t.Fatalf("Mux: %v", err)
	}
	if removed {
		t.Error("keep_bak removal ran in separate mode")
	}
	if res.BackupPath != "" {
		t.Errorf("BackupPath = %q, want empty", res.BackupPath)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("separate output missing: %v", err)
	}
	if _, err := os.Stat(h.target + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".bak created in separate mode: %v", err)
	}
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
