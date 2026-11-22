// Package watch holds watch folders and library ingest.
//
// The M2-3 scanner periodically walks the configured media roots, confines
// every discovered path to a canonicalized root, and records new donor video
// files in the import_queue table as pending_approval candidates.
//
// Start/Stop own the scan goroutine; Scan is exported for on-demand passes. UI
// wiring arrives in M2-6 and only needs the constructor.
package watch
