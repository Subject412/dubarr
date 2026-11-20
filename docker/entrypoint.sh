#!/bin/sh
# Dubarr container entrypoint.
#
# PUID/PGID (default 1000/1000) are the only environment variables this image
# honours. They select the uid/gid of the unprivileged process; all application
# configuration is passed as flags, never through the environment.
set -eu

DATA_DIR=/config
LISTEN=0.0.0.0:8080
APP_USER=dubarr
APP_GROUP=dubarr

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"

log() { printf 'dubarr-entrypoint: %s\n' "$*" >&2; }

is_id() {
    case "$1" in
        ''|*[!0-9]*) return 1 ;;
    esac
    [ "$1" -le 2147483647 ]
}

if ! is_id "$PUID" || ! is_id "$PGID"; then
    log "PUID and PGID must be numeric (got PUID='$PUID' PGID='$PGID')"
    exit 64
fi

if [ "$(id -u)" = "0" ]; then
    # Remap the image's baseline user to the requested ids. -o tolerates an id
    # that already exists in the container (e.g. 1000 = a stock Debian user).
    if [ "$(id -g "$APP_USER")" -ne "$PGID" ]; then
        groupmod -o -g "$PGID" "$APP_GROUP" || log "could not set gid $PGID"
    fi
    if [ "$(id -u "$APP_USER")" -ne "$PUID" ]; then
        usermod -o -u "$PUID" "$APP_USER" || log "could not set uid $PUID"
    fi

    # Data dir: owned by the runtime user, private. A read-only bind mount
    # makes chown/chmod fail — warn and continue, the app validates perms.
    mkdir -p "$DATA_DIR" 2>/dev/null || true
    if ! chown -R "$PUID:$PGID" "$DATA_DIR" 2>/dev/null; then
        log "warning: could not chown $DATA_DIR (read-only mount?)"
    fi
    if ! chmod 0700 "$DATA_DIR" 2>/dev/null; then
        log "warning: could not chmod 0700 $DATA_DIR (read-only mount?)"
    fi

    log "starting as uid=$PUID gid=$PGID data=$DATA_DIR listen=$LISTEN"
    exec gosu "$PUID:$PGID" /app/dubarr --data-dir "$DATA_DIR" --listen "$LISTEN" "$@"
fi

log "already unprivileged (uid=$(id -u)); skipping uid/gid remap"
exec /app/dubarr --data-dir "$DATA_DIR" --listen "$LISTEN" "$@"
