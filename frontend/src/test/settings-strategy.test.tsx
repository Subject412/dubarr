import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { DEFAULT_SETTINGS } from '../lib/defaults'
import { settingsSectionId, type SettingsSectionKey } from '../lib/settings-sections'
import { SettingsPage } from '../pages/Settings'
import { isMethod, jsonResponse, renderWithProviders } from '../test/helpers'

/**
 * `analyze.DefaultPiecewisePolicy()` (#84, D5), field for field: segmentation is
 * off on a fresh install, because a piecewise job re-encodes the dub copy and
 * nobody should pay for that on a film that needed one offset. Both shipped
 * strategies below point at this object rather than copying the numbers twice.
 */
const PIECEWISE_DEFAULTS = {
  enabled: false,
  step_sec: 45,
  window_sec: 60,
  split_penalty_ms: 250,
  min_segment_sec: 120,
  max_segments: 12,
}

/**
 * The shipped strategy (#82, D4), spelled out. The server seeds exactly this
 * (`analyze.DefaultStrategy` / `settings.PresetTechniques("balanced")`), so
 * DEFAULTS['sync.strategy'] must stay equal to it — that equality is one of the
 * assertions below, because a drift there would rewrite every operator's list.
 *
 * `z_gate` is true: the waveform matcher is judged on its peak's z as well as on
 * quality. The flag is part of the section, not a UI preference, so the literal
 * carries it.
 */
const SHIPPED = {
  preset: 'balanced',
  techniques: [
    {
      id: 'waveform',
      enabled: true,
      when: 'always',
      min_confidence: 0.1,
      min_match_z: 5,
      z_gate: true,
    },
  ],
  // `analyze.DefaultPiecewisePolicy()` (#84, D5): segmentation is off on a
  // fresh install, because a piecewise job re-encodes the operator's dub copy.
  piecewise: PIECEWISE_DEFAULTS,
}

/**
 * The `maximum_accuracy` list as the server serves it: a waveform step gated on
 * quality and σ, then the mel-cepstral second opinion judged on quality alone
 * (`z_gate: false`, no σ band), because its z lives on a different curve.
 */
const MAXIMUM_ACCURACY = {
  preset: 'maximum_accuracy',
  techniques: [
    {
      id: 'waveform',
      enabled: true,
      when: 'always',
      min_confidence: 0.1,
      min_match_z: 5,
      z_gate: true,
    },
    {
      id: 'mfcc',
      enabled: true,
      when: 'below_confidence',
      min_confidence: 0.15,
      min_match_z: 0,
      z_gate: false,
    },
  ],
  // Off, like every preset (#84): `settings.Preset` keeps the documented
  // piecewise block in all three starting points, and `sameStrategy` compares
  // it, so a section that flipped it would be stored as `custom`.
  piecewise: PIECEWISE_DEFAULTS,
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

  it('hides the σ field for a method that is judged on match quality', async () => {
    const user = userEvent.setup()
    mockFetch({ strategy: MAXIMUM_ACCURACY })
    await openPage()

    const card = strategyCard()

    // Collapsed, the mfcc row reads out no σ at all, and says in one line what
    // judges it instead.
    expect(within(card).queryByTestId('strategy-z-diagnostic-1')).toBeNull()
    expect(within(card).getByTestId('strategy-z-diagnostic-2')).toHaveTextContent(
      /judged on match quality/i,
    )

    // Both rows are on, so both disclosures are offered.
    const disclosures = within(card).getAllByRole('button', { name: 'Advanced' })
    expect(disclosures).toHaveLength(2)
    await user.click(disclosures[0])
    await user.click(disclosures[1])

    // The waveform step keeps its σ band: it is z-gated.
    expect(within(card).getByLabelText('Step 1 peak must stand out by (σ)')).toHaveValue(5)
    // The mfcc step has none, and the note stays in its place.
    expect(within(card).queryByLabelText('Step 2 peak must stand out by (σ)')).toBeNull()
    expect(within(card).getByLabelText('Step 2 confidence needed')).toHaveValue(0.15)
    expect(within(card).getByTestId('strategy-z-diagnostic-2')).toHaveTextContent(
      /judged on match quality/i,
    )
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

  /**
   * D4a makes Fast a `sync.defaults` decision as well as a `sync.strategy` one,
   * so a preset press that stored only the label would leave the two sections
   * disagreeing — the reported `preset:"fast"` over six stored points. The
   * settings endpoint applies one payload in one transaction (settings.Store.Apply
   * validates everything before writing anything), so both halves travel in the
   * same PUT: neither can land without the other.
   */
  it('writes the strategy and the checkpoint positions in one save when a preset is pressed', async () => {
    const user = userEvent.setup()
    const fetchMock = mockFetch()
    await openPage()

    await user.click(within(strategyCard()).getByRole('button', { name: 'Fast' }))
    // Until it is saved the card says so: a press is not a stored starting point.
    expect(within(strategyCard()).getByTestId('strategy-preset-pending')).toHaveTextContent(
      /not saved/i,
    )

    await user.click(screen.getByLabelText('Save Matching techniques'))
    await waitFor(() => expect(putPayloads(fetchMock)).toHaveLength(1))

    const payload = putPayloads(fetchMock)[0]
    expect(Object.keys(payload).sort()).toEqual(['sync.defaults', 'sync.strategy'])
    const strategy = payload['sync.strategy'] as { preset: string; techniques: unknown[] }
    const defaults = payload['sync.defaults'] as {
      checkpoint_positions_pct: number[]
      max_offset_sec: number
    }
    expect(strategy.preset).toBe('fast')
    expect(strategy.techniques).toEqual(SHIPPED.techniques)
    expect(defaults.checkpoint_positions_pct).toEqual(FAST_POSITIONS)
    // Only the positions move; the rest of the section goes back as stored.
    expect(defaults.max_offset_sec).toBe(180)

    // Stored now, so the card stops saying otherwise.
    await waitFor(() => expect(within(strategyCard()).queryByTestId('strategy-preset-pending')).toBeNull())
    expect(within(strategyCard()).getByTestId('strategy-preset')).toHaveTextContent(/^Fast$/)
  })

  it('carries both sections when the Sync defaults card saves a staged preset', async () => {
    const user = userEvent.setup()
    const fetchMock = mockFetch()
    await openPage()

    await user.click(within(strategyCard()).getByRole('button', { name: 'Fast' }))
    await user.click(screen.getByLabelText('Save Sync defaults'))
    await waitFor(() => expect(putPayloads(fetchMock)).toHaveLength(1))

    // The other card's Save is the same user action, so it writes the pair too:
    // storing the positions alone would print Balanced over three points.
    const payload = putPayloads(fetchMock)[0]
    expect(Object.keys(payload).sort()).toEqual(['sync.defaults', 'sync.strategy'])
    expect((payload['sync.strategy'] as { preset: string }).preset).toBe('fast')
    expect(
      (payload['sync.defaults'] as { checkpoint_positions_pct: number[] })
        .checkpoint_positions_pct,
    ).toEqual(FAST_POSITIONS)
  })

  it('keeps the preset visibly unsaved when the paired save is refused', async () => {
    const user = userEvent.setup()
    const refusal = 'sync.defaults: checkpoint positions must be 1..12 entries'
    const fetchMock = mockFetch({ putStatus: 400, putError: refusal })
    await openPage()

    await user.click(within(strategyCard()).getByRole('button', { name: 'Fast' }))
    await user.click(screen.getByLabelText('Save Matching techniques'))

    expect(await within(strategyCard()).findByRole('alert')).toHaveTextContent(refusal)
    // One refused request, so neither half is stored — and the card cannot claim
    // to hold a starting point it never stored.
    await waitFor(() => expect(putPayloads(fetchMock)).toHaveLength(1))
    expect(Object.keys(putPayloads(fetchMock)[0]).sort()).toEqual([
      'sync.defaults',
      'sync.strategy',
    ])
    expect(within(strategyCard()).getByTestId('strategy-preset')).toHaveTextContent(
      /Fast \(not saved yet\)/,
    )
    expect(within(strategyCard()).getByTestId('strategy-preset')).not.toHaveTextContent(/^Fast$/)
  })

  it('abandons the staged positions when the list is edited away from the preset', async () => {
    const user = userEvent.setup()
    const fetchMock = mockFetch()
    await openPage()

    await user.click(within(strategyCard()).getByRole('button', { name: 'Fast' }))
    await user.selectOptions(within(strategyCard()).getByLabelText('Step 1 method'), 'mfcc')

    // A custom list is not the preset any more, so its checkpoint count is not
    // rewritten behind the operator's back.
    expect(within(strategyCard()).getByTestId('strategy-preset')).toHaveTextContent(/^Custom$/)
    expect(within(strategyCard()).queryByTestId('strategy-preset-pending')).toBeNull()
    await waitFor(() =>
      expect(within(syncCard()).getByLabelText('Checkpoint positions (%)')).toHaveValue(
        '10, 25, 40, 55, 70, 85',
      ),
    )

    await user.click(screen.getByLabelText('Save Matching techniques'))
    await waitFor(() => expect(putPayloads(fetchMock)).toHaveLength(1))
    expect(Object.keys(putPayloads(fetchMock)[0])).toEqual(['sync.strategy'])
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

/**
 * The segmentation block (#84, D5), in the operator's words (D11): one switch
 * that says what it is for, and five numbers the operator never has to touch
 * unless the default is wrong for their film.
 */
const PIECEWISE_TOGGLE = 'Handle films with a cut or an inserted scene'

const PIECEWISE_KNOBS = [
  { label: 'Check every (seconds)', value: 45 },
  { label: 'Sample length (seconds)', value: 60 },
  { label: 'Evidence needed for another segment', value: 250 },
  { label: 'Shortest segment (seconds)', value: 120 },
  { label: 'At most how many segments', value: 12 },
]

function piecewiseBlock(): HTMLElement {
  return within(strategyCard()).getByTestId('strategy-piecewise')
}

describe('piecewise settings (#84)', () => {
  it('ships switched off, with every knob hidden but described', async () => {
    mockFetch()
    await openPage()

    const block = piecewiseBlock()
    const toggle = within(block).getByRole('switch', { name: PIECEWISE_TOGGLE })
    expect(toggle).toHaveAttribute('aria-checked', 'false')

    // Off means the knobs are not in the form at all — but each one is still
    // described, so the operator can read what the switch would use.
    for (const knob of PIECEWISE_KNOBS) {
      expect(within(block).queryByLabelText(knob.label)).toBeNull()
      expect(
        within(block).getByRole('button', { name: `About ${knob.label}` }),
      ).toBeInTheDocument()
    }
    // Exactly one help button per authored key: the switch plus five knobs.
    expect(within(block).getAllByRole('button', { name: /^About / })).toHaveLength(6)

    // The SPA default is the server's documented policy, number for number.
    expect(DEFAULT_SETTINGS['sync.strategy'].piecewise).toEqual(PIECEWISE_DEFAULTS)
  })

  it('reveals the five knobs with the documented defaults when switched on', async () => {
    const user = userEvent.setup()
    mockFetch()
    await openPage()

    const block = piecewiseBlock()
    await user.click(within(block).getByRole('switch', { name: PIECEWISE_TOGGLE }))
    expect(within(block).getByRole('switch', { name: PIECEWISE_TOGGLE })).toHaveAttribute(
      'aria-checked',
      'true',
    )

    // Still collapsed: the values are read out, with nothing to mis-set.
    for (const knob of PIECEWISE_KNOBS) {
      expect(within(block).queryByLabelText(knob.label)).toBeNull()
    }

    await user.click(within(block).getByRole('button', { name: 'Advanced' }))
    for (const knob of PIECEWISE_KNOBS) {
      expect(within(block).getByLabelText(knob.label)).toHaveValue(knob.value)
    }
    // Advanced moves the tips onto the inputs: still one per key, never both.
    expect(within(block).getAllByRole('button', { name: /^About / })).toHaveLength(6)
  })

  it('round-trips the switch and an edited knob into the saved payload', async () => {
    const user = userEvent.setup()
    const fetchMock = mockFetch()
    await openPage()

    const block = piecewiseBlock()
    await user.click(within(block).getByRole('switch', { name: PIECEWISE_TOGGLE }))
    await user.click(within(block).getByRole('button', { name: 'Advanced' }))
    const cap = within(block).getByLabelText('At most how many segments')
    await user.clear(cap)
    await user.type(cap, '8')

    await user.click(screen.getByLabelText('Save Matching techniques'))
    await waitFor(() => expect(putPayloads(fetchMock)).toHaveLength(1))

    // The block is part of the section, so the server stores it with the list —
    // and an edited block is no longer any starting point.
    expect(Object.keys(putPayloads(fetchMock)[0])).toEqual(['sync.strategy'])
    const strategy = savedSection<{ preset: string; piecewise: unknown }>(
      fetchMock,
      'sync.strategy',
    )
    expect(strategy.preset).toBe('custom')
    expect(strategy.piecewise).toEqual({ ...PIECEWISE_DEFAULTS, enabled: true, max_segments: 8 })
  })

  it('keeps the block off when a starting point is pressed, as the server preset does', async () => {
    const user = userEvent.setup()
    const fetchMock = mockFetch({
      strategy: { ...MAXIMUM_ACCURACY, piecewise: { ...PIECEWISE_DEFAULTS, enabled: true } },
    })
    await openPage()

    const block = piecewiseBlock()
    expect(within(block).getByRole('switch', { name: PIECEWISE_TOGGLE })).toHaveAttribute(
      'aria-checked',
      'true',
    )

    // `settings.Preset` keeps the documented (off) block in every preset and
    // `resolvePreset` compares it, so a press that left the switch on would be
    // stored as `custom` while the button claims a starting point.
    await user.click(within(strategyCard()).getByRole('button', { name: 'Maximum accuracy' }))
    expect(within(block).getByRole('switch', { name: PIECEWISE_TOGGLE })).toHaveAttribute(
      'aria-checked',
      'false',
    )

    await user.click(screen.getByLabelText('Save Matching techniques'))
    await waitFor(() => expect(putPayloads(fetchMock)).toHaveLength(1))
    const strategy = savedSection<{ preset: string; piecewise: unknown }>(
      fetchMock,
      'sync.strategy',
    )
    expect(strategy.preset).toBe('maximum_accuracy')
    expect(strategy.piecewise).toEqual(PIECEWISE_DEFAULTS)
  })
})
