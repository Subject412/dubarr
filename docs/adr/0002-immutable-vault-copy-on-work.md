# Immutable Audio Vault with Copy-on-Work

Extracted tracks are written once into a separate Vault and never modified; every job copies them into the temp folder and verifies SHA-256 before processing. We accept the extra disk/IO cost so that Radarr/Sonarr or any external processing pipeline deleting or replacing a video can never destroy archived dubbing, and so sync/mux bugs cannot corrupt the archive.

Status: accepted; amended by ADR-0015 (operator-initiated removal and replacement are the only vault writes besides archiving).
