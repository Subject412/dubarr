import { useEffect, useState } from 'react'
import type {
  SyncPiecewisePolicy,
  SyncPreset,
  SyncStrategy,
  SyncTechnique,
  SyncTechniqueID,
  SyncWhen,
} from '../lib/api'
import { helpIdFor } from '../lib/help-id'
import { settingsHelp } from '../lib/settings-help'
import {
  DEFAULT_PIECEWISE,
  PRESET_NAMES,
  PRESET_PIECEWISE,
  PRESET_POSITIONS_PCT,
  PRESET_TECHNIQUES,
  isZGated,
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

/** The shipped σ band on the sample correlator's curve: "a human should look". */
const WAVEFORM_Z_BAND = 5

/**
 * The gates a method brings with it, applied when the operator picks it. The
 * waveform matcher is judged on its peak's z as well as on quality, so it keeps
 * the shipped 5σ band. MFCC is not z-gated (`z_gate: false`, no band): its z
 * lives on a different curve — a few thousand overlapping hop lags against
 * ~8·10⁴ near-independent samples — so a shared band would refuse the very
 * answers it exists for. It is judged on match quality alone and its z is
 * recorded as a diagnostic.
 */
function gatesFor(id: SyncTechniqueID): Pick<SyncTechnique, 'z_gate' | 'min_match_z'> {
  return id === 'mfcc'
    ? { z_gate: false, min_match_z: 0 }
    : { z_gate: true, min_match_z: WAVEFORM_Z_BAND }
}

/**
 * A row the operator added: off by default, so it changes nothing until switched
 * on. `z_gate: true` is the shipped rule — the σ field is there until a method
 * that is judged on quality alone is picked, and the server refuses a gated
 * entry without a band.
 */
function newStep(id: SyncTechniqueID): SyncTechnique {
  return {
    id,
    enabled: false,
    when: 'below_confidence',
    min_confidence: 0.15,
    min_match_z: 8,
    z_gate: true,
  }
}

/**
 * A numeric keystroke as the number it stands for, or `null` while it does not
 * stand for one yet — empty, just a minus sign, or a fraction mid-integer.
 */
function knobNumber(raw: string, integer: boolean): number | null {
  const parsed = Number.parseFloat(raw)
  if (!Number.isFinite(parsed)) return null
  return integer ? Math.round(parsed) : parsed
}

/**
 * A numeric knob the operator can clear before retyping it.
 *
 * The box holds the keystrokes as a string — empty and half-typed are legal
 * intermediate states — so `clear()` followed by the new number lands as that
 * number, not as the old digits with the new ones appended. A keystroke inside
 * [min, max] reaches the draft at once (a save that never leaves the box still
 * stores what was typed); a number outside the range waits, and leaving the box
 * clamps it into the range and shows the clamped value. Blur is also what a
 * press on Save does, so a save never stores a value the field could not show.
 */
function NumericKnob({
  label,
  value,
  min,
  max,
  step,
  integer = false,
  onCommit,
}: {
  label: string
  value: number
  min: number
  max: number
  step: number
  /** The knob counts things: a typed fraction commits as the nearest whole. */
  integer?: boolean
  onCommit: (next: number) => void
}) {
  const [draft, setDraft] = useState<string | null>(null)

  // A preset press (or any edit from elsewhere) replaces the number: keystrokes
  // typed against the old value must give way, or the box would keep showing a
  // number the draft no longer holds.
  // oxlint-disable react/set-state-in-effect
  useEffect(() => {
    setDraft((current) => {
      if (current === null) return null
      return knobNumber(current, integer) === value ? current : null
    })
  }, [value, integer])
  // oxlint-enable react/set-state-in-effect

  const commit = (raw: string) => {
    const next = knobNumber(raw, integer)
    if (next === null || next < min || next > max) return
    if (next !== value) onCommit(next)
  }

  const clampOnBlur = () => {
    const typed = draft === null ? value : knobNumber(draft, integer)
    const next = typed === null ? value : Math.min(max, Math.max(min, typed))
    setDraft(null)
    if (next !== value) onCommit(next)
  }

  return (
    <Input
      type="number"
      step={step}
      min={min}
      max={max}
      aria-label={label}
      value={draft ?? value}
      onChange={(event) => {
        const raw = event.target.value
        setDraft(raw)
        commit(raw)
      }}
      onBlur={clampOnBlur}
    />
  )
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
  const [openPiecewise, setOpenPiecewise] = useState(false)

  // A section written before 0.5.0 has no block, which means the documented
  // default: off at 45/60/250/120/12 (analyze.DefaultPiecewisePolicy).
  const piecewise: SyncPiecewisePolicy = draft.piecewise ?? DEFAULT_PIECEWISE

  /**
   * A number the block does not carry reads as its documented default, so the
   * card never shows a zero the operator did not choose.
   */
  const knob = (key: keyof SyncPiecewisePolicy): number => {
    const value = piecewise[key] ?? DEFAULT_PIECEWISE[key]
    return typeof value === 'number' && Number.isFinite(value) ? value : 0
  }

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

  /**
   * The segmentation block is part of the same section, so an edit there makes
   * the starting point `custom` exactly as a technique edit does — the server
   * derives the preset from the whole section, and every preset keeps the block
   * off. It also withdraws a staged preset pair: that pair describes the preset
   * this draft no longer is.
   */
  const writePiecewise = (patch: Partial<SyncPiecewisePolicy>) => {
    setNotice('')
    update({ preset: 'custom', piecewise: { ...piecewise, ...patch } })
    onPreset?.(null)
  }

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
    setOpenPiecewise(false)
    // The preset's whole section: its list and its segmentation block, both of
    // which the server compares when it decides the stored name. Every preset
    // ships the block off, so a press that left it on would be stored as
    // `custom` while the button claimed a starting point.
    const next: SyncStrategy = {
      preset: name,
      techniques: PRESET_TECHNIQUES[name](),
      piecewise: PRESET_PIECEWISE[name](),
    }
    update(next)
    // Fast and Balanced move the checkpoint positions as well, so the press is
    // two sections; Maximum accuracy leaves them alone and needs no pair.
    const positions = PRESET_POSITIONS_PCT[name]
    onPreset?.(positions ? { strategy: next, positions: [...positions] } : null)
  }

  /**
   * The five knobs as a read-out: the numbers the switch would use, each with
   * the one tip that explains it. Collapsed — and, while the feature is off,
   * always — this is all the operator sees. Advanced moves the same tips onto
   * the inputs that edit those numbers, so a key never has two help buttons.
   */
  const piecewiseSummary = (
    <div
      data-testid="strategy-piecewise-summary"
      className="flex flex-wrap items-center gap-3 text-xs text-zinc-500"
    >
      <span className="inline-flex items-center gap-1">
        <span>
          Check every <span className="font-medium text-zinc-300">{knob('step_sec')} s</span>
        </span>
        <InfoTip
          id={helpIdFor('Check every (seconds)')}
          label="Check every (seconds)"
          text={settingsHelp('sync.strategy', 'piecewise_step_sec') ?? ''}
        />
      </span>
      <span className="inline-flex items-center gap-1">
        <span>
          Sample length <span className="font-medium text-zinc-300">{knob('window_sec')} s</span>
        </span>
        <InfoTip
          id={helpIdFor('Sample length (seconds)')}
          label="Sample length (seconds)"
          text={settingsHelp('sync.strategy', 'piecewise_window_sec') ?? ''}
        />
      </span>
      <span className="inline-flex items-center gap-1">
        <span>
          Evidence for another segment{' '}
          <span className="font-medium text-zinc-300">{knob('split_penalty_ms')} ms</span>
        </span>
        <InfoTip
          id={helpIdFor('Evidence needed for another segment')}
          label="Evidence needed for another segment"
          text={settingsHelp('sync.strategy', 'piecewise_split_penalty_ms') ?? ''}
        />
      </span>
      <span className="inline-flex items-center gap-1">
        <span>
          Shortest segment{' '}
          <span className="font-medium text-zinc-300">{knob('min_segment_sec')} s</span>
        </span>
        <InfoTip
          id={helpIdFor('Shortest segment (seconds)')}
          label="Shortest segment (seconds)"
          text={settingsHelp('sync.strategy', 'piecewise_min_segment_sec') ?? ''}
        />
      </span>
      <span className="inline-flex items-center gap-1">
        <span>
          At most <span className="font-medium text-zinc-300">{knob('max_segments')}</span> segments
        </span>
        <InfoTip
          id={helpIdFor('At most how many segments')}
          label="At most how many segments"
          text={settingsHelp('sync.strategy', 'piecewise_max_segments') ?? ''}
        />
      </span>
    </div>
  )

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
          // Not every method is judged on its peak's z: see `gatesFor`.
          const gated = isZGated(entry)
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
                    onChange={(event) => {
                      // The method decides which gates apply: MFCC drops the σ
                      // band (quality alone judges it), waveform brings the
                      // shipped 5σ back.
                      const id = event.target.value as SyncTechniqueID
                      updateStep(index, { id, ...gatesFor(id) })
                    }}
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
                      The confidence gate always applies, so its tip is always
                      here: collapsed, the row reads out the values the step is
                      judged by and each value carries its own tip; expanded,
                      the tips move onto the inputs that edit those same values.
                      One tip per gate, never both at once. The σ gate is the
                      exception — it only exists for a method that is judged on
                      it, so its tip lives where its value does.
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
                        {gated && (
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
                        )}
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
                        <NumericKnob
                          label={`Step ${step} confidence needed`}
                          step={0.01}
                          min={0}
                          max={1}
                          value={entry.min_confidence}
                          onCommit={(min_confidence) => updateStep(index, { min_confidence })}
                        />
                      </Field>
                      {gated && (
                        <Field
                          label={`Step ${step} peak must stand out by (σ)`}
                          hint="5 means a human should check the result."
                          help={settingsHelp('sync.strategy', 'min_match_z')}
                        >
                          <NumericKnob
                            label={`Step ${step} peak must stand out by (σ)`}
                            step={0.5}
                            min={0}
                            max={100}
                            value={entry.min_match_z}
                            onCommit={(min_match_z) => updateStep(index, { min_match_z })}
                          />
                        </Field>
                      )}
                    </div>
                  )}
                  {/*
                    A quality-judged method has no σ to show and none to edit,
                    so the note takes the field's place — in both states, since
                    the reason is not something Advanced reveals. It is a line of
                    text, not a tip: the σ help key stays on the entry that
                    still applies it.
                  */}
                  {!gated && (
                    <p
                      data-testid={`strategy-z-diagnostic-${step}`}
                      className="text-xs text-zinc-500"
                    >
                      <span className="font-medium text-zinc-300">Judged on match quality.</span>{' '}
                      σ does not gate this method — its peak’s z is recorded for diagnostics only,
                      and the confidence floor alone decides.
                    </p>
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

      {/*
        Segmentation (#84) is a block of its own, not another step: it is
        switched on as one thing, and the five numbers behind it are correct for
        everybody (D11). Off, the knobs are absent from the form entirely — but
        each one is still described, so the switch can be understood before it
        is turned on and its cost is never hidden behind a click.
      */}
      <div
        data-testid="strategy-piecewise"
        className="space-y-3 rounded-lg border border-ink-700 bg-ink-950/40 p-3"
      >
        <Toggle
          label="Handle films with a cut or an inserted scene"
          help={settingsHelp('sync.strategy', 'piecewise_enabled')}
          checked={Boolean(piecewise.enabled)}
          onChange={(enabled) => writePiecewise({ enabled })}
        />
        <p className="text-xs text-zinc-500">
          Off, one offset is applied to the whole track. On, Dubarr looks for the point where the dub
          stops lining up and shifts each part of the film separately; the dub copy is then cut and
          rejoined in one lossless pass, and your video is never touched.
        </p>
        {piecewise.enabled ? (
          <div className="flex flex-wrap items-center gap-3">
            <Button
              type="button"
              variant="ghost"
              size="sm"
              data-testid="strategy-piecewise-advanced"
              aria-expanded={openPiecewise}
              onClick={() => setOpenPiecewise((current) => !current)}
            >
              {openPiecewise ? 'Hide advanced' : 'Advanced'}
            </Button>
            {!openPiecewise && piecewiseSummary}
          </div>
        ) : (
          piecewiseSummary
        )}
        {piecewise.enabled && openPiecewise && (
          <div className="grid gap-4 md:grid-cols-2">
            <Field
              label="Check every (seconds)"
              hint="15–300 s; the sample length may be at most twice this."
              help={settingsHelp('sync.strategy', 'piecewise_step_sec')}
            >
              <NumericKnob
                label="Check every (seconds)"
                step={1}
                min={15}
                max={300}
                value={knob('step_sec')}
                onCommit={(step_sec) => writePiecewise({ step_sec })}
              />
            </Field>
            <Field
              label="Sample length (seconds)"
              hint="20–190 s, and at most twice the interval above."
              help={settingsHelp('sync.strategy', 'piecewise_window_sec')}
            >
              <NumericKnob
                label="Sample length (seconds)"
                step={1}
                min={20}
                max={190}
                value={knob('window_sec')}
                onCommit={(window_sec) => writePiecewise({ window_sec })}
              />
            </Field>
            <Field
              label="Evidence needed for another segment"
              hint="50–5000 ms; 250 ms is the shipped value."
              help={settingsHelp('sync.strategy', 'piecewise_split_penalty_ms')}
            >
              <NumericKnob
                label="Evidence needed for another segment"
                step={10}
                min={50}
                max={5000}
                value={knob('split_penalty_ms')}
                onCommit={(split_penalty_ms) => writePiecewise({ split_penalty_ms })}
              />
            </Field>
            <Field
              label="Shortest segment (seconds)"
              hint="30–3600 s; shorter runs are merged into their neighbour."
              help={settingsHelp('sync.strategy', 'piecewise_min_segment_sec')}
            >
              <NumericKnob
                label="Shortest segment (seconds)"
                step={10}
                min={30}
                max={3600}
                value={knob('min_segment_sec')}
                onCommit={(min_segment_sec) => writePiecewise({ min_segment_sec })}
              />
            </Field>
            <Field
              label="At most how many segments"
              hint="2–40; a real re-cut has a handful of joins."
              help={settingsHelp('sync.strategy', 'piecewise_max_segments')}
            >
              <NumericKnob
                label="At most how many segments"
                step={1}
                min={2}
                max={40}
                integer
                value={knob('max_segments')}
                onCommit={(max_segments) => writePiecewise({ max_segments })}
              />
            </Field>
          </div>
        )}
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
