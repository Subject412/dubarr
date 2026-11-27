import { useState } from 'react'
import type { SyncPreset, SyncStrategy, SyncTechnique, SyncTechniqueID, SyncWhen } from '../lib/api'
import { settingsHelp } from '../lib/settings-help'
import {
  PRESET_NAMES,
  PRESET_POSITIONS_PCT,
  PRESET_TECHNIQUES,
  presetChoiceLabel,
  type PresetName,
  type PresetPress,
} from '../lib/strategy'
import { Badge, Button, Field, InfoTip, Input, Select, Toggle } from './ui'

/**
 * Editor for `sync.strategy`: the ordered list of matching techniques a job may
 * try, its three starting points, and the acceptance gates behind "Advanced".
 *
 * Two rules shape the UI. The list is authoritative and the preset name is only
 * a label — the server stores `custom` whenever the list is not that preset's
 * canonical list — so every edit here sets `preset: 'custom'`. And a preset is a
 * shortcut, not a hidden write: Fast and Balanced also move the checkpoint
 * positions, which live in `sync.defaults`, so the press stages both sections
 * through `onPreset` and the page saves the pair in one request. Until that save
 * succeeds the card says the preset is not saved yet.
 */

/** A method in the operator's words — no filter names or hop sizes (D11). */
const METHOD_OPTIONS: { value: SyncTechniqueID; label: string }[] = [
  { value: 'waveform', label: 'Waveform (FFT)' },
  { value: 'mfcc', label: 'MFCC' },
]

const WHEN_OPTIONS: { value: SyncWhen; label: string }[] = [
  { value: 'always', label: 'Always' },
  { value: 'below_confidence', label: 'Only when the previous result is weak' },
  { value: 'above_drift', label: 'Only when the offsets disagree' },
]

/** The ids the server accepts (analyze.TechniqueID); a list may hold each once. */
const METHOD_IDS: readonly SyncTechniqueID[] = ['waveform', 'mfcc']

/** The server's bound on a strategy: 1..4 entries. */
const MAX_STEPS = 4

/** A row the operator added: off by default, so it changes nothing until switched on. */
function newStep(id: SyncTechniqueID): SyncTechnique {
  return { id, enabled: false, when: 'below_confidence', min_confidence: 0.15, min_match_z: 8 }
}

/**
 * A threshold keystroke. An unparseable (or cleared) box keeps the previous
 * number rather than silently saving a zero the operator never typed.
 */
function thresholdFrom(raw: string, previous: number): number {
  const next = Number.parseFloat(raw)
  return Number.isFinite(next) ? next : previous
}

export function StrategyEditor({
  draft,
  update,
  error = '',
  presetPending = false,
  onPreset,
}: {
  draft: SyncStrategy
  update: (patch: Partial<SyncStrategy>) => void
  /** The server's rejection of the last save, rendered next to the list. */
  error?: string
  /**
   * A preset press is staged but not stored yet: the label must not read as if
   * it were. The page owns the flag because the pair spans two cards.
   */
  presetPending?: boolean
  /**
   * Hands the press to the page, which stages the `sync.defaults` half and
   * saves both sections in one request. `null` withdraws the pair (an edit makes
   * the list no longer that preset, or the preset leaves positions alone).
   */
  onPreset?: (press: PresetPress | null) => void
}) {
  const techniques = draft.techniques ?? []
  // Refusals are answered here, next to the button that was pressed: an empty
  // list or a fifth step would only come back as a 400 after a save.
  const [notice, setNotice] = useState('')
  const [openAdvanced, setOpenAdvanced] = useState<Record<number, boolean>>({})

  const preset: SyncPreset = (PRESET_NAMES as readonly string[]).includes(draft.preset)
    ? draft.preset
    : 'custom'

  // Every edit makes the label `custom`: the server derives the name from the
  // list, so the card must stop claiming a starting point it no longer is. It
  // also withdraws a staged preset pair — the checkpoint count a preset brings
  // belongs to that preset's list, not to the operator's edited one.
  const writeSteps = (next: SyncTechnique[]) => {
    setNotice('')
    update({ preset: 'custom', techniques: next })
    onPreset?.(null)
  }

  const updateStep = (index: number, patch: Partial<SyncTechnique>) =>
    writeSteps(techniques.map((entry, i) => (i === index ? { ...entry, ...patch } : entry)))

  const moveStep = (index: number, delta: -1 | 1) => {
    const target = index + delta
    if (target < 0 || target >= techniques.length) return
    const next = [...techniques]
    ;[next[index], next[target]] = [next[target], next[index]]
    writeSteps(next)
  }

  const removeStep = (index: number) => {
    if (techniques.length <= 1) {
      // A job with no step cannot answer anything, and the server refuses an
      // empty list, so the last row stays.
      setNotice('A sync job needs at least one step, so the last one cannot be removed.')
      return
    }
    writeSteps(techniques.filter((_, i) => i !== index))
  }

  const addStep = () => {
    if (techniques.length >= MAX_STEPS) {
      setNotice(`A strategy can hold at most ${MAX_STEPS} steps.`)
      return
    }
    const used = new Set(techniques.map((entry) => entry.id))
    const free = METHOD_IDS.find((id) => !used.has(id))
    if (!free) {
      setNotice('Both methods are already in the list; every step must use a different one.')
      return
    }
    writeSteps([...techniques, newStep(free)])
  }

  const applyPreset = (name: PresetName) => {
    setNotice('')
    setOpenAdvanced({})
    const next: SyncStrategy = { preset: name, techniques: PRESET_TECHNIQUES[name]() }
    update(next)
    // Fast and Balanced move the checkpoint positions as well, so the press is
    // two sections; Maximum accuracy leaves them alone and needs no pair.
    const positions = PRESET_POSITIONS_PCT[name]
    onPreset?.(positions ? { strategy: next, positions: [...positions] } : null)
  }

  return (
    <div className="space-y-5">
      <div className="space-y-2">
        <div className="flex items-center gap-1.5">
          <span className="block text-sm font-medium text-zinc-200">Starting point</span>
          <InfoTip
            id="sync-strategy-preset"
            label="Starting point"
            text={settingsHelp('sync.strategy', 'preset') ?? ''}
          />
        </div>
        <div className="flex flex-wrap gap-2">
          {PRESET_NAMES.map((name) => (
            <Button
              key={name}
              type="button"
              size="sm"
              variant={preset === name ? 'primary' : 'secondary'}
              aria-pressed={preset === name}
              onClick={() => applyPreset(name)}
            >
              {presetChoiceLabel(name)}
            </Button>
          ))}
        </div>
        <p className="text-xs text-zinc-500">
          Current starting point:{' '}
          <span data-testid="strategy-preset" className="font-medium text-zinc-300">
            {presetChoiceLabel(preset)}
            {presetPending && (
              // A press is a draft until its save lands; a refused save leaves
              // this in place, so the card never claims a starting point the
              // server does not hold.
              <span data-testid="strategy-preset-pending" className="ml-1 text-amber-300">
                {' '}(not saved yet)
              </span>
            )}
          </span>
        </p>
        <p className="text-xs text-zinc-500">
          Fast and Balanced also set the checkpoint positions in Sync defaults. Saving after a
          preset press writes both in one go, so the starting point and the positions cannot
          disagree; Maximum accuracy leaves the positions alone.
        </p>
      </div>

      <div className="space-y-2">
        <div className="flex items-center gap-1.5">
          <span className="block text-sm font-medium text-zinc-200">Matching steps</span>
          <InfoTip
            id="sync-strategy-techniques"
            label="Matching steps"
            text={settingsHelp('sync.strategy', 'techniques') ?? ''}
          />
        </div>
        <p className="text-xs text-zinc-500">
          The first step that returns a confident answer decides a checkpoint; the steps below it only
          run when their condition says the answer so far is not good enough.
        </p>

        {techniques.map((entry, index) => {
          const step = index + 1
          const advanced = Boolean(openAdvanced[index])
          return (
            <div key={index} className="space-y-3 rounded-lg border border-ink-700 bg-ink-950/40 p-3">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="flex items-center gap-2">
                  <Badge tone={entry.enabled ? 'success' : 'neutral'}>Step {step}</Badge>
                  <Toggle
                    label={`Step ${step} enabled`}
                    checked={entry.enabled}
                    onChange={(enabled) => updateStep(index, { enabled })}
                    help={settingsHelp('sync.strategy', 'enabled')}
                  />
                </div>
                <div className="flex items-center gap-1">
                  <Button
                    type="button"
                    variant="secondary"
                    size="sm"
                    aria-label={`Move step ${step} up`}
                    disabled={index === 0}
                    onClick={() => moveStep(index, -1)}
                  >
                    ↑
                  </Button>
                  <Button
                    type="button"
                    variant="secondary"
                    size="sm"
                    aria-label={`Move step ${step} down`}
                    disabled={index === techniques.length - 1}
                    onClick={() => moveStep(index, 1)}
                  >
                    ↓
                  </Button>
                  <Button
                    type="button"
                    variant="danger"
                    size="sm"
                    aria-label={`Remove step ${step}`}
                    onClick={() => removeStep(index)}
                  >
                    Remove
                  </Button>
                </div>
              </div>

              <div className="grid gap-4 md:grid-cols-2">
                <Field label={`Step ${step} method`} help={settingsHelp('sync.strategy', 'technique_id')}>
                  <Select
                    aria-label={`Step ${step} method`}
                    value={entry.id}
                    onChange={(event) =>
                      updateStep(index, { id: event.target.value as SyncTechniqueID })
                    }
                  >
                    {METHOD_OPTIONS.map((option) => (
                      <option key={option.value} value={option.value}>
                        {option.label}
                      </option>
                    ))}
                  </Select>
                </Field>
                <Field label={`Step ${step} when it runs`} help={settingsHelp('sync.strategy', 'when')}>
                  <Select
                    aria-label={`Step ${step} when it runs`}
                    value={entry.when}
                    onChange={(event) => updateStep(index, { when: event.target.value as SyncWhen })}
                  >
                    {WHEN_OPTIONS.map((option) => (
                      <option key={option.value} value={option.value}>
                        {option.label}
                      </option>
                    ))}
                  </Select>
                </Field>
              </div>

              {/*
                A switched-off step never runs, so its acceptance gates cannot
                apply: both thresholds stay out of the card entirely until the
                step is on. Even then they are only read out, never editable,
                unless the operator opens Advanced — a correct installation
                costs the operator no decisions (D11).
              */}
              {entry.enabled && (
                <div className="space-y-3">
                  <div className="flex flex-wrap items-center gap-3">
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      aria-expanded={advanced}
                      onClick={() =>
                        setOpenAdvanced((current) => ({ ...current, [index]: !advanced }))
                      }
                    >
                      {advanced ? 'Hide advanced' : 'Advanced'}
                    </Button>
                    {/*
                      Both gates keep their help in either state: collapsed, the
                      row reads out the values the step is judged by and each
                      value carries its own tip; expanded, the tips move onto the
                      inputs that edit those same values. One tip per gate, never
                      both at once.
                    */}
                    {!advanced && (
                      <div className="flex flex-wrap items-center gap-3 text-xs text-zinc-500">
                        <span className="inline-flex items-center gap-1">
                          <span>
                            Confidence ≥{' '}
                            <span className="font-medium text-zinc-300">
                              {entry.min_confidence}
                            </span>
                          </span>
                          <InfoTip
                            id={`sync-strategy-min-confidence-${index}`}
                            label={`Step ${step} confidence needed`}
                            text={settingsHelp('sync.strategy', 'min_confidence') ?? ''}
                          />
                        </span>
                        <span className="inline-flex items-center gap-1">
                          <span>
                            Peak σ ≥{' '}
                            <span className="font-medium text-zinc-300">
                              {entry.min_match_z}
                            </span>
                          </span>
                          <InfoTip
                            id={`sync-strategy-min-match-z-${index}`}
                            label={`Step ${step} peak must stand out by (σ)`}
                            text={settingsHelp('sync.strategy', 'min_match_z') ?? ''}
                          />
                        </span>
                      </div>
                    )}
                  </div>
                  {advanced && (
                    <div className="grid gap-4 md:grid-cols-2">
                      <Field
                        label={`Step ${step} confidence needed`}
                        hint="0.10 is the shipped floor."
                        help={settingsHelp('sync.strategy', 'min_confidence')}
                      >
                        <Input
                          type="number"
                          step="0.01"
                          min={0}
                          max={1}
                          aria-label={`Step ${step} confidence needed`}
                          value={entry.min_confidence}
                          onChange={(event) =>
                            updateStep(index, {
                              min_confidence: thresholdFrom(
                                event.target.value,
                                entry.min_confidence,
                              ),
                            })
                          }
                        />
                      </Field>
                      <Field
                        label={`Step ${step} peak must stand out by (σ)`}
                        hint="5 means a human should check the result."
                        help={settingsHelp('sync.strategy', 'min_match_z')}
                      >
                        <Input
                          type="number"
                          step="0.5"
                          min={0}
                          max={100}
                          aria-label={`Step ${step} peak must stand out by (σ)`}
                          value={entry.min_match_z}
                          onChange={(event) =>
                            updateStep(index, {
                              min_match_z: thresholdFrom(event.target.value, entry.min_match_z),
                            })
                          }
                        />
                      </Field>
                    </div>
                  )}
                </div>
              )}
            </div>
          )
        })}

        <div className="flex flex-wrap items-center gap-3">
          <Button type="button" variant="secondary" size="sm" onClick={addStep}>
            Add a step
          </Button>
          {notice && (
            <p role="status" className="text-xs text-amber-300">
              {notice}
            </p>
          )}
        </div>
      </div>

      {error && (
        // The server's message belongs beside the list the operator is editing,
        // not in a dialog or a page-level banner: it names the row that failed.
        <p
          role="alert"
          className="rounded-lg border border-red-800/70 bg-red-950/40 px-3 py-2 text-sm text-red-300"
        >
          {error}
        </p>
      )}
    </div>
  )
}
