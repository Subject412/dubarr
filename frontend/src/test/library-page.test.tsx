import { cleanup, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { LibraryPage } from '../pages/Library'
import type { LibraryHealthInstance, LibraryItem } from '../lib/library'
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

type ItemsHandler = (url: URL) => {
  items: LibraryItem[]
  total: number
  limit: number
  offset: number
}

function pageOf(items: LibraryItem[], url: URL, total = items.length) {
  return {
    items,
    total,
    limit: Number(url.searchParams.get('limit') ?? 50),
    offset: Number(url.searchParams.get('offset') ?? 0),
  }
}

/** Fetch mock for the library endpoints; anything else is a 404. */
function mockLibrary({
  instances,
  cacheItems = 0,
  cacheMovies = 0,
  cacheSeries = 0,
  items,
  refresh,
  rescan,
}: {
  instances: LibraryHealthInstance[]
  cacheItems?: number
  /** Films and shows in the library; the header reads these, not cacheItems. */
  cacheMovies?: number
  cacheSeries?: number
  items: ItemsHandler
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
    if (url.pathname === '/api/library/items') {
      return jsonResponse(items(url))
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
    mockLibrary({ instances: [instance()], items: (url) => pageOf([], url) })
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
    mockLibrary({ instances: [], items: (url) => pageOf([], url) })
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
      items: (url) => pageOf([], url),
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
      items: (url) => pageOf([], url),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('Nothing in the library yet')).toBeInTheDocument()
    expect(screen.getByText('The last sync succeeded but returned no titles.')).toBeInTheDocument()
    expect(screen.queryByText('No sync has run yet')).not.toBeInTheDocument()
  })

  it('filtered_nothing: the cache has rows but the filters hide them, with a Clear filters control', async () => {
    const user = userEvent.setup()
    const titles = [movie()]
    mockLibrary({
      instances: [instance({ last_ok: true, items_seen: titles.length })],
      cacheItems: titles.length,
      items: (url) => pageOf(url.searchParams.get('q') ? [] : titles, url),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('Arrival')).toBeInTheDocument()

    await user.type(screen.getByLabelText('Search titles'), 'zzz')
    expect(
      await screen.findByText('No titles match these filters', {}, { timeout: 2000 }),
    ).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Clear filters' }))
    expect(await screen.findByText('Arrival')).toBeInTheDocument()
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
      items: (url) => pageOf([], url),
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
      items: (url) => pageOf([], url),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('Nothing in the library yet')).toBeInTheDocument()
    expect(screen.queryByText('No sync has run yet')).not.toBeInTheDocument()
  })

  it('keeps the failure visible when a kind filter hides the only rows', async () => {
    const user = userEvent.setup()
    const sonarrRow = movie({
      id: 7,
      media_type: 'series',
      title: 'Andor',
      year: 2022,
      season: 2,
      episode: 5,
      kind: 'sonarr',
      instance_name: 'Sonarr',
    })
    mockLibrary({
      instances: [
        instance({
          last_ok: false,
          last_attempt_at: '2026-11-25 09:00:00',
          last_error: 'dial tcp 10.0.0.2:7878: connection refused',
        }),
        instance({ kind: 'sonarr', name: 'Sonarr', last_ok: true, items_seen: 1 }),
      ],
      cacheItems: 1,
      items: (url) =>
        url.searchParams.get('kind') === 'radarr' ? pageOf([], url) : pageOf([sonarrRow], url),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('Andor')).toBeInTheDocument()

    await user.selectOptions(screen.getByLabelText('Kind'), 'radarr')
    expect(
      await screen.findByText('The last sync failed', {}, { timeout: 2000 }),
    ).toBeInTheDocument()
    expect(screen.getAllByText(/connection refused/).length).toBeGreaterThan(0)
    expect(screen.queryByText('No titles match these filters')).not.toBeInTheDocument()
  })

  it('sync_failed: with a kind filter active the detail names the failing instance in scope', async () => {
    const user = userEvent.setup()
    const sonarrRow = movie({
      id: 8,
      media_type: 'series',
      title: 'Andor',
      year: 2022,
      season: 2,
      episode: 5,
      kind: 'sonarr',
      instance_name: 'Sonarr',
    })
    mockLibrary({
      instances: [
        instance({
          last_ok: false,
          last_attempt_at: '2026-11-25 09:00:00',
          last_error: 'radarr: dial tcp 10.0.0.2:7878: connection refused',
        }),
        instance({
          kind: 'sonarr',
          name: 'Sonarr',
          last_ok: false,
          last_attempt_at: '2026-11-25 09:00:00',
          last_error: 'sonarr: 401 Unauthorized',
          items_seen: 1,
        }),
      ],
      cacheItems: 1,
      items: (url) =>
        url.searchParams.get('kind') === 'radarr' ? pageOf([], url) : pageOf([sonarrRow], url),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('Andor')).toBeInTheDocument()

    await user.selectOptions(screen.getByLabelText('Kind'), 'radarr')

    const heading = await screen.findByText('The last sync failed', {}, { timeout: 2000 })
    const detail = heading.parentElement as HTMLElement
    expect(detail).toHaveTextContent('radarr: dial tcp 10.0.0.2:7878: connection refused')
    expect(detail).not.toHaveTextContent('sonarr: 401 Unauthorized')
  })
})

describe('library header counts titles, not files', () => {
  it('never adds films and episodes: 486 movies and 166 series, no cache row total', async () => {
    mockLibrary({
      instances: [instance({ last_ok: true, items_seen: 12909 })],
      cacheItems: 12909,
      cacheMovies: 486,
      cacheSeries: 166,
      items: (url) => pageOf([], url),
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
      items: (url) => pageOf([], url),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    const single = await screen.findByLabelText('Arr sync health')
    expect(within(single).getByText('1 movie · 1 series')).toBeInTheDocument()

    // A library that has never synced still renders the strip: "series" is
    // invariable, so zero reads "0 movies · 0 series" (never "0 serie").
    cleanup()
    mockLibrary({ instances: [instance()], items: (url) => pageOf([], url) })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    const empty = await screen.findByLabelText('Arr sync health')
    expect(within(empty).getByText('0 movies · 0 series')).toBeInTheDocument()
  })
})

describe('library page table (#29)', () => {
  it('renders title, year, episode formatting and eligibility labels', async () => {
    mockLibrary({
      instances: [instance({ last_ok: true, items_seen: 2 })],
      cacheItems: 2,
      items: (url) =>
        pageOf(
          [
            movie(),
            movie({
              id: 2,
              media_type: 'series',
              title: 'Andor',
              year: 2022,
              season: 2,
              episode: 5,
              kind: 'sonarr',
              instance_name: 'Sonarr',
              eligibility_status: 'waiting_for_tags',
              matches_target_tags: false,
              target_file_path: '/media/tv/Andor/Season 02/Andor.S02E05.mkv',
            }),
          ],
          url,
        ),
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    const table = await screen.findByRole('table', { name: 'Library titles' })
    expect(within(table).getByRole('columnheader', { name: 'Title' })).toBeInTheDocument()
    expect(within(table).getByRole('columnheader', { name: 'Target file' })).toBeInTheDocument()
    expect(within(table).getAllByRole('row')).toHaveLength(3)

    expect(within(table).getByText('Arrival')).toBeInTheDocument()
    expect(within(table).getByText('2016')).toBeInTheDocument()
    expect(within(table).getByText('Movie')).toBeInTheDocument()
    expect(within(table).getByText('Eligible')).toBeInTheDocument()

    expect(within(table).getByText('Andor')).toBeInTheDocument()
    expect(within(table).getByText('S02E05')).toBeInTheDocument()
    expect(within(table).getByText('Waiting for tags')).toBeInTheDocument()

    const target = within(table).getByText('Andor.S02E05.mkv')
    expect(target).toHaveAttribute('title', '/media/tv/Andor/Season 02/Andor.S02E05.mkv')
    expect(within(table).getByText('Sonarr')).toBeInTheDocument()
  })
})

describe('library page filters and paging (#29)', () => {
  it('sends the debounced search as q and returns to the first page', async () => {
    const user = userEvent.setup()
    const fetchMock = mockLibrary({
      instances: [instance({ last_ok: true })],
      cacheItems: 120,
      items: (url) => {
        const args = pageOf([], url, 120)
        return {
          ...args,
          items: Array.from({ length: Math.min(args.limit, 120 - args.offset) }, (_, i) =>
            movie({ id: args.offset + i + 1, title: `Title ${args.offset + i + 1}` }),
          ),
        }
      },
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    await screen.findByRole('table', { name: 'Library titles' })
    await user.click(screen.getByRole('button', { name: 'Next' }))
    await waitFor(() =>
      expect(
        wasCalled(fetchMock, (url) => url.searchParams.get('offset') === '50'),
      ).toBe(true),
    )

    await user.type(screen.getByLabelText('Search titles'), 'arrival')
    await waitFor(
      () =>
        expect(
          wasCalled(
            fetchMock,
            (url) => url.searchParams.get('q') === 'arrival' && url.searchParams.get('offset') === '0',
          ),
        ).toBe(true),
      { timeout: 3000 },
    )
  })

  it('sends the selected kind and resets the offset to the first page', async () => {
    const user = userEvent.setup()
    const fetchMock = mockLibrary({
      instances: [instance({ last_ok: true })],
      cacheItems: 120,
      items: (url) => {
        const args = pageOf([], url, 120)
        return {
          ...args,
          items: Array.from({ length: Math.min(args.limit, 120 - args.offset) }, (_, i) =>
            movie({ id: args.offset + i + 1, title: `Title ${args.offset + i + 1}` }),
          ),
        }
      },
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    await screen.findByRole('table', { name: 'Library titles' })
    await user.click(screen.getByRole('button', { name: 'Next' }))
    await waitFor(() =>
      expect(
        wasCalled(fetchMock, (url) => url.searchParams.get('offset') === '50'),
      ).toBe(true),
    )

    await user.selectOptions(screen.getByLabelText('Kind'), 'sonarr')
    await waitFor(() =>
      expect(
        wasCalled(
          fetchMock,
          (url) => url.searchParams.get('kind') === 'sonarr' && url.searchParams.get('offset') === '0',
        ),
      ).toBe(true),
    )
  })

  it('pages with Showing X-Y of N and Next sending the next offset', async () => {
    const user = userEvent.setup()
    const fetchMock = mockLibrary({
      instances: [instance({ last_ok: true })],
      cacheItems: 120,
      items: (url) => {
        const args = pageOf([], url, 120)
        return {
          ...args,
          items: Array.from({ length: Math.min(args.limit, 120 - args.offset) }, (_, i) =>
            movie({ id: args.offset + i + 1, title: `Title ${args.offset + i + 1}` }),
          ),
        }
      },
    })
    renderWithProviders(<LibraryPage />, { route: '/library' })

    expect(await screen.findByText('Showing 1-50 of 120')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Previous' })).toBeDisabled()

    await user.click(screen.getByRole('button', { name: 'Next' }))
    await waitFor(() =>
      expect(
        wasCalled(fetchMock, (url) => url.searchParams.get('offset') === '50'),
      ).toBe(true),
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
      items: (url) => pageOf([movie()], url),
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
      items: (url) => pageOf([movie()], url),
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
      items: (url) => pageOf([], url),
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
      items: (url) =>
        pageOf(
          [
            movie({ id: 1, title: 'Arrival' }),
            movie({
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
      items: (url) => pageOf([movie({ id: 1, title: 'Arrival' })], url),
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
      items: (url) =>
        pageOf(
          [
            movie({ id: 1, title: 'Arrival' }),
            movie({
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
