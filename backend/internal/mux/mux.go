package mux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/gate"
	"github.com/Subject412/dubarr/backend/internal/probe"
)

// DefaultTimeout bounds one mkvmerge remux. Unlike probe/gate this is a bulk
// operation, so the hang guard is generous rather than a performance budget.
const DefaultTimeout = 30 * time.Minute

// Candidate is one queued vault track that may be muxed into the target. M5-3
// supplies the queue; Mux only resolves it.
type Candidate struct {
	TrackID int64
	// LangISO2 is the archived track language (ISO 639-1 or any spelling
	// probe.ISO639_1 understands).
	LangISO2 string
	// Variant is the audio_tracks.variant (dubbing, lektor, original, ...).
	Variant string
	// TargetFilename is the target file the track is queued against. An empty
	// value means the job's own target.
	TargetFilename string
}

// Job is a completed sync job ready to be muxed into its target.
type Job struct {
	// ID names {WORK_DIR}/jobs/{id}/ and is the sync_mux_jobs row id.
	ID int64
	// WorkDir is storage.paths.work_dir (the Work Area root), not the job dir.
	WorkDir string
	// TargetPath is the library MKV to publish into. Only the Tdarr Gate and
	// the copy read it; it is never edited in place.
	TargetPath string
	// TargetLangISO2 is the language of the incoming track (audio_tracks
	// lang_iso2), written as the Matroska track language.
	TargetLangISO2 string
	// AudioPath is the synced .mka already staged by Copy-on-Work under
	// {WORK_DIR}/jobs/{id}/.
	AudioPath string
	// SyncOffsetMS is final_offset_ms: offset = t_target - t_donor, positive
	// delays the donor (mkvmerge --sync 0:+N), negative advances it (IDEA.md:335).
	SyncOffsetMS int64
	// TrackTitle is the Matroska track name (settings mux.options
	// track_title_in_mkv). Empty selects [DefaultTrackTitle].
	TrackTitle string
	// SetDefault marks the appended track as the container default.
	SetDefault bool
	// Gate is the target.tag_filter rule re-checked against the base filename.
	Gate gate.Rule
	// Force bypasses the filename gate only when security.options
	// allow_force_tag_override is on; the caller owns that switch.
	Force bool
	// Report is the completed analysis; Verdict must be ok or stable.
	Report analyze.Report
	// Candidates are the queued vault tracks for this target, used to
	// loud-fail ambiguous pairings. An empty slice means the job itself is the
	// only mapping and there is nothing to disambiguate.
	Candidates []Candidate
}

// Result describes a successful publish.
type Result struct {
	OutputPath string
	BackupPath string
	// WorkCopy is the Copy-on-Work target input mkvmerge read.
	WorkCopy string
	// AudioPath is the staged .mka that was appended.
	AudioPath string
	// Language and TrackTitle are what were written onto the new track.
	Language   string
	TrackTitle string
	// Command is the exact mkvmerge argv (including argv[0]) for job logs.
	Command []string
}

// Dependencies configure a Muxer. All fields are optional.
type Dependencies struct {
	// MKVMergePath pins the binary; empty resolves mkvmerge on PATH.
	MKVMergePath string
	// LookPath overrides binary discovery (tests).
	LookPath func(string) (string, error)
	// Run executes the tool and returns combined output; nil uses
	// exec.CommandContext.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Timeout bounds one invocation; 0 selects DefaultTimeout.
	Timeout time.Duration
}

// Muxer performs tag-gated, atomic muxing.
type Muxer struct {
	mkvmergePath string
	lookPath     func(string) (string, error)
	run          func(ctx context.Context, name string, args ...string) ([]byte, error)
	timeout      time.Duration
}

// New builds a Muxer. A missing mkvmerge is reported from Mux, not here.
func New(deps Dependencies) *Muxer {
	m := &Muxer{
		mkvmergePath: deps.MKVMergePath,
		lookPath:     deps.LookPath,
		run:          deps.Run,
		timeout:      deps.Timeout,
	}
	if m.lookPath == nil {
		m.lookPath = exec.LookPath
	}
	if m.run == nil {
		m.run = execRun
	}
	if m.timeout <= 0 {
		m.timeout = DefaultTimeout
	}
	return m
}

// Mux publishes one synchronized track into its target.
//
// Order is deliberate: verdict, ambiguity and the Tdarr Gate are all resolved
// read-only before the first write, so an abort leaves the target directory and
// the Work Area untouched. The target is then copied into
// {WORK_DIR}/jobs/{id}/, mkvmerge writes <target>.dubarr.tmp beside the target
// (same filesystem), the original is copied to <target>.bak, and only then is
// the temp renamed over the original. Any failure removes the temp and leaves
// the original bytes in place; staging cleanup belongs to the caller.
func (m *Muxer) Mux(ctx context.Context, job Job) (Result, error) {
	target, _, jobDir, lang, err := job.normalize()
	if err != nil {
		return Result{}, err
	}

	info, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{}, fmt.Errorf("%w: %s", ErrTargetMissing, target)
		}
		return Result{}, fmt.Errorf("%w: stat %s: %v", ErrTargetMissing, target, err)
	}
	if !info.Mode().IsRegular() {
		return Result{}, fmt.Errorf("%w: %s is not a regular file", ErrTargetMissing, target)
	}

	if err := checkVerdict(job.Report); err != nil {
		return Result{}, err
	}
	if err := checkAmbiguity(job, target, lang); err != nil {
		return Result{}, err
	}
	if !job.Force {
		res := job.Gate.Classify(filepath.Base(target))
		if res.Status != gate.StatusEligible {
			return Result{}, fmt.Errorf("%w: %s is %s (matched %v, missing %v)",
				ErrIneligible, filepath.Base(target), res.Status, res.Matched, res.Missing)
		}
	}
	audio, err := resolveAudio(job.AudioPath, jobDir)
	if err != nil {
		return Result{}, err
	}
	audioInfo, err := os.Stat(audio)
	if err != nil || !audioInfo.Mode().IsRegular() {
		return Result{}, fmt.Errorf("%w: %s", ErrAudioMissing, audio)
	}

	title := strings.TrimSpace(job.TrackTitle)
	if title == "" {
		title = DefaultTrackTitle(lang)
	}

	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("%w: create job dir: %v", ErrPublish, err)
	}
	workCopy := filepath.Join(jobDir, "target"+filepath.Ext(target))
	if err := copyFile(target, workCopy, 0o644); err != nil {
		return Result{}, fmt.Errorf("%w: work copy: %v", ErrPublish, err)
	}

	bin := m.mkvmergePath
	if bin == "" {
		if bin, err = m.lookPath("mkvmerge"); err != nil {
			return Result{}, fmt.Errorf("%w: mkvmerge not found on PATH: %v", ErrTool, err)
		}
	}
	temp := target + ".dubarr.tmp"
	if err := os.Remove(temp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("%w: stale temp: %v", ErrPublish, err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.Remove(temp)
		}
	}()

	// IDEA.md:397-403: the target is input 0 and is copied as-is; per-input
	// options bind to the appended .mka (input 0 of that file).
	args := []string{
		"-o", temp,
		"--quiet",
		workCopy,
		"--language", "0:" + lang,
		"--track-name", "0:" + title,
		"--sync", "0:" + strconv.FormatInt(job.SyncOffsetMS, 10),
		"--default-track-flag", "0:" + yesNo(job.SetDefault),
		audio,
	}
	if _, err := m.invoke(ctx, bin, args...); err != nil {
		return Result{}, err
	}
	if err := syncFile(temp); err != nil {
		return Result{}, fmt.Errorf("%w: sync temp: %v", ErrPublish, err)
	}
	if fi, err := os.Stat(temp); err != nil || fi.Size() == 0 {
		return Result{}, fmt.Errorf("%w: mkvmerge produced no output", ErrTool)
	}

	bak := target + ".bak"
	if err := writeBackup(target, bak, info.Mode().Perm()); err != nil {
		return Result{}, fmt.Errorf("%w: backup: %v", ErrPublish, err)
	}
	// Same directory as the target, so this rename is atomic on one
	// filesystem. The temp is flushed above; the original is still intact
	// until this succeeds.
	if err := os.Rename(temp, target); err != nil {
		return Result{}, fmt.Errorf("%w: rename output: %v", ErrPublish, err)
	}
	published = true

	return Result{
		OutputPath: target,
		BackupPath: bak,
		WorkCopy:   workCopy,
		AudioPath:  audio,
		Language:   lang,
		TrackTitle: title,
		Command:    append([]string{bin}, args...),
	}, nil
}

// normalize validates the job shape and returns canonical paths. Symlinks are
// resolved so a rename replaces the real target, never a link pointing at it.
func (j Job) normalize() (target, workDir, jobDir, lang string, err error) {
	if j.ID <= 0 {
		return "", "", "", "", fmt.Errorf("%w: job id must be positive", ErrInvalidJob)
	}
	if strings.TrimSpace(j.WorkDir) == "" {
		return "", "", "", "", ErrNoWorkDir
	}
	if strings.TrimSpace(j.TargetPath) == "" || strings.TrimSpace(j.AudioPath) == "" {
		return "", "", "", "", fmt.Errorf("%w: target path and staged audio are required", ErrInvalidJob)
	}
	lang = probe.ISO639_1(j.TargetLangISO2)
	if lang == "" {
		return "", "", "", "", fmt.Errorf("%w: target language %q has no ISO 639-1 mapping", ErrAmbiguous, j.TargetLangISO2)
	}
	absWork, err := filepath.Abs(j.WorkDir)
	if err != nil {
		return "", "", "", "", fmt.Errorf("%w: resolve work dir: %v", ErrInvalidJob, err)
	}
	workDir = filepath.Clean(absWork)
	jobDir = filepath.Join(workDir, "jobs", strconv.FormatInt(j.ID, 10))

	absTarget, err := filepath.Abs(j.TargetPath)
	if err != nil {
		return "", "", "", "", fmt.Errorf("%w: resolve target: %v", ErrInvalidJob, err)
	}
	if target, err = filepath.EvalSymlinks(absTarget); err != nil {
		return "", "", "", "", fmt.Errorf("%w: %s: %v", ErrTargetMissing, j.TargetPath, err)
	}
	return target, workDir, jobDir, lang, nil
}

// checkVerdict accepts only the settled ok/stable reports. Drifting,
// unreliable, out-of-range and missing verdicts fail loudly (PLAN.md M5-1).
func checkVerdict(r analyze.Report) error {
	switch strings.ToLower(strings.TrimSpace(r.Verdict)) {
	case analyze.VerdictOK, analyze.ClassificationStable:
		return nil
	default:
		return fmt.Errorf("%w: verdict %q (classification %q)", ErrVerdict, r.Verdict, r.Classification)
	}
}

// checkAmbiguity enforces the loud-fail rule before any write.
//
// A candidate applies when its target filename matches this target's base name
// (empty means "this target"). Every applicable candidate must carry a
// normalizable language, and exactly one of them must carry the job's target
// language: two queued tracks for the same target/language (e.g. a dubbing and
// a lektor) make the pairing ambiguous, and a candidate set with no member for
// the report's language means the language mapping is broken.
func checkAmbiguity(job Job, target, lang string) error {
	if len(job.Candidates) == 0 {
		return nil
	}
	base := filepath.Base(target)
	seen := map[int64]Candidate{}
	anonymous := 0
	for _, c := range job.Candidates {
		if c.TargetFilename != "" && !strings.EqualFold(filepath.Base(c.TargetFilename), base) {
			continue
		}
		candLang := probe.ISO639_1(c.LangISO2)
		if candLang == "" {
			return fmt.Errorf("%w: queued track %d has no ISO 639-1 language (%q)",
				ErrAmbiguous, c.TrackID, c.LangISO2)
		}
		if candLang != lang {
			continue
		}
		if c.TrackID > 0 {
			seen[c.TrackID] = c
			continue
		}
		anonymous++
		seen[-int64(anonymous)] = c
	}
	switch {
	case len(seen) == 0:
		return fmt.Errorf("%w: no queued vault track maps to %s for %s", ErrAmbiguous, lang, base)
	case len(seen) > 1:
		return fmt.Errorf("%w: %d queued vault tracks map to %s for %s", ErrAmbiguous, len(seen), lang, base)
	}
	return nil
}

// resolveAudio canonicalizes the staged audio and confines it to the job
// directory: IDEA.md:114/393 forbid reading a vault file during mux.
func resolveAudio(audioPath, jobDir string) (string, error) {
	abs, err := filepath.Abs(audioPath)
	if err != nil {
		return "", fmt.Errorf("%w: resolve audio: %v", ErrInvalidJob, err)
	}
	audio, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrAudioMissing, audioPath, err)
	}
	root, err := filepath.EvalSymlinks(jobDir)
	if err != nil {
		return "", fmt.Errorf("%w: job dir %s: %v", ErrInvalidJob, jobDir, err)
	}
	rel, err := filepath.Rel(root, audio)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: staged audio %s is outside %s", ErrInvalidJob, audio, root)
	}
	return audio, nil
}

// DefaultTrackTitle is the fallback Matroska track name: the ISO-2 language
// plus the dub marker, rendered like the vault badge "PL DUB" (IDEA.md:506).
// A configured mux.options.track_title_in_mkv (default "Polski Dubbing")
// overrides it.
func DefaultTrackTitle(iso2 string) string {
	return strings.ToUpper(iso2) + " DUB"
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// writeBackup copies the original to <name>.bk through a same-directory temp,
// preserving its permission bits (0644-ish for library files).
func writeBackup(src, dst string, perm os.FileMode) error {
	tmp := dst + ".tmp"
	if err := copyFile(src, tmp, perm); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// copyFile copies src to dst, creating dst with perm. The source is opened
// read-only, which is the only access Mux ever makes to a target or vault file.
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// syncFile flushes the muxed temp before it is renamed into place.
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// invoke runs mkvmerge under the hang guard. Exit code 1 is a warning exit
// with a valid output file, so it is accepted (mirroring internal/gate).
func (m *Muxer) invoke(ctx context.Context, bin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	out, err := m.run(ctx, bin, args...)
	if err == nil {
		return out, nil
	}
	if ctx.Err() != nil {
		return out, fmt.Errorf("%w: timed out after %s", ErrTool, m.timeout)
	}
	return out, fmt.Errorf("%w: %v: %s", ErrTool, err, tail(out))
}

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return out, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && len(bytes.TrimSpace(out)) > 0 {
		return out, nil
	}
	return out, err
}

func tail(out []byte) string {
	msg := strings.TrimSpace(string(out))
	if len(msg) > 500 {
		msg = msg[len(msg)-500:]
	}
	return msg
}
