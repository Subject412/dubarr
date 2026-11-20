import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { SettingsPage } from '../pages/Settings'
import { SetupPage } from '../pages/Setup'
import { jsonResponse, isMethod, renderWithProviders } from '../test/helpers'

function mockFetch(): ReturnType<typeof vi.fn> {
  let authenticated = false
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.includes('/api/auth/status')) {
      return jsonResponse({ initialized: authenticated, authenticated, keyring_available: true })
    }
    if (url.includes('/api/auth/setup') && isMethod(init, 'POST')) {
      authenticated = true
      return jsonResponse({ id: 1, username: 'admin', is_admin: true }, 201)
    }
    if (url.includes('/api/auth/login') && isMethod(init, 'POST')) {
      return jsonResponse({ id: 1, username: 'admin', is_admin: true })
    }
    if (url.includes('/api/settings')) {
      return jsonResponse({ settings: {} })
    }
    return jsonResponse({ error: 'unexpected' }, 404)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('first-run wizard admin step', () => {
  it('rejects a password shorter than 8 characters', async () => {
    const user = userEvent.setup()
    mockFetch()
    renderWithProviders(<SetupPage />, { route: '/setup' })

    await user.type(screen.getByLabelText('Username'), 'admin')
    await user.type(screen.getByLabelText('Password'), 'short')
    await user.type(screen.getByLabelText('Confirm password'), 'short')
    await user.click(screen.getByRole('button', { name: /create administrator/i }))

    expect(await screen.findByText('Password must be at least 8 characters.')).toBeInTheDocument()
  })

  it('rejects mismatched confirmation', async () => {
    const user = userEvent.setup()
    mockFetch()
    renderWithProviders(<SetupPage />, { route: '/setup' })

    await user.type(screen.getByLabelText('Username'), 'admin')
    await user.type(screen.getByLabelText('Password'), 'longenough')
    await user.type(screen.getByLabelText('Confirm password'), 'different')
    await user.click(screen.getByRole('button', { name: /create administrator/i }))

    expect(await screen.findByText('Passwords do not match.')).toBeInTheDocument()
  })

  it('creates the admin and advances to storage', async () => {
    const user = userEvent.setup()
    const fetchMock = mockFetch()
    renderWithProviders(<SetupPage />, { route: '/setup' })

    await user.type(screen.getByLabelText('Username'), 'admin')
    await user.type(screen.getByLabelText('Password'), 'longenough')
    await user.type(screen.getByLabelText('Confirm password'), 'longenough')
    await user.click(screen.getByRole('button', { name: /create administrator/i }))

    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()

    const setupCall = fetchMock.mock.calls.find(
      ([input]) => String(input).includes('/api/auth/setup'),
    )
    expect(setupCall).toBeDefined()
    expect(JSON.parse(String(setupCall?.[1]?.body))).toEqual({
      username: 'admin',
      password: 'longenough',
    })
  })
})

describe('settings section save', () => {
  it('PUTs the whole storage.paths section', async () => {
    const user = userEvent.setup()
    const settings = {
      'storage.paths': {
        watch_dir: '/watch',
        vault_dir: '/vault',
        work_dir: '/work',
        media_roots: [],
      },
      'languages.policy': {
        target_import_languages: ['pl'],
        extra_reference_languages: ['en'],
        original_language_from_arr: true,
        exclude_commentary: true,
        export_mode: 'rule_based',
      },
      'target.tag_filter': {
        required_filename_tags: ['[ArcA310]'],
        tag_match_mode: 'ANY',
        tag_case_sensitive: false,
        excluded_filename_tags: [],
        enforce_tag_filter: true,
      },
      'sync.defaults': {
        max_offset_sec: 180,
        checkpoint_positions_pct: [10],
        checkpoint_window_sec: 150,
        preview_positions_pct: [10],
        preview_window_sec: 60,
      },
      'security.options': {
        vault_read_only: true,
        allow_force_tag_override: false,
        require_av1_warning: true,
        require_av1_strict: false,
      },
      'mux.options': { keep_bak: true, track_title_in_mkv: 'Polski Dubbing' },
      'watch.options': { scan_interval_sec: 10, stability_check_interval_sec: 4 },
      'arr.options': { poll_interval_sec: 300, manual_sync: true },
    }

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

    renderWithProviders(<SettingsPage />, { route: '/settings' })

    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()

    const watch = screen.getByLabelText('Watch folder')
    await user.clear(watch)
    await user.type(watch, '/new/watch')
    await user.click(screen.getByLabelText('Save Storage paths'))

    await waitFor(() => {
      const put = fetchMock.mock.calls.find(([, init]) => isMethod(init, 'PUT'))
      expect(put).toBeDefined()
    })

    const putCall = fetchMock.mock.calls.find(([, init]) => isMethod(init, 'PUT'))
    const body = JSON.parse(String(putCall?.[1]?.body)) as {
      settings: Record<string, { watch_dir: string }>
    }
    expect(Object.keys(body.settings)).toEqual(['storage.paths'])
    expect(body.settings['storage.paths'].watch_dir).toBe('/new/watch')
    expect(await screen.findByText('saved')).toBeInTheDocument()
  })
})
