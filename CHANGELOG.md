# Changelog

All notable changes to Dubarr are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project is
pre-1.0, so it follows the 0.x reading of
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) — see
[docs/RELEASING.md](docs/RELEASING.md) for what a minor and patch release may
change.

## [Unreleased]

_Nothing yet._

## [0.5.0] - 2026-11-27

Pre-1.0. The sync engine learned to say how sure it is, to tell a speed drift
from a cut, to run a chosen set of matchers, and to fix a dub that was cut
differently from your copy. Approving a job stopped being a promise the app
could not keep, and the library stopped being a flat list.

Settings, jobs and the Audio Vault all migrate forward on start. One behaviour
change is worth reading twice: **no preset turns the new piecewise sync on**,
not even Maximum accuracy, because a saved preset must never start re-encoding
dub copies behind your back.

### Sync engine

- **Confidence is a z-score** over the whole lag curve, not a fit through three
  numbers, and the peak is interpolated between samples — so a checkpoint now
  reports both how sure it is and how precisely it could place the offset
  (±0.125 ms at 8 kHz). The thresholds live in one table; nothing that was `ok`
  in a 0.4.0 library reclassifies.
- **Drift is separated from a cut** by fitting a line through the checkpoint
  offsets. A high-R² line means the two releases run at different speeds; a
  step means the film was re-cut. A clean drift is corrected in one ffmpeg pass
  with `atempo = 1 − slope/1000` — for 25.000 → 23.976 fps that is exactly
  0.95904 — and the residual offset is scaled by the same factor.
- **You choose which techniques run, and in what order.** Each technique is a
  rule: waveform, or MFCC, fired always or only when an earlier answer was
  below a confidence. Presets are `balanced`, `fast` and `maximum_accuracy`;
  Fast measures fewer checkpoints rather than measuring them worse. A preset
  press writes the technique list and the checkpoint positions in one save, and
  a refused save stores neither.
- **MFCC correlation** as a second matcher behind the same contract. On a
  codec-degraded pair it scores **0.188 quality against the waveform's 0.017**,
  because a spectrally-smeared encode hurts a sample-domain correlator far more
  than a mel-cepstral one. Its z-scores are not on the waveform's scale, so the
  sigma gate became per-technique.
- **Piecewise sync** for a dub that was cut differently: with *Handle films
  with a cut or an inserted scene* enabled, the analysis samples the whole film,
  a split-penalty dynamic program finds the joins, and each part of the dub is
  shifted on its own. The dub copy is re-encoded **once** (lossless when no rule
  asks for anything else) and muxed at zero offset because the track is already
  aligned. A join is only accepted where both neighbouring windows are confident
  and the step clears the floor — otherwise the plan is rejected, nothing is
  transcoded, and the job surfaces for review with the reason. A film with no
  cut stays an ordinary single-offset job.

Two defects were found in review and are worth naming, because both would have
lost audio silently: the segment plan stopped at the last analysed window (the
published track ended ~37.5 s early, and the render check could not see it
because the expected total was built from the truncated plan), and segments were
positioned by concatenation order alone, so a quiet opening — checkpoints below
the quality floor — shifted the whole track earlier while the job recorded a
zero offset. An accepted plan now **tiles the film** (first segment at 0, last at
the film's own duration, interiors contiguous) and a render refuses anything
else with `ErrInvalidPlan`. `TestPiecewiseEndToEndKeepsTailAndHead` proves it on
generated media with real ffmpeg: published 200.005 s against a 200.000 s target,
head marker Δ0 ms, post-join marker Δ1 ms, video stream hash unchanged.

### Workflow

- **Approve records intent.** It no longer asks the tag gate anything — it keeps
  only the four honest refusals (no identity match, no target file, unreadable
  target folder, no donor) and lets extraction run. The sync job is created where
  the track finally exists, as `queued` when the pair is eligible or `held` with
  a reason (`waiting_for_tags`, `missing_target`, `target_changed`,
  `cancelled_by_rejection`). Release is a projection at every epoch — each arr
  sync pass, after a rescan, once at boot, and again when a worker claims the
  job — so a held job promotes itself the moment its tags arrive. Migration 0008
  rebuilds `sync_mux_jobs` for the new states and is tested against a populated
  pre-0008 database.
- A practical consequence: an ineligible film no longer blocks **extraction**,
  so the expensive half of the work happens while the pair waits for tags.

### Library

- **Series, seasons and episodes.** The library was a flat list of files, which
  is fine for films and useless for television. Five read endpoints now serve the
  hierarchy with one aggregate query per level and no N+1: films, series (with
  season and episode counts and the **worst** eligibility in the tree, so one
  unprocessable episode surfaces at the series level), a series, its seasons with
  `percent_of_episodes`, and a season's episodes with paging. A season too long
  to show at once is paged and says so rather than silently ending. Migration
  0009 adds the three indexes the drill-down and search need.
- The flat `/api/library/items` view is unchanged.

### Upgrade notes

- **Nothing to migrate by hand.** Settings rows load unchanged; missing keys are
  served with their defaults. Migrations 0008 and 0009 run on start.
- **Piecewise sync is off** until you turn it on, in every preset.
- **A held job is not a failure.** If the queue shows a job waiting for tags, it
  was approved and is waiting for its target to become eligible; it releases
  itself, no second press needed.
- Reports from 0.4.0 render unchanged in Sync Studio; jobs created before 0.5.0
  keep their stored offsets.

## [0.4.0] - 2026-11-27

Pre-1.0. This is a minor release and the largest internal change since 0.2.0:
the sync engine is no longer a separate program, and a sync job no longer
decodes the whole film.

Nothing has to be migrated. Settings rows load unchanged, the two new keys are
served with their defaults when absent, and `sync-engine/` was never part of the
installed application — if you put `dubarr-sync` on `PATH` yourself, delete it;
nothing reads it.

The correlator is now Go code inside `dubarr`, reproducing the previous engine's
offsets exactly on the committed fixtures and on both real films through the
whole pipeline — and it is **faster than the engine it replaced**: a 190 s
checkpoint costs ~425 ms against the Rust engine's 687 ms, because the median
lag score is now selected instead of found by sorting 2.9 M values.

A sync job now analyses 190 s windows by default. Measured on the two reference
films: **3.5× and 3.6× faster** end to end, **4.7× less audio decoded**, with the
same verdict, the same checkpoint plan and |Δquality| 0.000. When a plan cannot
be built, or a window still looks wrong after its one re-decode, the job falls
back to a whole-track decode rather than failing; `full` stays selectable for an
operator who would rather never fall back.

### Upgrade notes

- **The DSP engine is no longer a separate program.** Dubarr is now a single
  binary: `dubarr-sync` is gone, the release archives contain `dubarr` only, and
  `/api/health` no longer reports the engine. If you installed `dubarr-sync` on
  `PATH` yourself, you can delete it — nothing reads it. A missing engine is no
  longer a way for the appliance to look degraded, because it cannot be missing.
- **Container images are smaller and build faster** for the same reason: the
  Rust build stage is gone.

### Added

- `sync.defaults.parallel_checkpoints` (1..8, default `1`): how many checkpoints
  one sync job analyses at once. The queue still runs one job at a time — this is
  concurrency inside a job, never between jobs. The app lowers the number itself
  when memory is short, reading the container's own limit rather than the host's.
  Expect roughly 400 MB per worker during a job; a 4 GB container yields four.

### Changed

- **The shipped analysis mode is now windowed.** `sync.defaults.analysis_source`
  defaults to `windows`: a sync job analyses 190 s windows with a 2.0 s pre-roll
  instead of decoding both tracks whole, so it finishes sooner and decodes about
  4.7× less audio — measured 3.5× and 3.6× faster end-to-end on the two
  reference films, with the same verdict as before (same offsets, same drift,
  |Δquality| 0.000 on both). When a window plan cannot be built, or a window
  still looks wrong after its one re-decode, the job falls back to a whole-track
  decode rather than failing; `full` stays selectable for operators who would
  rather never fall back.
- The correlator is now pure Go and runs in-process. It reproduces the previous
  engine's offsets exactly on the committed fixtures and on both real films
  through the whole pipeline — the recorded reference values live in
  `backend/internal/dsp/testdata` and are checked in CI.
- **Correlation is ~2.3× cheaper.** The median lag score used to be found by
  sorting ~2.9 M values, which was 51 % of a checkpoint and nearly three times
  the transforms it summarises; it is now an exact selection that returns the
  same value. A 190 s checkpoint went from ~981 ms to ~425 ms, faster than the
  Rust engine's 687 ms rather than slower. No reported number changes: the
  goldens still match with exact offsets.

### Removed

- `sync-engine/` (the Rust CLI), its CI job, its Docker build stage, and its
  release-archive entry.

## [0.3.1] - 2026-11-26

Pre-1.0. This is a patch release: it removes settings that never did anything,
tightens what a mux will publish, and fixes three failures that could strand a
job. No API route was removed and no settings key was renamed, but several keys
are gone — read the upgrade notes before pulling.

### Upgrade notes

- **`.bak` files are no longer written.** If you relied on the copy beside the
  target, it stops appearing after this upgrade. Existing `.bak` files are left
  exactly as they are; Dubarr does not delete them.
- **Five settings keys were removed:** `mux.options.keep_bak`,
  `security.options.vault_read_only`, `security.options.require_av1_warning`,
  `security.options.require_av1_strict` and `arr.options.manual_sync`. A stored
  row carrying one still loads and is served without it; writing one back is
  rejected with `400` naming the field. Nothing else in the row changes.
- **The tag filter now offers filename tags only.** A filter stored with
  `source_mode: arr` or `both` is shown as stored-but-not-applied and keeps its
  `required_arr_tags`, `excluded_arr_tags` and `root_folder_paths` — they simply
  do not filter anything, because no code ever read them.

### Fixed

- **A mux refuses a non-Matroska target.** `.mkv` and `.mka` only; `.mp4`, `.avi`
  and `.m2ts` are refused before any work copy is made, and the refusal is
  terminal for the job instead of being retried forever.
- **A target that changed mid-mux is no longer overwritten.** The target's
  identity (size, mtime, inode) is re-checked immediately before the atomic
  rename; a mismatch drops the temp and requeues the job, so an arr upgrade that
  lands while Dubarr is working is not silently destroyed.
- **`GET /api/health` reports `degraded` instead of panicking** when the store
  has been closed during shutdown.
- **The watch folder's cadence and stability window are read live** from
  settings, and a settled file whose queue row was deleted is no longer stranded.
- **Subsystems start when their settings arrive** during setup, not only at boot.

### Changed

- **Windows support dropped.** The release workflow no longer builds or publishes
  a `windows_amd64` archive, and no document describes Windows as supported or
  planned (ADR-0016).
- `IDEA.md` is replaced by `VISION.md`. The WebSocket, 60-second preview slices
  and `POST /api/mux` documented there were never built and are no longer
  described anywhere.
- **Folder labels say what the folders are:** "Work folder" is now "Temp folder"
  and "Vault folder" is now "Audio Vault". The temp folder's help explains the
  trade-off an operator actually has to make — same pool means the publish is an
  atomic rename, a faster separate pool means a copy across filesystems.
- **`Mux output folder` is absent, not disabled,** when the saved mux mode is
  replace. A greyed-out control invites you to look for the switch that enables
  it, and there is none.

### Removed

- `mux.options.keep_bak` and the `.bak` copy it controlled. Safety is validating
  the muxed file before the atomic rename, not a sidecar in a directory Dubarr
  does not own.
- `security.options.vault_read_only` — a switch with no backend consumer at all;
  the vault has always written read-only files regardless of it.
- `security.options.require_av1_warning` and `require_av1_strict` — muxing audio
  does not depend on the video codec, so an AV1 target was never a special case.
- `arr.options.manual_sync` — manual refresh has been unconditional since 0.3.0.

### Added

- ADR-0014: metadata is delegated to the media managers — Dubarr mirrors
  Radarr/Sonarr and has no TMDB/TVDB client of its own.
- ADR-0015: vault lifecycle — remove a track to the Vault Trash, replace its
  audio with a better source in place, and mark tracks orphaned when their title
  leaves arr. Amends ADR-0002: write-once still protects the vault from the
  pipeline, not from the operator.
- ADR-0016: Linux only.
- ADR-0017: a target that the media manager replaces is re-muxed automatically,
  because an arr upgrade (WEBDL to BluRay to remux) discards the dub every time.
  A mux is additive, so the worst case is a wrong audio track, not lost video.
- ADR-0018: publish by validated temp and atomic rename, written beside the
  target so the swap is atomic across pools.


## [0.3.0]

Pre-1.0. The HTTP API, settings schema and on-disk layout may still change before
1.0, which stays reserved for the point at which those are stable. Read the
upgrade notes before moving from 0.2.0 — two behaviour changes can surprise an
existing install.

### Added

- **Library page** — browse what Radarr and Sonarr report, paginated and
  filterable, with per-instance sync health so a never-synced or failing
  connection is visible instead of looking like an empty library.
- **Manual refresh** — sync the arr library on demand. A manual pass shares one
  in-flight slot with the scheduled poller, so the two can never overlap.
- **Per-item rescan** — ask arr to look at a single item again and re-read it,
  reporting the command's acceptance separately from whether the re-read landed.
- **Queue triage** — every release carries a state (unmatched, no donor, waiting
  for tags, ready) and a machine-readable blocker, plus an instruction for what
  to do next. An empty queue distinguishes no instance configured, never synced,
  a failed sync, a partly synced library, an empty library and a genuinely clear
  queue.
- **Identify** — when the library cache is cold, ask the row's own arr instance
  to parse the release name and rank library candidates. The operator's pick is
  what gets stored; nothing is auto-matched.
- **Arr connection diagnostics** — a failed connection test now says what failed
  (auth, status, kind mismatch, DNS, timeout, TLS, network) with the host and
  port, secrets redacted.
- **Draft connection test** — test a base URL and API key before saving them.
- **Set up later** — the first-run wizard can be completed later, explicitly,
  without writing placeholder storage paths.

### Changed

- **Approve refuses an ineligible target.** Approving a target the processing
  gate has not cleared previously succeeded and then produced an approved queue
  row whose sync job creation was refused — a row that could never run. Approve
  now applies the same verdict the job would, with the reason shown on the card.
  The force tag override still admits such a target, and the queue reports it as
  ready while that override is on.
- **`arr.options.manual_sync` is enforced.** It was declared and defaulted but
  never read; with it off, manual refresh is now refused.
- **A human library pick records `parse_confidence = "manual"`.** It previously
  recorded `"ambiguous"`, which describes a release name the parser could not
  resolve — nothing is parsed when an operator chooses by hand.
- **Empty-state copy is counted, not absolute.** A partly synced install no
  longer claims the whole library has never synced, and a healthy instance's
  success time is no longer printed directly above "the last sync failed".
- The Docker Compose stack pulls `subject928/dubarr:latest` instead of building
  from source, and `/media` is mounted writable.

### Fixed

- A new administrator was bounced out of the setup wizard by an auth-status
  refetch that still reported the instance as uninitialised.
- The sidebar scrolled out of view on long pages, taking navigation with it.
- A replace-mode mux could not write to the library because `/media` was mounted
  read-only; a fresh `./media` created by Docker as root also failed, which the
  README now covers.
- A blank or whitespace-only API key in the connection form silently replaced the
  stored key.
- An unreachable arr host reported "DNS lookup failed" when the resolver had
  simply timed out, and IPv6 hosts rendered without brackets.

### Upgrade notes

- **Docker Compose changed.** `/media` must be writable and owned by `PUID`/`PGID`
  (`sudo chown -R 1000:1000 ./media`). If you built locally with `build:`, the
  file now pulls an image — set `DUBARR_IMAGE_TAG` to pin a version.
- **Database migrations run automatically on start** and are additive. A
  migration that cannot complete rolls back, leaving the previous database
  intact.
- **Approving an ineligible target now returns 409** instead of succeeding. If
  you scripted approvals, treat that status as "the gate has not cleared this
  target yet".
- Windows builds remain untested and unsupported.

## [0.2.0]

Earlier releases predate this changelog; see the GitHub release notes for
`v0.1.0` through `v0.2.0`.
