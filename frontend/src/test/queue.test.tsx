import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { QueuePage } from '../pages/Queue'
import { isMethod, jsonResponse, renderWithProviders } from '../test/helpers'

const items = [
  {
    id: 1,
    source_file_path: '/watch/Minions and Monsters 2026 PL.DUB.MD.1080p.AMZN.WEB-DL.XviD-MAXX.mp4',
    filename: 'Minions and Monsters 2026 PL.DUB.MD.1080p.AMZN.WEB-DL.XviD-MAXX.mp4',
    file_size: 1024,
    status: 'pending_approval',
    title: 'Minions and Monsters',
    year: 2026,
    media_type: 'movie',
    target_path: '/media/Minions and Monsters (2026) [ArcA310].mkv',
    original_language: 'en',
    audio_languages: ['en', 'pl'],
    eligibility: 'eligible',
    matched: true,
    library_title: 'Minions and Monsters',
    match_state: 'ready',
    match_reason: 'Ready to approve.',
    matched_tags: ['[ArcA310]'],
    missing_tags: [],
    donor_candidates: [
      {
        filename: 'Minions and Monsters 2026 PL.DUB.MD.1080p.AMZN.WEB-DL.XviD-MAXX.mp4',
        path: '/media/Minions and Monsters 2026 PL.DUB.MD.1080p.AMZN.WEB-DL.XviD-MAXX.mp4',
        file_size: 2048,
        selected: true,
      },
    ],
  },
  {
    id: 2,
    source_file_path: '/watch/Shrek.2001.PL.DUB.mkv',
    filename: 'Shrek.2001.PL.DUB.mkv',
    file_size: 512,
    status: 'pending_approval',
    title: 'Shrek',
    year: 2001,
    media_type: 'movie',
    target_path: '/media/Shrek (2001) [Remux].mkv',
    original_language: 'en',
    audio_languages: ['en'],
    eligibility: 'eligible',
    matched: true,
    library_title: 'Shrek',
    match_state: 'matched_no_donor',
    match_reason: 'No sibling release carries the dubbing marker (PL.DUB).',
    matched_tags: ['[ArcA310]'],
    missing_tags: [],
    donor_candidates: [],
  },
  {
    id: 3,
    source_file_path: '/watch/Kraina.Lodu.2013.PL.mkv',
    filename: 'Kraina.Lodu.2013.PL.mkv',
    file_size: 256,
    status: 'pending_approval',
    title: '',
    media_type: '',
    target_path: '',
    original_language: '',
    audio_languages: [],
    eligibility: 'missing_target',
    matched: false,
    library_title: '',
    match_state: 'unmatched',
    match_reason: 'No arr title matched this release.',
    matched_tags: [],
    missing_tags: [],
    donor_candidates: [],
  },
  {
    id: 4,
    source_file_path: '/watch/Shrek 2 2004 PL.DUB.mkv',
    filename: 'Shrek 2 2004 PL.DUB.mkv',
    file_size: 384,
    status: 'pending_approval',
    title: 'Shrek 2',
    year: 2004,
    media_type: 'movie',
    target_path: '/media/Shrek 2 (2004) [Remux].mkv',
    original_language: 'en',
    audio_languages: ['en'],
    eligibility: 'waiting_for_tags',
    matched: true,
    library_title: 'Shrek 2',
    match_state: 'matched_no_donor',
    match_reason: 'Target is not eligible for processing (missing required target tags: [ArcA310]).',
    matched_tags: [],
    missing_tags: ['[ArcA310]'],
    donor_candidates: [
      {
        filename: 'Shrek 2 2004 PL.DUB.mkv',
        path: '/media/Shrek 2 2004 PL.DUB.mkv',
        file_size: 4096,
        selected: true,
      },
    ],
  },
]

const libraryResults = [
  {
    id: 7,
    title: 'Kraina Lodu',
    year: 2013,
    media_type: 'movie',
    original_language_iso2: 'en',
  },
]

function queueFetch() {
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.endsWith('/api/queue') && isMethod(init, 'GET')) {
      return jsonResponse({ items })
    }
    if (url.includes('/api/queue/1/approve')) {
      return jsonResponse({ id: 1, status: 'extracting' })
    }
    if (url.includes('/api/imports/search-library')) {
      return jsonResponse({ items: libraryResults })
    }
    if (url.includes('/api/queue/3/approve')) {
      return jsonResponse({ id: 3, status: 'extracting' })
    }
    if (url.includes('/api/queue/2/reject')) {
      return jsonResponse({ id: 2, status: 'ignored' })
    }
    return jsonResponse({ error: 'unexpected' }, 404)
  })
}

describe('approval queue page', () => {
  it('renders pre-selected cards with gate badges, languages and donor pairing', async () => {
    vi.stubGlobal('fetch', queueFetch())
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const eligible = await screen.findByTestId('queue-item-1')
    expect(within(eligible).getByText('Minions and Monsters (2026)')).toBeInTheDocument()
    expect(within(eligible).getByText('eligible')).toBeInTheDocument()
    expect(within(eligible).getByText('pl')).toBeInTheDocument()
    expect(within(eligible).getByText('pre-selected donor')).toBeInTheDocument()
    expect(within(eligible).getByText(/ArcA310/)).toBeInTheDocument()

    const noDonor = screen.getByTestId('queue-item-2')
    expect(within(noDonor).getByText('eligible')).toBeInTheDocument()
    expect(within(noDonor).getByText(/No donor paired/)).toBeInTheDocument()

    const blocked = screen.getByTestId('queue-item-4')
    expect(within(blocked).getByText('waiting for tags')).toBeInTheDocument()
    expect(within(blocked).getByText(/Missing filename tags/)).toBeInTheDocument()
  })

  it('badges a ready release', async () => {
    vi.stubGlobal('fetch', queueFetch())
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-1')
    expect(within(card).getByText('Ready')).toBeInTheDocument()
    expect(within(card).queryByText('Not matched')).not.toBeInTheDocument()
    expect(within(card).queryByText('Matched, not ready')).not.toBeInTheDocument()
    expect(within(card).queryByText(/No donor paired/)).not.toBeInTheDocument()
  })

  it('badges a matched release that has no donor', async () => {
    vi.stubGlobal('fetch', queueFetch())
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-2')
    expect(within(card).getByText('Matched, not ready')).toBeInTheDocument()
    expect(within(card).queryByText('Not matched')).not.toBeInTheDocument()
  })

  it('badges an ineligible release that has a donor as not ready', async () => {
    vi.stubGlobal('fetch', queueFetch())
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-4')
    expect(within(card).getByText('Matched, not ready')).toBeInTheDocument()
    expect(within(card).queryByText('Ready')).not.toBeInTheDocument()
    // The reason is visible prose next to the badge, not a title attribute.
    expect(
      within(card).getByText(
        'Target is not eligible for processing (missing required target tags: [ArcA310]).',
      ),
    ).toBeInTheDocument()
  })

  it('badges an unmatched release', async () => {
    vi.stubGlobal('fetch', queueFetch())
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-3')
    expect(within(card).getByText('Not matched')).toBeInTheDocument()
    expect(within(card).queryByText('Matched, not ready')).not.toBeInTheDocument()
    expect(within(card).getByText('No arr title matched this release.')).toBeInTheDocument()
    // No target means the "drop a release next to the target" guidance would
    // point at nothing.
    expect(within(card).queryByText(/drop a release/i)).not.toBeInTheDocument()
  })

  it('derives the badge from matched when match_state is missing', async () => {
    const withoutMatchState: Record<string, unknown> = { ...items[1] }
    delete withoutMatchState.match_state
    vi.stubGlobal('fetch', vi.fn(async () => jsonResponse({ items: [withoutMatchState] })))
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-2')
    expect(within(card).getByText('Matched, not ready')).toBeInTheDocument()
    expect(within(card).queryByText('Not matched')).not.toBeInTheDocument()
  })

  it('shows the match reason as visible prose next to the badge', async () => {
    vi.stubGlobal('fetch', queueFetch())
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-2')
    expect(within(card).getByText(/No donor paired/)).toBeInTheDocument()
    expect(
      within(card).getByText('No sibling release carries the dubbing marker (PL.DUB).'),
    ).toBeInTheDocument()
  })

  it('approves through the API', async () => {
    const fetchMock = queueFetch()
    vi.stubGlobal('fetch', fetchMock)
    const user = userEvent.setup()
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-1')
    await user.click(within(card).getByRole('button', { name: /approve/i }))

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(
        ([url, init]) => String(url).includes('/api/queue/1/approve') && isMethod(init, 'POST'),
      )
      expect(call).toBeTruthy()
    })
  })

  it('rejects through the API', async () => {
    const fetchMock = queueFetch()
    vi.stubGlobal('fetch', fetchMock)
    const user = userEvent.setup()
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-2')
    await user.click(within(card).getByRole('button', { name: /reject/i }))

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(
        ([url, init]) => String(url).includes('/api/queue/2/reject') && isMethod(init, 'POST'),
      )
      expect(call).toBeTruthy()
    })
  })

  it('matches an unmatched item against the arr library and approves with it', async () => {
    const fetchMock = queueFetch()
    vi.stubGlobal('fetch', fetchMock)
    const user = userEvent.setup()
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-3')
    const approveButton = within(card).getByRole('button', { name: /approve/i })
    expect(approveButton).toBeDisabled()

    await user.type(within(card).getByPlaceholderText(/search the arr library/i), 'kraina')
    const option = await within(card).findByRole(
      'button',
      { name: /Kraina Lodu \(2013\)/i },
      { timeout: 2000 },
    )
    await user.click(option)

    expect(within(card).getByText(/Matched to Kraina Lodu \(2013\)/)).toBeInTheDocument()
    await user.click(within(card).getByRole('button', { name: /approve/i }))

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(
        ([url, init]) => String(url).includes('/api/queue/3/approve') && isMethod(init, 'POST'),
      )
      expect(call).toBeTruthy()
      expect(JSON.parse(String(call?.[1]?.body))).toEqual({ library_cache_id: 7 })
    })
  })

  it('shows a clear empty state', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => jsonResponse({ items: [] })),
    )
    renderWithProviders(<QueuePage />, { route: '/queue' })
    expect(await screen.findByText('Queue is clear')).toBeInTheDocument()
    expect(screen.getByText('0 pending')).toBeInTheDocument()
  })
})
