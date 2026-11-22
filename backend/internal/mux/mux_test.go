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
	if _, err := os.Stat(h.target + ".dubarr.tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file still present: %v", err)
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

	// Video stream is copied byte-for-byte: codec/size/frame count/bitrate and
	// duration are untouched.
	afterProbe := probeFile(t, h.target)
	if want, got := videoOnly(beforeProbe), videoOnly(afterProbe); !reflect.DeepEqual(got, want) {
		t.Errorf("video stream changed:\n got %+v\nwant %+v", got, want)
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
	if _, err := os.Stat(h.target + ".dubarr.tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp not cleaned up: %v", err)
	}
	if _, err := os.Stat(h.target + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Error(".bak created despite failed mux")
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
