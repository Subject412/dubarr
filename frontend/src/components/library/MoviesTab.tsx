import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Badge, Button, Card, Input, Select, Spinner } from '../ui'
import { useDebouncedValue } from '../../lib/hooks'
import { api, type LibraryMovieResource } from '../../lib/api'
import { EligibilityBadge } from './EligibilityBadge'
import {
  basename,
  firstSyncError,
  libraryEligibilityOptions,
  libraryEmptyState,
  LIBRARY_PAGE_SIZE,
  LibraryRescanError,
  rescanLibraryItem,
  type LibraryEligibility,
  type LibraryEmptyState,
  type LibraryHealthInstance,
  type LibraryHealthResponse,
} from '../../lib/library'

/** Page sizes the Movies tab offers; the route clamps to the same maximum. */
const PAGE_SIZES = [25, 50, 100, 200]

/**
 * One Radarr movie as the table renders it. `id` is `arr_library_cache.id` —
 * the id the rescan route takes, never Radarr's own movieId.
 */
type MovieRow = {
  id: number
  title: string
  year: number
  originalLanguage: string
  audioLanguages: string[]
  monitored: boolean
  available: boolean
  eligibility: string
  targetFilePath: string
  instance: string
}

function movieRow(movie: LibraryMovieResource): MovieRow {
  return {
    id: movie.id,
    title: movie.title,
    year: movie.year ?? 0,
    originalLanguage: movie.original_language ?? '',
    audioLanguages: movie.audio_languages ?? [],
    monitored: movie.monitored ?? false,
    available: movie.available ?? false,
    eligibility: movie.eligibility ?? '',
    targetFilePath: movie.target_file_path ?? '',
    instance: movie.instance ?? '',
  }
}

/** `ja` / `da, en`, and a dash when arr reported nothing. */
function languagesText(codes: string[]): string {
  if (codes.length === 0) return '—'
  // A file with many tracks keeps the cell narrow: `ja, en, da +2`.
  if (codes.length <= 3) return codes.join(', ')
  return `${codes.slice(0, 3).join(', ')} +${codes.length - 3}`
}

/**
 * Empty states the list can show. `filtered_page` is local to this tab: the
 * Eligibility control narrows the page the server already returned, so an
 * empty page says nothing about the rest of the library.
 */
type MovieEmptyState = LibraryEmptyState | 'filtered_page'

/**
 * Copy for the mutually exclusive empty states of the movie list. `state`
 * comes from [libraryEmptyState] plus the page-local `filtered_page`, so a
 * search that hides existing rows reads differently from a library that has
 * never been synced — and a page emptied by Eligibility never claims the
 * library has no matches.
 */
function EmptyState({
  state,
  instances,
  onClearFilters,
}: {
  state: MovieEmptyState
  instances: LibraryHealthInstance[]
  onClearFilters: () => void
}) {
  if (state === 'filtered_page') {
    return (
      <div className="py-8 text-center">
        <p className="text-sm font-medium text-zinc-200">
          No titles on this page match these filters
        </p>
        <p className="mt-1 text-sm text-zinc-500">
          Eligibility narrows the titles loaded on this page only, so matches can sit on another
          page. Try the next page or clear the filter.
        </p>
        <div className="mt-4 flex justify-center">
          <Button variant="secondary" size="sm" onClick={onClearFilters}>
            Clear filters
          </Button>
        </div>
      </div>
    )
  }

  if (state === 'filtered_nothing') {
    return (
      <div className="py-8 text-center">
        <p className="text-sm font-medium text-zinc-200">No titles match this search</p>
        <p className="mt-1 text-sm text-zinc-500">
          Search runs over the whole library, so no title matches it. Clear the search to see the
          titles again.
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
    const detail = firstSyncError(instances)
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

/**
 * One movie row: what it is, what languages it carries, whether it can be
 * tagged, and where its file is. Monitored and available sit under the title as
 * compact badges — `available` means the mirror holds a file, which is the same
 * fact the target cell shows.
 */
function MovieTableRow({
  movie,
  onRescan,
  rescanning,
  note,
}: {
  movie: MovieRow
  onRescan?: () => void
  rescanning?: boolean
  note?: string
}) {
  const target = basename(movie.targetFilePath)
  const audioTitle =
    movie.audioLanguages.length > 3 ? movie.audioLanguages.join(', ') : undefined

  return (
    <tr className="border-b border-ink-800/70 last:border-0">
      <td className="px-4 py-3">
        <div className="text-sm font-medium text-zinc-100">{movie.title}</div>
        <div className="mt-1 flex flex-wrap gap-1">
          <span title="Whether Radarr is monitoring this movie.">
            <Badge tone={movie.monitored ? 'accent' : 'neutral'}>
              {movie.monitored ? 'Monitored' : 'Unmonitored'}
            </Badge>
          </span>
          <span title="Whether the mirror holds a file for this movie.">
            <Badge tone={movie.available ? 'success' : 'neutral'}>
              {movie.available ? 'Available' : 'No file'}
            </Badge>
          </span>
        </div>
      </td>
      <td className="px-4 py-3 text-sm text-zinc-400">{movie.year || '—'}</td>
      <td className="px-4 py-3 text-sm text-zinc-400">{movie.originalLanguage || '—'}</td>
      <td className="px-4 py-3 text-sm text-zinc-400" title={audioTitle}>
        {languagesText(movie.audioLanguages)}
      </td>
      <td className="px-4 py-3">
        <EligibilityBadge status={movie.eligibility} />
      </td>
      <td className="px-4 py-3">
        <span
          className="font-mono text-xs text-zinc-300"
          title={movie.targetFilePath || 'No target file'}
        >
          {target || '—'}
        </span>
      </td>
      <td className="px-4 py-3 text-xs text-zinc-400">{movie.instance || '—'}</td>
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
 * The Movies tab (#128): one row per Radarr movie, read from
 * GET /api/library/movies. That route returns movies only and carries the
 * language, monitored and available fields; the flat /api/library/items route
 * mixed in episode rows and had none of them, which is why this tab left it.
 *
 * Searching and paging go to the server. Health is read and refreshed by the
 * page shell, which passes it down so the empty state can tell "never synced"
 * from "nothing matched" — and, while it is still loading, shows no empty state
 * at all rather than the wrong one.
 */
export function MoviesTab({ health }: { health: LibraryHealthResponse | undefined }) {
  const [search, setSearch] = useState('')
  const debouncedSearch = useDebouncedValue(search, 250)
  const [eligibility, setEligibility] = useState<LibraryEligibility | ''>('')
  const [pageSize, setPageSize] = useState(LIBRARY_PAGE_SIZE)
  const [page, setPage] = useState(1)

  const query = debouncedSearch.trim()
  const filtersActive = query !== '' || eligibility !== ''

  // Changing the search, the verdict or the page size returns to the first
  // page. Deriving it during render avoids the extra request a post-commit
  // effect would fire with the stale page.
  const filterKey = [query, eligibility, pageSize].join('\u0000')
  const [appliedFilterKey, setAppliedFilterKey] = useState(filterKey)
  if (appliedFilterKey !== filterKey) {
    setAppliedFilterKey(filterKey)
    setPage(1)
  }
  const effectivePage = appliedFilterKey === filterKey ? page : 1

  const movies = useQuery({
    queryKey: ['library', 'movies', query, effectivePage, pageSize],
    queryFn: () => api.libraryMovies({ q: query, page: effectivePage, pageSize }),
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

  const startRescan = async (movie: MovieRow) => {
    if (rescanning.has(movie.id)) return
    setRescanning((prev) => new Set(prev).add(movie.id))
    setRescanNotes((prev) => {
      if (!(movie.id in prev)) return prev
      const next = { ...prev }
      delete next[movie.id]
      return next
    })
    try {
      const result = await rescanLibraryItem(movie.id)
      // Accepted or not, what the page shows may now be stale: arr was asked to
      // look at the movie again, so re-read both queries either way.
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['library', 'health'] }),
        queryClient.invalidateQueries({ queryKey: ['library', 'movies'] }),
      ])
      if (!mountedRef.current) return
      if (result.reread === 'failed') {
        const cause = result.reread_error || 'unknown error'
        setRescanNotes((prev) => ({
          ...prev,
          [movie.id]: `The rescan was accepted, but re-reading the item failed: ${cause}`,
        }))
      }
    } catch (error) {
      if (!mountedRef.current) return
      const message =
        error instanceof LibraryRescanError || error instanceof Error
          ? error.message
          : 'unknown error'
      setRescanNotes((prev) => ({ ...prev, [movie.id]: `Could not rescan: ${message}` }))
    } finally {
      if (mountedRef.current) {
        setRescanning((prev) => {
          const next = new Set(prev)
          next.delete(movie.id)
          return next
        })
      }
    }
  }

  const pageRows = (movies.data?.items ?? []).map(movieRow)
  // The movie route filters on q / instanceId / page / pageSize / sort / dir —
  // there is no verdict parameter, so the Eligibility control narrows the page
  // the server returned. The pager still counts titles.
  const rows =
    eligibility === '' ? pageRows : pageRows.filter((row) => row.eligibility === eligibility)
  const total = movies.data?.total ?? 0
  const instances = health?.instances ?? []
  // A page the verdict filter emptied is a page-local filter hit, not an empty
  // library: the matching titles may be on any other page.
  let emptyState: MovieEmptyState | null = null
  if (pageRows.length > 0 && rows.length === 0) {
    emptyState = 'filtered_page'
  } else if (movies.data && health) {
    emptyState = libraryEmptyState({
      instances,
      // Movies, not every cache row: 12 400 episodes must not make an empty
      // movie list look like a filter problem.
      cacheItems: health.cache_movies,
      filtersActive: query !== '',
      total,
    })
  }

  const clearFilters = () => {
    setSearch('')
    setEligibility('')
  }

  const first = (effectivePage - 1) * pageSize + 1
  const last = first + pageRows.length - 1

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
        <div className="w-48">
          <label htmlFor="library-eligibility" className="block text-xs font-medium text-zinc-400">
            Eligibility
          </label>
          <Select
            id="library-eligibility"
            className="mt-1"
            value={eligibility}
            onChange={(event) => setEligibility(event.target.value as LibraryEligibility | '')}
            title="Narrows the titles on this page by their Processing Gate verdict. Searching and paging stay server-side."
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
        {movies.isLoading && <Spinner label="Loading library titles" />}
        {movies.error && <Alert>Could not load the library: {movies.error.message}</Alert>}

        {!movies.isLoading && !movies.error && rows.length === 0 && emptyState && (
          <EmptyState state={emptyState} instances={instances} onClearFilters={clearFilters} />
        )}

        {rows.length > 0 && (
          <div className="-mx-5 -mb-5 overflow-x-auto">
            <table className="w-full min-w-[56rem] text-left" aria-label="Library titles">
              <thead>
                <tr className="border-b border-ink-800 text-[11px] uppercase tracking-wider text-zinc-500">
                  <th scope="col" className="px-4 pb-2 font-medium">
                    Title
                  </th>
                  <th scope="col" className="px-4 pb-2 font-medium">
                    Year
                  </th>
                  <th scope="col" className="px-4 pb-2 font-medium">
                    Original language
                  </th>
                  <th scope="col" className="px-4 pb-2 font-medium">
                    Audio languages
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
                {rows.map((movie) => (
                  <MovieTableRow
                    key={movie.id}
                    movie={movie}
                    rescanning={rescanning.has(movie.id)}
                    note={rescanNotes[movie.id]}
                    onRescan={() => void startRescan(movie)}
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
            {eligibility !== '' && pageRows.length > 0 && (
              <span>
                {' '}
                · Eligibility matches {rows.length} of these {pageRows.length} titles on this page
              </span>
            )}
          </p>
          <div className="flex items-center gap-2">
            <Button
              variant="secondary"
              size="sm"
              disabled={effectivePage === 1 || movies.isFetching}
              onClick={() => setPage(Math.max(1, effectivePage - 1))}
            >
              Previous
            </Button>
            <Button
              variant="secondary"
              size="sm"
              disabled={last >= total || movies.isFetching}
              onClick={() => setPage(effectivePage + 1)}
            >
              Next
            </Button>
          </div>
        </div>
      )}
    </div>
  )
}
