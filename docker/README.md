# docker/

Container packaging for the Dubarr appliance. For the project overview and
source-build instructions, start at the [root README](../README.md).

- `Dockerfile` — multi-stage build: Vite/TypeScript frontend → static Go server
  (`CGO_ENABLED=0`, SPA embedded via `go:embed`) → Debian slim runtime with
  `ffmpeg`, `ffprobe`, `mkvmerge` and a `PUID`/`PGID` entrypoint.
- `entrypoint.sh` — remaps the `dubarr` user to `PUID`/`PGID` (default
  1000/1000), fixes `/config` ownership (0700) and drops privileges with
  `gosu`.
- `docker-compose.yml` — the canonical pull-based appliance service: it runs
  `subject928/dubarr:latest` (no local build), carries inline `${VAR:-…}`
  fallbacks for every knob, so `docker compose up` from this directory works
  with no `.env`; it documents itself in comments and every variable is listed
  once in the table below. The image is published to Docker Hub
  (`subject928/dubarr`) and GHCR (`ghcr.io/subject412/dubarr`) with tags
  `X.Y.Z`, `X.Y` and `latest` by the tag-triggered Docker Release workflow.

## Compose variables

This is the single reference for the Compose variables. `docker-compose.yml`
carries a `${VAR:-…}` fallback for each one, so it runs with no `.env`; the
defaults below are what it uses. A `docker/.env` overrides any of them.

| Variable | Default | Consumed by | Purpose |
| --- | --- | --- | --- |
| `PUID` | `1000` | entrypoint, inside the container | uid the `dubarr` process runs as; must own `/config` and be able to read and write the media mount |
| `PGID` | `1000` | entrypoint, inside the container | gid for the same process |
| `CONFIG_PATH` | `./config` | Compose, on the host | host directory bind-mounted at `/config` for appliance state (database, master keyfile, logs); back it up |
| `MEDIA_PATH` | `./media` | Compose, on the host | host directory bind-mounted at `/media` (writable) |
| `DUBARR_PORT` | `8080` | Compose, on the host | published host port for the web UI |

`PUID` and `PGID` are the only environment variables the image honours.
`CONFIG_PATH`, `MEDIA_PATH` and `DUBARR_PORT` never enter the container: Compose
resolves the first two when it builds the bind mounts, and `DUBARR_PORT=8090`
publishes `8090:8080` while the process keeps listening on `0.0.0.0:8080`, so
`entrypoint.sh` and the image need no change.

Application settings and secrets never come from the environment. The process
is started with explicit flags (`--data-dir /config --listen 0.0.0.0:8080`) and
everything else is configured in the first-run wizard and stored in `/config`
(ADR-0003).

To change any of these values, put them in `docker/.env` (Compose loads it from
this directory) or export them in the shell:

```sh
PUID=1000
PGID=1000
CONFIG_PATH=./config
MEDIA_PATH=/srv/media
DUBARR_PORT=8090
```

## Run

From this directory, with the defaults:

```sh
cd docker
docker compose up -d
```

This pulls `subject928/dubarr:latest` from Docker Hub — no local build. The same
image is published to `ghcr.io/subject412/dubarr:latest`; swap the `image:` line
in `docker-compose.yml` to use that registry instead.

`docker/.env` overrides `PUID`, `PGID`, `CONFIG_PATH`, `MEDIA_PATH` and
`DUBARR_PORT` without touching the compose file.

### Recommended storage layout

The storage roots are independent settings under **Settings → Storage**: keep
each on its own directory, never inside another. For a single combined bind
(`MEDIA_PATH` mounted at `/media`) use:

| Setting | Value |
| --- | --- |
| `storage.paths.watch_dir` | `/media/download` |
| `storage.paths.vault_dir` | `/media/vault` |
| `storage.paths.media_roots` | `/media/content` |
| `storage.paths.work_dir` | `/config/work` |

`/config/work` is also the default when `work_dir` is left unset
(`<DATA_DIR>/work`). `storage.paths.output_dir` is only used when
`mux.options.output_mode=separate`, and it must not sit inside the vault.

Or build and run without Compose from the repository root:

```sh
docker build -f docker/Dockerfile -t dubarr:latest .
docker run -d --name dubarr -p 8080:8080 \
  -e PUID=1000 -e PGID=1000 \
  -v "$PWD/docker/config:/config" -v "$PWD/docker/media:/media" dubarr:latest
```

The UI is served on the published port (8080 by default).

## Secrets

In a container there is no OS keyring (no D-Bus session), so `/api/health`
reports `keyring_available: false` and the master key lives in
`/config/.master.key` (mode 0600). Native installs use the OS keyring instead;
switching between the two is explicit via `dubarr rekey --keyfile PATH`
(ADR-0008).

## Logs

Two ways to read Dubarr logs, neither requiring the Docker socket:

- **Host side** — everything the process writes to stderr goes through the
  container's log driver:

  ```sh
  docker logs -f dubarr
  ```

- **App side** — the admin-only **Logs** tab in the web GUI shows the same
  entries from the in-memory ring (last 1000), with level and component
  filters. The runtime level can be changed there without a restart.

Logs written to `/config/logs/dubarr.log` survive container recreation because
`/config` is the bind-mounted data volume. The file sink is opt-in and rotated
(10 MB × 3); enable it with `--log-file`:

```yaml
    command: ["--log-level", "debug", "--log-file"]
```

`--log-level` and `--log-file` are CLI flags (ADR-0013), not environment
variables: `entrypoint.sh` forwards extra arguments after its own
`--data-dir`/`--listen`. Changing the level needs no app setting and no env var
(ADR-0003). Do **not** mount `/var/run/docker.sock` for logging.

## DSP CLI

`dubarr-sync` (Rust) is required for sync and mux jobs. It is bundled at
`/app/dubarr-sync` and `/app` is on the container `PATH`, so `/api/health`
reports it available out of the box. Native installs still keep both binaries
side by side on `PATH`; release archives ship them together (see
[docs/RELEASING.md](../docs/RELEASING.md)).
