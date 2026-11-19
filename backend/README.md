# Dubarr — agent contract (see ../AGENTS.md)

Go 1.23 module `github.com/Subject412/dubarr/backend`. Layout:

- `cmd/dubarr/` — the only `main` package; wires routes, never feature logic.
- `internal/<pkg>/` — feature packages; each will expose
  `RegisterRoutes(mux *http.ServeMux, deps Dependencies)`.
- `web/` — built SPA assets, embedded via `go:embed` (T6).
- `schema.sql` — migrations (T2).

M1 state: `main` only resolves configuration (flags, DATA_DIR 0700) and exits.
