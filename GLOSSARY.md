# Dubarr

Dubarr archives dubbing/lektor audio from donor files and muxes it, time-synchronized, into tagged target MKVs in a Radarr/Sonarr-managed library. This glossary fixes the language for that workflow.

## Language

### Media & flow

**Donor**:
An incoming video file in the Watch Folder whose audio tracks are wanted (e.g. Polish dubbing or lektor), typically a lower-quality release. Its selected tracks are extracted losslessly into the Audio Vault after approval.
_Avoid_: source, upload, input file

**Target**:
The library MKV into which a synchronized track is muxed. Its video stream is never transcoded or altered.
_Avoid_: destination, output file

**Audio Vault**:
The write-once archive of extracted audio tracks, kept separate from the video library so that deleting or replacing a video never deletes archived audio.
_Avoid_: library, archive, repository

**Donor Group**:
All tracks extracted from one donor file, linked so a sibling track can serve as the timing reference for another.
_Avoid_: group_uuid

**Watch Folder**:
The dropzone scanned for new donors; a file is only picked up once its size is stable across two checks.
_Avoid_: inbox

**Work Area**:
The isolated scratch space where every job operates on verified copies of vault tracks; nothing is ever modified in place in the Vault.
_Avoid_: temp, staging

**Copy-on-Work**:
The contract that every sync, preview, tempo adjustment and mux first copies the needed vault tracks into the Work Area and verifies their checksums before any processing.
_Avoid_: in-place editing

**Approval Queue**:
The queue of detected donors awaiting explicit user approval before any extraction into the Vault; tracks are pre-selected by the Export Policy.
_Avoid_: import list

### Sync

**Reference Track**:
A sibling donor track or target stream used as the timing reference for cross-correlation, chosen by the Reference Language resolution order.
_Avoid_: guide track, anchor track

**Reference Language**:
The language used to pick the Reference Track: original language first, then any other shared language, then a low-pass fallback against the target's original audio.
_Avoid_: sync language

**Target Language**:
The two-letter language of the track being muxed into the Target (e.g. pl).
_Avoid_: dub language, import language

**Original Language**:
The library's original language for the media (from Radarr/Sonarr), used as the first-choice Reference Language.
_Avoid_: source language

**Sync Offset**:
The constant time shift applied to the donor track, defined as offset_ms = t_target − t_donor; positive delays the donor, negative advances it.
_Avoid_: delay, shift, lag

**Checkpoint**:
One sampling point along the timeline where donor and reference are cross-correlated; several checkpoints reveal constant offset, linear drift or mid-file cuts.
_Avoid_: anchor, sync point

**Preview Slice**:
A short clip generated for visual inspection in Sync Studio; distinct from an analysis Checkpoint.
_Avoid_: checkpoint (when meaning the studio clip)

**Drift**:
A speed/FPS mismatch between donor and target detected as a linear trend across checkpoints, expressed as a ratio and corrected by tempo adjustment.
_Avoid_: speedup, desync

**Variant**:
The classification of an audio track: dubbing, lektor, original, standard or commentary; commentary is excluded by policy.
_Avoid_: type, kind

**Lektor**:
A Polish voice-over variant where a single reader narrates over the original audio, distinct from full dubbing.
_Avoid_: voice-over (when meaning the Polish variant)

**Untagged Track**:
A donor audio stream whose language is unknown; it is surfaced for explicit manual selection and never auto-imported by the Export Policy.
_Avoid_: unknown track

### Policy & eligibility

**Export Policy**:
The configurable rules deciding which donor tracks are pre-selected and kept: target languages, original language, extra reference languages, commentary exclusion.
_Avoid_: language rules, import policy

**Tdarr Gate**:
The filename-tag eligibility check ensuring only Tdarr-processed targets are synced/muxed; donor ingestion into the Vault is never gated.
_Avoid_: tag filter (when meaning the eligibility concept)

**Eligibility Status**:
A target's state: eligible (required tags present), waiting_for_tags (tags missing, blocked while enforced), or missing_target (no file found).
_Avoid_: tag status
