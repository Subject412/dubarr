// Package mux publishes a synchronized audio track into a tag-eligible target
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
//  4. Refuse a non-Matroska target or destination before any write: mkvmerge
//     always writes Matroska, so anything but .mkv/.mka returns
//     [ErrUnsupportedContainer] and leaves the target, the destination
//     directory and the temp folder byte-identical.
//  5. Copy the target into {WORK_DIR}/jobs/{id}/ (Copy-on-Work, ADR-0002) and
//     let mkvmerge read only that copy plus the staged .mka. The target itself
//     is opened read-only.
//  6. Append the .mka with `--language 0:<iso2> --track-name 0:<title>
//     --sync 0:<offset>` (ADR-0004). <title> is the language's own name
//     ("Polish"), never a configured string, and [DefaultTrackTitle] falls back
//     to a dub marker only for a language it cannot resolve. <iso2> is the
//     job's language as mkvmerge accepts it: a language that cannot be
//     resolved to an ISO 639-2 code ("zz") is written as `und` with one log
//     line, so the container stays valid and the job still publishes. The
//     target's video and existing audio are copied as-is, never transcoded.
//  7. Validate the muxed temp with ffprobe (ADR-0004): the probe must succeed
//     and exactly one audio track must have been appended. A failure returns
//     [ErrPublish] with the original untouched.
//  8. Publish atomically: mkvmerge wrote a uniquely named hidden temp
//     (<target>.mkv.dubarr-<rand>.tmp) in the target's directory (same
//     filesystem), then the validated muxed temp is renamed over the original.
//     No .bak sidecar is written: the pre-rename ffprobe validation in step 7
//     is the safety net, and a pre-existing backup file beside the target is
//     never created, replaced or deleted by this call.
//
// Any failure removes only the temp files this call created and leaves the
// original untouched. Mux owns its job staging directory and removes
// {WORK_DIR}/jobs/{id}/ on every return that created it, so no caller has to;
// that removal also takes the inputs [Result].WorkCopy and
// [Result].AudioPath name, so those paths no longer exist once Mux returns.
// VAULT_DIR is never opened for writing.
package mux
