# Techniques are ordered rules; presets are named sets of them

0.4.0 had one matcher and no vocabulary for a second one. Adding MFCC meant
either branching inside the correlator — every caller, every report and every
test learning a new shape — or inventing a second analysis path that would
eventually disagree with the first. The decision is to make the *list* the
interface: a sync job runs the techniques an operator enabled, in the order
they were placed, under the conditions they were given.

## A technique is a rule

An entry in `sync.strategy.techniques` names a matcher (`waveform` or `mfcc`),
whether it is enabled, its position, and three conditions: `when`, which is
`always`, `below_confidence` (a second opinion only after a weaker answer) or
`above_drift` (only on a checkpoint that does not sit on the fitted line),
`min_confidence`, and `min_match_z` with a `z_gate` switch. The entry is
validated as a whole, so a stored strategy cannot name a condition, a modifier
or a technique this build does not understand.

## The pipeline is two phases, and only two

Phase one runs inside a checkpoint's worker: the audio is cut once, then every
enabled technique reads that same slice in list order. `always` runs
unconditionally, `below_confidence` is skipped once the best answer so far
clears its own floor, and the first accepted answer wins the checkpoint and
stops the loop — until something is accepted, the healthiest answer is kept as
the fallback. Every attempt is recorded, accepted or not, with its own error,
so a report explains why a checkpoint fell through instead of just reporting a
different offset. An engine error is one attempt's error and never becomes a
zero offset.

Phase two exists because "does this checkpoint sit on the line?" needs every
first answer. It runs once, sequentially, after the join, in checkpoint order:
`above_drift` entries are tried on the checkpoints whose residual exceeds
`above_drift_residual_ms`, so the extra cost is bounded by the number of
outliers, normally zero. A phase-2 technique can only improve a checkpoint it
already has an answer for, so its errors are recorded and never fail the job.

## Presets are labels on a list, and one place holds their contents

`settings.Preset` is the only definition of `balanced`, `fast` and
`maximum_accuracy`; the server and the SPA both read it, and a test pins the
SPA's default object to it so the two cannot drift. `balanced` is the shipped
section: one enabled waveform entry, `always`, gated at the 0.4.0 quality floor
and 5σ. `fast` is that same list with fewer checkpoints — it measures fewer
points rather than measuring them worse, which is a `sync.defaults` decision,
not a technique one. `maximum_accuracy` adds MFCC as `below_confidence` at 0.15,
so the second matcher pays for a decode only where the waveform answer was weak.

Anything else is `custom`. A label is believed only where the section is that
label's own canonical section: an edited `balanced` is stored as `custom`, never
silently promoted back. MFCC is never part of a preset on its own terms — its
hop-limited precision is an opt-in, not a default.

## One preset press is one save

The technique list lives in `sync.strategy` and the checkpoint positions in
`sync.defaults`, and a preset is a statement about both. The SPA stages the two
sections and sends them in a single `PUT /api/settings`; the server validates
every section in the payload before writing any of them and then writes them in
one transaction. A refused save therefore stores neither half — there is no
state where the list says `maximum_accuracy` and the plan still has Fast's
three checkpoints.

## The gate is per technique

The z-score is only comparable within one matcher's curve. A correct MFCC
answer scores 5.3 where the waveform scores 21–28 on the same pair, because a
few thousand overlapping hop lags are not ~8·10⁴ near-independent samples. A
single sigma gate would therefore refuse exactly the answers MFCC exists for.
Each entry carries its own gate: the waveform entry keeps 5σ, the
`maximum_accuracy` MFCC entry carries `min_match_z: 0` and is judged on quality
alone, with its z still recorded as a diagnostic.

## Consequences

- The shipped strategy is one waveform entry, so a 0.5.0 job produces the
  offsets 0.4.0 produced; the pipeline had to be provably output-neutral before
  a second matcher could ride on it.
- `notes` explains a configured technique this build cannot run, so a strategy
  naming an unknown matcher says why it was skipped instead of quietly doing
  less.
- A report echoes the `strategy` that produced it: two reports are comparable
  only when the same list ran.
- No preset enables piecewise sync (ADR-0020). Segmentation changes what a job
  does to the operator's dub copy, so a stored preset must never be redefined
  into switching it on; a section that does is `custom`.

Status: accepted and implemented in `internal/analyze` (`technique.go`,
`pipeline.go`) and `internal/settings` (`Preset`, `Apply`).
