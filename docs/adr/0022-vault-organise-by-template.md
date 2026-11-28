# Organise the vault by template; never move a file on a setting change

The Audio Vault was organised by whatever the source file happened to be called.
For an arr-style media manager that is the wrong default: the vault is the one
library Dubarr owns, so its names should describe the media, not the release it
came from.

## Templates, not code

Film, episode and season-folder patterns are operator-editable text with a small
closed token set (`{Title}`, `{Year}`, `{Season:00}`, `{Episode:00}`,
`{Episode Title}`, `{Variant}`, `{Codec}`, `{Channels}`, `{TmdbId}`, `{TvdbId}`,
`{ImdbId}`). Only `{Season}` and `{Episode}` take a zero-padding modifier,
because it is the only modifier that has a meaning here — an id token refuses
one rather than pad an identifier. Rendering substitutes, drops the separator
run before an empty token, removes empty groups, and cleans the result; an id
the mirror does not hold renders empty and takes its group with it, so
`{Title} ({TmdbId}) [{Year}]` becomes `Title [2024]`, never `Title (0) [2024]`; a stem that would
exceed 200 bytes, end in `.mka`, or collide with a reserved name is refused at
validation time, naming the field that caused it. `{Language}` is deliberately
not a token: the language is what the file *is*, and it belongs in the `.iso2.mka`
suffix, never inside the name.

## Setting a template moves nothing

This is the decision. A template change is inert; nothing inside the vault
changes until an operator previews a plan and applies it. Automatic reorganisation
on a settings save would be a rename cascade triggered by a form field.

## The plan is the gate

Preview writes nothing and returns a row per move plus conflicts. **A plan with
any conflict has no id**, so it cannot be submitted — the refusal is structural,
not a check the caller could forget.

Apply re-plans and compares the `plan_id` — a sha256 over the sorted
`(track_id, source, destination, size, mtime_ns)` tuples — so a file that changed
after the preview yields `stale_plan` instead of a move to the wrong place.

Preflight runs before anything is created or renamed: every source must be a
regular non-symlink, no two tracks may share a destination, no destination may be
another track's source (a swap is a `path_cycle`), the destination must not exist,
the resolved destination must stay inside the resolved vault root (which covers a
symlinked `VAULT_DIR` and a symlink inside it), and everything must share a
`st_dev`. Any failure is a 409 with the database untouched.

## Three phases, and an honest last resort

1. Create destination parents, recording only the directories this request
   created.
2. Rename in plan order; on the first error, rename back **in reverse** and remove
   only the directories this request created, deepest first, while empty.
3. One `BEGIN IMMEDIATE` transaction of guarded
   `UPDATE … WHERE id = ? AND file_path = <source>` plus one audit row per track
   and the run row. A zero-row update means the row moved under us: roll back,
   then undo phase 2 in reverse.

If a rename-back itself fails, the filesystem and the database genuinely
disagree. The only honest move is to reconcile: stat destination then source,
rewrite `file_path` to whichever exists, and write a `needs_attention` run that
lists the paths an operator must inspect. Scoring that as success would be
lying; leaving it unreported would be worse.

## No file is ever deleted or rewritten

Moves only. The only `os.Remove` calls target directories this request created,
deepest first, and only while empty.

## Consequences

- A vault organise can be previewed with no risk and abandoned at any point
  before apply.
- Operators on one filesystem can organise; a vault split across devices gets a
  clear refusal rather than a slow copy nobody asked for.
- The audit tables (migration 0010) are the record of what actually happened,
  which is what makes `needs_attention` meaningful rather than a log line.
