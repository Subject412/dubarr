# docker/

Placeholder for the Dubarr container packaging (T8).

Planned contents:
- `Dockerfile` — multi-stage build: node (frontend), rust (sync-engine), go (server).
- `docker-compose.yml` — appliance service with `PUID`/`PGID`, data volume and
  read-only media mounts.

Note: PUID/PGID are the only environment variables the appliance accepts;
application settings and secrets never come from the environment.
