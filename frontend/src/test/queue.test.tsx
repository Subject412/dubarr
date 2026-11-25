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

// identifiedRelease is what arr's parser and the cache lookup return for the
// unmatched Kraina Lodu drop: a clean parse and two ranked candidates.
const identifiedRelease = {
  parse: { parsed: true, title: 'Kraina Lodu', year: 2013, quality: 'Bluray-1080p', confidence: 1 },
  candidates: [
    {
      library_cache_id: 7,
      title: 'Kraina Lodu',
      year: 2013,
      media_type: 'movie',
      external_item_id: 812,
      confidence: 1,
      matched_by: 'parse',
    },
    {
      library_cache_id: 8,
      title: 'Kraina Lodu 2',
      year: 2019,
      media_type: 'movie',
      external_item_id: 330457,
      confidence: 0.5,
      matched_by: 'parse',
    },
  ],
}

// unparsableRelease is what an instance answers for a release name it cannot
// read: no title, no confidence, and no candidate scan behind it.
const unparsableRelease = {
  parse: { parsed: false, title: '', year: 0, quality: '', confidence: 0 },
  candidates: [],
}

/**
 * Identify-aware queue fetch. `identifyFailure` makes the identify route answer
 * 502 with that body; `identifyResult` overrides the 200 verdict the route
 * returns (default: a clean parse with two candidates). Once a match is
 * persisted the list reports the row as matched, so the invalidated query
 * observes the change.
 */
function identifyFetch(
  identifyFailure?: Record<string, unknown>,
  identifyResult: Record<string, unknown> = identifiedRelease,
) {
  let matched = false
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.endsWith('/api/queue') && isMethod(init, 'GET')) {
      const list = items.map((item) =>
        item.id === 3 && matched
          ? {
              ...item,
              title: 'Kraina Lodu',
              year: 2013,
              media_type: 'movie',
              matched: true,
              library_title: 'Kraina Lodu',
              match_state: 'matched_no_donor',
              match_reason: 'No sibling release carries the dubbing marker (PL.DUB).',
            }
          : item,
      )
      return jsonResponse({ items: list })
    }
    if (url.includes('/api/queue/3/identify')) {
      if (identifyFailure) return jsonResponse(identifyFailure, 502)
      return jsonResponse(identifyResult)
    }
    if (url.includes('/api/queue/3/match')) {
      matched = true
      return jsonResponse({ match_state: 'matched_no_donor' })
    }
    if (url.includes('/api/imports/search-library')) {
      return jsonResponse({ items: libraryResults })
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

  it('identifies an unmatched release and shows candidates', async () => {
    const fetchMock = identifyFetch()
    vi.stubGlobal('fetch', fetchMock)
    const user = userEvent.setup()
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-3')
    await user.click(within(card).getByRole('button', { name: /identify with arr/i }))

    const result = await within(card).findByTestId('identify-result')
    // The operator sees what arr thought of the release name.
    expect(within(result).getByText(/Bluray-1080p/)).toBeInTheDocument()
    expect(within(result).getByText(/arr parsed/)).toBeInTheDocument()
    // Both library rows are offered, best first, with their confidence.
    const exact = within(result).getByRole('button', { name: /Kraina Lodu \(2013\)/ })
    expect(exact).toHaveTextContent('100%')
    expect(within(result).getByRole('button', { name: /Kraina Lodu 2 \(2019\)/ })).toHaveTextContent(
      '50%',
    )

    expect(
      fetchMock.mock.calls.some(
        ([url, init]) =>
          String(url).includes('/api/queue/3/identify') && isMethod(init, 'POST'),
      ),
    ).toBe(true)
  })

  it('shows an unparsable message instead of an empty candidate list', async () => {
    vi.stubGlobal('fetch', identifyFetch(undefined, unparsableRelease))
    const user = userEvent.setup()
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-3')
    await user.click(within(card).getByRole('button', { name: /identify with arr/i }))

    const result = await within(card).findByTestId('identify-result')
    // An unparsable release is named as such, not shown as an empty library.
    expect(
      within(result).getByText(/arr could not parse this release name/i),
    ).toBeInTheDocument()
    expect(within(result).queryByText(/No library candidates/i)).not.toBeInTheDocument()
    expect(within(result).queryByText(/arr parsed/i)).not.toBeInTheDocument()
    // The manual path stays usable so the operator can act on the message.
    expect(within(card).getByPlaceholderText(/search the arr library/i)).toBeInTheDocument()
  })

  it('picking a candidate persists the match and refreshes the card', async () => {
    const fetchMock = identifyFetch()
    vi.stubGlobal('fetch', fetchMock)
    const user = userEvent.setup()
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-3')
    await user.click(within(card).getByRole('button', { name: /identify with arr/i }))
    const result = await within(card).findByTestId('identify-result')
    await user.click(within(result).getByRole('button', { name: /Kraina Lodu \(2013\)/ }))

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(
        ([url, init]) => String(url).includes('/api/queue/3/match') && isMethod(init, 'POST'),
      )
      expect(call).toBeTruthy()
      expect(JSON.parse(String(call?.[1]?.body))).toEqual({ library_cache_id: 7, confidence: 1 })
    })

    // The queue query is invalidated, so the card flips to the matched state.
    await waitFor(() => {
      expect(within(card).getByText('Matched, not ready')).toBeInTheDocument()
    })
    expect(within(card).queryByTestId('identify-result')).not.toBeInTheDocument()
  })

  it('surfaces an identify failure with its diagnostic', async () => {
    vi.stubGlobal(
      'fetch',
      identifyFetch({
        error: 'the instance did not answer the parse request in time',
        code: 'identify_failed',
        class: 'timeout',
      }),
    )
    const user = userEvent.setup()
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-3')
    await user.click(within(card).getByRole('button', { name: /identify with arr/i }))

    expect(
      await within(card).findByText(/did not answer the parse request in time/),
    ).toBeInTheDocument()
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
