import { Fragment, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Alert, Button, Card, Input, Select, Spinner } from '../ui'
import { cn } from '../../lib/cn'
import { useDebouncedValue } from '../../lib/hooks'
import { basename } from '../../lib/library'
import { api } from '../../lib/api'
import { EligibilityBadge } from './EligibilityBadge'

/** Page sizes the TV shows tab offers; the route clamps to the same maximum. */
const PAGE_SIZES = [25, 50, 100, 200]
const SERIES_PAGE_SIZE = 50
/** The episode endpoint's own default page size. */
const EPISODE_PAGE_SIZE = 200

/** `S02E03`, the same code the Films tab shows for an episode row. */
function episodeCode(season: number, episode: number): string {
  const pad = (value: number) => String(value).padStart(2, '0')
  return `S${pad(season)}E${pad(episode)}`
}

/**
 * One season's episodes, one page at a time (#56). A season is fetched when it
 * is opened, never with the series list, and a season too long for one payload
 * says so above the table instead of quietly dropping episodes.
 */
function EpisodesPanel({ seriesId, seasonNumber }: { seriesId: number; seasonNumber: number }) {
  const [page, setPage] = useState(1)

  const episodes = useQuery({
    queryKey: ['library', 'tv', 'episodes', seriesId, seasonNumber, page],
    queryFn: () =>
      api.librarySeasonEpisodes(seriesId, seasonNumber, { page, pageSize: EPISODE_PAGE_SIZE }),
    retry: false,
    staleTime: 30_000,
  })

  const rows = episodes.data?.items ?? []
  const total = episodes.data?.total ?? 0
  const first = (page - 1) * EPISODE_PAGE_SIZE + 1
  const last = first + rows.length - 1

  return (
    <div className="mt-2 border-l-2 border-ink-700 pl-3">
      {episodes.isLoading && <Spinner label="Loading episodes" />}
      {episodes.error && (
        <Alert>Could not load this season's episodes: {episodes.error.message}</Alert>
      )}

      {!episodes.isLoading && !episodes.error && episodes.data && rows.length === 0 && (
        <p className="text-sm text-zinc-400">No episodes are listed for this season yet.</p>
      )}

      {rows.length > 0 && (
        <>
          {episodes.data?.truncated && (
            <p role="status" className="mb-2 text-xs text-amber-300">
              This season has {total.toLocaleString()} episodes, more than Dubarr lists at once.
              The table below is one page — use Next to keep going.
            </p>
          )}
          <table
            className="w-full min-w-[36rem] text-left"
            aria-label={`Episodes of season ${seasonNumber}`}
          >
            <thead>
              <tr className="border-b border-ink-800 text-[11px] uppercase tracking-wider text-zinc-500">
                <th scope="col" className="px-3 pb-1 font-medium">
                  Episode
                </th>
                <th scope="col" className="px-3 pb-1 font-medium">
                  Title
                </th>
                <th scope="col" className="px-3 pb-1 font-medium">
                  File
                </th>
                <th scope="col" className="px-3 pb-1 font-medium">
                  Eligibility
                </th>
              </tr>
            </thead>
            <tbody>
              {rows.map((episode) => (
                <tr key={episode.id} className="border-b border-ink-800/70 last:border-0">
                  <td className="whitespace-nowrap px-3 py-2 text-xs text-zinc-400">
                    {episodeCode(
                      episode.season_number ?? seasonNumber,
                      episode.episode_number ?? 0,
                    )}
                  </td>
                  <td className="px-3 py-2 text-sm text-zinc-100">{episode.title || '—'}</td>
                  <td className="px-3 py-2">
                    {episode.has_file ? (
                      <span
                        className="font-mono text-xs text-zinc-300"
                        title={episode.target_file_path || 'No target file'}
                      >
                        {basename(episode.target_file_path ?? '') || '—'}
                      </span>
                    ) : (
                      <span className="text-xs text-zinc-500">No file yet</span>
                    )}
                  </td>
                  <td className="px-3 py-2">
                    <EligibilityBadge status={episode.eligibility} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {total > rows.length && (
            <div className="mt-2 flex flex-wrap items-center justify-between gap-2">
              <p className="text-xs text-zinc-500">
                Showing {first}-{last} of {total}
              </p>
              <div className="flex items-center gap-2">
                <Button
                  variant="secondary"
                  size="sm"
                  disabled={page === 1 || episodes.isFetching}
                  onClick={() => setPage((current) => Math.max(1, current - 1))}
                >
                  Previous
                </Button>
                <Button
                  variant="secondary"
                  size="sm"
                  disabled={last >= total || episodes.isFetching}
                  onClick={() => setPage((current) => current + 1)}
                >
                  Next
                </Button>
              </div>
            </div>
          )}
        </>
      )}
    </div>
  )
}

/**
 * The seasons of one series, fetched when the row is opened and cached by
 * react-query per series id, so collapsing and reopening costs no request.
 */
function SeasonsPanel({ seriesId }: { seriesId: number }) {
  const [openSeason, setOpenSeason] = useState<number | null>(null)

  const seasons = useQuery({
    queryKey: ['library', 'tv', 'seasons', seriesId],
    queryFn: () => api.librarySeriesSeasons(seriesId),
    retry: false,
    staleTime: 30_000,
  })

  const rows = seasons.data?.items ?? []

  return (
    <div className="space-y-2">
      <p className="text-[11px] uppercase tracking-wider text-zinc-500">Seasons</p>
      {seasons.isLoading && <Spinner label="Loading seasons" />}
      {seasons.error && (
        <Alert>Could not load this series' seasons: {seasons.error.message}</Alert>
      )}
      {!seasons.isLoading && !seasons.error && rows.length === 0 && (
        <p className="text-sm text-zinc-400">This series has no episodes in the library cache yet.</p>
      )}
      {rows.map((row) => {
        const open = openSeason === row.season_number
        const fileCount = row.statistics?.episode_file_count ?? 0
        const episodeCount = row.statistics?.episode_count ?? 0
        return (
          <div key={row.season_number}>
            <button
              type="button"
              aria-expanded={open}
              onClick={() => setOpenSeason(open ? null : row.season_number)}
              title="Episodes with a file that Dubarr can see. Open the season to see each episode."
              className={cn(
                'inline-flex items-center gap-1.5 rounded-lg border px-3 py-1.5 text-sm transition-colors',
                open
                  ? 'border-accent-500/40 bg-accent-500/10 text-accent-300'
                  : 'border-ink-600 bg-ink-800 text-zinc-200 hover:bg-ink-700',
              )}
            >
              <span aria-hidden="true">{open ? '▾' : '▸'}</span>
              Season {row.season_number} — {fileCount} of {episodeCount} episodes
            </button>
            {open && <EpisodesPanel seriesId={seriesId} seasonNumber={row.season_number} />}
          </div>
        )
      })}
    </div>
  )
}

/**
 * The TV shows tab (#56): one row per Sonarr series, expanded into its seasons,
 * and a season into its episodes. Every level is its own request, paging is the
 * server's, and nothing is grouped in the browser — which is what keeps a
 * 12 000-episode library inside one screen's worth of data.
 */
export function SeriesTab() {
  const [search, setSearch] = useState('')
  const debouncedSearch = useDebouncedValue(search, 250)
  const [pageSize, setPageSize] = useState(SERIES_PAGE_SIZE)
  const [page, setPage] = useState(1)
  const [openSeries, setOpenSeries] = useState<number | null>(null)

  const query = debouncedSearch.trim()

  // A new search or page size returns to the first page, derived during render
  // so the stale page never reaches the server.
  const filterKey = [query, pageSize].join('\u0000')
  const [appliedFilterKey, setAppliedFilterKey] = useState(filterKey)
  if (appliedFilterKey !== filterKey) {
    setAppliedFilterKey(filterKey)
    setPage(1)
  }
  const effectivePage = appliedFilterKey === filterKey ? page : 1

  const series = useQuery({
    queryKey: ['library', 'tv', 'list', query, effectivePage, pageSize],
    queryFn: () => api.librarySeries({ q: query, page: effectivePage, pageSize }),
    retry: false,
    staleTime: 5_000,
  })

  const rows = series.data?.items ?? []
  const total = series.data?.total ?? 0
  const first = (effectivePage - 1) * pageSize + 1
  const last = first + rows.length - 1

  return (
    <div>
      <div className="mb-4 flex flex-wrap items-end gap-3">
        <div className="min-w-56 flex-1">
          <label htmlFor="series-search" className="block text-xs font-medium text-zinc-400">
            Search titles
          </label>
          <Input
            id="series-search"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
            placeholder="Series title…"
            autoComplete="off"
            className="mt-1"
            title="Filters on the server, across every title in the library. It matches part of a name, so ‘offi’ finds The Office. Cost: none, results update as you type."
          />
        </div>
        <div className="w-36">
          <label htmlFor="series-page-size" className="block text-xs font-medium text-zinc-400">
            Rows per page
          </label>
          <Select
            id="series-page-size"
            className="mt-1"
            value={String(pageSize)}
            onChange={(event) => setPageSize(Number(event.target.value))}
            title="How many titles one page loads. Larger pages mean fewer clicks and a heavier first load. The maximum is 200."
          >
            {PAGE_SIZES.map((size) => (
              <option key={size} value={size}>
                {size}
              </option>
            ))}
          </Select>
        </div>
        <Button
          variant="ghost"
          size="sm"
          onClick={() => setSearch('')}
          disabled={search === ''}
        >
          Clear
        </Button>
      </div>

      <Card className="overflow-hidden">
        {series.isLoading && <Spinner label="Loading TV shows" />}
        {series.error && <Alert>Could not load the TV shows: {series.error.message}</Alert>}

        {!series.isLoading && !series.error && rows.length === 0 && (
          <div className="py-8 text-center">
            {query ? (
              <>
                <p className="text-sm font-medium text-zinc-200">No TV shows match this search.</p>
                <p className="mt-1 text-sm text-zinc-500">
                  Try a shorter part of the name, or clear the search to see every series.
                </p>
                <div className="mt-4 flex justify-center">
                  <Button variant="secondary" size="sm" onClick={() => setSearch('')}>
                    Clear search
                  </Button>
                </div>
              </>
            ) : (
              <>
                <p className="text-sm font-medium text-zinc-200">Nothing here yet</p>
                <p className="mt-1 text-sm text-zinc-500">
                  No TV shows have been synced from Sonarr yet. Add a Sonarr connection under
                  Settings and run a sync, and the series will appear here.
                </p>
              </>
            )}
          </div>
        )}

        {rows.length > 0 && (
          <div className="-mx-5 -mb-5 overflow-x-auto">
            <table className="w-full min-w-[44rem] text-left" aria-label="Library TV shows">
              <thead>
                <tr className="border-b border-ink-800 text-[11px] uppercase tracking-wider text-zinc-500">
                  <th scope="col" className="px-4 pb-2 font-medium">
                    Title
                  </th>
                  <th scope="col" className="px-4 pb-2 font-medium">
                    Year
                  </th>
                  <th scope="col" className="px-4 pb-2 font-medium">
                    Seasons
                  </th>
                  <th scope="col" className="px-4 pb-2 font-medium">
                    Episodes with a file
                  </th>
                  <th scope="col" className="px-4 pb-2 font-medium">
                    Eligibility
                  </th>
                  <th scope="col" className="px-4 pb-2 font-medium">
                    Instance
                  </th>
                </tr>
              </thead>
              <tbody>
                {rows.map((row) => {
                  const open = openSeries === row.id
                  return (
                    <Fragment key={row.id}>
                      <tr className="border-b border-ink-800/70">
                        <td className="px-4 py-3">
                          <button
                            type="button"
                            aria-expanded={open}
                            onClick={() => setOpenSeries(open ? null : row.id)}
                            title="Open this series to see its seasons."
                            className="inline-flex items-center gap-1.5 text-left text-sm font-medium text-zinc-100 hover:text-accent-300"
                          >
                            <span aria-hidden="true">{open ? '▾' : '▸'}</span>
                            {row.title}
                          </button>
                        </td>
                        <td className="px-4 py-3 text-sm text-zinc-400">{row.year || '—'}</td>
                        <td className="px-4 py-3 text-sm text-zinc-400">{row.season_count ?? 0}</td>
                        <td className="px-4 py-3 text-sm text-zinc-400">
                          {row.episode_file_count ?? 0} of {row.episode_count ?? 0}
                        </td>
                        <td className="px-4 py-3">
                          <EligibilityBadge status={row.eligibility} />
                        </td>
                        <td className="px-4 py-3 text-xs text-zinc-400">{row.instance || '—'}</td>
                      </tr>
                      {open && (
                        <tr className="border-b border-ink-800/70 bg-ink-950/40">
                          <td colSpan={6} className="px-4 py-3">
                            <SeasonsPanel seriesId={row.id} />
                          </td>
                        </tr>
                      )}
                    </Fragment>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      {total > rows.length && (
        <div className="mt-4 flex flex-wrap items-center justify-between gap-2">
          <p className="text-xs text-zinc-500">
            Showing {first}-{last} of {total}
          </p>
          <div className="flex items-center gap-2">
            <Button
              variant="secondary"
              size="sm"
              disabled={effectivePage === 1 || series.isFetching}
              onClick={() => setPage(Math.max(1, effectivePage - 1))}
            >
              Previous
            </Button>
            <Button
              variant="secondary"
              size="sm"
              disabled={last >= total || series.isFetching}
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
