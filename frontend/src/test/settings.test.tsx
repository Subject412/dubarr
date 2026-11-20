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
      settings: Record<string, { watch_dir: string; vault_dir: string; work_dir: string; media_roots: string[] }>
    }
    expect(Object.keys(body.settings)).toEqual(['storage.paths'])
    expect(body.settings['storage.paths']).toEqual({
      watch_dir: '/new/watch',
      vault_dir: '/vault',
      work_dir: '/work',
      media_roots: [],
    })
    expect(await screen.findByText('saved')).toBeInTheDocument()
  })
})

describe('tag match mode (IDEA.md §3.5, ADR-0005)', () => {
  const tagFilter = (mode: string) => ({
    required_filename_tags: ['[ArcA310]'],
    tag_match_mode: mode,
    tag_case_sensitive: false,
    excluded_filename_tags: [],
    enforce_tag_filter: true,
  })

  function mockSettingsFetch(mode: string): ReturnType<typeof vi.fn> {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.includes('/api/settings')) {
        return jsonResponse({ settings: { 'target.tag_filter': tagFilter(mode) } })
      }
      return jsonResponse({ error: 'unexpected' }, 404)
    })
    vi.stubGlobal('fetch', fetchMock)
    return fetchMock
  }

  function putTagFilter(fetchMock: ReturnType<typeof vi.fn>) {
    const put = fetchMock.mock.calls.find(([, init]) => isMethod(init, 'PUT'))
    expect(put).toBeDefined()
    const body = JSON.parse(String(put?.[1]?.body)) as {
      settings: { 'target.tag_filter': { tag_match_mode: string; required_filename_tags: string[] } }
    }
    return body.settings['target.tag_filter']
  }

  it('offers ANY, ALL and REGEX and saves REGEX in the PUT payload', async () => {
    const user = userEvent.setup()
    const fetchMock = mockSettingsFetch('ANY')
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Target tag filter' })).toBeInTheDocument()

    const select = screen.getByLabelText('Tag match mode') as HTMLSelectElement
    expect(Array.from(select.options).map((option) => option.value)).toEqual(['ANY', 'ALL', 'REGEX'])

    await user.selectOptions(select, 'REGEX')
    await user.click(screen.getByLabelText('Save Target tag filter'))
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true)
    })
    expect(putTagFilter(fetchMock).tag_match_mode).toBe('REGEX')
  })

  it('renders a stored REGEX mode selected and round-trips it on save', async () => {
    const user = userEvent.setup()
    const fetchMock = mockSettingsFetch('REGEX')
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Target tag filter' })).toBeInTheDocument()

    const select = screen.getByLabelText('Tag match mode') as HTMLSelectElement
    expect(select).toHaveValue('REGEX')

    await user.click(screen.getByLabelText('Save Target tag filter'))
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true)
    })
    const saved = putTagFilter(fetchMock)
    expect(saved.tag_match_mode).toBe('REGEX')
    expect(saved.required_filename_tags).toEqual(['[ArcA310]'])
  })
})

describe('setup wizard tag filter step', () => {
  it('offers REGEX and saves it without coercing to ANY', async () => {
    const user = userEvent.setup()
    const fetchMock = mockFetch()
    renderWithProviders(<SetupPage />, { route: '/setup' })

    await user.type(screen.getByLabelText('Username'), 'admin')
    await user.type(screen.getByLabelText('Password'), 'longenough')
    await user.type(screen.getByLabelText('Confirm password'), 'longenough')
    await user.click(screen.getByRole('button', { name: /create administrator/i }))
    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()

    await user.type(screen.getByLabelText('Watch folder'), '/watch')
    await user.type(screen.getByLabelText('Vault folder'), '/vault')
    await user.click(screen.getByRole('button', { name: /save & continue/i }))
    expect(await screen.findByRole('heading', { name: 'Languages & tags' })).toBeInTheDocument()

    const select = screen.getByLabelText('Tag match mode') as HTMLSelectElement
    expect(Array.from(select.options).map((option) => option.value)).toEqual(['ANY', 'ALL', 'REGEX'])

    await user.selectOptions(select, 'REGEX')
    await user.click(screen.getByRole('button', { name: /save & continue/i }))
    await waitFor(() => {
      const puts = fetchMock.mock.calls.filter(([, init]) => isMethod(init, 'PUT'))
      expect(puts.length).toBeGreaterThan(1)
      expect(String(puts[puts.length - 1]?.[1]?.body)).toContain('target.tag_filter')
    })
    const puts = fetchMock.mock.calls.filter(([, init]) => isMethod(init, 'PUT'))
    const put = puts[puts.length - 1]
    const body = JSON.parse(String(put?.[1]?.body)) as {
      settings: Record<string, { tag_match_mode?: string }>
    }
    expect(body.settings['target.tag_filter'].tag_match_mode).toBe('REGEX')
  })
})
