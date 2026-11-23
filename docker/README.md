# docker/

Container packaging for the Dubarr appliance (T8).

- `Dockerfile` — multi-stage build: Vite/TypeScript frontend → static Go server
  (`CGO_ENABLED=0`, SPA embedded via `go:embed`) → Debian slim runtime with
  `ffmpeg`, `ffprobe`, `mkvmerge` and a `PUID`/`PGID` entrypoint.
- `entrypoint.sh` — remaps the `dubarr` user to `PUID`/`PGID` (default
  1000/1000), fixes `/config` ownership (0700) and drops privileges with `gosu`.
- `docker-compose.yml` — appliance service with a `/config` volume and a
  read-only media mount (`MEDIA_PATH`, default `./media`). The host port is
  published from `DUBARR_PORT` (default `8080`) onto the container's fixed
  `8080`.

Build and run from the repository root:

```sh
docker build -f docker/Dockerfile -t dubarr:latest .
docker run -d --name dubarr -p 8080:8080 \
  -e PUID=1000 -e PGID=1000 -v "$PWD/docker/config:/config" dubarr:latest
```

or `cd docker && docker compose up --build`.

To publish the web UI on a different host port, set `DUBARR_PORT` in `docker/.env`:

```sh
PUID=1000
PGID=1000
MEDIA_PATH=/srv/media
DUBARR_PORT=8090
```

`DUBARR_PORT=8090` publishes the service as `8090:8080` — the container still
listens on `8080`, so `docker/entrypoint.sh` and the image need no change.

`PUID` and `PGID` are the only environment variables the image honours;
`DUBARR_PORT` is read by Compose on the host to choose the published port and is
never passed into the container. Application settings and secrets never come
from the environment: the process
is started with explicit flags (`--data-dir /config --listen 0.0.0.0:8080`) and
everything else is configured in the first-run wizard and stored in `/config`.

In a container there is no OS keyring (no D-Bus session), so `/api/health`
reports `keyring_available: false` and the master key lives in
`/config/.master.key` (mode 0600).

`dubarr-sync` (Rust DSP CLI) is not built yet (M3/M4); `/api/health` reports it
as `available: false`.
