package extract

import (
	"context"
	"encoding/json"
	"fmt"
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

	donor := job.SourceFilePath
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

	mkvIDs, err := w.audioTrackIDs(ctx, donor)
	if err != nil {
		return err
	}
	if len(mkvIDs) != len(media.Tracks) {
		return fmt.Errorf("extract: donor %s: mkvmerge reports %d audio tracks, probe reports %d",
			donor, len(mkvIDs), len(media.Tracks))
	}

	bin, err := w.mkvextractBinary()
	if err != nil {
		return err
	}
	base := strings.TrimSuffix(job.Filename, filepath.Ext(job.Filename))
	args := []string{"tracks", donor}
	outputs := make([]string, 0, len(tracks))
	for _, track := range tracks {
		lang := track.LanguageISO1
		if lang == "" {
			lang = "und"
		}
		out := filepath.Join(jobDir, fmt.Sprintf("%s.%s.%d.mka", base, lang, track.Index))
		args = append(args, fmt.Sprintf("%d:%s", mkvIDs[track.Index], out))
		outputs = append(outputs, out)
	}
	if outp, err := w.run(ctx, bin, args...); err != nil {
		return fmt.Errorf("extract: mkvextract %s: %w: %s", donor, err, firstLine(outp))
	}

	for i, out := range outputs {
		if err := validateExtraction(ctx, w.prober, out, tracks[i], media.Duration); err != nil {
			return err
		}
	}
	ok = true
	return nil
}

// selectTracks applies the settled export policy to the donor's probed audio
// tracks. export_mode "all" additionally keeps extra tracks that carry a real
// language; untagged tracks stay manual-pick only (glossary) and commentary is
// already removed by SplitExportTracks when excluded.
func (w *Worker) selectTracks(ctx context.Context, job store.ImportExtractionJob, media *probe.MediaInfo) ([]probe.AudioTrack, error) {
	if len(media.Tracks) == 0 {
		return nil, fmt.Errorf("extract: donor %s has no audio tracks", job.SourceFilePath)
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
		return nil, fmt.Errorf("extract: donor %s: no audio track matches the export policy", job.SourceFilePath)
	}
	return selected, nil
}

// audioTrackIDs returns mkvextract's track ids for the donor's audio tracks,
// in container order (which matches probe's audio-stream order). mkvextract
// ids are not ffprobe stream indices, so they must come from mkvmerge -J.
func (w *Worker) audioTrackIDs(ctx context.Context, donor string) ([]int, error) {
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
			ID   int    `json:"id"`
			Type string `json:"type"`
		} `json:"tracks"`
	}
	if err := json.Unmarshal(outp, &doc); err != nil {
		return nil, fmt.Errorf("extract: decode mkvmerge identification of %s: %w", donor, err)
	}
	ids := make([]int, 0, len(doc.Tracks))
	for _, track := range doc.Tracks {
		if track.Type == "audio" {
			ids = append(ids, track.ID)
		}
	}
	return ids, nil
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
