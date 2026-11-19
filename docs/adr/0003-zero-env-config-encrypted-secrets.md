# Zero environment-variable app config; encrypted secret store

All settings live in SQLite under DATA_DIR and all secrets are AES-256-GCM encrypted with a master key held in the OS keyring (native) or a 0600 keyfile (Docker); only --data-dir/--listen/--desktop bootstrap flags exist. We deliberately reject 12-factor env config so the appliance is self-contained and copying DATA_DIR alone never exposes API keys.

Status: accepted
