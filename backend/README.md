# backend/

Go server for Dubarr. Module `github.com/Subject412/dubarr/backend`, declaring
`go 1.26.0` (the container and CI build with Go 1.27). See
[../AGENTS.md](../AGENTS.md) for the full contract.

## Layout

- `cmd/dubarr/` — the only `main` package: flag parsing, wiring and lifecycle.
  Feature packages never edit it.
- `internal/<pkg>/` — feature packages (`api`, `auth`, `secrets`, `settings`,
  `store`, `arr`, `queue`, `vault`, `sync`, `syncjob`, `mux`, `gate`,
  `health`, `logs`, …). Each exposes `RegisterRoutes(mux, deps)`; only
  `cmd/dubarr` wires packages into the root router.
- `web/` — built SPA assets embedded via `go:embed`. `npm run build` in
  `frontend/` writes here, so build the frontend before the Go binary.
- `schema.sql` — SQLite schema/migrations.

## Build and test

```sh
go build ./cmd/dubarr
go test ./...
go vet ./...
```

Run natively with `./dubarr --data-dir ../data --listen 127.0.0.1:8080`
(bootstrap flags are `--data-dir`, `--listen`, `--desktop`, plus the logging
flags `--log-level` and `--log-file`; see ADR-0013). Application settings and
secrets are configured in the web UI, never through the environment (ADR-0003).
