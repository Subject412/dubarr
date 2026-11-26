# Linux only; Windows support dropped

ADR-0006 kept the code Windows-ready and deferred Windows builds and CI. In
practice the release workflow still published a `windows_amd64` archive while the
documentation called Windows deferred and untested — shipping a binary the docs
disown is worse than not shipping one.

Windows support is now **dropped**, not deferred. The release workflow no longer
builds or publishes a Windows archive, and no document describes Windows as
supported, planned or upcoming.

## What this does not mean

The code stays portable where portability costs nothing — path handling,
signal handling, the keyring fallback — because that portability is also what
makes Docker and arm64 work. It is not a promise, and nothing is tested on
Windows.

## Rationale

The project is built for one operator's Linux host and published for others to
adapt. A second platform multiplies the verification surface for a use case
nobody has asked for, and an unverified build that users can download is a
liability rather than a feature.

Status: accepted. Supersedes ADR-0006.
