package sync

import (
	"fmt"
	"math"
)

// PCM is an inline raw mono PCM slice (sync-engine/src/job.rs `PcmSlice`).
type PCM struct {
	SampleRate uint32    `json:"sample_rate"`
	Samples    []float32 `json:"samples"`
}

// Source is either a WAV file path or inline PCM. It mirrors the untagged Rust
// `Source` enum: exactly one of Path/PCM is set, and the JSON carries only that
// key ("path" or "pcm").
type Source struct {
	Path string `json:"path,omitempty"`
	PCM  *PCM   `json:"pcm,omitempty"`
}

// PathSource returns a file-backed source.
func PathSource(path string) Source { return Source{Path: path} }

// PCMSource returns an inline-samples source.
func PCMSource(sampleRate uint32, samples []float32) Source {
	return Source{PCM: &PCM{SampleRate: sampleRate, Samples: samples}}
}

// Job is the object written to the engine's stdin (sync-engine/src/job.rs
// `Job`). SliceStartSec/SliceLenSec select the window; WindowSec bounds the
// offset search and defaults to the engine's 5 s when nil.
type Job struct {
	Target        Source   `json:"target"`
	Donor         Source   `json:"donor"`
	SliceStartSec float64  `json:"slice_start_sec"`
	SliceLenSec   float64  `json:"slice_len_sec"`
	WindowSec     *float64 `json:"window_sec,omitempty"`
}

// Analyzed is the window the engine actually analysed, in seconds
// (sync-engine/src/job.rs `Analyzed`).
type Analyzed struct {
	Start float64 `json:"start"`
	Len   float64 `json:"len"`
}

// Result is the object the engine prints on stdout (sync-engine/src/job.rs
// `SyncResult`). OffsetMS = target time - donor time, so a positive value means
// the donor track must be delayed.
type Result struct {
	OffsetMS           float64  `json:"offset_ms"`
	CorrelationQuality float64  `json:"correlation_quality"`
	SampleRate         uint32   `json:"sample_rate"`
	Analyzed           Analyzed `json:"analyzed"`
}

// validate rejects jobs the engine would reject with exit 2, before spawning
// the binary. It intentionally mirrors only the cheap checks in job.rs.
func (j Job) validate() error {
	if err := j.Target.validate("target"); err != nil {
		return err
	}
	if err := j.Donor.validate("donor"); err != nil {
		return err
	}
	if math.IsNaN(j.SliceStartSec) || math.IsInf(j.SliceStartSec, 0) || j.SliceStartSec < 0 {
		return fmt.Errorf("slice_start_sec must be a non-negative number")
	}
	if math.IsNaN(j.SliceLenSec) || math.IsInf(j.SliceLenSec, 0) || j.SliceLenSec <= 0 {
		return fmt.Errorf("slice_len_sec must be a positive number")
	}
	if j.WindowSec != nil {
		w := *j.WindowSec
		if math.IsNaN(w) || math.IsInf(w, 0) || w <= 0 {
			return fmt.Errorf("window_sec must be a positive number")
		}
	}
	return nil
}

func (s Source) validate(which string) error {
	hasPath := s.Path != ""
	hasPCM := s.PCM != nil
	switch {
	case hasPath && hasPCM:
		return fmt.Errorf("%s must be either a path or inline PCM, not both", which)
	case !hasPath && !hasPCM:
		return fmt.Errorf("%s source is empty", which)
	}
	if hasPCM {
		if s.PCM.SampleRate == 0 {
			return fmt.Errorf("%s PCM sample_rate must be greater than zero", which)
		}
		for _, sample := range s.PCM.Samples {
			if math.IsNaN(float64(sample)) || math.IsInf(float64(sample), 0) {
				return fmt.Errorf("%s PCM samples must all be finite numbers", which)
			}
		}
	}
	return nil
}
