import { screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import App from '../App'
import { jsonResponse, renderWithProviders } from '../test/helpers'

function mockStatus(status: { initialized: boolean; authenticated: boolean }): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.includes('/api/auth/status')) {
        return jsonResponse({ ...status, keyring_available: true })
      }
      if (url.includes('/api/auth/me')) {
        return jsonResponse({ id: 1, username: 'admin', is_admin: true })
      }
      return jsonResponse({ error: 'not found' }, 404)
    }),
  )
}

describe('auth gate routing', () => {
  it('sends an uninitialised appliance to /setup', async () => {
    mockStatus({ initialized: false, authenticated: false })
    renderWithProviders(<App />, { route: '/library' })

    expect(await screen.findByRole('heading', { name: /first-run setup/i })).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: /administrator account/i })).toBeInTheDocument()
  })

  it('sends an unauthenticated visitor to /login', async () => {
    mockStatus({ initialized: true, authenticated: false })
    renderWithProviders(<App />, { route: '/library' })

    expect(await screen.findByRole('heading', { name: /sign in/i })).toBeInTheDocument()
  })

  it('renders the shell and library placeholder when authenticated', async () => {
    mockStatus({ initialized: true, authenticated: true })
    renderWithProviders(<App />, { route: '/library' })

    expect(await screen.findByText(/Library — coming in M2/)).toBeInTheDocument()
    expect(screen.getAllByRole('link', { name: /Settings/i }).length).toBeGreaterThan(0)
  })
})
