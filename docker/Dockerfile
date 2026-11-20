# syntax=docker/dockerfile:1
#
# Dubarr appliance image. Build from the repository root:
#
#   docker build -f docker/Dockerfile -t dubarr:latest .
#
# Three stages: Vite/TS frontend -> static Go server (CGO off, SPA embedded)
# -> slim runtime with ffmpeg + mkvtoolnix and a PUID/PGID entrypoint.
#
# The appliance takes no application environment variables: the runtime
# process is started with explicit flags only (--data-dir / --listen). PUID and
# PGID are consumed by the entrypoint to pick the container user's uid/gid.

# --------------------------------------------------------------------------
# Stage 1: frontend build (Vite 8 / TypeScript 7 need a recent Node).
# --------------------------------------------------------------------------
FROM node:24-alpine AS frontend

WORKDIR /build

# Dependencies first so source edits do not invalidate `npm ci`.
COPY frontend/package.json frontend/package-lock.json ./frontend/
RUN cd frontend && npm ci --no-audit --no-fund

# The whole frontend tree (node_modules/dist excluded via .dockerignore), plus
# the embed stub: Vite writes into ../backend/web and emptyOutDir is false, so
# doc.go must already exist there or the embed would lose its Go file.
COPY frontend/ ./frontend/
COPY backend/web/doc.go ./backend/web/doc.go

RUN cd frontend && npm run build

# --------------------------------------------------------------------------
# Stage 2: backend build. modernc.org/sqlite is pure Go, so CGO stays off.
# --------------------------------------------------------------------------
FROM golang:1.27-alpine AS backend

WORKDIR /src

ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local

COPY backend/go.mod backend/go.sum ./backend/
RUN cd backend && go mod download

COPY backend/ ./backend/
# Built SPA assets (hashed bundle + index.html) and doc.go from stage 1.
COPY --from=frontend /build/backend/web/ ./backend/web/

ARG VERSION=dev
RUN cd backend && go build -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /dubarr ./cmd/dubarr

# --------------------------------------------------------------------------
# Stage 3: runtime. Debian slim because mkvtoolnix/ffmpeg are first-class
# packages there; gosu drops privileges after the entrypoint fixes ownership.
# --------------------------------------------------------------------------
FROM debian:bookworm-slim AS runtime

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates \
        ffmpeg \
        mkvtoolnix \
        gosu \
        tzdata \
 && rm -rf /var/lib/apt/lists/*

# Fixed baseline ids; the entrypoint remaps them to PUID/PGID at start.
RUN groupadd -g 1000 dubarr \
 && useradd -u 1000 -g 1000 -M -d /app -s /usr/sbin/nologin dubarr

COPY --from=backend /dubarr /app/dubarr
COPY docker/entrypoint.sh /app/entrypoint.sh
RUN chmod 0755 /app/entrypoint.sh /app/dubarr \
 && mkdir -p /config \
 && chown dubarr:dubarr /config \
 && chmod 0700 /config

WORKDIR /app
VOLUME ["/config"]
EXPOSE 8080

# No DUBARR_* / DATA_DIR application env: the entrypoint passes --data-dir.
ENTRYPOINT ["/app/entrypoint.sh"]
