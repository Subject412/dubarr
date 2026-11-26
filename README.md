# Dubarr

Self-hosted dubbing-sync appliance for a Radarr/Sonarr media library.

Dubarr watches a donor folder, identifies the media, archives the foreign-language
audio losslessly in a write-once **Audio Vault**, measures the timing offset
against the target's original track, and muxes the matched dub into the target
MKV. Video is never re-encoded, and the target is only replaced atomically after
the muxed file passes `ffprobe` validation.

Nothing is destructive without an explicit decision: a target must be matched,
tag-eligible and paired with a donor before it can be approved, and the Audio
Vault is never written to after its first archive.

Dubarr is pre-1.0: the HTTP API, settings schema and on-disk layout may still
change between minor releases. Pin an exact tag for production and read the
release notes before upgrading.

## What it does

- **Library** — browse what Radarr/Sonarr report, with per-instance sync health,
  so a never-synced or failing connection is visible instead of looking empty.
  Refresh on demand, or rescan a single item.
- **Queue** — every donor release, with a state that says what it needs next
  (no match, no donor, waiting for tags, ready) and an identify action that asks
  arr to parse the release name when the library cache is cold.
- **Sync and mux** — offset detection, lossless vault archiving, tag-gated
  muxing, `ffprobe` validation, atomic replace.
- **Settings** — arr connections, storage roots, processing gate, mux output.
  Connection tests validate values before you save them.

## Platform support

- **Linux** — native and Docker; the verified platform.
- **Windows** — the code is kept cross-platform, but Windows builds and CI are
  deferred and untested. Do not expect a supported Windows deployment yet.

## Quick start with Docker

Every value in the compose file has a working default, so it starts with no
configuration at all:

```sh
git clone https://github.com/Subject412/dubarr.git
cd dubarr/docker
docker compose up -d
```

This pulls `subject928/dubarr:latest`, publishes the UI on **8080** and mounts
`./media` writable at `/media`. Open <http://localhost:8080> and complete the
first-run wizard. Follow the logs with `docker logs -f dubarr`.

`/media` must be mounted read-write, not `:ro`, because a `replace` mux writes
beside the target file — a read-only mount fails with `EACCES` on the first
replace. If `./media` was created by Docker as root, the container user cannot
write to it either — give it to `PUID`/`PGID` first:

```sh
sudo chown -R 1000:1000 ./media
```

To adapt the stack, put overrides in `docker/.env` — Compose loads it from the
`docker/` directory — rather than editing the compose file:

```sh
PUID=1000
PGID=1000
CONFIG_PATH=./config
MEDIA_PATH=/srv/media
DUBARR_PORT=8090
DUBARR_IMAGE_TAG=latest
```

| Knob | Meaning |
| --- | --- |
| `PUID` / `PGID` | uid/gid the container process runs as; must own `MEDIA_PATH` |
| `CONFIG_PATH` | host directory bind-mounted at `/config` (database, keyfile, logs) |
| `MEDIA_PATH` | host directory bind-mounted at `/media` (read and write) |
| `DUBARR_PORT` | host port published onto the container's fixed `8080` |
| `DUBARR_IMAGE_TAG` | image tag to pull; defaults to `latest` |

`docker/README.md` documents every compose variable once.

## Configuration

All application settings and secrets are set in the **first-run wizard** and the
**Settings** page, then stored under the data directory (`/config` in Docker,
`--data-dir` natively). Dubarr deliberately does **not** read application
settings or credentials from environment variables, including Radarr/Sonarr URLs
and API keys, which are entered in the wizard or Settings and encrypted in the
database.

Secrets are encrypted with AES-256-GCM. Native installs keep the master key in
the OS keyring; containers have no keyring, so Dubarr falls back to a `0600`
keyfile at `/config/.master.key`. Moving between key sources is explicit via
`dubarr rekey --keyfile PATH`.

Logging is controlled by CLI flags, not environment variables:
`--log-level debug|info|warn|error` (default `info`) and the opt-in `--log-file`,
which also writes the rotating log under `<data-dir>/logs/`. In Compose, pass
them as a command override:

```yaml
command: ["--log-level", "debug", "--log-file"]
```

### Mux output destinations

`mux.options.output_mode` decides where a finished mux lands:

| Mode | Behaviour |
| --- | --- |
| `replace` (default) | The muxed file atomically replaces the library target; `keep_bak` keeps a `.bak` copy next to it. |
| `separate` | The muxed file is written to the **Mux output folder** (`storage.paths.output_dir`) and the library target is not touched. |

In `separate` mode the name is derived from the target basename plus
`.dubarr.mkv`: `Movie (2021) [TAG].mkv` becomes `Movie (2021) [TAG].dubarr.mkv`.
The destination is never overwritten and never auto-suffixed — a job whose
deliverable already exists fails with a clear error, so re-running a mux is an
explicit decision. The folder is a storage root of its own (absolute,
symlink-resolved, must exist) and may not be the Audio Vault or inside it.
`keep_bak` does not apply because nothing is replaced, and the arr rescan is
skipped because the library file is unchanged. Temp files are created inside the
destination folder so the final rename stays atomic on one filesystem: if the
destination is on another filesystem from the temp, the job fails instead of
falling back to a non-atomic copy. The tag gate and `ffprobe` validation still
apply, and the vault is never written. Completed jobs record the published path,
exposed as `output_path` by `GET /api/sync/jobs`.

**Never commit** `data/`, `docker/config/`, `docker/media/`, media files, `.env`
files, `.master.key` or `*.keyfile`. These paths are already in `.gitignore`;
secrets and media must stay out of Git.

## Build from source

### Prerequisites

| Tool | Version | Notes |
| --- | --- | --- |
| Go | 1.26 or newer | `backend/go.mod` declares `go 1.26.0`; the Docker image uses `golang:1.27-alpine` |
| Node.js + npm | Node 24 or newer | Vite 8 / TypeScript 7 (image: `node:24-alpine`) |
| Rust + Cargo | stable | `dubarr-sync`, edition 2021 |
| `ffmpeg`, `ffprobe` | any current build | extraction, probing, previews |
| `mkvmerge` | Mkvtoolnix | final muxing — `mkvtoolnix` package on Debian/Ubuntu |

`ffmpeg`, `ffprobe`, `mkvmerge` and `dubarr-sync` must be on `PATH`;
`/api/health` reports which ones are found.

### Build order

The SPA must be built **before** the Go binary, because the Go server embeds
`backend/web` into the executable at compile time.

```sh
# 1. SPA -> backend/web (embedded by the Go binary)
cd frontend
npm ci
npm run build

# 2. Go server (embeds the assets produced above)
cd ../backend
go build ./cmd/dubarr

# 3. Rust DSP CLI (sync and mux need it on PATH)
cd ../sync-engine
cargo build --release
mkdir -p "$HOME/.local/bin"
cp target/release/dubarr-sync "$HOME/.local/bin/"
```

Ensure `$HOME/.local/bin` is on your `PATH`, or copy `dubarr-sync` into any
directory that is. The DSP CLI is a separate program: without it the web UI and
library browsing work, but sync jobs and muxing do not. The Docker image bundles
`dubarr-sync` in `/app` and puts `/app` on `PATH`.

### Run

```sh
cd backend
./dubarr --data-dir ../data --listen 127.0.0.1:8080
```

Open <http://localhost:8080>, finish the first-run wizard, then add your
Radarr/Sonarr connections and library roots in **Settings**. The data directory
is created with `0700` and holds the database, key material and logs. For a
native desktop deployment, add the optional `--desktop` flag
(`./dubarr --data-dir ../data --desktop`) to open the UI in the default browser.

## Development

```sh
# Backend: build, vet, tests
cd backend && go build ./cmd/dubarr && go vet ./...
cd backend && go test ./... -timeout 1400s

# Frontend: dev server, production build (-> backend/web), lint, tests
cd frontend && npm install && npm run dev
cd frontend && npm run build && npm run lint && npm test

# DSP: build, tests, fmt, lint
cd sync-engine && cargo build && cargo test && cargo fmt --check && cargo clippy
```

`internal/analyze` and `internal/acceptance` rebuild the Rust engine in their
`TestMain`, which exceeds Go's default ten-minute per-package timeout; pass
`-timeout 1400s` when running the whole backend suite locally. CI does not need
it, because those packages skip the rebuild when Rust is absent.

Docker image build: `docker build -f docker/Dockerfile -t dubarr:latest .`

Published multi-arch images (`linux/amd64`, `linux/arm64`) are pushed to GHCR
(`ghcr.io/subject412/dubarr`) and Docker Hub (`subject928/dubarr`) on every
release tag.

## Documentation

- [CHANGELOG.md](CHANGELOG.md) — what changed in each release, and upgrade notes.
- [PLAN.md](PLAN.md) — milestones, defaults and implementation plan.
- [GLOSSARY.md](GLOSSARY.md) — domain vocabulary (donor, target, vault, gate…).
- [docs/adr/](docs/adr/) — architecture decision records.
- [docs/RELEASING.md](docs/RELEASING.md) — versioning policy and release process.
- [docker/README.md](docker/README.md) — container packaging and compose variables.
- [AGENTS.md](AGENTS.md) — contributor and agent conventions.
- [IDEA.md](IDEA.md) — historical design narrative, non-normative.

## License

MIT — see [LICENSE](LICENSE).
