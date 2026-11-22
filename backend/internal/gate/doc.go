// Package gate implements the Tdarr Gate (PLAN.md M2-5, ADR-0005) and the
// rule-based export-policy split.
//
// The gate decides whether a target video is eligible for sync/mux by looking
// for the configured processed-marker tag ("[ArcA310]" by default) on the
// target. Donor ingestion is never gated; only targets are. The possible
// states are the glossary's eligible / waiting_for_tags / missing_target.
//
// Tag discovery has two sources:
//
//   - the target filename, which ADR-0005 makes the authoritative marker
//     location, and
//   - the Matroska container's global tags, which are read with mkvtoolnix.
//     `mkvmerge -J` identifies the file but only reports how many global tags
//     exist (global_tags: [{"num_entries": N}]) — it does not expose tag names
//     or values — so tag content is extracted with the companion `mkvextract
//     tags` (same mkvtoolnix package, same host/Docker image).
//
// Classification is a pure function of the target filename (Rule.Classify):
// ADR-0005 makes the filename the only source of eligibility, so container tags
// are read for informational/display purposes only and never broaden the gate.
// Service binds the import-queue store to persist the outcome per candidate.
//
// The export-policy split (SplitExportTracks) is likewise pure: it partitions
// probed audio tracks into the policy import set (dub + original + reference
// languages) and the beyond-policy extras that the user must opt into
// (Q14b). Commentary and untagged (und) tracks are never policy-selected.
//
// Force tag override is intentionally absent in M2: settings already default
// settings.security.allow_force_tag_override to false and M2 ships without the
// override path.
package gate
