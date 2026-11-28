import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { DEFAULT_SETTINGS } from '../lib/defaults'
import { settingsSectionId } from '../lib/settings-sections'
import { SETTINGS_HELP } from '../lib/settings-help'
import { TAG_FILTER_REGEX_EXAMPLE, tagListHint } from '../lib/tag-filter-hint'
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
      'mux.options': { output_mode: 'replace' },
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
    'Restart required to apply watch changes. The watch folder is read at startup; ' +
    'the scan interval, stability window and tag filter apply without a restart.'

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

  it('does not ask for a restart when the tag filter changes', async () => {
    const user = userEvent.setup()
    mockSettings()
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    expect(
      await screen.findByRole('heading', { name: 'Process only files tagged as …' }),
    ).toBeInTheDocument()
    await user.click(screen.getByRole('switch', { name: 'Case sensitive' }))
    await user.click(screen.getByLabelText('Save Process only files tagged as …'))

    expect(await screen.findByText('saved')).toBeInTheDocument()
    // The gate re-reads target.tag_filter on every classification, so an edit
    // applies to the next candidate without a restart.
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
          settings: { 'mux.options': { output_mode: 'replace' } },
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
    expect(body.settings['mux.options']).toEqual({ output_mode: 'replace' })
  })

  // U10: the Matroska track title is derived from the language, so the operator
  // has no field for it and none is posted back.
  it('is not an input and is not part of the saved payload', async () => {
    const user = userEvent.setup()
    const fetchMock = mockMuxFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Mux options' })).toBeInTheDocument()

    const card = document.getElementById(settingsSectionId('mux.options')) as HTMLElement | null
    expect(card).not.toBeNull()
    expect(within(card as HTMLElement).queryByLabelText(/track title/i)).toBeNull()
    expect(screen.queryByRole('button', { name: /about track title/i })).toBeNull()
    expect(within(card as HTMLElement).getByText(/named after its language/i)).toBeInTheDocument()

    await user.click(screen.getByLabelText('Save Mux options'))
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true)
    })
    const put = fetchMock.mock.calls.find(([, init]) => isMethod(init, 'PUT'))
    const body = JSON.parse(String(put?.[1]?.body)) as {
      settings: { 'mux.options': Record<string, unknown> }
    }
    expect(body.settings['mux.options']).not.toHaveProperty('track_title_in_mkv')
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

  it('round-trips the force tag override toggle through the settings payload', async () => {
    // #48 reads the same switch at approval time, so flipping it must reach the
    // server as the stored section, not just as local state.
    const user = userEvent.setup()
    const fetchMock = mockSecurityFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    const control = await screen.findByRole('switch', { name: 'Allow force tag override' })
    expect(control).toHaveAttribute('aria-checked', 'false')

    await user.click(control)
    expect(control).toHaveAttribute('aria-checked', 'true')

    await user.click(screen.getByLabelText('Save Security options'))
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true)
    })

    const put = fetchMock.mock.calls.find(([, init]) => isMethod(init, 'PUT'))
    const body = JSON.parse(String(put?.[1]?.body)) as {
      settings: { 'security.options': { allow_force_tag_override: boolean } }
    }
    expect(body.settings['security.options']).toEqual({ allow_force_tag_override: true })
    // The help text documents the new reach of the switch (#48) without turning
    // every approval into a force run.
    expect(SETTINGS_HELP['security.options'].allow_force_tag_override).toMatch(/one job|one approved job|single item/i)
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

  // U15: REGEX mode must show a pattern the gate's engine accepts, and say
  // which dialect it is — an operator who copies a shell glob or a PCRE
  // pattern otherwise gets a filter that compiles to nothing.
  it('TestRegexModeShowsARegexExample', async () => {
    const user = userEvent.setup()
    mockSettingsFetch('REGEX')
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(
      await screen.findByRole('heading', { name: 'Process only files tagged as …' }),
    ).toBeInTheDocument()

    // The rendered placeholder is the shared example itself, not a copy of it.
    expect(screen.getByLabelText('Add required tag')).toHaveAttribute(
      'placeholder',
      TAG_FILTER_REGEX_EXAMPLE,
    )
    expect(TAG_FILTER_REGEX_EXAMPLE).not.toBe('[TAG]')
    // An escaped dot is what makes it a pattern rather than the old tag stub.
    expect(TAG_FILTER_REGEX_EXAMPLE).toContain('\\.')

    const note = tagListHint('REGEX').note
    expect(note).toMatch(/regular expression/i)
    expect(note).toMatch(/filename/i)
    expect(note).toMatch(/RE2/)
    expect(note).toMatch(/backreference/i)
    expect(note).toMatch(/lookaround/i)
    expect(screen.getByText(note)).toBeInTheDocument()

    // Switching back to ANY restores the literal-tag placeholder and drops the
    // regex explanation, so the two modes cannot be confused.
    await user.selectOptions(screen.getByLabelText('Tag match mode'), 'ANY')
    expect(screen.getByLabelText('Add required tag')).toHaveAttribute('placeholder', '[TAG]')
    expect(screen.queryByText(note)).toBeNull()
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
    await user.type(screen.getByLabelText('Audio Vault'), '/vault')
    await user.click(screen.getByRole('button', { name: /save & continue/i }))
    expect(await screen.findByRole('heading', { name: 'Languages & tags' })).toBeInTheDocument()
    expect(screen.queryByText(/Tdarr/i)).not.toBeInTheDocument()
    expect(screen.getByLabelText('Add required tag')).toHaveAttribute('placeholder', '[TAG]')
    // The wizard keeps the filter to filename tags; the Settings card owns the source choice.
    expect(screen.queryByLabelText('Tag source')).toBeNull()
    expect(screen.queryByLabelText('Add required *arr tag')).toBeNull()
    expect(screen.queryByLabelText('Add excluded *arr tag')).toBeNull()

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

  // U15: the wizard must teach the same pattern and the same dialect as the
  // Settings card — both read tagListHint, so the two pages cannot drift.
  it('TestWizardRegexModeShowsTheSameExample', async () => {
    const user = userEvent.setup()
    mockFetch()
    renderWithProviders(<SetupPage />, { route: '/setup' })

    await user.type(screen.getByLabelText('Username'), 'admin')
    await user.type(screen.getByLabelText('Password'), 'longenough')
    await user.type(screen.getByLabelText('Confirm password'), 'longenough')
    await user.click(screen.getByRole('button', { name: /create administrator/i }))
    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()

    await user.type(screen.getByLabelText('Watch folder'), '/watch')
    await user.type(screen.getByLabelText('Audio Vault'), '/vault')
    await user.click(screen.getByRole('button', { name: /save & continue/i }))
    expect(await screen.findByRole('heading', { name: 'Languages & tags' })).toBeInTheDocument()
    expect(screen.getByLabelText('Add required tag')).toHaveAttribute('placeholder', '[TAG]')

    await user.selectOptions(screen.getByLabelText('Tag match mode'), 'REGEX')
    expect(tagListHint('REGEX').placeholder).toBe(TAG_FILTER_REGEX_EXAMPLE)
    expect(screen.getByLabelText('Add required tag')).toHaveAttribute(
      'placeholder',
      TAG_FILTER_REGEX_EXAMPLE,
    )
    expect(screen.getByText(tagListHint('REGEX').note)).toBeInTheDocument()
  })
})

describe('mux output destination', () => {
  type OutputMode = 'replace' | 'separate'

  function muxSettings(outputMode: OutputMode) {
    return {
      'storage.paths': {
        watch_dir: '/watch',
        vault_dir: '/vault',
        work_dir: '/work',
        output_dir: '/deliver',
        media_roots: [],
      },
      'mux.options': {
        output_mode: outputMode,
      },
    }
  }

  /**
   * `current` is echoed back on PUT, the way the real endpoint replaces the
   * whole envelope, so a save re-seeds every card from the new settings.
   */
  function mockMuxFetch(outputMode: OutputMode = 'replace'): ReturnType<typeof vi.fn> {
    let current: Record<string, unknown> = muxSettings(outputMode)
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.includes('/api/settings') && isMethod(init, 'PUT')) {
        const body = JSON.parse(String(init?.body)) as { settings: Record<string, unknown> }
        current = { ...current, ...body.settings }
        return jsonResponse({ settings: current })
      }
      if (url.includes('/api/settings')) {
        return jsonResponse({ settings: current })
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

  function storageCard(): HTMLElement {
    const element = document.getElementById(settingsSectionId('storage.paths'))
    expect(element).not.toBeNull()
    return element as HTMLElement
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
      output_mode: 'separate',
    })
  })

  it('saves the mux output folder with the section', async () => {
    const user = userEvent.setup()
    const fetchMock = mockMuxFetch('separate')
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

  it('omits the mux output folder entirely when the saved mode is replace', async () => {
    mockMuxFetch('replace')
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()

    const card = storageCard()
    // A control that does not apply must be absent, not greyed out: no label,
    // no input, no disabled placeholder and no help trigger.
    expect(within(card).queryByLabelText('Mux output folder')).toBeNull()
    expect(document.getElementById('path-mux-output-folder')).toBeNull()
    expect(screen.queryByRole('button', { name: 'About Mux output folder' })).toBeNull()
    // Only the three unconditional path fields render, so the folder is gone
    // rather than present-and-disabled.
    expect(card.querySelectorAll('input[id^="path-"]')).toHaveLength(3)
    const textboxes = within(card).getAllByRole('textbox') as HTMLInputElement[]
    expect(textboxes.some((input) => input.disabled)).toBe(false)
  })

  it('shows the mux output folder with its stored value when the saved mode is separate', async () => {
    mockMuxFetch('separate')
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()

    const field = within(storageCard()).getByLabelText('Mux output folder')
    expect(field).toHaveValue('/deliver')
    expect(field).toBeEnabled()
  })

  it('still saves the stored output_dir while the field is hidden', async () => {
    const user = userEvent.setup()
    const fetchMock = mockMuxFetch('replace')
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()

    expect(screen.queryByLabelText('Mux output folder')).toBeNull()
    await user.click(screen.getByLabelText('Save Storage paths'))
    await waitFor(() =>
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true),
    )

    // The draft keeps the value; hiding the control only drops it from the DOM.
    expect(savedSection(fetchMock, 'storage.paths')).toMatchObject({
      watch_dir: '/watch',
      vault_dir: '/vault',
      work_dir: '/work',
      output_dir: '/deliver',
      media_roots: [],
    })
  })

  it('reveals the folder only once separate is saved, keeping the stored value', async () => {
    const user = userEvent.setup()
    const fetchMock = mockMuxFetch('replace')
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Mux options' })).toBeInTheDocument()

    // The unsaved draft does not move the control: it follows the saved mode.
    await user.selectOptions(screen.getByLabelText('Output mode'), 'separate')
    expect(screen.queryByLabelText('Mux output folder')).toBeNull()

    await user.click(screen.getByLabelText('Save Mux options'))
    await waitFor(() =>
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true),
    )

    expect(await screen.findByLabelText('Mux output folder')).toHaveValue('/deliver')
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

  function savedFilters(fetchMock: ReturnType<typeof vi.fn>) {
    const bodies: Array<Record<string, unknown>> = []
    for (const call of fetchMock.mock.calls) {
      const init = call[1] as RequestInit | undefined
      if (!isMethod(init, 'PUT')) continue
      const body = JSON.parse(String(init?.body)) as {
        settings: { 'target.tag_filter': Record<string, unknown> }
      }
      bodies.push(body.settings['target.tag_filter'])
    }
    return bodies
  }

  function putCount(fetchMock: ReturnType<typeof vi.fn>): number {
    let puts = 0
    for (const call of fetchMock.mock.calls) {
      if (isMethod(call[1] as RequestInit | undefined, 'PUT')) puts += 1
    }
    return puts
  }

  /** Server that stores whatever a PUT sends, so a reload sees the saved filter. */
  function mockSavingFilterFetch(
    initial: Record<string, unknown> = storedFilter,
  ): ReturnType<typeof vi.fn> {
    let current = { ...initial }
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.includes('/api/settings') && isMethod(init, 'PUT')) {
        const body = JSON.parse(String(init?.body)) as {
          settings: { 'target.tag_filter': Record<string, unknown> }
        }
        current = { ...body.settings['target.tag_filter'] }
        return jsonResponse({ settings: { 'target.tag_filter': current } })
      }
      if (url.includes('/api/settings')) {
        return jsonResponse({ settings: { 'target.tag_filter': current } })
      }
      return jsonResponse({ error: 'unexpected' }, 404)
    })
    vi.stubGlobal('fetch', fetchMock)
    return fetchMock
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
        'Which files the Processing Gate admits: tags in the Target filename, Radarr/Sonarr tags and root folders, or both. Donor ingestion is never gated.',
      ),
    ).toBeInTheDocument()
    expect(within(card).queryByText(/Tdarr/i)).toBeNull()
    expect(within(card).queryByText(/ArcA310/)).toBeNull()

    // The default source is filename, and only its criteria are on screen.
    const source = screen.getByLabelText('Tag source') as HTMLSelectElement
    expect(source).toHaveValue('filename')
    expect(source).toBeEnabled()
    expect(within(card).queryByLabelText('Add required *arr tag')).toBeNull()
    expect(within(card).queryByLabelText('Add excluded *arr tag')).toBeNull()
    expect(within(card).queryByRole('button', { name: 'Add folder…' })).toBeNull()
    expect(within(card).queryByText(/Radarr\/Sonarr root folders/i)).toBeNull()

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

  it('TestTagSourceCanBeChosen: the select offers filename, arr and both and each choice is saved', async () => {
    const user = userEvent.setup()
    const fetchMock = mockSavingFilterFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(
      await screen.findByRole('heading', { name: 'Process only files tagged as …' }),
    ).toBeInTheDocument()

    const card = filterCard()
    const source = within(card).getByLabelText('Tag source') as HTMLSelectElement
    expect(source).toBeEnabled()
    expect(Array.from(source.options).map((option) => option.value)).toEqual([
      'filename',
      'arr',
      'both',
    ])
    expect(Array.from(source.options).map((option) => option.textContent)).toEqual([
      'Filename tags',
      '*arr metadata',
      'Both',
    ])
    expect(source).toHaveValue('filename')
    // Filename mode shows only the filename criteria.
    expect(within(card).queryByLabelText('Add required *arr tag')).toBeNull()

    const save = screen.getByLabelText('Save Process only files tagged as …')
    for (const mode of ['arr', 'both', 'filename']) {
      // Let the previous save settle before touching the draft again.
      await waitFor(() => expect(save).toBeEnabled())
      await user.selectOptions(source, mode)
      await user.click(save)
      await waitFor(() => expect(savedFilters(fetchMock).at(-1)?.source_mode).toBe(mode))
      await waitFor(() => expect(screen.getByText('saved')).toBeInTheDocument())
    }
    // Each choice persists as chosen — the stored arrays are never rewritten.
    expect(savedFilters(fetchMock).map((saved) => saved.source_mode)).toEqual([
      'arr',
      'both',
      'filename',
    ])
  })

  it('TestArrCriteriaAreEditable: the three *arr lists save and survive a reload', async () => {
    const user = userEvent.setup()
    const fetchMock = mockSavingFilterFetch()
    const view = renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(
      await screen.findByRole('heading', { name: 'Process only files tagged as …' }),
    ).toBeInTheDocument()

    await user.selectOptions(screen.getByLabelText('Tag source'), 'both')
    await user.type(screen.getByLabelText('Add required *arr tag'), 'Polish Dubbing{Enter}')
    await user.type(screen.getByLabelText('Add excluded *arr tag'), 'cam{Enter}')
    await user.type(screen.getByLabelText('Add root folder'), '/media/movies{Enter}')
    await user.click(screen.getByLabelText('Save Process only files tagged as …'))
    await waitFor(() => expect(putCount(fetchMock)).toBe(1))

    expect(savedFilter(fetchMock)).toEqual({
      ...storedFilter,
      source_mode: 'both',
      required_arr_tags: ['Polish Dubbing'],
      excluded_arr_tags: ['cam'],
      root_folder_paths: ['/media/movies'],
    })

    // A reload fetches the saved filter back, so the entries are not a draft-only illusion.
    view.unmount()
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(
      await screen.findByRole('heading', { name: 'Process only files tagged as …' }),
    ).toBeInTheDocument()
    const card = filterCard()
    expect(within(card).getByLabelText('Tag source')).toHaveValue('both')
    expect(within(card).getByText('Polish Dubbing')).toBeInTheDocument()
    expect(within(card).getByText('cam')).toBeInTheDocument()
    expect(within(card).getByText('/media/movies')).toBeInTheDocument()
  })

  it('TestFilenameModeKeepsStoredArrCriteria: switching to filename saves the arr lists unchanged', async () => {
    const user = userEvent.setup()
    const stored = {
      ...storedFilter,
      source_mode: 'both',
      required_arr_tags: ['dub'],
      excluded_arr_tags: ['cam'],
      root_folder_paths: ['/media/movies'],
    }
    const fetchMock = mockSavingFilterFetch(stored)
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(
      await screen.findByRole('heading', { name: 'Process only files tagged as …' }),
    ).toBeInTheDocument()

    const card = filterCard()
    const source = within(card).getByLabelText('Tag source') as HTMLSelectElement
    expect(source).toHaveValue('both')
    expect(within(card).getByLabelText('Add required *arr tag')).toBeInTheDocument()

    await user.selectOptions(source, 'filename')
    // Hidden, not cleared: the arr editors disappear but their values stay in the draft.
    expect(within(card).queryByLabelText('Add required *arr tag')).toBeNull()

    await user.click(screen.getByLabelText('Save Process only files tagged as …'))
    await waitFor(() => expect(putCount(fetchMock)).toBe(1))
    expect(savedFilter(fetchMock)).toEqual({ ...stored, source_mode: 'filename' })
    await waitFor(() => expect(screen.getByText('saved')).toBeInTheDocument())

    // Switching back shows the same criteria instead of an emptied form.
    await user.selectOptions(source, 'both')
    expect(await within(card).findByText('dub')).toBeInTheDocument()
    expect(within(card).getByText('cam')).toBeInTheDocument()
    expect(within(card).getByText('/media/movies')).toBeInTheDocument()
  })

  it('TestEmptyArrCriteriaExplainsTheMatch: no criteria says what the source admits', async () => {
    mockSavingFilterFetch({ ...storedFilter, source_mode: 'arr' })
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(
      await screen.findByRole('heading', { name: 'Process only files tagged as …' }),
    ).toBeInTheDocument()

    const card = filterCard()
    expect(within(card).getByLabelText('Tag source')).toHaveValue('arr')
    // D-TAGS: an empty list is a valid, explained state — not a blank box.
    const note = within(card).getByRole('note', { name: 'Empty *arr criteria' })
    expect(note).toHaveTextContent(/matches every title Dubarr has matched/i)
    expect(note).toHaveTextContent(/does not fill in default tags/i)
    expect(
      within(card).getByText(/has not matched to a Radarr\/Sonarr title are rejected/i),
    ).toBeInTheDocument()
    expect(within(card).getByLabelText('Add required *arr tag')).toHaveValue('')
    expect(within(card).getByLabelText('Add excluded *arr tag')).toHaveValue('')
    expect(within(card).getByLabelText('Add root folder')).toHaveValue('')
  })

  it('applies the match-mode control to the filename tags', async () => {
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

describe('parallel checkpoints (U-DSP.8)', () => {
  const storedSync = {
    max_offset_sec: 180,
    checkpoint_positions_pct: [10, 25, 40, 55, 70, 85],
    checkpoint_window_sec: 150,
    preview_positions_pct: [10, 25, 50, 75, 90],
    preview_window_sec: 60,
    parallel_checkpoints: 1,
    analysis_source: 'full',
  }

  function mockSyncFetch(): ReturnType<typeof vi.fn> {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.includes('/api/settings') && isMethod(init, 'PUT')) {
        return jsonResponse({ settings: { 'sync.defaults': storedSync } })
      }
      if (url.includes('/api/settings')) {
        return jsonResponse({ settings: { 'sync.defaults': storedSync } })
      }
      return jsonResponse({ error: 'unexpected' }, 404)
    })
    vi.stubGlobal('fetch', fetchMock)
    return fetchMock
  }

  function syncCard(): HTMLElement {
    const element = document.getElementById(settingsSectionId('sync.defaults'))
    expect(element).not.toBeNull()
    return element as HTMLElement
  }

  function savedSync(fetchMock: ReturnType<typeof vi.fn>): Record<string, unknown> {
    const put = fetchMock.mock.calls.find(([, init]) => isMethod(init, 'PUT'))
    expect(put).toBeDefined()
    const body = JSON.parse(String(put?.[1]?.body)) as {
      settings: Record<string, Record<string, unknown>>
    }
    // Only the section that changed is sent; every other field goes back as it
    // arrived, so a save cannot rewrite the operator's other sync defaults.
    expect(Object.keys(body.settings)).toEqual(['sync.defaults'])
    return body.settings['sync.defaults']
  }

  it('renders the field beside the checkpoint window with its help text', async () => {
    mockSyncFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Sync defaults' })).toBeInTheDocument()

    const field = within(syncCard()).getByLabelText('Parallel checkpoints')
    expect(field).toHaveValue(1)
    expect(field).toHaveAttribute('min', '1')
    expect(field).toHaveAttribute('max', '8')

    const trigger = screen.getByRole('button', { name: 'About Parallel checkpoints' })
    const helpId = field.getAttribute('aria-describedby') ?? ''
    expect(helpId).not.toBe('')
    expect(trigger).toHaveAttribute('aria-describedby', helpId)
    act(() => trigger.focus())
    const tooltip = await screen.findByRole('tooltip')
    expect(tooltip).toHaveTextContent(SETTINGS_HELP['sync.defaults'].parallel_checkpoints)
    // The measured cost, not the earlier estimate that the plan corrected.
    expect(tooltip).toHaveTextContent(/about 400 MB per extra worker/i)
    expect(tooltip).toHaveTextContent(/4 GB container gets 4 workers/i)
    expect(tooltip.textContent).not.toMatch(/170 MB/)
  })

  it('saves 4 and leaves every other sync default untouched', async () => {
    const user = userEvent.setup()
    const fetchMock = mockSyncFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Sync defaults' })).toBeInTheDocument()

    const field = within(syncCard()).getByLabelText('Parallel checkpoints')
    await user.clear(field)
    await user.type(field, '4')
    await user.click(screen.getByLabelText('Save Sync defaults'))

    await waitFor(() =>
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true),
    )
    expect(savedSync(fetchMock)).toEqual({ ...storedSync, parallel_checkpoints: 4 })
  })

  it('refuses 0 and 9 with the validator message and keeps the last valid value', async () => {
    const user = userEvent.setup()
    const fetchMock = mockSyncFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Sync defaults' })).toBeInTheDocument()

    const field = within(syncCard()).getByLabelText('Parallel checkpoints')

    await user.clear(field)
    await user.type(field, '0')
    expect(await within(syncCard()).findByRole('alert')).toHaveTextContent(
      'parallel_checkpoints must be 1..8',
    )

    // The refused entry stays visible, but the section saves the last value the
    // engine can actually use.
    await user.click(screen.getByLabelText('Save Sync defaults'))
    await waitFor(() =>
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true),
    )
    expect(savedSync(fetchMock).parallel_checkpoints).toBe(1)

    await user.clear(field)
    await user.type(field, '9')
    expect(within(syncCard()).getByRole('alert')).toHaveTextContent(
      'parallel_checkpoints must be 1..8',
    )
    expect(field).toHaveValue(9)
  })
})

// analysis_source (U-WIN.6). The backend serves a materialised value, but a
// section stored before the key existed (or an older server) must still render
// a *selected* option — a blank select is the finding-4 symptom — and an
// untouched save must send the mode it displays, not leave the key out.
describe('analysis source (U-WIN.6)', () => {
  const storedSync = {
    max_offset_sec: 180,
    checkpoint_positions_pct: [10, 25, 40, 55, 70, 85],
    checkpoint_window_sec: 150,
    preview_positions_pct: [10, 25, 50, 75, 90],
    preview_window_sec: 60,
    parallel_checkpoints: 1,
    analysis_source: 'full',
  }

  /** The same section as stored before the key existed: no analysis_source. */
  const legacySync = {
    max_offset_sec: 180,
    checkpoint_positions_pct: [10, 25, 40, 55, 70, 85],
    checkpoint_window_sec: 150,
    preview_positions_pct: [10, 25, 50, 75, 90],
    preview_window_sec: 60,
    parallel_checkpoints: 1,
  }

  function mockSyncFetch(
    section: Record<string, unknown>,
  ): ReturnType<typeof vi.fn> {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.includes('/api/settings') && isMethod(init, 'PUT')) {
        return jsonResponse({ settings: { 'sync.defaults': section } })
      }
      if (url.includes('/api/settings')) {
        return jsonResponse({ settings: { 'sync.defaults': section } })
      }
      return jsonResponse({ error: 'unexpected' }, 404)
    })
    vi.stubGlobal('fetch', fetchMock)
    return fetchMock
  }

  function syncCard(): HTMLElement {
    const element = document.getElementById(settingsSectionId('sync.defaults'))
    expect(element).not.toBeNull()
    return element as HTMLElement
  }

  function analysisSelect(): HTMLSelectElement {
    return within(syncCard()).getByLabelText('Analysis source') as HTMLSelectElement
  }

  function savedSync(fetchMock: ReturnType<typeof vi.fn>): Record<string, unknown> {
    const put = fetchMock.mock.calls.find(([, init]) => isMethod(init, 'PUT'))
    expect(put).toBeDefined()
    const body = JSON.parse(String(put?.[1]?.body)) as {
      settings: Record<string, Record<string, unknown>>
    }
    expect(Object.keys(body.settings)).toEqual(['sync.defaults'])
    return body.settings['sync.defaults']
  }

  it('renders Windows and Full with the stored mode selected and its help text', async () => {
    mockSyncFetch(storedSync)
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Sync defaults' })).toBeInTheDocument()

    const field = analysisSelect()
    expect(field).toHaveValue('full')
    expect(Array.from(field.options).map((option) => option.value)).toEqual(['windows', 'full'])

    const trigger = screen.getByRole('button', { name: 'About Analysis source' })
    const helpId = field.getAttribute('aria-describedby') ?? ''
    expect(helpId).not.toBe('')
    expect(trigger).toHaveAttribute('aria-describedby', helpId)
    act(() => trigger.focus())
    const tooltip = await screen.findByRole('tooltip')
    expect(tooltip).toHaveTextContent(SETTINGS_HELP['sync.defaults'].analysis_source)
    expect(tooltip).toHaveTextContent(/several times faster/i)
    expect(tooltip).toHaveTextContent(/never falls back/i)
  })

  it('saves full plus 4 parallel workers and leaves every other field untouched', async () => {
    const user = userEvent.setup()
    const fetchMock = mockSyncFetch({ ...storedSync, analysis_source: 'windows' })
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Sync defaults' })).toBeInTheDocument()

    expect(analysisSelect()).toHaveValue('windows')
    await user.selectOptions(analysisSelect(), 'full')

    const workers = within(syncCard()).getByLabelText('Parallel checkpoints')
    await user.clear(workers)
    await user.type(workers, '4')
    await user.click(screen.getByLabelText('Save Sync defaults'))

    await waitFor(() =>
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true),
    )
    expect(savedSync(fetchMock)).toEqual({ ...storedSync, parallel_checkpoints: 4 })
  })

  it('renders the default for a section without the key and saves it untouched', async () => {
    const user = userEvent.setup()
    const fetchMock = mockSyncFetch(legacySync)
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByRole('heading', { name: 'Sync defaults' })).toBeInTheDocument()

    // Never a blank control: the compiled default is what the operator sees.
    const field = analysisSelect()
    expect(field).toHaveValue('windows')
    expect(field.value).not.toBe('')

    // An untouched save PATCHes the mode it displays, so the section is
    // complete from then on instead of relying on the read-time default.
    await user.click(screen.getByLabelText('Save Sync defaults'))
    await waitFor(() =>
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true),
    )
    expect(savedSync(fetchMock)).toEqual({ ...legacySync, analysis_source: 'windows' })
  })
})

describe('watch options defaults', () => {
  it('shows the 60 s scan interval before the section has ever been saved', async () => {
    mockFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    expect(await screen.findByRole('heading', { name: 'Watch options' })).toBeInTheDocument()
    expect(screen.getByLabelText('Scan interval (s)')).toHaveValue(60)
  })
})
