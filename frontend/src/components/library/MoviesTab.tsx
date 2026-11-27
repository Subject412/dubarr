import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Button, Card, Input, Select, Spinner } from '../ui'
import { useDebouncedValue } from '../../lib/hooks'
import { EligibilityBadge } from './EligibilityBadge'
import {
  basename,
  fetchLibraryItems,
  firstSyncError,
  libraryEligibilityOptions,
  libraryEmptyState,
  libraryItemsPath,
  libraryKindOptions,
  libraryTypeLabel,
  LIBRARY_PAGE_SIZE,
  LibraryRescanError,
  rescanLibraryItem,
  scopedInstances,
  type LibraryEligibility,
  type LibraryEmptyState,
  type LibraryHealthInstance,
  type LibraryHealthResponse,
  type LibraryItem,
  type LibraryItemsResponse,
  type LibraryKind,
} from '../../lib/library'

/** Page sizes the Films tab offers; the flat route clamps to the same maximum. */
const PAGE_SIZES = [25, 50, 100, 200]

/**
 * Copy for the four mutually exclusive empty states of the flat list. `state`
 * comes from [libraryEmptyState], so a search that hides existing rows reads
 * differently from a library that has never been synced.
 */
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

function ItemRow({
  item,
  onRescan,
  rescanning,
  note,
}: {
  item: LibraryItem
  onRescan?: () => void
  rescanning?: boolean
  note?: string
}) {
  const target = basename(item.target_file_path)

  return (
    <tr className="border-b border-ink-800/70 last:border-0">
      <td className="px-4 py-3 text-sm font-medium text-zinc-100">{item.title}</td>
      <td className="px-4 py-3 text-sm text-zinc-400">{item.year || '—'}</td>
      <td className="whitespace-nowrap px-4 py-3 text-xs text-zinc-400">
        {libraryTypeLabel(item)}
      </td>
      <td className="px-4 py-3">
        <EligibilityBadge status={item.eligibility_status} />
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
      <td className="px-4 py-3">
        <Button variant="secondary" size="sm" disabled={rescanning} onClick={onRescan}>
          {rescanning ? 'Rescanning...' : 'Rescan'}
        </Button>
        {note && (
          <p role="status" className="mt-1 max-w-64 text-xs text-amber-300">
            {note}
          </p>
        )}
      </td>
    </tr>
  )
}

/**
 * The Films tab (#56): the flat library exactly as it behaved before the split,
 * still served by GET /api/library/items. Radarr has no levels, so this list is
 * the movie side of the tree; searching, filtering and paging all go to the
 * server, and a row can still ask arr to rescan itself.
 *
 * Health is read and refreshed by the page shell, which passes it down so the
 * empty state can tell "never synced" from "nothing matched" — and, while it is
 * still loading, shows no empty state at all rather than the wrong one.
 */
export function MoviesTab({ health }: { health: LibraryHealthResponse | undefined }) {
  const [search, setSearch] = useState('')
  const debouncedSearch = useDebouncedValue(search, 250)
  const [kind, setKind] = useState<LibraryKind | ''>('')
  const [eligibility, setEligibility] = useState<LibraryEligibility | ''>('')
  const [pageSize, setPageSize] = useState(LIBRARY_PAGE_SIZE)
  const [offset, setOffset] = useState(0)

  const query = debouncedSearch.trim()
  const filtersActive = query !== '' || kind !== '' || eligibility !== ''

  // Changing any filter returns to the first page. Deriving it during render
  // avoids the extra request a post-commit effect would fire with the old offset.
  const filterKey = [query, kind, eligibility, pageSize].join('\u0000')
  const [appliedFilterKey, setAppliedFilterKey] = useState(filterKey)
  if (appliedFilterKey !== filterKey) {
    setAppliedFilterKey(filterKey)
    setOffset(0)
  }
  const effectiveOffset = appliedFilterKey === filterKey ? offset : 0

  const itemsPath = libraryItemsPath({
    q: query,
    kind,
    eligibility,
    limit: pageSize,
    offset: effectiveOffset,
  })

  const items = useQuery<LibraryItemsResponse, Error>({
    queryKey: ['library', 'items', itemsPath],
    queryFn: () =>
      fetchLibraryItems({ q: query, kind, eligibility, limit: pageSize, offset: effectiveOffset }),
    retry: false,
    staleTime: 5_000,
  })

  const queryClient = useQueryClient()
  const mountedRef = useRef(true)
  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
    }
  }, [])

  // Per-row rescan: which rows are in flight, and the inline verdict per row.
  // Rows are independent — one slow instance must not block rescanning another.
  const [rescanning, setRescanning] = useState<ReadonlySet<number>>(() => new Set())
  const [rescanNotes, setRescanNotes] = useState<Record<number, string>>({})

  const startRescan = async (item: LibraryItem) => {
    if (rescanning.has(item.id)) return
    setRescanning((prev) => new Set(prev).add(item.id))
    setRescanNotes((prev) => {
      if (!(item.id in prev)) return prev
      const next = { ...prev }
      delete next[item.id]
      return next
    })
    try {
      const result = await rescanLibraryItem(item.id)
      // Accepted or not, what the page shows may now be stale: arr was asked to
      // look at the item again, so re-read both queries either way.
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['library', 'health'] }),
        queryClient.invalidateQueries({ queryKey: ['library', 'items'] }),
      ])
      if (!mountedRef.current) return
      if (result.reread === 'failed') {
        const cause = result.reread_error || 'unknown error'
        setRescanNotes((prev) => ({
          ...prev,
          [item.id]: `The rescan was accepted, but re-reading the item failed: ${cause}`,
        }))
      }
    } catch (error) {
      if (!mountedRef.current) return
      const message =
        error instanceof LibraryRescanError || error instanceof Error
          ? error.message
          : 'unknown error'
      setRescanNotes((prev) => ({ ...prev, [item.id]: `Could not rescan: ${message}` }))
    } finally {
      if (mountedRef.current) {
        setRescanning((prev) => {
          const next = new Set(prev)
          next.delete(item.id)
          return next
        })
      }
    }
  }

  const rows = items.data?.items ?? []
  const total = items.data?.total ?? 0
  const instances = health?.instances ?? []
  const emptyState =
    items.data && health
      ? libraryEmptyState({
          instances,
          cacheItems: health.cache_items,
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
        <div className="w-36">
          <label htmlFor="library-page-size" className="block text-xs font-medium text-zinc-400">
            Rows per page
          </label>
          <Select
            id="library-page-size"
            className="mt-1"
            value={String(pageSize)}
            onChange={(event) => setPageSize(Number(event.target.value))}
          >
            {PAGE_SIZES.map((size) => (
              <option key={size} value={size}>
                {size}
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
                  <th scope="col" className="px-4 pb-2 font-medium">
                    <span className="sr-only">Actions</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {rows.map((item) => (
                  <ItemRow
                    key={item.id}
                    item={item}
                    rescanning={rescanning.has(item.id)}
                    note={rescanNotes[item.id]}
                    onRescan={() => void startRescan(item)}
                  />
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
              onClick={() => setOffset(Math.max(0, effectiveOffset - pageSize))}
            >
              Previous
            </Button>
            <Button
              variant="secondary"
              size="sm"
              disabled={last >= total || items.isFetching}
              onClick={() => setOffset(effectiveOffset + pageSize)}
            >
              Next
            </Button>
          </div>
        </div>
      )}
    </div>
  )
}
