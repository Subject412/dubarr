// Package arr holds typed clients for the Radarr and Sonarr REST v3 APIs.
//
// It covers only what Dubarr reads: library entries (movies/series), quality
// profiles, root folders and a system-status ping used as a connection test.
// Tests run against in-process fakes from the arrmock subpackage, never a live
// instance (ADR-0007). Clients receive credentials as parameters and neither
// read them from the environment nor persist them; storage lives in M2-2.
package arr
