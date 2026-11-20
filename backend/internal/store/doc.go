// Package store owns Dubarr's SQLite persistence layer (modernc.org/sqlite,
// WAL journaling): DATA_DIR creation and permission hardening, the database
// handle exposed as Store.DB, and the forward-only migration runner over the
// go:embed'ed migrations/*.sql revisions.
package store
