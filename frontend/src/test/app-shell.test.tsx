import { screen, within } from '@testing-library/react'
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

describe('AppShell navigation', () => {
  it('shows the admin-only Logs tab', async () => {
    mockAuthenticatedAdmin()
    renderWithProviders(<App />, { route: '/library' })

    // Wait for /api/auth/me: admin-only items appear only once it resolves.
    await screen.findByText('Administrator')
    const links = await screen.findAllByRole('link', { name: /Logs/ })
    expect(links.some((link) => link.getAttribute('href') === '/logs')).toBe(true)
  })
})

describe('AppShell sidebar', () => {
  it('pins the aside with a viewport-height cap and scrolls the nav internally', async () => {
    mockAuthenticatedAdmin()
    renderWithProviders(<App />, { route: '/library' })

    // Wait for /api/auth/me so the full nav (incl. admin entries) has settled.
    await screen.findByText('Administrator')

    const aside = screen.getByRole('complementary')
    expect(aside.className).toContain('md:sticky')
    expect(aside.className).toContain('md:top-0')
    expect(aside.className).toContain('md:h-dvh')
    expect(aside.className).toContain('md:self-start')

    // The nav must be able to shrink below its content and scroll on short viewports.
    const nav = within(aside).getByRole('navigation')
    expect(nav.className).toContain('min-h-0')
    expect(nav.className).toContain('overflow-y-auto')
  })
})

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
