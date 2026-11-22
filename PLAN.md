# Dubarr — Implementation Plan

Spec: `IDEA.md`. Domain: `GLOSSARY.md`. Decisions: `docs/adr/0001–0007`. Contract: `AGENTS.md`.
Execution: subagent-driven-development (fresh implementer per task → spec review → quality review). Gate protocol: **stop for user OK at each milestone gate (M1→M5)**.

## Settled defaults (grilling Q1–Q15)
- `required_filename_tags: ["[ArcA310]"]` (Tdarr node/GPU processed-marker), `tag_match_mode: ANY`, `tag_case_sensitive: false`, `enforce_tag_filter: true`.
- Target import languages `["pl"]`; extra reference languages `["en"]`; original language from arr; commentary excluded.
- Export default: policy-selected tracks exported (dub + original + selected reference); beyond-policy tracks opt-in (Q14b).
- Untagged (`und`) donor tracks: manual pick only, never auto-imported.
- Vault read-only 0444: ON (`settings.security.vault_read_only: true`).
- `.bak` after verified mux: keep (`settings.mux.keep_bak: true`), opt-in auto-clean.
- Track title in MKV: `Polski Dubbing` (configurable).
- Verification: native Linux + Docker (M1/M5); Windows-ready code, Windows CI deferred.
- arr integration: mock-first (httptest), no live instance.
- Repo: `~/Dokumenty/GitHub/dubarr`, branch main, conventional commits, local-only (remote/push deferred).

## Proposed resolutions (approved Phase 0)
- Checkpoints (analysis): 6 @ 10/25/40/55/70/85% of shorter duration, 150s windows, 8000 Hz mono `pcm_f32le`. Preview Slices (studio): 60s @ 10/25/50/75/90%.
- `require_av1_warning: true`, `require_av1_strict: false`.
- `force_ignore_tag_filter` honored only when `settings.security.allow_force_tag_override: true` (default false).
- `dubarr-sync --max-offset-sec` default 180.
- `--desktop` = open default browser at listen addr (`xdg-open`); no embedded webview v1.
- Watch scan 10s (stability: two size checks ≥4s apart); arr sync manual + 5-min poll.
- Go directive 1.26 (floor required by modernc.org/sqlite v1.62; toolchain 1.27), Rust edition 2021, module `github.com/Subject412/dubarr/backend`. Docker build image must be golang 1.27+.
- License: MIT. Deferred: GitHub remote/push, Windows CI, Linux CI (post-M1).

## Fixture matrix (testfiles/, originals read-only, tests on copies)
| Pair | Donor | Target | Proves |
|---|---|---|---|
| Minions | MP4 AC3 2.0 `und`, 11.62s shorter, unknown cut | MKV AV1 Opus 7.1 `eng` | und manual pick; lowpass_fallback; drift/jump on unknown cut → fail loudly to ready_for_review if ambiguous |
| Shrek | MKV MULTi EAC3 5.1 `pol` + EAC3 5.1 `eng` sibling, 0.54s shorter, 23.976 | MKV AV1 Opus 7.1 `eng` | tagged pl auto-select; same_lang_reference (eng↔eng Priority 1); near-constant offset |

## Milestone 1 — Foundations (vertical slice)
Acceptance: container builds & starts with zero app env vars; Setup Wizard creates admin, persists paths/rules in DATA_DIR; `/api/health` reports ffmpeg/ffprobe/mkvmerge available; native run works; `go test ./...` green.

### T1 — Scaffolding (coder)
Create monorepo skeleton: `backend/go.mod` (module …/backend, go 1.23), `backend/cmd/dubarr/main.go` (flag parsing --data-dir/--listen/--desktop, zero-config defaults: ./data + 127.0.0.1:8080 native), `backend/internal/{auth,secrets,settings,store,api,arr,watch,vault,sync,mux}/` package dirs with doc.go, `backend/web/` embed placeholder, `sync-engine/` placeholder Cargo crate (dubarr-sync, edition 2021, clap/hound/rustfft/serde deps declared, main.rs stub), `frontend/` Vite React-TS + Tailwind init, `docker/` placeholder. Expand .gitignore (data/, node_modules/, target/, dist/, *.db). Proving: `go build ./cmd/dubarr` compiles stub; `cargo build` placeholder; `npm run build` frontend shell. TDD where logic exists (flag defaults test).

### T2 — SQLite store + schema (coder)
`internal/store`: modernc.org/sqlite, WAL, DATA_DIR 0700, db 0600. `schema.sql` + migration runner. M1 tables: users, sessions, api_tokens, settings (kv json), secrets (encrypted blobs), schema_migrations. Also create arr/import/vault/sync tables per spec schema (used M2+). TDD: open/migrate/idempotent, CRUD settings, file perms. Proving: `go test ./internal/store/...` green.

### T3 — Auth (coder)
`internal/auth`: Argon2id password hash, signed HttpOnly SameSite=Lax session cookies, API tokens stored SHA-256 hashed. `RegisterRoutes`: POST /api/auth/setup (first-run admin), /login, /logout, GET /api/auth/me, token CRUD. Middleware: RequireAuth, RequireAdmin. TDD: hash/verify, session sign/validate, token hash, setup-only-once, 401/403. Proving: `go test ./internal/auth/...` green. Depends T2.

### T4 — Secret store (coder)
`internal/secrets`: AES-256-GCM, unique 96-bit nonce per record (nonce||ciphertext||tag). Master key: OS keyring native (Linux Secret Service via go-keyring), 0600 `.master.key` CSPRNG fallback (Docker/headless), optional keyfile path / Argon2id unlock passphrase. Encrypt/decrypt arr API keys; masked responses (api_key_masked, has_api_key). TDD: roundtrip, nonce uniqueness, keyring fallback, masking, wrong-key failure. Proving: `go test ./internal/secrets/...` green. Depends T2.

### T5 — Settings + system browse (coder)
`internal/settings`: /api/settings GET/PUT (paths, languages.policy, target.tag_filter, security.options, sync.defaults, mux), sanitized /api/settings/export (no keys/hashes). `internal/api`: /api/system/browse canonical path validation (EvalSymlinks, confine roots). Seed defaults from "Settled defaults" above. TDD: CRUD, default seeding, path traversal rejection, export sanitization. Proving: green. Depends T2.

### T6 — Health + server wiring (coder)
`internal/api`: /api/health (tool detection ffmpeg/ffprobe/mkvmerge via exec.LookPath + version, db status, version). `cmd/dubarr`: wire all RegisterRoutes, embed frontend (go:embed web), graceful shutdown, --desktop xdg-open. TDD: health reports tools, router wiring smoke. Proving: native run boots, /api/health green, `go test ./...` green. Depends T3,T4,T5.

### T7 — Frontend shell + wizard (coder_free)
React shell: dark *arr-style layout, react-router, tanstack-query, Tailwind. First-Run Setup Wizard (create admin, configure paths WATCH/VAULT/WORK/MEDIA_ROOTS, language policy, tag filter). Settings skeleton pages. API client (fetch, credentials include). Build → backend/web. Proving: `npm run build` succeeds, embed serves SPA, wizard flows against running backend. Depends T6 (API contract).

### T8 — Docker (coder)
`docker/Dockerfile` multi-stage: node build frontend → go build backend (CGO_ENABLED=0, embed) → final debian-slim/alpine with ffmpeg + mkvtoolnix-cli, non-root PUID/PGID entrypoint, DATA_DIR /config, EXPOSE 8080. `docker/docker-compose.yml`: zero app env (PUID/PGID only), volumes for /config + media, port 8080. Proving: `docker compose up --build` starts, /api/health reports all tools, wizard works in container. Depends T7.

### T9 — M1 integration verification (review/tester)
Full acceptance pass: container zero-env build+start, wizard creates admin + persists paths/rules in DATA_DIR, /api/health all tools, native run, `go test ./...` + `go vet` green, no secrets in env/logs. Report evidence. GATE: stop for user OK before M2.

## Milestones 2–5 (outline; detailed tasks drafted at each gate)
- M2: arr clients (mock-first) + watch scanner + ffprobe/ISO-2 + Tdarr Gate eligibility + Approval Queue UI. Acceptance: donor drop → approval card pre-selected; eligible vs waiting_for_tags.

### M2 tasks (drafted at gate, 2026-11-22)
- M2-1 — Mock-first arr integration: `internal/arr` client interface (Radarr + Sonarr REST v3) with a mock server implementation (ADR-0007); real client behind the same interface; table-driven tests against mocks (list series/movies, quality profiles, paths, test-connection). No live keys in tests.
- M2-2 — Arr settings + key storage: base URLs + API keys stored via `internal/secrets` (UI-only entry, never env/chat); settings CRUD + connection-test endpoint; per-arr enabled flag.
- M2-3 — Watch scanner: `internal/watch` periodic scan (10s default, settled), detects new media files in configured roots (canonicalized paths, symlink-confined), debounces, registers candidates in DB.
- M2-4 — ffprobe probe + ISO-639-2: probe target/donor audio tracks (codec, channels, language tags) via ffprobe; map language names/codes to ISO-639-2; cache probe results per file (mtime-keyed).
- M2-5 — Tdarr Gate eligibility: `[ArcA310]` tag gate (settled; force-override OFF): tag-eligible vs waiting_for_tags classification on candidates; rule-based export policy (policy tracks default, extras opt-in — settled).
- M2-6 — Approval Queue: backend queue API + frontend queue UI; donor drop → approval card pre-selected (target + matched donor, languages, eligibility badge); approve/reject actions (no mux in M2 — approval only).
- M3: approve → lossless extraction → validated .mka → atomic immutable Vault (0444) + Vault UI. Acceptance: .{iso2}.mka + sha256 + zero staging leftovers.

### M3 tasks (drafted at gate, 2026-11-22)
- M3-1 — Extraction worker: consume `extracting` queue rows; mkvextract lossless audio from the donor into work dir; validate the extracted track (codec/channels via probe; duration sanity vs donor); per-job staging dir under work/, cleanup on failure; concurrency 1 (serialized jobs); crash-safe (stale staging dirs reaped at boot).
- M3-2 — Vault write pipeline: normalize to `.{iso2}.mka` naming per GLOSSARY; sha256 sidecar manifest; single atomic rename into vault dir; write-once (refuse overwrite existing vault entry); final perms 0444, dir 0555 where supported; vault dir from storage.paths.vault_dir.
- M3-3 — Vault UI: vault page listing entries (name, size, sha256, source target/donor, created); no delete-by-default (`.bak`-style opt-in clean is settled for M4+); read-only.
- M3-4 — Approve→Vault integration: approve action triggers extraction→vault pipeline end-to-end; queue status advances to vaulted (or failed with reason); acceptance run: Shrek-like fixture produces valid .pl.mka + sha256, zero staging leftovers on success AND failure paths.
- M4: Rust dubarr-sync + Copy-on-Work + Reference Language Resolver + checkpoints + drift regression. Acceptance: cargo/go tests 100%; Vault byte-identical (RO-mount); accurate same-lang & lowpass reports.

### M4 tasks (drafted at gate, 2026-11-22)
- M4-1 — Rust `dubarr-sync` CLI core (sync-engine/): read WAV PCM slices (hound), compute offset_ms = t_target − t_donor via cross-correlation (rustfft), stdio/JSON output contract; golden tests with synthetic WAVs (known offsets); clap CLI.
- M4-2 — Go↔Rust bridge: Go invokes the `dubarr-sync` binary (PATH, health-reported), passes job JSON on stdin, parses result; typed errors; timeout; version handshake; absent-binary ⇒ typed error (matches /api/health dubarr-sync:false).
- M4-3 — Copy-on-Work: jobs copy vault `.mka` + target audio into WORK_DIR (SHA-256 verify against vault manifest), never touch VAULT_DIR (settled: vault write-once + copy-on-work + SHA-256); cleanup on completion/failure.
- M4-4 — Reference Language Resolver: per IDEA.md priority (same_lang_reference original-language pair first, then policy languages); pure functions + tests over track metadata (probe types). NOTE (reviewer-accepted deviation): IDEA.md:367 prefers "channel count/bitrate" for tie-breaks; probe.AudioTrack carries no bitrate, so channels-then-index is the implemented proxy (documented in reflang.go). If bitrate matters later, extend probe to expose it.
- M4-5 — Checkpoints + drift: multi-point offset analysis at sync.defaults.checkpoint_positions_pct with checkpoint_window_sec; drift report (per-point offsets, monotonic sanity, max drift); lowpass/same-lang report accuracy tests.
- M4-6 — Sync job API + status: POST /api/sync/jobs (target_track_id, target_library_cache_id, force_ignore_tag_filter gated by settled allow_force_tag_override=OFF), job status polling, results persisted.
- M5: Sync Studio + tag-gated atomic mux + arr Rescan. Acceptance: Shrek end-to-end; Minions fails loudly if ambiguous; zero vault/video modification, zero untagged mux, zero secret leaks.
