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

1. **Gate interaction.** The Processing Gate exists to stop wrong matches. A
   re-apply is not a new match — the pair was already proven when the track was
   first muxed. Whether a re-apply must re-satisfy the tag gate, or may bypass it
   on the strength of the proven pair, is open.
2. **Offset drift.** A different cut (extended, remastered) has different timing.
   Re-application always re-runs offset analysis rather than reusing the previous
   offset; whether a materially different result still applies automatically or
   waits for approval is open.
3. **Loop protection.** A file that keeps changing must not produce an endless
   series of muxes. One attempt per observed change, and a cap, are agreed;
   the exact cap is open.
4. **Telling the operator.** Whether a re-apply is silent in the job history or
   surfaces as a queue entry is open.

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
