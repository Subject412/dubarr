# Dubarr — Agent Contract

Self-hosted dubbing-sync appliance. Domain language: `GLOSSARY.md`. Spec: `IDEA.md` (read-only reference). Decisions: `docs/adr/` — do not contradict an ADR without a new one. License: PolyForm Noncommercial 1.0.0 (`LICENSE`) — free for non-commercial use.

## Stack
- Backend: Go 1.23+ (toolchain 1.27), module `github.com/Subject412/dubarr/backend`. Pure-Go SQLite (`modernc.org/sqlite`, WAL). REST + WebSocket. SPA embedded via `go:embed`.
- DSP: Rust CLI `dubarr-sync` (edition 2021; clap, hound, rustfft, serde, serde_json). Reads WAV PCM slices. `offset_ms = t_target − t_donor`.
- Frontend: React 18 + TypeScript + Vite + Tailwind; react-router; tanstack-query; wavesurfer.js (Sync Studio). Dark *arr-style UI.
- External tools: `ffmpeg`, `ffprobe`, `mkvmerge` on PATH; `/api/health` reports availability.
- No Python anywhere. No app settings or secrets in env vars (only `--data-dir`/`--listen`/`--desktop`; PUID/PGID in Docker).

## Commands
- Backend: `cd backend && go build ./cmd/dubarr && go test ./... && go vet ./...`
- Run native: `./dubarr --data-dir ./data --listen 127.0.0.1:8080`
- DSP: `cd sync-engine && cargo build && cargo test && cargo clippy`
- Frontend: `cd frontend && npm install && npm run dev|build|lint` (build output → `backend/web`)
- Docker: `docker compose -f docker/docker-compose.yml up --build`

## Route registration contract (avoid main.go conflicts)
Each `internal/<pkg>` exposes `RegisterRoutes(mux *http.ServeMux, deps Dependencies)` (or an `http.Handler`). Only `cmd/dubarr` wires packages into the root router. Feature packages MUST NOT edit `cmd/dubarr/main.go` or another package's files.

## Rules
- Conventional commits, branch `main`. Never commit media (`testfiles/`), `data/`, or secrets.
- Protected (orchestrator-only writes): `AGENTS.md`, `PLAN.md`, `GLOSSARY.md`, `docs/adr/**`, `LICENSE`, applied migrations.
- Vault write-once. Every job Copy-on-Work into WORK_DIR + SHA-256 verify; never modify VAULT_DIR in place.
- Target video never transcoded. Mux only tag-eligible targets (Tdarr Gate) unless force override allowed by settings.
- Secrets AES-256-GCM (12-byte nonce || ciphertext || tag); master key OS keyring (native) / 0600 keyfile (Docker). `X-Api-Key` header only; scrub secrets from logs/errors. Never print secrets.
- Paths canonicalized + `filepath.EvalSymlinks`, confined to configured roots. DATA_DIR 0700; db/keyfile 0600.
- Fixture tests run on COPIES in scratch; originals read-only. `go test` + `cargo test` green before any milestone gate.
- TDD: failing test → minimal implementation → green → commit. Report diff + command evidence, not claims.
- Subagents: no system installs, no destructive commands, no `git push`, no protected-file edits.
