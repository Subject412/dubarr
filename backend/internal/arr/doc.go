// Package arr holds typed clients for the Radarr and Sonarr REST APIs.
//
// Each client speaks only the newest API its product ships — Sonarr 5 at
// /api/v5, Radarr 3 at /api/v3 — with no version probe and no older-API
// fallback: a 404 on the base path is reported as an API-version failure that
// names the product and the version required.
//
// It covers only what Dubarr reads: library entries (movies/series/episodes with
// their files), quality profiles, root folders, tags and a system-status ping
// used as a connection test. Tests run against in-process fakes from the
// arrmock subpackage, never a live instance (ADR-0007). Clients receive
// credentials as parameters and neither read them from the environment nor
// persist them; storage lives in M2-2.
package arr
