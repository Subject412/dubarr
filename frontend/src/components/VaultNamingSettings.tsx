import type { VaultNaming } from '../lib/api'
import { settingsHelp } from '../lib/settings-help'
import {
  VAULT_NAMING_PRESET_KEYS,
  VAULT_NAMING_PRESETS,
  namingPresetFor,
  previewEpisodeFile,
  previewMovieFile,
  validateEpisodeTemplate,
  validateMovieTemplate,
  validateSeasonFolder,
  type VaultNamingPreset,
} from '../lib/vault-naming'
import { Alert, Field, Input, Select } from './ui'

/**
 * The `vault.naming` card (#47): a named preset, the two file-name patterns and
 * the season folder, with a live example that updates as the operator types.
 *
 * Every string here says what the setting does *inside* the vault. A pattern is
 * text only — it cannot rename anything until the operator previews and applies
 * a plan on the Vault page — and no control here can touch a video file. The
 * preview is rendered in this component from the same sample values the server
 * validates with, so it costs no request.
 */

/** Labels for the presets only: `custom` is a status, never a choice. */
const PRESET_LABELS: Record<keyof typeof VAULT_NAMING_PRESETS, string> = {
  arr: 'Arr style (recommended)',
  title_year: 'Title and year only',
}

const FIELD_NAMES = ['movie_template', 'episode_template', 'season_folder'] as const

/**
 * The server refuses an invalid section with one message naming the field it
 * rejected (`vault.naming: movie_template: …`). Put that message on the field
 * that caused it instead of at the bottom of the card.
 */
function serverFieldError(error: string, field: (typeof FIELD_NAMES)[number]): string | undefined {
  if (error === '') return undefined
  const match = new RegExp(`${field}:\\s*(.+)$`).exec(error)
  return match?.[1]
}

function namesAField(error: string): boolean {
  return FIELD_NAMES.some((field) => error.includes(`${field}:`))
}

export function VaultNamingSettings({
  value,
  onChange,
  error = '',
}: {
  value: VaultNaming
  /** Receives the whole section, so a preset can fill all three keys at once. */
  onChange: (next: VaultNaming) => void
  /** The save error from the card, if the server refused this section. */
  error?: string
}) {
  const movieError =
    serverFieldError(error, 'movie_template') ?? validateMovieTemplate(value.movie_template) ?? undefined
  const episodeError =
    serverFieldError(error, 'episode_template') ??
    validateEpisodeTemplate(value.episode_template) ??
    undefined
  const seasonError =
    serverFieldError(error, 'season_folder') ?? validateSeasonFolder(value.season_folder) ?? undefined

  const preset = namingPresetFor(value)
  const moviePreview = previewMovieFile(value) ?? 'Fix the movie pattern below to see an example.'
  const episodePreview =
    previewEpisodeFile(value) ?? 'Fix the episode pattern below to see an example.'

  const pickPreset = (next: VaultNamingPreset) => {
    // "Custom" is a state, not a choice: it describes patterns that match no
    // preset, which is why the select renders it as a disabled status option.
    // The guard stays anyway, so a value the control cannot produce is ignored
    // instead of overwriting what the operator wrote.
    if (next === 'custom') return
    onChange({ ...VAULT_NAMING_PRESETS[next] })
  }

  return (
    <div className="space-y-4">
      <Field
        label="Naming style"
        htmlFor="vault-naming-preset"
        hint="Arr style fills both patterns; you can edit either one afterwards."
        help={settingsHelp('vault.naming', 'preset')}
      >
        <Select
          id="vault-naming-preset"
          value={preset}
          onChange={(event) => pickPreset(event.target.value as VaultNamingPreset)}
        >
          {/*
            Edited patterns match no preset, and a <select> whose value matches
            no option silently displays the first one — which would claim "Arr
            style". This disabled status option comes first so the control shows
            the state without offering it: a disabled option is announced as
            unavailable and skipped by the keyboard.
          */}
          {preset === 'custom' && (
            <option value="custom" disabled>
              Custom (edited)
            </option>
          )}
          {VAULT_NAMING_PRESET_KEYS.map((name) => (
            <option key={name} value={name}>
              {PRESET_LABELS[name]}
            </option>
          ))}
        </Select>
      </Field>

      <div data-testid="vault-naming-movie">
        <Field
          label="Movie file name"
          htmlFor="vault-naming-movie"
          hint="Dubarr adds the language suffix itself, so a pattern never ends in .mka."
          help={settingsHelp('vault.naming', 'movie_template')}
          error={movieError}
        >
          <Input
            id="vault-naming-movie"
            value={value.movie_template}
            spellCheck={false}
            onChange={(event) => onChange({ ...value, movie_template: event.target.value })}
          />
        </Field>
      </div>

      <div data-testid="vault-naming-episode">
        <Field
          label="Episode file name"
          htmlFor="vault-naming-episode"
          hint="Same tokens as a movie, plus season and episode numbers."
          help={settingsHelp('vault.naming', 'episode_template')}
          error={episodeError}
        >
          <Input
            id="vault-naming-episode"
            value={value.episode_template}
            spellCheck={false}
            onChange={(event) => onChange({ ...value, episode_template: event.target.value })}
          />
        </Field>
      </div>

      <div className="rounded-lg border border-ink-800 bg-ink-900/40 p-3">
        <p className="text-[11px] uppercase tracking-wider text-zinc-500">Advanced</p>
        <div className="mt-2" data-testid="vault-naming-season">
          <Field
            label="Season folder"
            htmlFor="vault-naming-season-folder"
            hint="Leave blank to keep episodes beside the show folder."
            help={settingsHelp('vault.naming', 'season_folder')}
            error={seasonError}
          >
            <Input
              id="vault-naming-season-folder"
              value={value.season_folder}
              spellCheck={false}
              onChange={(event) => onChange({ ...value, season_folder: event.target.value })}
            />
          </Field>
        </div>
      </div>

      <div
        data-testid="vault-naming-preview"
        className="rounded-lg border border-ink-800 bg-ink-950/50 p-3"
      >
        <Field label="Preview" help={settingsHelp('vault.naming', 'preview')}>
          <div className="space-y-1 font-mono text-xs leading-relaxed text-zinc-300">
            <p data-testid="vault-naming-preview-movie" className="break-all">
              {moviePreview}
            </p>
            <p data-testid="vault-naming-preview-episode" className="break-all">
              {episodePreview}
            </p>
          </div>
        </Field>
        <p className="mt-2 text-xs text-zinc-500">
          This changes the inside of the Audio Vault only — your videos and audio files are never
          renamed, moved or re-encoded. Nothing changes until you press Preview changes on the Vault
          page and apply that plan.
        </p>
      </div>

      {error !== '' && !namesAField(error) && <Alert>{error}</Alert>}
    </div>
  )
}
