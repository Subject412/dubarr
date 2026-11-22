// Package vault owns the Audio Vault write path: the write-once archive of
// losslessly extracted .mka tracks described by ADR-0002.
//
// M3-2 implements the write pipeline that consumes a staged extraction from
// WORK_DIR/extract/<import_id>/ (produced by package extract) and publishes it
// into VAULT_DIR:
//
//  1. canonicalize and validate the staged file (regular file, confined to the
//     extraction root);
//  2. stream its bytes into a temp file inside the vault directory while
//     computing SHA-256 of exactly the bytes being archived, then fsync it;
//  3. atomically rename the temp file to <base>.<iso2>.mka (ISO 639-1 suffix,
//     per GLOSSARY "Target Language" and IDEA.md §3.3);
//  4. fsync the vault directory so the rename is durable;
//  5. chmod the published file 0444 (ADR-0002 immutability);
//  6. record the track in audio_tracks (and its vault_media parent);
//  7. remove the staged file and, when empty, its import directory.
//
// # Write-once
//
// An existing destination is never overwritten: the pipeline refuses with
// ErrCollision (which also matches fs.ErrExist) before creating any temp file.
// The check and the rename run under the writer's mutex, so a single process
// cannot race itself. The extraction worker is serialized (concurrency 1), so
// vault writes are serialized too; a second process writing the same vault is
// outside the M3-2 scope and remains a documented TOCTOU window. The rename
// itself is a plain os.Rename because no portable RENAME_NOREPLACE primitive
// exists in the standard library and the repository targets Windows as well.
//
// # Directory permissions
//
// VAULT_DIR is created 0755 when missing and an existing directory's mode is
// never changed. It is deliberately NOT set to 0555: a read-only directory
// would block the next atomic rename (and the temp file it needs) with no
// unlock step, which contradicts a pipeline that keeps ingesting new donors.
// Immutability is enforced per published file instead: mode 0444 plus the
// collision refusal above. Later milestones that need to archive again write a
// new file rather than touching an existing one.
//
// A failure at any point before the rename removes the temp file and leaves the
// vault exactly as it was, with the staged file still present. A failure after
// the rename (directory fsync, chmod, database record) is reported with the
// final path and SHA-256 in the error: the vault file is already durable and is
// never deleted from under the caller.
package vault
