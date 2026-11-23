import { screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import App from '../App'
import { jsonResponse, renderWithProviders } from '../test/helpers'

function mockAuthenticatedAdmin(): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.includes('/api/auth/status')) {
        return jsonResponse({ initialized: true, authenticated: true, keyring_available: true })
      }
      if (url.includes('/api/auth/me')) {
        return jsonResponse({ id: 1, username: 'admin', is_admin: true })
      }
      return jsonResponse({ error: 'not found' }, 404)
    }),
  )
}

describe('AppShell top bar', () => {
  it('labels the admin role by name instead of repeating the username', async () => {
    mockAuthenticatedAdmin()
    renderWithProviders(<App />, { route: '/library' })

    expect(await screen.findByText(/Library — coming in M2/)).toBeInTheDocument()
    expect(await screen.findByText('Administrator')).toBeInTheDocument()
    // The username badge stays, but the bare role badge no longer duplicates it.
    expect(screen.getAllByText('admin')).toHaveLength(1)
  })
})
