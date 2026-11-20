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
- M3: approve → lossless extraction → validated .mka → atomic immutable Vault (0444) + Vault UI. Acceptance: .{iso2}.mka + sha256 + zero staging leftovers.
- M4: Rust dubarr-sync + Copy-on-Work + Reference Language Resolver + checkpoints + drift regression. Acceptance: cargo/go tests 100%; Vault byte-identical (RO-mount); accurate same-lang & lowpass reports.
- M5: Sync Studio + tag-gated atomic mux + arr Rescan. Acceptance: Shrek end-to-end; Minions fails loudly if ambiguous; zero vault/video modification, zero untagged mux, zero secret leaks.
