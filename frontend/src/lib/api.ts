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

export type SecurityOptions = {
  allow_force_tag_override: boolean
}

export type MuxOutputMode = 'replace' | 'separate'

export type MuxOptions = {
  track_title_in_mkv: string
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

/** Sibling filenames that mark a dropped file as a donor dub. */
export type ImportsOptions = {
  donor_patterns: string[]
}

export type DubarrSettings = {
  'storage.paths': StoragePaths
  'languages.policy': LanguagesPolicy
  'target.tag_filter': TargetTagFilter
  'sync.defaults': SyncDefaults
  'security.options': SecurityOptions
  'mux.options': MuxOptions
  'audio.options': AudioOptions
  'watch.options': WatchOptions
  'arr.options': ArrOptions
  'imports.options': ImportsOptions
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
   * The configured donor filename patterns (blank entries dropped). Empty means
   * no donor pattern is configured at all, which is a different next step from
   * "nothing in the folder matched it". Absent on older payloads, in which case
   * the card does not claim either cause.
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
   * setting, not a per-row fact. When it is, the server accepts a gate-ineligible
   * target, so its rows arrive as `match_state: 'ready'` and the card enables
   * Approve without any separate eligibility rule. Absent on older payloads.
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
 * same thing as a library with no matching rows. */
export type QueueIdentifyParse = {
  parsed: boolean
  title: string
  year: number
  quality: string
  confidence: number
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

export type QueueTransition = {
  id: number
  status: string
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

export type SyncJobStatus = 'queued' | 'running' | 'done' | 'failed'

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
  /** One sample of the analysis rate (0.125 at 8 kHz). Absent before 0.5.0. */
  precision_ms?: number
}

export type SyncVerdict = 'ok' | 'drifting' | 'unreliable' | 'out_of_range'

/** The verdict table that judged a report; absent on reports stored before 0.5.0. */
export type SyncThresholds = {
  stable_spread_ms: number
  low_quality: number
  match_z_maybe: number
  match_z_good: number
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
  vault: () => request<VaultResponse>('/api/vault'),
  syncJobs: () => request<SyncJobsResponse>('/api/sync/jobs'),
  syncJob: (id: number) => request<SyncJob>(`/api/sync/jobs/${id}`),
  createSyncJob: (body: CreateSyncJobBody) =>
    request<SyncJob>('/api/sync/jobs', {
      method: 'POST',
      body: JSON.stringify(body),
    }),
  approveQueueItem: (id: number, libraryCacheId?: number) =>
    request<QueueTransition>(`/api/queue/${id}/approve`, {
      method: 'POST',
      body:
        libraryCacheId == null ? undefined : JSON.stringify({ library_cache_id: libraryCacheId }),
    }),
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
