import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { VaultPage } from '../pages/Vault'
import { isMethod, jsonResponse, renderWithProviders } from '../test/helpers'

const TRACKS = {
  items: [
    {
      id: 7,
      title: 'Shrek',
      media_type: 'movie',
      season_number: null,
      episode_number: null,
      filename: 'Shrek.2001.pl.mka',
      file_size: 1024,
      sha256: '0123456789abcdef',
      language: 'pl',
      variant: 'dubbing',
      codec: 'eac3',
      channels: '5.1',
      channel_count: 6,
      source_release_name: 'Shrek.2001.PL.DUB.mkv',
      created_at: '2026-11-22 10:00:00',
    },
  ],
}

const SOURCE = '/vault/Shrek.2001.pl.mka'
const DESTINATION = '/vault/Shrek (2001)/Shrek (2001) [Dubbing EAC3 5.1].pl.mka'
const UNCHANGED_DESTINATION = '/vault/Minions/Season 01/Minions - S01E02 [Dubbing AC3 2.0].pl.mka'

const CLEAN_PLAN = {
  plan_id: 'sha256:abc123',
  expires_at: '2026-11-27T20:15:00Z',
  files: [
    {
      track_id: 7,
      media_id: 3,
      source: SOURCE,
      destination: DESTINATION,
      changed: true,
      reason: 'template_change',
      conflict: null,
    },
    {
      track_id: 8,
      media_id: 4,
      source: '/vault/Minions.S01E02.pl.mka',
      destination: UNCHANGED_DESTINATION,
      changed: false,
      reason: 'unchanged',
      conflict: null,
    },
  ],
  conflicts: [],
  impact: {
    renamed: 1,
    unchanged: 1,
    jobs_referencing_tracks: 2,
    jobs_blocked: 0,
    library_rows_affected: 0,
  },
}

/** A conflicted plan: two tracks want one name, so the backend sends no plan_id. */
const CONFLICTED_PLAN = {
  ...CLEAN_PLAN,
  plan_id: undefined,
  expires_at: undefined,
  files: [
    { ...CLEAN_PLAN.files[0], destination: '/vault/Shrek (2001)/Shrek (2001).pl.mka', conflict: 'destination_duplicate' },
    {
      ...CLEAN_PLAN.files[1],
      source: '/vault/Minions.S01E02.pl.mka',
      destination: '/vault/Shrek (2001)/Shrek (2001).pl.mka',
      changed: true,
      reason: 'template_change',
      conflict: 'destination_duplicate',
    },
  ],
  conflicts: [
    {
      kind: 'destination_duplicate',
      destination: '/vault/Shrek (2001)/Shrek (2001).pl.mka',
      track_ids: [7, 8],
    },
  ],
}

type Handlers = {
  preview?: () => Response
  apply?: () => Response
  runs?: () => Response
}

function mockOrganise(handlers: Handlers = {}) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.includes('/api/vault/organise/preview') && isMethod(init, 'POST')) {
      return handlers.preview?.() ?? jsonResponse(CLEAN_PLAN)
    }
    if (url.includes('/api/vault/organise/apply') && isMethod(init, 'POST')) {
      return (
        handlers.apply?.() ??
        jsonResponse({ run_id: 1, status: 'applied', file_count: 1, renamed: 1, unchanged: 1 })
      )
    }
    if (url.includes('/api/vault/organise/runs') && isMethod(init, 'GET')) {
      return handlers.runs?.() ?? jsonResponse({ items: [] })
    }
    if (url.endsWith('/api/vault') && isMethod(init, 'GET')) {
      return jsonResponse(TRACKS)
    }
    return jsonResponse({ error: 'unexpected' }, 404)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

async function runPreview(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: 'Preview changes' }))
  // The table only exists after the preview response lands.
  await screen.findByRole('table', { name: 'Planned renames' })
}

/** Preview, open the confirmation and press the apply button inside it. */
async function confirmApply(user: ReturnType<typeof userEvent.setup>, label: RegExp) {
  const apply = await screen.findByRole('button', { name: /^Apply/ })
  await user.click(apply)
  const dialog = await screen.findByRole('dialog')
  await user.click(within(dialog).getByRole('button', { name: label }))
}

describe('vault organise preview and apply (#47)', () => {
  it('lists every source and destination and refuses a plan with a conflict', async () => {
    const user = userEvent.setup()
    mockOrganise({ preview: () => jsonResponse(CONFLICTED_PLAN) })
    renderWithProviders(<VaultPage />, { route: '/vault' })

    // Apply is inert until a preview exists.
    expect(await screen.findByRole('button', { name: 'Apply changes' })).toBeDisabled()

    await runPreview(user)

    // One row per track, in plain language.
    expect(await screen.findByText(SOURCE)).toBeInTheDocument()
    expect(screen.getAllByText('/vault/Shrek (2001)/Shrek (2001).pl.mka').length).toBeGreaterThan(0)
    expect(screen.getAllByText('cannot move').length).toBe(2)
    expect(screen.getAllByText('Another track would get this same name.').length).toBe(2)

    // The conflict is called out above the table, with the tracks involved.
    expect(
      screen.getByText('Two tracks would end up with the same file name.'),
    ).toBeInTheDocument()
    expect(screen.getByText(/Tracks 7, 8/)).toBeInTheDocument()

    // The spec forbids applying a conflicted plan: no plan_id is sent.
    const apply = screen.getByRole('button', { name: /^Apply/ })
    expect(apply).toBeDisabled()
    expect(
      screen.getByText('Fix the conflicts above first. Nothing can be applied while a name is unusable.'),
    ).toBeInTheDocument()
  })

  it('applies the previewed plan by plan_id after a confirmation that repeats the count', async () => {
    const user = userEvent.setup()
    const fetchMock = mockOrganise()
    renderWithProviders(<VaultPage />, { route: '/vault' })

    await runPreview(user)

    // Nothing moves on a preview alone.
    expect(fetchMock.mock.calls.some(([url]) => String(url).includes('/organise/apply'))).toBe(false)

    const apply = await screen.findByRole('button', { name: 'Apply 1 rename' })
    expect(apply).toBeEnabled()
    await user.click(apply)

    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(/1 file will move inside \/vault/i)).toBeInTheDocument()
    await user.click(within(dialog).getByRole('button', { name: 'Yes, move 1 file' }))

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(
        ([url, init]) => String(url).includes('/organise/apply') && isMethod(init, 'POST'),
      )
      expect(call).toBeDefined()
      expect(JSON.parse(String(call?.[1]?.body))).toEqual({ plan_id: 'sha256:abc123', confirm: true })
    })

    expect(await screen.findByText(/Done: 1 file moved/)).toBeInTheDocument()
  })

  it('asks for a fresh preview when the vault changed since the preview', async () => {
    const user = userEvent.setup()
    mockOrganise({
      apply: () =>
        jsonResponse({ code: 'stale_plan', error: 'the vault changed since the preview; preview again' }, 409),
    })
    renderWithProviders(<VaultPage />, { route: '/vault' })

    await runPreview(user)
    await confirmApply(user, /^Yes, move/)

    expect(
      await screen.findByText(/The vault changed since the preview\. Preview again/i),
    ).toBeInTheDocument()
    // The stale plan is dropped: applying again needs a new preview.
    expect(screen.getByRole('button', { name: 'Apply changes' })).toBeDisabled()
  })

  it('explains a muxing job instead of a bare 409', async () => {
    const user = userEvent.setup()
    mockOrganise({
      apply: () =>
        jsonResponse(
          { code: 'jobs_running', count: 1, error: 'a sync job is analysing or muxing right now' },
          409,
        ),
    })
    renderWithProviders(<VaultPage />, { route: '/vault' })

    await runPreview(user)
    await confirmApply(user, /^Yes, move/)

    expect(await screen.findByText(/A sync job is muxing right now\./)).toBeInTheDocument()
    expect(screen.getByText(/nothing was changed/i)).toBeInTheDocument()
  })

  it('lists the paths to inspect when the apply could not be undone', async () => {
    const user = userEvent.setup()
    mockOrganise({
      apply: () =>
        jsonResponse(
          {
            code: 'manual_attention',
            error: 'the rename partly failed and could not be undone',
            files: ['/vault/Angry Birds (2016)/Angry Birds (2016) [Lektor AC3 5.1].pl.mka'],
          },
          500,
        ),
    })
    renderWithProviders(<VaultPage />, { route: '/vault' })

    await runPreview(user)
    await confirmApply(user, /^Yes, move/)

    const attention = await screen.findByTestId('organise-attention')
    expect(
      within(attention).getByText(
        '/vault/Angry Birds (2016)/Angry Birds (2016) [Lektor AC3 5.1].pl.mka',
      ),
    ).toBeInTheDocument()
    expect(within(attention).getByText(/could not be undone/i)).toBeInTheDocument()
  })

  it('names the files to inspect when a run needs attention (#127)', async () => {
    const FAILED_SOURCE = '/vault/Minions.S01E02.pl.mka'
    const FAILED_DESTINATION = '/vault/Minions/Season 01/Minions - S01E02 [Dubbing AC3 2.0].pl.mka'
    mockOrganise({
      runs: () =>
        jsonResponse({
          items: [
            {
              id: 3,
              started_at: '2026-11-27T20:10:00Z',
              finished_at: '2026-11-27T20:10:04Z',
              status: 'needs_attention',
              plan_hash: 'sha256:def456',
              file_count: 40,
              detail: 'stage=rename: permission denied',
              files: [
                {
                  source_path: SOURCE,
                  destination_path: DESTINATION,
                  result: 'reconciled',
                },
                {
                  source_path: FAILED_SOURCE,
                  destination_path: FAILED_DESTINATION,
                  result: 'failed',
                },
              ],
            },
            {
              // An applied row is the ordinary case: it must stay one row even
              // if a file list ever came back with it.
              id: 1,
              started_at: '2026-11-27T19:00:00Z',
              finished_at: '2026-11-27T19:00:02Z',
              status: 'applied',
              plan_hash: 'sha256:abc123',
              file_count: 41,
              files: [
                {
                  source_path: '/vault/Other.2001.pl.mka',
                  destination_path: '/vault/Other (2001)/Other (2001).pl.mka',
                  result: 'reconciled',
                },
              ],
            },
          ],
        }),
    })
    renderWithProviders(<VaultPage />, { route: '/vault' })

    const attention = await screen.findByTestId('organise-run-files-3')
    // The reconciled file is at its destination, so that is the path to check.
    expect(within(attention).getByText(DESTINATION)).toBeInTheDocument()
    expect(within(attention).getByText('moved, database pointed at the new path')).toBeInTheDocument()
    // The failed file never moved, so its source is the path to check — and the
    // destination is named too, because the row may mean the database update
    // alone failed after the file had already moved.
    expect(within(attention).getByText(FAILED_SOURCE)).toBeInTheDocument()
    expect(within(attention).getByText('never moved')).toBeInTheDocument()
    expect(within(attention).getByText(FAILED_DESTINATION)).toBeInTheDocument()
    expect(within(attention).getByText(/or already at/i)).toBeInTheDocument()
    expect(within(attention).getByText(/if the move happened/i)).toBeInTheDocument()
    expect(within(attention).getByText(/check these paths/i)).toBeInTheDocument()
    // The paths replace the Logs pointer rather than sitting next to it.
    expect(within(attention).queryByText(/Logs page/i)).toBeNull()

    const clean = screen.getByTestId('organise-run-applied-1')
    expect(within(clean).getByText('Files moved')).toBeInTheDocument()
    expect(within(clean).queryByText('/vault/Other (2001)/Other (2001).pl.mka')).toBeNull()
    expect(within(clean).queryByText(/moved, database pointed/i)).toBeNull()
    expect(within(clean).queryByText(/check these paths/i)).toBeNull()
  })

  it('renders a run that needs attention differently from a clean run', async () => {
    mockOrganise({
      runs: () =>
        jsonResponse({
          items: [
            {
              id: 2,
              started_at: '2026-11-27T20:10:00Z',
              finished_at: '2026-11-27T20:10:04Z',
              status: 'needs_attention',
              plan_hash: 'sha256:def456',
              file_count: 3,
              detail: 'stage=sql: database is locked',
            },
            {
              id: 1,
              started_at: '2026-11-27T19:00:00Z',
              finished_at: '2026-11-27T19:00:02Z',
              status: 'applied',
              plan_hash: 'sha256:abc123',
              file_count: 41,
            },
          ],
        }),
    })
    renderWithProviders(<VaultPage />, { route: '/vault' })

    const attention = await screen.findByTestId('organise-run-needs_attention-2')
    expect(within(attention).getByText(/Needs your attention/)).toBeInTheDocument()
    expect(within(attention).getByText(/check the file paths this run logged/i)).toBeInTheDocument()

    const clean = screen.getByTestId('organise-run-applied-1')
    expect(within(clean).getByText('Files moved')).toBeInTheDocument()
    expect(within(clean).queryByText(/Needs your attention/)).toBeNull()
    expect(within(clean).queryByText(/check the file paths/i)).toBeNull()
  })

  it('shows a failed file at its source with a fallback destination', async () => {
    const FAILED_SOURCE = '/vault/Angry Birds.2016.pl.mka'
    const FAILED_DESTINATION = '/vault/Angry Birds (2016)/Angry Birds (2016) [Lektor AC3 5.1].pl.mka'
    mockOrganise({
      runs: () =>
        jsonResponse({
          items: [
            {
              id: 4,
              started_at: '2026-11-27T20:10:00Z',
              finished_at: '2026-11-27T20:10:04Z',
              status: 'needs_attention',
              plan_hash: 'sha256:ghi789',
              file_count: 2,
              detail: 'stage=sql: database is locked',
              files: [
                {
                  source_path: FAILED_SOURCE,
                  destination_path: FAILED_DESTINATION,
                  result: 'failed',
                },
              ],
            },
          ],
        }),
    })
    renderWithProviders(<VaultPage />, { route: '/vault' })

    const files = await screen.findByTestId('organise-run-files-4')
    // Both paths are named: the source is where the file should be, the
    // destination is where it is if only the database update failed.
    expect(within(files).getByText(FAILED_SOURCE)).toBeInTheDocument()
    expect(within(files).getByText('never moved')).toBeInTheDocument()
    expect(within(files).getByText(FAILED_DESTINATION)).toBeInTheDocument()
    expect(within(files).getByText(/or already at/i)).toBeInTheDocument()
    expect(within(files).getByText(/database update failed/i)).toBeInTheDocument()
  })

  it('shows only the source for a failed file with no destination', async () => {
    const FAILED_SOURCE = '/vault/Orphan.2001.pl.mka'
    mockOrganise({
      runs: () =>
        jsonResponse({
          items: [
            {
              id: 5,
              started_at: '2026-11-27T20:10:00Z',
              finished_at: '2026-11-27T20:10:04Z',
              status: 'needs_attention',
              plan_hash: 'sha256:jkl012',
              file_count: 1,
              detail: 'stage=rename: permission denied',
              files: [{ source_path: FAILED_SOURCE, destination_path: '', result: 'failed' }],
            },
          ],
        }),
    })
    renderWithProviders(<VaultPage />, { route: '/vault' })

    const files = await screen.findByTestId('organise-run-files-5')
    expect(within(files).getByText(FAILED_SOURCE)).toBeInTheDocument()
    expect(within(files).getByText('never moved')).toBeInTheDocument()
    // No destination to fall back to: no second line, no repeated path.
    expect(within(files).queryByText(/or already at/i)).toBeNull()
  })

  it('leaves a reconciled file at its destination alone', async () => {
    mockOrganise({
      runs: () =>
        jsonResponse({
          items: [
            {
              id: 6,
              started_at: '2026-11-27T20:10:00Z',
              finished_at: '2026-11-27T20:10:04Z',
              status: 'needs_attention',
              plan_hash: 'sha256:mno345',
              file_count: 2,
              detail: 'stage=sql: database is locked',
              files: [{ source_path: SOURCE, destination_path: DESTINATION, result: 'reconciled' }],
            },
          ],
        }),
    })
    renderWithProviders(<VaultPage />, { route: '/vault' })

    const files = await screen.findByTestId('organise-run-files-6')
    expect(within(files).getByText(DESTINATION)).toBeInTheDocument()
    expect(within(files).getByText('moved, database pointed at the new path')).toBeInTheDocument()
    expect(within(files).queryByText(SOURCE)).toBeNull()
    expect(within(files).queryByText(/or already at/i)).toBeNull()
  })
})
