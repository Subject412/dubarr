# Re-cut a dub copy once, never the video, and refuse a plan that does not tile

A dub that was cut differently has one offset per run of the film, so no single
mkvmerge `--sync` value can place it and no tempo factor can remove it. The only
honest fix is to take each run out of the dub copy, give it exactly the length of
its span of film, and put the pieces back in film order.

## What is re-encoded, and what never is

The render reads the dub copy under `{WORK_DIR}/jobs/{id}/` — the Copy-on-Work
artifact (ADR-0002) — and writes a new audio file beside it. `VAULT_DIR` is never
written, never re-hashed and never re-encoded, and the target video is never
touched: mkvmerge appends the finished track to a copy of the target, exactly as
in any other job (ADR-0018).

It is **one** ffmpeg pass, not one per segment. Each run is `atrim`med to its own
source span, padded with `adelay`/`apad` to exactly its length on the target
timeline, and the branches are `concat`ed — a per-segment encode would be a
second generation of loss at every join, which is the boundary ADR-0010 already
draws. The encode is lossless unless a Transcode Rule asks for something else; a
rule's codec, channels and rate apply to the whole segmented output, and a
segmented render refuses to carry a tempo factor, because a stretch and a
re-cut plan are different corrections for different material.

## A plan must tile the film

`concat` stacks the pieces end to end and knows nothing about absolute time: a
piece lands at the sum of the lengths before it, not at the position it names.
Every position in a plan is therefore a claim about the render, and the only
honest plan is one that tiles the target timeline from 0 with no holes and no
overlap: the first segment starts at 0, the last ends at the film's own duration,
and every interior edge is contiguous with its neighbour.

`BuildSegmentedArgs` refuses anything else with `ErrInvalidPlan`, before ffmpeg
runs and regardless of what the caller believed. `analyze` finalises an accepted
plan the same way (`tileToFilm`): the segmenter is never handed the film's
length, so it states each run over the windows it actually sampled, and the plan
that is stored and rendered is the tiled one. Tolerance is 1 ms — the analysis
grid is snapped to whole 8 kHz samples (0.125 ms) and the totals are whole
milliseconds, so a real plan is exact and the tolerance only absorbs rounding.
The shifts this check refuses are steps, tens of seconds; nothing real hides in a
millisecond.

## Why the mux offset is zero

A piecewise job muxes at offset 0. That is not a shortcut — it is a statement
about the plan: because the plan tiles the film, the first run really does land
at time 0 and the last one on the film's last frame, so the track is already
aligned and there is nothing for mkvmerge to shift. The stored 0 plus
`application.mode: "piecewise"` is the signature of that mechanism; a reader who
sees 0 without the application block is looking at a different job.

## A rejected plan transcodes nothing

A join is accepted only where both neighbouring runs are confident and the
offset step across it clears the floor. Otherwise the segmentation is rejected:
no audio is re-encoded, the job keeps the dominant offset, and it surfaces for
review with the reason in `review_reason`. A film with no cut stays an ordinary
single-offset job, which is why piecewise is off by default and absent from every
preset (ADR-0019): it is the one setting that changes what a job does to the
operator's dub copy.

## Two defects this rule exists to prevent

Both shipped in review and both would have lost audio silently. The plan first
stopped at the last analysed window, so the published track ended ~37.5 s early
— and the render check could not see it, because the expected total was built
from the truncated plan. Separately, segments were positioned by concatenation
order alone, so a quiet opening (checkpoints below the quality floor) shifted the
whole track earlier while the job recorded an offset of 0.

An accepted plan now tiles the film and a render refuses anything else. The
expected total is the target track's own duration when the caller knows it, and
the plan's total only otherwise, so a plan cannot be judged tiled against one
number and verified against another.
`TestPiecewiseEndToEndKeepsTailAndHead` proves the tail and the head on generated
media with real ffmpeg: published 200.005 s against a 200.000 s target, head
marker Δ0 ms, post-join marker Δ1 ms, video stream hash unchanged.

## Consequences

- The cost is one encode of the dub copy per piecewise job, on the temp folder,
  and a segmented track that is not the vault's lossless track. That is why the
  feature is opt-in and why no preset enables it.
- Dense sampling (45 s apart, 60 s windows by default) replaces the six-point
  plan for that job only; the ordinary plan is untouched.
- Reports from a piecewise job carry `piecewise` and `application` blocks; a job
  with neither means piecewise never ran, which is different from "ran and found
  nothing".

Status: accepted and implemented (`internal/analyze/segment.go`,
`internal/transcode/segments.go`, `internal/syncworker`).
