# Mux output destination modes

Mux publication has two modes: `replace` (the default and current behaviour — atomic replace of the Target with an optional `.bak`) and `separate` (write `<basename>.dubarr.mkv` into a configured output directory, refuse to overwrite an existing file, skip arr rescan).

`separate` preserves the original Target for review or archival, but places the deliverable outside the arr-managed library: there is no rescan, no library visibility and no automatic installation. We keep `replace` as the default to preserve all existing behaviour, create the temp file in the destination directory so the rename stays atomic, and fail rather than copy across filesystems. The Tdarr tag gate still applies to the source Target in both modes, and nothing is ever written into the Audio Vault (ADR-0002).

Status: accepted
