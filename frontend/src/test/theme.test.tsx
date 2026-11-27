import { act, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import App from '../App'
import { ThemeSelector } from '../components/ThemeSelector'
import { ThemeProvider } from '../lib/ThemeProvider'
import { THEME_STORAGE_KEY } from '../lib/theme'
import { jsonResponse, renderWithProviders } from '../test/helpers'

/** Minimal matchMedia stand-in with a controllable "OS is dark" switch. */
function stubMatchMedia(initialDark: boolean) {
  const listeners = new Set<(event: { matches: boolean }) => void>()
  const query = {
    matches: initialDark,
    media: '(prefers-color-scheme: dark)',
    onchange: null,
    addEventListener: (_: string, listener: (event: { matches: boolean }) => void) => {
      listeners.add(listener)
    },
    removeEventListener: (_: string, listener: (event: { matches: boolean }) => void) => {
      listeners.delete(listener)
    },
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  }
  vi.stubGlobal('matchMedia', vi.fn(() => query))
  return {
    setDark(dark: boolean) {
      query.matches = dark
      listeners.forEach((listener) => listener({ matches: dark }))
    },
  }
}

function renderSelector() {
  return renderWithProviders(
    <ThemeProvider>
      <ThemeSelector />
    </ThemeProvider>,
  )
}

function htmlTheme() {
  return document.documentElement.dataset.theme
}

function htmlColorScheme() {
  return document.documentElement.style.colorScheme
}

beforeEach(() => {
  localStorage.clear()
  document.documentElement.removeAttribute('data-theme')
  document.documentElement.style.colorScheme = ''
  // jsdom has no matchMedia; tests opt into the OS being light by default.
  stubMatchMedia(false)
})

describe('theme preference', () => {
  it('defaults to dark and applies it to <html> with a matching color-scheme', () => {
    renderSelector()

    expect(screen.getByRole('radio', { name: 'Dark' })).toBeChecked()
    expect(htmlTheme()).toBe('dark')
    expect(htmlColorScheme()).toBe('dark')
  })

  it('persists the selection in localStorage and restores it on the next load', async () => {
    const user = userEvent.setup()
    const first = renderSelector()

    await user.click(screen.getByRole('radio', { name: 'Light' }))

    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe('light')
    expect(htmlTheme()).toBe('light')
    expect(htmlColorScheme()).toBe('light')

    first.unmount()
    renderSelector()

    expect(screen.getByRole('radio', { name: 'Light' })).toBeChecked()
    expect(htmlTheme()).toBe('light')
    expect(htmlColorScheme()).toBe('light')
  })

  it('follows prefers-color-scheme live while System is selected', async () => {
    const user = userEvent.setup()
    const media = stubMatchMedia(false)
    renderSelector()

    await user.click(screen.getByRole('radio', { name: 'System' }))
    expect(htmlTheme()).toBe('light')
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe('system')

    act(() => media.setDark(true))
    expect(htmlTheme()).toBe('dark')
    expect(htmlColorScheme()).toBe('dark')

    act(() => media.setDark(false))
    expect(htmlTheme()).toBe('light')
  })

  it('applies the theme before authentication (login screen)', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.includes('/api/auth/status')) {
        return jsonResponse({ initialized: true, authenticated: false, keyring_available: true })
      }
      return jsonResponse({ error: 'not found' }, 404)
    })
    vi.stubGlobal('fetch', fetchMock)

    renderWithProviders(<App />, { route: '/login' })

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument()
    expect(htmlTheme()).toBe('dark')

    // The logo beside the wordmark is decorative: it adds no accessible name,
    // so "Dubarr" is still exposed exactly once.
    const mark = document.querySelector('img[src="/icon-512.png"]')
    expect(mark?.getAttribute('aria-hidden')).toBe('true')
    expect(mark?.getAttribute('alt')).toBe('')
    expect(screen.getAllByRole('heading', { name: 'Dubarr' })).toHaveLength(1)
  })
})
