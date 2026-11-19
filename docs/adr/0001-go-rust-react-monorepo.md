# Go backend, Rust DSP CLI, React frontend — no Python

Dubarr is a single Go binary (embedded React SPA, SQLite) that shells out to a small Rust CLI (dubarr-sync) for FFT cross-correlation and to ffmpeg/mkvmerge for media work. We chose this over a Python DSP service to keep the appliance lightweight (~20–40 MB RAM), dependency-free at runtime, and cross-platform without a Python/SciPy stack.

Status: accepted
