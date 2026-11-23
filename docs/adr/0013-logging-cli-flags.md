# Logging controls are CLI flags, not environment variables (exception to ADR-0003)

ADR-0003 closes app configuration to `--data-dir`/`--listen`/`--desktop`; ADR-0008 adds the one-shot `rekey --keyfile` exception. Issue #12 adds two more bootstrap flags: `--log-level debug|info|warn|error` (default `info`) and the opt-in `--log-file PATH` that enables the rotating file sink. Both are startup-only controls carried by the process arguments — Docker's `entrypoint.sh` forwards `"$@"` — so no application setting or secret enters the environment and the zero-env-config property remains intact.

We accept a longer flag list rather than an env-var escape hatch, because log verbosity is an operator/deployment concern rather than user data and must be visible in the process command line. The runtime log level is also writable through the admin `PUT /api/logs/level` endpoint; that change is process-wide until restart. The file sink stays off unless explicitly requested, and log records are redacted before reaching stderr, the in-memory ring or the file.

Status: accepted
