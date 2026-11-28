# Dubarr — Agent Contract

Self-hosted **arr for audio**: a curated vault of extracted audio tracks for the movies and series in a Radarr/Sonarr library. Intent: `VISION.md`. Domain language: `GLOSSARY.md`. **Authority order:** `VISION.md` (intent) → this file and `docs/adr/**` (rules and decisions) → `GLOSSARY.md` (vocabulary) → the working code. Where two disagree, fix the document rather than tolerating the drift. License: MIT (`LICENSE`).

## Stack
- Backend: Go 1.27.1 per `backend/go.mod` (no `toolchain` directive); Docker builds with `golang:1.27.4-alpine`, CI with the go.mod version. Module `github.com/Subject412/dubarr/backend`. Pure-Go SQLite (`modernc.org/sqlite`, WAL). REST only — there is no WebSocket. SPA embedded via `go:embed`.
- DSP: pure Go, in-process (`backend/internal/dsp`: `wav`, `correlate`; f32, `go-fft/fft` pinned exactly). Reads WAV PCM slices. `offset_ms = t_target − t_donor`. One job at a time; checkpoints inside a job run in parallel up to `sync.defaults.parallel_checkpoints`.
- Frontend: React 19 + TypeScript 7 + Vite 8 + Tailwind 4 (`@tailwindcss/vite`, `@theme` in CSS); react-router 7; tanstack-query 5; no charting library — Sync Studio draws a plain SVG checkpoint chart. Dark *arr-style UI. Lint: `oxlint`.
- External tools: `ffmpeg`, `ffprobe`, `mkvmerge` on PATH; `/api/health` reports availability.
- No Python anywhere. No app settings or secrets in env vars. Bootstrap flags: `--data-dir`/`--listen`/`--desktop`, plus `--log-level`/`--log-file` (ADR-0013) and the one-shot `rekey --keyfile` (ADR-0008); PUID/PGID in Docker.

## Commands
- Backend: `cd backend && go build ./cmd/dubarr && go test ./... && go vet ./...`
- Run native: `./dubarr --data-dir ./data --listen 127.0.0.1:8080`
- DSP: `cd backend && go test ./internal/dsp/...` (goldens live in `internal/dsp/testdata`; `-check` re-runs the capture)
- Windowed-decode parity (needs `testfiles/`; ~14 min, so it is opt-in and outside `go test ./...`): `DUBARR_PARITY=1 go test ./internal/syncjob ./internal/acceptance -run Windowed`
- Frontend: `cd frontend && npm install && npm run dev|build|lint` (build output → `backend/web`)
- Docker: `docker compose -f docker/docker-compose.yml up --build`

## Route registration contract (avoid main.go conflicts)
Each `internal/<pkg>` exposes `RegisterRoutes(mux *http.ServeMux, deps Dependencies)` (or an `http.Handler`). Only `cmd/dubarr` wires packages into the root router. Feature packages MUST NOT edit `cmd/dubarr/main.go` or another package's files.

## Rules
- Conventional commits. Work on a branch and merge via PR — never commit straight to `main`. Never commit media (`testfiles/`), `data/`, or secrets.
- Protected (orchestrator-only writes): `AGENTS.md`, `PLAN.md`, `GLOSSARY.md`, `docs/adr/**`, `LICENSE`, applied migrations.
- Applied migrations are frozen once shipped, and `backend/schema.sql` must stay byte-identical to `0001_initial.sql`. A few comments in `schema.sql`, `0001_initial.sql` and `0002_auth.sql` therefore still cite the deleted `IDEA.md`. That is a historical artefact of the freeze, not a live reference — do not "fix" it by editing an applied migration.
- Metadata is delegated: dubarr mirrors Radarr/Sonarr and has no TMDB/TVDB client, so a working media manager is a prerequisite (ADR-0014). The mirror is read-only; dubarr never writes it.
- One API base per product, the newest each ships: Sonarr `/api/v5`, Radarr `/api/v3` (ADR-0023). No version probe, no fallback to an older base. A 404 on a route that always exists is a configuration error naming the product, the path and the release that requires it. Ask for what you need explicitly (`includeSubresources=episodeFile`): an omitted sub-resource returns a legal `null` that reads exactly like "no file".
- Vault write-once against the pipeline: every job Copy-on-Works into WORK_DIR and verifies SHA-256; the pipeline never modifies VAULT_DIR in place. The only other vault writes are operator lifecycle actions — remove a track to Trash, replace it with better audio — both previewed and audited (ADR-0015).
- Linux only. Windows is dropped, not deferred: no Windows build, no Windows docs (ADR-0016).
- Target video never transcoded. Mux only tag-eligible targets (Processing Gate, ADR-0005 as amended by ADR-0009 — a user-configured filename-tag criterion with an optional arr criterion; donors are never tag-gated) unless the settings force override is allowed.
- Opt-in Transcode Rules (`audio.options`, ADR-0010): shipped empty; conditions match PRE-TEMPO vault metadata and the first matching enabled rule wins. When a rule matches, re-encode only the temp-folder copy (never VAULT_DIR) in ONE ffmpeg pass that also applies any required tempo factor; ffprobe must confirm codec/channels/duration or the job fails clearly, and the encoder is checked before the job starts. Atomic mux publication and language tags are unchanged; the appended track is named for its language (`Polish`), derived from the target it already has — the `mux.options.track_title_in_mkv` setting was retired in 0.6.0 (ADR-0004).
- Secrets AES-256-GCM (12-byte nonce || ciphertext || tag); master key OS keyring (native) / 0600 keyfile (Docker). Dubarr's own HTTP API authenticates with `Authorization: Bearer <api key>`; `X-Api-Key` is how dubarr authenticates to the arrs, not how clients authenticate to dubarr. Scrub secrets from logs/errors. Never print secrets.
- Paths canonicalized + `filepath.EvalSymlinks`, confined to configured roots. DATA_DIR 0700; db/keyfile 0600.
- Fixture tests run on COPIES in scratch; originals read-only. `go test ./...` green before any milestone gate.
- TDD: failing test → minimal implementation → green → commit. Report diff + command evidence, not claims.
- Subagents: no system installs, no destructive commands, no `git push`, no protected-file edits.
