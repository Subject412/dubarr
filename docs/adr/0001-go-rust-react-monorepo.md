# Two-binary appliance: Go backend + Rust DSP CLI, React/TypeScript frontend — no Python

Dubarr ships as **two executables that always travel together**: the Go backend `dubarr` (HTTP API, embedded React/TypeScript SPA, pure-Go SQLite, job queue, arr clients, ffmpeg/mkvmerge orchestration) and the Rust DSP CLI `dubarr-sync` (FFT cross-correlation). `dubarr` resolves the engine on `PATH`, so an install is only complete when both binaries sit in one directory; the release archives and the Docker image lay them out that way. The runtime stack is therefore **Rust, React, TypeScript, Go** — there is no Python anywhere, and we chose it to stay dependency-free at runtime and cross-platform without a Python/SciPy stack.

The design goal for footprint is qualitative: a small **idle** footprint, in the tens of megabytes while idle. That is explicitly **not a strict cap** — active jobs decode and analyse media and may exceed it. What this design rejects is background software idling at hundreds of megabytes while doing nothing.

Status: accepted

Supersedes: the original ADR-0001 wording, which described "a single Go binary" and justified the stack with a bare "~20–40 MB RAM" figure. See issue #17.
