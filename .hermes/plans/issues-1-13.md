# Approved implementation plan — issues #1–#13 (branch `feat/issues-1-13`)

Source: all 13 open GitHub issues on Subject412/dubarr, read via `gh` by `fast` (no comments). Synthesised by `planner` into `/home/gwdawid/hermes-work/dubarr-design-tree.md`. Settled by the user in grill round 1 (Q1–Q23, all yes with two amendments). Docs written during the grill: `GLOSSARY.md` (Processing Gate, Eligibility Source, Output Mode, Mux Output Path, Reference Language Token, Prefer Original Language, Transcode Rule, Log Sink), ADRs 0009–0012.

## Execution contract
- Branch `feat/issues-1-13`; never commit to `main`. Conventional commits, TDD, `go test ./... && go vet ./...`, `cargo test`, frontend `npm run build && npm run lint` per slice.
- **No stop-for-OK gates** for M7–M10 (user Q1 amendment); milestones are work batches only. M1–M5 gates stay as settled.
- Protected files (`AGENTS.md`, `PLAN.md`, `GLOSSARY.md`, `docs/adr/**`, `LICENSE`, applied migrations) are orchestrator-only writes; implementers ship exact proposed text as a diff in the PR body. `PLAN.md` already carries the M7–M10 section and no-gate note.
- Implementers: one fresh coder per task/slice → tester (run code) → review (spec + quality). `git status --porcelain` checked after each review/tester/fast/planner call.

## Settled decisions (one line each)
- Q1 M7–M10 batches, no gates; Q2 protected-doc rule; Q3 `DUBARR_PORT` host-side only; Q4 separate-mode naming `<basename>.dubarr.mkv`, error on collision, atomic rename in destination, fail cross-device; Q5 ignore `keep_bak` + skip rescan in separate; Q6 new `storage.paths.output_dir` with root guard + `sync_mux_jobs.output_path` migration; Q7 shared `InfoTip` + `settings-help.ts`; Q8 root README + docker examples, Go 1.26/toolchain 1.27, `dubarr-sync` documented; Q9 role badge "Administrator".
- Q10 `POST /api/auth/password`, min 8, reject same-as-current, revoke other sessions only; Q11 localStorage theme, dark default, `data-theme` + `dark:` variant, Settings Appearance section; Q12 `org`/`all` tokens, `prefer_original_language:true`, skip original when it equals a target import language; Q13 wire `original_language_from_arr` (false disables priority 1 and `org`).
- Q14 default required tags -> empty list (ADR 0012); Q15 "Processing Gate" canonical, "Tdarr Gate" deprecated alias; Q16 `source_mode` filename|arr|both, AND across sources, arr labels by canonical path, fail closed, 300 s refresh; Q17 `arr.Client.Tags()` + migration 0003 + ADR 0009.
- Q18 frontend-only arr UI, blank key keeps stored value, masked suffix, 503 copy, skippable wizard step **plus user amendment: "Test connection" button using the existing API, Base URL accepts IP literals and DNS hostnames but requires an explicit `http://`/`https://` scheme (no auto-prefix)**.
- Q19 match pre-tempo vault metadata, min+max bitrate, validated ffmpeg layouts, global-only, first match; Q20 single ffmpeg encode folding tempo + transcode, ffprobe verify, IDEA/GLOSSARY update + ADR 0010; Q21 central slog, `--log-level` + runtime API, ring 1000, optional rotating file 10MB×3, admin-only polling UI, sink-boundary redaction; Q22 shared section registry + `?section=` + draft-preserving scroll + mobile chips + IntersectionObserver; Q23 explicit out-of-scope list.
- Assumptions taken from recommended options because the confirmation prompt timed out (reversible): keep milestones with no gates; Base URL accepts IP or hostname with an explicit scheme.

## Slices
1. **M7a** — #5 role badge, #1 Docker port var + docs. Low risk, tiny diff.
2. **M7b** — #7 theme tokens/provider/selector/bootstrap + tests.
3. **M7c** — #13 settings submenu + shared registry, then #3 tooltips + help copy + tests.
4. **M8a** — #6 password change API + account menu + session tests.
5. **M8b** — #10 arr connection UI incl. test-connection button and full-URL validation + tests.
6. **M8c** — #12 logging (slog, ring, optional file, admin API, UI, redaction) + tests.
7. **M8d** — #4 root README, compose/env examples, build-from-source, link/consistency fixes; validate with `docker compose config`.
8. **M9a** — #8 `org`/`all` + prefer-original, wire dead config, reflang/gate/tests.
9. **M9b** — #2 mux output modes, new root, publication path, rescan skip, job result + migration, UI, tests.
10. **M9c** — #9 arr-tag/root-folder eligibility: client method, mock, migration 0003, poller, gate, settings/UI, tests.
11. **M10** — #11 transcode rules: rule engine, single-pass ffmpeg path, ffprobe verify, UI, tests.

Out of scope: live arr in tests, Windows CI, Docker socket logs, Tdarr API, in-container port override, per-media transcode overrides, git push.
