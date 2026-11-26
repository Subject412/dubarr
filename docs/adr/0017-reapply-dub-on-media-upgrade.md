# Re-apply the dub when the media manager replaces a target

## Context

An arr upgrades media continuously: WEBDL becomes BluRay, BluRay becomes remux,
and each new file arrives with only the original audio. Every upgrade therefore
discards the dub that was muxed into the previous file, and the operator would
have to reopen dubarr after every single upgrade to put it back.

That is the opposite of what an arr is for. The operator wants to open Jellyfin,
not to babysit a queue.

## Decision

When the mirror shows that a target's file has changed, and dubarr already holds
a vault track proven for that title and language, dubarr **re-muxes
automatically** — no approval step.

The reason this is safe to automate is the shape of a mux: it is additive. The
video and every existing track are copied as-is, the result is validated before
publication (ADR-0018), and the video stream is never re-encoded. The worst
outcome of a bad automatic mux is a container carrying a wrong or duplicate audio
track. That is recoverable by muxing again, and it is not data loss.

## Guardrails

The decision above is settled. These parameters are not yet:

1. **Gate interaction — settled: the gate always applies.** The tag is not only a
   match filter, it is a **pipeline-stage marker**: in the reference setup the tag
   means "the upstream video pipeline has finished with this file". Muxing before
   that stage is worse than a wrong match — the upstream pipeline cleans the
   container and drops unwanted audio tracks, so a dub muxed in early would be
   removed when that pipeline later replaces the file, and the work would be lost.
   A re-apply therefore waits for the tag, exactly like a first mux. The upgrade
   sequence is arr upgrade → upstream processing → tag → dubarr mux.
2. **Offset drift — settled.** Re-application always re-runs offset analysis
   rather than reusing the previous offset. If the new offset moves by more than
   the acceptance threshold from the previous one, the job is **parked for
   approval** instead of published: a large shift means a different cut, and a
   parked job that waits is the correct outcome.
3. **Loop protection.** One attempt per observed change, with a cap per file per
   day; when the cap trips the file is surfaced as a problem rather than retried
   silently, because an arr that keeps re-grabbing the same title is a signal
   that something is wrong upstream.
4. **Telling the operator.** A successful re-apply is recorded in the job history
   and visible in the library view without asking for approval. A parked job —
   offset drift, or the loop cap tripping — surfaces as a queue entry, because
   those are the cases that need a human.

## Consequences

- Completion is confirmed by the mirror, not by the job: after publishing, dubarr
  asks arr to rescan, and the next sync showing the muxed track is the proof
  (VISION.md).
- The job history must record *why* a mux ran — "target replaced by arr" is a
  different event from "operator approved", and confusing the two would make the
  history useless.
- A vault track that is orphaned (its title left arr) cannot be re-applied, since
  there is no target left to apply it to.

Status: accepted in principle; guardrails open. Implementation not started.
