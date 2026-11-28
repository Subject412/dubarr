import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Badge, Button, PageHeader, Spinner } from '../components/ui'
import { cn } from '../lib/cn'
import {
  fetchLibraryHealth,
  healthState,
  LibraryRefreshError,
  refreshLibrary,
  relativeTime,
  type LibraryHealthInstance,
  type LibraryHealthResponse,
  type LibraryKind,
} from '../lib/library'
import { MoviesTab } from '../components/library/MoviesTab'
import { SeriesTab } from '../components/library/SeriesTab'

/** One instance's last-sync state; a disabled connection stays visible but muted. */
function HealthCard({
  instance,
  onRefresh,
  syncing,
}: {
  instance: LibraryHealthInstance
  onRefresh: () => void
  syncing: boolean
}) {
  const state = healthState(instance)
  const failed = instance.last_ok === false

  return (
    <div
      className={cn(
        'rounded-xl border border-ink-800 bg-ink-900 px-4 py-3',
        !instance.enabled && 'opacity-60',
      )}
    >
      <div className="flex items-center justify-between gap-2">
        <h3 className="truncate text-sm font-semibold text-zinc-100" title={instance.name}>
          {instance.name}
        </h3>
        <Badge tone={state.tone}>{state.label}</Badge>
      </div>
      <dl className="mt-2 space-y-1 text-xs text-zinc-500">
        <div className="flex items-center justify-between gap-2">
          <dt>Last attempt</dt>
          <dd className="text-zinc-300" title={instance.last_attempt_at ?? 'Never attempted'}>
            {relativeTime(instance.last_attempt_at)}
          </dd>
        </div>
        <div className="flex items-center justify-between gap-2">
          <dt>Items seen</dt>
          <dd className="text-zinc-300">{instance.items_seen}</dd>
        </div>
      </dl>
      {failed && instance.last_error && (
        <p className="mt-2 truncate text-xs text-red-300" title={instance.last_error}>
          {instance.last_error}
        </p>
      )}
      <Button
        variant="secondary"
        size="sm"
        className="mt-3"
        disabled={syncing}
        onClick={onRefresh}
      >
        {syncing ? 'Syncing...' : 'Refresh now'}
      </Button>
    </div>
  )
}

function HealthStrip({
  health,
  error,
  onRefresh,
  pendingKind,
  note,
}: {
  health: LibraryHealthResponse | undefined
  error: Error | null
  onRefresh: (kind: LibraryKind | '') => void
  /** The kind whose refresh is in flight; '' = all, null = nothing pending. */
  pendingKind: LibraryKind | '' | null
  note: string | null
}) {
  if (error) return <Alert>Could not read arr sync health: {error.message}</Alert>
  if (!health) return <Spinner label="Loading sync health" />

  return (
    <section aria-label="Arr sync health" className="mb-6">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-[11px] uppercase tracking-wider text-zinc-500">Sync health</h2>
        <div className="flex items-center gap-3">
          {/* Titles, not cache rows: 486 films + ~12 400 episode rows would
              read as 12 909 "titles cached". "series" is invariable. */}
          <p className="text-xs text-zinc-400">
            {health.cache_movies} {health.cache_movies === 1 ? 'movie' : 'movies'} ·{' '}
            {health.cache_series} series
          </p>
          <Button
            variant="secondary"
            size="sm"
            disabled={pendingKind !== null}
            onClick={() => onRefresh('')}
          >
            {pendingKind === '' ? 'Syncing...' : 'Refresh all'}
          </Button>
        </div>
      </div>
      {note && (
        <p role="status" className="mb-2 text-xs text-amber-300">
          {note}
        </p>
      )}
      {health.instances.length === 0 ? (
        <p className="rounded-xl border border-ink-800 bg-ink-900 px-4 py-3 text-sm text-zinc-400">
          No Radarr or Sonarr connection is configured. Add one under Settings to start syncing.
        </p>
      ) : (
        <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-3">
          {health.instances.map((instance) => (
            <HealthCard
              key={`${instance.kind}-${instance.name}`}
              instance={instance}
              syncing={pendingKind === instance.kind}
              onRefresh={() => onRefresh(instance.kind)}
            />
          ))}
        </div>
      )}
    </section>
  )
}

/**
 * The two views of the library (#56), mirroring the arrs it reads: Radarr is a
 * flat movie list, Sonarr is a series -> seasons -> episodes tree. The tabs are
 * views, not filters, so switching never issues a write and each tab keeps its
 * own paging and search.
 */
const libraryTabs = [
  { id: 'movies', label: 'Movies' },
  { id: 'series', label: 'TV shows' },
] as const

type LibraryTabId = (typeof libraryTabs)[number]['id']

export function LibraryPage() {
  const [activeTab, setActiveTab] = useState<LibraryTabId>('movies')

  const health = useQuery<LibraryHealthResponse, Error>({
    queryKey: ['library', 'health'],
    queryFn: fetchLibraryHealth,
    retry: false,
    staleTime: 10_000,
  })

  const queryClient = useQueryClient()
  const [pendingRefresh, setPendingRefresh] = useState<LibraryKind | '' | null>(null)
  const [refreshNote, setRefreshNote] = useState<string | null>(null)
  const pollTimers = useRef<number[]>([])
  // A 202 can land after unmount; scheduling a poll then would outlive the
  // cleanup that already ran, so the effect below gates every new timer.
  const mountedRef = useRef(true)

  // A pass lands after the 202, not with it: health and every library list are
  // re-read a couple of times (~3 s apart) so the screen updates when the pass
  // finishes and then polling stops. Nothing here outlives the page.
  const clearRefreshPolls = () => {
    for (const timer of pollTimers.current) window.clearTimeout(timer)
    pollTimers.current = []
  }
  const invalidateLibrary = () =>
    Promise.all([
      queryClient.invalidateQueries({ queryKey: ['library', 'health'] }),
      queryClient.invalidateQueries({ queryKey: ['library', 'items'] }),
      queryClient.invalidateQueries({ queryKey: ['library', 'tv'] }),
    ])

  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      clearRefreshPolls()
    }
  }, [])

  const startRefresh = async (kind: LibraryKind | '') => {
    if (pendingRefresh !== null) return
    setPendingRefresh(kind)
    setRefreshNote(null)
    try {
      await refreshLibrary(kind)
      if (!mountedRef.current) return
      clearRefreshPolls()
      await invalidateLibrary()
      // The awaits above may have outlived the page: a timer scheduled now
      // would never be cleared, so drop anything created in that window.
      if (!mountedRef.current) {
        clearRefreshPolls()
        return
      }
      for (const delay of [3000, 6000]) {
        pollTimers.current.push(
          window.setTimeout(() => {
            void invalidateLibrary()
          }, delay),
        )
      }
    } catch (error) {
      // A 409 is a decision, not a failure to retry: name the reason inline.
      if (error instanceof LibraryRefreshError) {
        switch (error.code) {
          case 'busy':
            setRefreshNote('A sync pass is already running.')
            break
          case 'not_configured':
            setRefreshNote('No arr instance is configured yet.')
            break
          case 'stopped':
            setRefreshNote('The sync poller is stopping. Try again in a moment.')
            break
          default:
            setRefreshNote(`Could not start a sync: ${error.message}`)
        }
      } else {
        setRefreshNote(
          `Could not start a sync: ${error instanceof Error ? error.message : 'unknown error'}`,
        )
      }
    } finally {
      setPendingRefresh(null)
    }
  }

  return (
    <div>
      <PageHeader
        title="Library"
        description="Browse titles, inspect tracks and start a sync job."
      />

      <HealthStrip
        health={health.data}
        error={health.error ?? null}
        onRefresh={(kind) => void startRefresh(kind)}
        pendingKind={pendingRefresh}
        note={refreshNote}
      />

      <div
        role="tablist"
        aria-label="Library view"
        className="mb-4 flex gap-1 border-b border-ink-800"
      >
        {libraryTabs.map((tab) => (
          <button
            key={tab.id}
            type="button"
            role="tab"
            id={`library-tab-${tab.id}`}
            aria-selected={activeTab === tab.id}
            aria-controls="library-panel"
            onClick={() => setActiveTab(tab.id)}
            className={cn(
              '-mb-px border-b-2 px-4 py-2 text-sm font-medium transition-colors',
              activeTab === tab.id
                ? 'border-accent-500 text-zinc-100'
                : 'border-transparent text-zinc-400 hover:text-zinc-200',
            )}
          >
            {tab.label}
          </button>
        ))}
      </div>

      <div role="tabpanel" id="library-panel" aria-labelledby={`library-tab-${activeTab}`}>
        {activeTab === 'movies' ? <MoviesTab health={health.data} /> : <SeriesTab />}
      </div>
    </div>
  )
}
