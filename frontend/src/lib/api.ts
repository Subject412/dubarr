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
}

export type SecurityOptions = {
  vault_read_only: boolean
  allow_force_tag_override: boolean
  require_av1_warning: boolean
  require_av1_strict: boolean
}

export type MuxOutputMode = 'replace' | 'separate'

export type MuxOptions = {
  keep_bak: boolean
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
  manual_sync: boolean
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
}

export type QueueResponse = {
  items: QueueItem[]
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
}

export type SyncVerdict = 'ok' | 'drifting' | 'unreliable' | 'out_of_range'

export type SyncReport = {
  verdict: SyncVerdict | string
  offsets_ms: number[]
  qualities: number[]
  max_drift_ms: number
  classification: string
  checkpoints: SyncCheckpoint[]
  sync_method: string
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

  constructor(status: number, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
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
    try {
      const body: unknown = await response.json()
      if (body && typeof body === 'object' && 'error' in body) {
        const raw = (body as { error?: unknown }).error
        if (typeof raw === 'string' && raw !== '') message = raw
      }
    } catch {
      // Non-JSON error bodies fall back to the status message.
    }
    throw new ApiError(response.status, message)
  }

  if (response.status === 204) return undefined as T
  return (await response.json()) as T
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
  testArrConnection: (kind: ArrConnectionKind) =>
    request<ArrConnectionTestResult>(`/api/settings/arr/${kind}/test`, { method: 'POST' }),
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
  rejectQueueItem: (id: number) =>
    request<QueueTransition>(`/api/queue/${id}/reject`, { method: 'POST' }),
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
