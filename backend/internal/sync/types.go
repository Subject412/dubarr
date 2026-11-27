package sync

import (
	"fmt"
	"math"
)

// Job is one correlation job, run in-process against the Go DSP engine.
//
// TargetPath and DonorPath are staged WAV files; SliceStartSec/SliceLenSec
// select the window analysed in both, on the same timeline positions.
// WindowSec bounds the absolute offset search and defaults to
// [DefaultWindowSec] when nil.
type Job struct {
	TargetPath    string
	DonorPath     string
	SliceStartSec float64
	SliceLenSec   float64
	WindowSec     *float64
}

// Analyzed is the window that was actually analysed, in seconds
// (sync-engine/src/job.rs `Analyzed`).
type Analyzed struct {
	Start float64 `json:"start"`
	Len   float64 `json:"len"`
}

// Result is one correlation's answer. OffsetMS = target time - donor time, so a
// positive value means the donor track must be delayed.
type Result struct {
	OffsetMS           float64  `json:"offset_ms"`
	CorrelationQuality float64  `json:"correlation_quality"`
	SampleRate         uint32   `json:"sample_rate"`
	Analyzed           Analyzed `json:"analyzed"`
}

// validate rejects a job the engine would reject, before any file is opened. It
// mirrors the cheap checks in job.rs, with the same wording.
func (j Job) validate() error {
	if j.TargetPath == "" {
		return fmt.Errorf("target source is empty")
	}
	if j.DonorPath == "" {
		return fmt.Errorf("donor source is empty")
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
