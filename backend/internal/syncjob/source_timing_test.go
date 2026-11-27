package syncjob

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/probe"
)

// TestSourceTimingsProbesBothInputsOnce pins the shape of U-WIN.1's numbers:
// the target's length comes from the analysed stream with the format duration
// as fallback (Matroska says N/A per stream), the donor's from its audio-only
// container, and each input is probed exactly once, in the order the job
// decodes them.
func TestSourceTimingsProbesBothInputsOnce(t *testing.T) {
	const (
		targetPath = "library/Shrek.mkv"
		donorPath  = "work/donor.mka"
	)
	targetInfo := probe.MediaInfo{
		Path:           targetPath,
		Format:         "matroska,webm",
		Duration:       5431772 * time.Millisecond,
		DurationMS:     5431772,
		StartTime:      0,
		StartTimeKnown: true,
		Tracks: []probe.AudioTrack{{
			Index:          1,
			Duration:       0, // ffprobe reports `duration: N/A` for the stream
			StartTime:      500 * time.Millisecond,
			StartTimeKnown: true,
		}},
	}
	donorInfo := probe.MediaInfo{
		Path:           donorPath,
		Format:         "matroska,webm",
		Duration:       5431232 * time.Millisecond,
		DurationMS:     5431232,
		StartTime:      20 * time.Millisecond,
		StartTimeKnown: true,
		Tracks: []probe.AudioTrack{{
			Index:          0,
			StartTime:      20 * time.Millisecond,
			StartTimeKnown: true,
		}},
	}

	var probed []string
	svc := &Service{probeTimings: func(_ context.Context, path string) (probe.MediaInfo, error) {
		probed = append(probed, path)
		switch path {
		case targetPath:
			return targetInfo, nil
		case donorPath:
			return donorInfo, nil
		}
		return probe.MediaInfo{}, fmt.Errorf("unexpected probe of %q", path)
	}}

	timings, err := svc.sourceTimings(context.Background(), nil, targetPath, 1, donorPath)
	if err != nil {
		t.Fatalf("sourceTimings: %v", err)
	}
	if len(probed) != 2 || probed[0] != targetPath || probed[1] != donorPath {
		t.Fatalf("probed %v, want [%s %s]", probed, targetPath, donorPath)
	}

	if got, want := timings.Target.Duration, 5431772*time.Millisecond; got != want {
		t.Errorf("target duration = %v, want %v (format fallback)", got, want)
	}
	if timings.Target.ContainerStart != 0 || timings.Target.TrackStart != 500*time.Millisecond {
		t.Errorf("target starts = %v/%v, want 0/500ms",
			timings.Target.ContainerStart, timings.Target.TrackStart)
	}
	if !timings.Target.Known {
		t.Error("target Known = false, want true (duration and both origins stated)")
	}

	if got, want := timings.Donor.Duration, 5431232*time.Millisecond; got != want {
		t.Errorf("donor duration = %v, want %v", got, want)
	}
	if !timings.Donor.Known {
		t.Error("donor Known = false, want true")
	}

	// DURATION BASE: the donor is the shorter input, so it is the base.
	if timings.Donor.Duration >= timings.Target.Duration {
		t.Errorf("donor %v is not shorter than target %v",
			timings.Donor.Duration, timings.Target.Duration)
	}
}

// TestSourceTimingUnknownIsNotZero: an absent duration or origin must stay
// unknown. The two failure shapes are distinguishable — Duration == 0 means
// "no duration", Duration > 0 with Known false means "unknown origin" — and
// neither is a silent 0 that percentages could consume.
func TestSourceTimingUnknownIsNotZero(t *testing.T) {
	// Duration known, origins omitted.
	originUnknown := sourceTiming(probe.MediaInfo{
		Duration: 100 * time.Second,
		Tracks:   []probe.AudioTrack{{Index: 0}},
	}, 0)
	if originUnknown.Duration != 100*time.Second {
		t.Errorf("duration = %v, want 100s", originUnknown.Duration)
	}
	if originUnknown.Known {
		t.Error("Known = true without a container or track start_time")
	}
	if originUnknown.ContainerStart != 0 || originUnknown.TrackStart != 0 {
		t.Errorf("starts = %v/%v, want the zero values alongside Known=false",
			originUnknown.ContainerStart, originUnknown.TrackStart)
	}

	// Nothing known at all.
	nothing := sourceTiming(probe.MediaInfo{}, 0)
	if nothing.Known || nothing.Duration != 0 {
		t.Errorf("empty probe = %+v, want the zero value with Known=false", nothing)
	}

	// A track that is not in the document: no track origin, so not Known even
	// when the container duration exists.
	missingTrack := sourceTiming(probe.MediaInfo{
		Duration:       100 * time.Second,
		StartTimeKnown: true,
	}, 3)
	if missingTrack.Known {
		t.Error("Known = true for a track index the document does not contain")
	}
}

// TestSourceTimingsProbeError: a probe failure is reported, never folded into
// an unknown timing, because "ffprobe could not read this file" is not the same
// fact as "the container is silent about its duration". The caller is what
// degrades - it logs and falls back to whole-file analysis - so the distinction
// survives in the log even though the job continues.
func TestSourceTimingsProbeError(t *testing.T) {
	boom := errors.New("ffprobe exploded")
	svc := &Service{probeTimings: func(context.Context, string) (probe.MediaInfo, error) {
		return probe.MediaInfo{}, boom
	}}
	_, err := svc.sourceTimings(context.Background(), nil, "target.mkv", 0, "donor.mka")
	if !errors.Is(err, boom) {
		t.Fatalf("sourceTimings error = %v, want wrapping %v", err, boom)
	}
}
