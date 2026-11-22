// Package arrsync keeps arr_library_cache in step with the configured Radarr
// and Sonarr instances.
//
// One pass walks every enabled connection in settings arr.connections, lists
// its library with the M2 clients (Radarr GET /api/v3/movie, Sonarr
// GET /api/v3/series plus GET /api/v3/episode?seriesId=N), resolves each item's
// target video and caches the Tdarr Gate verdict, then applies the snapshot as
// a diff: new rows are inserted, changed rows are updated in place (ids
// survive), unchanged rows are left alone and vanished rows are deleted (see
// store.SyncLibraryCache).
//
// Credentials are read from internal/secrets under arr.<kind>.api_key and
// re-encrypted into arr_instances.api_key_enc, so the cache's foreign key has a
// real instance row without duplicating key storage. Keys are never logged.
//
// The background loop runs every settings arr.options.poll_interval_sec
// (settled default 300 s, PLAN.md "arr sync manual + 5-min poll"). A pass never
// stops the loop: per-instance and per-pass panics are recovered and logged,
// and a failed instance leaves its previous cache intact.
package arrsync
