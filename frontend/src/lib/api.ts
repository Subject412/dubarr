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
  media_roots: string[]
}

export type LanguagesPolicy = {
  target_import_languages: string[]
  extra_reference_languages: string[]
  original_language_from_arr: boolean
  exclude_commentary: boolean
  export_mode: 'rule_based' | 'all'
}

export type TargetTagFilter = {
  required_filename_tags: string[]
  tag_match_mode: 'ANY' | 'ALL' | 'REGEX'
  tag_case_sensitive: boolean
  excluded_filename_tags: string[]
  enforce_tag_filter: boolean
}

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

export type MuxOptions = {
  keep_bak: boolean
  track_title_in_mkv: string
}

export type WatchOptions = {
  scan_interval_sec: number
  stability_check_interval_sec: number
}

export type ArrOptions = {
  poll_interval_sec: number
  manual_sync: boolean
}

export type DubarrSettings = {
  'storage.paths': StoragePaths
  'languages.policy': LanguagesPolicy
  'target.tag_filter': TargetTagFilter
  'sync.defaults': SyncDefaults
  'security.options': SecurityOptions
  'mux.options': MuxOptions
  'watch.options': WatchOptions
  'arr.options': ArrOptions
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
}

export type QueueResponse = {
  items: QueueItem[]
}

export type QueueTransition = {
  id: number
  status: string
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
  settings: () => request<SettingsEnvelope>('/api/settings'),
  saveSettings: (settings: Record<string, unknown>) =>
    request<SettingsEnvelope>('/api/settings', {
      method: 'PUT',
      body: JSON.stringify({ settings }),
    }),
  exportSettings: () => request<SettingsExport>('/api/settings/export'),
  browse: (path: string) =>
    request<BrowseResponse>(`/api/system/browse?path=${encodeURIComponent(path)}`),
  health: () => request<Health>('/api/health'),
  queue: () => request<QueueResponse>('/api/queue'),
  approveQueueItem: (id: number) =>
    request<QueueTransition>(`/api/queue/${id}/approve`, { method: 'POST' }),
  rejectQueueItem: (id: number) =>
    request<QueueTransition>(`/api/queue/${id}/reject`, { method: 'POST' }),
}
