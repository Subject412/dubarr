import { cleanup, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { LibraryPage } from '../pages/Library'
import type { LibraryMovieResource } from '../lib/api'
import type { LibraryHealthInstance } from '../lib/library'
import { jsonResponse, renderWithProviders } from './helpers'

function instance(overrides: Partial<LibraryHealthInstance> = {}): LibraryHealthInstance {
  return {
    kind: 'radarr',
    name: 'Radarr',
    enabled: true,
    last_sync_at: null,
    last_attempt_at: null,
    last_ok: null,
    last_error: '',
    items_seen: 0,
    ...overrides,
  }
}

function movieResource(overrides: Partial<LibraryMovieResource> = {}): LibraryMovieResource {
  return {
    id: 1,
    arr_id: 318,
    instance_id: 1,
    instance: 'Radarr',
    title: 'Arrival',
    year: 2016,
    original_language: 'en',
    audio_languages: ['en'],
    monitored: true,
    available: true,
    path: '/media/movies/Arrival (2016)',
    target_file_path: '/media/movies/Arrival (2016)/Arrival.2016.mkv',
    has_file: true,
    eligibility: 'eligible',
    updated_at: '2026-11-25 09:30:00',
    ...overrides,
  }
}

type MovieHandler = (url: URL) => {
  page: number
  pageSize: number
  total: number
  items: LibraryMovieResource[]
}

function pageOf(items: LibraryMovieResource[], url: URL, total = items.length) {
  return {
    page: Number(url.searchParams.get('page') ?? 1),
    pageSize: Number(url.searchParams.get('pageSize') ?? 50),
    total,
    items,
  }
}

/** A server page of `total` movies, numbered so a page boundary is visible. */
function pagedMovies(url: URL, total: number) {
  const page = Number(url.searchParams.get('page') ?? 1)
  const pageSize = Number(url.searchParams.get('pageSize') ?? 50)
  const start = (page - 1) * pageSize
  return {
    page,
    pageSize,
    total,
    items: Array.from({ length: Math.max(0, Math.min(pageSize, total - start)) }, (_, i) =>
      movieResource({ id: start + i + 1, title: `Title ${start + i + 1}` }),
    ),
  }
}

/** Fetch mock for the library endpoints; anything else is a 404. */
function mockLibrary({
  instances,
  cacheItems = 0,
  cacheMovies = 0,
  cacheSeries = 0,
  movies,
  refresh,
  rescan,
}: {
  instances: LibraryHealthInstance[]
  cacheItems?: number
  /** Films and shows in the library; the header reads these, not cacheItems. */
  cacheMovies?: number
  cacheSeries?: number
  /** The Radarr list the Movies tab reads; `cacheMovies` is its total. */
  movies: MovieHandler
  /** POST /api/library/refresh response; defaults to an accepted pass. */
  refresh?: () => Response | Promise<Response>
  /** POST /api/library/items/{id}/rescan response; defaults to accepted + reread ok. */
  rescan?: (id: string) => Response | Promise<Response>
}) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/library/health') {
      return jsonResponse({
        instances,
        cache_items: cacheItems,
        cache_movies: cacheMovies,
        cache_series: cacheSeries,
      })
    }
    if (url.pathname === '/api/library/movies') {
      return jsonResponse(movies(url))
    }
    if (url.pathname === '/api/library/items') {
      // The legacy flat list is still served — and stubbed — although the
      // Movies tab no longer reads it; a regression back to it renders nothing.
      return jsonResponse({ items: [], total: 0, limit: 50, offset: 0 })
    }
    if (url.pathname === '/api/library/refresh' && (init?.method ?? 'GET').toUpperCase() === 'POST') {
      return refresh ? await refresh() : jsonResponse({ accepted: true, kind: 'all' }, 202)
    }
    const rescanMatch = /^\/api\/library\/items\/(\d+)\/rescan$/.exec(url.pathname)
    if (rescanMatch && (init?.method ?? 'GET').toUpperCase() === 'POST') {
      return rescan
        ? await rescan(rescanMatch[1])
        : jsonResponse({ accepted: true, reread: 'ok' }, 202)
    }
    return jsonResponse({ error: 'unexpected' }, 404)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function wasCalled(
  fetchMock: ReturnType<typeof mockLibrary>,
  predicate: (url: URL) => boolean,
): boolean {
  return fetchMock.mock.calls.some(([input]) => predicate(new URL(String(input), 'http://localhost')))
}

describe('library page empty states (#29)', () => {
  it('never_synced: no instance has ever been polled', async () => {
    mockLibrary({ instances: [instance()], movies: (url) => pageOf([], url) })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('No sync has run yet')).toBeInTheDocument()
    expect(
      screen.getByText(
        'Dubarr has not polled Radarr or Sonarr. Run a sync, or check the connections under Settings.',
      ),
    ).toBeInTheDocument()
    expect(screen.queryByText('The last sync failed')).not.toBeInTheDocument()
    expect(screen.queryByText('Nothing in the library yet')).not.toBeInTheDocument()
  })

  it('never_synced: points at Settings when no instance is configured at all', async () => {
    mockLibrary({ instances: [], movies: (url) => pageOf([], url) })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('No sync has run yet')).toBeInTheDocument()
    expect(screen.getByText(/Add one under Settings, then run a sync/)).toBeInTheDocument()
  })

  it('sync_failed: every instance failed its last attempt and shows the error', async () => {
    mockLibrary({
      instances: [
        instance({
          last_ok: false,
          last_attempt_at: '2026-11-25 09:00:00',
          last_error: 'dial tcp 10.0.0.2:7878: connection refused',
        }),
      ],
      movies: (url) => pageOf([], url),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('The last sync failed')).toBeInTheDocument()
    expect(screen.getAllByText(/connection refused/).length).toBeGreaterThan(0)
    expect(screen.queryByText('No sync has run yet')).not.toBeInTheDocument()
  })

  it('empty_library: the last sync succeeded but returned no titles', async () => {
    mockLibrary({
      instances: [
        instance({ last_ok: true, last_sync_at: '2026-11-25 09:00:00', last_attempt_at: '2026-11-25 09:00:00' }),
      ],
      movies: (url) => pageOf([], url),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('Nothing in the library yet')).toBeInTheDocument()
    expect(screen.getByText('The last sync succeeded but returned no titles.')).toBeInTheDocument()
    expect(screen.queryByText('No sync has run yet')).not.toBeInTheDocument()
  })

  it('filtered_nothing: the library has movies but the search hides them, with a Clear filters control', async () => {
    const user = userEvent.setup()
    const titles = [movieResource()]
    mockLibrary({
      instances: [instance({ last_ok: true, items_seen: titles.length })],
      cacheMovies: titles.length,
      movies: (url) => pageOf(url.searchParams.get('q') ? [] : titles, url),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('Arrival')).toBeInTheDocument()

    await user.type(screen.getByLabelText('Search titles'), 'zzz')
    expect(
      await screen.findByText('No titles match this search', {}, { timeout: 2000 }),
    ).toBeInTheDocument()
    expect(screen.queryByText(/No titles on this page/)).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Clear filters' }))
    expect(await screen.findByText('Arrival')).toBeInTheDocument()
  })

  it('a page emptied by Eligibility says the page is empty, not the library', async () => {
    const user = userEvent.setup()
    const titles = [
      movieResource(),
      movieResource({ id: 2, title: 'Kayak', eligibility: 'missing_target' }),
    ]
    mockLibrary({
      instances: [instance({ last_ok: true, items_seen: titles.length })],
      cacheMovies: titles.length,
      movies: (url) => pageOf(titles, url),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('Arrival')).toBeInTheDocument()

    await user.selectOptions(screen.getByLabelText('Eligibility'), 'waiting_for_tags')

    expect(
      await screen.findByText('No titles on this page match these filters'),
    ).toBeInTheDocument()
    expect(screen.queryByText('Nothing in the library yet')).not.toBeInTheDocument()
    expect(screen.getByText(/Showing 1-2 of 2/)).toBeInTheDocument()
    expect(
      screen.getByText(/Eligibility matches 0 of these 2 titles on this page/),
    ).toBeInTheDocument()
  })

  it('reports a failure even when another instance has never been polled', async () => {
    mockLibrary({
      instances: [
        instance({
          last_ok: false,
          last_attempt_at: '2026-11-25 09:00:00',
          last_error: 'dial tcp 10.0.0.2:7878: connection refused',
        }),
        instance({ kind: 'sonarr', name: 'Sonarr' }),
      ],
      movies: (url) => pageOf([], url),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('The last sync failed')).toBeInTheDocument()
    expect(screen.getAllByText(/connection refused/).length).toBeGreaterThan(0)
    expect(screen.queryByText('No sync has run yet')).not.toBeInTheDocument()
  })

  it('reports an empty library when one instance succeeded and another failed', async () => {
    mockLibrary({
      instances: [
        instance({ last_ok: true, last_sync_at: '2026-11-25 09:00:00' }),
        instance({
          kind: 'sonarr',
          name: 'Sonarr',
          last_ok: false,
          last_attempt_at: '2026-11-25 09:00:00',
          last_error: 'dial tcp 10.0.0.3:8989: connection refused',
        }),
      ],
      movies: (url) => pageOf([], url),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('Nothing in the library yet')).toBeInTheDocument()
    expect(screen.queryByText('No sync has run yet')).not.toBeInTheDocument()
  })
})

describe('library header counts titles, not files', () => {
  it('never adds films and episodes: 486 movies and 166 series, no cache row total', async () => {
    mockLibrary({
      instances: [instance({ last_ok: true, items_seen: 12909 })],
      cacheItems: 12909,
      cacheMovies: 486,
      cacheSeries: 166,
      movies: (url) => pageOf([], url),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    const strip = await screen.findByLabelText('Arr sync health')
    // 12 909 is every cache row (486 films + ~12 400 episodes): a file count,
    // so the header sentence must never carry it, whatever cache_items says.
    // The per-instance "Items seen" diagnostic keeps its own row count.
    const counts = within(strip).getByText(/486 movies/)
    expect(counts).toHaveTextContent('486 movies · 166 series')
    expect(counts).not.toHaveTextContent('12909')
    expect(within(strip).queryByText(/titles cached/)).not.toBeInTheDocument()
  })

  it('handles the singular and the empty library without hiding the strip', async () => {
    mockLibrary({
      instances: [instance({ last_ok: true, items_seen: 2 })],
      cacheItems: 2,
      cacheMovies: 1,
      cacheSeries: 1,
      movies: (url) => pageOf([], url),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    const single = await screen.findByLabelText('Arr sync health')
    expect(within(single).getByText('1 movie · 1 series')).toBeInTheDocument()

    // A library that has never synced still renders the strip: "series" is
    // invariable, so zero reads "0 movies · 0 series" (never "0 serie").
    cleanup()
    mockLibrary({ instances: [instance()], movies: (url) => pageOf([], url) })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    const empty = await screen.findByLabelText('Arr sync health')
    expect(within(empty).getByText('0 movies · 0 series')).toBeInTheDocument()
  })
})

describe('library page table (#29)', () => {
  it('renders title, year, languages and eligibility labels', async () => {
    mockLibrary({
      instances: [instance({ last_ok: true, items_seen: 2 })],
      cacheMovies: 2,
      movies: (url) =>
        pageOf(
          [
            movieResource(),
            movieResource({
              id: 2,
              title: 'Dune',
              year: 2021,
              audio_languages: ['en', 'da'],
              monitored: false,
              available: false,
              eligibility: 'waiting_for_tags',
              target_file_path: '',
              has_file: false,
            }),
          ],
          url,
        ),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    const table = await screen.findByRole('table', { name: 'Library titles' })
    expect(within(table).getByRole('columnheader', { name: 'Title' })).toBeInTheDocument()
    expect(
      within(table).getByRole('columnheader', { name: 'Original language' }),
    ).toBeInTheDocument()
    expect(
      within(table).getByRole('columnheader', { name: 'Audio languages' }),
    ).toBeInTheDocument()
    expect(within(table).getByRole('columnheader', { name: 'Target file' })).toBeInTheDocument()
    expect(within(table).getAllByRole('row')).toHaveLength(3)

    expect(within(table).getByText('Arrival')).toBeInTheDocument()
    expect(within(table).getByText('2016')).toBeInTheDocument()
    expect(within(table).getByText('Eligible')).toBeInTheDocument()
    expect(within(table).getByText('Monitored')).toBeInTheDocument()
    expect(within(table).getByText('Available')).toBeInTheDocument()

    const dune = within(table).getByText('Dune').closest('tr') as HTMLElement
    expect(within(dune).getByText('en, da')).toBeInTheDocument()
    expect(within(dune).getByText('Waiting for tags')).toBeInTheDocument()
    // Unmonitored and file-less read as their own words, and a movie the mirror
    // has no file for shows a dash in the target cell.
    expect(within(dune).getByText('Unmonitored')).toBeInTheDocument()
    expect(within(dune).getByText('No file')).toBeInTheDocument()
    expect(within(dune).getByText('—')).toBeInTheDocument()

    const target = within(table).getByText('Arrival.2016.mkv')
    expect(target).toHaveAttribute('title', '/media/movies/Arrival (2016)/Arrival.2016.mkv')
    expect(within(table).getAllByText('Radarr')).toHaveLength(2)
  })
})

describe('library page filters and paging (#29)', () => {
  it('sends the debounced search as q and returns to the first page', async () => {
    const user = userEvent.setup()
    const fetchMock = mockLibrary({
      instances: [instance({ last_ok: true })],
      cacheMovies: 120,
      movies: (url) => pagedMovies(url, 120),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    await screen.findByRole('table', { name: 'Library titles' })
    await user.click(screen.getByRole('button', { name: 'Next' }))
    await waitFor(() =>
      expect(wasCalled(fetchMock, (url) => url.searchParams.get('page') === '2')).toBe(true),
    )

    await user.type(screen.getByLabelText('Search titles'), 'arrival')
    await waitFor(
      () =>
        expect(
          wasCalled(
            fetchMock,
            (url) => url.searchParams.get('q') === 'arrival' && url.searchParams.get('page') === '1',
          ),
        ).toBe(true),
      { timeout: 3000 },
    )
  })

  it('sends a new page size and resets to the first page', async () => {
    const user = userEvent.setup()
    const fetchMock = mockLibrary({
      instances: [instance({ last_ok: true })],
      cacheMovies: 120,
      movies: (url) => pagedMovies(url, 120),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    await screen.findByRole('table', { name: 'Library titles' })
    await user.click(screen.getByRole('button', { name: 'Next' }))
    await waitFor(() =>
      expect(wasCalled(fetchMock, (url) => url.searchParams.get('page') === '2')).toBe(true),
    )

    await user.selectOptions(screen.getByLabelText('Rows per page'), '100')
    await waitFor(() =>
      expect(
        wasCalled(
          fetchMock,
          (url) => url.searchParams.get('pageSize') === '100' && url.searchParams.get('page') === '1',
        ),
      ).toBe(true),
    )
  })

  it('pages with Showing X-Y of N and Next sending the next page', async () => {
    const user = userEvent.setup()
    const fetchMock = mockLibrary({
      instances: [instance({ last_ok: true })],
      cacheMovies: 120,
      movies: (url) => pagedMovies(url, 120),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('Showing 1-50 of 120')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Previous' })).toBeDisabled()

    await user.click(screen.getByRole('button', { name: 'Next' }))
    await waitFor(() =>
      expect(wasCalled(fetchMock, (url) => url.searchParams.get('page') === '2')).toBe(true),
    )
    expect(await screen.findByText('Showing 51-100 of 120')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Previous' })).toBeEnabled()
  })
})

describe('library page manual refresh (#29)', () => {
  it('refresh now posts the instance kind and shows a syncing state', async () => {
    const user = userEvent.setup()
    let release!: (value: Response) => void
    const refresh = vi.fn(() => new Promise<Response>((resolve) => (release = resolve)))
    const fetchMock = mockLibrary({
      instances: [instance()],
      movies: (url) => pageOf([movieResource()], url),
      refresh,
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('Arrival')).toBeInTheDocument()

    const healthReads = () =>
      fetchMock.mock.calls.filter(
        ([input]) => new URL(String(input), 'http://localhost').pathname === '/api/library/health',
      ).length
    const readsBefore = healthReads()

    await user.click(screen.getByRole('button', { name: 'Refresh now' }))

    // While the request is in flight the control says so and cannot be used.
    expect(await screen.findByRole('button', { name: 'Syncing...' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Refresh all' })).toBeDisabled()

    const posts = fetchMock.mock.calls.filter(
      ([input, init]) =>
        new URL(String(input), 'http://localhost').pathname === '/api/library/refresh' &&
        (init?.method ?? 'GET').toUpperCase() === 'POST',
    )
    expect(posts).toHaveLength(1)
    expect(JSON.parse(String(posts[0][1]?.body))).toEqual({ kind: 'radarr' })

    release(jsonResponse({ accepted: true, kind: 'radarr' }, 202))

    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Refresh now' })).toBeEnabled(),
    )
    // The 202 invalidates health so the strip shows the landed pass.
    await waitFor(() => expect(healthReads()).toBeGreaterThan(readsBefore))
  })

  it('surfaces a busy conflict without breaking the page', async () => {
    const user = userEvent.setup()
    mockLibrary({
      instances: [instance()],
      movies: (url) => pageOf([movieResource()], url),
      refresh: () => jsonResponse({ error: 'a sync pass is already running', code: 'busy' }, 409),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('Arrival')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Refresh all' }))

    expect(await screen.findByText('A sync pass is already running.')).toBeInTheDocument()
    // Still usable: the operator can retry once the running pass lands.
    expect(screen.getByRole('button', { name: 'Refresh all' })).toBeEnabled()
    expect(screen.getByText('Arrival')).toBeInTheDocument()
  })

  it('keeps the refresh controls usable with a healthy payload', async () => {
    mockLibrary({
      instances: [instance()],
      movies: (url) => pageOf([], url),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByRole('button', { name: 'Refresh now' })).toBeEnabled()
    expect(screen.getByRole('button', { name: 'Refresh all' })).toBeEnabled()
  })
})

describe('library page per-item rescan (#29)', () => {
  /** The table row holding a title, so assertions stay scoped to one item. */
  function rowFor(title: string): HTMLElement {
    const cell = screen.getByText(title)
    const row = cell.closest('tr')
    if (!row) throw new Error(`no row for ${title}`)
    return row
  }

  it('rescan posts for the row and reports a successful re-read', async () => {
    const user = userEvent.setup()
    const fetchMock = mockLibrary({
      instances: [instance()],
      movies: (url) =>
        pageOf(
          [
            movieResource({ id: 1, title: 'Arrival' }),
            movieResource({
              id: 2,
              title: 'Dune',
              target_file_path: '/media/movies/Dune (2021)/Dune.2021.mkv',
            }),
          ],
          url,
        ),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('Dune')).toBeInTheDocument()
    await user.click(within(rowFor('Dune')).getByRole('button', { name: 'Rescan' }))

    await waitFor(() =>
      expect(
        wasCalled(
          fetchMock,
          (url) => url.pathname === '/api/library/items/2/rescan',
        ),
      ).toBe(true),
    )
    const posts = fetchMock.mock.calls.filter(
      ([input, init]) =>
        new URL(String(input), 'http://localhost').pathname === '/api/library/items/2/rescan' &&
        (init?.method ?? 'GET').toUpperCase() === 'POST',
    )
    expect(posts).toHaveLength(1)
    // Accepted and re-read: no inline note, and the row settles back to idle.
    await waitFor(() =>
      expect(within(rowFor('Dune')).getByRole('button', { name: 'Rescan' })).toBeEnabled(),
    )
    expect(
      screen.queryByText(/The rescan was accepted, but re-reading the item failed/),
    ).not.toBeInTheDocument()
    expect(screen.queryByText(/Could not rescan/)).not.toBeInTheDocument()
  })

  it('reports an accepted rescan whose re-read failed', async () => {
    const user = userEvent.setup()
    mockLibrary({
      instances: [instance()],
      movies: (url) => pageOf([movieResource({ id: 1, title: 'Arrival' })], url),
      rescan: () =>
        jsonResponse(
          {
            accepted: true,
            reread: 'failed',
            reread_error: 'arr: unexpected HTTP status 500 from GET /api/v3/movie/1',
          },
          202,
        ),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('Arrival')).toBeInTheDocument()
    await user.click(within(rowFor('Arrival')).getByRole('button', { name: 'Rescan' }))

    // The command was accepted, so the page says so and names the cause.
    expect(
      await screen.findByText(
        'The rescan was accepted, but re-reading the item failed: arr: unexpected HTTP status 500 from GET /api/v3/movie/1',
      ),
    ).toBeInTheDocument()
    expect(within(rowFor('Arrival')).getByRole('button', { name: 'Rescan' })).toBeEnabled()
    expect(screen.getByText('Arrival')).toBeInTheDocument()
  })

  it('disables only the clicked row while rescanning', async () => {
    const user = userEvent.setup()
    let release: ((response: Response) => void) | undefined
    const pending = new Promise<Response>((resolve) => {
      release = resolve
    })
    mockLibrary({
      instances: [instance()],
      movies: (url) =>
        pageOf(
          [
            movieResource({ id: 1, title: 'Arrival' }),
            movieResource({
              id: 2,
              title: 'Dune',
              target_file_path: '/media/movies/Dune (2021)/Dune.2021.mkv',
            }),
          ],
          url,
        ),
      rescan: () => pending,
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('Arrival')).toBeInTheDocument()
    await user.click(within(rowFor('Arrival')).getByRole('button', { name: 'Rescan' }))

    await waitFor(() =>
      expect(within(rowFor('Arrival')).getByRole('button', { name: 'Rescanning...' })).toBeDisabled(),
    )
    // The other row is untouched: one slow instance must not block the list.
    expect(within(rowFor('Dune')).getByRole('button', { name: 'Rescan' })).toBeEnabled()

    release?.(jsonResponse({ accepted: true, reread: 'ok' }, 202))

    await waitFor(() =>
      expect(within(rowFor('Arrival')).getByRole('button', { name: 'Rescan' })).toBeEnabled(),
    )
  })
})
