import { act, screen, waitFor, within } from '@testing-library/react'
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
    // #48: the gate verdict is a projection, not a veto. This pair has a target
    // and a donor, so approve accepts it; the job is born held afterwards.
    eligibility: 'waiting_for_tags',
    matched: true,
    library_title: 'Shrek 2',
    match_state: 'ready',
    match_reason: 'Ready to approve.',
    match_blocker: 'none',
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
      // #48: approval records intent; the job is born after the extraction, so
      // the response carries no job id.
      return jsonResponse({ approved: true })
    }
    if (url.includes('/api/imports/search-library')) {
      return jsonResponse({ items: libraryResults })
    }
    if (url.includes('/api/queue/3/approve')) {
      return jsonResponse({ approved: true })
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

// mirrorOnlyRelease is what identify answers when arr cannot read the name but
// the synced library knows the film: no parse verdict, one mirror candidate.
const mirrorOnlyRelease = {
  parse: { parsed: false, title: '', year: 0, quality: '', confidence: 0 },
  candidates: [
    {
      library_cache_id: 7,
      title: 'Kraina Lodu',
      year: 2013,
      media_type: 'movie',
      external_item_id: 812,
      confidence: 0.5,
      matched_by: 'mirror',
    },
  ],
}

// unreachableRelease is what identify answers when the instance could not be
// asked at all: no verdict, the failure message, and the mirror row it still
// found in the synced library.
const unreachableRelease = {
  parse: {
    parsed: false,
    title: '',
    year: 0,
    quality: '',
    confidence: 0,
    error: 'could not reach the instance: check the base URL and network',
  },
  candidates: [
    {
      library_cache_id: 7,
      title: 'Kraina Lodu',
      year: 2013,
      media_type: 'movie',
      external_item_id: 812,
      confidence: 0.5,
      matched_by: 'mirror',
    },
  ],
}

// ambiguousEnvelope is the 409 envelope the server sends when both libraries are
// enabled and the release proves no kind: it names the choice it will accept.
const ambiguousEnvelope = {
  error: 'this release is unmatched: choose the arr library to identify it in (radarr or sonarr)',
  code: 'ambiguous_kind',
  kinds: ['radarr', 'sonarr'],
}

/**
 * Identify-aware queue fetch. `identifyFailure` makes the FIRST identify
 * request answer `identifyFailureStatus` (default 502) with that body, so a 409
 * envelope can be stubbed and a kind-carrying retry can still succeed;
 * `identifyResult` overrides the 200 verdict the route returns (default: a clean
 * parse with two candidates). Once a match is persisted the list reports the row
 * as matched, so the invalidated query observes the change.
 */
function identifyFetch(
  identifyFailure?: Record<string, unknown>,
  identifyResult: Record<string, unknown> = identifiedRelease,
  identifyFailureStatus = 502,
) {
  let matched = false
  let identifyRequests = 0
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
      identifyRequests += 1
      if (identifyFailure && identifyRequests === 1) {
        return jsonResponse(identifyFailure, identifyFailureStatus)
      }
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

/** The identify POSTs the page actually sent, in order. */
function identifyCalls(fetchMock: { mock: { calls: unknown[][] } }) {
  return fetchMock.mock.calls.filter(
    ([url, init]) =>
      String(url).includes('/api/queue/3/identify') &&
      isMethod(init as RequestInit | undefined, 'POST'),
  )
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

  it('names a pattern the regex engine refused instead of only a missing tag', async () => {
    // #155: no filename tag can satisfy a pattern that does not compile, so the
    // card says why the wait cannot end — next to, not instead of, the
    // missing-tag line, because the pattern is also a tag that was not found.
    const broken = {
      ...items[3],
      gate_reason:
        'filename tag pattern "\\d+\\1" is not a valid regular expression: invalid escape sequence: `\\1`',
    }
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => jsonResponse({ items: [broken] })),
    )
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-4')
    const reason = within(card).getByTestId('queue-gate-reason')
    expect(reason).toHaveTextContent('Cannot become eligible: filename tag pattern')
    expect(reason).toHaveTextContent(/invalid escape sequence/)
    expect(within(card).getByText(/Missing filename tags/)).toBeInTheDocument()
  })

  it('adds no gate reason when every pattern compiled', async () => {
    vi.stubGlobal('fetch', queueFetch())
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-4')
    expect(within(card).queryByTestId('queue-gate-reason')).not.toBeInTheDocument()
  })

  it('renders no gate reason on a row that is already eligible', async () => {
    // The reason describes a wait: it is shown only where there is one. A stale
    // field on an eligible card would claim a target cannot become eligible
    // when it already is, so the card keeps it out of reach.
    const stale = {
      ...items[0],
      gate_reason: 'filename tag pattern "\\\\d+\\\\1" is not a valid regular expression',
    }
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => jsonResponse({ items: [stale] })),
    )
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-1')
    expect(within(card).queryByTestId('queue-gate-reason')).not.toBeInTheDocument()
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

  it('badges an ineligible release that has a donor as approvable and says it will wait', async () => {
    vi.stubGlobal('fetch', queueFetch())
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-4')
    // #48: a target that fails the gate is not a blocker any more — the pair is
    // formable, so approve accepts it and the job is born held.
    expect(within(card).getByText('Ready')).toBeInTheDocument()
    expect(within(card).queryByText('Matched, not ready')).not.toBeInTheDocument()
    expect(within(card).getByText('waiting for tags')).toBeInTheDocument()
    expect(within(card).getByRole('button', { name: /approve/i })).toBeEnabled()
    expect(within(card).queryByTestId('approve-blocked')).not.toBeInTheDocument()
    // The card says what approving will do, in plain language.
    expect(within(card).getByTestId('approve-outcome')).toHaveTextContent(
      'Approve now, it waits for tags',
    )
    // The override is off in this installation, so it must not be offered.
    expect(within(card).queryByTestId('approve-force')).not.toBeInTheDocument()
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

  it('shows only the arr failure when the instance could not be asked', async () => {
    // parsed:false with parse.error is an unreachable instance or a refused key,
    // not a name arr could not read: "enter a title and try again" would send
    // the operator to retype a filename that was never the problem, while the
    // real cause is the red line the server sent.
    vi.stubGlobal('fetch', identifyFetch(undefined, unreachableRelease))
    const user = userEvent.setup()
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-3')
    await user.click(within(card).getByRole('button', { name: /identify with arr/i }))

    const result = await within(card).findByTestId('identify-result')
    expect(within(result).getByTestId('identify-parse-error')).toHaveTextContent(
      /could not reach the instance/i,
    )
    expect(within(result).queryByTestId('identify-unparsable')).not.toBeInTheDocument()
    expect(
      within(result).queryByText(/arr could not parse this release name/i),
    ).not.toBeInTheDocument()
    // The library the appliance already has is still offered, and labelled.
    expect(within(result).getByText(/From your synced library/i)).toBeInTheDocument()
    expect(within(result).getByRole('button', { name: /Kraina Lodu \(2013\)/ })).toHaveTextContent(
      '50%',
    )
  })

  it('shows an unparsable message instead of an empty candidate list', async () => {
    vi.stubGlobal('fetch', identifyFetch(undefined, unparsableRelease))
    const user = userEvent.setup()
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-3')
    await user.click(within(card).getByRole('button', { name: /identify with arr/i }))

    const result = await within(card).findByTestId('identify-result')
    // An unparsable release is named as such, not shown as an empty library.
    // No parse.error here: the instance answered, it just could not read the
    // name, so the advice is the only explanation and must be shown.
    expect(within(result).getByTestId('identify-unparsable')).toHaveTextContent(
      /arr could not parse this release name/i,
    )
    expect(within(result).queryByTestId('identify-parse-error')).not.toBeInTheDocument()
    expect(within(result).queryByText(/No library candidates/i)).not.toBeInTheDocument()
    expect(within(result).queryByText(/arr parsed/i)).not.toBeInTheDocument()
    // The manual path stays usable so the operator can act on the message.
    expect(within(card).getByPlaceholderText(/search the arr library/i)).toBeInTheDocument()
  })

  it('shows mirror candidates when arr could not parse the name', async () => {
    vi.stubGlobal('fetch', identifyFetch(undefined, mirrorOnlyRelease))
    const user = userEvent.setup()
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-3')
    await user.click(within(card).getByRole('button', { name: /identify with arr/i }))

    const result = await within(card).findByTestId('identify-result')
    // The unparsable sentence stays, but it no longer hides the library list.
    expect(within(result).getByText(/arr could not parse this release name/i)).toBeInTheDocument()
    expect(within(result).getByText(/From your synced library/i)).toBeInTheDocument()
    expect(within(result).getByRole('button', { name: /Kraina Lodu \(2013\)/ })).toHaveTextContent(
      '50%',
    )
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

  it('offers exactly the libraries the server named and asks once', async () => {
    // Both libraries are enabled, so the server refuses to guess: it answers
    // 409 ambiguous_kind naming the choice. The panel must offer exactly those
    // two, and it must not fire a retry of its own — the operator chooses.
    const fetchMock = identifyFetch(ambiguousEnvelope, undefined, 409)
    vi.stubGlobal('fetch', fetchMock)
    const user = userEvent.setup()
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-3')
    await user.click(within(card).getByRole('button', { name: /identify with arr/i }))

    const panel = await within(card).findByTestId('identify-ambiguous')
    const buttons = within(panel).getAllByRole('button')
    expect(buttons.map((button) => button.textContent)).toEqual(['Radarr', 'Sonarr'])
    // The server's own sentence, so the operator knows why they were asked.
    expect(within(panel).getByText(/choose the arr library/i)).toBeInTheDocument()

    // One request only: nothing re-asks in the background.
    expect(identifyCalls(fetchMock)).toHaveLength(1)
  })

  it('sends the chosen library once and renders the result it gets back', async () => {
    let releaseIdentify: (() => void) | undefined
    let identifyRequests = 0
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.endsWith('/api/queue') && isMethod(init, 'GET')) {
        return jsonResponse({ items })
      }
      if (url.includes('/api/queue/3/identify')) {
        identifyRequests += 1
        if (identifyRequests === 1) return jsonResponse(ambiguousEnvelope, 409)
        await new Promise<void>((resolve) => {
          releaseIdentify = resolve
        })
        return jsonResponse(identifiedRelease)
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
    await user.click(within(card).getByRole('button', { name: /identify with arr/i }))
    const panel = await within(card).findByTestId('identify-ambiguous')
    await user.click(within(panel).getByRole('button', { name: 'Sonarr' }))

    // The panel names the library it is parsing in while the request runs.
    const inFlight = await within(panel).findByTestId('identify-kind-in-flight')
    expect(inFlight).toHaveTextContent('Identifying against Sonarr…')
    expect(within(panel).getByRole('button', { name: 'Sonarr' })).toBeDisabled()
    expect(releaseIdentify).toBeDefined()

    await act(async () => {
      releaseIdentify?.()
    })

    // The choice collapses the panel and the verdict renders as it always does.
    const result = await within(card).findByTestId('identify-result')
    expect(within(result).getByRole('button', { name: /Kraina Lodu \(2013\)/ })).toBeInTheDocument()
    expect(within(card).queryByTestId('identify-ambiguous')).not.toBeInTheDocument()

    const calls = identifyCalls(fetchMock)
    expect(calls).toHaveLength(2)
    const [, second] = calls
    expect(JSON.parse(String((second?.[1] as RequestInit | undefined)?.body))).toEqual({
      kind: 'sonarr',
    })
  })

  it('keeps the not_configured guidance and offers no choice', async () => {
    // An unconfigured appliance is absence, not a choice: the copy stays and no
    // library button may appear.
    vi.stubGlobal(
      'fetch',
      identifyFetch(
        { error: 'no arr instance is configured for this release', code: 'not_configured' },
        undefined,
        409,
      ),
    )
    const user = userEvent.setup()
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-3')
    await user.click(within(card).getByRole('button', { name: /identify with arr/i }))

    expect(
      await within(card).findByText(/arr is not configured for this release/i),
    ).toBeInTheDocument()
    expect(within(card).queryByTestId('identify-ambiguous')).not.toBeInTheDocument()
    expect(within(card).queryAllByRole('button', { name: /^(radarr|sonarr)$/i })).toHaveLength(0)
  })

  it('renders a kind_conflict refusal as the sentence the server sent', async () => {
    // The operator can act on this one, so it must not be dressed up as a
    // generic "Identify failed" dead end.
    vi.stubGlobal(
      'fetch',
      identifyFetch(
        {
          error: 'the release belongs to the movie library, not series',
          code: 'kind_conflict',
        },
        undefined,
        409,
      ),
    )
    const user = userEvent.setup()
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-3')
    await user.click(within(card).getByRole('button', { name: /identify with arr/i }))

    expect(
      await within(card).findByText('the release belongs to the movie library, not series'),
    ).toBeInTheDocument()
    expect(within(card).queryByText(/Identify failed:/)).not.toBeInTheDocument()
    expect(within(card).queryByTestId('identify-ambiguous')).not.toBeInTheDocument()
  })

  it('marks only the row being identified while the request is in flight', async () => {
    // The identify mutation carries object variables now; comparing that object
    // to a bare id is always false, so the row's in-flight mark would vanish
    // without any test noticing. A second unmatched row proves the mark is not
    // slapped on every row either.
    let releaseIdentify: (() => void) | undefined
    const sibling = {
      ...items[2],
      id: 30,
      source_file_path: '/watch/Frozen.2013.PL.mkv',
      filename: 'Frozen.2013.PL.mkv',
    }
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.endsWith('/api/queue') && isMethod(init, 'GET')) {
        return jsonResponse({ items: [...items, sibling] })
      }
      if (url.includes('/api/queue/3/identify')) {
        await new Promise<void>((resolve) => {
          releaseIdentify = resolve
        })
        return jsonResponse(identifiedRelease)
      }
      return jsonResponse({ error: 'unexpected' }, 404)
    })
    vi.stubGlobal('fetch', fetchMock)
    const user = userEvent.setup()
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-3')
    const other = await screen.findByTestId('queue-item-30')
    await user.click(within(card).getByRole('button', { name: /identify with arr/i }))

    await within(card).findByRole('button', { name: /identifying/i })
    expect(releaseIdentify).toBeDefined()
    expect(within(other).getByRole('button', { name: 'Identify with arr' })).toBeInTheDocument()
    expect(within(other).queryByRole('button', { name: /identifying/i })).not.toBeInTheDocument()

    await act(async () => {
      releaseIdentify?.()
    })
    await within(card).findByTestId('identify-result')
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

  it('gives the excluded tag and an empty marker list their own next step', async () => {
    // gate_excluded is not a missing tag: telling the operator to ADD a tag
    // sends them the wrong way, so the exclusion gets its own sentence.
    const excluded = {
      ...items[3],
      id: 41,
      // A payload from before #48 can still name the gate blockers; the closed
      // set keeps them so their next step stays available.
      match_state: 'matched_no_donor',
      match_reason: 'Target is not eligible for processing (the target carries an excluded tag).',
      match_blocker: 'gate_excluded',
    }
    // An older payload can still report an empty marker list. There is no
    // setting to point at any more (U11): the copy says the markers are fixed.
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
      /built-in donor marker/i,
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

    // An ineligible target with a donor is not a blocker any more (#48): the
    // control stays live and the card states that the job will wait.
    const gated = screen.getByTestId('queue-item-4')
    expect(within(gated).getByRole('button', { name: /approve/i })).toBeEnabled()
    expect(within(gated).queryByTestId('approve-blocked')).not.toBeInTheDocument()
    expect(within(gated).getByTestId('approve-outcome')).toHaveTextContent(/waits for tags/i)

    // Not matched at all: nothing to approve until a library target is picked.
    const unmatched = screen.getByTestId('queue-item-3')
    expect(within(unmatched).getByRole('button', { name: /approve/i })).toBeDisabled()
    expect(within(unmatched).getByTestId('approve-blocked')).toHaveTextContent(
      'Match this release to an arr library item first.',
    )
  })

  it('offers the force override for an ineligible pair when the setting allows it', async () => {
    // #48: the escape hatch is no longer an alternative verdict — the row is
    // approvable either way. The checkbox only decides whether the job is born
    // queued instead of held.
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.endsWith('/api/queue') && isMethod(init, 'GET')) {
        return jsonResponse({
          items,
          diagnostics: {
            instances_configured: 1,
            never_synced: 0,
            sync_failed: 0,
            last_pass_at: null,
            cache_items: items.length,
            sync_health_available: true,
            force_tag_override: true,
          },
        })
      }
      if (url.includes('/api/queue/4/approve')) {
        return jsonResponse({ approved: true })
      }
      return jsonResponse({ error: 'unexpected' }, 404)
    })
    vi.stubGlobal('fetch', fetchMock)
    const user = userEvent.setup()
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const gated = await screen.findByTestId('queue-item-4')
    const override = within(gated).getByTestId('approve-force')
    // One line saying what it does and how far it reaches.
    expect(override).toHaveTextContent(/bypasses the tag gate/i)
    expect(override).toHaveTextContent(/this one job/i)
    expect(within(override).getByRole('checkbox')).not.toBeChecked()

    await user.click(within(override).getByRole('checkbox'))
    expect(within(override).getByRole('checkbox')).toBeChecked()

    await user.click(within(gated).getByRole('button', { name: /approve/i }))
    await waitFor(() => {
      const call = fetchMock.mock.calls.find(
        ([url, init]) => String(url).includes('/api/queue/4/approve') && isMethod(init, 'POST'),
      )
      expect(call).toBeTruthy()
      expect(JSON.parse(String(call?.[1]?.body))).toEqual({ force_ignore_tag_filter: true })
    })

    // An eligible neighbour has nothing to override.
    const ready = screen.getByTestId('queue-item-1')
    expect(within(ready).queryByTestId('approve-force')).not.toBeInTheDocument()
  })

  it('refreshes the list after approving instead of reading a job id from the response', async () => {
    // The response is `{approved: true}`: the job is born after the extraction,
    // so there is no id in the body and nothing may crash without one.
    const fetchMock = queueFetch()
    vi.stubGlobal('fetch', fetchMock)
    const user = userEvent.setup()
    renderWithProviders(<QueuePage />, { route: '/queue' })

    const card = await screen.findByTestId('queue-item-1')
    const listCalls = () =>
      fetchMock.mock.calls.filter(
        ([url, init]) =>
          String(url).endsWith('/api/queue') &&
          isMethod(init as RequestInit | undefined, 'GET'),
      ).length

    const before = listCalls()
    await user.click(within(card).getByRole('button', { name: /approve/i }))

    await waitFor(() => expect(listCalls()).toBeGreaterThan(before))
    // The control settles back to its idle label: a response shaped
    // `{approved: true}` was read without a missing job id throwing.
    expect(await within(card).findByRole('button', { name: 'Approve' })).toBeEnabled()
  })

  // The four unformable cases approve still refuses. The card renders the
  // server's sentence as given for every one of them: they are errors, never a
  // quiet hold, and none of them is re-worded client-side.
  const approveRefusals = [
    'no matched library target',
    'matched library target has no target file to pair a donor with',
    'the target folder could not be read - check the library mount',
    'no donor paired: no sibling file is eligible as a donor',
  ]

  for (const message of approveRefusals) {
    it(`renders the server's 409 as an error: ${message}`, async () => {
      vi.stubGlobal(
        'fetch',
        vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
          const url = String(input)
          if (url.endsWith('/api/queue') && isMethod(init, 'GET')) {
            return jsonResponse({ items: [items[0]] })
          }
          if (url.includes('/api/queue/1/approve')) {
            return jsonResponse({ error: message }, 409)
          }
          return jsonResponse({ error: 'unexpected' }, 404)
        }),
      )
      const user = userEvent.setup()
      renderWithProviders(<QueuePage />, { route: '/queue' })

      const card = await screen.findByTestId('queue-item-1')
      await user.click(within(card).getByRole('button', { name: /approve/i }))

      expect(
        await screen.findByText(`Could not update the queue item: ${message}`),
      ).toBeInTheDocument()
    })
  }

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
