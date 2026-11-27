package mfcc

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/dsp/wav"
)

// This file replays the committed capture of U-DSP.1 through the mel-cepstral
// matcher: #83's "reproduces the waveform matcher's offset within a few ms"
// measured on real audio, with a tolerance the 10 ms hop can actually meet.
//
// It reads the excerpts under internal/dsp/testdata and adds nothing there;
// TestGoldenExcerpts in internal/dsp/correlate stays the 0.4.0 parity record.
const (
	mfccGoldenDir = "../testdata"

	// mfccGoldenCleanQuality is the recorded waveform prominence at or above
	// which an entry is a real match. Entries below it are skipped by name in
	// the test body: the waveform matcher does not trust them either, so they
	// measure nothing about either matcher.
	mfccGoldenCleanQuality = 0.5
)

// The offset tolerance is testdata/expected.json's mfcc_max_error_ms — the hop
// plus one grid step of slack, as Fixture design and the test plan state — so a
// regeneration changes it in one place.

// mfccGoldenFile is one committed capture.
type mfccGoldenFile struct {
	Case    string            `json:"case"`
	Entries []mfccGoldenEntry `json:"entries"`
}

// mfccGoldenEntry is one recorded correlation.
type mfccGoldenEntry struct {
	ID        string `json:"id"`
	TargetWav string `json:"target_wav"`
	DonorWav  string `json:"donor_wav"`
	Payload   string `json:"payload"`
	Result    struct {
		OffsetMS           float64 `json:"offset_ms"`
		CorrelationQuality float64 `json:"correlation_quality"`
		SampleRate         uint32  `json:"sample_rate"`
	} `json:"result"`
}

// mfccGoldenPayload is the job the capture tool sent the reference engine.
type mfccGoldenPayload struct {
	SliceStartSec float64 `json:"slice_start_sec"`
	SliceLenSec   float64 `json:"slice_len_sec"`
	WindowSec     float64 `json:"window_sec"`
}

// TestMFCCAgreesWithWaveformOnCommittedExcerpts asserts the mel-cepstral offset
// is within 20 ms of the waveform matcher's recorded offset on every clean
// committed excerpt.
//
// It must not pass by finding nothing: if the capture yields no entry above
// mfccGoldenCleanQuality the test fails, so a broken glob cannot turn the gate
// green.
func TestMFCCAgreesWithWaveformOnCommittedExcerpts(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(mfccGoldenDir, "golden", "*.json"))
	if err != nil {
		t.Fatalf("globbing the goldens: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("no goldens under %s", filepath.Join(mfccGoldenDir, "golden"))
	}

	compared, skipped := 0, 0
	tolerance := loadExpected(t).MFCCMaxErrorMS
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		var golden mfccGoldenFile
		if err := json.Unmarshal(data, &golden); err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		if len(golden.Entries) == 0 {
			t.Fatalf("%s records no entries", path)
		}
		for _, entry := range golden.Entries {
			if entry.Result.CorrelationQuality < mfccGoldenCleanQuality {
				// The waveform matcher's own quality is below the clean band:
				// it sits on a spurious peak, so there is nothing to agree with.
				skipped++
				t.Logf("%s/%s skipped: recorded quality %v < %v",
					golden.Case, entry.ID, entry.Result.CorrelationQuality, mfccGoldenCleanQuality)
				continue
			}
			compared++
			t.Run(golden.Case+"/"+entry.ID, func(t *testing.T) {
				gotMS := replayMFCCGolden(t, entry)
				if delta := math.Abs(gotMS - entry.Result.OffsetMS); delta > tolerance {
					t.Errorf("offset = %v ms, waveform matcher recorded %v ms (|delta| = %v ms > %v)",
						gotMS, entry.Result.OffsetMS, delta, tolerance)
				}
			})
		}
	}

	if compared == 0 {
		t.Fatalf("no golden entry cleared quality %v (%d skipped); the cross-matcher gate measured nothing",
			mfccGoldenCleanQuality, skipped)
	}
}

// replayMFCCGolden reads one entry's excerpts as the recorded job asked for
// them and returns the matcher's offset in milliseconds.
func replayMFCCGolden(t *testing.T, entry mfccGoldenEntry) float64 {
	t.Helper()

	var job mfccGoldenPayload
	if err := json.Unmarshal([]byte(entry.Payload), &job); err != nil {
		t.Fatalf("%s: parsing the recorded payload: %v", entry.ID, err)
	}
	rate := entry.Result.SampleRate
	if rate == 0 {
		t.Fatalf("%s: the golden records no sample rate", entry.ID)
	}

	target, err := readExcerptMono(entry.TargetWav, job.SliceStartSec, job.SliceLenSec, rate)
	if err != nil {
		t.Fatalf("%s: target: %v", entry.ID, err)
	}
	donor, err := readExcerptMono(entry.DonorWav, job.SliceStartSec, job.SliceLenSec, rate)
	if err != nil {
		t.Fatalf("%s: donor: %v", entry.ID, err)
	}

	got, err := Analyze(context.Background(), target, donor, rate, job.WindowSec)
	if err != nil {
		t.Fatalf("%s: analyze: %v", entry.ID, err)
	}
	gotMS := offsetMillis(got.OffsetSamples, rate)
	t.Logf("%s: offset %v ms (waveform %v ms), quality %v, match_z %v, precision %v ms",
		entry.ID, gotMS, entry.Result.OffsetMS, got.Quality, got.MatchZ, got.PrecisionMS)
	return gotMS
}

// readExcerptMono reads the frames a recorded job asked for, out of the
// committed excerpt. The golden stores repository-relative paths; the file
// lives next to the goldens, under internal/dsp/testdata/wav.
func readExcerptMono(recordedPath string, startSec, lenSec float64, rate uint32) ([]float32, error) {
	if recordedPath == "" {
		return nil, fmt.Errorf("the golden records no WAV path")
	}
	path := filepath.Join(mfccGoldenDir, "wav", filepath.Base(recordedPath))

	reader, err := wav.Open(path)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	if got := reader.SampleRate(); got != rate {
		return nil, fmt.Errorf("%s: sample rate %d Hz, the golden records %d Hz", path, got, rate)
	}

	start := int64(math.Round(startSec * float64(rate)))
	frames := int64(math.Round(lenSec * float64(rate)))
	return reader.ReadMono(start, frames)
}
