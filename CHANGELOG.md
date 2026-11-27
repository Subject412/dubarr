# Changelog

All notable changes to Dubarr are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project is
pre-1.0, so it follows the 0.x reading of
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) — see
[docs/RELEASING.md](docs/RELEASING.md) for what a minor and patch release may
change.

## [Unreleased]

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

- The correlator is now pure Go and runs in-process. It reproduces the previous
  engine's offsets exactly on the committed fixtures and on both real films
  through the whole pipeline — the recorded reference values live in
  `backend/internal/dsp/testdata` and are checked in CI.

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
