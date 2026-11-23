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
})
