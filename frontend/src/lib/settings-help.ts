import type { SettingsSectionKey } from './settings-sections'

/**
 * Static help copy for the Settings page, keyed by settings section and field.
 * Definitions come from GLOSSARY.md / IDEA.md / the ADRs; short examples stay
 * inline in the form (placeholders, ranges). This module is presentation only —
 * help strings are never part of the settings payload or stored settings.
 */
export const SETTINGS_HELP: Record<SettingsSectionKey, Record<string, string>> = {
  'storage.paths': {
    watch_dir:
      'Watch Folder: the dropzone scanned for new donors. A file is only picked up once its size is stable across two checks.',
    vault_dir:
      'Audio Vault: the write-once archive of extracted tracks, kept outside the video library so replacing a video never deletes its audio.',
    work_dir:
      'Work Area: isolated scratch space. Every job copies vault tracks here and verifies their checksums before processing.',
    output_dir:
      'Destination for the "write to the mux output folder" mode: the muxed file lands here as <name>.dubarr.mkv and the library file is left untouched. Must not be the Audio Vault or inside it.',
    media_roots:
      'Library folders Dubarr is allowed to read Targets from. Paths are canonicalized and confined to these roots.',
  },
  'languages.policy': {
    target_import_languages:
      'Target Language codes (two-letter ISO-639-1, e.g. pl): the dub/lektor tracks the Export Policy pre-selects from a donor.',
    extra_reference_languages:
      'Reference Language candidates tried after the Original Language. Use org for the arr-reported original or all for every shared language.',
    export_mode:
      'Rule based keeps the tracks the Export Policy selects; All tracks keeps every donor track, with beyond-policy tracks still needing explicit approval.',
    original_language_from_arr:
      'Use Radarr/Sonarr metadata to identify the Original Language, which becomes the first-choice Reference Language.',
    prefer_original_language:
      'Make the arr-reported Original Language the first-choice Reference Language unless it is a Target Language (the dub being synchronized).',
    exclude_commentary:
      'Commentary tracks never become the Reference Track or the Target track.',
  },
  'target.tag_filter': {
    required_filename_tags:
      'Tags in the Target filename that make it eligible. The match mode decides how many of them must be present.',
    excluded_filename_tags:
      'Tags that always make a Target ineligible, whatever else matches.',
    tag_match_mode:
      'ANY: at least one required tag; ALL: every required tag; REGEX: each entry is treated as a regular expression.',
    tag_case_sensitive: 'Match tags with exact letter case; off ignores case.',
    enforce_tag_filter:
      'Reject Targets without a required tag. Donor ingestion into the Audio Vault is never gated.',
  },
  'security.options': {
    vault_read_only:
      'Keep the mounted Audio Vault read-only (0444): the write-once archive is never modified, only verified copies in the Work Area.',
    allow_force_tag_override:
      'Let an operator bypass the destination tag gate for a single item. Off by default.',
    require_av1_warning: 'Show the AV1 hand-off warning before muxing into an AV1 Target.',
    require_av1_strict:
      'Treat the AV1 hand-off warning as a hard stop instead of an acknowledgement.',
  },
  'sync.defaults': {
    max_offset_sec:
      'Search bound for the automatic Sync Offset search; offsets outside ± this value are rejected.',
    checkpoint_window_sec: 'Length of each analysis window centred on a Checkpoint.',
    preview_window_sec: 'Length of each Preview Slice generated for Sync Studio.',
    checkpoint_positions_pct:
      'Timeline positions where donor and reference are cross-correlated; several Checkpoints reveal a constant offset, drift or mid-file cuts.',
    preview_positions_pct: 'Timeline positions where Preview Slices start.',
  },
  'mux.options': {
    keep_bak:
      'Keep a .bak copy of the replaced Target. Off replaces the original atomically with no backup. Ignored when the output mode writes to the mux output folder, because nothing is replaced.',
    track_title_in_mkv: 'Track title written into the Matroska container for the muxed audio.',
    output_mode:
      'Replace: atomically overwrite the library file (optional .bak). Write to the mux output folder: save <name>.dubarr.mkv in the Mux output folder instead, refuse to overwrite an existing file and skip the *arr rescan.',
  },
  'watch.options': {
    scan_interval_sec: 'How often the Watch Folder is scanned for new donors.',
    stability_check_interval_sec:
      'Minimum time between the two size checks that mark a file as stable.',
  },
  'imports.options': {
    donor_patterns:
      'Sibling filenames that mark a dropped file as a donor dub. Without * a pattern is a substring match; with * it is a glob.',
  },
  'arr.connections': {
    radarr_base_url:
      'Root URL of the Radarr instance including the scheme, e.g. http://192.168.1.10:7878. A missing http:// or https:// is rejected, never guessed.',
    radarr_api_key:
      'Radarr API key (Settings → General in Radarr). It is stored encrypted server-side; leave the field blank to keep the saved key.',
    radarr_enabled:
      'Only enabled connections are used for library sync and original-language lookups.',
    sonarr_base_url:
      'Root URL of the Sonarr instance including the scheme, e.g. http://192.168.1.10:8989. A missing http:// or https:// is rejected, never guessed.',
    sonarr_api_key:
      'Sonarr API key (Settings → General in Sonarr). It is stored encrypted server-side; leave the field blank to keep the saved key.',
    sonarr_enabled:
      'Only enabled connections are used for library sync and original-language lookups.',
  },
  'arr.options': {
    poll_interval_sec: 'How often the Radarr/Sonarr libraries are refreshed.',
    manual_sync: 'Allow triggering an arr library sync from the UI as well as on the poll timer.',
  },
}

/** Help text for one settings field, or `undefined` when none is authored. */
export function settingsHelp(section: SettingsSectionKey, field: string): string | undefined {
  return SETTINGS_HELP[section][field]
}
