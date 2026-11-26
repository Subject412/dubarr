// Package gate implements the Processing Gate (GLOSSARY.md; ADR-0005 and
// ADR-0009) and the rule-based export-policy split.
//
// The gate decides whether a target video is eligible for sync/mux. It has two
// sources: the processed-marker tag on the target filename (ADR-0005) and the
// optional arr criterion — Radarr/Sonarr tag labels and root-folder paths from
// arr_library_cache (ADR-0009). Source modes are filename (default), arr and
// both; active sources are AND-ed and a target with no arr library row fails
// closed while the arr source is active. Donor ingestion is never gated; only
// targets are. The possible states are the glossary's eligible /
// waiting_for_tags / missing_target.
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
// Container tags are read for informational/display purposes only and never
// decide eligibility; the arr criterion comes from the arr poll cache, not from
// the container. Service binds the import-queue store to persist the outcome
// per candidate and evaluates the filename source only (donor ingestion). Its
// rule is re-read from settings on every classification, so an operator editing
// target.tag_filter changes the next verdict without a restart.
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
