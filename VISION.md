# Dubarr — Vision

Dubarr is a self-hosted **arr for audio**. It manages a library of extracted audio
tracks for the movies and series that already live in a Radarr/Sonarr library: it
pulls audio out of donor releases, keeps it losslessly in an Audio Vault, and
muxes that audio into other releases of the same title.

This document states intent. `docs/adr/**` records decisions, `GLOSSARY.md` fixes
vocabulary, `AGENTS.md` states the rules an agent must follow, and `PLAN.md`
records what is being built.

## The one library dubarr owns

Dubarr owns exactly one library: the **Audio Vault**.

Everything else it displays is a **mirror** of a Radarr or Sonarr library —
titles, years, ids, file paths, languages. Dubarr does not talk to TMDB or TVDB,
does not resolve metadata, and does not own those identifiers. Metadata is
delegated to the media manager; dubarr clones what it reports (ADR-0014).

The consequence is a hard prerequisite: **a working Radarr and/or Sonarr is
required**, not an optional integration. With no media manager connected there is
no library to mirror and dubarr has nothing to process. It can still be
configured, and the vault can still be browsed.

## What it does

1. **Mirror** the media manager's library, on a poll and on demand.
2. **Match** a donor release to a target release of the same title.
3. **Extract** the chosen audio track from the donor, losslessly, into the vault.
4. **Measure** the timing offset between donor audio and the target's original
   track.
5. **Mux** the vault track into a copy of the target video, atomically, with the
   video stream never re-encoded.
6. **Curate** the vault: name it, browse it, remove a bad import, replace audio
   with a better source.

## What it does not do

- **No metadata management.** No TMDB/TVDB client, no identifiers of its own, no
  metadata repair. That is the media manager's job.
- **No video library management.** It never renames, moves or deletes a film or
  episode in your library.
- **No video transcoding.** Video is copied, never re-encoded.
- **No Windows support.** Linux native and Docker only (ADR-0016).
- **No multi-user.** One operator, one library. The application is published for
  others to adapt, but it is designed for a single person's collection.

## Two libraries, two sets of rules

|  | Audio Vault | Mirror Library | Target media files |
|---|---|---|---|
| What it is | the audio tracks dubarr archives | arr's catalogue, as arr reports it | the `.mkv` files themselves |
| Owned by dubarr | yes | no | no — arr's, and yours |
| May be organised | yes | never | never: dubarr does not rename or move them |
| Written by | archiving and operator lifecycle actions | arr's rescan; dubarr only reads it | dubarr, deliberately — a validated mux replaces the file. This is the core job |
| May be deleted | only by explicit operator action | never | never by dubarr |

## Automation posture

Dubarr is an automated application. It acts without asking wherever an action is
reversible, and stops for an explicit decision wherever data could be lost.

- **Automatic:** mirroring, matching, identification, extraction, offset
  analysis, orphan detection, and **re-applying a dub to a target the media
  manager replaced**.
- **Gated:** removing a vault track, purging the Vault Trash, reorganising the
  vault, overriding the Processing Gate.

Per-row approval for routine matching is not the goal; approval is reserved for
the steps that cannot be undone.

### Why replacing a target file is automatic

An arr upgrades media constantly: a WEBDL becomes a BluRay, the BluRay becomes a
remux, and every new file arrives with only the original audio. Each upgrade
silently throws away the dub that was muxed into the previous file. If dubarr
waited for a human after every upgrade, the operator would be reopening it
forever — which defeats the point of having an arr stack at all. The operator
wants to open Jellyfin, not dubarr.

So a target that changed is re-muxed automatically, because a mux is **additive
and therefore not destructive**: the video and every existing track are copied
as-is, the result is validated before it is published, and the worst outcome is a
container carrying the wrong or a duplicate audio track — recoverable by muxing
again, with the video never at risk. That is media-container damage, not data
loss, and the distinction is what decides the automation boundary.

Automatic re-application is still subject to the guardrails in ADR-0017: a proven
track pair, a fresh offset analysis, and no loop when a file keeps changing.

## Data-safety invariants

These hold in every version, regardless of what the version number promises:

1. **No silent destruction.** Nothing is deleted or overwritten without an
   explicit, previewed operator decision.
2. **Trash before purge.** A removed vault track, or audio replaced by a better
   source, moves to the Vault Trash. Purging is a separate, explicit step.
3. **Processing never mutates the vault.** Jobs copy out to the Work Area and
   verify SHA-256. The only writes into the vault are archiving and
   operator-initiated lifecycle actions (ADR-0015).
4. **Publish by validated temp, then swap.** mkvmerge writes a hidden temp in
   the target's own directory, so the result is on the same filesystem as the
   file it replaces. That temp is validated with `ffprobe` — the probe must
   succeed and exactly one audio track must have been appended — and only then is
   it renamed over the target, which is atomic. A crash leaves the original plus
   a temp file, never a half-written target. An optional `.bak` may be kept as a
   convenience copy; it is **not** the safety mechanism, and validation before the
   rename is.
5. **Video is never re-encoded.**
6. **Dubarr never writes arr's metadata.** It writes media files, then asks arr to
   rescan. Arr re-probes the file, sees the new audio track and updates its own
   catalogue; the mirror changes because arr changed it, not because dubarr
   pushed anything. That rescan is also the **confirmation loop**: the next mirror
   sync showing the muxed track is how dubarr knows the job really landed, rather
   than trusting its own success report.

## Versioning

Dubarr is 0.x and stays 0.x until the vault model has stopped moving and has run
against a real library long enough to trust. There is no 1.0 target: `1.0.0` is
reserved for the day the HTTP API and on-disk layout are genuinely stable, not
used as a milestone to aim at. See `docs/RELEASING.md`.
