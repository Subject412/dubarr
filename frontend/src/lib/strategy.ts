import type { SyncPiecewisePolicy, SyncPreset, SyncStrategy, SyncTechnique } from './api'

/**
 * The SPA's one copy of the preset tables (#82).
 *
 * Three spellings of a preset exist in this repo and each has an owner:
 *
 *  - `settings.PresetTechniques` (Go) is the source of truth. The server expands
 *    a preset into a list; the SPA never does, it only offers the same list as a
 *    starting point and sends the list back.
 *  - `DEFAULT_SETTINGS['sync.strategy']` in ./defaults.ts keeps its own object
 *    literal, on purpose: a Go test brace-scans that file and decodes the
 *    literal to prove it equals the Go default, so it cannot be replaced by a
 *    reference to this module.
 *  - this module is what every *consumer* imports — the Matching techniques
 *    editor and the Sync report summary — so the labels, the three canonical
 *    lists and the checkpoint counts a preset press moves each exist once.
 */

/** The three starting points a press can offer. `custom` is never one of them. */
export type PresetName = 'fast' | 'balanced' | 'maximum_accuracy'

/** The three starting points, in the order the buttons are offered. */
export const PRESET_NAMES: readonly PresetName[] = ['fast', 'balanced', 'maximum_accuracy']

/** A preset's plain name: the report's vocabulary, without editor decoration. */
export const PRESET_LABELS: Record<SyncPreset, string> = {
  fast: 'Fast',
  balanced: 'Balanced',
  maximum_accuracy: 'Maximum accuracy',
  custom: 'Custom',
}

/**
 * The label for a name that may be unknown to this build (an older or newer
 * server): the report is the record of what ran, so an unknown preset renders
 * as itself instead of disappearing.
 */
export function presetLabel(preset: string): string {
  return (PRESET_LABELS as Record<string, string>)[preset] ?? preset
}

/**
 * The label where a starting point is being *chosen*: Balanced is the shipped
 * default, so the picker says so. A report summary has no default to mark, which
 * is why the plain table stays the report's vocabulary.
 */
export function presetChoiceLabel(preset: SyncPreset): string {
  return preset === 'balanced' ? `${PRESET_LABELS[preset]} (default)` : PRESET_LABELS[preset]
}

/**
 * The one waveform step the shipped strategy is (see below). It is z-gated: 5σ
 * on the sample correlator's curve is the shipped "a human should look" line.
 */
function waveformStep(): SyncTechnique {
  return {
    id: 'waveform',
    enabled: true,
    when: 'always',
    min_confidence: 0.1,
    min_match_z: 5,
    z_gate: true,
  }
}

/**
 * The second opinion `maximum_accuracy` adds: only where the first was unsure.
 *
 * It is NOT z-gated (`z_gate: false`, no σ band). Its z has the waveform
 * matcher's formula but not its scale — this matcher scores a few thousand
 * overlapping hop lags against ~8·10⁴ near-independent samples, so a correct
 * answer scores z 5.3 where the waveform scores 21-28 on the same pair — so a
 * shared band would refuse the very answers it exists for. Quality decides; the
 * z is still recorded, as a diagnostic.
 */
function secondOpinionStep(): SyncTechnique {
  return {
    id: 'mfcc',
    enabled: true,
    when: 'below_confidence',
    min_confidence: 0.15,
    min_match_z: 0,
    z_gate: false,
  }
}

/**
 * Whether this entry's `min_match_z` gates acceptance. The flag is the server's
 * (`analyze.Technique.ZGate`) and only an explicit `false` turns the gate off:
 * a stored list from before 0.5.0 carries no flag, but the σ it carries was a
 * band, and the report names it as the reason a peak was refused — so hiding it
 * there would erase a rule that did apply.
 */
export function isZGated(entry: SyncTechnique): boolean {
  return entry.z_gate !== false
}

/**
 * The canonical list each preset stands for — `settings.PresetTechniques`, step
 * for step. Fast and Balanced share it (D4a: Fast is fewer checkpoints, never a
 * coarser matcher) and MFCC is never a preset.
 *
 * Functions, not constants, and a fresh array per call: the editor hands the
 * list to a draft the operator then edits, so a shared array would leak that
 * edit into the next preset press.
 */
export const PRESET_TECHNIQUES: Record<PresetName, () => SyncTechnique[]> = {
  fast: () => [waveformStep()],
  balanced: () => [waveformStep()],
  maximum_accuracy: () => [waveformStep(), secondOpinionStep()],
}

/**
 * What a preset does to `sync.defaults.checkpoint_positions_pct`. Fast measures
 * three points instead of six and Balanced puts the shipped six back; Maximum
 * accuracy only changes the method list, so it has no entry here.
 */
export const PRESET_POSITIONS_PCT: Partial<Record<PresetName, number[]>> = {
  fast: [15, 50, 85],
  balanced: [10, 25, 40, 55, 70, 85],
}

/**
 * The segmentation block at its documented defaults (#84, D5): off, sampling
 * every 45 s with a 60 s window, a 250 ms split penalty, no segment shorter
 * than two minutes and at most a dozen of them — `analyze.DefaultPiecewisePolicy`,
 * field for field.
 *
 * `DEFAULT_SETTINGS['sync.strategy']` keeps its own literal because a Go test
 * brace-scans that file; this copy is what the editors seed a section written
 * before 0.5.0 with, and the settings test pins the two together.
 */
export const DEFAULT_PIECEWISE: SyncPiecewisePolicy = {
  enabled: false,
  step_sec: 45,
  window_sec: 60,
  split_penalty_ms: 250,
  min_segment_sec: 120,
  max_segments: 12,
}

/**
 * The segmentation block each starting point stands for — `settings.Preset`,
 * which keeps the documented (off) block in all three. The block is part of the
 * section that `resolvePreset` compares, so a press that left segmentation on
 * would be stored as `custom` while the button still claimed a starting point;
 * a press therefore stores the preset's own block.
 *
 * Functions and a fresh object per call, for the same reason as the lists: the
 * editor edits the object it is handed.
 */
export const PRESET_PIECEWISE: Record<PresetName, () => SyncPiecewisePolicy> = {
  fast: () => ({ ...DEFAULT_PIECEWISE }),
  balanced: () => ({ ...DEFAULT_PIECEWISE }),
  maximum_accuracy: () => ({ ...DEFAULT_PIECEWISE }),
}

/**
 * One preset press, as the two cards must store it: the `sync.strategy` half and,
 * for Fast/Balanced, the `sync.defaults` checkpoint positions. `positions` is
 * null when the preset leaves that section alone, so the caller knows the press
 * is complete with the strategy write alone.
 */
export type PresetPress = {
  strategy: SyncStrategy
  positions: number[] | null
}
