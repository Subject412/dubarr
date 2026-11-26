import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import App from '../App'
import { BrowseModal } from '../components/BrowseModal'
import { UNAUTHORIZED_EVENT } from '../lib/api'
import { SettingsPage } from '../pages/Settings'
import { SetupPage } from '../pages/Setup'
import { isMethod, jsonResponse, renderWithProviders } from './helpers'

const SYNC_DEFAULTS = {
  max_offset_sec: 180,
  checkpoint_positions_pct: [10],
  checkpoint_window_sec: 150,
  preview_positions_pct: [10],
  preview_window_sec: 60,
}

function statusFetch(overrides: Record<string, (init?: RequestInit) => Response | Promise<Response>> = {}) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.includes('/api/auth/status')) {
      return jsonResponse({ initialized: true, authenticated: true, keyring_available: true })
    }
    for (const [fragment, handler] of Object.entries(overrides)) {
      if (url.includes(fragment)) return handler(init)
    }
    return jsonResponse({ error: 'unexpected' }, 404)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('session expiry (IMP-2, MIN-2)', () => {
  it('bounces to /login when an API call returns 401', async () => {
    statusFetch({
      '/api/settings': () => jsonResponse({ error: 'session expired' }, 401),
    })
    renderWithProviders(<App />, { route: '/settings' })

    expect(await screen.findByRole('heading', { name: /sign in/i })).toBeInTheDocument()
  })

  it('handles 401 outside the app shell (setup wizard)', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        jsonResponse({ initialized: false, authenticated: false, keyring_available: true }),
      ),
    )
    renderWithProviders(<App />, { route: '/setup' })
    expect(await screen.findByRole('heading', { name: /first-run setup/i })).toBeInTheDocument()

    act(() => {
      window.dispatchEvent(new Event(UNAUTHORIZED_EVENT))
    })

    expect(await screen.findByRole('heading', { name: /sign in/i })).toBeInTheDocument()
  })

  it('redirects an authenticated visitor away from /login', async () => {
    statusFetch({
      '/api/auth/me': () => jsonResponse({ id: 1, username: 'admin', is_admin: true }),
    })
    renderWithProviders(<App />, { route: '/login' })

    expect(await screen.findByRole('heading', { name: 'Library' })).toBeInTheDocument()
    expect(
      screen.getByText('Browse titles, inspect tracks and start a sync job.'),
    ).toBeInTheDocument()
  })

  it('clears the query cache on logout', async () => {
    const user = userEvent.setup()
    statusFetch({
      '/api/auth/me': () => jsonResponse({ id: 1, username: 'admin', is_admin: true }),
      '/api/auth/logout': () => new Response(null, { status: 204 }),
    })
    const { client } = renderWithProviders(<App />, { route: '/library' })
    const clear = vi.spyOn(client, 'clear')

    // Account actions moved into the username menu (issue #6).
    await user.click(await screen.findByRole('button', { name: 'admin' }))
    const button = await screen.findByRole('menuitem', { name: /log out/i })
    await user.click(button)

    await waitFor(() => expect(clear).toHaveBeenCalled())
  })
})

describe('wizard dead ends (IMP-2c, MIN-3)', () => {
  it('surfaces a 409 setup-once error with a link to sign in', async () => {
    const user = userEvent.setup()
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input)
        if (url.includes('/api/auth/status')) {
          return jsonResponse({ initialized: false, authenticated: false, keyring_available: true })
        }
        if (url.includes('/api/auth/setup') && isMethod(init, 'POST')) {
          return jsonResponse({ error: 'setup already completed' }, 409)
        }
        return jsonResponse({ error: 'unexpected' }, 404)
      }),
    )
    renderWithProviders(<SetupPage />, { route: '/setup' })

    await user.type(screen.getByLabelText('Username'), 'admin')
    await user.type(screen.getByLabelText('Password'), 'longenough')
    await user.type(screen.getByLabelText('Confirm password'), 'longenough')
    await user.click(screen.getByRole('button', { name: /create administrator/i }))

    expect(await screen.findByText(/setup already completed/i)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /sign in/i })).toHaveAttribute('href', '/login')
  })

  it('rejects a relative media root from stored settings before saving', async () => {
    const user = userEvent.setup()
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.includes('/api/auth/status')) {
        return jsonResponse({ initialized: false, authenticated: false, keyring_available: true })
      }
      if (url.includes('/api/auth/setup') && isMethod(init, 'POST')) {
        return jsonResponse({ id: 1, username: 'admin', is_admin: true }, 201)
      }
      if (url.includes('/api/auth/login') && isMethod(init, 'POST')) {
        return jsonResponse({ id: 1, username: 'admin', is_admin: true })
      }
      if (url.includes('/api/settings')) {
        return jsonResponse({
          settings: {
            'storage.paths': {
              watch_dir: '/watch',
              vault_dir: '/vault',
              work_dir: '/work',
              media_roots: ['relative/root'],
            },
          },
        })
      }
      return jsonResponse({ error: 'unexpected' }, 404)
    })
    vi.stubGlobal('fetch', fetchMock)
    renderWithProviders(<SetupPage />, { route: '/setup' })

    await user.type(screen.getByLabelText('Username'), 'admin')
    await user.type(screen.getByLabelText('Password'), 'longenough')
    await user.type(screen.getByLabelText('Confirm password'), 'longenough')
    await user.click(screen.getByRole('button', { name: /create administrator/i }))
    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: /save & continue/i }))

    expect(await screen.findByText(/must be absolute/i)).toBeInTheDocument()
    expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(false)
  })
})

describe('directory browser errors (IMP-2d)', () => {
  const cases = [
    [403, 'permission denied'],
    [404, 'no such directory'],
    [400, 'path must be absolute'],
  ] as const

  it.each(cases)('shows the backend message for %i', async (status, message) => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => jsonResponse({ error: message }, status)),
    )
    renderWithProviders(<BrowseModal open onClose={() => {}} onSelect={() => {}} />)

    expect(await screen.findByText(message)).toBeInTheDocument()
  })
})

describe('settings export (IMP-2e, MIN-4c)', () => {
  it('downloads a sanitized filename', async () => {
    const user = userEvent.setup()
    let download = ''
    Object.defineProperty(URL, 'createObjectURL', {
      configurable: true,
      value: vi.fn(() => 'blob:mock'),
    })
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: vi.fn() })
    const click = vi
      .spyOn(HTMLAnchorElement.prototype, 'click')
      .mockImplementation(function (this: HTMLAnchorElement) {
        download = this.download
      })

    statusFetch({
      '/api/settings/export': () =>
        jsonResponse({ exported_at: '2026-01-02T03:04:05Z', settings: {} }),
      '/api/settings': () => jsonResponse({ settings: {} }),
    })
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    await user.click(await screen.findByRole('button', { name: /export json/i }))

    await waitFor(() => expect(click).toHaveBeenCalled())
    expect(download).toBe('dubarr-settings-2026-01-02T03-04-05Z.json')
  })
})

describe('list editor typing (IMP-1)', () => {
  it('keeps raw comma text while typing and commits numbers on blur', async () => {
    const user = userEvent.setup()
    const fetchMock = statusFetch({
      '/api/settings': (init) =>
        isMethod(init, 'PUT')
          ? jsonResponse({ settings: { 'sync.defaults': SYNC_DEFAULTS } })
          : jsonResponse({ settings: { 'sync.defaults': SYNC_DEFAULTS } }),
    })
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    const input = await screen.findByLabelText('Checkpoint positions (%)')
    await user.clear(input)
    await user.type(input, '10,25,40')
    expect(input).toHaveValue('10,25,40')

    await user.tab()
    await user.click(screen.getByLabelText('Save Sync defaults'))

    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true)
    })
    const put = fetchMock.mock.calls.find(([, init]) => isMethod(init, 'PUT'))
    const body = JSON.parse(String(put?.[1]?.body)) as {
      settings: { 'sync.defaults': { checkpoint_positions_pct: number[] } }
    }
    expect(body.settings['sync.defaults'].checkpoint_positions_pct).toEqual([10, 25, 40])
  })
})
