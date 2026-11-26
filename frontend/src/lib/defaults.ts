import type {
  ArrOptions,
  AudioOptions,
  DubarrSettings,
  ImportsOptions,
  LanguagesPolicy,
  MuxOptions,
  SecurityOptions,
  StoragePaths,
  SyncDefaults,
  TargetTagFilter,
  WatchOptions,
} from './api'

/**
 * Seeded first-run defaults (mirrors backend internal/settings Defaults).
 * The wizard and settings editors fall back to these while data loads so no
 * form ever renders an empty, unsaveable object.
 */
export const DEFAULT_SETTINGS: DubarrSettings = {
  'storage.paths': { watch_dir: '', vault_dir: '', work_dir: '', output_dir: '', media_roots: [] },
  'languages.policy': {
    target_import_languages: ['pl'],
    extra_reference_languages: ['en'],
    original_language_from_arr: true,
    prefer_original_language: true,
    exclude_commentary: true,
    export_mode: 'rule_based',
  },
  'target.tag_filter': {
    required_filename_tags: [],
    tag_match_mode: 'ANY',
    tag_case_sensitive: false,
    excluded_filename_tags: [],
    enforce_tag_filter: true,
    source_mode: 'filename',
    required_arr_tags: [],
    excluded_arr_tags: [],
    root_folder_paths: [],
  },
  'sync.defaults': {
    max_offset_sec: 180,
    checkpoint_positions_pct: [10, 25, 40, 55, 70, 85],
    checkpoint_window_sec: 150,
    preview_positions_pct: [10, 25, 50, 75, 90],
    preview_window_sec: 60,
  },
  'security.options': {
    allow_force_tag_override: false,
  },
  'mux.options': {
    keep_bak: true,
    track_title_in_mkv: 'Polski Dubbing',
    output_mode: 'replace',
  },
  // Shipped default: no rules, no transcoding (ADR-0010).
  'audio.options': { rules: [] } as AudioOptions,
  'watch.options': { scan_interval_sec: 10, stability_check_interval_sec: 4 },
  'arr.options': { poll_interval_sec: 300 },
  'imports.options': { donor_patterns: ['PL.DUB'] },
}

export type {
  ArrOptions,
  AudioOptions,
  ImportsOptions,
  LanguagesPolicy,
  MuxOptions,
  SecurityOptions,
  StoragePaths,
  SyncDefaults,
  TargetTagFilter,
  WatchOptions,
}
