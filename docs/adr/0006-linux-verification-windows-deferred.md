# Linux-only runtime verification; Windows-ready code, CI deferred

Development and acceptance testing run on native Linux (and Docker); the code stays cross-platform (paths, keyring fallback, signals) but Windows builds/CI are deferred. We accept unverified Windows behavior now because the dev host is Linux and the primary deployment is Docker.

Status: accepted
