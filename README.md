# Dubarr

Self-hosted dubbing-sync appliance for a Radarr/Sonarr media library. Dubarr
watches a donor folder, identifies the media, archives foreign-language audio
losslessly in a write-once **Audio Vault**, measures the timing offset against
the target's original track, and muxes the matched dub track into the target
MKV. Video is never re-encoded and the target is only replaced atomically after
the muxed file passes `ffprobe` validation.

Dubarr is pre-1.0 software: the API, settings schema and on-disk layout may
still change between minor releases. See [docs/RELEASING.md](docs/RELEASING.md).

## Platform support

- **Linux** (native and Docker) — verified platform.
- **Windows** — the code is kept cross-platform, but Windows builds and CI are
  deferred and untested ([ADR-0006](docs/adr/0006-linux-verification-windows-deferred.md)).
  Do not expect a supported Windows deployment yet.

## Quick start with Docker

The canonical stack is `docker/docker-compose.yml`, which pulls the published
image. Every value in it has a working default, so it starts with no
configuration at all:

```sh
git clone https://github.com/Subject412/dubarr.git
cd dubarr/docker
docker compose up -d
```

This pulls the published image (`subject928/dubarr:latest`) — no local build —
publishes the UI on **8080** and mounts `./media` into the container
read-write. Open <http://localhost:8080> and complete the first-run wizard
(administrator account, storage paths, and optionally Radarr/Sonarr
connections). Follow the container logs with `docker logs -f dubarr`.

`/media` is writable because the default `mux.options.output_mode=replace`
publishes the muxed file beside the Target in its own directory (optionally
keeping a `.bak`); switch to `separate` mode to leave the library untouched.

To adapt the stack to your host, put the overrides in `docker/.env` — Compose
loads it from the `docker/` directory — instead of editing the compose file:

```sh
PUID=1000
PGID=1000
CONFIG_PATH=./config
MEDIA_PATH=/srv/media
DUBARR_PORT=8090
```

`docker/docker-compose.yml` is the single documented example: it is
self-documenting and pull-based (no local build), carries inline `${VAR:-…}`
fallbacks so it also runs without a `.env`, and `docker/README.md` documents
every compose variable once.

## Configuration

All application settings and secrets are set in the **first-run wizard** and the
**Settings** page, then stored under the data directory (`/config` in Docker,
`--data-dir` natively). Dubarr deliberately does **not** read application
settings or credentials from environment variables
([ADR-0003](docs/adr/0003-zero-env-config-encrypted-secrets.md)); this includes
Radarr/Sonarr URLs and API keys, which are entered in the wizard or Settings
and encrypted in the database.

The only knobs at the Compose level are deployment concerns:

| Knob | Meaning |
| --- | --- |
| `PUID` / `PGID` | uid/gid the container process runs as |
| `CONFIG_PATH` | host directory bind-mounted at `/config` (database, keyfile, logs); default `./config` |
| `MEDIA_PATH` | host directory bind-mounted at `/media` (writable; required for the default replace-mode publication) |
| `DUBARR_PORT` | host port published onto the container's fixed `8080` |

The full variable reference lives in [docker/README.md](docker/README.md).

Secrets are encrypted with AES-256-GCM. Native installs keep the master key in
the OS keyring; containers have no keyring, so Dubarr falls back to a `0600`
keyfile at `/config/.master.key`. Moving between key sources is explicit via
`dubarr rekey --keyfile PATH` ([ADR-0008](docs/adr/0008-pinned-master-key-source.md)).

Logging is controlled by CLI flags, not environment variables
([ADR-0013](docs/adr/0013-logging-cli-flags.md)): `--log-level debug|info|warn|error`
(default `info`) and the opt-in `--log-file`, which also writes the rotating log
under `<data-dir>/logs/`. In Compose, pass them as a command override:

```yaml
command: ["--log-level", "debug", "--log-file"]
```

### Mux output destinations

`mux.options.output_mode` decides where a finished mux lands
([ADR-0011](docs/adr/0011-mux-output-destination-modes.md)):

| Mode | Behaviour |
| --- | --- |
| `replace` (default) | The muxed file atomically replaces the library Target; `keep_bak` keeps a `.bak` copy next to it. |
| `separate` | The muxed file is written to the **Mux output folder** (`storage.paths.output_dir`) and the library Target is not touched. |

In `separate` mode the name is derived from the source Target basename plus
`.dubarr.mkv`: `Movie (2021) [TAG].mkv` becomes
`Movie (2021) [TAG].dubarr.mkv`. The destination is never overwritten and
never auto-suffixed — a job whose deliverable already exists fails with a clear
error, so re-running a mux is an explicit decision. The folder is a storage
root of its own (absolute, symlink-resolved, must exist) and may not be the
Audio Vault or inside it; `keep_bak` does not apply because nothing is
replaced, and the `*arr` rescan is skipped because the library file is
unchanged. Temp files are created inside the destination folder so the final
rename stays atomic on one filesystem: if the destination is on another
filesystem from the temp, the job fails instead of falling back to a
non-atomic copy. The tag gate and `ffprobe` validation still apply, and the
vault is never written. Completed jobs record the published path, exposed as
`output_path` by `GET /api/sync/jobs`.

**Never commit** `data/`, `docker/config/`, `docker/media/`, media files,
`.env` files, `.master.key` or `*.keyfile`. These paths are already in
`.gitignore`; secrets and media must stay out of Git.

## Build from source

### Prerequisites

| Tool | Version | Notes |
| --- | --- | --- |
| Go | 1.26 or newer | `backend/go.mod` declares `go 1.26.0`; the Docker image uses `golang:1.27-alpine`, CI uses the `go.mod` version |
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
library browsing work, but sync jobs and muxing do not. The Docker image
bundles `dubarr-sync` in `/app` and puts `/app` on `PATH`, so `/api/health`
inside the container reports it available.

### Run

```sh
cd backend
./dubarr --data-dir ../data --listen 127.0.0.1:8080
```

Open <http://localhost:8080>, finish the first-run wizard, then add your
Radarr/Sonarr connections and library roots in **Settings**. The data directory
is created with `0700` and holds the database, key material and logs. For a
native desktop deployment, add the optional `--desktop` bootstrap flag
(`./dubarr --data-dir ../data --desktop`) to open the UI in the default
browser.

## Development

```sh
# Backend: build, tests, vet
cd backend && go build ./cmd/dubarr && go test ./... && go vet ./...

# Frontend: dev server, production build (-> backend/web), lint, tests
cd frontend && npm install && npm run dev
cd frontend && npm run build && npm run lint && npm test

# DSP: build, tests, fmt, lint
cd sync-engine && cargo build && cargo test && cargo fmt --check && cargo clippy
```

Docker image build: `docker build -f docker/Dockerfile -t dubarr:latest .`

Published multi-arch images (`linux/amd64`, `linux/arm64`) are pushed to GHCR
(`ghcr.io/subject412/dubarr`) and Docker Hub (`subject928/dubarr`) on every
release tag; see [docs/RELEASING.md](docs/RELEASING.md). `docker/docker-compose.yml`
already pulls `subject928/dubarr:latest`, so no local build is needed to run the
appliance; use the `docker build` line above only when testing changes to the
image itself.

## Documentation

- [IDEA.md](IDEA.md) — historical design narrative (non-normative; see the authority banner at the top).
- [GLOSSARY.md](GLOSSARY.md) — domain vocabulary (donor, target, vault, gate…).
- [PLAN.md](PLAN.md) — milestones, defaults and implementation plan.
- [docs/adr/](docs/adr/) — architecture decision records.
- [docs/RELEASING.md](docs/RELEASING.md) — release process and archive layout.
- [docker/README.md](docker/README.md) — container packaging and compose variables.
- [AGENTS.md](AGENTS.md) — contributor/agent contract and conventions.

## License

MIT — see [LICENSE](LICENSE).
