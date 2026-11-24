package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Subject412/dubarr/backend/internal/gate"
	"github.com/Subject412/dubarr/backend/internal/probe"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// processJob extracts and validates one donor row into its staging directory.
// The staging directory is wiped on every failure and left in place on
// success for M3-2.
func (w *Worker) processJob(ctx context.Context, job store.ImportExtractionJob) (err error) {
	root, err := w.stagingRoot(ctx)
	if err != nil {
		return err
	}
	jobDir := filepath.Join(root, strconv.FormatInt(job.ID, 10))

	// A stale directory for this id can only be a leftover from an interrupted
	// run; start from a clean slate.
	if err := os.RemoveAll(jobDir); err != nil {
		return fmt.Errorf("extract: clear staging %s: %w", jobDir, err)
	}
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		return fmt.Errorf("extract: create staging %s: %w", jobDir, err)
	}
	ok := false
	defer func() {
		if !ok {
			if rmErr := os.RemoveAll(jobDir); rmErr != nil {
				w.logf("extract: job %d: remove staging %s: %v", job.ID, jobDir, rmErr)
			}
		}
	}()

	// The donor is the file selected at approval time; the queue row's
	// source_file_path is the candidate drop and must never stand in for it.
	donor := job.DonorPath
	if strings.TrimSpace(donor) == "" {
		return fmt.Errorf("extract: job %d: no donor paired", job.ID)
	}
	info, err := os.Stat(donor)
	if err != nil {
		return fmt.Errorf("extract: donor %s: %w", donor, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("extract: donor %s is not a regular file", donor)
	}

	media, err := w.prober.Probe(ctx, donor)
	if err != nil {
		return fmt.Errorf("extract: probe donor: %w", err)
	}
	tracks, err := w.selectTracks(ctx, job, media)
	if err != nil {
		return err
	}

	mkvTracks, err := w.audioTracks(ctx, donor)
	if err != nil {
		return err
	}
	if len(mkvTracks) != len(media.Tracks) {
		return fmt.Errorf("extract: donor %s: mkvmerge reports %d audio tracks, probe reports %d",
			donor, len(mkvTracks), len(media.Tracks))
	}

	bin, err := w.mkvextractBinary()
	if err != nil {
		return err
	}
	base := strings.TrimSuffix(job.Filename, filepath.Ext(job.Filename))
	args := []string{"tracks", donor}
	outputs := make([]string, 0, len(tracks))
	durations := make([]time.Duration, 0, len(tracks))
	for _, track := range tracks {
		// mkvmerge's audio ids are not ffprobe stream indices: the video stream
		// shifts every container index. Pair the probed track with the mkvmerge
		// entry at the same position among the audio streams instead.
		pos, err := audioPosition(media.Tracks, track.Index)
		if err != nil {
			return fmt.Errorf("extract: donor %s: %w", donor, err)
		}
		lang := track.LanguageISO1
		if lang == "" {
			lang = "und"
		}
		out := filepath.Join(jobDir, fmt.Sprintf("%s.%s.%d.mka", base, lang, track.Index))
		args = append(args, fmt.Sprintf("%d:%s", mkvTracks[pos].ID, out))
		outputs = append(outputs, out)
		// Validate against this track's own donor duration: donor audio tracks
		// may differ in length, so the container duration is only a fallback.
		duration := mkvTracks[pos].Duration
		if duration <= 0 {
			duration = media.Duration
		}
		durations = append(durations, duration)
	}
	if outp, err := w.run(ctx, bin, args...); err != nil {
		return fmt.Errorf("extract: mkvextract %s: %w: %s", donor, err, firstLine(outp))
	}

	for i, out := range outputs {
		if err := validateExtraction(ctx, w.prober, out, tracks[i], durations[i]); err != nil {
			return err
		}
	}

	// Hand the validated staging output to the vault pipeline (M3-4). The hook
	// owns the staged files on success; on failure the deferred cleanup below
	// wipes the job directory, matching the extraction failure contract.
	if w.vaultHook != nil {
		staged := make([]StagedTrack, 0, len(outputs))
		for i, out := range outputs {
			info, err := w.prober.Probe(ctx, out)
			if err != nil {
				return fmt.Errorf("extract: probe staged output %s: %w", out, err)
			}
			got := info.Tracks[0]
			staged = append(staged, StagedTrack{
				Path:         out,
				Language:     trackLanguage(tracks[i]),
				Codec:        got.Codec,
				Channels:     channelLayout(got),
				ChannelCount: got.Channels,
				BitrateBPS:   got.BitrateBPS,
				SampleRate:   got.SampleRate,
				Duration:     info.Duration,
			})
		}
		if err := w.vaultHook(ctx, job, staged); err != nil {
			return fmt.Errorf("extract: vault: %w", err)
		}
	}
	ok = true
	return nil
}

// trackLanguage is the token the vault uses to name the archived file: the
// normalized ISO 639-1 code when the donor tag resolved, else the ISO 639-2/B
// form (the vault rejects "und", which the export policy never selects).
func trackLanguage(track probe.AudioTrack) string {
	if track.LanguageISO1 != "" {
		return track.LanguageISO1
	}
	return track.LanguageISO2B
}

// channelLayout is the human-readable layout stored with the vault entry,
// normalizing ffprobe's mono/stereo names to the "1.0"/"2.0" form.
func channelLayout(track probe.AudioTrack) string {
	switch track.ChannelLayout {
	case "mono":
		return "1.0"
	case "stereo":
		return "2.0"
	case "":
		if track.Channels > 0 {
			return fmt.Sprintf("%d.0", track.Channels)
		}
	}
	return track.ChannelLayout
}

// selectTracks applies the settled export policy to the donor's probed audio
// tracks. export_mode "all" additionally keeps extra tracks that carry a real
// language; untagged tracks stay manual-pick only (glossary) and commentary is
// already removed by SplitExportTracks when excluded.
func (w *Worker) selectTracks(ctx context.Context, job store.ImportExtractionJob, media *probe.MediaInfo) ([]probe.AudioTrack, error) {
	if len(media.Tracks) == 0 {
		return nil, fmt.Errorf("extract: donor %s has no audio tracks", job.DonorPath)
	}
	policy := settings.LanguagesPolicy{}
	if w.settings != nil {
		if raw, ok, err := w.settings.Get(ctx, settings.KeyLanguagesPolicy); err == nil && ok {
			if err := json.Unmarshal(raw, &policy); err != nil {
				return nil, fmt.Errorf("extract: decode languages.policy: %w", err)
			}
		}
	}
	exp := gate.PolicyFromLanguages(policy, job.OriginalLanguage)
	split := gate.SplitExportTracks(media.Tracks, exp)

	selected := split.Policy
	if policy.ExportMode == "all" {
		for _, track := range split.Extras {
			if strings.TrimSpace(track.Language) != "" && !strings.EqualFold(track.Language, "und") {
				selected = append(selected, track)
			}
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("extract: donor %s: no audio track matches the export policy", job.DonorPath)
	}
	return selected, nil
}

// mkvAudioTrack is one audio track as mkvmerge identifies it: the id
// mkvextract addresses and the track's own duration (zero when the container
// does not expose one).
type mkvAudioTrack struct {
	ID       int
	Duration time.Duration
}

// audioTracks returns mkvmerge's audio track entries for the donor, in
// container order (which matches probe's audio-stream order). mkvextract ids
// are not ffprobe stream indices, so they must come from mkvmerge -J.
func (w *Worker) audioTracks(ctx context.Context, donor string) ([]mkvAudioTrack, error) {
	bin, err := w.mkvmergeBinary()
	if err != nil {
		return nil, err
	}
	outp, err := w.run(ctx, bin, "-J", donor)
	if err != nil {
		return nil, fmt.Errorf("extract: mkvmerge identify %s: %w: %s", donor, err, firstLine(outp))
	}
	var doc struct {
		Tracks []struct {
			ID         int    `json:"id"`
			Type       string `json:"type"`
			Properties struct {
				// Duration is nanoseconds across formats that store it.
				Duration json.RawMessage `json:"duration"`
				// TagDuration is mkvmerge's DURATION statistic
				// ("HH:MM:SS.nnnnnnnnn"), present for most Matroska files.
				TagDuration string `json:"tag_duration"`
			} `json:"properties"`
		} `json:"tracks"`
	}
	if err := json.Unmarshal(outp, &doc); err != nil {
		return nil, fmt.Errorf("extract: decode mkvmerge identification of %s: %w", donor, err)
	}
	tracks := make([]mkvAudioTrack, 0, len(doc.Tracks))
	for _, track := range doc.Tracks {
		if track.Type != "audio" {
			continue
		}
		duration := parseNanoDuration(track.Properties.Duration)
		if duration <= 0 {
			duration = parseTagDuration(track.Properties.TagDuration)
		}
		tracks = append(tracks, mkvAudioTrack{ID: track.ID, Duration: duration})
	}
	return tracks, nil
}

// audioPosition maps a probed track's container-wide stream index to its
// ordinal among the donor's audio streams; that ordinal pairs it with the
// matching mkvmerge audio entry.
func audioPosition(probed []probe.AudioTrack, streamIndex int) (int, error) {
	for i, track := range probed {
		if track.Index == streamIndex {
			return i, nil
		}
	}
	return -1, fmt.Errorf("audio stream %d not found in probe result", streamIndex)
}

// parseNanoDuration decodes mkvmerge's properties.duration, which is a JSON
// number of nanoseconds (occasionally a numeric string).
func parseNanoDuration(raw json.RawMessage) time.Duration {
	if len(raw) == 0 {
		return 0
	}
	var nanos float64
	if err := json.Unmarshal(raw, &nanos); err != nil {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0
		}
		if v, err := strconv.ParseFloat(text, 64); err == nil {
			nanos = v
		} else {
			return 0
		}
	}
	if nanos <= 0 {
		return 0
	}
	return time.Duration(math.Round(nanos))
}

// parseTagDuration decodes mkvmerge's "HH:MM:SS.nnnnnnnnn" duration statistic.
func parseTagDuration(raw string) time.Duration {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) != 3 {
		return 0
	}
	hours, errH := strconv.Atoi(parts[0])
	minutes, errM := strconv.Atoi(parts[1])
	seconds, errS := strconv.ParseFloat(parts[2], 64)
	if errH != nil || errM != nil || errS != nil || hours < 0 || minutes < 0 || seconds < 0 {
		return 0
	}
	return time.Duration(hours)*time.Hour +
		time.Duration(minutes)*time.Minute +
		time.Duration(math.Round(seconds*float64(time.Second)))
}

// validateExtraction re-probes an extracted file and checks it against the
// donor track it came from.
func validateExtraction(ctx context.Context, prober *probe.Prober, path string, donor probe.AudioTrack, donorDuration time.Duration) error {
	extracted, err := prober.Probe(ctx, path)
	if err != nil {
		return fmt.Errorf("extract: probe output %s: %w", path, err)
	}
	if len(extracted.Tracks) != 1 {
		return fmt.Errorf("extract: output %s has %d audio tracks, want exactly 1", path, len(extracted.Tracks))
	}
	got := extracted.Tracks[0]
	if !strings.EqualFold(got.Codec, donor.Codec) {
		return fmt.Errorf("extract: output %s codec %q, donor %q", path, got.Codec, donor.Codec)
	}
	if got.Channels != donor.Channels {
		return fmt.Errorf("extract: output %s has %d channels, donor %d", path, got.Channels, donor.Channels)
	}
	if delta := extracted.Duration - donorDuration; delta > DurationTolerance || delta < -DurationTolerance {
		return fmt.Errorf("extract: output %s duration %v differs from donor %v by more than %v",
			path, extracted.Duration, donorDuration, DurationTolerance)
	}
	return nil
}

func (w *Worker) mkvmergeBinary() (string, error) {
	return resolveTool(w.mkvmerge, "mkvmerge")
}

func (w *Worker) mkvextractBinary() (string, error) {
	return resolveTool(w.mkvextract, "mkvextract")
}

func resolveTool(pinned, name string) (string, error) {
	if pinned != "" {
		return pinned, nil
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("extract: %s not found on PATH: %w", name, err)
	}
	return path, nil
}

// firstLine keeps command diagnostics to one bounded line for the stored
// failure reason and logs.
func firstLine(out []byte) string {
	text := strings.TrimSpace(string(out))
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		text = text[:idx]
	}
	const limit = 500
	if len(text) > limit {
		text = text[:limit]
	}
	if text == "" {
		return "(no output)"
	}
	return text
}
