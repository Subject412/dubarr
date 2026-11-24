import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { TokensPage } from '../pages/Tokens'
import { isMethod, jsonResponse, renderWithProviders } from './helpers'

type TokenRow = {
  id: number
  name: string
  prefix: string
  created_at: string
  last_used_at?: string | null
}

function tokenFetch(tokens: TokenRow[], overrides: Record<string, (init?: RequestInit) => Response | Promise<Response>> = {}) {
  let rows = [...tokens]
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    for (const [fragment, handler] of Object.entries(overrides)) {
      if (url.includes(fragment)) return handler(init)
    }
    if (url.endsWith('/api/auth/tokens') && isMethod(init, 'GET')) {
      return jsonResponse({ tokens: rows })
    }
    if (url.endsWith('/api/auth/tokens') && isMethod(init, 'POST')) {
      const body = JSON.parse(String(init?.body)) as { name: string }
      const created = {
        id: 99,
        name: body.name,
        prefix: 'dub_NEWTOKEN',
        created_at: '2026-01-02 10:30:00',
        token: 'dub_NEWTOKENsecretvalue',
      }
      rows = [...rows, created]
      return jsonResponse(created, 201)
    }
    const revoke = /\/api\/auth\/tokens\/(\d+)$/.exec(url)
    if (revoke && isMethod(init, 'DELETE')) {
      rows = rows.filter((row) => row.id !== Number(revoke[1]))
      return new Response(null, { status: 204 })
    }
    return jsonResponse({ error: 'unexpected' }, 404)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

const SAMPLE: TokenRow[] = [
  {
    id: 1,
    name: 'cli',
    prefix: 'dub_AbCdEfGh',
    created_at: '2026-01-02 10:30:00',
    last_used_at: '2026-01-03 08:15:00',
  },
  {
    id: 2,
    name: 'unused',
    prefix: 'dub_ZyXwVuTs',
    created_at: '2026-01-04 09:00:00',
    last_used_at: null,
  },
]

describe('token management', () => {
  it('renders the token list with name, prefix, created and last used', async () => {
    tokenFetch(SAMPLE)
    renderWithProviders(<TokensPage />)

    const cli = await screen.findByRole('row', { name: /cli/ })
    expect(within(cli).getByText('dub_AbCdEfGh')).toBeInTheDocument()
    expect(within(cli).getByText('2026-01-02 10:30 UTC')).toBeInTheDocument()
    expect(within(cli).getByText('2026-01-03 08:15 UTC')).toBeInTheDocument()

    const unused = screen.getByRole('row', { name: /unused/ })
    expect(within(unused).getByText('Never')).toBeInTheDocument()
  })

  it('shows a newly created token exactly once, then hides it on dismiss', async () => {
    const user = userEvent.setup()
    tokenFetch(SAMPLE)
    renderWithProviders(<TokensPage />)

    await user.type(await screen.findByLabelText('Token name'), 'laptop')
    await user.click(screen.getByRole('button', { name: /create token/i }))

    const callout = await screen.findByRole('status', { name: /new api token/i })
    expect(within(callout).getByText('dub_NEWTOKENsecretvalue')).toBeInTheDocument()
    expect(within(callout).getByText(/will not be shown again/i)).toBeInTheDocument()
    expect(await screen.findByRole('row', { name: /laptop/ })).toBeInTheDocument()

    await user.click(within(callout).getByRole('button', { name: /dismiss/i }))
    await waitFor(() => {
      expect(screen.queryByText('dub_NEWTOKENsecretvalue')).not.toBeInTheDocument()
    })
  })

  it('revokes a token after confirmation', async () => {
    const user = userEvent.setup()
    const fetchMock = tokenFetch(SAMPLE)
    renderWithProviders(<TokensPage />)

    const row = await screen.findByRole('row', { name: /cli/ })
    await user.click(within(row).getByRole('button', { name: /revoke cli/i }))
    await user.click(within(row).getByRole('button', { name: /confirm revoke/i }))

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/auth/tokens/1',
        expect.objectContaining({ method: 'DELETE' }),
      )
    })
    await waitFor(() => {
      expect(screen.queryByRole('row', { name: /cli/ })).not.toBeInTheDocument()
    })
  })

  it('surfaces API errors', async () => {
    tokenFetch([], {
      '/api/auth/tokens': () => jsonResponse({ error: 'could not list tokens' }, 500),
    })
    renderWithProviders(<TokensPage />)

    expect(await screen.findByText(/could not list tokens/i)).toBeInTheDocument()
  })
})
