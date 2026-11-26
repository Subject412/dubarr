import type { SettingsSectionKey } from './settings-sections'

/**
 * Static help copy for the Settings page, keyed by settings section and field.
 * Definitions come from GLOSSARY.md and the ADRs; short examples stay
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
    source_mode:
      'Where tags come from: tags in the Target filename, tags on the Radarr/Sonarr library item, or both. When both are used a file must satisfy each selected source, so adding a source can only narrow what is processed.',
    required_filename_tags:
      'Tags in the Target filename that make it eligible. The match mode decides how many of them must be present.',
    excluded_filename_tags:
      'Tags that always make a Target ineligible, whatever else matches.',
    required_arr_tags:
      'Radarr/Sonarr tag labels that make a library item eligible. Labels are compared case-insensitively unless case-sensitive matching is on; the match mode decides how many must be present.',
    excluded_arr_tags:
      'Radarr/Sonarr tag labels that always make a Target ineligible, whatever else matches.',
    root_folder_paths:
      'Allowlist of Radarr/Sonarr root folders. When set, only Targets inside one of these folders are processed. Leave empty to accept every root folder.',
    tag_match_mode:
      'ANY: at least one required tag; ALL: every required tag; REGEX: each entry is treated as a regular expression. Applies to filename tags and Radarr/Sonarr tags alike.',
    tag_case_sensitive: 'Match tags with exact letter case; off ignores case.',
    enforce_tag_filter:
      'Reject Targets that do not satisfy the filter. Donor ingestion into the Audio Vault is never gated.',
  },
  'security.options': {
    allow_force_tag_override:
      'Let an operator bypass the processing filter for a single item. Off by default.',
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
    track_title_in_mkv: 'Track title written into the Matroska container for the muxed audio.',
    output_mode:
      'Replace: atomically overwrite the library file. Write to the mux output folder: save <name>.dubarr.mkv in the Mux output folder instead, refuse to overwrite an existing file and skip the *arr rescan.',
  },
  'audio.options': {
    rules:
      'Ordered, opt-in Transcode Rules. The first enabled rule whose conditions all match the PRE-TEMPO vault track is applied to a copy in the Work Area; with no rules the audio is copied losslessly and nothing is re-encoded.',
    when_source_codecs:
      'Source codec names (ffmpeg codec_name values such as pcm_s16le or ac3). The track matches if its codec appears anywhere in this list; entries are compared case-insensitively.',
    when_source_codec_regex:
      'Optional regular expression matched against the source codec name, for example ^pcm_.*$ to catch every PCM flavour.',
    when_min_bitrate_bps:
      'Only match tracks whose archived bitrate is at least this many bits per second.',
    when_max_bitrate_bps:
      'Only match tracks whose archived bitrate is at most this many bits per second.',
    when_channel_count: 'Only match tracks with exactly this channel count.',
    when_channel_layout:
      'Only match tracks with this ffmpeg channel layout (stereo, 5.1, 7.1, ...). Layout names are validated so a typo is rejected on save.',
    when_sample_rate:
      'Only match tracks whose archived sample rate is exactly this value in Hz.',
    when_lang_iso2: 'Only match tracks tagged with this two-letter language code, e.g. pl.',
    when_variant:
      'Only match tracks with this variant label, e.g. dubbing or commentary.',
    then_codec:
      'Output format. keep copies the track untouched; opus, aac, flac and PCM re-encode it in the Work Area only — the Audio Vault file is never modified.',
    then_bitrate_bps:
      'Target bitrate for Opus or AAC. Lossless formats (FLAC/PCM) ignore this field.',
    then_channels: 'Downmix/upmix to this channel count; a matching layout is chosen automatically.',
    then_channel_layout:
      'Explicit ffmpeg output layout (stereo, 5.1, ...). Takes precedence over the channel count when both are set.',
    then_sample_rate: 'Output sample rate in Hz.',
    enabled:
      'Disabled rules stay in the list but are never matched. Rules are evaluated top to bottom; move a rule up to give it priority.',
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
