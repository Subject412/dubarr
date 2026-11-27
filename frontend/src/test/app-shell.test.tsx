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

    expect(await screen.findByRole('heading', { name: 'Library' })).toBeInTheDocument()
    expect(
      screen.getByText('Browse titles, inspect tracks and start a sync job.'),
    ).toBeInTheDocument()
    expect(await screen.findByText('Administrator')).toBeInTheDocument()
    // The username badge stays, but the bare role badge no longer duplicates it.
    expect(screen.getAllByText('admin')).toHaveLength(1)
  })
})

describe('AppShell brand mark', () => {
  it('shows the decorative logo instead of the old "D" chip', async () => {
    mockAuthenticatedAdmin()
    const { container } = renderWithProviders(<App />, { route: '/library' })

    // Wait for /api/auth/me so the whole shell has settled.
    await screen.findByText('Administrator')

    // The purple "D" chip is gone: no element in the chrome renders a bare "D".
    const aside = screen.getByRole('complementary')
    // The shell's top bar is the first <header> in the document; the page
    // headers rendered inside <main> come after it.
    const header = container.querySelector<HTMLElement>('header')
    if (header === null) throw new Error('the shell renders no top bar')
    expect(within(aside).queryByText('D')).toBeNull()
    expect(within(header).queryByText('D')).toBeNull()

    // The sidebar mark and the mobile-only mark, both decorative (alt="" +
    // aria-hidden) and letterboxed so the wider-than-tall mark never distorts.
    const asideMark = aside.querySelector('img[aria-hidden="true"]')
    expect(asideMark?.getAttribute('src')).toBe('/icon-512.png')
    expect(asideMark?.getAttribute('alt')).toBe('')
    expect(asideMark?.className).toContain('h-8 w-8')
    expect(asideMark?.className).toContain('object-contain')

    const headerMark = header.querySelector('img[aria-hidden="true"]')
    expect(headerMark?.getAttribute('src')).toBe('/icon-512.png')
    expect(headerMark?.getAttribute('alt')).toBe('')
    expect(headerMark?.className).toContain('h-7 w-7')
    expect(headerMark?.className).toContain('object-contain')

    // Exactly the two brand marks; the page body adds none.
    expect(
      Array.from(container.querySelectorAll('img')).filter((img) =>
        (img.getAttribute('src') ?? '').endsWith('icon-512.png'),
      ),
    ).toHaveLength(2)
  })
})
