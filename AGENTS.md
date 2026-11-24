# Dubarr — Agent Contract

Self-hosted dubbing-sync appliance. Domain language: `GLOSSARY.md`. **Authority order:** normative are the rules in this file and the decisions in `docs/adr/**`, then the working code; `IDEA.md` is historical design narrative — where it disagrees with an ADR or the code, follow the ADR/code and amend either explicitly. License: MIT (`LICENSE`).

## Stack
- Backend: Go 1.26.0 per `backend/go.mod` (no `toolchain` directive); Docker builds with `golang:1.27-alpine`, CI with the go.mod version. Module `github.com/Subject412/dubarr/backend`. Pure-Go SQLite (`modernc.org/sqlite`, WAL). REST + WebSocket. SPA embedded via `go:embed`.
- DSP: Rust CLI `dubarr-sync` (edition 2021; clap, hound, rustfft, serde, serde_json). Reads WAV PCM slices. `offset_ms = t_target − t_donor`.
- Frontend: React 19 + TypeScript 7 + Vite 8 + Tailwind 4 (`@tailwindcss/vite`, `@theme` in CSS); react-router 7; tanstack-query 5; wavesurfer.js (Sync Studio, M5). Dark *arr-style UI. Lint: `oxlint`.
- External tools: `ffmpeg`, `ffprobe`, `mkvmerge` on PATH; `/api/health` reports availability.
- No Python anywhere. No app settings or secrets in env vars. Bootstrap flags: `--data-dir`/`--listen`/`--desktop`, plus `--log-level`/`--log-file` (ADR-0013) and the one-shot `rekey --keyfile` (ADR-0008); PUID/PGID in Docker.

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
- Target video never transcoded. Mux only tag-eligible targets (Processing Gate, ADR-0005 as amended by ADR-0009 — a user-configured filename-tag criterion with an optional arr criterion; donors are never tag-gated) unless the settings force override is allowed.
- Opt-in Transcode Rules (`audio.options`, ADR-0010): shipped empty; conditions match PRE-TEMPO vault metadata and the first matching enabled rule wins. When a rule matches, re-encode only the Work Area copy (never VAULT_DIR) in ONE ffmpeg pass that also applies any required tempo factor; ffprobe must confirm codec/channels/duration or the job fails clearly, and the encoder is checked before the job starts. Atomic mux publication, language tags and `mux.options.track_title_in_mkv` are unchanged.
- Secrets AES-256-GCM (12-byte nonce || ciphertext || tag); master key OS keyring (native) / 0600 keyfile (Docker). `X-Api-Key` header only; scrub secrets from logs/errors. Never print secrets.
- Paths canonicalized + `filepath.EvalSymlinks`, confined to configured roots. DATA_DIR 0700; db/keyfile 0600.
- Fixture tests run on COPIES in scratch; originals read-only. `go test` + `cargo test` green before any milestone gate.
- TDD: failing test → minimal implementation → green → commit. Report diff + command evidence, not claims.
- Subagents: no system installs, no destructive commands, no `git push`, no protected-file edits.
