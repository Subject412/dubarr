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

1. **Gate interaction — settled: the gate always applies.** Eligibility is a policy
   question — which targets dubarr may touch at all — and the operator answers it
   with a filename tag (in the reference setup, the marker that the upstream video
   pipeline has finished) or with arr tags on the title. **Only the filename path
   is wired today**: `tag_filter.source_mode = "filename"` is implemented and
   enforced, while `required_arr_tags` / `excluded_arr_tags` exist in settings and
   `arr_tag_ids_json` is stored, but no non-test code consumes them
   (`gate.ArrMetadata` is constructed only in tests). The arr-tag path is
   unscoped work, not existing behaviour.

   Muxing before the upstream stage is not destructive: a well-built pipeline keeps
   the audio the operator wants. It is simply wasted, because the pipeline replaces
   the file and the mux goes with it, so dubarr would mux again once the tag
   appears. The gate is enforced for policy and for a sane sequence, not to prevent
   data loss.

   Expected order in the reference setup: arr upgrade → upstream processing → tag →
   dubarr mux.
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

## Idempotency — how dubarr knows a target is already dubbed

Two sources, in this order: the stored record for the file, and a probe of the
target for a track carrying the dub language and title. The record is the fast
path; the probe is the truth, because arr may have replaced the file since the
record was written. A record that says "dubbed" while the probe disagrees is the
signature of a replaced file — exactly the case re-application exists to handle.

**Dubarr writes no tag into the filename.** The muxed track is the marker and
cannot lie. A filename tag would need arr to rename the file to keep the naming
rule tidy, and that rename would look like a target change and trigger another
mux — the automation would feed itself.

## Consequences

- Completion is confirmed by the mirror, not by the job: after publishing, dubarr
  asks arr to rescan, and the next sync showing the muxed track is the proof
  (VISION.md).
- The job history must record *why* a mux ran — "target replaced by arr" is a
  different event from "operator approved", and confusing the two would make the
  history useless.
- A vault track that is orphaned (its title left arr) cannot be re-applied, since
  there is no target left to apply it to.

Status: accepted. Guardrails settled; implementation not started.
