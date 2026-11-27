import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import type { SyncJob, SyncReport } from '../lib/api'
import { SyncPage } from '../pages/Sync'
import { isMethod, jsonResponse, renderWithProviders } from '../test/helpers'

function makeReport(overrides: Partial<SyncReport> = {}): SyncReport {
  return {
    verdict: 'ok',
    offsets_ms: [120, 118, 121],
    qualities: [0.92, 0.88, 0.9],
    max_drift_ms: 3,
    classification: 'stable',
    checkpoints: [
      { position_pct: 10, slice_start_sec: 60, slice_len_sec: 3, offset_ms: 120, correlation_quality: 0.92 },
      { position_pct: 30, slice_start_sec: 180, slice_len_sec: 3, offset_ms: 118, correlation_quality: 0.88 },
      { position_pct: 50, slice_start_sec: 300, slice_len_sec: 3, offset_ms: 121, correlation_quality: 0.9 },
      { position_pct: 70, slice_start_sec: 420, slice_len_sec: 3, offset_ms: 119, correlation_quality: 0.86 },
      { position_pct: 90, slice_start_sec: 540, slice_len_sec: 3, offset_ms: 120, correlation_quality: 0.91 },
    ],
    sync_method: 'correlation',
    ...overrides,
  }
}

function makeJob(overrides: Partial<SyncJob> = {}): SyncJob {
  return {
    id: 1,
    status: 'done',
    vault_track_id: 11,
    target_library_cache_id: 7,
    target_video_path: '/media/Shrek (2001) [Remux].mkv',
    target_lang_iso2: 'pl',
    calculated_offset_ms: 120,
    report: makeReport(),
    created_at: '2026-11-22T10:00:00Z',
    ...overrides,
  }
}

function stubJob(job: SyncJob) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.endsWith('/api/sync/jobs')) return jsonResponse({ items: [job] })
      if (url.endsWith(`/api/sync/jobs/${job.id}`)) return jsonResponse(job)
      return jsonResponse({ error: 'unexpected' }, 404)
    }),
  )
}

const vaultItems = [
  {
    id: 11,
    title: 'Shrek',
    media_type: 'movie',
    season_number: null,
    episode_number: null,
    filename: 'Shrek.2001.PL.DUB.mka',
    file_size: 1024,
    sha256: 'a'.repeat(64),
    language: 'pl',
    variant: 'standard',
    codec: 'aac',
    channels: 'stereo',
    channel_count: 2,
    source_release_name: 'Shrek.2001.PL.DUB',
    created_at: '2026-11-22T09:00:00Z',
  },
]

const libraryItems = [{ id: 7, title: 'Shrek', year: 2001, media_type: 'movie', original_language_iso2: 'en' }]

describe('sync studio', () => {
  it('lists jobs newest first with status badges', async () => {
    const older = makeJob({ id: 1, status: 'done', created_at: '2026-11-22T08:00:00Z' })
    const newer = makeJob({ id: 2, status: 'failed', report: undefined, error_message: 'no anchor', created_at: '2026-11-22T11:00:00Z' })
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (url.endsWith('/api/sync/jobs')) return jsonResponse({ items: [newer, older] })
        if (url.endsWith('/api/sync/jobs/2')) return jsonResponse(newer)
        return jsonResponse({ error: 'unexpected' }, 404)
      }),
    )
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const list = await screen.findByTestId('sync-job-list')
    const rows = within(list).getAllByTestId(/^sync-job-row-/)
    expect(rows.map((row) => row.getAttribute('data-testid'))).toEqual([
      'sync-job-row-2',
      'sync-job-row-1',
    ])
    expect(within(rows[0]).getByText('failed')).toBeInTheDocument()
    expect(within(rows[1]).getByText('done')).toBeInTheDocument()
  })

  it('shows the unreliable verdict variant in the list and detail', async () => {
    const job = makeJob({
      id: 3,
      report: makeReport({ verdict: 'unreliable', classification: 'unreliable', max_drift_ms: 400 }),
    })
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (url.endsWith('/api/sync/jobs')) return jsonResponse({ items: [job] })
        if (url.endsWith('/api/sync/jobs/3')) return jsonResponse(job)
        return jsonResponse({ error: 'unexpected' }, 404)
      }),
    )
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const row = await screen.findByTestId('sync-job-row-3')
    expect(within(row).getByText('unreliable')).toBeInTheDocument()
    const detail = await screen.findByTestId('verdict-badge')
    expect(within(detail).getByText('unreliable')).toBeInTheDocument()
    expect(screen.getByText('classification: unreliable')).toBeInTheDocument()
    expect(screen.getByText('400 ms')).toBeInTheDocument()
  })

  it('posts the selected vault track and library target when executing', async () => {
    const user = userEvent.setup()
    const created = makeJob({ id: 9, status: 'queued', report: undefined })
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.endsWith('/api/vault')) return jsonResponse({ items: vaultItems })
      if (url.includes('/api/imports/search-library')) return jsonResponse({ items: libraryItems })
      if (url.endsWith('/api/sync/jobs') && isMethod(init, 'POST')) {
        return jsonResponse(created, 201)
      }
      if (url.endsWith('/api/sync/jobs')) return jsonResponse({ items: [] })
      if (url.endsWith('/api/sync/jobs/9')) return jsonResponse(created)
      return jsonResponse({ error: 'unexpected' }, 404)
    })
    vi.stubGlobal('fetch', fetchMock)
    renderWithProviders(<SyncPage />, { route: '/sync' })

    await screen.findByText(/Vault is empty|#11/)
    await user.selectOptions(screen.getByLabelText('Vault track'), '11')
    await user.type(screen.getByLabelText('Library target search'), 'Shrek')
    await user.click(await screen.findByTestId('library-result-7'))

    const execute = screen.getByTestId('sync-execute')
    expect(execute).toBeEnabled()
    await user.click(execute)

    await waitFor(() => {
      const post = fetchMock.mock.calls.find(
        ([input, init]) => String(input).endsWith('/api/sync/jobs') && isMethod(init, 'POST'),
      )
      expect(post).toBeTruthy()
      expect(JSON.parse(String(post?.[1]?.body))).toEqual({
        vault_track_id: 11,
        target_library_cache_id: 7,
      })
    })
  })

  it('polls a running job until it settles on done', async () => {
    const running = makeJob({ id: 5, status: 'running', report: undefined, calculated_offset_ms: 0 })
    const done = makeJob({ id: 5, status: 'done' })
    let detailCalls = 0
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (url.endsWith('/api/sync/jobs')) return jsonResponse({ items: [running] })
        if (url.endsWith('/api/sync/jobs/5')) {
          detailCalls += 1
          return jsonResponse(detailCalls === 1 ? running : done)
        }
        return jsonResponse({ error: 'unexpected' }, 404)
      }),
    )
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const status = await screen.findByTestId('sync-status')
    expect(within(status).getByText('running')).toBeInTheDocument()

    await waitFor(() => expect(within(status).getByText('done')).toBeInTheDocument(), {
      timeout: 5_000,
    })
    expect(detailCalls).toBeGreaterThan(1)
    expect(await screen.findByTestId('verdict-badge')).toBeInTheDocument()
  }, 10_000)

  it('renders an SVG dot and quality bar for every checkpoint', async () => {
    const job = makeJob({ id: 6 })
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (url.endsWith('/api/sync/jobs')) return jsonResponse({ items: [job] })
        if (url.endsWith('/api/sync/jobs/6')) return jsonResponse(job)
        return jsonResponse({ error: 'unexpected' }, 404)
      }),
    )
    renderWithProviders(<SyncPage />, { route: '/sync' })

    expect(await screen.findByTestId('checkpoint-chart')).toBeInTheDocument()
    expect(screen.getAllByTestId(/^checkpoint-dot-/)).toHaveLength(5)
    expect(screen.getAllByTestId(/^checkpoint-quality-/)).toHaveLength(5)
    expect(screen.getAllByTestId(/^checkpoint-row-/)).toHaveLength(5)
  })

  it('re-runs a failed job with its original ids', async () => {
    const user = userEvent.setup()
    const failed = makeJob({ id: 4, status: 'failed', report: undefined, error_message: 'no anchor' })
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.endsWith('/api/sync/jobs/4')) return jsonResponse(failed)
      if (url.endsWith('/api/sync/jobs') && isMethod(init, 'POST')) {
        return jsonResponse(makeJob({ id: 8, status: 'queued', report: undefined }), 201)
      }
      if (url.endsWith('/api/sync/jobs')) return jsonResponse({ items: [failed] })
      if (url.endsWith('/api/sync/jobs/8')) {
        return jsonResponse(makeJob({ id: 8, status: 'queued', report: undefined }))
      }
      return jsonResponse({ error: 'unexpected' }, 404)
    })
    vi.stubGlobal('fetch', fetchMock)
    renderWithProviders(<SyncPage />, { route: '/sync' })

    await user.click(await screen.findByRole('button', { name: 'Re-run' }))
    await waitFor(() => {
      const post = fetchMock.mock.calls.find(
        ([input, init]) => String(input).endsWith('/api/sync/jobs') && isMethod(init, 'POST'),
      )
      expect(JSON.parse(String(post?.[1]?.body))).toEqual({
        vault_track_id: 11,
        target_library_cache_id: 7,
      })
    })
  })

  it('shows the published output path when mux wrote to a separate destination', async () => {
    const outputPath = '/output/Shrek (2001) [Remux] [PL].mkv'
    const job = makeJob({ id: 12, output_path: outputPath })
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (url.endsWith('/api/sync/jobs')) return jsonResponse({ items: [job] })
        if (url.endsWith('/api/sync/jobs/12')) return jsonResponse(job)
        return jsonResponse({ error: 'unexpected' }, 404)
      }),
    )
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const row = await screen.findByTestId('sync-job-row-12')
    const rowOutput = within(row).getByTestId('sync-job-output-12')
    expect(rowOutput).toHaveTextContent(`Published path ${outputPath}`)
    // The untouched target is still visible, but only as the secondary line.
    expect(within(row).getByText(/^Target: \/media\/Shrek/)).toBeInTheDocument()

    const detailOutput = await screen.findByTestId('sync-detail-output')
    expect(detailOutput).toHaveTextContent(`Published path: ${outputPath}`)
    expect(screen.getByTestId('sync-detail-target')).toHaveTextContent(
      '/media/Shrek (2001) [Remux].mkv',
    )
  })

  it('falls back to the target path when no output path was published', async () => {
    const job = makeJob({ id: 13 })
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (url.endsWith('/api/sync/jobs')) return jsonResponse({ items: [job] })
        if (url.endsWith('/api/sync/jobs/13')) return jsonResponse(job)
        return jsonResponse({ error: 'unexpected' }, 404)
      }),
    )
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const row = await screen.findByTestId('sync-job-row-13')
    expect(within(row).getByText('/media/Shrek (2001) [Remux].mkv')).toBeInTheDocument()
    expect(within(row).queryByText(/Published path/)).toBeNull()

    const detailTarget = await screen.findByTestId('sync-detail-target')
    expect(detailTarget).toHaveTextContent('/media/Shrek (2001) [Remux].mkv')
    expect(screen.queryByTestId('sync-detail-output')).toBeNull()
  })

  it('shows the analysis mode and why it fell back', async () => {
    const job = makeJob({
      id: 14,
      analysis_source: 'full',
      analysis_fallback: '2-suspect-windows',
    })
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (url.endsWith('/api/sync/jobs')) return jsonResponse({ items: [job] })
        if (url.endsWith('/api/sync/jobs/14')) return jsonResponse(job)
        return jsonResponse({ error: 'unexpected' }, 404)
      }),
    )
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const analysis = await screen.findByTestId('sync-detail-analysis')
    expect(analysis).toHaveTextContent('full (fallback: 2-suspect-windows)')
  })

  it('shows an unknown analysis mode as a dash rather than claiming full', async () => {
    // Jobs analysed before windowing existed have no recorded mode. Rendering
    // "full" for them would be a claim the database never made.
    const job = makeJob({ id: 15 })
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (url.endsWith('/api/sync/jobs')) return jsonResponse({ items: [job] })
        if (url.endsWith('/api/sync/jobs/15')) return jsonResponse(job)
        return jsonResponse({ error: 'unexpected' }, 404)
      }),
    )
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const analysis = await screen.findByTestId('sync-detail-analysis')
    expect(analysis).toHaveTextContent('—')
    expect(analysis).not.toHaveTextContent('full')
  })

  it('renders the match z column and the precision badge for a 0.5.0 report', async () => {
    const job = makeJob({
      id: 16,
      report: makeReport({
        match_z: [27.5, 24.2, 0, 18.4, 26.1],
        precision_ms: 0.125,
        thresholds: {
          stable_spread_ms: 40,
          low_quality: 0.1,
          match_z_maybe: 5,
          match_z_good: 10,
        },
        checkpoints: [
          { position_pct: 10, slice_start_sec: 60, slice_len_sec: 3, offset_ms: 120, correlation_quality: 0.92, offset_sub_ms: 120.06, match_z: 27.5, precision_ms: 0.125 },
          { position_pct: 30, slice_start_sec: 180, slice_len_sec: 3, offset_ms: 118, correlation_quality: 0.88, offset_sub_ms: 118.02, match_z: 24.2, precision_ms: 0.125 },
          { position_pct: 50, slice_start_sec: 300, slice_len_sec: 3, offset_ms: 121, correlation_quality: 0.9, offset_sub_ms: 121, match_z: 0, precision_ms: 0.125 },
          { position_pct: 70, slice_start_sec: 420, slice_len_sec: 3, offset_ms: 119, correlation_quality: 0.86, offset_sub_ms: 119.04, match_z: 18.4, precision_ms: 0.125 },
          { position_pct: 90, slice_start_sec: 540, slice_len_sec: 3, offset_ms: 120, correlation_quality: 0.91, offset_sub_ms: 120.01, match_z: 26.1, precision_ms: 0.125 },
        ],
      }),
    })
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (url.endsWith('/api/sync/jobs')) return jsonResponse({ items: [job] })
        if (url.endsWith('/api/sync/jobs/16')) return jsonResponse(job)
        return jsonResponse({ error: 'unexpected' }, 404)
      }),
    )
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const rows = await screen.findAllByTestId(/^checkpoint-row-/)
    expect(rows).toHaveLength(5)
    expect(within(rows[0].closest('table') as HTMLTableElement).getByText('Match z')).toBeInTheDocument()
    expect(within(rows[0]).getByText('27.5')).toBeInTheDocument()
    expect(within(rows[1]).getByText('24.2')).toBeInTheDocument()
    // match_z = 0 is the documented "not recorded" sentinel, not a score.
    expect(within(rows[2]).getByText('—')).toBeInTheDocument()
    expect(within(rows[4]).getByText('26.1')).toBeInTheDocument()

    const precision = await screen.findByTestId('sync-precision')
    expect(precision).toHaveTextContent('±0.125 ms')

    const dot = screen.getByTestId('checkpoint-dot-0')
    expect(dot.querySelector('title')).toHaveTextContent('z 27.5')
  })

  it('states a hop-quantised offset as hop-quantised instead of sample-accurate', async () => {
    // MFCC answers on a 10 ms hop and reports that as its precision. The badge
    // must name the grid and the matcher — ±10 ms · MFCC — so the number is
    // never read as the waveform matcher's 0.125 ms sample (D3).
    const job = makeJob({
      id: 18,
      report: makeReport({
        precision_ms: 10,
        checkpoints: [
          { position_pct: 10, slice_start_sec: 60, slice_len_sec: 190, offset_ms: 250, correlation_quality: 0.8, technique: 'mfcc', precision_ms: 10 },
          { position_pct: 50, slice_start_sec: 300, slice_len_sec: 190, offset_ms: 250, correlation_quality: 0.75, technique: 'mfcc', precision_ms: 10 },
          { position_pct: 90, slice_start_sec: 540, slice_len_sec: 190, offset_ms: 260, correlation_quality: 0.7, technique: 'mfcc', precision_ms: 10 },
        ],
      }),
    })
    stubJob(job)
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const precision = await screen.findByTestId('sync-precision')
    expect(precision).toHaveTextContent('±10 ms · MFCC')
    expect(precision).not.toHaveTextContent('±0.125 ms')
  })

  it('names MFCC when only one checkpoint of a mixed report sits on the hop', async () => {
    // precision_ms is the coarsest answer in the report, so one MFCC row makes
    // the whole report hop-quantised; naming the matcher is what keeps the
    // waveform rows' sample precision from being claimed for all of them.
    const job = makeJob({
      id: 19,
      report: makeReport({
        precision_ms: 10,
        checkpoints: [
          { position_pct: 10, slice_start_sec: 60, slice_len_sec: 190, offset_ms: 120, correlation_quality: 0.92, technique: 'waveform', precision_ms: 0.125 },
          { position_pct: 50, slice_start_sec: 300, slice_len_sec: 190, offset_ms: 250, correlation_quality: 0.4, technique: 'mfcc', precision_ms: 10 },
        ],
      }),
    })
    stubJob(job)
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const precision = await screen.findByTestId('sync-precision')
    expect(precision).toHaveTextContent('±10 ms · MFCC')
  })

  it('keeps the plain sample-precision badge for a waveform report', async () => {
    const job = makeJob({
      id: 20,
      report: makeReport({
        precision_ms: 0.125,
        checkpoints: [
          { position_pct: 10, slice_start_sec: 60, slice_len_sec: 3, offset_ms: 120, correlation_quality: 0.92, technique: 'waveform', precision_ms: 0.125 },
          { position_pct: 50, slice_start_sec: 300, slice_len_sec: 3, offset_ms: 121, correlation_quality: 0.9, technique: 'waveform', precision_ms: 0.125 },
        ],
      }),
    })
    stubJob(job)
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const precision = await screen.findByTestId('sync-precision')
    // Exactly today's badge: no method suffix and no hop claim.
    expect(precision.textContent).toBe('±0.125 ms')
  })

  it('renders a 0.4.0 report without match z or precision', async () => {
    // Exactly what a job stored before 0.5.0 returns: no match_z array, no
    // precision_ms, and checkpoints without the new keys. The table must still
    // render, with a dash instead of a fabricated score.
    const legacy: SyncReport = {
      verdict: 'ok',
      offsets_ms: [120, 118, 121],
      qualities: [0.92, 0.88, 0.9],
      max_drift_ms: 3,
      classification: 'stable',
      checkpoints: [
        { position_pct: 10, slice_start_sec: 60, slice_len_sec: 3, offset_ms: 120, correlation_quality: 0.92 },
        { position_pct: 50, slice_start_sec: 300, slice_len_sec: 3, offset_ms: 121, correlation_quality: 0.9 },
        { position_pct: 90, slice_start_sec: 540, slice_len_sec: 3, offset_ms: 120, correlation_quality: 0.91 },
      ],
      sync_method: 'correlation',
    }
    const job = makeJob({ id: 17, report: legacy })
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (url.endsWith('/api/sync/jobs')) return jsonResponse({ items: [job] })
        if (url.endsWith('/api/sync/jobs/17')) return jsonResponse(job)
        return jsonResponse({ error: 'unexpected' }, 404)
      }),
    )
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const rows = await screen.findAllByTestId(/^checkpoint-row-/)
    expect(rows).toHaveLength(3)
    expect(within(rows[0].closest('table') as HTMLTableElement).getByText('Match z')).toBeInTheDocument()
    for (const row of rows) expect(within(row).getByText('—')).toBeInTheDocument()
    expect(screen.queryByTestId('sync-precision')).toBeNull()
    expect(screen.queryByText(/^±/)).toBeNull()

    const dot = screen.getByTestId('checkpoint-dot-0')
    expect(dot.querySelector('title')).not.toHaveTextContent('z ')
  })

  it('renders the trend block with the slope, R² and the applied tempo', async () => {
    const job = makeJob({
      id: 21,
      calculated_offset_ms: -355,
      report: makeReport({
        verdict: 'drifting',
        classification: 'drifting',
        fit: {
          shape: 'drift',
          slope_ms_per_sec: 42.7,
          intercept_ms: -340,
          residual_rms_ms: 3.2,
          max_residual_ms: 7.9,
          r2: 0.999,
          points: 6,
          tempo_factor: 0.95904,
          tempo_applied: true,
        },
        application: { mode: 're-encoded', note: 're-encoded (tempo 0.95904)' },
      }),
    })
    stubJob(job)
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const trend = await screen.findByTestId('sync-trend')
    expect(within(trend).getByText('Trend')).toBeInTheDocument()
    expect(within(screen.getByTestId('sync-trend-shape')).getByText('drift')).toBeInTheDocument()
    expect(screen.getByTestId('sync-trend-slope')).toHaveTextContent('+42.7 ms/s')
    expect(screen.getByTestId('sync-trend-r2')).toHaveTextContent('0.999')
    expect(screen.getByTestId('sync-trend-tempo')).toHaveTextContent('tempo 0.9590 · applied')
    // The applied offset is the scaled residual: the intercept divided by the
    // tempo factor, because the encode already stretched the timeline.
    expect(screen.getByText('applied offset: -355 ms')).toBeInTheDocument()

    const row = await screen.findByTestId('sync-job-row-21')
    expect(within(row).getByTestId('sync-job-application-21')).toHaveTextContent(
      're-encoded (tempo 0.95904)',
    )
  })

  it('renders an em-dash trend block for a report stored without a fit', async () => {
    // A 0.4.0 row was never fitted: `fit` is absent, not zeroed. The block must
    // say so with a dash instead of inventing a slope.
    const job = makeJob({ id: 22 })
    stubJob(job)
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const trend = await screen.findByTestId('sync-trend')
    expect(trend).toHaveTextContent('—')
    expect(screen.queryByTestId('sync-trend-slope')).toBeNull()
    expect(screen.queryByTestId('sync-trend-shape')).toBeNull()
    expect(screen.getByTestId('verdict-badge')).toBeInTheDocument()
    expect(screen.getByTestId('checkpoint-chart')).toBeInTheDocument()
    expect(screen.queryByTestId('fit-line')).toBeNull()
  })

  it('shows the boundary when the shape is a cut', async () => {
    const job = makeJob({
      id: 23,
      report: makeReport({
        verdict: 'drifting',
        classification: 'drifting',
        fit: {
          shape: 'cut',
          slope_ms_per_sec: 1.4,
          intercept_ms: 120,
          r2: 0.51,
          points: 6,
          cut_at_sec: 1800,
          cut_uncertainty_sec: 2.5,
        },
      }),
    })
    stubJob(job)
    renderWithProviders(<SyncPage />, { route: '/sync' })

    expect(within(await screen.findByTestId('sync-trend-shape')).getByText('cut')).toBeInTheDocument()
    expect(screen.getByTestId('sync-trend-cut')).toHaveTextContent('cut at 1800.0 s ± 2.5 s')
    // A cut is not a drift: no tempo was computed, so the row says nothing.
    expect(screen.getByTestId('sync-trend-tempo')).toHaveTextContent('—')
    expect(screen.queryByTestId('sync-job-application-23')).toBeNull()
  })

  it('draws the fitted line through the checkpoint dots when the report carries a fit', async () => {
    const offsets = [1230, 1290, 1380, 1520, 1640]
    const job = makeJob({
      id: 24,
      report: makeReport({
        verdict: 'drifting',
        classification: 'drifting',
        offsets_ms: offsets,
        max_drift_ms: 410,
        checkpoints: [60, 180, 300, 420, 540].map((sliceStart, index) => ({
          position_pct: 10 + index * 20,
          slice_start_sec: sliceStart,
          slice_len_sec: 3,
          offset_ms: offsets[index],
          correlation_quality: 0.9,
        })),
        fit: {
          shape: 'drift',
          slope_ms_per_sec: 0.9,
          intercept_ms: 1180,
          r2: 0.995,
          points: 5,
          tempo_factor: 1.0009,
          tempo_applied: false,
        },
      }),
    })
    stubJob(job)
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const line = await screen.findByTestId('fit-line')
    const points = (line.getAttribute('points') ?? '').trim().split(/\s+/)
    expect(points).toHaveLength(5)

    // Same x as its dot; y from the reported line, on the chart's own scale.
    const plotHeight = 220 - 18 - 56
    const maxOffset = 1640
    const expectedY = (sliceStart: number) =>
      18 + plotHeight / 2 - ((1180 + 0.9 * sliceStart) / maxOffset) * (plotHeight / 2)
    const dot = screen.getByTestId('checkpoint-dot-0')
    const [x0, y0] = points[0].split(',').map(Number)
    expect(x0).toBeCloseTo(Number(dot.getAttribute('cx')), 5)
    expect(y0).toBeCloseTo(expectedY(60), 5)
    expect(Number(points[4].split(',')[1])).toBeCloseTo(expectedY(540), 5)

    // Dots keep their own geometry (positions and offsets untouched).
    expect(Number(dot.getAttribute('cy'))).toBeCloseTo(
      18 + plotHeight / 2 - (1230 / maxOffset) * (plotHeight / 2),
      5,
    )
    expect(dot.getAttribute('data-offset-ms')).toBe('1230')
  })

  it('omits the fitted line when the fit is insufficient', async () => {
    const job = makeJob({
      id: 25,
      report: makeReport({
        verdict: 'unreliable',
        classification: 'unreliable',
        fit: {
          shape: 'insufficient',
          slope_ms_per_sec: 0.2,
          intercept_ms: 120,
          r2: 0.4,
          points: 2,
        },
      }),
    })
    stubJob(job)
    const { container } = renderWithProviders(<SyncPage />, { route: '/sync' })

    const trend = await screen.findByTestId('sync-trend')
    expect(within(within(trend).getByTestId('sync-trend-shape')).getByText('insufficient')).toBeInTheDocument()
    expect(within(container).queryByTestId('fit-line')).toBeNull()
    expect(within(container).getAllByTestId(/^checkpoint-dot-/)).toHaveLength(5)
  })

  it('hides the precision badge for the zero sentinel while keeping match z', async () => {
    // A stored 0 means "not recorded", so it must not render as ±0 ms.
    const job = makeJob({
      id: 26,
      report: makeReport({
        precision_ms: 0,
        match_z: [27.5, 24.2, 0, 18.4, 26.1],
        checkpoints: [27.5, 24.2, 0, 18.4, 26.1].map((z, index) => ({
          position_pct: 10 + index * 20,
          slice_start_sec: 60 + index * 120,
          slice_len_sec: 3,
          offset_ms: 120,
          correlation_quality: 0.9,
          match_z: z,
          precision_ms: 0,
        })),
      }),
    })
    stubJob(job)
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const rows = await screen.findAllByTestId(/^checkpoint-row-/)
    expect(within(rows[0]).getByText('27.5')).toBeInTheDocument()
    expect(within(rows[1]).getByText('24.2')).toBeInTheDocument()
    expect(within(rows[2]).getByText('—')).toBeInTheDocument()
    expect(screen.queryByTestId('sync-precision')).toBeNull()
    expect(screen.queryByText(/^±/)).toBeNull()
  })

  it('shows which method answered each checkpoint', async () => {
    const job = makeJob({
      id: 31,
      report: makeReport({
        checkpoints: [
          { position_pct: 10, slice_start_sec: 60, slice_len_sec: 3, offset_ms: 120, correlation_quality: 0.92, technique: 'waveform' },
          { position_pct: 50, slice_start_sec: 300, slice_len_sec: 3, offset_ms: 121, correlation_quality: 0.8, technique: 'mfcc' },
          // A checkpoint whose stored answer names no method: a dash, not a guess.
          { position_pct: 90, slice_start_sec: 540, slice_len_sec: 3, offset_ms: 120, correlation_quality: 0.9 },
        ],
      }),
    })
    stubJob(job)
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const rows = await screen.findAllByTestId(/^checkpoint-row-/)
    expect(within(rows[0].closest('table') as HTMLTableElement).getByText('Method')).toBeInTheDocument()
    expect(screen.getByTestId('checkpoint-method-0')).toHaveTextContent('Waveform')
    expect(screen.getByTestId('checkpoint-method-1')).toHaveTextContent('MFCC')
    expect(screen.getByTestId('checkpoint-method-2')).toHaveTextContent('—')
    // One method per checkpoint is not worth a disclosure.
    expect(screen.queryByTestId('checkpoint-attempts-toggle-0')).toBeNull()
  })

  it('expands a checkpoint to show both attempts and why the second method ran', async () => {
    const user = userEvent.setup()
    const job = makeJob({
      id: 32,
      report: makeReport({
        strategy: {
          preset: 'maximum_accuracy',
          techniques: [
            { id: 'waveform', enabled: true, when: 'always', min_confidence: 0.1, min_match_z: 5 },
            { id: 'mfcc', enabled: true, when: 'below_confidence', min_confidence: 0.15, min_match_z: 8 },
          ],
        },
        checkpoints: [
          {
            position_pct: 10,
            slice_start_sec: 60,
            slice_len_sec: 3,
            offset_ms: 120,
            correlation_quality: 0.8,
            technique: 'mfcc',
            attempts: [
              { technique: 'waveform', offset_ms: 118, quality: 0.05, match_z: 4.2, precision_ms: 0.125, accepted: false },
              { technique: 'mfcc', offset_ms: 120, quality: 0.8, match_z: 12, precision_ms: 10, accepted: true },
            ],
          },
        ],
      }),
    })
    stubJob(job)
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const row = await screen.findByTestId('checkpoint-row-0')
    expect(within(row).getByTestId('checkpoint-method-0')).toHaveTextContent('MFCC')
    // Collapsed until asked: the row stays one line on the happy path.
    expect(screen.queryByTestId('checkpoint-attempts-0')).toBeNull()

    await user.click(screen.getByTestId('checkpoint-attempts-toggle-0'))

    const disclosure = await screen.findByTestId('checkpoint-attempts-0')
    const attempts = within(disclosure).getAllByTestId(/^checkpoint-attempt-0-/)
    expect(attempts).toHaveLength(2)
    expect(attempts[0]).toHaveTextContent('Waveform')
    expect(attempts[0]).toHaveTextContent('confidence 0.05')
    expect(attempts[0]).toHaveTextContent('rejected')
    expect(attempts[0]).toHaveTextContent('below the confidence gate (0.10)')
    expect(attempts[0]).toHaveTextContent('peak under the z gate (5.0σ)')
    expect(attempts[1]).toHaveTextContent('MFCC')
    expect(attempts[1]).toHaveTextContent('accepted')
  })

  it('labels the shipped strategy as Balanced and lists its steps in order', async () => {
    const job = makeJob({
      id: 33,
      report: makeReport({
        strategy: {
          preset: 'balanced',
          techniques: [
            { id: 'waveform', enabled: true, when: 'always', min_confidence: 0.1, min_match_z: 5 },
          ],
        },
      }),
    })
    stubJob(job)
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const summary = await screen.findByTestId('sync-strategy-summary')
    expect(summary).toHaveTextContent('Balanced')
    expect(summary).toHaveTextContent('Waveform always')
    // Nothing to note on the shipped strategy.
    expect(screen.queryByTestId('sync-strategy-notes')).toBeNull()
  })

  it('labels an edited strategy as Custom', async () => {
    const job = makeJob({
      id: 34,
      report: makeReport({
        strategy: {
          preset: 'custom',
          techniques: [
            { id: 'waveform', enabled: true, when: 'always', min_confidence: 0.1, min_match_z: 5 },
            { id: 'mfcc', enabled: true, when: 'below_confidence', min_confidence: 0.15, min_match_z: 8 },
          ],
        },
      }),
    })
    stubJob(job)
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const summary = await screen.findByTestId('sync-strategy-summary')
    const text = summary.textContent ?? ''
    expect(summary).toHaveTextContent('Custom')
    expect(summary).toHaveTextContent('when the previous step was unsure')
    expect(text.indexOf('Waveform')).toBeLessThan(text.indexOf('MFCC'))
  })

  it('renders the report notes inline', async () => {
    const job = makeJob({
      id: 35,
      report: makeReport({
        strategy: {
          preset: 'maximum_accuracy',
          techniques: [
            { id: 'waveform', enabled: true, when: 'always', min_confidence: 0.1, min_match_z: 5 },
            { id: 'mfcc', enabled: true, when: 'below_confidence', min_confidence: 0.15, min_match_z: 8 },
          ],
        },
        notes: ['mfcc is configured but this build has no engine for it'],
      }),
    })
    stubJob(job)
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const notes = await screen.findByTestId('sync-strategy-notes')
    expect(notes).toHaveTextContent('mfcc is configured but this build has no engine for it')
    expect(within(notes).getAllByRole('listitem')).toHaveLength(1)
  })

  it('renders a 0.4.0 report without a method column or a strategy summary', async () => {
    // A stored 0.4.0 report has no strategy, no notes and no attempts, so the
    // table keeps its five columns and nothing new appears above it.
    const legacy: SyncReport = {
      verdict: 'ok',
      offsets_ms: [120, 118, 121],
      qualities: [0.92, 0.88, 0.9],
      max_drift_ms: 3,
      classification: 'stable',
      checkpoints: [
        { position_pct: 10, slice_start_sec: 60, slice_len_sec: 3, offset_ms: 120, correlation_quality: 0.92 },
        { position_pct: 50, slice_start_sec: 300, slice_len_sec: 3, offset_ms: 121, correlation_quality: 0.9 },
        { position_pct: 90, slice_start_sec: 540, slice_len_sec: 3, offset_ms: 120, correlation_quality: 0.91 },
      ],
      sync_method: 'correlation',
    }
    const job = makeJob({ id: 36, report: legacy })
    stubJob(job)
    renderWithProviders(<SyncPage />, { route: '/sync' })

    const rows = await screen.findAllByTestId(/^checkpoint-row-/)
    expect(rows).toHaveLength(3)
    const table = rows[0].closest('table') as HTMLTableElement
    expect(within(table).queryByText('Method')).toBeNull()
    expect(table.querySelectorAll('thead th')).toHaveLength(5)
    expect(screen.queryByTestId(/^checkpoint-method-/)).toBeNull()
    expect(screen.queryByTestId(/^checkpoint-attempt/)).toBeNull()
    expect(screen.queryByTestId('sync-strategy-summary')).toBeNull()
    expect(screen.queryByTestId('sync-strategy-notes')).toBeNull()
    expect(within(table).getByText('Match z')).toBeInTheDocument()
    expect(screen.getByTestId('sync-trend')).toBeInTheDocument()
    expect(screen.getByTestId('checkpoint-chart')).toBeInTheDocument()
  })
})
