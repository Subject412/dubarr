# Zero environment-variable app config; encrypted secret store

Two prohibitions and one explicit allowlist govern every environment variable this project reads.

**Secrets never come from the environment.** No API key, token, password or master key is read from an environment variable. Secrets are AES-256-GCM rows inside the SQLite database under `DATA_DIR`, sealed with a master key held in the OS keyring (native) or a 0600 keyfile (Docker), pinned per data-dir by ADR-0008.

**Application configuration never comes from the environment.** Settings, arr URLs and keys, storage paths and language rules live in the database and change through flags, the WebUI or the REST API. There is no `os.Getenv`/`os.LookupEnv` for application configuration anywhere in the backend.

**Permitted operational environment (allowlist):**

| Variable | Where consumed | Purpose |
| :--- | :--- | :--- |
| `PUID` | container, `docker/entrypoint.sh` | uid the unprivileged process runs as |
| `PGID` | container, `docker/entrypoint.sh` | gid for the same process |
| `MEDIA_PATH` | host-side Compose only | host directory bind-mounted into the container |
| `DUBARR_PORT` | host-side Compose only | published host port |

`MEDIA_PATH` and `DUBARR_PORT` never enter the container; they are Compose substitutions evaluated on the host. Build/CI variables are outside this ADR's scope.

Runtime behaviour is otherwise controlled by CLI flags plus stored settings: `--data-dir`, `--listen`, `--desktop`, the logging flags `--log-level` / `--log-file` (ADR-0013), and the one-shot `rekey --keyfile PATH` (ADR-0008). Both flag exceptions are startup-only process arguments, not environment variables, and both are recorded here so the ADR set stays internally consistent. **Any new environment variable requires an explicit ADR exception.**

We keep copying `DATA_DIR` as the single self-contained unit: it never exposes API keys in plaintext, and no operator has to reconstruct configuration from a shell environment.

Status: accepted

Supersedes: the original ADR-0003 wording, which claimed "only `--data-dir`/`--listen`/`--desktop` bootstrap flags exist" and left operators with no written rule for legitimate environment variables. Reconciles ADR-0008 and ADR-0013.
