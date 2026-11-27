# ADR drafts for 0.5.0

Three drafts for the orchestrator to commit. They are written in the shape the
existing files use (`docs/adr/0018-publish-by-validated-temp.md` for the long
form, `docs/adr/0004-mkvmerge-for-muxing.md` for the short one), so they can be
copied into place with only a status line to update as the code lands.

Nothing in `docs/adr/**` may be written by a subagent (`AGENTS.md:25`). These are
drafts: the orchestrator commits them, and updates each `Status:` line when the
corresponding slice merges.

- `0019-sync-technique-pipeline.md` — commit with slice 03 (`#82`).
- `0020-piecewise-reencode-of-the-dub-copy.md` — commit with slice 05 (`#84`).
- Amendment to `0004-mkvmerge-for-muxing.md` — commit with slice 05 (`#84`).

---

## 1. `docs/adr/0019-sync-technique-pipeline.md`

```markdown
# Sync techniques are chosen per checkpoint, and one of them is the default

Matching a dub to a film used to be one hardcoded algorithm, so every improvement
changed the result for everybody and an operator with a difficult release had no
lever. The sync engine is now a **strategy**: an ordered list of techniques, each
with a `when` condition (`always`, `below_confidence`, `above_drift`), each with
its own acceptance thresholds. Techniques are evaluated **per checkpoint** — one
window of the film at a time — and the first technique that returns a confident
match decides that checkpoint. A weak checkpoint falls back on itself; the job is
never re-run and never degrades because one point was bad.

The shipped default list contains exactly one entry: waveform FFT
cross-correlation, `when: always`, confidence 0.10, match-z 5.0. That is the 0.4.0
engine, so an untouched install produces byte-identical offsets after the
upgrade, and the operator opts *in* to anything else. The alternative matcher
shipped in 0.5.0 (MFCC, issue #83) is deliberately **not** in the default list:
it trades accuracy for robustness, and that is a choice the operator makes for a
release, not a default they inherit.

## Precision is part of the answer

A technique does not return a bare offset. It returns
`(offset_ms, confidence, match_z, precision_ms)`. Precision is not decoration: the
waveform matcher is sample-accurate (0.125 ms at the analysis rate) while the
MFCC matcher is quantised to its 10 ms hop, and reporting a hop-quantised offset
as if it were sample-accurate would be a lie told by the API. `precision_ms` is
stored in the report and shown next to the offset, and a technique's answer
without it is a bug, not an omission.

## Two confidences, one verdict

`correlation_quality` — the peak's prominence over the median lag response —
keeps its formula and keeps being stored, because 0.4.0 rows and every stored
report depend on it. The strategy adds `match_z`, the peak's distance from the
lag distribution in standard deviations: scale-free, and therefore comparable
between a quiet dub and a loud one, which prominence is not. The verdict gate
uses the new number as a **veto, not a replacement**: a checkpoint is `unreliable`
when its quality is below the floor *and* its match-z is unclear. A checkpoint
0.4.0 considered fine cannot be reclassified by a number it never reported.

## Consequences

- The pipeline is a container, not a matcher: adding a technique is adding an
  entry, and the routing, reporting and cost accounting come for free.
- A second technique costs one more pass over the same checkpoint, so `when`
  conditions carry the cost argument: `below_confidence` only on weak points,
  `above_drift` only on the points that break the timeline's line.
- Every technique must be evaluable in isolation on one window; a technique that
  needs the whole film is a stage, not a technique (piecewise search is a stage,
  ADR-0020).
- Reports record the strategy that produced them, so an old report can explain
  itself without the settings row that has since changed.

Status: accepted, implemented in `internal/analyze` (`technique.go`,
`pipeline.go`) and surfaced as the `sync.strategy` settings section.
```

---

## 2. `docs/adr/0020-piecewise-reencode-of-the-dub-copy.md`

```markdown
# A re-cut dub is fixed by re-encoding the dub copy, never the film

A dub made for a different cut of a film does not have one offset. Applying the
median leaves every scene after the first inserted scene out by the total insert
so far, and verification rejects it — correctly, but uselessly. Piecewise sync
splits the timeline into runs with a constant offset and re-times the audio to
match.

## What is re-encoded, and what is never

The **dub copy in the job's temp folder** is cut, padded and concatenated into a
new audio track: one ffmpeg pass, `atrim` + `adelay` + `apad` + `concat`, encoded
once. If a Transcode Rule matched, its codec applies; if none matched, the pass
is FLAC, so a timing feature never introduces lossy generation into a track the
operator asked to keep.

The **film is never re-encoded** (ADR-0018). The video stream is copied by
mkvmerge exactly as it is in every other mux, and the published output's video
streamhash is asserted equal to the target's in the acceptance test. The **Audio
Vault is never written** (ADR-0002): the segmented render reads the Copy-on-Work
artifact and writes inside `{WORK_DIR}/jobs/{id}/`. The old single-pass
`--sync <offset>` path is untouched and remains what runs when piecewise is off.

## Why one pass and not one encode per segment

Each intermediate encode is a generation loss and a temp file, and concatenating
encoded segments requires matching stream parameters anyway. A single filtergraph
produces one stream with one set of parameters, so `concat` cannot fail on a
format mismatch and the result cannot drift a few milliseconds at each join.
Every segment is forced to its exact intended length (`adelay` +
`apad=whole_dur` + `atrim=duration`) before the join, and the finished track's
duration is probed against the sum of the segment lengths; a mismatch fails the
job before anything is muxed.

## When it refuses

Piecewise is off by default and is a per-installation opt-in. When it is on and
the analysis cannot place a boundary confidently — fewer than three points on
either side, a step smaller than twice the stable band, an unreadable point
adjacent to the join — the attempt is abandoned: the report says which boundary
was doubtful, the **dominant single offset** is what the job would have applied
anyway, and the job is surfaced for review rather than published with audio the
engine is not sure about. A feature that guesses about a two-hour film is worse
than a feature that declines.

## Consequences

- A piecewise job stores `sync_offset_ms = 0` and an `application` note
  (`re-encoded (piecewise, 3 segments)`) with the segment and boundary list, so
  "why is the offset zero?" is answerable from the report alone.
- Boundary resolution is the sampling step (±45 s by default), so the tooltip
  says so and the segment list is shown; a boundary is a region, not a frame.
- Cost is one lossless re-encode of the dub plus a dense analysis pass, paid only
  by jobs the operator opted in for, and stated in the setting's tooltip.
- ADR-0010's no-lossy-re-encode default is unchanged: this ADR adds a timing
  reason to re-encode, not a quality one, and the default it re-encodes with is
  lossless.

Status: accepted, implemented in `internal/analyze/segment.go` and
`internal/transcode/segments.go`, off by default.
```

---

## 3. Amendment to `docs/adr/0004-mkvmerge-for-muxing.md`

Append to the existing ADR (do not rewrite its decision or its `Status:` line):

```markdown
## Amendment: the track mkvmerge consumes may be re-encoded first

ADR-0020 lets the sync worker cut and re-time a re-cut dub before muxing. That
does not move the boundary this ADR draws: **ffmpeg still produces the audio
track, mkvmerge still produces the MKV.** The segmented pass writes
`audio.transcoded.mka` in the job's temp folder — the same artifact slot the
Transcode Rule path has always used — and mkvmerge reads it with `--sync 0:0`.
Language and title tagging, the default-track flag, the append-and-validate step,
the atomic rename and the staleness check are all still mkvmerge's, untouched.

The only new thing ffmpeg is asked to do is render several trimmed pieces of one
input into one continuous track, in one invocation. That is the "windowing and
tempo adjustment" this ADR already assigns to ffmpeg, applied per segment rather
than once.
```

---

## Notes for the orchestrator

- `docs/adr/` has no index file; no index update is needed.
- The three files must be committed **before** their slices' final commit so that
  `AGENTS.md`'s authority order (ADR over code) never has to be violated by a
  slice landing ahead of its rule.
- ADR-0019 and ADR-0020 each state the 0.4.0-preserving default. If a later
  decision changes a default (for example putting MFCC in `balanced`), the ADR is
  the file that has to be edited first.
