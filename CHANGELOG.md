# Changelog

All notable changes to Dubarr are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project is
pre-1.0, so it follows the 0.x reading of
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) — see
[docs/RELEASING.md](docs/RELEASING.md) for what a minor and patch release may
change.

## [Unreleased]

### Changed

- **Windows support dropped.** The release workflow no longer builds or publishes
  a `windows_amd64` archive, and no document describes Windows as supported or
  planned (ADR-0016). Previously the release shipped a Windows zip that the docs
  called deferred and untested.
- `IDEA.md` is replaced by `VISION.md`. The WebSocket, 60-second preview slices
  and `POST /api/mux` documented there were never built and are no longer
  described anywhere.

### Added

- ADR-0014: metadata is delegated to the media managers — Dubarr mirrors
  Radarr/Sonarr and has no TMDB/TVDB client of its own.
- ADR-0015: vault lifecycle — remove a track to the Vault Trash, replace its
  audio with a better source in place, and mark tracks orphaned when their title
  leaves arr. Amends ADR-0002: write-once still protects the vault from the
  pipeline, not from the operator.
- ADR-0016: Linux only.


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
