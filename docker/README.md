# docker/

Container packaging for the Dubarr appliance. For the project overview and
source-build instructions, start at the [root README](../README.md).

- `Dockerfile` — multi-stage build: Vite/TypeScript frontend → static Go server
  (`CGO_ENABLED=0`, SPA embedded via `go:embed`) → Debian slim runtime with
  `ffmpeg`, `ffprobe`, `mkvmerge` and a `PUID`/`PGID` entrypoint.
- `entrypoint.sh` — remaps the `dubarr` user to `PUID`/`PGID` (default
  1000/1000), fixes `/config` ownership (0700) and drops privileges with
  `gosu`.
- `docker-compose.yml` — the canonical, all-defaults appliance service.
- `docker-compose.example.yml` + `.env.example` — copy-ready variant with
  non-default host values (port `8090`, custom media path, different
  `PUID`/`PGID`, explicit config bind mount).

## Compose variables

This is the single reference for the variables both compose files use. Both
files carry inline `${VAR:-…}` fallbacks, so each runs with no `.env`: the
canonical file falls back to the defaults below, while the example deliberately
falls back to non-default values (port `8090`, `/srv/media/library`,
`PUID`/`PGID` `1001`). A `docker/.env` overrides either set.

| Variable | Canonical default | Example fallback | Consumed by | Purpose |
| --- | --- | --- | --- | --- |
| `PUID` | `1000` | `1001` | entrypoint, inside the container | uid the `dubarr` process runs as; must own `/config` and be able to read the media mount |
| `PGID` | `1000` | `1001` | entrypoint, inside the container | gid for the same process |
| `MEDIA_PATH` | `./media` | `/srv/media/library` | Compose, on the host | host directory bind-mounted read-only at `/media` |
| `DUBARR_PORT` | `8080` | `8090` | Compose, on the host | published host port for the web UI |

`PUID` and `PGID` are the only environment variables the image honours.
`MEDIA_PATH` and `DUBARR_PORT` never enter the container: `DUBARR_PORT=8090`
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
MEDIA_PATH=/srv/media
DUBARR_PORT=8090
```

## Run

With the defaults:

```sh
cd docker
docker compose up --build
```

With the example values:

```sh
cd docker
cp .env.example .env    # edit PUID/PGID and MEDIA_PATH
docker compose -f docker-compose.example.yml up --build
```

Or build and run without Compose from the repository root:

```sh
docker build -f docker/Dockerfile -t dubarr:latest .
docker run -d --name dubarr -p 8080:8080 \
  -e PUID=1000 -e PGID=1000 -v "$PWD/docker/config:/config" dubarr:latest
```

The UI is served on the published port (8080 by default, 8090 for the example
above).

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

`dubarr-sync` (Rust) is required for sync and mux jobs. The image does not
include it, so `/api/health` reports it as `available: false` inside the
container unless the binary is provided (bind-mount it and extend `PATH`, or
build a derived image). A release archive ships `dubarr-sync` beside `dubarr`;
see [docs/RELEASING.md](../docs/RELEASING.md).
