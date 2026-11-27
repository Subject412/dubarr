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
      'Audio Vault: the library Dubarr owns — the write-once archive of extracted tracks, kept outside the video library so replacing a video never deletes its audio.',
    work_dir:
      'Temp folder: the workspace a job runs in. It holds copies only while the job is running and is cleared afterwards. On the same pool as your media the finished file moves into place as an atomic rename; on a faster separate pool the job runs quicker, but publishing has to copy the file across filesystems.',
    output_dir:
      'Destination for the "write to the mux output folder" mode: the muxed file lands here as <name>.dubarr.mkv and the library file is left untouched. It is used only when Output mode is separate. Must not be the Audio Vault or inside it.',
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
      'Where tags come from. Only tags in the Target filename are applied today, so this is fixed to Filename tags. A filter previously stored with the Radarr/Sonarr sources (arr or both) is shown read-only and its stored *arr criteria are kept, but they do not filter anything yet.',
    required_filename_tags:
      'Tags in the Target filename that make it eligible. The match mode decides how many of them must be present.',
    excluded_filename_tags:
      'Tags that always make a Target ineligible, whatever else matches.',
    tag_match_mode:
      'ANY: at least one required tag; ALL: every required tag; REGEX: each entry is treated as a regular expression. Applies to the required filename tags.',
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
    parallel_checkpoints:
      'How many checkpoints one sync job analyses at the same time. 1 uses one core and is the safest choice on a laptop; 4 suits a server, and a 4 GB container gets 4 workers. Budget about 400 MB per extra worker; the app reduces the number automatically when memory is short. Only one sync job runs at a time.',
    analysis_source:
      'How the analysis gets its audio. Windows (the shipped default) decodes only the checkpoint windows and falls back to a full decode if a window looks wrong, which is several times faster; Full decodes both tracks completely, which is slower but never falls back.',
  },
  'sync.strategy': {
    preset:
      'Your starting point; you can edit the steps afterwards. Balanced matches the previous release: one waveform pass on every checkpoint. Fast spends three checkpoints instead of six with the same method, so a job finishes sooner but says less about cuts and drift, and it also sets the checkpoint positions in Sync defaults. Maximum accuracy adds a second, different method when the first is unsure, at roughly double the analysis time on a difficult release. Any edit turns the starting point into Custom.',
    techniques:
      'The methods a job may try, in order. Each enabled step runs on every checkpoint the shipped way, and the first confident answer decides that point, so one weak checkpoint is retried on its own and the whole job is never re-run. A second step costs a second analysis pass whenever its condition makes it run.',
    technique_id:
      'Waveform compares the actual sound samples: exact to the millisecond on clean material, but it struggles when the dub was re-encoded or broadcast-processed, so use it first. MFCC compares the shape of the sound instead, which survives heavy compression and clipping but is only accurate to about ±10 ms, so it is best used as a second opinion rather than the first step.',
    when:
      'Always runs the step on every checkpoint. Only when the previous result is weak runs it as a second opinion, which costs nothing on clean material. Only when the offsets disagree runs it on the points that break the straight line through the rest, which is the cheapest way to rescue one stubborn checkpoint after a mid-film edit or a bad decode.',
    enabled:
      'A disabled step stays in the list but never runs and costs nothing; the first step must stay enabled, because it is the one that always answers.',
    min_confidence:
      'How good a match must be before this step is believed. 0.10 is the shipped floor: lower accepts more answers, including wrong ones, and higher sends more checkpoints to the next step, or to review when this is the last one.',
    min_match_z:
      'How far the match has to stand out from the rest of the curve, in standard deviations: 5 is the point where a human should look at the result and 10 is almost certainly right. Raising it rejects doubtful matches at the cost of more checkpoints falling through to the next step; lower it only when a difficult release keeps coming back as unreliable.',
  },
  'mux.options': {
    track_title_in_mkv: 'Track title written into the Matroska container for the muxed audio.',
    output_mode:
      'Replace: atomically overwrite the library file. Write to the mux output folder: save <name>.dubarr.mkv in the Mux output folder instead, refuse to overwrite an existing file and skip the *arr rescan.',
  },
  'audio.options': {
    rules:
      'Ordered, opt-in Transcode Rules. The first enabled rule whose conditions all match the PRE-TEMPO vault track is applied to a copy in the temp folder; with no rules the audio is copied losslessly and nothing is re-encoded.',
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
      'Output format. keep copies the track untouched; opus, aac, flac and PCM re-encode it in the temp folder only — the Audio Vault file is never modified.',
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
  },
}

/** Help text for one settings field, or `undefined` when none is authored. */
export function settingsHelp(section: SettingsSectionKey, field: string): string | undefined {
  return SETTINGS_HELP[section][field]
}
