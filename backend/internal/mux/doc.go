// Package mux publishes a synchronized audio track into a Tdarr-tagged target
// MKV without transcoding it (PLAN.md M5-1, ADR-0004).
//
// [Muxer.Mux] is the pipeline behind the future POST /api/sync/jobs/{id}/mux:
//
//  1. Refuse a sync report whose verdict is not ok|stable. Drifting,
//     unreliable and out_of_range jobs never reach mkvmerge.
//  2. Loud-fail an ambiguous pairing before any write: when the target
//     filename resolves to several queued vault tracks, or the report's
//     language does not map to exactly one of them, the call returns
//     [ErrAmbiguous] and leaves every directory byte-identical.
//  3. Re-check the Processing Gate against the base filename and, when the
//     arr source is active, the target's cached arr tags/root folder unless the
//     caller forces an override (ADR-0005, ADR-0009); ineligible targets return
//     [ErrIneligible].
//  4. Copy the target into {WORK_DIR}/jobs/{id}/ (Copy-on-Work, ADR-0002) and
//     let mkvmerge read only that copy plus the staged .mka. The target itself
//     is opened read-only.
//  5. Append the .mka with `--language 0:<iso2> --track-name 0:<title>
//     --sync 0:<offset>` (IDEA.md:397-403). The target's video and existing
//     audio are copied as-is, never transcoded.
//  6. Validate the muxed temp with ffprobe (ADR-0004): the probe must succeed
//     and exactly one audio track must have been appended. A failure returns
//     [ErrPublish] with the original untouched.
//  7. Publish atomically: mkvmerge wrote a uniquely named hidden temp
//     (<target>.mkv.dubarr-<rand>.tmp) in the target's directory (same
//     filesystem), the original is copied to <target>.mkv.bak through its own
//     unique temp, then the muxed temp is renamed over the original. The .bak
//     is the previous good copy and is overwritten by design; cleaning it is an
//     opt-in later step.
//
// Any failure removes only the temp files this call created and leaves the
// original untouched. WORK_DIR staging is the caller's to clean up, and
// VAULT_DIR is never opened for writing.
package mux
