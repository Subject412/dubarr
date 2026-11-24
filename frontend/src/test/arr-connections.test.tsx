import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { validateArrBaseUrl } from '../lib/arr-connections'
import { SettingsPage } from '../pages/Settings'
import { SetupPage } from '../pages/Setup'
import { isMethod, jsonResponse, renderWithProviders } from '../test/helpers'

type ArrInstance = {
  kind: 'radarr' | 'sonarr'
  base_url: string
  enabled: boolean
  api_key: string
}

const LS = window.localStorage

function settingsPayload() {
  return {
    settings: {
      'arr.options': { poll_interval_sec: 300, manual_sync: true },
    },
  }
}

/** Fetch mock for the four arr connection endpoints; `state` is mutated so a
 * save/delete/refetch round trip behaves like the real API. */
function mockArrFetch({
  instances = [] as ArrInstance[],
  listStatus = 200,
  saveStatus = 200,
  testStatus = 200,
  listError = 'arr settings are not configured',
} = {}) {
  const state = { instances: [...instances], authenticated: false }
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    const method = (init?.method ?? 'GET').toUpperCase()

    if (url.endsWith('/api/auth/status')) {
      return jsonResponse({
        initialized: state.authenticated,
        authenticated: state.authenticated,
        keyring_available: true,
      })
    }
    if (url.endsWith('/api/auth/setup') && method === 'POST') {
      state.authenticated = true
      return jsonResponse({ id: 1, username: 'admin', is_admin: true }, 201)
    }
    if (url.endsWith('/api/auth/login') && method === 'POST') {
      return jsonResponse({ id: 1, username: 'admin', is_admin: true })
    }

    if (url.endsWith('/api/settings/arr') && method === 'GET') {
      if (listStatus !== 200) return jsonResponse({ error: listError }, listStatus)
      return jsonResponse({ instances: state.instances })
    }
    const save = /\/api\/settings\/arr\/(radarr|sonarr)$/.exec(url)
    if (save && method === 'PUT') {
      const body = JSON.parse(String(init?.body)) as {
        base_url: string
        api_key?: string
        enabled?: boolean
      }
      if (saveStatus !== 200) return jsonResponse({ error: 'arr settings are not configured' }, saveStatus)
      const kind = save[1] as ArrInstance['kind']
      const existing = state.instances.find((candidate) => candidate.kind === kind)
      const next: ArrInstance = {
        kind,
        base_url: body.base_url,
        enabled: body.enabled ?? existing?.enabled ?? true,
        api_key: existing?.api_key ?? '********wxyz',
      }
      state.instances = existing
        ? state.instances.map((candidate) => (candidate.kind === kind ? next : candidate))
        : [...state.instances, next]
      return jsonResponse(next)
    }
    const remove = /\/api\/settings\/arr\/(radarr|sonarr)$/.exec(url)
    if (remove && method === 'DELETE') {
      state.instances = state.instances.filter((candidate) => candidate.kind !== remove[1])
      return new Response(null, { status: 204 })
    }
    const test = /\/api\/settings\/arr\/(radarr|sonarr)\/test$/.exec(url)
    if (test && method === 'POST') {
      if (testStatus !== 200) return jsonResponse({ error: 'arr settings are not configured' }, testStatus)
      return jsonResponse({ ok: true, kind: test[1] })
    }
    if (url === '/api/settings' && method === 'GET') return jsonResponse(settingsPayload())
    if (url === '/api/settings' && method === 'PUT') return jsonResponse(settingsPayload())
    return jsonResponse({ error: `unexpected ${method} ${url}` }, 404)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

async function radarrCard() {
  return within(await screen.findByRole('group', { name: 'Radarr connection' }))
}

function putsTo(fetchMock: ReturnType<typeof vi.fn>, path: string) {
  return fetchMock.mock.calls.filter(
    ([input, init]) => String(input) === path && isMethod(init, 'PUT'),
  )
}

describe('arr connection base URL validation', () => {
  it('accepts an IP literal with an explicit scheme', () => {
    expect(validateArrBaseUrl('http://192.168.1.10:7878')).toEqual({ ok: true })
    expect(validateArrBaseUrl('https://10.0.0.5')).toEqual({ ok: true })
  })

  it('accepts a DNS hostname with an explicit scheme', () => {
    expect(validateArrBaseUrl('http://radarr.local:7878')).toEqual({ ok: true })
    expect(validateArrBaseUrl('https://radarr.example.com/radarr')).toEqual({ ok: true })
  })

  it('rejects a bare host:port without a scheme instead of auto-prefixing it', () => {
    const result = validateArrBaseUrl('radarr.local:7878')
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error).toMatch(/http:\/\/ or https:\/\//)
    expect(validateArrBaseUrl('192.168.1.10:7878').ok).toBe(false)
  })

  it('rejects an unsupported scheme and an empty host', () => {
    expect(validateArrBaseUrl('ftp://radarr.local').ok).toBe(false)
    expect(validateArrBaseUrl('http://').ok).toBe(false)
    expect(validateArrBaseUrl('   ').ok).toBe(false)
  })
})

describe('first-run wizard arr step (optional)', () => {
  it('offers a skippable arr connection step after languages', async () => {
    const user = userEvent.setup()
    mockArrFetch()
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

    await user.click(screen.getByRole('button', { name: /save & continue/i }))
    expect(await screen.findByRole('heading', { name: 'Radarr & Sonarr' })).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Skip for now' }))
    expect(await screen.findByRole('heading', { name: 'Security' })).toBeInTheDocument()
  })
})

describe('Radarr / Sonarr connections settings section', () => {
  it('renders the masked key suffix from GET and never the plaintext key', async () => {
    mockArrFetch({
      instances: [
        {
          kind: 'radarr',
          base_url: 'http://192.168.1.5:7878',
          enabled: true,
          api_key: '********abcd',
        },
      ],
    })
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    expect(
      await screen.findByRole('heading', { name: 'Radarr / Sonarr connections' }),
    ).toBeInTheDocument()
    expect(await screen.findByText('••••abcd')).toBeInTheDocument()

    const card = await radarrCard()
    expect(card.getByLabelText('Base URL')).toHaveValue('http://192.168.1.5:7878')
    expect(card.getByLabelText('API key')).toHaveValue('')
    expect(card.getByLabelText('API key')).toHaveAttribute('type', 'password')
    expect(card.getByLabelText('API key')).toHaveAttribute('autocomplete', 'off')
    expect(document.body.innerHTML).not.toContain('abcd1234-secret')
    expect(LS.length).toBe(0)
  })

  it('saves a rotated key only through PUT /api/settings/arr/{kind} and clears the field', async () => {
    const user = userEvent.setup()
    const fetchMock = mockArrFetch({
      instances: [
        {
          kind: 'radarr',
          base_url: 'http://192.168.1.5:7878',
          enabled: true,
          api_key: '********abcd',
        },
      ],
    })
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByText('••••abcd')).toBeInTheDocument()

    const card = await radarrCard()
    await user.type(card.getByLabelText('API key'), 'abcd1234-secret')
    await user.click(card.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(putsTo(fetchMock, '/api/settings/arr/radarr')).toHaveLength(1))
    const body = JSON.parse(String(putsTo(fetchMock, '/api/settings/arr/radarr')[0][1]?.body))
    expect(body).toEqual({
      base_url: 'http://192.168.1.5:7878',
      api_key: 'abcd1234-secret',
      enabled: true,
    })

    // The generic settings endpoint must never see the key.
    expect(putsTo(fetchMock, '/api/settings')).toHaveLength(0)

    await waitFor(() => expect(card.getByLabelText('API key')).toHaveValue(''))
    expect(document.body.innerHTML).not.toContain('abcd1234-secret')
    expect(screen.queryByDisplayValue('abcd1234-secret')).toBeNull()
    expect(LS.length).toBe(0)
    expect(await screen.findByText(/Saved Radarr connection/)).toBeInTheDocument()
  })

  it('omits api_key from the payload when the field is left blank', async () => {
    const user = userEvent.setup()
    const fetchMock = mockArrFetch({
      instances: [
        {
          kind: 'radarr',
          base_url: 'http://192.168.1.5:7878',
          enabled: true,
          api_key: '********abcd',
        },
      ],
    })
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByText('••••abcd')).toBeInTheDocument()

    const card = await radarrCard()
    await user.click(card.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(putsTo(fetchMock, '/api/settings/arr/radarr')).toHaveLength(1))
    const body = JSON.parse(String(putsTo(fetchMock, '/api/settings/arr/radarr')[0][1]?.body)) as {
      api_key?: string
    }
    expect(body.api_key).toBeUndefined()
  })

  it('calls the test endpoint when Test connection is clicked', async () => {
    const user = userEvent.setup()
    const fetchMock = mockArrFetch({
      instances: [
        {
          kind: 'radarr',
          base_url: 'http://192.168.1.5:7878',
          enabled: true,
          api_key: '********abcd',
        },
      ],
    })
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByText('••••abcd')).toBeInTheDocument()

    const card = await radarrCard()
    await user.click(card.getByRole('button', { name: 'Test connection' }))

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(
        ([input, init]) =>
          String(input) === '/api/settings/arr/radarr/test' && isMethod(init, 'POST'),
      )
      expect(call).toBeDefined()
    })
    expect(await screen.findByText(/Radarr responded/)).toBeInTheDocument()
  })

  it('removes an existing connection through DELETE and hides the Remove button', async () => {
    const user = userEvent.setup()
    const fetchMock = mockArrFetch({
      instances: [
        {
          kind: 'radarr',
          base_url: 'http://192.168.1.5:7878',
          enabled: true,
          api_key: '********abcd',
        },
      ],
    })
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByText('••••abcd')).toBeInTheDocument()

    const card = await radarrCard()
    await user.click(card.getByRole('button', { name: 'Remove connection' }))

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(
        ([input, init]) =>
          String(input) === '/api/settings/arr/radarr' && isMethod(init, 'DELETE'),
      )
      expect(call).toBeDefined()
    })
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: 'Remove connection' })).toBeNull(),
    )
    expect(await screen.findByText(/Removed Radarr connection/)).toBeInTheDocument()
  })

  it('maps a 503 from the arr endpoints to "Secrets are not initialised"', async () => {
    mockArrFetch({ listStatus: 503 })
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    expect((await screen.findAllByText('Secrets are not initialised')).length).toBeGreaterThan(0)
  })

  it('maps a 503 on save to "Secrets are not initialised"', async () => {
    const user = userEvent.setup()
    mockArrFetch({
      instances: [
        {
          kind: 'radarr',
          base_url: 'http://192.168.1.5:7878',
          enabled: true,
          api_key: '********abcd',
        },
      ],
      saveStatus: 503,
    })
    renderWithProviders(<SettingsPage />, { route: '/settings' })
    expect(await screen.findByText('••••abcd')).toBeInTheDocument()

    const card = await radarrCard()
    await user.click(card.getByRole('button', { name: 'Save' }))

    expect(await screen.findByText('Secrets are not initialised')).toBeInTheDocument()
  })

  it('rejects a bare host:port inline and sends no request', async () => {
    const user = userEvent.setup()
    const fetchMock = mockArrFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    const card = await radarrCard()
    const baseUrl = await card.findByLabelText('Base URL')
    await user.type(baseUrl, 'radarr.local:7878')
    await user.type(card.getByLabelText('API key'), 'abcd1234-secret')
    await user.click(card.getByRole('button', { name: 'Save' }))

    expect(await card.findByText(/Base URL must start with http:\/\/ or https:\/\//)).toBeInTheDocument()
    expect(putsTo(fetchMock, '/api/settings/arr/radarr')).toHaveLength(0)
  })

  it('accepts an IP-literal base URL and saves it', async () => {
    const user = userEvent.setup()
    const fetchMock = mockArrFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    const card = await radarrCard()
    await user.type(await card.findByLabelText('Base URL'), 'http://192.168.1.10:7878')
    await user.type(card.getByLabelText('API key'), 'abcd1234-secret')
    await user.click(card.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(putsTo(fetchMock, '/api/settings/arr/radarr')).toHaveLength(1))
    const body = JSON.parse(String(putsTo(fetchMock, '/api/settings/arr/radarr')[0][1]?.body))
    expect(body.base_url).toBe('http://192.168.1.10:7878')
  })

  it('accepts a DNS hostname base URL and saves it', async () => {
    const user = userEvent.setup()
    const fetchMock = mockArrFetch()
    renderWithProviders(<SettingsPage />, { route: '/settings' })

    const card = await radarrCard()
    await user.type(await card.findByLabelText('Base URL'), 'https://radarr.example.com')
    await user.type(card.getByLabelText('API key'), 'abcd1234-secret')
    await user.click(card.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(putsTo(fetchMock, '/api/settings/arr/radarr')).toHaveLength(1))
    const body = JSON.parse(String(putsTo(fetchMock, '/api/settings/arr/radarr')[0][1]?.body))
    expect(body.base_url).toBe('https://radarr.example.com')
  })
})
