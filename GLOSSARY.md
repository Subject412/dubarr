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

**Temp Folder**:
The scratch space named by `work_dir` where every job operates on verified copies of vault tracks; it holds files only while a job runs and is cleared afterwards. Nothing is ever modified in place in the Audio Vault.
_Avoid_: work area, workspace, staging, scratch dir

**Copy-on-Work**:
The contract that every sync, preview, tempo adjustment and mux first copies the needed vault tracks into the temp folder and verifies their checksums before any processing.
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

**Technique**:
One matcher the sync engine can run on a checkpoint — `waveform` (FFT cross-correlation of the raw samples) or `mfcc` (mel-cepstral correlation, more tolerant of a re-encoded pair and coarser in precision). A technique entry is a rule, not just a name: which matcher, enabled or not, its position in the list, and the condition it runs under.
_Avoid_: matcher (when meaning the configured entry), algorithm

**Technique Pipeline**:
The two-phase routing that applies the technique list. Phase one runs each enabled technique on a checkpoint in list order and keeps the first answer that clears its own gates; phase two gives the `above_drift` entries their turn on the checkpoints that do not sit on the fitted line, once, after every first answer is in.
_Avoid_: strategy chain, fallback ladder

**Match Z**:
The z-score of the winning lag on a checkpoint, measured over the whole lag curve rather than fitted through a few numbers, so it says how sure the match is. `0` means no z was scored — including every report written before 0.5.0 — and is read as "not recorded", never as "unsure". Sync Studio shows it as the **Match z** column of the checkpoint table.
_Avoid_: confidence score, correlation quality (that is the raw peak value, not the z)

**Precision**:
How finely a checkpoint's offset can be placed, in milliseconds: one analysis sample (0.125 ms at 8 kHz) for the waveform technique, one hop for MFCC. A report states the coarsest precision among its checkpoints, so it never claims more resolution than its worst one. Sync Studio renders it as a **±… ms** badge beside the offset, and hides it for the zero sentinel.
_Avoid_: accuracy, error

**Preview Slice**:
A short clip generated for visual inspection in Sync Studio; distinct from an analysis Checkpoint.
_Avoid_: checkpoint (when meaning the studio clip)

**Drift**:
A speed/FPS mismatch between donor and target: the checkpoint offsets form a straight, sloped line (high R², slope above the floor), so the whole track is corrected in one ffmpeg pass by a tempo factor, `atempo = 1 − slope/1000`. Sync Studio badges the fit shape as `drift`.
_Avoid_: speedup, desync

**Cut**:
The other shape a line fit tells apart: the offset steps once and is flat on both sides, because the film was re-cut or a scene was inserted, and no tempo factor can fix it. A cut needs per-segment work (see Piecewise Sync); Sync Studio badges the shape as `cut` and names the step position.
_Avoid_: jump, splice, edit (when meaning the shape)

**Piecewise Sync**:
The opt-in path for a cut dub: the analysis samples the whole film densely, a split-penalty dynamic program locates the joins, and each part of the dub is shifted on its own, in one re-encode of the dub copy. A join is only accepted where both neighbouring runs are confident and the step clears the floor; otherwise the plan is refused and the job surfaces for review. Off in every preset; the settings toggle is **Handle films with a cut or an inserted scene**.
_Avoid_: segmented sync, chunked mux

**Segment**:
One run of the film during which the offset is constant, stated as a start, an end and an offset on the target timeline. An accepted plan's segments must tile the film — first at 0, last at the film's own end, contiguous in between — or the render refuses the plan.
_Avoid_: chunk, part, piece

**Boundary**:
Where one segment ends and the next begins, in film time, carrying the offset step across it and an uncertainty of half the sampling step. A boundary is a claim about a render, so it is only reported where the evidence supports it; the render is what proves it.
_Avoid_: join, split point

**Birth**:
The moment the sync job row is created, which is after extraction has written the vault track — not when the operator approves, because `sync_mux_jobs.target_track_id` is NOT NULL and the vault track does not exist yet at approval. The birth is also where the Processing Gate is consulted, deciding `queued` versus `held`.
_Avoid_: job creation, insert (when meaning this moment)

**Held**:
A sync job that exists but may not start yet: the approval is recorded, the job was born `held` with a reason (`waiting_for_tags`, `missing_target`, `target_changed`), and it promotes itself to `queued` the next time eligibility is projected — no second approval. A held job is not a failure; a withdrawn approval cancels it with `cancelled_by_rejection`. The UI calls it **waiting** and shows the reason as a sentence.
_Avoid_: blocked, stalled, pending

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
An ordered condition/action pair applied to the temp-folder copy of a vault track before muxing. The first matching rule selects the output codec, channels, bitrate and sample rate; with no rules the track is copied untouched. Rules never modify the Audio Vault.
_Avoid_: profile, preset (when meaning a single rule)

**Application**:
The report's own record of what was done to the dub copy before the mux: `copy` with a note, or `re-encoded` with either the tempo factor or the segment count and the segments themselves. It exists so a job can say in words why its stored offset is 0 instead of leaving the operator to infer it, and it is absent on a plain copy.
_Avoid_: processing log, journal

**Log Sink**:
A destination for redacted log entries: stderr, a bounded in-memory ring buffer for the GUI, or an optional rotating file under DATA_DIR.
_Avoid_: log target

### Library ownership

**Media Manager**:
A Radarr or Sonarr instance whose library Dubarr mirrors. Dubarr has no TMDB/TVDB client of its own: it stores the identifiers the media manager reports and never resolves metadata itself (ADR-0014).
_Avoid_: arr (ambiguous — say which), metadata source

**Mirror Library**:
Dubarr's read-only view of a Media Manager's movies, series and episodes — titles, years, identifiers and target paths. It is a clone, never a second source of truth: Dubarr never writes it, and a title leaving arr leaves the mirror but not the Audio Vault.
_Avoid_: cache (when meaning the mirrored library), local library

**Track Identity**:
What makes a Vault track the same track across time: the arr-reported TMDB/TVDB id plus language and edition. It survives an arr reinstall and is preserved when audio is replaced, so existing jobs and Targets stay valid (ADR-0015).
_Avoid_: track id, row id

**Orphaned Track**:
A Vault track whose title has disappeared from the Mirror Library. It is marked, never deleted: a media manager's library cleanup is not a decision about Dubarr's archive.
_Avoid_: missing, stale, deleted

**Vault Trash**:
The holding area a removed track, or audio replaced by a better source, is moved to. Emptying it is a separate, explicit purge, and nothing reaches it except an operator action.
_Avoid_: recycle bin, deleted

**Vault Reorganisation**:
Applying the vault naming scheme to files already archived — a previewed plan of source-to-destination moves, applied only on approval. Planned (issue #47), not implemented.
_Avoid_: rename job, cleanup

**Pipeline Tag**:
A tag in the target's filename that marks a stage of the operator's own pipeline
rather than a property of the match — in the reference setup, "this file has been
through the upstream video pipeline". Muxing before that stage is wasted rather
than destructive: the pipeline replaces the file, so the mux goes with it and
dubarr would mux again once the tag appears. Eligibility can also come from arr
tags on the title, not only from the filename (ADR-0005, ADR-0017).
_Avoid_: quality tag, match tag

