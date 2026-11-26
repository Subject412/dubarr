# Metadata is delegated to the media managers

Dubarr does not resolve metadata. It has no TMDB or TVDB client, stores no
identifier it resolved itself, and never repairs a title, year or external id.
Instead it **mirrors** the library that a Radarr or Sonarr instance reports and
works against that mirror.

A working media manager is therefore a prerequisite: with nothing connected there
is no library to mirror. The one library dubarr genuinely owns is the Audio Vault
(VISION.md).

## Consequences

- **Identifiers are borrowed, not invented.** A vault track is keyed by the
  TMDB/TVDB id that arr reports, plus language and edition, so it survives an arr
  reinstall even though the arr-side record id changes (ADR-0015).
- **The mirror is never written.** Dubarr reads arr's library and never pushes
  metadata changes back; the only arr call it makes with side effects is a
  best-effort rescan after a successful mux.
- **Arr's library can shrink underneath us.** When a title leaves arr, the mirror
  row disappears but the vault track does not: it becomes *orphaned* and is never
  deleted automatically (ADR-0015).
- **No metadata repair path.** A wrong title or year in arr stays wrong in dubarr.
  Fixing it is the media manager's job.

Status: accepted
