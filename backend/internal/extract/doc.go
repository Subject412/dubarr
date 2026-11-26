// Package extract implements the M3-1 extraction worker: it consumes
// approval-queue rows that the user approved (status 'extracting') and
// losslessly extracts the policy-selected donor audio tracks into a per-job
// staging directory under WORK_DIR for the vault pipeline (M3-2).
//
// # Status model
//
// M3-1 is additive and migration-free, so import_queue's status CHECK is the
// authority: only scanning, pending_approval, extracting, completed, ignored
// and failed are legal. The worker therefore uses this subset:
//
//	extracting  approved by the user, awaiting/under extraction (claim is a
//	            conditional UPDATE that only succeeds while the row is still
//	            extracting; a crash leaves the row here and the next boot
//	            reprocesses it after reaping its staging directory)
//	completed   extraction succeeded and the validated output is staged under
//	            WORK_DIR/extract/<id>/; M3-2 advances the row to vaulted
//	failed      extraction failed; error_message carries the reason and the
//	            staging directory has been removed
//
// A distinct 'extracted' or 'processing' status would need a migration and is
// deliberately not introduced.
//
// # Staging layout
//
// Output goes to {WORK_DIR}/extract/{import_id}/, never into
// the Vault. WORK_DIR is storage.paths.work_dir; when unset it falls back to
// DATA_DIR/work. On any failure the job directory is wiped; on success it is
// left in place for the vault pipeline unless a VaultHook is configured, in
// which case the hook owns the staged files and the row only completes when it
// returns nil.
//
// # Track selection and validation
//
// Tracks are chosen with gate.SplitExportTracks over probe output, using
// languages.policy plus the arr original language of the matched library row
// (export_mode "all" additionally keeps extra tracks that carry a real
// language). mkvmerge's audio-only ids are paired with the probed audio tracks
// positionally — never through ffprobe's container-wide stream index, which a
// video stream shifts. Each extracted file is re-probed: codec and channel
// count must match the donor track and the duration must stay within
// DurationTolerance of that track's own donor duration (mkvmerge's per-track
// duration, falling back to the container duration when the file carries none).
// The worker is serialized (concurrency 1) and reaps staging directories left
// behind by a previous boot at startup.
package extract
