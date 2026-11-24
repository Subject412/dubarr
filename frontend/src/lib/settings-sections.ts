import type { SettingsKey } from './api'

/**
 * Keys of the cards rendered on the Settings page. `arr.connections` is not a
 * section of the generic settings envelope: it is backed by the dedicated,
 * masked arr-connection endpoints, but it still needs a card, a nav entry and
 * an anchor like every other category.
 */
export type SettingsSectionKey = SettingsKey | 'arr.connections'

/**
 * Single source of truth for the settings categories. The Settings page renders
 * its cards from this list and the AppShell submenu is built from it, so the
 * navigation cannot drift from the rendered sections.
 */
export type SettingsSection = {
  key: SettingsSectionKey
  title: string
  description: string
}

export const SETTINGS_SECTIONS: readonly SettingsSection[] = [
  {
    key: 'storage.paths',
    title: 'Storage paths',
    description: 'Watch folder, vault, working area and media roots (absolute paths).',
  },
  {
    key: 'languages.policy',
    title: 'Languages policy',
    description: 'Which dubbing tracks are imported and which reference tracks are kept.',
  },
  {
    key: 'target.tag_filter',
    title: 'Process only files tagged as …',
    description:
      'Only files that carry a required tag are processed. Add tags found in filenames, or require tags from Radarr/Sonarr.',
  },
  {
    key: 'security.options',
    title: 'Security options',
    description: 'Safety switches around the vault and AV1 hand-off.',
  },
  {
    key: 'sync.defaults',
    title: 'Sync defaults',
    description: 'Automatic offset search bounds and sampling windows.',
  },
  {
    key: 'mux.options',
    title: 'Mux options',
    description: 'Atomic muxing side effects.',
  },
  {
    key: 'audio.options',
    title: 'Audio transcoding',
    description:
      'Opt-in rules that re-encode the injected audio into a predictable format. The Audio Vault stays lossless and write-once; with no rules nothing is transcoded.',
  },
  {
    key: 'watch.options',
    title: 'Watch options',
    description: 'Donor-folder scanning cadence and stability window.',
  },
  {
    key: 'imports.options',
    title: 'Donor patterns',
    description:
      'Sibling filenames that mark a dropped file as a donor dub. Matching is case-insensitive; a pattern without * is a substring match, with * a glob.',
  },
  {
    key: 'arr.connections',
    title: 'Radarr / Sonarr connections',
    description:
      'Saved Radarr and Sonarr instances. API keys are encrypted server-side and only ever shown masked.',
  },
  {
    key: 'arr.options',
    title: '*arr options',
    description: 'Radarr/Sonarr polling.',
  },
]

/** Stable DOM id for a section card, derived from its settings key. */
export function settingsSectionId(key: SettingsSectionKey): string {
  return `settings-${key.replace(/\./g, '-')}`
}

export function settingsSection(key: SettingsSectionKey): SettingsSection {
  const section = SETTINGS_SECTIONS.find((candidate) => candidate.key === key)
  if (!section) throw new Error(`unknown settings section: ${key}`)
  return section
}

export function isSettingsSectionKey(value: string | null): value is SettingsSectionKey {
  return Boolean(value) && SETTINGS_SECTIONS.some((section) => section.key === value)
}

/** Reverse of {@link settingsSectionId}: maps a rendered card id back to its key. */
export function settingsSectionKeyFromId(id: string): SettingsSectionKey | null {
  const section = SETTINGS_SECTIONS.find((candidate) => settingsSectionId(candidate.key) === id)
  return section ? section.key : null
}
