// Same-origin API client. The SPA is served by the Dubarr binary, so every
// request is relative to '/api' and rides the dubarr_session cookie.
// No secrets ever come from frontend configuration.

export type AuthStatus = {
  initialized: boolean
  authenticated: boolean
  keyring_available: boolean
}

export type User = {
  id: number
  username: string
  is_admin: boolean
}

export type BrowseEntry = {
  name: string
  path: string
  is_dir: boolean
}

export type BrowseResponse = {
  path: string
  parent: string | null
  entries: BrowseEntry[]
}

export type ToolHealth = {
  available: boolean
  version?: string
  path?: string
}

export type Health = {
  status: 'ok' | 'degraded'
  version: string
  db_ok: boolean
  tools: Record<string, ToolHealth>
  keyring_available: boolean
  secrets_initialized: boolean
  auth_initialized: boolean
}

export type StoragePaths = {
  watch_dir: string
  vault_dir: string
  work_dir: string
  /** Destination root for mux output_mode=separate (ADR-0011). */
  output_dir: string
  media_roots: string[]
}

export type LanguagesPolicy = {
  target_import_languages: string[]
  extra_reference_languages: string[]
  original_language_from_arr: boolean
  prefer_original_language: boolean
  exclude_commentary: boolean
  export_mode: 'rule_based' | 'all'
}

/** Which sources the Processing Gate consults; active sources are AND-ed. */
export type TagSourceMode = 'filename' | 'arr' | 'both'

export type TargetTagFilter = {
  required_filename_tags: string[]
  tag_match_mode: 'ANY' | 'ALL' | 'REGEX'
  tag_case_sensitive: boolean
  excluded_filename_tags: string[]
  enforce_tag_filter: boolean
  source_mode: TagSourceMode
  /** Radarr/Sonarr tag labels; excluded labels always win. */
  required_arr_tags: string[]
  excluded_arr_tags: string[]
  /** Allowlist of canonical Radarr/Sonarr root-folder paths. */
  root_folder_paths: string[]
}

export type TranscodeCodec = 'keep' | 'opus' | 'aac' | 'flac' | 'pcm_s16le' | 'pcm_s24le' | 'pcm_f32le'

/**
 * One opt-in Audio Transcode Rule (ADR-0010). Conditions are AND-ed and match
 * the PRE-TEMPO vault-track metadata; the first enabled rule that matches wins.
 * An empty rule list keeps the historical lossless-copy behaviour.
 */
export type TranscodeRule = {
  enabled: boolean
  when: {
    /** Exact ffmpeg codec names, matched case-insensitively. */
    source_codecs?: string[]
    /** Optional RE2 pattern matched against the source codec name. */
    source_codec_regex?: string
    min_bitrate_bps?: number | null
    max_bitrate_bps?: number | null
    channel_count?: number | null
    /** Canonical ffmpeg layout name (stereo, 5.1, ...). */
    channel_layout?: string
    sample_rate?: number | null
    lang_iso2?: string
    variant?: string
  }
  then: {
    codec: TranscodeCodec
    bitrate_bps?: number | null
    channels?: number | null
    channel_layout?: string
    sample_rate?: number | null
  }
}

/** `audio.options`: ordered, global Transcode Rules. Global only, no per-media overrides. */
export type AudioOptions = { rules: TranscodeRule[] }

export type SyncDefaults = {
  max_offset_sec: number
  checkpoint_positions_pct: number[]
  checkpoint_window_sec: number
  preview_positions_pct: number[]
  preview_window_sec: number
  /** Absent in a row stored before U-DSP.8; the form falls back to 1. */
  parallel_checkpoints?: number
  /** Absent in a row stored before U-WIN.6; the form falls back to 'windows'. */
  analysis_source?: 'windows' | 'full'
}

/** A matcher the pipeline can run on a checkpoint (`analyze.TechniqueID`). */
export type SyncTechniqueID = 'waveform' | 'mfcc'

/**
 * Whether a step runs everywhere, reacts to a weak answer, or is sent in only
 * where the offsets break the trend (`analyze.When`).
 */
export type SyncWhen = 'always' | 'below_confidence' | 'above_drift'

/**
 * The name a strategy carries. It is only a label on the list: the server
 * derives it from `techniques` and stores `custom` whenever the list is not
 * that preset's canonical list, so the UI never has to trust its own label.
 */
export type SyncPreset = 'fast' | 'balanced' | 'maximum_accuracy' | 'custom'

/**
 * One step of `sync.strategy` (`analyze.Technique`). The two thresholds are
 * this step's acceptance gates: an answer is good enough when its correlation
 * quality is at least `min_confidence` and its peak stands at least
 * `min_match_z` standard deviations out (`0` means the method does not report
 * one, and is not treated as a failure).
 */
export type SyncTechnique = {
  id: SyncTechniqueID
  enabled: boolean
  when: SyncWhen
  min_confidence: number
  /** This entry's σ floor. Ignored unless `z_gate` is true; 0 when not gated. */
  min_match_z: number
  /**
   * Whether `min_match_z` gates acceptance (`analyze.Technique.ZGate`, new in
   * 0.5.0). The waveform matcher is gated by it; the mel-cepstral matcher is
   * not, because its z lives on a different curve — for it, quality decides and
   * the σ is recorded as a diagnostic. Absent in a list written before 0.5.0,
   * which reads as false: no band is applied to a number that is not there.
   */
  z_gate?: boolean
}

/** `sync.strategy`: the ordered technique list plus the preset it came from. */
export type SyncStrategy = {
  preset: SyncPreset
  techniques: SyncTechnique[]
  /**
   * The segmentation block (#84). Absent in a section written before 0.5.0,
   * which means the same as the documented default at `enabled: false`.
   */
  piecewise?: SyncPiecewisePolicy
}

export type SecurityOptions = {
  allow_force_tag_override: boolean
}

export type MuxOutputMode = 'replace' | 'separate'

export type MuxOptions = {
  /** replace: atomically overwrite the Target; separate: write to output_dir (ADR-0011). */
  output_mode: MuxOutputMode
}

export type WatchOptions = {
  scan_interval_sec: number
  stability_check_interval_sec: number
}

export type ArrOptions = {
  poll_interval_sec: number
}

/** Supported *arr applications; also the `{kind}` path segment on the API. */
export type ArrConnectionKind = 'radarr' | 'sonarr'

/**
 * One saved Radarr/Sonarr connection as returned by GET /api/settings/arr.
 * `api_key` is always the server mask (e.g. `********abcd`), never plaintext.
 */
export type ArrConnection = {
  kind: ArrConnectionKind
  base_url: string
  enabled: boolean
  api_key: string
}

export type ArrConnectionsResponse = {
  instances: ArrConnection[]
}

/** PUT /api/settings/arr/{kind} body. An omitted `api_key` keeps the stored key. */
export type ArrConnectionPayload = {
  base_url: string
  api_key?: string
  enabled: boolean
}

export type ArrConnectionTestResult = {
  ok: boolean
  kind: ArrConnectionKind
}

/**
 * POST /api/settings/arr/{kind}/test body: the draft values from the form.
 * An omitted or blank `api_key` tests with the stored key, and an omitted
 * `base_url` tests the saved connection. The test never persists anything.
 */
export type ArrConnectionTestPayload = {
  base_url?: string
  api_key?: string
}

/**
 * `vault.naming`: the patterns that decide the *stem* of an archived track
 * inside the folder Dubarr already owns (#47). They never carry the extension —
 * every vault file still ends in ".<language>.mka" — and nothing is renamed
 * until an operator previews and applies a plan on the Vault page.
 */
export type VaultNaming = {
  movie_template: string
  episode_template: string
  /** A single folder name rendered per episode; blank keeps episodes beside the show folder. */
  season_folder: string
}

export type DubarrSettings = {
  'storage.paths': StoragePaths
  'languages.policy': LanguagesPolicy
  'target.tag_filter': TargetTagFilter
  'sync.defaults': SyncDefaults
  'sync.strategy': SyncStrategy
  'security.options': SecurityOptions
  'mux.options': MuxOptions
  'audio.options': AudioOptions
  'watch.options': WatchOptions
  'arr.options': ArrOptions
  'vault.naming': VaultNaming
}

export type SettingsKey = keyof DubarrSettings

export type SettingsEnvelope = {
  settings: Partial<DubarrSettings> & Record<string, unknown>
}

export type SettingsExport = {
  exported_at: string
  settings: Record<string, unknown>
}

export type QueueEligibility = 'eligible' | 'waiting_for_tags' | 'missing_target'

/**
 * Tri-state match outcome: no arr target, matched but not actionable, or
 * approvable now (gate-eligible target plus a donor).
 */
export type QueueMatchState = 'unmatched' | 'matched_no_donor' | 'ready'

/**
 * Machine-readable cause behind `match_reason`, so the card never has to read
 * the human sentence. Mirrors the server's closed set: `none` is the ready row,
 * and a cause the server cannot prove arrives as `unknown`.
 */
export type QueueMatchBlocker =
  | 'none'
  | 'no_match'
  | 'no_target'
  | 'gate_tags'
  | 'gate_excluded'
  | 'mount'
  | 'no_donor'
  | 'unknown'

export type QueueDonorCandidate = {
  filename: string
  path: string
  file_size: number
  selected: boolean
}

export type QueueItem = {
  id: number
  source_file_path: string
  filename: string
  file_size: number
  status: string
  title: string
  year?: number
  media_type?: string
  season_number?: number
  episode_number?: number
  target_path: string
  original_language?: string
  audio_languages: string[]
  eligibility: QueueEligibility
  matched_tags: string[]
  missing_tags: string[]
  /**
   * Why no filename tag can ever satisfy the gate — the configured pattern does
   * not compile (#155). Empty/absent when every pattern compiled, so the card
   * only shows it when the wait cannot end by adding a tag.
   */
  gate_reason?: string
  donor_candidates: QueueDonorCandidate[]
  /** Whether the row carries an arr library identity; when false the operator must match it manually. */
  matched: boolean
  library_title: string
  /** Tri-state match outcome; `unmatched` exactly when `matched` is false. */
  match_state: QueueMatchState
  /** Short human sentence explaining `match_state`, shown on the card. */
  match_reason: string
  /**
   * Machine-readable blocker behind `match_reason`; the card branches on this
   * and never parses the prose. Absent on older payloads, in which case the
   * card falls back to a neutral hint rather than guessing a cause.
   */
  match_blocker?: QueueMatchBlocker
  /**
   * The donor markers the pairing searched for (blank entries dropped). The
   * backend reports its fixed list (U11), so an empty list only ever comes from
   * an older payload or a direct caller with none — a different next step from
   * "nothing in the folder matched a marker". Absent on older payloads, in
   * which case the card does not claim either cause.
   */
  donor_patterns?: string[]
}

/**
 * Why an empty approval queue is empty. Mirrors the server's `diagnostics`
 * object: no arr instance at all, configured but never synced, or synced and
 * failing. `last_pass_at` is the last SUCCESSFUL pass, so a failure leaves it
 * untouched. The counters are per-instance facts, so they only describe the
 * installation when they add up to every configured instance.
 */
export type QueueDiagnostics = {
  instances_configured: number
  never_synced: number
  sync_failed: number
  last_pass_at: string | null
  cache_items: number
  /**
   * False when the server could not read the sync health at all: the counts are
   * then zeroed and say nothing about the installation. Absent on older
   * payloads, which assert nothing either way.
   */
  sync_health_available?: boolean
  /**
   * Whether `security.options.allow_force_tag_override` is on: a global
   * setting, not a per-row fact. It changes no verdict any more (#48): an
   * ineligible pair is approvable with the switch either way. All it decides now
   * is whether a card offers the "start without waiting" checkbox, which makes
   * that one job skip the hold. Absent on older payloads.
   */
  force_tag_override?: boolean
}

export type QueueResponse = {
  items: QueueItem[]
  /** Absent on older payloads and in test fixtures; the page then says only that the queue is clear. */
  diagnostics?: QueueDiagnostics
}

/** One arr library row arr's parse matched, with a deterministic 0..1 score. */
export type QueueIdentifyCandidate = {
  library_cache_id: number
  title: string
  year: number
  media_type: string
  external_item_id: number
  confidence: number
  matched_by: string
}

/** What arr's own parser made of the release name. `parsed` is false when the
 * instance recognised no title at all — an unparsable release, which is not the
 * same thing as a library with no matching rows. `error` is set only when the
 * call itself failed, and candidates may still follow it. */
export type QueueIdentifyParse = {
  parsed: boolean
  title: string
  year: number
  quality: string
  confidence: number
  error?: string
}

export type QueueIdentifyResponse = {
  parse: QueueIdentifyParse
  candidates: QueueIdentifyCandidate[]
}

export type QueueMatchResponse = {
  match_state: QueueMatchState
}

export type LibrarySearchItem = {
  id: number
  title: string
  year: number
  media_type: 'movie' | 'episode'
  season_number?: number
  episode_number?: number
  original_language_iso2: string
}

export type LibrarySearchResponse = {
  items: LibrarySearchItem[]
}

// --- Library hierarchy (#56) -------------------------------------------------
//
// Radarr is one flat movie list; Sonarr is series -> seasons -> episodes, one
// read per level. Every field below is optional-tolerant where the server can
// legitimately omit it (an older row, a mirror column that is still null), so a
// tab renders a dash instead of inventing a value.

/** One Radarr movie (`GET /api/library/movies`). `id` is the cache row id. */
export type LibraryMovieResource = {
  id: number
  /** Radarr's own movieId — never the same thing as `id`. */
  arr_id?: number
  instance_id?: number
  instance?: string
  title: string
  original_title?: string
  year?: number
  /** The movie's original language, as the arr reports it (ISO 639-1 code). */
  original_language?: string
  /** The file's audio languages, arr-reported, in stream order. Never null. */
  audio_languages?: string[]
  /** arr's monitored flag. */
  monitored?: boolean
  /** True when the mirror holds a target path for the movie. */
  available?: boolean
  /** The movie folder. */
  path?: string
  target_file_path?: string
  has_file?: boolean
  /** 0 until the mirror records a size; the UI hides the column while it is 0. */
  size_on_disk?: number
  eligibility?: string
  video_codec?: string
  updated_at?: string
}

/** One aggregated Sonarr series (`GET /api/library/series`). */
export type LibrarySeriesResource = {
  id: number
  instance_id?: number
  instance?: string
  title: string
  year?: number
  /** The series' own language, as Sonarr reports it (ISO 639-1 code). */
  original_language?: string
  /** arr's monitored flag for the series. */
  monitored?: boolean
  /**
   * True when at least one episode of the series has a file in the mirror.
   * No `audio_languages` is served on this resource: audio tracks belong to one
   * file, and a series row aggregates its episodes, so the server omits the
   * field rather than invent one for the whole show.
   */
  available?: boolean
  path?: string
  season_count?: number
  episode_count?: number
  episode_file_count?: number
  /** The worst verdict across the series' episodes. */
  eligibility?: string
  updated_at?: string
}

/** One season's counts, computed by the server (`GROUP BY season_number`). */
export type LibrarySeasonStatistics = {
  episode_count?: number
  episode_file_count?: number
  percent_of_episodes?: number
}

export type LibrarySeasonResource = {
  season_number: number
  statistics?: LibrarySeasonStatistics
}

/** One episode row of one season (`.../seasons/{season}/episodes`). */
export type LibraryEpisodeResource = {
  id: number
  series_id?: number
  /** Sonarr's own episodeId — never the same thing as `id`. */
  arr_id?: number
  season_number?: number
  episode_number?: number
  title?: string
  has_file?: boolean
  target_file_path?: string
  eligibility?: string
}

/** The paging envelope the movie and series lists share. */
export type LibraryPagedResponse<T> = {
  page?: number
  /** Echoes the size actually applied after the server clamp. */
  pageSize?: number
  total?: number
  items?: T[]
}

export type LibrarySeasonsResponse = {
  items?: LibrarySeasonResource[]
}

export type LibrarySeasonEpisodesResponse = {
  seasonNumber?: number
  /** The whole season, so the page can say "showing 200 of 2001". */
  total?: number
  /** True when the season is longer than one payload can carry. */
  truncated?: boolean
  items?: LibraryEpisodeResource[]
}

/** Server-side query surface of the movie and series lists. */
export type LibraryHierarchyQuery = {
  q?: string
  instanceId?: number
  page?: number
  pageSize?: number
  sort?: 'title' | 'year' | 'updated'
  dir?: 'asc' | 'desc'
}

/** Query surface of one season's episode page. */
export type LibraryEpisodeQuery = {
  page?: number
  pageSize?: number
}

/** Every control is a query parameter; nothing is filtered in the browser. */
function hierarchyQueryString(query: LibraryHierarchyQuery): string {
  const params = new URLSearchParams()
  if (query.q) params.set('q', query.q)
  if (query.instanceId != null && query.instanceId > 0) {
    params.set('instanceId', String(query.instanceId))
  }
  if (query.page != null) params.set('page', String(query.page))
  if (query.pageSize != null) params.set('pageSize', String(query.pageSize))
  if (query.sort) params.set('sort', query.sort)
  if (query.dir) params.set('dir', query.dir)
  const qs = params.toString()
  return qs ? `?${qs}` : ''
}

export type QueueTransition = {
  id: number
  status: string
}

/**
 * POST /api/queue/{id}/approve response (#48). Approval only records intent:
 * extraction writes the vault track and the sync job is born from it, so the
 * body carries no job id. Refresh the queue instead, and read the job from
 * GET /api/sync/jobs once it exists.
 */
export type QueueApproveResponse = {
  approved: boolean
}

/** The optional approve body: a manual match, and the force "start now" intent. */
export type ApproveQueueOptions = {
  /** Manual arr library match for a row the automatic parse could not identify. */
  libraryCacheId?: number
  /**
   * Skip the tag gate for this one job. The server records it only while
   * `security.options.allow_force_tag_override` is on, so a request cannot grant
   * itself the escape hatch.
   */
  forceIgnoreTagFilter?: boolean
}

export type VaultItem = {
  id: number
  title: string
  media_type: 'movie' | 'series'
  season_number: number | null
  episode_number: number | null
  filename: string
  file_size: number
  sha256: string
  language: string
  language_iso3?: string
  variant: string
  codec: string
  channels: string
  channel_count: number
  source_release_name: string
  created_at: string
}

export type VaultResponse = {
  items: VaultItem[]
}

/** Which archived tracks a rename plan covers (#47). */
export type VaultOrganiseScope = {
  media_type: 'movie' | 'series' | 'all'
  ids?: number[]
}

/**
 * One row of the plan table: the file where it is now and where the naming
 * pattern would put it. `conflict` is the machine name of the plan-wide
 * conflict this row takes part in (`destination_duplicate`,
 * `destination_exists`, `source_missing`, `destination_parent`) — the UI
 * translates it and never parses the free text.
 */
export type VaultPlanFile = {
  track_id: number
  media_id: number
  source: string
  destination: string
  changed: boolean
  reason: string
  conflict: string | null
}

/** One name a plan cannot produce, with every track involved. */
export type VaultPlanConflict = {
  kind: string
  destination: string
  track_ids: number[]
}

export type VaultPlanImpact = {
  renamed: number
  unchanged: number
  /** Jobs referencing these tracks by id; a rename does not break them. */
  jobs_referencing_tracks: number
  /** Non-zero exactly when apply would be refused for a job in analyzing/muxing. */
  jobs_blocked: number
  library_rows_affected: number
}

/**
 * POST /api/vault/organise/preview response. `plan_id` is absent whenever the
 * plan carries any conflict: apply takes a plan_id, so a conflicted plan
 * cannot be applied at all.
 */
export type VaultPlanResponse = {
  plan_id?: string
  expires_at?: string
  files: VaultPlanFile[]
  conflicts: VaultPlanConflict[]
  impact: VaultPlanImpact
}

/** POST /api/vault/organise/apply response: what the run actually did. */
export type VaultApplyResponse = {
  run_id: number
  status: string
  file_count: number
  renamed: number
  unchanged: number
}

/**
 * One audited file of a needs_attention run (#127). `result` is the closed
 * vocabulary `reconciled` | `failed`; the UI translates it and never parses
 * the run's `detail` prose.
 */
export type VaultOrganiseRunFile = {
  source_path: string
  destination_path: string
  result: string
}

/** One audit run of GET /api/vault/organise/runs, newest first. */
export type VaultOrganiseRun = {
  id: number
  started_at: string
  finished_at?: string
  /** `applied` | `rolled_back` | `needs_attention`; kept loose for the future. */
  status: string
  plan_hash: string
  file_count: number
  actor?: string
  detail?: string
  /**
   * Present only for a needs_attention run, and only with the files a human has
   * to inspect (reconciled, failed). An applied run carries no list at all.
   */
  files?: VaultOrganiseRunFile[]
}

export type VaultOrganiseRunsResponse = {
  items: VaultOrganiseRun[]
}

/**
 * Public status of GET /api/sync/jobs. `held` means the approval is recorded and
 * the job exists but waits for its target to become eligible (#48) — it is not
 * running, not finished and not broken. `cancelled` is a withdrawn approval; the
 * server only cancels a job that had not started, so it is never a failure.
 */
export type SyncJobStatus =
  | 'queued'
  | 'running'
  | 'done'
  | 'failed'
  | 'held'
  | 'cancelled'

/**
 * The closed vocabulary a held (or cancelled) job carries (`store.HeldReason*`).
 * The server never puts free text here: the UI translates the code.
 */
export type SyncHeldReason =
  | 'waiting_for_tags'
  | 'missing_target'
  | 'target_changed'
  | 'cancelled_by_rejection'

/** One technique's try at one checkpoint (`analyze.Attempt`). */
export type SyncAttempt = {
  /** `waveform` | `mfcc`; kept loose so an unknown method renders as itself. */
  technique: string
  offset_ms: number
  quality: number
  match_z: number
  precision_ms: number
  /** Whether this try cleared its own gates. The first accepted one wins. */
  accepted: boolean
  /** Why the try failed outright (decode, timeout); empty for a real answer. */
  error?: string
}

/** One multi-point correlation sample persisted in the job's DriftReport. */
export type SyncCheckpoint = {
  position_pct: number
  slice_start_sec: number
  slice_len_sec: number
  offset_ms: number
  correlation_quality: number
  /**
   * Parabolic peak interpolation, diagnostics only — the applied offset stays
   * `offset_ms`. Absent on a report written before 0.5.0.
   */
  offset_sub_ms?: number
  /**
   * Peak distance from the window's lag distribution in standard deviations.
   * `0` is the "not recorded" sentinel, so a 0.4.0 row renders as unknown.
   */
  match_z?: number
  /**
   * The grid this checkpoint's offset is reported on, in milliseconds — the
   * matcher's own, so the UI never prints a precision the answer does not have
   * (D3). The sample correlator reports one sample of the analysis rate
   * (0.125 at 8 kHz); the mel-cepstral matcher reports its 10 ms hop. Read it,
   * do not derive it from the sample rate. Absent before 0.5.0.
   */
  precision_ms?: number
  /**
   * The technique whose answer was kept here (`analyze.Checkpoint.Technique`).
   * Every 0.5.0 report carries it: the Go field has no `omitempty` and the
   * worker always sets it, even for the shipped single technique. Only a report
   * written before 0.5.0 (or a partial object from a hand-edited store) leaves
   * it out, which is the one case the Method column's dash is for.
   */
  technique?: string
  /** Every technique that ran on this checkpoint, in list order. Absent before 0.5.0. */
  attempts?: SyncAttempt[]
}

export type SyncVerdict = 'ok' | 'drifting' | 'unreliable' | 'out_of_range'

/** The verdict table that judged a report; absent on reports stored before 0.5.0. */
export type SyncThresholds = {
  stable_spread_ms: number
  low_quality: number
  match_z_maybe: number
  match_z_good: number
}

/** What the line through the checkpoint offsets says about the material. */
export type SyncFitShape = 'constant' | 'drift' | 'cut' | 'mixed' | 'insufficient'

/**
 * The trend through the checkpoint offsets (`analyze.LineFit`, #86). `slope_ms_per_sec`
 * is milliseconds of offset per second of film: a 25.000 -> 23.976 fps mismatch
 * reads +40.96. `tempo_factor` is the atempo factor that removes the slope and is
 * set only for a drift the encoder can represent; `tempo_applied` says whether
 * the worker folded it into the job's single encode.
 *
 * Every field is optional: the fit itself is absent on a 0.4.0 report, and a
 * reader must tolerate a partial object rather than invent numbers.
 */
export type SyncFit = {
  slope_ms_per_sec?: number
  intercept_ms?: number
  residual_rms_ms?: number
  max_residual_ms?: number
  r2?: number
  points?: number
  /** Closed set on the server; kept loose so an unknown shape renders as itself. */
  shape?: string
  cut_at_sec?: number
  cut_uncertainty_sec?: number
  tempo_factor?: number
  tempo_applied?: boolean
}

/** One run of the film during which the offset is constant (`analyze.Segment`). */
export type SyncSegment = {
  /** Start of the run in the TARGET timeline, in film seconds. */
  start_sec: number
  /** End of the run in the TARGET timeline, in film seconds. */
  end_sec: number
  /** The offset applied for this run alone. */
  offset_ms: number
  /** The weakest checkpoint in the run, 0..1: the run is only as good as it. */
  confidence: number
  /** How many checkpoints the run rests on. */
  points: number
}

/** Where one segment ends and the next begins, in film time (`analyze.Boundary`). */
export type SyncBoundary = {
  at_sec: number
  /** Half the sampling step: how precisely the join is known, not more. */
  uncertainty_sec: number
  /** The offset change across the join, in milliseconds. */
  step_ms: number
}

/**
 * The segmentation result of a `piecewise` job (`analyze.Piecewise`, #84).
 *
 * Every field is optional so both directions stay tolerant: a 0.4.0 report has
 * no block at all, and a 0.5.0 report whose piecewise settings were off has no
 * block either — that absence is how a reader tells "never ran" from "ran and
 * found nothing" (which is a single segment and no rejection).
 *
 * `accepted` and `cut` are methods in Go, not wire fields, so they are derived
 * here by [piecewiseAccepted] and [piecewiseCut].
 */
export type SyncPiecewise = {
  segments?: SyncSegment[]
  boundaries?: SyncBoundary[]
  /** The offset a rejected plan falls back to: the median over every point. */
  dominant_offset_ms?: number
  /** The sampling step the plan actually used; widened past `MaxDenseSamples`. */
  step_sec?: number
  /** Why the plan was refused; empty (absent) on success. */
  rejected_reason?: string
}

/**
 * Whether the segmentation is a plan the job could use: nothing was rejected.
 * A single-segment plan is accepted as what it is — the ordinary constant-offset
 * job — so it must not read as a failure.
 */
export function piecewiseAccepted(piecewise?: SyncPiecewise): boolean {
  return Boolean(piecewise) && !(piecewise?.rejected_reason ?? '').trim()
}

/** Whether the accepted plan actually found a join: only that is a re-cut. */
export function piecewiseCut(piecewise?: SyncPiecewise): boolean {
  return piecewiseAccepted(piecewise) && (piecewise?.segments?.length ?? 0) > 1
}

/**
 * The `sync.strategy.piecewise` settings block (`analyze.PiecewisePolicy`, #84):
 * the policy a job is analysed under, as opposed to [SyncPiecewise], the result
 * a report carries. Off by default (D5) — a piecewise job re-encodes the
 * operator's dub copy, which nobody should pay for a film that needed one
 * offset.
 *
 * Every field is optional because a section written before 0.5.0 has no block
 * at all, which reads as the documented default at `enabled: false`. The
 * ranges the server validates are on each field.
 */
export type SyncPiecewisePolicy = {
  enabled?: boolean
  /** How often the timeline is sampled: 15..300 s. */
  step_sec?: number
  /** How much audio each dense checkpoint compares: 20..190 s, at most 2× step. */
  window_sec?: number
  /** How much better a split must fit before it is believed: 50..5000 ms. */
  split_penalty_ms?: number
  /** Runs shorter than this are merged into their neighbour: 30..3600 s. */
  min_segment_sec?: number
  /** A cap on the result: 2..40. */
  max_segments?: number
}

/** What was done to the dub copy before the mux, so a row can say it in words. */
export type SyncApplication = {
  /** `copy` | `re-encoded` | `piecewise` */
  mode: string
  /** e.g. `copied untouched`, `re-encoded (tempo 0.95904)` */
  note: string
  /** How many runs of the film a piecewise render cut and placed separately. */
  segment_count?: number
  /** The same runs the report segmented (`analyze.Application.Segments`). */
  segments?: SyncSegment[]
  /**
   * Milliseconds of the film's end that no dub audio covers after a piecewise
   * render (#129). Absent means the dub covers the film (or its length was
   * unknown), so the UI says nothing.
   */
  tail_silence_ms?: number
}

export type SyncReport = {
  verdict: SyncVerdict | string
  offsets_ms: number[]
  qualities: number[]
  max_drift_ms: number
  classification: string
  checkpoints: SyncCheckpoint[]
  sync_method: string
  /** Checkpoint-ordered, like `offsets_ms`; absent on a 0.4.0 report. */
  match_z?: number[]
  /** Coarsest precision among the checkpoints; absent on a 0.4.0 report. */
  precision_ms?: number
  thresholds?: SyncThresholds
  /** The timeline fit; absent on a 0.4.0 report, so the UI must render "unknown". */
  fit?: SyncFit
  /** Absent on a plain copy; present when the dub copy was re-encoded. */
  application?: SyncApplication
  /** What this job ran, so a report is self-describing; absent before 0.5.0. */
  strategy?: SyncStrategy
  /**
   * The segmentation result (#84); absent when the feature was off, so a plain
   * 0.4.0-shaped report stays byte-identical. See [piecewiseCut].
   */
  piecewise?: SyncPiecewise
  /**
   * One operator-facing sentence saying why a job needs a human look even
   * though the verdict alone does not demand it — today "the cut was not
   * confident, the dominant offset was applied" (D5). Distinct from `reason`,
   * which belongs to the reference-language resolver.
   */
  review_reason?: string
  /**
   * A configured technique that did not run in this build (no engine for it),
   * so the report says why instead of quietly doing less. Absent for the
   * shipped strategy, which always runs.
   */
  notes?: string[]
  reason?: string
}

export type SyncJob = {
  id: number
  status: SyncJobStatus | string
  vault_track_id: number
  target_library_cache_id: number
  target_video_path?: string
  target_lang_iso2?: string
  /** Where mux published the file (Target, or the separate-mode deliverable). */
  output_path?: string
  sync_method?: string
  calculated_offset_ms: number
  report?: SyncReport
  /**
   * Which analysis mode the job actually used. `full` after a fallback, even if
   * `windows` was requested. Absent on jobs analysed before windowing existed —
   * render those as unknown, never as `full`.
   */
  analysis_source?: string
  /** Why the requested mode was abandoned (e.g. `2-suspect-windows`); empty when it ran as asked. */
  analysis_fallback?: string
  error_message?: string
  /**
   * Why a job is `held` — or why a `cancelled` approval was withdrawn — as a
   * closed-set code (`SyncHeldReason`). A running or finished job carries no
   * reason. Kept loose so a code this build does not know renders as itself
   * rather than being guessed into a cause.
   */
  held_reason?: string
  /**
   * The operator approved this job with the force override: it skipped the tag
   * gate, is born `queued` and is never re-held
   * (`security.options.allow_force_tag_override`). Absent on older payloads and
   * on a job nobody forced.
   */
  force_ignore_tag_filter?: boolean
  /** The import_queue row whose approval created this job; 0/absent for a Studio job. */
  origin_queue_id?: number
  created_at?: string
  updated_at?: string
}

export type SyncJobsResponse = {
  items: SyncJob[]
}

export type CreateSyncJobBody = {
  vault_track_id: number
  target_library_cache_id: number
}

/** Masked API token as returned by GET /api/auth/tokens (never the plaintext). */
export type ApiToken = {
  id: number
  name: string
  /** Leading characters stored for display, e.g. `dub_AbCdEfGh`. */
  prefix: string
  created_at: string
  last_used_at?: string | null
}

export type ApiTokensResponse = {
  tokens: ApiToken[]
}

/** POST /api/auth/tokens response: the plaintext token is present only here. */
export type ApiTokenCreated = ApiToken & {
  token: string
}

/** Log severities exposed by GET /api/logs and PUT /api/logs/level. */
export type LogLevel = 'debug' | 'info' | 'warn' | 'error'

/** One ring-buffer entry. `ts` is an RFC 3339 timestamp. */
export type LogEntry = {
  ts: string
  level: LogLevel
  component?: string
  message: string
  job_id?: string
  media_id?: string
}

export type LogsResponse = {
  entries: LogEntry[]
  level: LogLevel
}

export type LogsQuery = {
  /** Minimum severity; omitted means every entry kept in the ring. */
  level?: LogLevel
  component?: string
  limit?: number
  since?: string
}

export class ApiError extends Error {
  readonly status: number
  /** Machine-readable reason from the error body, when the route sends one. */
  readonly code?: string
  /**
   * The arr libraries the server asks the operator to choose between. Only the
   * `ambiguous_kind` envelope carries it, and the UI must render exactly these —
   * never a list of its own making.
   */
  readonly kinds?: string[]
  /** Parsed JSON error body when the server sent one; otherwise undefined. */
  readonly payload: unknown

  constructor(
    status: number,
    message: string,
    code?: string,
    payload: unknown = undefined,
    kinds?: string[],
  ) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.kinds = kinds
    this.payload = payload
  }
}

/** Event dispatched on 401 so the app shell can bounce to /login. */
export const UNAUTHORIZED_EVENT = 'dubarr:unauthorized'

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  if (init.body !== undefined && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }
  const response = await fetch(path, {
    credentials: 'include',
    ...init,
    headers,
  })

  if (response.status === 401) {
    window.dispatchEvent(new Event(UNAUTHORIZED_EVENT))
  }

  if (!response.ok) {
    let message = `Request failed (${response.status})`
    let code: string | undefined
    let kinds: string[] | undefined
    let payload: unknown
    try {
      payload = await response.json()
      if (payload && typeof payload === 'object') {
        if ('error' in payload) {
          const raw = (payload as { error?: unknown }).error
          if (typeof raw === 'string' && raw !== '') message = raw
        }
        if ('code' in payload) {
          const raw = (payload as { code?: unknown }).code
          if (typeof raw === 'string' && raw !== '') code = raw
        }
        if ('kinds' in payload) {
          const raw = (payload as { kinds?: unknown }).kinds
          if (Array.isArray(raw)) {
            kinds = raw.filter(
              (entry): entry is string => typeof entry === 'string' && entry !== '',
            )
          }
        }
      }
    } catch {
      // Non-JSON error bodies fall back to the status message.
    }
    throw new ApiError(response.status, message, code, payload, kinds)
  }

  if (response.status === 204) return undefined as T
  return (await response.json()) as T
}

export type IdentifyQueueOptions = {
  /** Release name override; the server falls back to the filename stem. */
  title?: string
  /** The arr library the operator picked after a 409 ambiguous_kind. */
  kind?: string
}

export const api = {
  authStatus: () => request<AuthStatus>('/api/auth/status'),
  me: () => request<User>('/api/auth/me'),
  setup: (username: string, password: string) =>
    request<User>('/api/auth/setup', {
      method: 'POST',
      body: JSON.stringify({ username, password }),
    }),
  login: (username: string, password: string) =>
    request<User>('/api/auth/login', {
      method: 'POST',
      body: JSON.stringify({ username, password }),
    }),
  logout: () => request<void>('/api/auth/logout', { method: 'POST' }),
  changePassword: (currentPassword: string, newPassword: string) =>
    request<void>('/api/auth/password', {
      method: 'POST',
      body: JSON.stringify({ current_password: currentPassword, new_password: newPassword }),
    }),
  settings: () => request<SettingsEnvelope>('/api/settings'),
  saveSettings: (settings: Record<string, unknown>) =>
    request<SettingsEnvelope>('/api/settings', {
      method: 'PUT',
      body: JSON.stringify({ settings }),
    }),
  exportSettings: () => request<SettingsExport>('/api/settings/export'),
  arrConnections: () => request<ArrConnectionsResponse>('/api/settings/arr'),
  saveArrConnection: (kind: ArrConnectionKind, payload: ArrConnectionPayload) =>
    request<ArrConnection>(`/api/settings/arr/${kind}`, {
      method: 'PUT',
      body: JSON.stringify(payload),
    }),
  deleteArrConnection: (kind: ArrConnectionKind) =>
    request<void>(`/api/settings/arr/${kind}`, { method: 'DELETE' }),
  testArrConnection: (kind: ArrConnectionKind, payload?: ArrConnectionTestPayload) =>
    request<ArrConnectionTestResult>(`/api/settings/arr/${kind}/test`, {
      method: 'POST',
      // No body at all when there is nothing to send, so a caller that only
      // knows the kind still tests the saved connection.
      ...(payload ? { body: JSON.stringify(payload) } : {}),
    }),
  browse: (path: string) =>
    request<BrowseResponse>(`/api/system/browse?path=${encodeURIComponent(path)}`),
  health: () => request<Health>('/api/health'),
  queue: () => request<QueueResponse>('/api/queue'),
  searchLibrary: (q: string) =>
    request<LibrarySearchResponse>(`/api/imports/search-library?q=${encodeURIComponent(q)}`),
  /** Radarr as one flat, server-paged list. */
  libraryMovies: (query: LibraryHierarchyQuery = {}) =>
    request<LibraryPagedResponse<LibraryMovieResource>>(
      `/api/library/movies${hierarchyQueryString(query)}`,
    ),
  /** Sonarr as one row per series, aggregated server-side. */
  librarySeries: (query: LibraryHierarchyQuery = {}) =>
    request<LibraryPagedResponse<LibrarySeriesResource>>(
      `/api/library/series${hierarchyQueryString(query)}`,
    ),
  /** One series header; the list already carries these fields for an expanded row. */
  librarySeriesDetail: (id: number) => request<LibrarySeriesResource>(`/api/library/series/${id}`),
  /** Every season of one series with its statistics. */
  librarySeriesSeasons: (id: number) =>
    request<LibrarySeasonsResponse>(`/api/library/series/${id}/seasons`),
  /** One page of one season; `truncated` says the season outgrew one payload. */
  librarySeasonEpisodes: (id: number, season: number, query: LibraryEpisodeQuery = {}) => {
    const params = new URLSearchParams()
    if (query.page != null) params.set('page', String(query.page))
    if (query.pageSize != null) params.set('pageSize', String(query.pageSize))
    const qs = params.toString()
    return request<LibrarySeasonEpisodesResponse>(
      `/api/library/series/${id}/seasons/${season}/episodes${qs ? `?${qs}` : ''}`,
    )
  },
  vault: () => request<VaultResponse>('/api/vault'),
  /**
   * Plans a rename for the scope and returns every row. It writes nothing: no
   * directories, no files, no rows. A plan with a conflict comes back without a
   * `plan_id`, which is how the UI knows it cannot be applied.
   */
  vaultOrganisePreview: (scope: VaultOrganiseScope) =>
    request<VaultPlanResponse>('/api/vault/organise/preview', {
      method: 'POST',
      body: JSON.stringify({ scope }),
    }),
  /** Applies a previewed plan; the server re-checks the hash and refuses `stale_plan`. */
  vaultOrganiseApply: (planId: string) =>
    request<VaultApplyResponse>('/api/vault/organise/apply', {
      method: 'POST',
      body: JSON.stringify({ plan_id: planId, confirm: true }),
    }),
  /** Audit history, newest first. Reads the database only, so it survives a restart. */
  vaultOrganiseRuns: (limit = 50) =>
    request<VaultOrganiseRunsResponse>(`/api/vault/organise/runs?limit=${limit}`),
  syncJobs: (status?: SyncJobStatus) =>
    request<SyncJobsResponse>(
      `/api/sync/jobs${status ? `?status=${encodeURIComponent(status)}` : ''}`,
    ),
  syncJob: (id: number) => request<SyncJob>(`/api/sync/jobs/${id}`),
  createSyncJob: (body: CreateSyncJobBody) =>
    request<SyncJob>('/api/sync/jobs', {
      method: 'POST',
      body: JSON.stringify(body),
    }),
  /**
   * Records the operator's approval intent (#48). Only the keys with an intent
   * behind them are sent: an empty body keeps the arr-matched, hold-if-ineligible
   * behaviour, and `force_ignore_tag_filter` is never sent as `false` because
   * "no opinion" and "do not force" are the same thing to the server.
   */
  approveQueueItem: (id: number, opts: ApproveQueueOptions = {}) => {
    const body: { library_cache_id?: number; force_ignore_tag_filter?: boolean } = {}
    if (opts.libraryCacheId != null) body.library_cache_id = opts.libraryCacheId
    if (opts.forceIgnoreTagFilter) body.force_ignore_tag_filter = true
    return request<QueueApproveResponse>(`/api/queue/${id}/approve`, {
      method: 'POST',
      body: Object.keys(body).length === 0 ? undefined : JSON.stringify(body),
    })
  },
  rejectQueueItem: (id: number) => request<QueueTransition>(`/api/queue/${id}/reject`, { method: 'POST' }),
  /**
   * Asks arr's own parser to identify the release; title overrides the filename
   * stem, and kind names the arr library when the server asked the operator to
   * choose (409 ambiguous_kind). Absent keys are omitted, never sent as
   * undefined: an empty body still means "parse it in the only library there is".
   */
  identifyQueueItem: (id: number, opts: IdentifyQueueOptions = {}) => {
    const body: { title?: string; kind?: string } = {}
    if (opts.title) body.title = opts.title
    if (opts.kind) body.kind = opts.kind
    return request<QueueIdentifyResponse>(`/api/queue/${id}/identify`, {
      method: 'POST',
      body: Object.keys(body).length === 0 ? undefined : JSON.stringify(body),
    })
  },
  /** Persists the operator's pick; the server reports the resulting match state. */
  matchQueueItem: (id: number, libraryCacheId: number, confidence?: number) =>
    request<QueueMatchResponse>(`/api/queue/${id}/match`, {
      method: 'POST',
      body: JSON.stringify(
        confidence == null
          ? { library_cache_id: libraryCacheId }
          : { library_cache_id: libraryCacheId, confidence },
      ),
    }),
  tokens: () => request<ApiTokensResponse>('/api/auth/tokens'),
  createToken: (name: string) =>
    request<ApiTokenCreated>('/api/auth/tokens', {
      method: 'POST',
      body: JSON.stringify({ name }),
    }),
  revokeToken: (id: number) => request<void>(`/api/auth/tokens/${id}`, { method: 'DELETE' }),
  logs: (query: LogsQuery = {}) => {
    const params = new URLSearchParams()
    if (query.level) params.set('level', query.level)
    if (query.component) params.set('component', query.component)
    if (query.limit != null) params.set('limit', String(query.limit))
    if (query.since) params.set('since', query.since)
    const qs = params.toString()
    return request<LogsResponse>(`/api/logs${qs ? `?${qs}` : ''}`)
  },
  logLevel: () => request<{ level: LogLevel }>('/api/logs/level'),
  setLogLevel: (level: LogLevel) =>
    request<{ level: LogLevel }>('/api/logs/level', {
      method: 'PUT',
      body: JSON.stringify({ level }),
    }),
}
