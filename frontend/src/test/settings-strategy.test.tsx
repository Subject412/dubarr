import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { DEFAULT_SETTINGS } from '../lib/defaults'
import { settingsSectionId, type SettingsSectionKey } from '../lib/settings-sections'
import { SettingsPage } from '../pages/Settings'
import { isMethod, jsonResponse, renderWithProviders } from '../test/helpers'

/**
 * The shipped strategy (#82, D4), spelled out. The server seeds exactly this
 * (`analyze.DefaultStrategy` / `settings.PresetTechniques("balanced")`), so
 * DEFAULTS['sync.strategy'] must stay equal to it — that equality is one of the
 * assertions below, because a drift there would rewrite every operator's list.
 */
const SHIPPED = {
  preset: 'balanced',
  techniques: [
    { id: 'waveform', enabled: true, when: 'always', min_confidence: 0.1, min_match_z: 5 },
  ],
}

const FAST_POSITIONS = [15, 50, 85]
const BALANCED_POSITIONS = [10, 25, 40, 55, 70, 85]

type Fixture = {
  strategy?: unknown
  positions?: number[]
  putStatus?: number
  putError?: string
}

/**
 * Echoes the saved envelope back, the way the real endpoint replaces the whole
 * settings map, so a save re-seeds every card from what the server now holds.
 */
function mockFetch({
  strategy = SHIPPED,
  positions = BALANCED_POSITIONS,
  putStatus = 200,
  putError = '',
}: Fixture = {}): ReturnType<typeof vi.fn> {
  let current: Record<string, unknown> = {
    'sync.defaults': {
      max_offset_sec: 180,
      checkpoint_positions_pct: positions,
      checkpoint_window_sec: 150,
      preview_positions_pct: [10, 25, 50, 75, 90],
      preview_window_sec: 60,
      parallel_checkpoints: 1,
      analysis_source: 'windows',
    },
    'sync.strategy': strategy,
  }
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.includes('/api/settings') && isMethod(init, 'PUT')) {
      if (putStatus >= 400) return jsonResponse({ error: putError }, putStatus)
      const body = JSON.parse(String(init?.body)) as { settings: Record<string, unknown> }
      current = { ...current, ...body.settings }
      return jsonResponse({ settings: current })
    }
    if (url.includes('/api/settings')) return jsonResponse({ settings: current })
    return jsonResponse({ error: 'unexpected' }, 404)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function cardOf(key: SettingsSectionKey): HTMLElement {
  const element = document.getElementById(settingsSectionId(key))
  expect(element).not.toBeNull()
  return element as HTMLElement
}

const strategyCard = () => cardOf('sync.strategy')
const syncCard = () => cardOf('sync.defaults')

/** Every saved section payload, one per PUT, in the order the cards saved. */
function putPayloads(
  fetchMock: ReturnType<typeof vi.fn>,
): Array<Record<string, Record<string, unknown>>> {
  return fetchMock.mock.calls
    .filter(([, init]) => isMethod(init as RequestInit | undefined, 'PUT'))
    .map(
      ([, init]) =>
        (JSON.parse(String((init as RequestInit).body)) as {
          settings: Record<string, Record<string, unknown>>
        }).settings,
    )
}

/** One saved section, whichever card's Save carried it. */
function savedSection<T>(fetchMock: ReturnType<typeof vi.fn>, key: string): T {
  const section = putPayloads(fetchMock)
    .map((payload) => payload[key])
    .find((candidate) => candidate !== undefined)
  expect(section).toBeDefined()
  return section as T
}

async function openPage() {
  renderWithProviders(<SettingsPage />, { route: '/settings' })
  expect(await screen.findByRole('heading', { name: 'Matching techniques' })).toBeInTheDocument()
}

describe('matching techniques settings (#82)', () => {
  it('renders the shipped one-step strategy with Balanced marked active', async () => {
    const user = userEvent.setup()
    mockFetch()
    await openPage()

    const card = strategyCard()
    expect(within(card).getByRole('button', { name: 'Save Matching techniques' })).toBeInTheDocument()
    expect(within(card).getByLabelText('Step 1 method')).toHaveValue('waveform')
    expect(within(card).getByLabelText('Step 1 when it runs')).toHaveValue('always')
    expect(within(card).getByRole('switch', { name: 'Step 1 enabled' })).toHaveAttribute(
      'aria-checked',
      'true',
    )
    expect(within(card).queryByLabelText('Step 2 method')).toBeNull()
    expect(within(card).getByRole('button', { name: 'Balanced (default)' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    expect(within(card).getByTestId('strategy-preset')).toHaveTextContent('Balanced (default)')

    // The two thresholds stay behind the Advanced disclosure, with the shipped
    // values, so a correct default costs the operator no decisions (D11).
    expect(within(card).queryByLabelText('Step 1 confidence needed')).toBeNull()
    await user.click(within(card).getByRole('button', { name: 'Advanced' }))
    expect(within(card).getByLabelText('Step 1 confidence needed')).toHaveValue(0.1)
    expect(within(card).getByLabelText('Step 1 peak must stand out by (σ)')).toHaveValue(5)
    expect(within(card).getByRole('button', { name: 'Hide advanced' })).toHaveAttribute(
      'aria-expanded',
      'true',
    )

    // The SPA default is the server's stored default, byte for byte.
    expect(DEFAULT_SETTINGS['sync.strategy']).toEqual(SHIPPED)
  })

  it('switches to the Fast preset and writes the three checkpoint positions too', async () => {
    const user = userEvent.setup()
    const fetchMock = mockFetch()
    await openPage()

    await user.click(within(strategyCard()).getByRole('button', { name: 'Fast' }))

    // D4a: Fast measures fewer checkpoints with the same waveform matcher.
    expect(within(strategyCard()).getByLabelText('Step 1 method')).toHaveValue('waveform')
    expect(within(strategyCard()).getByTestId('strategy-preset')).toHaveTextContent('Fast')
    const positions = within(syncCard()).getByLabelText('Checkpoint positions (%)')
    await waitFor(() => expect(positions).toHaveValue('15, 50, 85'))

    await user.click(screen.getByLabelText('Save Matching techniques'))
    await user.click(screen.getByLabelText('Save Sync defaults'))

    await waitFor(() => expect(putPayloads(fetchMock)).toHaveLength(2))
    const strategy = savedSection<{ preset: string; techniques: unknown[] }>(
      fetchMock,
      'sync.strategy',
    )
    const defaults = savedSection<{ checkpoint_positions_pct: number[]; max_offset_sec: number }>(
      fetchMock,
      'sync.defaults',
    )
    expect(strategy.preset).toBe('fast')
    expect(strategy.techniques).toEqual(SHIPPED.techniques)
    expect(defaults.checkpoint_positions_pct).toEqual(FAST_POSITIONS)
    // Only the positions change; the rest of the sync defaults came back as stored.
    expect(defaults.max_offset_sec).toBe(180)
  })

  it('puts the six checkpoint positions back when Balanced is picked after Fast', async () => {
    const user = userEvent.setup()
    const fetchMock = mockFetch({ strategy: { preset: 'fast', techniques: SHIPPED.techniques }, positions: FAST_POSITIONS })
    await openPage()

    await user.click(within(strategyCard()).getByRole('button', { name: 'Balanced (default)' }))
    await waitFor(() =>
      expect(within(syncCard()).getByLabelText('Checkpoint positions (%)')).toHaveValue(
        '10, 25, 40, 55, 70, 85',
      ),
    )

    await user.click(screen.getByLabelText('Save Matching techniques'))
    await user.click(screen.getByLabelText('Save Sync defaults'))

    await waitFor(() => expect(putPayloads(fetchMock)).toHaveLength(2))
    const defaults = savedSection<{ checkpoint_positions_pct: number[] }>(fetchMock, 'sync.defaults')
    const strategy = savedSection<{ preset: string }>(fetchMock, 'sync.strategy')
    expect(defaults.checkpoint_positions_pct).toEqual(BALANCED_POSITIONS)
    expect(strategy.preset).toBe('balanced')
  })

  it('labels an edited list Custom and submits the custom preset', async () => {
    const user = userEvent.setup()
    const fetchMock = mockFetch()
    await openPage()

    const card = strategyCard()
    expect(within(card).getByTestId('strategy-preset')).toHaveTextContent('Balanced (default)')
    await user.selectOptions(within(card).getByLabelText('Step 1 method'), 'mfcc')

    expect(within(card).getByTestId('strategy-preset')).toHaveTextContent('Custom')
    expect(within(card).getByRole('button', { name: 'Balanced (default)' })).toHaveAttribute(
      'aria-pressed',
      'false',
    )

    await user.click(screen.getByLabelText('Save Matching techniques'))
    await waitFor(() => expect(putPayloads(fetchMock)).toHaveLength(1))
    const strategy = putPayloads(fetchMock)[0]['sync.strategy'] as {
      preset: string
      techniques: Array<{ id: string }>
    }
    expect(strategy.preset).toBe('custom')
    expect(strategy.techniques.map((step) => step.id)).toEqual(['mfcc'])
  })

  it('refuses to remove the last step and explains why', async () => {
    const user = userEvent.setup()
    mockFetch()
    await openPage()

    const card = strategyCard()
    await user.click(within(card).getByRole('button', { name: 'Remove step 1' }))

    expect(within(card).getByRole('status')).toHaveTextContent(/at least one step/i)
    expect(within(card).getByLabelText('Step 1 method')).toBeInTheDocument()
    expect(screen.getByLabelText('Save Matching techniques')).toBeInTheDocument()
  })

  it('adds a switched-off second-opinion step when asked', async () => {
    const user = userEvent.setup()
    mockFetch()
    await openPage()

    const card = strategyCard()
    await user.click(within(card).getByRole('button', { name: 'Add a step' }))

    expect(within(card).getByLabelText('Step 2 method')).toHaveValue('mfcc')
    expect(within(card).getByLabelText('Step 2 when it runs')).toHaveValue('below_confidence')
    expect(within(card).getByRole('switch', { name: 'Step 2 enabled' })).toHaveAttribute(
      'aria-checked',
      'false',
    )
    expect(
      within(card).getAllByRole('option', { name: 'Only when the previous result is weak' }).length,
    ).toBeGreaterThan(0)

    // Every edit leaves the preset label behind, so a second step is Custom.
    expect(within(card).getByTestId('strategy-preset')).toHaveTextContent('Custom')
  })

  it('reorders the steps and submits them in the new order', async () => {
    const user = userEvent.setup()
    const reversed = {
      preset: 'custom',
      techniques: [
        { id: 'mfcc', enabled: true, when: 'always', min_confidence: 0.1, min_match_z: 5 },
        { id: 'waveform', enabled: true, when: 'always', min_confidence: 0.1, min_match_z: 5 },
      ],
    }
    const fetchMock = mockFetch({ strategy: reversed })
    await openPage()

    const card = strategyCard()
    expect(within(card).getByLabelText('Step 1 method')).toHaveValue('mfcc')
    await user.click(within(card).getByRole('button', { name: 'Move step 2 up' }))

    expect(within(card).getByLabelText('Step 1 method')).toHaveValue('waveform')
    expect(within(card).getByLabelText('Step 2 method')).toHaveValue('mfcc')

    await user.click(screen.getByLabelText('Save Matching techniques'))
    await waitFor(() => expect(putPayloads(fetchMock)).toHaveLength(1))
    const strategy = putPayloads(fetchMock)[0]['sync.strategy'] as {
      techniques: Array<{ id: string }>
    }
    expect(strategy.techniques.map((step) => step.id)).toEqual(['waveform', 'mfcc'])
  })

  it('renders a rejected save inside the card and keeps the draft', async () => {
    const user = userEvent.setup()
    const refusal = 'sync.strategy: at least one technique must be enabled'
    const fetchMock = mockFetch({ putStatus: 400, putError: refusal })
    await openPage()

    const card = strategyCard()
    await user.click(within(card).getByRole('switch', { name: 'Step 1 enabled' }))
    await user.click(screen.getByLabelText('Save Matching techniques'))

    const message = await within(card).findByRole('alert')
    expect(message).toHaveTextContent(refusal)
    // Inline, next to the list — never a dialog over the page.
    expect(screen.queryByRole('dialog')).toBeNull()
    // The refused draft is still there, and it is what was submitted.
    expect(within(card).getByRole('switch', { name: 'Step 1 enabled' })).toHaveAttribute(
      'aria-checked',
      'false',
    )
    const strategy = putPayloads(fetchMock)[0]['sync.strategy'] as {
      techniques: Array<{ enabled: boolean }>
    }
    expect(strategy.techniques[0].enabled).toBe(false)
  })
})
