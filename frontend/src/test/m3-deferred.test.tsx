import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { SettingsPage } from '../pages/Settings'
import { isMethod, jsonResponse, renderWithProviders } from '../test/helpers'

type SettingsBody = { settings: Record<string, unknown> }

function mockSettings(
  settings: Record<string, unknown>,
): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.includes('/api/settings') && isMethod(init, 'GET')) {
      return jsonResponse({ settings })
    }
    if (url.includes('/api/settings') && isMethod(init, 'PUT')) {
      return jsonResponse({ settings })
    }
    return jsonResponse({ error: 'unexpected' }, 404)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function lastPut(fetchMock: ReturnType<typeof vi.fn>): SettingsBody {
  const put = fetchMock.mock.calls.find(([, init]) => isMethod(init, 'PUT'))
  expect(put).toBeDefined()
  return JSON.parse(String(put?.[1]?.body)) as SettingsBody
}

describe('donor patterns settings card (imports.options)', () => {
  it('renders the stored patterns and saves an added one via the settings PUT', async () => {
    const user = userEvent.setup()
    const fetchMock = mockSettings({
      'imports.options': { donor_patterns: ['PL.DUB'] },
    })
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    expect(await screen.findByRole('heading', { name: 'Donor patterns' })).toBeInTheDocument()
    expect(screen.getByText('PL.DUB')).toBeInTheDocument()

    await user.type(screen.getByLabelText('Add donor pattern'), '*LEKTOR.PL*')
    const card = screen.getByRole('heading', { name: 'Donor patterns' }).closest('section')
    expect(card).not.toBeNull()
    await user.click(within(card as HTMLElement).getByRole('button', { name: 'Add' }))
    await user.click(screen.getByLabelText('Save Donor patterns'))

    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true)
    })
    const body = lastPut(fetchMock)
    expect(Object.keys(body.settings)).toEqual(['imports.options'])
    expect(body.settings['imports.options']).toEqual({
      donor_patterns: ['PL.DUB', '*LEKTOR.PL*'],
    })
    expect(await screen.findByText('saved')).toBeInTheDocument()
  })

  it('removes a pattern before saving', async () => {
    const user = userEvent.setup()
    const fetchMock = mockSettings({
      'imports.options': { donor_patterns: ['PL.DUB', '*LEKTOR.PL*'] },
    })
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    expect(await screen.findByRole('heading', { name: 'Donor patterns' })).toBeInTheDocument()
    await user.click(screen.getByLabelText('Remove PL.DUB'))
    await user.click(screen.getByLabelText('Save Donor patterns'))

    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true)
    })
    expect(lastPut(fetchMock).settings['imports.options']).toEqual({
      donor_patterns: ['*LEKTOR.PL*'],
    })
  })
})

describe('watch scanner restart notice', () => {
  it('warns after a storage.paths save that changes watch_dir', async () => {
    const user = userEvent.setup()
    mockSettings({
      'storage.paths': {
        watch_dir: '/watch',
        vault_dir: '/vault',
        work_dir: '/work',
        media_roots: [],
      },
    })
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()
    expect(screen.queryByText(/restart required to apply watch changes/i)).not.toBeInTheDocument()

    const watch = screen.getByLabelText('Watch folder')
    await user.clear(watch)
    await user.type(watch, '/new/watch')
    await user.click(screen.getByLabelText('Save Storage paths'))

    expect(
      await screen.findByText(/restart required to apply watch changes/i),
    ).toBeInTheDocument()
  })

  it('does not warn when only vault_dir changes', async () => {
    const user = userEvent.setup()
    mockSettings({
      'storage.paths': {
        watch_dir: '/watch',
        vault_dir: '/vault',
        work_dir: '/work',
        media_roots: [],
      },
    })
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()
    const vault = screen.getByLabelText('Vault folder')
    await user.clear(vault)
    await user.type(vault, '/new/vault')
    await user.click(screen.getByLabelText('Save Storage paths'))

    await waitFor(() => {
      expect(screen.getByText('saved')).toBeInTheDocument()
    })
    expect(screen.queryByText(/restart required to apply watch changes/i)).not.toBeInTheDocument()
  })

  it('applies the scan interval without a restart notice', async () => {
    const user = userEvent.setup()
    mockSettings({
      'watch.options': { scan_interval_sec: 10, stability_check_interval_sec: 4 },
    })
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    expect(await screen.findByRole('heading', { name: 'Watch options' })).toBeInTheDocument()
    const interval = screen.getByLabelText('Scan interval (s)')
    await user.clear(interval)
    await user.type(interval, '30')
    await user.click(screen.getByLabelText('Save Watch options'))

    await waitFor(() => {
      expect(screen.getByText('saved')).toBeInTheDocument()
    })
    // The scanner re-reads the interval every loop, so no restart is needed.
    expect(screen.queryByText(/restart required to apply watch changes/i)).not.toBeInTheDocument()
  })

  it('warns after target.tag_filter changes', async () => {
    const user = userEvent.setup()
    mockSettings({
      'target.tag_filter': {
        required_filename_tags: ['[ArcA310]'],
        tag_match_mode: 'ANY',
        tag_case_sensitive: false,
        excluded_filename_tags: [],
        enforce_tag_filter: true,
      },
    })
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    expect(await screen.findByRole('heading', { name: 'Process only files tagged as …' })).toBeInTheDocument()
    await user.click(screen.getByRole('switch', { name: 'Case sensitive' }))
    await user.click(screen.getByLabelText('Save Process only files tagged as …'))

    expect(
      await screen.findByText(/restart required to apply watch changes/i),
    ).toBeInTheDocument()
  })
})
