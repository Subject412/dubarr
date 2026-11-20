# docker/

Container packaging for the Dubarr appliance (T8).

- `Dockerfile` — multi-stage build: Vite/TypeScript frontend → static Go server
  (`CGO_ENABLED=0`, SPA embedded via `go:embed`) → Debian slim runtime with
  `ffmpeg`, `ffprobe`, `mkvmerge` and a `PUID`/`PGID` entrypoint.
- `entrypoint.sh` — remaps the `dubarr` user to `PUID`/`PGID` (default
  1000/1000), fixes `/config` ownership (0700) and drops privileges with `gosu`.
- `docker-compose.yml` — appliance service with a `/config` volume and a
  read-only media mount (`MEDIA_PATH`, default `./media`).

Build and run from the repository root:

```sh
docker build -f docker/Dockerfile -t dubarr:latest .
docker run -d --name dubarr -p 8080:8080 \
  -e PUID=1000 -e PGID=1000 -v "$PWD/docker/config:/config" dubarr:latest
```

or `cd docker && docker compose up --build`.

`PUID` and `PGID` are the only environment variables the image honours.
Application settings and secrets never come from the environment: the process
is started with explicit flags (`--data-dir /config --listen 0.0.0.0:8080`) and
everything else is configured in the first-run wizard and stored in `/config`.

In a container there is no OS keyring (no D-Bus session), so `/api/health`
reports `keyring_available: false` and the master key lives in
`/config/.master.key` (mode 0600).

`dubarr-sync` (Rust DSP CLI) is not built yet (M3/M4); `/api/health` reports it
as `available: false`.
