import type { SyncPreset, SyncStrategy, SyncTechnique } from './api'

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

/** The one waveform step the shipped strategy is (see below). */
function waveformStep(): SyncTechnique {
  return { id: 'waveform', enabled: true, when: 'always', min_confidence: 0.1, min_match_z: 5 }
}

/** The second opinion `maximum_accuracy` adds: only where the first was unsure. */
function secondOpinionStep(): SyncTechnique {
  return { id: 'mfcc', enabled: true, when: 'below_confidence', min_confidence: 0.15, min_match_z: 8 }
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
 * One preset press, as the two cards must store it: the `sync.strategy` half and,
 * for Fast/Balanced, the `sync.defaults` checkpoint positions. `positions` is
 * null when the preset leaves that section alone, so the caller knows the press
 * is complete with the strategy write alone.
 */
export type PresetPress = {
  strategy: SyncStrategy
  positions: number[] | null
}
