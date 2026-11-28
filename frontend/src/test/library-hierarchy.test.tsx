import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { EligibilityBadge } from '../components/library/EligibilityBadge'
import { LibraryPage } from '../pages/Library'
import type {
  LibraryEpisodeResource,
  LibrarySeasonResource,
  LibrarySeriesResource,
} from '../lib/api'
import type { LibraryHealthInstance, LibraryItem } from '../lib/library'
import { jsonResponse, renderWithProviders } from './helpers'

// --- fixtures ---------------------------------------------------------------

function instance(overrides: Partial<LibraryHealthInstance> = {}): LibraryHealthInstance {
  return {
    kind: 'sonarr',
    name: 'Sonarr',
    enabled: true,
    last_sync_at: null,
    last_attempt_at: null,
    last_ok: null,
    last_error: '',
    items_seen: 0,
    ...overrides,
  }
}

function movie(overrides: Partial<LibraryItem> = {}): LibraryItem {
  return {
    id: 1,
    media_type: 'movie',
    title: 'Arrival',
    year: 2016,
    season: null,
    episode: null,
    kind: 'radarr',
    instance_id: 1,
    instance_name: 'Radarr',
    eligibility_status: 'eligible',
    matches_target_tags: true,
    root_folder_path: '/media/movies',
    target_file_path: '/media/movies/Arrival (2016)/Arrival.2016.mkv',
    last_seen_at: '2026-11-25 10:00:00',
    updated_at: '2026-11-25 09:30:00',
    ...overrides,
  }
}

function series(overrides: Partial<LibrarySeriesResource> = {}): LibrarySeriesResource {
  return {
    id: 7,
    instance_id: 2,
    instance: 'Sonarr',
    title: 'The Office',
    year: 2005,
    path: '/media/tv/The Office (2005)',
    season_count: 2,
    episode_count: 32,
    episode_file_count: 30,
    eligibility: 'eligible',
    updated_at: '2026-11-25 09:00:00',
    ...overrides,
  }
}

function season(number: number, episodes: number, files: number): LibrarySeasonResource {
  return {
    season_number: number,
    statistics: {
      episode_count: episodes,
      episode_file_count: files,
      percent_of_episodes: episodes === 0 ? 0 : (files / episodes) * 100,
    },
  }
}

function episode(overrides: Partial<LibraryEpisodeResource> = {}): LibraryEpisodeResource {
  return {
    id: 913,
    series_id: 7,
    arr_id: 318,
    season_number: 2,
    episode_number: 3,
    title: 'Office Olympics',
    has_file: true,
    target_file_path: '/media/tv/The Office/Season 02/The Office - S02E03.mkv',
    eligibility: 'eligible',
    ...overrides,
  }
}

// --- fetch mock -------------------------------------------------------------

type SeriesPage = {
  page: number
  pageSize: number
  total: number
  items: LibrarySeriesResource[]
}

type EpisodePage = {
  seasonNumber: number
  total: number
  truncated: boolean
  items: LibraryEpisodeResource[]
}

type Fixtures = {
  instances?: LibraryHealthInstance[]
  cacheItems?: number
  /** Films and shows in the health payload's header counts. */
  cacheMovies?: number
  cacheSeries?: number
  /** The flat Films list (GET /api/library/items). */
  movies?: LibraryItem[]
  series?: (url: URL) => SeriesPage
  /** Seasons keyed by series id. */
  seasons?: Record<string, LibrarySeasonResource[]>
  episodes?: (url: URL) => EpisodePage
}

const SERIES_SEASONS = /^\/api\/library\/series\/(\d+)\/seasons$/
const SEASON_EPISODES = /^\/api\/library\/series\/(\d+)\/seasons\/(\d+)\/episodes$/

function mockLibrary(fixtures: Fixtures) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/library/health') {
      return jsonResponse({
        instances: fixtures.instances ?? [],
        cache_items: fixtures.cacheItems ?? 0,
        cache_movies: fixtures.cacheMovies ?? 0,
        cache_series: fixtures.cacheSeries ?? 0,
      })
    }
    if (url.pathname === '/api/library/items') {
      const items = fixtures.movies ?? []
      return jsonResponse({ items, total: items.length, limit: 50, offset: 0 })
    }
    if (url.pathname === '/api/library/series') {
      return jsonResponse(
        fixtures.series?.(url) ?? { page: 1, pageSize: 50, total: 0, items: [] },
      )
    }
    const seasonsMatch = SERIES_SEASONS.exec(url.pathname)
    if (seasonsMatch) {
      return jsonResponse({ items: fixtures.seasons?.[seasonsMatch[1]] ?? [] })
    }
    const episodesMatch = SEASON_EPISODES.exec(url.pathname)
    if (episodesMatch) {
      return jsonResponse(
        fixtures.episodes?.(url) ?? {
          seasonNumber: Number(episodesMatch[2]),
          total: 0,
          truncated: false,
          items: [],
        },
      )
    }
    return jsonResponse({ error: 'unexpected', code: 'not_found' }, 404)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function fetchedUrls(fetchMock: ReturnType<typeof mockLibrary>): URL[] {
  // fetch's call tuple is typed as a one-element rest in some DOM lib versions,
  // so the cast goes through unknown rather than asserting a shape the compiler
  // can prove wrong.
  const calls = fetchMock.mock.calls as unknown as Array<[RequestInfo | URL, RequestInit | undefined]>
  return calls.map(([input]) => new URL(String(input), 'http://localhost'))
}

function callsTo(fetchMock: ReturnType<typeof mockLibrary>, pathname: string): URL[] {
  return fetchedUrls(fetchMock).filter((url) => url.pathname === pathname)
}

function episodeCalls(fetchMock: ReturnType<typeof mockLibrary>): URL[] {
  return fetchedUrls(fetchMock).filter((url) => SEASON_EPISODES.test(url.pathname))
}

/**
 * Opens the page and switches to the TV shows tab (the shell starts on Films).
 */
async function openTvShows(user: ReturnType<typeof userEvent.setup>) {
  renderWithProviders(<LibraryPage />, { route: '/library' })
  await user.click(await screen.findByRole('tab', { name: 'TV shows' }))
}

describe('library tabs (#56)', () => {
  it('keeps the flat Films list when switching to TV shows and back', async () => {
    const user = userEvent.setup()
    mockLibrary({
      instances: [instance({ last_ok: true, items_seen: 1 })],
      cacheItems: 1,
      movies: [movie()],
      series: () => ({ page: 1, pageSize: 50, total: 1, items: [series()] }),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    // The flat library is the Films tab, unchanged.
    expect(await screen.findByRole('table', { name: 'Library titles' })).toBeInTheDocument()
    expect(screen.getByText('Arrival')).toBeInTheDocument()
    expect(screen.getByRole('tab', { name: 'Movies' })).toHaveAttribute('aria-selected', 'true')

    await user.click(screen.getByRole('tab', { name: 'TV shows' }))
    expect(await screen.findByRole('table', { name: 'Library TV shows' })).toBeInTheDocument()
    expect(screen.queryByRole('table', { name: 'Library titles' })).not.toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: 'Movies' }))
    expect(await screen.findByRole('table', { name: 'Library titles' })).toBeInTheDocument()
    expect(screen.getByText('Arrival')).toBeInTheDocument()
  })
})

describe('library series drill-down (#56)', () => {
  it('opens a series into seasons and a season into episodes, one request per level', async () => {
    const user = userEvent.setup()
    const fetchMock = mockLibrary({
      series: () => ({ page: 1, pageSize: 50, total: 1, items: [series()] }),
      seasons: { '7': [season(1, 10, 10), season(2, 22, 20)] },
      episodes: () => ({ seasonNumber: 2, total: 22, truncated: false, items: [episode()] }),
    })
    await openTvShows(user)

    await user.click(await screen.findByRole('button', { name: /The Office/ }))

    // Expanding a series asks for its seasons only — never for its episodes.
    expect(
      await screen.findByRole('button', { name: /Season 2 — 20 of 22 episodes/ }),
    ).toBeInTheDocument()
    expect(callsTo(fetchMock, '/api/library/series/7/seasons')).toHaveLength(1)
    expect(episodeCalls(fetchMock)).toHaveLength(0)

    await user.click(screen.getByRole('button', { name: /Season 2/ }))

    const table = await screen.findByRole('table', { name: 'Episodes of season 2' })
    expect(within(table).getByText('S02E03')).toBeInTheDocument()
    expect(within(table).getByText('Office Olympics')).toBeInTheDocument()
    expect(within(table).getByText('Eligible')).toBeInTheDocument()
    expect(episodeCalls(fetchMock)).toHaveLength(1)
  })

  it('says a series with no cached episodes has none yet, not that it failed', async () => {
    const user = userEvent.setup()
    mockLibrary({
      series: () => ({
        page: 1,
        pageSize: 50,
        total: 1,
        items: [series({ season_count: 0, episode_count: 0, episode_file_count: 0 })],
      }),
      seasons: { '7': [] },
    })
    await openTvShows(user)

    await user.click(await screen.findByRole('button', { name: /The Office/ }))
    expect(
      await screen.findByText('This series has no episodes in the library cache yet.'),
    ).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('pages the series list on the server and page two shows the next titles', async () => {
    const user = userEvent.setup()
    const total = 120
    const fetchMock = mockLibrary({
      series: (url) => {
        const page = Number(url.searchParams.get('page') ?? 1)
        const pageSize = Number(url.searchParams.get('pageSize') ?? 50)
        const start = (page - 1) * pageSize
        return {
          page,
          pageSize,
          total,
          items: Array.from({ length: Math.max(0, Math.min(pageSize, total - start)) }, (_, i) =>
            series({ id: start + i + 1, title: `Series ${start + i + 1}` }),
          ),
        }
      },
    })
    await openTvShows(user)

    expect(await screen.findByText('Showing 1-50 of 120')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Next' }))

    await waitFor(() =>
      expect(callsTo(fetchMock, '/api/library/series').some((url) => url.searchParams.get('page') === '2')).toBe(
        true,
      ),
    )
    expect(await screen.findByText('Showing 51-100 of 120')).toBeInTheDocument()
    expect(await screen.findByRole('button', { name: /Series 51/ })).toBeInTheDocument()
  })

  it('sends the typed search to the server and returns to the first page', async () => {
    const user = userEvent.setup()
    const fetchMock = mockLibrary({
      series: (url) => ({
        page: 1,
        pageSize: 50,
        total: 1,
        items:
          url.searchParams.get('q') === 'offi'
            ? [series()]
            : [series({ id: 3, title: 'Andor' })],
      }),
    })
    await openTvShows(user)

    expect(await screen.findByRole('button', { name: /Andor/ })).toBeInTheDocument()
    await user.type(await screen.findByLabelText('Search titles'), 'offi')

    await waitFor(
      () =>
        expect(
          callsTo(fetchMock, '/api/library/series').some(
            (url) => url.searchParams.get('q') === 'offi' && url.searchParams.get('page') === '1',
          ),
        ).toBe(true),
      { timeout: 3000 },
    )
    expect(await screen.findByRole('button', { name: /The Office/ })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Andor/ })).not.toBeInTheDocument()
  })

  it('pages a long season and says it is only one page at a time', async () => {
    const user = userEvent.setup()
    const total = 2001
    const fetchMock = mockLibrary({
      series: () => ({ page: 1, pageSize: 50, total: 1, items: [series()] }),
      seasons: { '7': [season(2, total, total)] },
      episodes: (url) => {
        const page = Number(url.searchParams.get('page') ?? 1)
        return {
          seasonNumber: 2,
          total,
          truncated: true,
          items: [
            episode({
              id: page,
              episode_number: page === 1 ? 1 : 201,
              title: page === 1 ? 'Pilot' : 'Later episode',
            }),
          ],
        }
      },
    })
    await openTvShows(user)

    await user.click(await screen.findByRole('button', { name: /The Office/ }))
    await user.click(await screen.findByRole('button', { name: /Season 2/ }))

    // The cap is reported, never silent — and as a notice, not a failure.
    expect(await screen.findByText(/more than Dubarr lists at once/)).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Next' }))
    await waitFor(() =>
      expect(
        episodeCalls(fetchMock).some((url) => url.searchParams.get('page') === '2'),
      ).toBe(true),
    )
    expect(await screen.findByText('Later episode')).toBeInTheDocument()
  })

  it('reads an empty library as nothing here yet, not as an error', async () => {
    const user = userEvent.setup()
    mockLibrary({
      instances: [instance({ last_ok: true })],
      series: () => ({ page: 1, pageSize: 50, total: 0, items: [] }),
    })
    await openTvShows(user)

    expect(await screen.findByText('Nothing here yet')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.queryByText(/could not/i)).not.toBeInTheDocument()
  })
})

describe('eligibility badge (#56)', () => {
  it('uses the three verdicts the queue page already uses', () => {
    render(
      <div>
        <EligibilityBadge status="eligible" />
        <EligibilityBadge status="waiting_for_tags" />
        <EligibilityBadge status="missing_target" />
      </div>,
    )

    expect(screen.getByText('Eligible')).toBeInTheDocument()
    expect(screen.getByText('Waiting for tags')).toBeInTheDocument()
    expect(screen.getByText('Missing target')).toBeInTheDocument()
  })

  it('shows a dash when the server sent no verdict instead of guessing one', () => {
    render(<EligibilityBadge />)
    expect(screen.getByText('—')).toBeInTheDocument()
  })
})
