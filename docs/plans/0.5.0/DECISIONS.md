# 0.5.0 — settled decisions (operator, 2026-11-27)

This file is the input to the 0.5.0 plans. Where a plan disagrees with this file, this file wins until the
operator says otherwise. Written by the orchestrator after the grilling round; the operator is AFK, so every
decision below is binding and only reversed by an explicit operator instruction.

## D1 — Scope

0.5.0 delivers **all remaining open issues**: #85, #86, #82, #83, #84, #48, #56, #47. Order of work follows
risk, not issue number (see each plan). #88 (chromaprint fingerprint) is **closed as not planned**: the operator
judged `fpcalc`/libchromaprint too heavy a dependency for what it adds over the existing confidence work, and it
would have added a second external binary to the image and to `/api/health`.

No issue may be left half-done behind a feature flag that pretends it exists. If a slice cannot be finished it
is reported as blocked, not merged.

## D2 — #85 confidence: dual reporting

Keep `correlation_quality` (peak prominence, `(peak − median)/(1 − median)`, `backend/internal/dsp/correlate/correlate.go:366`)
**unchanged and still stored**, and add:

- `match_z` — peak scored against the window's own lag distribution (robust z / MAD-based), comparable across
  content where prominence is not;
- interpolated sub-sample peak offset (parabolic fit over the three samples around the peak), so offsets stop
  snapping to whole samples;
- one thresholds table that the verdict function reads, so re-tuning is a single edit (`analyze.go` verdict
  precedence `out_of_range > unreliable > drifting > ok`, lines 817-848, is preserved).

Rationale: items already `ok` in a 0.4.0 library must not silently reclassify. Nothing that shipped changes
meaning; the new number is additive and the verdicts start using it in the same release with the old floor kept
as a named constant.

## D3 — #83 MFCC: honest precision

A new matcher in `backend/internal/dsp` (mel filterbank + DCT, pure Go, f32, no new dependency) returns
`(offset_ms, confidence, precision_ms)`. MFCC reports its hop-limited precision (10 ms at the shipped hop);
waveform reports sample precision. The report and the UI must not present a hop-quantised offset as
sample-accurate. The matcher is **not** in the default list.

## D4 — #82 strategy pipeline

- Techniques are evaluated **per checkpoint**; the job verdict aggregates them. A single weak checkpoint falls
  back on that checkpoint only.
- Each entry carries `when`: `always` | `below_confidence` (threshold) | `above_drift`.
- The shipped default list is exactly one entry — `waveform` / FFT correlation / `always` — so 0.5.0 produces
  byte-identical offsets to 0.4.0 until the operator adds a technique.
- Settings UX (operator decision, applies to every new option in this release): names in plain language, never
  matcher jargon; one tooltip per option with *what it does*, *when to use it*, *cost* (runtime/CPU); presets
  `Balanced (default)` / `Fast` / `Maximum accuracy`; an advanced section that stays collapsed.

## D5 — #84 piecewise: allowed to re-encode the dub copy

Piecewise detection is a strategy, **off by default**. When enabled and more than one segment survives the
confidence check:

- the dub is cut and padded per segment and encoded **once** in a single ffmpeg pass (same rule as ADR-0010:
  temp copy only, never `VAULT_DIR`; target video untouched);
- the queue row and job report say `re-encoded (piecewise, N segments)` and list the boundaries;
- an unconfident boundary does not get guessed: the job falls back to the single dominant offset and surfaces
  for review, saying why.

## D6 — #48 approve records intent, gate holds the mux

- `approve` always records intent; it no longer refuses an ineligible target with 409.
- A new job state means "approved, waiting for eligibility". Release is a **projection of eligibility**,
  re-evaluated on every gate pass **and once at boot** — never a timer, never a queue that a restart strands.
- Operator example accepted: approve an episode whose media is not yet upgraded → held; NAS reboots → still
  held; Sonarr upgrades → next pass releases and muxes, with no second approval. A rejection cancels intent.
- Force-override semantics unchanged. ADR-0021 records Design B replacing Design A (ADR-0019 and 0020 are
  taken by the sync-engine decisions: technique pipeline and piecewise re-encode).

### D6a — #48 refinements confirmed after the design pass

- The hold lives on `sync_mux_jobs` (`held`), **not** on `import_queue`: approve stops refusing on gate
  eligibility but still refuses (409) where no pair can be formed at all — no matched identity, no target file,
  unreadable folder, no donor. ADR-0005's "donors are never tag-gated" is untouched.
- Release points: end of each arr sync pass (covers the scheduled loop and manual Trigger), after a successful
  per-item rescan, and once at boot before the sync worker starts. Plus a claim-time re-check in the worker so a
  job released a moment before its target loses the tag re-holds instead of muxing.
- The glossary entry for Match State / Match Blocker must widen from "ready = approval possible" to
  "ready = no gate verdict blocks approval"; the orchestrator amends `GLOSSARY.md`, never a subagent.

## D7 — #56 hierarchy: mimic the arrs

The operator's rule: *do what Radarr/Sonarr do*. Read-only aggregate endpoints shaped like the arrs' own
resources (series → seasons → episodes; movies flat), server-side search and paging with hard caps, Radarr and
Sonarr as two tabs. Local C# sources to adapt from: `/home/gwdawid/Dokumenty/GitHub/Sonarr`,
`/home/gwdawid/Dokumenty/GitHub/Radarr`. Never write to the arr mirror (ADR-0014).

## D8 — #47 vault organising: adapt the arrs' file management

Arr-style naming (folder + file tokens, quality/language/edition tokens where dubarr has them), a
**preview-then-apply** reorganisation, applied in one transaction with one audit row per file, collisions
refused rather than resolved, and **no deletion ever**. `audio_tracks` paths, open jobs and future sync sources
must move with the files. Adapt the algorithm from the arrs' C# (`Organizer/FileNameBuilder`,
`OrganizerService`, `DiskTransferService`), not from memory.

## D9 — Process for the AFK stretch

- One branch per slice, PR per slice, merge to `main` only when green (build + `go test ./...` + frontend
  `build`/`lint`/`test`), tag `v0.5.0` only after the last merge.
- Protected files (`AGENTS.md`, `PLAN.md`, `GLOSSARY.md`, `docs/adr/**`) are written by the orchestrator only.
  If a write to a protected file is blocked or the approval prompt times out, **pause at that point and wait
  for the operator** — do not work around it.
- After the tag: delete every branch (local and remote), close every issue (each with evidence), leave zero
  open issues. An issue is opened during this session only if it cannot be resolved without the operator.

## D10 — Fixtures

The operator explicitly lifted the copyright concern: media fixtures are not a problem here. Still, the
reproducible default is:

1. **DSP-level** — synthetic WAVs generated by a committed generator script (known offset, known step, known
   degradation), so the matchers are tested without media and stay green in CI.
2. **End-to-end** — a generated `.mkv` (ffmpeg synthetic video + a deliberately offset/padded dub track), then
   dubarr is asked to sync it. This proves the pipeline, the mux application and the reports.
3. Real films in `testfiles/` are used for the local acceptance runs that need real content; those tests stay
   opt-in via `DUBARR_PARITY`-style env gating as today.

Generated media is gitignored; generators and the expected numbers are committed.

## D11 — Settings rule for everything in this release

A user of dubarr is a media hobbyist, not a signal-processing engineer. Every option must answer, in the UI:
*what happens if I turn this on*, *when would I want it*, *what does it cost*. Technical knobs live behind an
"Advanced" disclosure with defaults that are correct for everybody.
