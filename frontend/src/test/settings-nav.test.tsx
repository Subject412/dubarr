import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { AppShell } from '../components/AppShell'
import { SETTINGS_SECTIONS, settingsSectionId } from '../lib/settings-sections'
import { SettingsPage } from '../pages/Settings'
import { isMethod, jsonResponse, renderWithProviders } from '../test/helpers'

const SETTINGS = {
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
    checkpoint_positions_pct: [10, 25],
    checkpoint_window_sec: 150,
    preview_positions_pct: [10],
    preview_window_sec: 60,
  },
  'security.options': {
    allow_force_tag_override: false,
  },
  'mux.options': { track_title_in_mkv: 'Polski Dubbing' },
  'watch.options': { scan_interval_sec: 10, stability_check_interval_sec: 4 },
  'arr.options': { poll_interval_sec: 300, manual_sync: true },
  'imports.options': { donor_patterns: ['PL.DUB'] },
}

/** jsdom has no IntersectionObserver; tests drive entries through this stub. */
class FakeIntersectionObserver {
  static instances: FakeIntersectionObserver[] = []
  private callback: IntersectionObserverCallback

  constructor(callback: IntersectionObserverCallback) {
    this.callback = callback
    FakeIntersectionObserver.instances.push(this)
  }

  observe() {}
  unobserve() {}
  disconnect() {}
  takeRecords(): IntersectionObserverEntry[] {
    return []
  }

  emit(entries: Array<{ target: Element; isIntersecting: boolean }>) {
    this.callback(
      entries.map(
        (entry) =>
          ({
            ...entry,
            boundingClientRect: entry.target.getBoundingClientRect(),
            intersectionRatio: entry.isIntersecting ? 1 : 0,
          }) as IntersectionObserverEntry,
      ),
      this as unknown as IntersectionObserver,
    )
  }
}

function mockFetch(): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.includes('/api/auth/me')) {
      return jsonResponse({ id: 1, username: 'admin', is_admin: true })
    }
    if (url.includes('/api/settings') && isMethod(init, 'GET')) {
      return jsonResponse({ settings: SETTINGS })
    }
    if (url.includes('/api/settings') && isMethod(init, 'PUT')) {
      return jsonResponse({ settings: SETTINGS })
    }
    return jsonResponse({ error: 'unexpected' }, 404)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function renderShell(route = '/settings') {
  return renderWithProviders(
    <Routes>
      <Route element={<AppShell />}>
        <Route path="/settings" element={<SettingsPage />} />
        <Route path="/library" element={<p>library page</p>} />
      </Route>
    </Routes>,
    { route },
  )
}

function submenu() {
  const element = document.getElementById('settings-submenu')
  expect(element).not.toBeNull()
  return element as HTMLElement
}

beforeEach(() => {
  FakeIntersectionObserver.instances = []
  vi.stubGlobal('IntersectionObserver', FakeIntersectionObserver)
  Element.prototype.scrollIntoView = vi.fn()
})

describe('settings submenu (issue #13)', () => {
  it('renders one submenu link per shared settings-section registry entry', async () => {
    mockFetch()
    renderShell('/settings')
    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()

    const links = within(submenu()).getAllByRole('link')
    expect(links).toHaveLength(SETTINGS_SECTIONS.length)
    expect(links.map((link) => link.textContent)).toEqual(SETTINGS_SECTIONS.map((s) => s.title))
  })

  it('gives every rendered section the id derived from its section key', async () => {
    mockFetch()
    renderShell('/settings')
    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()

    for (const section of SETTINGS_SECTIONS) {
      expect(document.getElementById(settingsSectionId(section.key))).not.toBeNull()
    }
  })

  it('expands on /settings and collapses/expands through the aria toggle', async () => {
    const user = userEvent.setup()
    mockFetch()
    renderShell('/settings')
    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()

    const toggle = screen.getByRole('button', { name: /settings categories/i })
    expect(toggle).toHaveAttribute('aria-expanded', 'true')
    expect(toggle).toHaveAttribute('aria-controls', 'settings-submenu')
    expect(within(submenu()).getAllByRole('link')).toHaveLength(SETTINGS_SECTIONS.length)

    await user.click(toggle)
    expect(toggle).toHaveAttribute('aria-expanded', 'false')
    expect(within(submenu()).queryAllByRole('link')).toHaveLength(0)

    await user.click(toggle)
    expect(toggle).toHaveAttribute('aria-expanded', 'true')
    expect(within(submenu()).getAllByRole('link')).toHaveLength(SETTINGS_SECTIONS.length)
  })

  it('stays collapsed outside /settings', async () => {
    mockFetch()
    renderShell('/library')
    expect(await screen.findByText('library page')).toBeInTheDocument()

    const toggle = screen.getByRole('button', { name: /settings categories/i })
    expect(toggle).toHaveAttribute('aria-expanded', 'false')
    expect(within(submenu()).queryAllByRole('link')).toHaveLength(0)
  })

  it('jumps to ?section= on load and marks that category active', async () => {
    mockFetch()
    renderShell('/settings?section=mux.options')
    expect(await screen.findByRole('heading', { name: 'Mux options' })).toBeInTheDocument()

    const mux = document.getElementById(settingsSectionId('mux.options'))
    expect(mux).not.toBeNull()
    await waitFor(() => {
      expect(Element.prototype.scrollIntoView).toHaveBeenCalled()
    })
    expect((Element.prototype.scrollIntoView as ReturnType<typeof vi.fn>).mock.instances).toContain(mux)

    const link = within(submenu()).getByRole('link', { name: 'Mux options' })
    expect(link).toHaveAttribute('aria-current', 'page')
    expect(within(submenu()).getByRole('link', { name: 'Storage paths' })).not.toHaveAttribute(
      'aria-current',
    )
  })

  it('keeps unsaved drafts and does not remount cards when switching category', async () => {
    const user = userEvent.setup()
    mockFetch()
    renderShell('/settings')
    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()

    const watch = screen.getByLabelText('Watch folder')
    await user.clear(watch)
    await user.type(watch, '/draft/watch')

    await user.click(within(submenu()).getByRole('link', { name: 'Mux options' }))

    const watchAfter = await screen.findByLabelText('Watch folder')
    expect(watchAfter).toBe(watch)
    expect(watchAfter).toHaveValue('/draft/watch')
    await waitFor(() => {
      expect(within(submenu()).getByRole('link', { name: 'Mux options' })).toHaveAttribute(
        'aria-current',
        'page',
      )
    })
  })

  it('tracks the active category through IntersectionObserver scrolling', async () => {
    mockFetch()
    renderShell('/settings')
    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()

    const observer = FakeIntersectionObserver.instances[0]
    expect(observer).toBeDefined()
    const sync = document.getElementById(settingsSectionId('sync.defaults'))
    expect(sync).not.toBeNull()

    act(() => {
      observer.emit([
        { target: sync as Element, isIntersecting: true },
        { target: document.getElementById(settingsSectionId('storage.paths')) as Element, isIntersecting: false },
      ])
    })

    await waitFor(() => {
      expect(within(submenu()).getByRole('link', { name: 'Sync defaults' })).toHaveAttribute(
        'aria-current',
        'page',
      )
    })
  })

  it('offers the categories as a second chip row on mobile', async () => {
    const user = userEvent.setup()
    mockFetch()
    renderShell('/settings')
    expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()

    const chips = screen.getByRole('navigation', { name: 'Settings categories' })
    const chipLinks = within(chips).getAllByRole('link')
    expect(chipLinks).toHaveLength(SETTINGS_SECTIONS.length)

    await user.click(within(chips).getByRole('link', { name: 'Watch options' }))
    await waitFor(() => {
      expect(within(chips).getByRole('link', { name: 'Watch options' })).toHaveAttribute(
        'aria-current',
        'page',
      )
    })
  })
})
