import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { DEFAULT_SETTINGS } from '../lib/defaults'
import { settingsSectionId } from '../lib/settings-sections'
import { SettingsPage } from '../pages/Settings'
import { QueuePage } from '../pages/Queue'
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
        allow_force_tag_override: false,
      },
      'mux.options': { track_title_in_mkv: 'Polski Dubbing' },
      'watch.options': { scan_interval_sec: 10, stability_check_interval_sec: 4 },
      'arr.options': { poll_interval_sec: 300 },
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

describe('watch restart notice', () => {
  const settings = {
    'storage.paths': { watch_dir: '/watch', vault_dir: '/vault', work_dir: '/work', media_roots: [] },
    'watch.options': { scan_interval_sec: 10, stability_check_interval_sec: 4 },
    'target.tag_filter': {
      required_filename_tags: [],
      tag_match_mode: 'ANY',
      tag_case_sensitive: false,
      excluded_filename_tags: [],
      enforce_tag_filter: true,
    },
  }

  // The banner may only name the settings that are still read at startup.
  const RESTART_NOTICE =
    'Restart required to apply watch changes. The watch folder and tag filter are read at startup; ' +
    'the scan interval and stability window apply without a restart.'

  function mockSettings(): ReturnType<typeof vi.fn> {
    let current: Record<string, unknown> = settings
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.includes('/api/settings') && isMethod(init, 'GET')) {
        return jsonResponse({ settings: current })
      }
      if (url.includes('/api/settings') && isMethod(init, 'PUT')) {
        const body = JSON.parse(String(init?.body)) as { settings: Record<string, unknown> }
        current = { ...current, ...body.settings }
        return jsonResponse({ settings: current })
      }
      return jsonResponse({ error: 'unexpected' }, 404)
    })
    vi.stubGlobal('fetch', fetchMock)
    return fetchMock
  }

  it('does not ask for a restart when the scan interval changes', async () => {
    const user = userEvent.setup()
    mockSettings()
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    expect(await screen.findByRole('heading', { name: 'Watch options' })).toBeInTheDocument()
    const interval = screen.getByLabelText('Scan interval (s)')
    await user.clear(interval)
    await user.type(interval, '30')
    await user.click(screen.getByLabelText('Save Watch options'))

    expect(await screen.findByText('saved')).toBeInTheDocument()
    expect(screen.queryByText(/Restart required/)).not.toBeInTheDocument()
  })

  it('still asks for a restart for the boot-read watch folder and names only those settings', async () => {
    const user = userEvent.setup()
    mockSettings()
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()
    const watch = screen.getByLabelText('Watch folder')
    await user.clear(watch)
    await user.type(watch, '/new/watch')
    await user.click(screen.getByLabelText('Save Storage paths'))

    expect(await screen.findByText(RESTART_NOTICE)).toBeInTheDocument()
  })
})

describe('languages policy reference tokens', () => {
  const storedPolicy = {
    target_import_languages: ['pl'],
    extra_reference_languages: ['en'],
    original_language_from_arr: true,
    prefer_original_language: true,
    exclude_commentary: true,
    export_mode: 'rule_based',
  }

  function mockLanguagesFetch(policy: typeof storedPolicy = storedPolicy): ReturnType<typeof vi.fn> {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.includes('/api/settings') && isMethod(init, 'PUT')) {
        return jsonResponse({ settings: {} })
      }
      if (url.includes('/api/settings')) {
        return jsonResponse({ settings: { 'languages.policy': policy } })
      }
      return jsonResponse({ error: 'unexpected' }, 404)
    })
    vi.stubGlobal('fetch', fetchMock)
    return fetchMock
  }

  function savedPolicy(fetchMock: ReturnType<typeof vi.fn>) {
    const put = fetchMock.mock.calls.find(([, init]) => isMethod(init, 'PUT'))
    expect(put).toBeDefined()
    const body = JSON.parse(String(put?.[1]?.body)) as {
      settings: { 'languages.policy': typeof storedPolicy }
    }
    return body.settings['languages.policy']
  }

  it('adds org and all from friendly presets and saves the tokens', async () => {
    const user = userEvent.setup()
    const fetchMock = mockLanguagesFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Languages policy' })).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'org — original language from *arr' }))
    await user.click(screen.getByRole('button', { name: 'all — any shared language' }))
    await user.click(screen.getByLabelText('Save Languages policy'))

    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true)
    })
    expect(savedPolicy(fetchMock).extra_reference_languages).toEqual(['en', 'org', 'all'])
  })

  it('renders friendly labels for stored tokens', async () => {
    mockLanguagesFetch({ ...storedPolicy, extra_reference_languages: ['org', 'all'] })
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Languages policy' })).toBeInTheDocument()

    expect(screen.getByText('org — original language from *arr')).toBeInTheDocument()
    expect(screen.getByText('all — any shared language')).toBeInTheDocument()
  })

  it('saves prefer_original_language=false when the toggle is switched off', async () => {
    const user = userEvent.setup()
    const fetchMock = mockLanguagesFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Languages policy' })).toBeInTheDocument()

    const toggle = screen.getByRole('switch', { name: 'Prefer original language as reference' })
    expect(toggle.getAttribute('aria-checked')).toBe('true')
    await user.click(toggle)
    await user.click(screen.getByLabelText('Save Languages policy'))

    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true)
    })
    expect(savedPolicy(fetchMock).prefer_original_language).toBe(false)
  })
})

describe('mux options (retired keep_bak switch)', () => {
  function mockMuxFetch(): ReturnType<typeof vi.fn> {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.includes('/api/settings') && isMethod(init, 'PUT')) {
        return jsonResponse({ settings: {} })
      }
      if (url.includes('/api/settings')) {
        return jsonResponse({
          settings: { 'mux.options': { track_title_in_mkv: 'Polski Dubbing' } },
        })
      }
      return jsonResponse({ error: 'unexpected' }, 404)
    })
    vi.stubGlobal('fetch', fetchMock)
    return fetchMock
  }

  it('renders no backup switch and saves only the surviving mux fields', async () => {
    const user = userEvent.setup()
    const fetchMock = mockMuxFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Mux options' })).toBeInTheDocument()

    expect(screen.queryByRole('switch', { name: /keep \.bak/i })).toBeNull()
    expect(screen.queryByLabelText(/about keep \.bak/i)).toBeNull()
    expect(screen.queryByText(/\.bak/i)).toBeNull()

    await user.click(screen.getByLabelText('Save Mux options'))
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true)
    })

    const put = fetchMock.mock.calls.find(([, init]) => isMethod(init, 'PUT'))
    const body = JSON.parse(String(put?.[1]?.body)) as {
      settings: { 'mux.options': Record<string, unknown> }
    }
    expect(body.settings['mux.options']).not.toHaveProperty('keep_bak')
    expect(body.settings['mux.options'].track_title_in_mkv).toBe('Polski Dubbing')
  })
})

describe('security options (vault_read_only and AV1 switches retired)', () => {
  function mockSecurityFetch(): ReturnType<typeof vi.fn> {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.includes('/api/settings') && isMethod(init, 'PUT')) {
        return jsonResponse({ settings: {} })
      }
      if (url.includes('/api/settings')) {
        return jsonResponse({
          settings: {
            'security.options': {
              allow_force_tag_override: false,
            },
          },
        })
      }
      return jsonResponse({ error: 'unexpected' }, 404)
    })
    vi.stubGlobal('fetch', fetchMock)
    return fetchMock
  }

  it('renders the force tag override control and no retired switch', async () => {
    mockSecurityFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Security options' })).toBeInTheDocument()

    expect(
      screen.getByText('Safety switches for the tag override.'),
    ).toBeInTheDocument()
    expect(screen.getByRole('switch', { name: 'Allow force tag override' })).toBeInTheDocument()
    expect(screen.queryByRole('switch', { name: 'Vault read-only' })).toBeNull()
    expect(screen.queryByText(/vault read-only/i)).toBeNull()
    expect(screen.queryByRole('switch', { name: /AV1/i })).toBeNull()
    expect(screen.queryByText(/AV1/i)).toBeNull()
  })
})

describe('tag match mode (ADR-0005)', () => {
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
    expect(await screen.findByRole('heading', { name: 'Process only files tagged as …' })).toBeInTheDocument()

    const select = screen.getByLabelText('Tag match mode') as HTMLSelectElement
    expect(Array.from(select.options).map((option) => option.value)).toEqual(['ANY', 'ALL', 'REGEX'])

    await user.selectOptions(select, 'REGEX')
    await user.click(screen.getByLabelText('Save Process only files tagged as …'))
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true)
    })
    expect(putTagFilter(fetchMock).tag_match_mode).toBe('REGEX')
  })

  it('renders a stored REGEX mode selected and round-trips it on save', async () => {
    const user = userEvent.setup()
    const fetchMock = mockSettingsFetch('REGEX')
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Process only files tagged as …' })).toBeInTheDocument()

    const select = screen.getByLabelText('Tag match mode') as HTMLSelectElement
    expect(select).toHaveValue('REGEX')

    await user.click(screen.getByLabelText('Save Process only files tagged as …'))
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
    expect(screen.queryByText(/Tdarr/i)).not.toBeInTheDocument()
    expect(screen.getByLabelText('Add required tag')).toHaveAttribute('placeholder', '[TAG]')

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

describe('mux output destination', () => {
  const settings = {
    'storage.paths': {
      watch_dir: '/watch',
      vault_dir: '/vault',
      work_dir: '/work',
      output_dir: '/deliver',
      media_roots: [],
    },
    'mux.options': {
      track_title_in_mkv: 'Polski Dubbing',
      output_mode: 'replace',
    },
  }

  function mockMuxFetch(): ReturnType<typeof vi.fn> {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.includes('/api/settings') && isMethod(init, 'PUT')) {
        return jsonResponse({ settings })
      }
      if (url.includes('/api/settings')) {
        return jsonResponse({ settings })
      }
      return jsonResponse({ error: 'unexpected' }, 404)
    })
    vi.stubGlobal('fetch', fetchMock)
    return fetchMock
  }

  function savedSection(fetchMock: ReturnType<typeof vi.fn>, key: string) {
    const put = fetchMock.mock.calls.find(([, init]) => isMethod(init, 'PUT'))
    expect(put).toBeDefined()
    const body = JSON.parse(String(put?.[1]?.body)) as {
      settings: Record<string, Record<string, unknown>>
    }
    return body.settings[key]
  }

  it('offers both destination modes and saves separate', async () => {
    const user = userEvent.setup()
    const fetchMock = mockMuxFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    expect(await screen.findByRole('heading', { name: 'Mux options' })).toBeInTheDocument()
    const mode = screen.getByLabelText('Output mode') as HTMLSelectElement
    expect(Array.from(mode.options).map((option) => option.value)).toEqual(['replace', 'separate'])
    expect(mode.value).toBe('replace')

    await user.selectOptions(mode, 'separate')
    await user.click(screen.getByLabelText('Save Mux options'))

    await waitFor(() => expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true))
    expect(savedSection(fetchMock, 'mux.options')).toMatchObject({
      track_title_in_mkv: 'Polski Dubbing',
      output_mode: 'separate',
    })
  })

  it('saves the mux output folder with the section', async () => {
    const user = userEvent.setup()
    const fetchMock = mockMuxFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()
    const field = screen.getByLabelText('Mux output folder')
    expect(field).toHaveValue('/deliver')
    await user.clear(field)
    await user.type(field, '/deliver/new')
    await user.click(screen.getByLabelText('Save Storage paths'))

    await waitFor(() => expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true))
    expect(savedSection(fetchMock, 'storage.paths')).toMatchObject({
      watch_dir: '/watch',
      vault_dir: '/vault',
      work_dir: '/work',
      output_dir: '/deliver/new',
      media_roots: [],
    })
  })
})

describe('processing filter (issue #9)', () => {
  const storedFilter = {
    required_filename_tags: ['[TAG]'],
    tag_match_mode: 'ANY',
    tag_case_sensitive: false,
    excluded_filename_tags: [],
    enforce_tag_filter: true,
    source_mode: 'filename',
    required_arr_tags: [],
    excluded_arr_tags: [],
    root_folder_paths: [],
  }

  function mockFilterFetch(filter: Record<string, unknown> = storedFilter): ReturnType<typeof vi.fn> {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.includes('/api/system/browse')) {
        return jsonResponse({ path: '/media/movies', parent: '/media', entries: [] })
      }
      if (url.includes('/api/settings') && isMethod(init, 'PUT')) {
        return jsonResponse({ settings: { 'target.tag_filter': filter } })
      }
      if (url.includes('/api/settings')) {
        return jsonResponse({ settings: { 'target.tag_filter': filter } })
      }
      return jsonResponse({ error: 'unexpected' }, 404)
    })
    vi.stubGlobal('fetch', fetchMock)
    return fetchMock
  }

  function filterCard(): HTMLElement {
    const element = document.getElementById(settingsSectionId('target.tag_filter'))
    expect(element).not.toBeNull()
    return element as HTMLElement
  }

  function savedFilter(fetchMock: ReturnType<typeof vi.fn>) {
    const put = fetchMock.mock.calls.find(([, init]) => isMethod(init, 'PUT'))
    expect(put).toBeDefined()
    const body = JSON.parse(String(put?.[1]?.body)) as {
      settings: { 'target.tag_filter': Record<string, unknown> }
    }
    return body.settings['target.tag_filter']
  }

  it('defaults to the filename source with no arr criteria and no vendor copy', async () => {
    mockFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    expect(
      await screen.findByRole('heading', { name: 'Process only files tagged as …' }),
    ).toBeInTheDocument()
    const card = filterCard()
    expect(
      within(card).getByText(
        'Only files that carry a required tag are processed. Add tags found in filenames, or require tags from Radarr/Sonarr.',
      ),
    ).toBeInTheDocument()
    expect(within(card).queryByText(/Tdarr/i)).toBeNull()
    expect(within(card).queryByText(/ArcA310/)).toBeNull()

    expect(screen.getByLabelText('Tag source')).toHaveValue('filename')
    expect(screen.getByLabelText('Add required tag')).toHaveAttribute('placeholder', '[TAG]')

    expect(DEFAULT_SETTINGS['target.tag_filter']).toEqual({
      required_filename_tags: [],
      tag_match_mode: 'ANY',
      tag_case_sensitive: false,
      excluded_filename_tags: [],
      enforce_tag_filter: true,
      source_mode: 'filename',
      required_arr_tags: [],
      excluded_arr_tags: [],
      root_folder_paths: [],
    })
  })

  it('saves a both-source filter with arr tag labels and a root folder', async () => {
    const user = userEvent.setup()
    const fetchMock = mockFilterFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(
      await screen.findByRole('heading', { name: 'Process only files tagged as …' }),
    ).toBeInTheDocument()

    const source = screen.getByLabelText('Tag source') as HTMLSelectElement
    expect(Array.from(source.options).map((option) => option.value)).toEqual([
      'filename',
      'arr',
      'both',
    ])
    await user.selectOptions(source, 'both')

    await user.type(screen.getByLabelText('Add required *arr tag'), 'dub{Enter}')
    await user.type(screen.getByLabelText('Add excluded *arr tag'), 'cam{Enter}')

    const card = filterCard()
    await user.click(within(card).getByRole('button', { name: 'Add folder…' }))
    const dialog = await screen.findByRole('dialog', { name: 'Add Radarr/Sonarr root folders' })
    await user.click(within(dialog).getByRole('button', { name: 'Select this folder' }))

    await user.click(screen.getByLabelText('Save Process only files tagged as …'))
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true)
    })

    expect(savedFilter(fetchMock)).toEqual({
      ...storedFilter,
      source_mode: 'both',
      required_arr_tags: ['dub'],
      excluded_arr_tags: ['cam'],
      root_folder_paths: ['/media/movies'],
    })
  })

  it('applies the one match-mode control to filename and arr tags', async () => {
    const user = userEvent.setup()
    const fetchMock = mockFilterFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(
      await screen.findByRole('heading', { name: 'Process only files tagged as …' }),
    ).toBeInTheDocument()

    await user.selectOptions(screen.getByLabelText('Tag match mode'), 'REGEX')
    await user.click(screen.getByLabelText('Save Process only files tagged as …'))
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true)
    })
    expect(savedFilter(fetchMock).tag_match_mode).toBe('REGEX')
  })
})

describe('neutral processing-gate wording', () => {
  it('describes queue eligibility without vendor names', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        if (String(input).includes('/api/queue')) return jsonResponse({ items: [] })
        return jsonResponse({ error: 'unexpected' }, 404)
      }),
    )
    renderWithProviders(<QueuePage />, { route: '/queue' })

    expect(await screen.findByText(/Processing Gate eligibility/)).toBeInTheDocument()
    expect(screen.queryByText(/Tdarr/i)).not.toBeInTheDocument()
  })
})

describe('audio transcode rules', () => {
  function mockAudioFetch(): ReturnType<typeof vi.fn> {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.includes('/api/settings') && isMethod(init, 'GET')) {
        return jsonResponse({ settings: {} })
      }
      if (url.includes('/api/settings') && isMethod(init, 'PUT')) {
        return jsonResponse({ settings: {} })
      }
      return jsonResponse({ error: 'unexpected' }, 404)
    })
    vi.stubGlobal('fetch', fetchMock)
    return fetchMock
  }

  function savedRules(fetchMock: ReturnType<typeof vi.fn>) {
    const put = fetchMock.mock.calls.find(([, init]) => isMethod(init, 'PUT'))
    expect(put).toBeDefined()
    const body = JSON.parse(String(put?.[1]?.body)) as {
      settings: { 'audio.options': { rules: unknown[] } }
    }
    return body.settings['audio.options'].rules
  }

  it('defaults to no rules and states that nothing is transcoded', async () => {
    mockAudioFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    expect(await screen.findByRole('heading', { name: 'Audio transcoding' })).toBeInTheDocument()
    expect(
      screen.getByText('No rules — audio is copied losslessly.'),
    ).toBeInTheDocument()
    expect(DEFAULT_SETTINGS['audio.options']).toEqual({ rules: [] })
  })

  it('adds an ordered Opus rule and saves the whole audio.options section', async () => {
    const user = userEvent.setup()
    const fetchMock = mockAudioFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Audio transcoding' })).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Add rule' }))
    await user.type(screen.getByLabelText('Add source codec'), 'pcm_s16le{Enter}')
    await user.type(screen.getByLabelText('Rule 1 bitrate (bps)'), '96000')
    await user.type(screen.getByLabelText('Rule 1 output channels'), '2')

    await user.click(screen.getByLabelText('Save Audio transcoding'))
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true)
    })

    const rules = savedRules(fetchMock)
    expect(rules).toHaveLength(1)
    expect(rules[0]).toMatchObject({
      enabled: true,
      when: { source_codecs: ['pcm_s16le'] },
      then: { codec: 'opus', bitrate_bps: 96000, channels: 2 },
    })
  })

  it('strips incompatible action fields when the output codec changes', async () => {
    const user = userEvent.setup()
    const fetchMock = mockAudioFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Audio transcoding' })).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Add rule' }))
    await user.type(screen.getByLabelText('Rule 1 bitrate (bps)'), '96000')
    await user.type(screen.getByLabelText('Rule 1 output channels'), '2')
    await user.selectOptions(screen.getByLabelText('Rule 1 output codec'), 'flac')
    await user.selectOptions(screen.getByLabelText('Rule 1 output codec'), 'keep')

    await user.click(screen.getByLabelText('Save Audio transcoding'))
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true)
    })

    const rule = savedRules(fetchMock)[0] as {
      then: Record<string, unknown>
    }
    expect(rule.then.codec).toBe('keep')
    for (const field of ['bitrate_bps', 'channels', 'channel_layout', 'sample_rate']) {
      expect(Object.prototype.hasOwnProperty.call(rule.then, field)).toBe(false)
    }
  })
})
