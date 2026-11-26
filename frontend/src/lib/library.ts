// Library screen read surface (#29): the item/health shapes served by
// GET /api/library/items and GET /api/library/health, their human labels, and
// the empty-state decision the page renders.
//
// Fetchers live here (not in api.ts) so the library unit stays additive: the
// request contract mirrors api.ts' `request` helper, including the 401 event.
import { ApiError, UNAUTHORIZED_EVENT, type ArrConnectionKind } from './api'

/** Page size the screen asks for; also the offset step of Next/Previous. */
export const LIBRARY_PAGE_SIZE = 50

export type LibraryKind = ArrConnectionKind

/** Processing Gate verdicts, exactly as stored (migration 0004). */
export type LibraryEligibility = 'eligible' | 'waiting_for_tags' | 'missing_target'

export type BadgeTone = 'neutral' | 'success' | 'warning' | 'danger' | 'accent'

export type LibraryItem = {
  id: number
  media_type: string
  title: string
  year: number
  /** Null for movies. */
  season: number | null
  episode: number | null
  kind: LibraryKind
  instance_id: number
  instance_name: string
  eligibility_status: LibraryEligibility
  matches_target_tags: boolean
  root_folder_path: string
  target_file_path: string
  last_seen_at: string
  updated_at: string
}

export type LibraryItemsResponse = {
  items: LibraryItem[]
  total: number
  limit: number
  offset: number
}

/** One arr instance's last sync result. Tri-state `last_ok`: null = never attempted. */
export type LibraryHealthInstance = {
  kind: LibraryKind
  name: string
  enabled: boolean
  last_sync_at: string | null
  last_attempt_at: string | null
  last_ok: boolean | null
  last_error: string
  items_seen: number
}

export type LibraryHealthResponse = {
  instances: LibraryHealthInstance[]
  cache_items: number
}

export type LibraryItemsQuery = {
  q?: string
  kind?: LibraryKind | ''
  eligibility?: LibraryEligibility | ''
  limit?: number
  offset?: number
}

export const libraryKindOptions: { value: LibraryKind | ''; label: string }[] = [
  { value: '', label: 'All' },
  { value: 'radarr', label: 'Radarr' },
  { value: 'sonarr', label: 'Sonarr' },
]

export const libraryEligibilityOptions: { value: LibraryEligibility | ''; label: string }[] = [
  { value: '', label: 'All' },
  { value: 'eligible', label: 'Eligible' },
  { value: 'waiting_for_tags', label: 'Waiting for tags' },
  { value: 'missing_target', label: 'Missing target' },
]

export function eligibilityLabel(status: string): string {
  return libraryEligibilityOptions.find((option) => option.value === status)?.label ?? status
}

export function eligibilityTone(status: string): BadgeTone {
  switch (status) {
    case 'eligible':
      return 'success'
    case 'waiting_for_tags':
      return 'warning'
    case 'missing_target':
      return 'danger'
    default:
      return 'neutral'
  }
}

/** `S02E05` for an episode row, `Movie` otherwise. */
export function libraryTypeLabel(item: Pick<LibraryItem, 'media_type' | 'season' | 'episode'>): string {
  const { season, episode } = item
  if (season != null && episode != null) {
    const pad = (n: number) => String(n).padStart(2, '0')
    return `S${pad(season)}E${pad(episode)}`
  }
  return item.media_type === 'movie' ? 'Movie' : 'Series'
}

/** Last path segment of an arr file path (either separator). */
export function basename(path: string): string {
  if (!path) return ''
  const parts = path.split(/[\\/]/)
  return parts[parts.length - 1] ?? ''
}

/**
 * Backend timestamps are UTC `YYYY-MM-DD HH:MM:SS`. A bare space-separated
 * string parses as *local* time in most engines, so the zone is made explicit
 * (the server always writes UTC).
 */
export function parseServerTime(text: string | null | undefined): Date | null {
  if (!text) return null
  const iso = text.includes('T') ? text : text.replace(' ', 'T')
  const zoned = /[zZ]$|[+-]\d{2}:?\d{2}$/.test(iso) ? iso : `${iso}Z`
  const date = new Date(zoned)
  return Number.isNaN(date.getTime()) ? null : date
}

/** Humanised age of a server timestamp: `just now`, `12 minutes ago`, `Never`. */
export function relativeTime(text: string | null | undefined, now = Date.now()): string {
  const date = parseServerTime(text)
  if (!date) return 'Never'
  const seconds = Math.round((now - date.getTime()) / 1000)
  if (seconds < 45) return 'just now'
  const units: [number, string][] = [
    [60, 'minute'],
    [60 * 60, 'hour'],
    [60 * 60 * 24, 'day'],
    [60 * 60 * 24 * 30, 'month'],
    [60 * 60 * 24 * 365, 'year'],
  ]
  let label = 'just now'
  for (const [scale, name] of units) {
    if (seconds < scale) break
    const value = Math.floor(seconds / scale)
    label = `${value} ${name}${value === 1 ? '' : 's'} ago`
  }
  return label
}

/** Badge for an instance's last sync result; a disabled instance wins. */
export function healthState(instance: LibraryHealthInstance): { label: string; tone: BadgeTone } {
  if (!instance.enabled) return { label: 'Disabled', tone: 'neutral' }
  if (instance.last_ok === null) return { label: 'Never synced', tone: 'neutral' }
  if (instance.last_ok) return { label: 'Up to date', tone: 'success' }
  return { label: 'Sync failed', tone: 'danger' }
}

/** Instances in the active kind scope; `''` keeps the whole list. */
export function scopedInstances(
  instances: LibraryHealthInstance[],
  kind: LibraryKind | '' = '',
): LibraryHealthInstance[] {
  return kind === '' ? instances : instances.filter((instance) => instance.kind === kind)
}

/** First non-empty sync error, used by the `sync_failed` empty state. */
export function firstSyncError(instances: LibraryHealthInstance[]): string {
  return instances.find((instance) => instance.last_ok === false && instance.last_error)?.last_error ?? ''
}

export type LibraryEmptyState = 'never_synced' | 'sync_failed' | 'empty_library' | 'filtered_nothing'

/**
 * Decides which of the four mutually exclusive empty states applies. Only
 * called once the item page is empty. A filter hit that hides existing rows is
 * `filtered_nothing`; otherwise the instance sync state decides, in the order
 * never-attempted -> failed -> succeeded-empty.
 *
 * Both signals are scoped to the active `kind`: a `kind=radarr` view must not
 * claim "drop a filter" because Sonarr rows exist, nor let a healthy Sonarr
 * hide a failed Radarr. With no kind selected the whole cache/instance list is
 * in play.
 */
export function libraryEmptyState({
  instances,
  cacheItems,
  kind = '',
  filtersActive,
  total,
}: {
  instances: LibraryHealthInstance[]
  cacheItems: number
  kind?: LibraryKind | ''
  filtersActive: boolean
  total: number
}): LibraryEmptyState | null {
  if (total > 0) return null

  const scoped = scopedInstances(instances, kind)
  const existingItems =
    kind === ''
      ? cacheItems
      : scoped.reduce((sum, instance) => sum + instance.items_seen, 0)
  if (filtersActive && existingItems > 0) return 'filtered_nothing'

  // Only rows that were actually attempted can say anything about the sync.
  // A never-attempted instance (`last_ok === null`) must not veto a failure.
  const attempted = scoped.filter((instance) => instance.last_ok !== null)
  if (attempted.length === 0) return 'never_synced'
  if (attempted.every((instance) => instance.last_ok === false)) return 'sync_failed'
  // Mixed list (some failed, some succeeded): a success proves the pipeline
  // works, and the health strip above already shows the failing instance.
  return 'empty_library'
}

/** Query string for the items endpoint; limit/offset are always explicit. */
export function libraryItemsPath(query: LibraryItemsQuery = {}): string {
  const params = new URLSearchParams()
  if (query.q) params.set('q', query.q)
  if (query.kind) params.set('kind', query.kind)
  if (query.eligibility) params.set('eligibility', query.eligibility)
  params.set('limit', String(query.limit ?? LIBRARY_PAGE_SIZE))
  params.set('offset', String(query.offset ?? 0))
  return `/api/library/items?${params.toString()}`
}

/** Mirrors api.ts' request helper: same-origin, cookie auth, 401 event. */
async function getJSON<T>(path: string): Promise<T> {
  const response = await fetch(path, { credentials: 'include' })
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
  return (await response.json()) as T
}

export function fetchLibraryItems(query: LibraryItemsQuery = {}): Promise<LibraryItemsResponse> {
  return getJSON<LibraryItemsResponse>(libraryItemsPath(query))
}

export function fetchLibraryHealth(): Promise<LibraryHealthResponse> {
  return getJSON<LibraryHealthResponse>('/api/library/health')
}

/** Machine-readable reasons POST /api/library/refresh refuses a request. */
export type LibraryRefreshCode = 'busy' | 'not_configured' | 'stopped'

export type LibraryRefreshResponse = {
  accepted: boolean
  /** The requested kind, or `all` when the pass covers every instance. */
  kind: LibraryKind | 'all'
}

/**
 * A refused refresh. The 409 responses carry `code`, so the page can react to
 * the exact reason instead of matching the human message.
 */
export class LibraryRefreshError extends ApiError {
  readonly code: LibraryRefreshCode | ''

  constructor(status: number, message: string, code: LibraryRefreshCode | '') {
    super(status, message)
    this.name = 'LibraryRefreshError'
    this.code = code
  }
}

/**
 * Starts one manual sync pass (`''` = every instance) and resolves as soon as
 * the server accepts it (202); the pass itself lands later, which is why the
 * caller invalidates health and items afterwards.
 */
export async function refreshLibrary(
  kind: LibraryKind | '' = '',
): Promise<LibraryRefreshResponse> {
  const response = await fetch('/api/library/refresh', {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ kind }),
  })
  if (response.status === 401) {
    window.dispatchEvent(new Event(UNAUTHORIZED_EVENT))
  }
  if (!response.ok) {
    let message = `Request failed (${response.status})`
    let code: LibraryRefreshCode | '' = ''
    try {
      const body: unknown = await response.json()
      if (body && typeof body === 'object') {
        const raw = body as { error?: unknown; code?: unknown }
        if (typeof raw.error === 'string' && raw.error !== '') message = raw.error
        if (raw.code === 'busy' || raw.code === 'not_configured' || raw.code === 'stopped') {
          code = raw.code
        }
      }
    } catch {
      // Non-JSON error bodies fall back to the status message.
    }
    throw new LibraryRefreshError(response.status, message, code)
  }
  return (await response.json()) as LibraryRefreshResponse
}

/** Machine-readable reasons POST .../rescan refuses a request. */
export type LibraryRescanCode = 'not_configured' | 'rescan_failed'

export type LibraryRescanResponse = {
  accepted: boolean
  /**
   * Whether the cache row was refreshed from arr. A failed re-read still means
   * the rescan was accepted — the command is queued in arr either way.
   */
  reread: 'ok' | 'failed'
  /** Scrubbed cause, present only when `reread` is `failed`. */
  reread_error?: string
}

/** A refused rescan (400/404/409/502). */
export class LibraryRescanError extends ApiError {
  readonly code: LibraryRescanCode | ''

  constructor(status: number, message: string, code: LibraryRescanCode | '') {
    super(status, message)
    this.name = 'LibraryRescanError'
    this.code = code
  }
}

/**
 * Asks arr to rescan one cached item and re-read it. Resolves on 202 with the
 * honest re-read verdict; a 502 means the command itself was never accepted.
 */
export async function rescanLibraryItem(id: number): Promise<LibraryRescanResponse> {
  const response = await fetch(`/api/library/items/${id}/rescan`, {
    method: 'POST',
    credentials: 'include',
  })
  if (response.status === 401) {
    window.dispatchEvent(new Event(UNAUTHORIZED_EVENT))
  }
  if (!response.ok) {
    let message = `Request failed (${response.status})`
    let code: LibraryRescanCode | '' = ''
    try {
      const body: unknown = await response.json()
      if (body && typeof body === 'object') {
        const raw = body as { error?: unknown; code?: unknown }
        if (typeof raw.error === 'string' && raw.error !== '') message = raw.error
        if (raw.code === 'not_configured' || raw.code === 'rescan_failed') code = raw.code
      }
    } catch {
      // Non-JSON error bodies fall back to the status message.
    }
    throw new LibraryRescanError(response.status, message, code)
  }
  return (await response.json()) as LibraryRescanResponse
}
