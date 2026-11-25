import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Alert, Badge, Button, Card, Input, PageHeader, Select, Spinner } from '../components/ui'
import { cn } from '../lib/cn'
import { useDebouncedValue } from '../lib/hooks'
import {
  basename,
  eligibilityLabel,
  eligibilityTone,
  fetchLibraryHealth,
  fetchLibraryItems,
  firstSyncError,
  healthState,
  libraryEligibilityOptions,
  libraryEmptyState,
  libraryItemsPath,
  libraryKindOptions,
  libraryTypeLabel,
  LIBRARY_PAGE_SIZE,
  relativeTime,
  scopedInstances,
  type LibraryEligibility,
  type LibraryEmptyState,
  type LibraryHealthInstance,
  type LibraryHealthResponse,
  type LibraryItem,
  type LibraryItemsResponse,
  type LibraryKind,
} from '../lib/library'

/** One instance's last-sync state; a disabled connection stays visible but muted. */
function HealthCard({ instance }: { instance: LibraryHealthInstance }) {
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
    </div>
  )
}

function HealthStrip({
  health,
  error,
}: {
  health: LibraryHealthResponse | undefined
  error: Error | null
}) {
  if (error) return <Alert>Could not read arr sync health: {error.message}</Alert>
  if (!health) return <Spinner label="Loading sync health" />

  return (
    <section aria-label="Arr sync health" className="mb-6">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-[11px] uppercase tracking-wider text-zinc-500">Sync health</h2>
        <p className="text-xs text-zinc-400">
          {health.cache_items} {health.cache_items === 1 ? 'title' : 'titles'} cached
        </p>
      </div>
      {health.instances.length === 0 ? (
        <p className="rounded-xl border border-ink-800 bg-ink-900 px-4 py-3 text-sm text-zinc-400">
          No Radarr or Sonarr connection is configured. Add one under Settings to start syncing.
        </p>
      ) : (
        <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-3">
          {health.instances.map((instance) => (
            <HealthCard key={`${instance.kind}-${instance.name}`} instance={instance} />
          ))}
        </div>
      )}
    </section>
  )
}

/** Copy for the four mutually exclusive empty states. */
function EmptyState({
  state,
  instances,
  kind,
  onClearFilters,
}: {
  state: LibraryEmptyState
  instances: LibraryHealthInstance[]
  kind: LibraryKind | ''
  onClearFilters: () => void
}) {
  if (state === 'filtered_nothing') {
    return (
      <div className="py-8 text-center">
        <p className="text-sm font-medium text-zinc-200">No titles match these filters</p>
        <p className="mt-1 text-sm text-zinc-500">
          Widen the search or drop a filter to see the rest of the library.
        </p>
        <div className="mt-4 flex justify-center">
          <Button variant="secondary" size="sm" onClick={onClearFilters}>
            Clear filters
          </Button>
        </div>
      </div>
    )
  }

  if (state === 'sync_failed') {
    // The detail must name the instance that is failing in the current scope,
    // so another kind's error cannot be shown under a `kind=` filter.
    const detail = firstSyncError(scopedInstances(instances, kind))
    return (
      <div className="py-8 text-center">
        <p className="text-sm font-medium text-zinc-200">The last sync failed</p>
        <p className="mt-1 text-sm text-zinc-500">
          {detail || 'Check the Radarr and Sonarr connections under Settings and run a sync again.'}
        </p>
      </div>
    )
  }

  if (state === 'empty_library') {
    return (
      <div className="py-8 text-center">
        <p className="text-sm font-medium text-zinc-200">Nothing in the library yet</p>
        <p className="mt-1 text-sm text-zinc-500">
          The last sync succeeded but returned no titles.
        </p>
      </div>
    )
  }

  return (
    <div className="py-8 text-center">
      <p className="text-sm font-medium text-zinc-200">No sync has run yet</p>
      <p className="mt-1 text-sm text-zinc-500">
        {instances.length === 0
          ? 'No Radarr or Sonarr connection is configured. Add one under Settings, then run a sync.'
          : 'Dubarr has not polled Radarr or Sonarr. Run a sync, or check the connections under Settings.'}
      </p>
    </div>
  )
}

function ItemRow({ item }: { item: LibraryItem }) {
  const target = basename(item.target_file_path)

  return (
    <tr className="border-b border-ink-800/70 last:border-0">
      <td className="px-4 py-3 text-sm font-medium text-zinc-100">{item.title}</td>
      <td className="px-4 py-3 text-sm text-zinc-400">{item.year || '—'}</td>
      <td className="whitespace-nowrap px-4 py-3 text-xs text-zinc-400">
        {libraryTypeLabel(item)}
      </td>
      <td className="px-4 py-3">
        <Badge tone={eligibilityTone(item.eligibility_status)}>
          {eligibilityLabel(item.eligibility_status)}
        </Badge>
      </td>
      <td className="px-4 py-3">
        <span
          className="font-mono text-xs text-zinc-300"
          title={item.target_file_path || 'No target file'}
        >
          {target || '—'}
        </span>
      </td>
      <td className="px-4 py-3 text-xs text-zinc-400">{item.instance_name}</td>
    </tr>
  )
}

export function LibraryPage() {
  const [search, setSearch] = useState('')
  const debouncedSearch = useDebouncedValue(search, 250)
  const [kind, setKind] = useState<LibraryKind | ''>('')
  const [eligibility, setEligibility] = useState<LibraryEligibility | ''>('')
  const [offset, setOffset] = useState(0)

  const query = debouncedSearch.trim()
  const filtersActive = query !== '' || kind !== '' || eligibility !== ''

  // Changing any filter returns to the first page. Deriving it during render
  // avoids the extra request a post-commit effect would fire with the old offset.
  const filterKey = [query, kind, eligibility].join('\u0000')
  const [appliedFilterKey, setAppliedFilterKey] = useState(filterKey)
  if (appliedFilterKey !== filterKey) {
    setAppliedFilterKey(filterKey)
    setOffset(0)
  }
  const effectiveOffset = appliedFilterKey === filterKey ? offset : 0

  const itemsPath = libraryItemsPath({ q: query, kind, eligibility, offset: effectiveOffset })

  const health = useQuery<LibraryHealthResponse, Error>({
    queryKey: ['library', 'health'],
    queryFn: fetchLibraryHealth,
    retry: false,
    staleTime: 10_000,
  })

  const items = useQuery<LibraryItemsResponse, Error>({
    queryKey: ['library', 'items', itemsPath],
    queryFn: () => fetchLibraryItems({ q: query, kind, eligibility, offset: effectiveOffset }),
    retry: false,
    staleTime: 5_000,
  })

  const rows = items.data?.items ?? []
  const total = items.data?.total ?? 0
  const instances = health.data?.instances ?? []
  const emptyState =
    items.data && health.data
      ? libraryEmptyState({
          instances,
          cacheItems: health.data.cache_items,
          kind,
          filtersActive,
          total,
        })
      : null

  const clearFilters = () => {
    setSearch('')
    setKind('')
    setEligibility('')
  }

  const first = effectiveOffset + 1
  const last = effectiveOffset + rows.length

  return (
    <div>
      <PageHeader
        title="Library"
        description="Browse titles, inspect tracks and start a sync job."
      />

      <HealthStrip health={health.data} error={health.error ?? null} />

      <div className="mb-4 flex flex-wrap items-end gap-3">
        <div className="min-w-56 flex-1">
          <label htmlFor="library-search" className="block text-xs font-medium text-zinc-400">
            Search titles
          </label>
          <Input
            id="library-search"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
            placeholder="Title…"
            autoComplete="off"
            className="mt-1"
          />
        </div>
        <div className="w-40">
          <label htmlFor="library-kind" className="block text-xs font-medium text-zinc-400">
            Kind
          </label>
          <Select
            id="library-kind"
            className="mt-1"
            value={kind}
            onChange={(event) => setKind(event.target.value as LibraryKind | '')}
          >
            {libraryKindOptions.map((option) => (
              <option key={option.value} value={option.value}>
                {option.label}
              </option>
            ))}
          </Select>
        </div>
        <div className="w-48">
          <label htmlFor="library-eligibility" className="block text-xs font-medium text-zinc-400">
            Eligibility
          </label>
          <Select
            id="library-eligibility"
            className="mt-1"
            value={eligibility}
            onChange={(event) => setEligibility(event.target.value as LibraryEligibility | '')}
          >
            {libraryEligibilityOptions.map((option) => (
              <option key={option.value} value={option.value}>
                {option.label}
              </option>
            ))}
          </Select>
        </div>
        <Button variant="ghost" size="sm" onClick={clearFilters} disabled={!filtersActive}>
          Clear
        </Button>
      </div>

      <Card className="overflow-hidden">
        {items.isLoading && <Spinner label="Loading library titles" />}
        {items.error && <Alert>Could not load the library: {items.error.message}</Alert>}

        {!items.isLoading && !items.error && rows.length === 0 && emptyState && (
          <EmptyState
            state={emptyState}
            instances={instances}
            kind={kind}
            onClearFilters={clearFilters}
          />
        )}

        {rows.length > 0 && (
          <div className="-mx-5 -mb-5 overflow-x-auto">
            <table className="w-full min-w-[48rem] text-left" aria-label="Library titles">
              <thead>
                <tr className="border-b border-ink-800 text-[11px] uppercase tracking-wider text-zinc-500">
                  <th scope="col" className="px-4 pb-2 font-medium">
                    Title
                  </th>
                  <th scope="col" className="px-4 pb-2 font-medium">
                    Year
                  </th>
                  <th scope="col" className="px-4 pb-2 font-medium">
                    Type
                  </th>
                  <th scope="col" className="px-4 pb-2 font-medium">
                    Eligibility
                  </th>
                  <th scope="col" className="px-4 pb-2 font-medium">
                    Target file
                  </th>
                  <th scope="col" className="px-4 pb-2 font-medium">
                    Instance
                  </th>
                </tr>
              </thead>
              <tbody>
                {rows.map((item) => (
                  <ItemRow key={item.id} item={item} />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      {total > 0 && (
        <div className="mt-4 flex flex-wrap items-center justify-between gap-2">
          <p className="text-xs text-zinc-500">
            Showing {first}-{last} of {total}
          </p>
          <div className="flex items-center gap-2">
            <Button
              variant="secondary"
              size="sm"
              disabled={effectiveOffset === 0 || items.isFetching}
              onClick={() => setOffset(Math.max(0, effectiveOffset - LIBRARY_PAGE_SIZE))}
            >
              Previous
            </Button>
            <Button
              variant="secondary"
              size="sm"
              disabled={last >= total || items.isFetching}
              onClick={() => setOffset(effectiveOffset + LIBRARY_PAGE_SIZE)}
            >
              Next
            </Button>
          </div>
        </div>
      )}
    </div>
  )
}
