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
  SyncStrategy,
  TargetTagFilter,
  VaultNaming,
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
    parallel_checkpoints: 1,
    // Compiled default (settings.DefaultAnalysisSource): analyse 190 s windows
    // with a 2.0 s pre-roll, falling back to a whole-track decode when a window
    // plan cannot be built. Mirrors the backend constant and the select's own
    // fallback in Settings.tsx.
    analysis_source: 'windows',
  },
  'security.options': {
    allow_force_tag_override: false,
  },
  // The shipped strategy (#82, D4): one waveform step on every checkpoint,
  // which is settings.Preset("balanced") — the same value the server stores on
  // a fresh install. Fast shares this list and differs only in how many
  // checkpoints it measures (D4a).
  //
  // This literal stays written out on purpose. TestDefaultStrategyMatchesTheSPADefault
  // (backend/internal/settings) brace-scans *this file* and decodes the literal
  // to prove it equals the Go default, so it cannot be replaced by a reference to
  // ./strategy.ts — the shared tables the editors use live there.
  'sync.strategy': {
    preset: 'balanced',
    techniques: [
      {
        id: 'waveform',
        enabled: true,
        when: 'always',
        min_confidence: 0.1,
        min_match_z: 5,
        z_gate: true,
      },
    ],
    // The segmentation block (#84) at analyze.DefaultPiecewisePolicy(): off, so
    // a fresh install samples six points and produces the 0.4.0 report (D5).
    // The Go SPA-parity test decodes this literal, so every number here is
    // pinned against the server default and no preset turns it on.
    piecewise: {
      enabled: false,
      step_sec: 45,
      window_sec: 60,
      split_penalty_ms: 250,
      min_segment_sec: 120,
      max_segments: 12,
    },
  } as SyncStrategy,
  'mux.options': {
    output_mode: 'replace',
  },
  // Shipped default: no rules, no transcoding (ADR-0010).
  'audio.options': { rules: [] } as AudioOptions,
  'watch.options': { scan_interval_sec: 10, stability_check_interval_sec: 4 },
  'arr.options': { poll_interval_sec: 300 },
  'imports.options': { donor_patterns: ['PL.DUB'] },
  // The shipped naming patterns (#47, `settings.Default*`). Nothing is renamed
  // until an operator previews and applies a plan on the Vault page.
  'vault.naming': {
    movie_template: '{Title} ({Year}) [{Variant} {Codec} {Channels}]',
    episode_template:
      '{Title} - S{Season:00}E{Episode:00} - {Episode Title} [{Variant} {Codec} {Channels}]',
    season_folder: 'Season {Season:00}',
  },
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
  SyncStrategy,
  TargetTagFilter,
  VaultNaming,
  WatchOptions,
}
