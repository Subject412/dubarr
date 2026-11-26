# Publish by validated temp, then atomic rename

Replacing a target writes into the media library, so a crash at the wrong moment
could leave a half-written film. The safety mechanism is **where the output is
written and what happens before it is swapped in** — not a backup copy.

## The protocol

1. The target is copied into `WORK_DIR` and mkvmerge reads only that copy, so the
   live file is opened read-only and never while it is being replaced.
2. mkvmerge writes its output to a hidden, uniquely named temp **in the target's
   own directory** — `<target>.mkv.dubarr-<rand>.tmp`. Writing beside the target
   rather than in `WORK_DIR` matters: the temp is on the same filesystem as the
   file it will replace, so the final step can be an atomic rename even when
   `WORK_DIR` lives on a different pool or SSD.
3. The temp is validated with `ffprobe`: the probe must succeed and exactly one
   audio track must have been appended. A failure removes the temp and leaves the
   original untouched.
4. The validated temp is renamed over the target. Same filesystem, so the swap is
   atomic: a reader sees either the old file or the new one.
5. A pre-existing `<target>.bak` is **left exactly as it is**. dubarr neither
   writes nor deletes a sidecar: an earlier version copied the previous file
   there, and deleting those leftovers now would destroy a file the operator may
   still be relying on.

## Why `.bak` is not the safety net

An earlier version of VISION.md described the `.bak` as what made replacement
safe. That is wrong, and it matters: a `.bak` only helps if someone notices the
new file is bad *after* the fact. Validation before the rename prevents the bad
file from ever becoming the target.

The `.bak` was a convenience copy, written only when configured, and overwritten
by design on each publish — the previous good copy, not a history. With automatic
re-application (ADR-0017) it would have been written on every arr upgrade, and it
lives in the media library beside the film, where it invites exactly the kind of
cleanup that deletes the wrong thing. The option has since been removed
completely: dubarr writes no sidecar, and deliberately does not delete existing
ones.

### Where it would live, and why that is a problem

`<target>.mkv.bak` sits **in the media library, beside the film**. That breaks the
rule the rest of the setup depends on: the library folder belongs to Radarr and
Sonarr, and nothing else writes files there. arr ignores unknown extensions when
scanning, so it will not import the `.bak`, but it is still a stray file in a
directory dubarr does not own — and the surrounding pipeline (a transcoder
watching the library, a cleaner that removes "trash" files) has every right to
delete it, making it a backup that quietly disappears.

Neither Radarr nor Sonarr keeps a backup of a replaced file; they move the old
one to their own recycle bin, or drop it. **`.bak` has been removed entirely.**
Validation before the rename is the safety mechanism, the Audio Vault already
holds the audio, and a copy beside the target is storage cost in a directory
dubarr does not own. Existing `.bak` files are left untouched: they were written
by a version that promised to keep them, and deleting them now would be dubarr
destroying a file the operator may still rely on.

## Staleness check before publish

A mux takes minutes, and the media manager does not wait for it. If arr replaces
the target mid-mux — an upgrade lands while dubarr is working — the file on disk
at publish time is **not** the file that was copied into `WORK_DIR`. Renaming the
temp over it would overwrite a brand-new release with a mux built from the old
one: the upgrade would be destroyed, and it would look like a successful mux.

So the target's identity is re-read immediately before the rename — size, mtime
and inode, compared against what was captured when the copy was taken. If it
moved, the temp is discarded, nothing is published, and the target is re-queued
against the new file. The window between the check and the rename is
unavoidable; it is microseconds, where the mux itself is minutes.

## Crash safety

Every failure removes only the temp files that call created and leaves the
original in place. A crash or power loss mid-write leaves the original plus a
`.tmp` file — never a truncated target. Leftover temps are identifiable by the
`.dubarr-<rand>.tmp` suffix and can be swept.

## Consequences

- Publishing costs a full extra copy of the muxed file on the target's filesystem,
  transiently. That is the price of atomicity across pools, and it is paid once
  per mux.
- The muxed temp must never be written to a directory the media manager scans for
  new media, or arr will import a partial file. The hidden `.dubarr-*.tmp` name
  keeps it out of the media-extension set.

Status: accepted and fully implemented. Steps 1-4 are in `internal/mux` (temp
beside the target, `ffprobe` validation, atomic rename, cleanup); the staleness
check now compares the target's identity (size, mtime, inode) immediately before
the rename and refuses to publish over a file that changed mid-mux; and the
`.bak` option has been deleted outright, so no sidecar is written and none is
removed.
