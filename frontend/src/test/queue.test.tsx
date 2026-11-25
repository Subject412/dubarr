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
    match_blocker: 'none',
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
    match_blocker: 'no_donor',
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
    match_blocker: 'no_match',
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
    match_blocker: 'gate_tags',
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
              match_blocker: 'no_donor',
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

function emptyQueueFetch(diagnostics?: Record<string, unknown>) {
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.endsWith('/api/queue') && isMethod(init, 'GET')) {
      return jsonResponse(diagnostics ? { items: [], diagnostics } : { items: [] })
    }
    if (url.includes('/api/library/refresh')) {
      return jsonResponse({ accepted: true, kind: 'all' }, 202)
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

  it('approves an unmatched item with a manual pick while the row is still unmatched', async () => {
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
    // The local pick only unblocks the control while the server still reports
    // the row unmatched: handleApprove accepts a client-supplied
    // library_cache_id for exactly that state. Once a refetch reports the row
    // matched, its own verdict decides again.
    expect(within(card).getByText('Not matched')).toBeInTheDocument()
    await user.click(within(card).getByRole('button', { name: /approve/i }))

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(
        ([url, init]) => String(url).includes('/api/queue/3/approve') && isMethod(init, 'POST'),
      )
      expect(call).toBeTruthy()
      expect(JSON.parse(String(call?.[1]?.body))).toEqual({ library_cache_id: 7 })
    })
  })

  it('keeps approve disabled after a manual pick that yields no donor', async () => {
    // The dead end this pins: the pick unblocked Approve, the refetch then
    // reported the row as matched without a donor, and approve answered 409 —
    // leaving an enabled control that could only fail. The local pick must not
    // outlive the server's verdict.
    let matched = false
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
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
                match_blocker: 'no_donor',
              }
            : item,
        )
        return jsonResponse({ items: list })
      }
      if (url.includes('/api/imports/search-library')) {
        return jsonResponse({ items: libraryResults })
      }
      return jsonResponse({ error: 'unexpected' }, 404)
    })
    vi.stubGlobal('fetch', fetchMock)
    const user = userEvent.setup()
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-3')
    await user.type(within(card).getByPlaceholderText(/search the arr library/i), 'kraina')
    const option = await within(card).findByRole(
      'button',
      { name: /Kraina Lodu \(2013\)/i },
      { timeout: 2000 },
    )
    await user.click(option)
    expect(within(card).getByText(/Matched to Kraina Lodu \(2013\)/)).toBeInTheDocument()

    // The server has the match now, so a refresh reports the row as matched
    // without a donor: the row's own verdict decides, not the local pick.
    matched = true
    await user.click(screen.getByRole('button', { name: 'Refresh' }))

    await waitFor(() => {
      expect(within(card).getByText('Matched, not ready')).toBeInTheDocument()
    })
    expect(within(card).getByRole('button', { name: /approve/i })).toBeDisabled()
    expect(within(card).getByTestId('approve-blocked')).toHaveTextContent(
      'No sibling release carries the dubbing marker (PL.DUB).',
    )
    expect(
      fetchMock.mock.calls.some(([url]) => String(url).includes('/approve')),
    ).toBe(false)
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

  it('gives each match state its own instruction and controls', async () => {
    vi.stubGlobal('fetch', queueFetch())
    renderWithProviders(<QueuePage />, { route: '/queue' })

    // Not identified: identify (or pick manually) is the only way forward.
    const unmatched = await screen.findByTestId('queue-item-3')
    expect(within(unmatched).getByTestId('queue-instruction')).toHaveTextContent(
      'Not identified — identify this release, or match it manually.',
    )
    expect(
      within(unmatched).getByRole('button', { name: /identify with arr/i }),
    ).toBeInTheDocument()
    expect(within(unmatched).getByPlaceholderText(/search the arr library/i)).toBeInTheDocument()
    expect(within(unmatched).getByRole('button', { name: /approve/i })).toBeDisabled()

    // Matched without a donor: the reason is the instruction, and the next step
    // points at the donor drop. Identify would be a dead control here.
    const blocked = screen.getByTestId('queue-item-2')
    const blockedInstruction = within(blocked).getByTestId('queue-instruction')
    expect(blockedInstruction).toHaveTextContent(
      'No sibling release carries the dubbing marker (PL.DUB).',
    )
    expect(blockedInstruction).toHaveTextContent(/watch folder/i)
    expect(within(blocked).queryByRole('button', { name: /identify/i })).not.toBeInTheDocument()
    expect(within(blocked).queryByPlaceholderText(/search the arr library/i)).not.toBeInTheDocument()
    expect(within(blocked).getByRole('button', { name: /approve/i })).toBeDisabled()

    // Ready: instruction and a live approve, nothing else.
    const ready = screen.getByTestId('queue-item-1')
    expect(within(ready).getByTestId('queue-instruction')).toHaveTextContent('Ready to approve.')
    expect(within(ready).getByRole('button', { name: /approve/i })).toBeEnabled()
    expect(within(ready).queryByRole('button', { name: /identify/i })).not.toBeInTheDocument()
  })

  it('gives the excluded tag and a missing donor pattern their own next step', async () => {
    // gate_excluded is not a missing tag: telling the operator to ADD a tag
    // sends them the wrong way, so the exclusion gets its own sentence.
    const excluded = {
      ...items[3],
      id: 41,
      match_reason: 'Target is not eligible for processing (the target carries an excluded tag).',
      match_blocker: 'gate_excluded',
    }
    // No donor pattern configured at all: pointing at the watch folder would
    // send the operator to a folder nothing ever scans. The payload proves it
    // with an empty list.
    const noPattern = { ...items[1], id: 42, donor_patterns: [] }
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => jsonResponse({ items: [excluded, noPattern] })),
    )
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const excludedCard = await screen.findByTestId('queue-item-41')
    expect(within(excludedCard).getByTestId('queue-instruction')).toHaveTextContent(
      /remove the excluded filename tag/i,
    )
    expect(within(excludedCard).getByTestId('queue-instruction')).not.toHaveTextContent(
      /add the required filename tag/i,
    )

    const noPatternCard = screen.getByTestId('queue-item-42')
    expect(within(noPatternCard).getByTestId('queue-instruction')).toHaveTextContent(
      /configure the donor filename pattern/i,
    )
    expect(within(noPatternCard).getByTestId('queue-instruction')).not.toHaveTextContent(
      /watch folder/i,
    )
  })

  it('takes the next step from match_blocker, not from the reason prose', async () => {
    // Same prose on both rows, and neither sentence mentions a mount: only the
    // machine-readable field can tell the mount row apart. If the card were
    // still string-matching `match_reason`, these two would get identical hints.
    const reason = 'The target folder is unavailable.'
    const mountBlocked = { ...items[1], match_reason: reason, match_blocker: 'mount' }
    const unclassified: Record<string, unknown> = { ...items[1], id: 22, match_reason: reason }
    delete unclassified.match_blocker
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => jsonResponse({ items: [mountBlocked, unclassified] })),
    )
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const mount = await screen.findByTestId('queue-item-2')
    const mountHint = within(mount).getByTestId('queue-instruction')
    expect(mountHint).toHaveTextContent(reason)
    expect(mountHint).toHaveTextContent(/library mount is reachable/i)
    expect(mountHint).not.toHaveTextContent(/watch folder/i)

    // A row with no blocker field falls back to the neutral hint, never to the
    // donor guidance it cannot justify.
    const neutral = screen.getByTestId('queue-item-22')
    const neutralHint = within(neutral).getByTestId('queue-instruction')
    expect(neutralHint).not.toHaveTextContent(/watch folder/i)
    expect(neutralHint).not.toHaveTextContent(/library mount is reachable/i)
    expect(neutralHint).toHaveTextContent(/Processing Gate verdict/i)
  })

  it('disables approve and says why when the row is not ready', async () => {
    vi.stubGlobal('fetch', queueFetch())
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const ready = await screen.findByTestId('queue-item-1')
    expect(within(ready).getByRole('button', { name: /approve/i })).toBeEnabled()
    expect(within(ready).queryByTestId('approve-blocked')).not.toBeInTheDocument()

    // A donor-less row: the blocked reason is the server's reason, verbatim,
    // and the control is described by it rather than by a title tooltip (which
    // never fires on a disabled button).
    const noDonor = screen.getByTestId('queue-item-2')
    const noDonorApprove = within(noDonor).getByRole('button', { name: /approve/i })
    expect(noDonorApprove).toBeDisabled()
    expect(within(noDonor).getByTestId('approve-blocked')).toHaveTextContent(
      'No sibling release carries the dubbing marker (PL.DUB).',
    )
    expect(noDonorApprove).toHaveAttribute(
      'aria-describedby',
      within(noDonor).getByTestId('approve-blocked').id,
    )
    expect(noDonorApprove).not.toHaveAttribute('title')

    // A gate-blocked target: the same control, the gate's reason, and a next
    // step aimed at the target's tags rather than the donor.
    const gated = screen.getByTestId('queue-item-4')
    expect(within(gated).getByRole('button', { name: /approve/i })).toBeDisabled()
    expect(within(gated).getByTestId('approve-blocked')).toHaveTextContent(
      'Target is not eligible for processing (missing required target tags: [ArcA310]).',
    )
    expect(within(gated).getByTestId('queue-instruction')).toHaveTextContent(
      /add the required filename tag/i,
    )

    // Not matched at all: nothing to approve until a library target is picked.
    const unmatched = screen.getByTestId('queue-item-3')
    expect(within(unmatched).getByRole('button', { name: /approve/i })).toBeDisabled()
    expect(within(unmatched).getByTestId('approve-blocked')).toHaveTextContent(
      'Match this release to an arr library item first.',
    )
  })

  it('enables approve when the force override makes the row ready', async () => {
    // The same gate-blocked shape as queue-item-4, but the server now reports
    // the row ready because the force override makes approve accept it. The
    // card must follow match_state, never the eligibility badge, or the
    // operator loses a control the server would honour.
    const overridden = {
      ...items[3],
      id: 5,
      source_file_path: '/watch/Shrek 3 2007 PL.DUB.mkv',
      filename: 'Shrek 3 2007 PL.DUB.mkv',
      title: 'Shrek 3',
      match_state: 'ready',
      match_reason: 'Ready to approve under the force tag override.',
      match_blocker: 'none',
    }
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input)
        if (url.endsWith('/api/queue') && isMethod(init, 'GET')) {
          return jsonResponse({
            items: [...items, overridden],
            diagnostics: {
              instances_configured: 1,
              never_synced: 0,
              sync_failed: 0,
              last_pass_at: null,
              cache_items: items.length + 1,
              sync_health_available: true,
              force_tag_override: true,
            },
          })
        }
        if (url.includes('/api/queue/5/approve')) {
          return jsonResponse({ id: 5, status: 'extracting' })
        }
        return jsonResponse({ error: 'unexpected' }, 404)
      }),
    )
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const forced = await screen.findByTestId('queue-item-5')
    expect(forced).toHaveTextContent('waiting for tags')
    expect(within(forced).getByRole('button', { name: /approve/i })).toBeEnabled()
    expect(within(forced).queryByTestId('approve-blocked')).not.toBeInTheDocument()
    expect(within(forced).getByTestId('queue-instruction')).toHaveTextContent('Ready to approve.')

    // The neighbouring gate-blocked row keeps its disabled control: the
    // override is an installation-wide setting, the verdict still per row.
    const gated = screen.getByTestId('queue-item-4')
    expect(within(gated).getByRole('button', { name: /approve/i })).toBeDisabled()
  })

  it('explains an empty queue with no instances configured', async () => {
    vi.stubGlobal(
      'fetch',
      emptyQueueFetch({
        instances_configured: 0,
        never_synced: 0,
        sync_failed: 0,
        last_pass_at: null,
        cache_items: 0,
      }),
    )
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const empty = await screen.findByTestId('queue-empty')
    expect(empty).toHaveAttribute('data-empty-case', 'no_instance')
    expect(within(empty).getByText('No arr instance is configured yet.')).toBeInTheDocument()
    expect(within(empty).getByRole('link', { name: /settings/i })).toHaveAttribute(
      'href',
      '/settings?section=arr.connections',
    )
    // No sync control: there is nothing to sync yet.
    expect(within(empty).queryByRole('button', { name: /refresh now/i })).not.toBeInTheDocument()
  })

  it('explains an empty queue that was never synced', async () => {
    const fetchMock = emptyQueueFetch({
      instances_configured: 1,
      never_synced: 1,
      sync_failed: 0,
      last_pass_at: null,
      cache_items: 0,
    })
    vi.stubGlobal('fetch', fetchMock)
    const user = userEvent.setup()
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const empty = await screen.findByTestId('queue-empty')
    expect(empty).toHaveAttribute('data-empty-case', 'never_synced')
    expect(within(empty).getByText('The library has not been synced yet.')).toBeInTheDocument()

    // The Library page's refresh call, reused rather than reinvented.
    await user.click(within(empty).getByRole('button', { name: /refresh now/i }))
    await waitFor(() => {
      const call = fetchMock.mock.calls.find(
        ([url, init]) =>
          String(url).includes('/api/library/refresh') && isMethod(init, 'POST'),
      )
      expect(call).toBeTruthy()
      expect(JSON.parse(String(call?.[1]?.body))).toEqual({ kind: '' })
    })
  })

  it('explains an empty queue whose last sync failed', async () => {
    vi.stubGlobal(
      'fetch',
      emptyQueueFetch({
        instances_configured: 1,
        never_synced: 0,
        sync_failed: 1,
        last_pass_at: '2026-11-20T10:00:00Z',
        cache_items: 4,
      }),
    )
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const empty = await screen.findByTestId('queue-empty')
    expect(empty).toHaveAttribute('data-empty-case', 'sync_failed')
    expect(within(empty).getByText('The last sync failed.')).toBeInTheDocument()
    // A failure is not a pass, so the last success is shown as its own fact.
    expect(within(empty).getByText(/Last successful sync:/)).toBeInTheDocument()
    expect(within(empty).getByRole('link', { name: /library page/i })).toHaveAttribute(
      'href',
      '/library',
    )
  })

  it('explains a genuinely empty queue', async () => {
    vi.stubGlobal(
      'fetch',
      emptyQueueFetch({
        instances_configured: 1,
        never_synced: 0,
        sync_failed: 0,
        last_pass_at: '2026-11-24T10:00:00Z',
        cache_items: 12,
      }),
    )
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const empty = await screen.findByTestId('queue-empty')
    expect(empty).toHaveAttribute('data-empty-case', 'clear')
    expect(within(empty).getByText('Queue is clear')).toBeInTheDocument()
    expect(within(empty).getByText(/Nothing is waiting for approval/)).toBeInTheDocument()
    expect(within(empty).queryByRole('link')).not.toBeInTheDocument()
    expect(within(empty).queryByRole('button', { name: /refresh now/i })).not.toBeInTheDocument()
  })

  it('counts a partly synced installation instead of claiming a global state', async () => {
    // Radarr synced, Sonarr never attempted: two per-instance counters, but the
    // library is not "not synced yet" — half of it is right here.
    vi.stubGlobal(
      'fetch',
      emptyQueueFetch({
        instances_configured: 2,
        never_synced: 1,
        sync_failed: 0,
        last_pass_at: '2026-11-24T10:00:00Z',
        cache_items: 40,
      }),
    )
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const empty = await screen.findByTestId('queue-empty')
    expect(empty).toHaveAttribute('data-empty-case', 'mixed')
    expect(within(empty).getByText(/1 of 2 instances has not synced yet/)).toBeInTheDocument()
    // Neither absolute verdict may appear: one instance did sync, just now.
    expect(within(empty).queryByText('The library has not been synced yet.')).not.toBeInTheDocument()
    expect(within(empty).queryByText('The last sync failed.')).not.toBeInTheDocument()
    expect(within(empty).queryByText(/Last successful sync:/)).not.toBeInTheDocument()
  })

  it('counts a partly failing installation instead of claiming the sync failed', async () => {
    // Healthy Radarr + failing Sonarr must not print "the last sync failed"
    // above a fresh last_pass_at.
    vi.stubGlobal(
      'fetch',
      emptyQueueFetch({
        instances_configured: 2,
        never_synced: 0,
        sync_failed: 1,
        last_pass_at: '2026-11-24T10:00:00Z',
        cache_items: 40,
      }),
    )
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const empty = await screen.findByTestId('queue-empty')
    expect(empty).toHaveAttribute('data-empty-case', 'mixed')
    expect(within(empty).getByText(/1 of 2 instances failed its last sync/)).toBeInTheDocument()
    expect(within(empty).queryByText('The last sync failed.')).not.toBeInTheDocument()
    // The remaining instance is healthy, so the copy says so rather than
    // asserting that nothing can be matched.
    expect(within(empty).getByText(/the rest are healthy/)).toBeInTheDocument()
  })

  it('points at the Library page when the synced library is empty', async () => {
    // The exact unusable state from issue #30: the sync works, the cache is
    // genuinely empty, so "drop a donor video" would never match anything.
    vi.stubGlobal(
      'fetch',
      emptyQueueFetch({
        instances_configured: 1,
        never_synced: 0,
        sync_failed: 0,
        last_pass_at: '2026-11-24T10:00:00Z',
        cache_items: 0,
      }),
    )
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const empty = await screen.findByTestId('queue-empty')
    expect(empty).toHaveAttribute('data-empty-case', 'empty_library')
    expect(within(empty).getByText('The arr library is empty.')).toBeInTheDocument()
    expect(within(empty).getByRole('link', { name: /library page/i })).toHaveAttribute(
      'href',
      '/library',
    )
    expect(within(empty).queryByText(/Drop a donor video/)).not.toBeInTheDocument()
  })

  it('says it could not read sync health instead of claiming no instance', async () => {
    vi.stubGlobal(
      'fetch',
      emptyQueueFetch({
        instances_configured: 0,
        never_synced: 0,
        sync_failed: 0,
        last_pass_at: null,
        cache_items: 0,
        sync_health_available: false,
      }),
    )
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const empty = await screen.findByTestId('queue-empty')
    expect(empty).toHaveAttribute('data-empty-case', 'health_unavailable')
    expect(within(empty).getByText('Could not read sync health.')).toBeInTheDocument()
    // Zeroed counts are not proof that nothing is configured.
    expect(within(empty).queryByText('No arr instance is configured yet.')).not.toBeInTheDocument()
    expect(within(empty).queryByRole('link', { name: /settings/i })).not.toBeInTheDocument()
  })

  it('no longer claims extraction waits for M2', async () => {
    vi.stubGlobal('fetch', queueFetch())
    renderWithProviders(<QueuePage />, { route: '/queue' })
    await screen.findByTestId('queue-item-1')

    expect(screen.queryByText(/no extraction happens/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/M2/)).not.toBeInTheDocument()
    expect(screen.getByText(/every card says what is still blocking it/i)).toBeInTheDocument()
  })
})
