import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { SettingsPage } from '../pages/Settings'
import { isMethod, jsonResponse, renderWithProviders } from '../test/helpers'

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

// U11: the three donor markers are fixed in the backend, so the card that let
// an operator edit them is gone. A legacy appliance can still serve the row; it
// must not resurrect a control nothing reads any more.
describe('donor markers are fixed (U11)', () => {
  it('renders no donor-pattern card or control, even when a legacy row is served', async () => {
    mockSettings({ 'imports.options': { donor_patterns: ['PL.DUB'] } })
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Donor patterns' })).not.toBeInTheDocument()
    expect(screen.queryByLabelText('Add donor pattern')).not.toBeInTheDocument()
    expect(screen.queryByLabelText('Save Donor patterns')).not.toBeInTheDocument()
    expect(document.getElementById('settings-imports-options')).toBeNull()
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
    const vault = screen.getByLabelText('Audio Vault')
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

  it('applies a target.tag_filter edit without a restart notice', async () => {
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

    await waitFor(() => {
      expect(screen.getByText('saved')).toBeInTheDocument()
    })
    // The gate re-reads target.tag_filter per classification, so no restart.
    expect(screen.queryByText(/restart required to apply watch changes/i)).not.toBeInTheDocument()
  })
})
