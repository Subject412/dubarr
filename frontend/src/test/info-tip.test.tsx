import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { SETTINGS_HELP } from '../lib/settings-help'
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
    // `both` keeps the *arr criteria editors mounted: with `filename` the four
    // arr tips are hidden and the help-button count below drifts down.
    source_mode: 'both',
    required_filename_tags: ['[ArcA310]'],
    required_arr_tags: ['Polish Dubbing'],
    excluded_arr_tags: ['cam'],
    root_folder_paths: ['/media/movies'],
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
  // `separate` keeps the dependent Mux output folder in the form, so every
  // authored help entry has a control to document.
  'mux.options': { output_mode: 'separate' },
  'audio.options': {
    rules: [
      {
        enabled: true,
        when: { source_codecs: ['pcm_s16le'] },
        then: { codec: 'opus', bitrate_bps: 96000, channels: 2 },
      },
    ],
  },
  'watch.options': { scan_interval_sec: 10, stability_check_interval_sec: 4 },
  'arr.options': { poll_interval_sec: 300 },
}

function mockFetch(): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.includes('/api/settings') && isMethod(init, 'PUT')) {
      return jsonResponse({ settings: SETTINGS })
    }
    if (url.includes('/api/settings')) {
      return jsonResponse({ settings: SETTINGS })
    }
    return jsonResponse({ error: 'unexpected' }, 404)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

async function renderSettings() {
  mockFetch()
  renderWithProviders(<SettingsPage />, { route: '/settings' })
  expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()
}

/**
 * Canary: every authored help key must have a rendered tip. The expectation is
 * derived, so it is only as good as the fixture above — this fixture must enable
 * the *arr source (`source_mode: 'both'`) or the arr tips stay hidden and the
 * count drifts below the authored total instead of failing loudly.
 */
const helpCount = Object.values(SETTINGS_HELP).reduce(
  (total, fields) => total + Object.keys(fields).length,
  0,
)

describe('info tooltip (issue #3)', () => {
  it('renders one help button per settings help entry across every field kind', async () => {
    await renderSettings()

    expect(screen.getAllByRole('button', { name: /^About / })).toHaveLength(helpCount)

    // Every control kind wires help, not just text inputs.
    expect(screen.getByLabelText('Audio Vault')).toHaveAttribute('aria-describedby')
    expect(screen.getByLabelText('Export mode')).toHaveAttribute('aria-describedby')
    expect(screen.getByLabelText('Output mode')).toHaveAttribute('aria-describedby')
    expect(screen.getByLabelText('Add target language')).toHaveAttribute('aria-describedby')
    // U09: the checkpoint percent lists are read-only now, so the number field
    // takes their place as the "numeric control wires help" case.
    expect(screen.getByLabelText('Max offset (s)')).toHaveAttribute('aria-describedby')
  })

  it('wires help to a switch control, not just inputs and selects', async () => {
    const user = userEvent.setup()
    await renderSettings()

    // Toggle renders role="switch" with the same aria-describedby wiring as
    // every other control kind, so the tip is announced on the switch itself.
    const control = screen.getByRole('switch', { name: 'Allow force tag override' })
    const helpId = control.getAttribute('aria-describedby') ?? ''
    expect(helpId).not.toBe('')

    const trigger = screen.getByRole('button', { name: 'About Allow force tag override' })
    expect(trigger).toHaveAttribute('aria-describedby', helpId)

    act(() => trigger.focus())
    // The tip carries the authored help for the switch it documents.
    const tooltip = await screen.findByRole('tooltip')
    expect(tooltip).toHaveAttribute('id', helpId)
    expect(tooltip).toHaveTextContent(SETTINGS_HELP['security.options'].allow_force_tag_override)

    await user.keyboard('{Escape}')
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument()
  })

  it('opens on hover with role=tooltip and closes when the pointer leaves', async () => {
    const user = userEvent.setup()
    await renderSettings()

    const trigger = screen.getByRole('button', { name: 'About Audio Vault' })
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument()

    await user.hover(trigger)
    const tooltip = await screen.findByRole('tooltip')
    expect(tooltip).toHaveTextContent(/write-once/i)
    expect(screen.getByLabelText('Audio Vault')).toHaveAttribute('aria-describedby', tooltip.id)

    await user.unhover(trigger)
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument()
  })

  it('opens on keyboard focus and closes on blur and Escape', async () => {
    const user = userEvent.setup()
    await renderSettings()

    const trigger = screen.getByRole('button', { name: 'About Output mode' })

    act(() => trigger.focus())
    // The tip carries the authored help for the mux option it documents.
    expect(await screen.findByRole('tooltip')).toHaveTextContent(
      SETTINGS_HELP['mux.options'].output_mode,
    )

    await user.keyboard('{Escape}')
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument()

    act(() => trigger.blur())
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument()

    act(() => trigger.focus())
    expect(await screen.findByRole('tooltip')).toBeInTheDocument()
    act(() => trigger.blur())
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument()
  })

  it('positions the popover out of flow above the control so it covers nothing', async () => {
    const user = userEvent.setup()
    await renderSettings()

    const trigger = screen.getByRole('button', { name: 'About Audio Vault' })
    const control = screen.getByLabelText('Audio Vault')
    const box = {
      top: 300,
      bottom: 316,
      left: 100,
      right: 116,
      width: 16,
      height: 16,
      x: 100,
      y: 300,
      toJSON: () => ({}),
    } as DOMRect
    vi.spyOn(trigger, 'getBoundingClientRect').mockReturnValue(box)

    await user.hover(trigger)
    const tooltip = await screen.findByRole('tooltip')

    expect(getComputedStyle(tooltip).position).toBe('fixed')
    // Above the trigger: no overlap with the control being configured.
    expect(Number.parseInt(tooltip.style.top, 10)).toBeLessThan(box.top)
    expect(control).toBeInTheDocument()
  })

  it('flips below the control — not the trigger — and clamps inside the viewport', async () => {
    const user = userEvent.setup()
    await renderSettings()

    const trigger = screen.getByRole('button', { name: 'About Audio Vault' })
    const control = screen.getByLabelText('Audio Vault')

    const viewport = { width: 1024, height: 800 }
    vi.spyOn(window, 'innerWidth', 'get').mockReturnValue(viewport.width)
    vi.spyOn(window, 'innerHeight', 'get').mockReturnValue(viewport.height)

    const rect = (top: number, left: number, width: number, height: number): DOMRect => ({
      top,
      bottom: top + height,
      left,
      right: left + width,
      width,
      height,
      x: left,
      y: top,
      toJSON: () => ({}),
    })
    // Trigger sits in the label row right at the top of the viewport: there is
    // no room above, so the popover must flip. The control is the next element.
    const triggerRect = rect(10, 100, 16, 16)
    const controlRect = rect(34, 100, 300, 32)
    const tipRect = rect(0, 0, 288, 60)
    const emptyRect = rect(0, 0, 0, 0)

    vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (
      this: Element,
    ) {
      if (this === trigger) return triggerRect
      if (this === control) return controlRect
      if (this.getAttribute('role') === 'tooltip') return tipRect
      return emptyRect
    })

    await user.hover(trigger)
    const tooltip = await screen.findByRole('tooltip')

    const top = Number.parseInt(tooltip.style.top, 10)
    const left = Number.parseInt(tooltip.style.left, 10)

    // Never over the control it documents.
    const overlapsControl = top < controlRect.bottom && top + tipRect.height > controlRect.top
    expect(overlapsControl).toBe(false)
    expect(top).toBeGreaterThanOrEqual(controlRect.bottom)

    // And always inside the viewport.
    expect(top).toBeGreaterThanOrEqual(8)
    expect(top + tipRect.height).toBeLessThanOrEqual(viewport.height - 8)
    expect(left).toBeGreaterThanOrEqual(8)
    expect(left + tipRect.width).toBeLessThanOrEqual(viewport.width - 8)
  })

  it('never puts help text into the settings payload', async () => {
    const user = userEvent.setup()
    const fetchMock = await renderSettingsAndReturnMock()

    await user.click(screen.getByLabelText('Save Languages policy'))
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => isMethod(init, 'PUT'))).toBe(true)
    })

    const put = fetchMock.mock.calls.find(([, init]) => isMethod(init, 'PUT'))
    const body = String(put?.[1]?.body)
    expect(body).toContain('languages.policy')
    expect(body).not.toContain(SETTINGS_HELP['languages.policy'].export_mode)
    expect(body).not.toContain(SETTINGS_HELP['languages.policy'].target_import_languages)
  })
})

async function renderSettingsAndReturnMock() {
  const fetchMock = mockFetch()
  renderWithProviders(<SettingsPage />, { route: '/settings' })
  expect(await screen.findByRole('heading', { name: 'Storage paths' })).toBeInTheDocument()
  return fetchMock
}

describe('settings help module', () => {
  it('documents every settings field exactly once', () => {
    expect(SETTINGS_HELP['storage.paths'].watch_dir).toMatch(/Watch Folder/i)
    expect(SETTINGS_HELP['security.options'].allow_force_tag_override).toMatch(/bypass/i)
    expect(SETTINGS_HELP['mux.options'].output_mode).toMatch(/atomically overwrite/i)
    expect(SETTINGS_HELP['sync.defaults'].max_offset_sec).toMatch(/offset/i)
  })
})
