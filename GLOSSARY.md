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
The configurable rules deciding which donor tracks are pre-selected and kept: target languages, original language, extra reference languages, commentary exclusion. By default every policy-selected track is exported; keeping tracks beyond the policy requires explicit opt-in.
_Avoid_: language rules, import policy

**Processing Gate**:
The eligibility check deciding whether a Target may be synced, previewed or muxed. It combines a user-configured filename-tag criterion with an optional arr-metadata criterion (arr tags and root folders); the two sources are AND-ed. Donor ingestion into the Audio Vault is never gated (ADR-0005, amended by ADR-0009).

**Eligibility Source**:
The origin of a Processing Gate criterion: `filename` (tags in the Target's basename), `arr` (Radarr/Sonarr tags and root folders), or `both`.
_Avoid_: gate mode

**Eligibility Status**:
A target's state: eligible (all required criteria present), waiting_for_tags (criteria missing, blocked while enforced), or missing_target (no file found).
_Avoid_: tag status

### Arr connections & library

**Draft Test**:
A connection check run against the values currently typed into the form rather than the saved connection, so an operator can validate a base URL and API key before committing them. Nothing is persisted by a Draft Test.
_Avoid_: test connection, ping

**Library Health**:
The per-instance sync state shown to the operator: never synced, failed (the last attempt did not succeed), or up to date. It answers whether the cached arr library can be trusted, separately from whether any donors are waiting.
_Avoid_: sync status, arr status

**Last Seen At**:
The time an arr library item was last present in a completed sync pass; an item missing from the cache has not been observed, which is distinct from having been deleted in arr.
_Avoid_: last updated, synced at

**Manual Refresh**:
An operator-triggered sync pass that shares the single pass slot with the scheduled poller: whichever starts second is refused rather than queued, and a skipped scheduled tick re-arms shortly instead of waiting a full interval.
_Avoid_: force sync, resync

### Queue triage

**Match State**:
A queue card's readiness: unmatched (no arr title identified), matched_no_donor (a target exists but the card is not actionable), or ready (approvable now). `ready` requires everything approval requires, so the state never advertises an action the server would refuse.
_Avoid_: match status, confidence

**Match Blocker**:
The machine-readable reason a card is not ready: no match, no target file, missing or excluded gate tags, an unreadable target folder, or no eligible donor. It exists so the interface never has to infer meaning from the human-readable reason sentence.
_Avoid_: reason, error code

**Identify**:
An operator-initiated lookup that asks the card's own arr instance to parse the release name and returns ranked library candidates. Identify only suggests; a separate explicit pick persists the match, and the server never chooses on the operator's behalf.
_Avoid_: auto-match, guess

**Parse Confidence**:
How a queue row's library match was established. The only value recorded today is `manual` — an operator chose the item by hand — and that is deliberately distinct from a parser-derived match. The other values the schema reserves (`exact`, `ambiguous`, `unmatched`) name parser outcomes and no code path writes them yet.
_Avoid_: score, match percent

### Output and processing

**Output Mode**:
The mux publication policy: `replace` (atomically replace the Target in place, with an optional backup) or `separate` (write the validated result to a configured output directory, leaving the Target untouched).
_Avoid_: mux mode, save mode

**Mux Output Path**:
The final path of a published mux result, recorded on the sync job so the UI can show where the file landed.
_Avoid_: output file (when meaning the recorded path)

**Reference Language Token**:
A reserved entry in Extra reference languages: `org` expands to the arr-reported Original Language, `all` expands to every usable shared language between the Donor Group and the Target.
_Avoid_: magic language code

**Prefer Original Language**:
The policy making the arr-reported Original Language the first-choice Reference Language unless it equals a Target Language.
_Avoid_: original-first flag

**Transcode Rule**:
An ordered condition/action pair applied to the Work Area copy of a vault track before muxing. The first matching rule selects the output codec, channels, bitrate and sample rate; with no rules the track is copied untouched. Rules never modify the Audio Vault.
_Avoid_: profile, preset (when meaning a single rule)

**Log Sink**:
A destination for redacted log entries: stderr, a bounded in-memory ring buffer for the GUI, or an optional rotating file under DATA_DIR.
_Avoid_: log target
