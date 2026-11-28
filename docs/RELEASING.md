# Releasing Dubarr

Release infrastructure for Dubarr: what a release is, how archives are built,
and how the resulting binary is installed.

## Semantic versioning (0.x)

Dubarr is pre-1.0, so the usual SemVer rules are read as:

- `0.MINOR.PATCH` — `MINOR` may contain breaking changes (HTTP API, settings
  schema, CLI, archive layout). `PATCH` is for backwards-compatible fixes only.
- While the major version is `0`, do not expect stability between `MINOR` bumps:
  pin an exact tag for production and read the release notes before upgrading.
- `1.0.0` is reserved for the point at which the HTTP API, settings schema and
  on-disk data layout are considered stable.

`0.6.0` is released. The next version is chosen from what the pairing model still
needs rather than from a schedule; there is no 1.0 target, because being
prepared for release is not the same as leaving beta. The version number
describes how stable the API is, not how ready the software is to run.

Pre-release identifiers (`v0.3.0-rc.1`) are permitted and publish a GitHub
pre-release via the normal tag flow. Build metadata (`+...`) is not used in
tags.

## What every 0.x release guarantees

The version number says how much the API may move. It says nothing about how
carefully data is treated — these hold in every release, 0.x included:

- No silent destruction: a target must be matched and paired with a donor before
  it can be approved, and the mux itself is tag-gated (unless the operator's
  force override is enabled) and validated with `ffprobe` before it replaces
  anything; the Audio Vault is write-once after first archive.
- The database schema is migrated forward automatically on start, and a
  migration that cannot complete rolls back rather than leaving a half-applied
  database.
- An upgrade does not require re-importing media or rebuilding the Audio Vault.

What 0.x does **not** promise:

- **API and on-disk stability.** The HTTP API, settings schema and on-disk
  layout may change between minor releases.
- **Windows.** Not supported and not built: no Windows archive is published and
  nothing is tested there (ADR-0016).
- **Automatic vault reorganisation.** 0.5.0 ships vault organising (ADR-0022),
  but never automatically: a template change moves nothing, and a move happens
  only when an operator previews a plan and applies it. That gating is a
  guarantee, not a convenience — the plan is previewed, preflighted (no swaps,
  nothing outside the vault, nothing cross-device, nothing occupied), applied in
  three phases with the renames undone in reverse on the first error, and
  reconciled with an audit run if even that fails. Operator-initiated removal and
  replacement are already defined (ADR-0015) and always go through the Vault
  Trash.
- **Stable internal Go packages.** Only the HTTP API is a contract; `internal/`
  packages may be restructured between minor releases.

## Tag format

- Annotated tag on `main`: `vX.Y.Z` (for example `v0.2.0`).
- The tag must match `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`; the release
  workflow rejects anything else before building.
- Tags are cut by a maintainer only, after CI on `main` is green. Never delete
  or move a published tag; publish a new patch release instead.

## What a tag triggers

Two workflows run on every `v*` tag push; both start from the same commit.

`.github/workflows/release.yml`:

1. validates the tag and sets up Node 24 and Go (from `backend/go.mod`);
2. builds the SPA (`frontend/`: `npm ci && npm run build`) into `backend/web`,
   where `go:embed` picks it up;
3. cross-compiles the Go backend with `CGO_ENABLED=0`;
4. writes the archives and `dubarr_<version>_checksums.txt`, then creates the
   GitHub release with `softprops/action-gh-release`.

`.github/workflows/docker-release.yml`:

1. validates the tag, sets up QEMU + Buildx and logs in to GHCR and Docker Hub;
2. derives shared tags from the version: `X.Y.Z`, `X.Y`, `latest`;
3. builds `docker/Dockerfile` natively per platform for `linux/amd64` and
   `linux/arm64` (frontend → Go → Debian slim runtime);
4. pushes the multi-arch manifest to `ghcr.io/subject412/dubarr` and
   `subject928/dubarr` with the same tag set.

Archive releases use the automatic `GITHUB_TOKEN` with `contents: write` scoped
to the release job only. The Docker job requests `packages: write` for GHCR and
needs two repository secrets:

| Secret | Purpose |
| --- | --- |
| `DOCKERHUB_USERNAME` | Docker Hub account name for `subject928/dubarr` |
| `DOCKERHUB_TOKEN` | Docker Hub access token (read/write/delete) |

Create both at Settings → Secrets and variables → Actions. GHCR images in the
public GitHub repository are pullable unauthenticated; the workflow verifies
the Docker Hub `subject928/dubarr` repository is public (flipping it if it was
created private), so pulls work without manual package/repository settings.

Actions are pinned to major versions. The archive job pins `ubuntu-22.04` so
the Linux glibc floor does not drift; the Docker job uses `ubuntu-24.04` for
Buildx/QEMU support.

## Archive layout

One archive per platform, all built from the same commit and tag:

| Platform | File | Contents |
| --- | --- | --- |
| linux/amd64 | `dubarr_<version>_linux_amd64.tar.gz` | `dubarr`, `README.txt`, `LICENSE` |
| linux/arm64 | `dubarr_<version>_linux_arm64.tar.gz` | `dubarr`, `README.txt`, `LICENSE` |

Each archive unpacks to a single top-level directory named like the archive
(`dubarr_<version>_linux_amd64/`). Checksums:

```sh
sha256sum -c dubarr_<version>_checksums.txt   # Linux (from the directory holding the archives)
```

The release does **not** contain ffmpeg, ffprobe or mkvmerge; install those from
your distribution (`ffmpeg`, `mkvtoolnix`) and make sure they are on `PATH`.
Container images are built separately by `.github/workflows/docker-release.yml`
and include ffmpeg, ffprobe and mkvmerge.

## One binary

Dubarr is a single static binary. The DSP engine is a Go package inside it, so
there is nothing to install beside `dubarr` and nothing to resolve on `PATH`; a
missing engine is no longer a failure mode, and `/api/health` reports only the
external tools.

Install on Linux (system-wide):

```sh
tar -xzf dubarr_<version>_linux_amd64.tar.gz
sudo install -m 0755 dubarr_<version>_linux_amd64/dubarr /usr/local/bin/
dubarr --data-dir /var/lib/dubarr --listen 127.0.0.1:8080
```

Or run from the unpacked directory:

```sh
./dubarr_<version>_linux_amd64/dubarr --data-dir ./data
```

Check `GET /api/health` and confirm `ffmpeg`, `ffprobe` and `mkvmerge` are
available; those are the only external tools dubarr needs.

## Cross-compilation decision

- **Go**: `CGO_ENABLED=0 GOOS/GOARCH` builds `linux/amd64` and `linux/arm64`
  directly; the SQLite driver is pure Go, so no C toolchain is involved.
- The Rust cross-linker step is gone with the engine: one Go binary per platform
  and no second artefact, so a release has one less way to fail and no glibc
  floor beyond Go's own.
- This avoids fragile cross-OS runner matrices: all archives are assembled in a
  single job, so a release either succeeds or fails as a unit.

## Release-cut checklist

1. `main` is green in `.github/workflows/ci.yml` (backend vet/tests, frontend
   lint/tests/build).
2. Write the `CHANGELOG.md` entry for this version: what changed, what breaks,
   and the upgrade steps if any. A `MAJOR` entry must state the migration step
   explicitly.
3. Confirm the working tree is clean and commit with a conventional message.
4. Create the annotated tag and push it (maintainer, from `main`):
   ```sh
   git tag -a v0.3.0 -m "dubarr v0.3.0"
   git push origin v0.3.0
   ```
5. Watch the `release` workflow; it fails if the tag is malformed or an
   expected archive is missing.
6. Download the archives, run `sha256sum -c`, and smoke-test at least
   `linux/amd64`: `./dubarr --data-dir ./data` then check `/api/health` shows
   `ffmpeg`, `ffprobe` and `mkvmerge`.
7. Publish/announce the generated release notes; do not edit published assets
   in place.
8. Watch the `Docker Release` workflow; confirm both registries show the
   multi-arch manifest (`docker buildx imagetools inspect
   subject928/dubarr:<version>`), then `docker pull subject928/dubarr:<version>`
   on the target host and check `/api/health` reports `ffmpeg`, `ffprobe` and
   `mkvmerge` available.

### Cutting 0.3.0 specifically

Before tagging, confirm the guarantees in "What every 0.x release guarantees" still
hold. The 0.3.0 `CHANGELOG.md` entry is what users read to decide whether to
upgrade from 0.2.0, so it must name every breaking change since that release
rather than summarising them — the approve-eligibility refusal and the Compose
changes are the two that can surprise an existing install.

## CI prerequisites

- GitHub Actions enabled for the repository; no self-hosted runners.
- Workflow permissions: the repository default should allow read-only tokens
  (the release job requests `contents: write`; the Docker job requests
  `packages: write`).
- Repository secrets `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` for the Docker
  release workflow; a public `subject928/dubarr` Docker Hub repository.
- The repository must not require status checks that these workflows do not
  produce; `ci.yml` job names are `backend (go)` and `frontend (vite)`.
