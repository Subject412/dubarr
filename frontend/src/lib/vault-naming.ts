// Client-side mirror of `backend/internal/naming` for the vault naming
// settings (#47).
//
// The settings card has to render an example line as the operator types, and
// the plan's test plan is explicit that this must happen *without a request*,
// so the same grammar is implemented here: the token whitelist, the `{Language}`
// refusal, zero padding, and the cleanup that removes the separators an empty
// token orphaned. The example values are the ones the backend validates with
// (`naming.sampleTrack`) plus the movie example the plan documents, so the two
// preview lines read exactly like the acceptance paths.
//
// This module only ever produces text: nothing here writes a file, and the
// season folder is validated as one folder name, never a path.

import type { VaultNaming } from './api'

/** The values one row of the preview line is rendered from. */
export type VaultNameSample = {
  title: string
  episode_title: string
  variant: string
  codec: string
  channels: string
  lang_iso2: string
  year: number
  season: number
  episode: number
}

/** The shipped patterns (`settings.Default*`), mirrored for the card's fallback. */
export const VAULT_NAMING_DEFAULTS: VaultNaming = {
  movie_template: '{Title} ({Year}) [{Variant} {Codec} {Channels}]',
  episode_template:
    '{Title} - S{Season:00}E{Episode:00} - {Episode Title} [{Variant} {Codec} {Channels}]',
  season_folder: 'Season {Season:00}',
}

/** A movie row: the example path in the plan (`Spirited Away (2001)`). */
export const MOVIE_SAMPLE: VaultNameSample = {
  title: 'Spirited Away',
  episode_title: '',
  variant: 'dubbing',
  codec: 'eac3',
  channels: '5.1',
  lang_iso2: 'pl',
  year: 2001,
  season: 0,
  episode: 0,
}

/** An episode row: `naming.sampleTrack`, the values the backend validates with. */
export const EPISODE_SAMPLE: VaultNameSample = {
  title: 'The Office',
  episode_title: 'Office Olympics',
  variant: 'lektor',
  codec: 'eac3',
  channels: '2.0',
  lang_iso2: 'pl',
  year: 2005,
  season: 2,
  episode: 3,
}

/** The token list the error text quotes, in the order the card shows it. */
export const VAULT_TOKEN_LIST =
  '{Title}, {Year}, {Season:00}, {Episode:00}, {Episode Title}, {Variant}, {Codec}, {Channels}'

const MAX_NAME_BYTES = 200

type TokenID =
  | 'title'
  | 'year'
  | 'season'
  | 'episode'
  | 'episode title'
  | 'variant'
  | 'codec'
  | 'channels'

const TOKEN_PADDED: Record<TokenID, boolean> = {
  title: false,
  year: false,
  season: true,
  episode: true,
  'episode title': false,
  variant: false,
  codec: false,
  channels: false,
}

/** Tokens an arr user reaches for first, refused with a reason (not "unknown"). */
const REJECTED_TOKENS = new Set(['language', 'lang', 'iso2', 'ext', 'extension', 'original title'])

type Part =
  | { kind: 'literal'; text: string }
  | { kind: 'token'; token: TokenID; pad: number }

type ParseResult = { parts: Part[] } | { error: string }

function parseToken(body: string): { part: Part } | { error: string } {
  const colon = body.indexOf(':')
  const name = colon >= 0 ? body.slice(0, colon) : body
  const modifier = colon >= 0 ? body.slice(colon + 1) : ''
  const written = name.trim()
  const key = written.toLowerCase().replace(/\s+/g, ' ')

  if (REJECTED_TOKENS.has(key)) {
    return {
      error: `{${written}} is not available: vault files always end in .<language>.mka, so a pattern cannot choose the language.`,
    }
  }
  if (!(key in TOKEN_PADDED)) {
    return { error: `Unknown token {${written}}. Available tokens are ${VAULT_TOKEN_LIST}.` }
  }
  const token = key as TokenID
  if (!TOKEN_PADDED[token]) {
    if (modifier !== '') {
      return {
        error: `{${written}} does not take ":${modifier}" — only {Season:00} and {Episode:00} pad with zeroes.`,
      }
    }
    return { part: { kind: 'token', token, pad: 0 } }
  }
  if (modifier === '') return { part: { kind: 'token', token, pad: 0 } }
  if (!/^0+$/.test(modifier)) {
    return {
      error: `{${written}} only takes zero padding, like :00 or :000 — not ":${modifier}".`,
    }
  }
  return { part: { kind: 'token', token, pad: modifier.length } }
}

/** Parse the pattern into literals and tokens; the first problem wins. */
function parsePattern(raw: string): ParseResult {
  const parts: Part[] = []
  let literal = ''
  for (let i = 0; i < raw.length; ) {
    const char = raw[i]
    if (char === '{') {
      const end = raw.indexOf('}', i + 1)
      if (end < 0) return { error: 'There is an unclosed "{" — every token needs a closing "}".' }
      const parsed = parseToken(raw.slice(i + 1, end))
      if ('error' in parsed) return parsed
      if (literal !== '') {
        parts.push({ kind: 'literal', text: literal })
        literal = ''
      }
      parts.push(parsed.part)
      i = end + 1
      continue
    }
    if (char === '}') return { error: 'There is a "}" with no matching "{" in the pattern.' }
    literal += char
    i += 1
  }
  if (literal !== '') parts.push({ kind: 'literal', text: literal })
  if (parts.length === 0) return { error: 'The pattern is empty. Add some text or a token.' }

  // The pattern must never end in .mka: Dubarr always appends ".<language>.mka".
  const last = parts[parts.length - 1]
  if (last.kind === 'literal' && /\.mka\s*$/i.test(last.text)) {
    return { error: 'The pattern must not end in .mka: Dubarr always adds .<language>.mka.' }
  }
  return { parts }
}

function byteLength(text: string): number {
  return new TextEncoder().encode(text).length
}

/** Names a shared vault cannot carry, in any case (SMB clients still resolve them). */
const RESERVED_NAMES = /^(CON|PRN|AUX|NUL|COM[1-9]|LPT[1-9])$/i

/** A catalog value as a name-safe component; never fails, may clean to "". */
export function cleanVaultTitle(value: string): string {
  // Forbidden on every filesystem Dubarr writes to, plus control characters.
  // Filtered by code point rather than by a regex class so the intent is
  // readable and no linter has to guess what a control-character range means.
  let cleaned = ''
  for (const ch of value) {
    const code = ch.codePointAt(0) ?? 0
    const forbidden = code < 0x20 || code === 0x7f || '/\\:*?"<>|'.includes(ch)
    cleaned += forbidden ? ' ' : ch
  }
  return cleaned.replace(/\s+/g, ' ').replace(/^[ .]+/, '').replace(/[ .]+$/, '')
}

function padNumber(value: number, pad: number): string {
  if (value <= 0) return ''
  return pad > 0 ? String(value).padStart(pad, '0') : String(value)
}

function variantLabel(value: string): string {
  switch (value.trim().toLowerCase()) {
    case 'dubbing':
      return 'Dubbing'
    case 'lektor':
      return 'Lektor'
    case 'original':
      return 'Original'
    case 'standard':
      return 'Standard'
    case 'commentary':
      return 'Commentary'
    default:
      return cleanVaultTitle(value)
  }
}

function tokenValue(token: TokenID, pad: number, sample: VaultNameSample): string {
  switch (token) {
    case 'title':
      return cleanVaultTitle(sample.title)
    case 'year':
      return sample.year > 0 ? String(sample.year) : ''
    case 'season':
      return padNumber(sample.season, pad)
    case 'episode':
      return padNumber(sample.episode, pad)
    case 'episode title':
      return cleanVaultTitle(sample.episode_title)
    case 'variant':
      return variantLabel(sample.variant)
    case 'codec':
      return cleanVaultTitle(sample.codec.trim().toUpperCase())
    case 'channels':
      return cleanVaultTitle(sample.channels.trim())
    default:
      // Unreachable — TokenID is closed — but keeps the renderer total.
      return ''
  }
}

/**
 * Remove what an empty token left behind: empty groups ((), [], {}) until
 * stable, the space just inside a surviving group, repeated spaces, repeated
 * dashes and dangling separators.
 */
function collapse(input: string): string {
  let text = input
  for (;;) {
    const before = text
    text = text
      .replace(/\(\s*\)/g, '')
      .replace(/\[\s*\]/g, '')
      .replace(/\{\s*\}/g, '')
      .replace(/([([])\s+/g, '$1')
      .replace(/\s+([)\]])/g, '$1')
      .replace(/\s+/g, ' ')
      .replace(/(?: *- *){2,}/g, ' - ')
      .replace(/^[ -]+/, '')
      .replace(/[ -]+$/, '')
    if (text === before) return text
  }
}

function renderParts(parts: Part[], sample: VaultNameSample): string {
  const pieces = parts.map((part) => {
    if (part.kind === 'literal') return { text: part.text, token: false, empty: false }
    const text = tokenValue(part.token, part.pad, sample)
    return { text, token: true, empty: text === '' }
  })

  // An empty token takes the separator run before it with it, which is the arr
  // behaviour: an unknown {Episode Title} loses its " - " instead of leaving a
  // dangling dash. The space that introduces "[..." stays.
  pieces.forEach((piece, index) => {
    if (!piece.empty || index === 0) return
    const previous = pieces[index - 1]
    if (!previous.token) previous.text = previous.text.replace(/[ -._]+$/, '')
  })

  return collapse(pieces.map((piece) => (piece.empty ? '' : piece.text)).join(''))
}

function cleanStem(stem: string): string | null {
  if (stem === '') return 'This pattern produces an empty name — add a token or some text.'
  if (stem === '.' || stem === '..') return `The pattern produces "${stem}", which is not a file name.`
  const base = stem.split('.')[0]
  if (RESERVED_NAMES.test(base)) {
    return `"${stem}" is a name Windows reserves, so a shared vault cannot carry it.`
  }
  if (byteLength(stem) > MAX_NAME_BYTES) {
    return `The name is ${byteLength(stem)} bytes; the limit is ${MAX_NAME_BYTES}. Shorten the pattern.`
  }
  return null
}

/** Render a file name stem, or say why this pattern cannot produce one. */
export function renderVaultStem(pattern: string, sample: VaultNameSample): { stem: string } | { error: string } {
  const parsed = parsePattern(pattern)
  if ('error' in parsed) return parsed
  const stem = cleanVaultTitle(renderParts(parsed.parts, sample))
  const problem = cleanStem(stem)
  if (problem) return { error: problem }
  return { stem }
}

/** Render the season folder, or say why it cannot be one directory name. */
export function renderSeasonFolder(
  pattern: string,
  sample: VaultNameSample,
): { folder: string } | { error: string } {
  if (pattern.trim() === '') return { folder: '' }
  if (/[/\\]/.test(pattern)) {
    return { error: 'The season folder must be a single folder name, not a path.' }
  }
  const parsed = parsePattern(pattern)
  if ('error' in parsed) return parsed
  const folder = cleanVaultTitle(renderParts(parsed.parts, sample))
  if (folder === '') return { error: 'This season folder pattern produces an empty name.' }
  if (RESERVED_NAMES.test(folder.split('.')[0])) {
    return { error: `"${folder}" is a name Windows reserves, so a shared vault cannot carry it.` }
  }
  if (byteLength(folder) > MAX_NAME_BYTES) {
    return { error: `The folder name is ${byteLength(folder)} bytes; the limit is ${MAX_NAME_BYTES}.` }
  }
  return { folder }
}

/** Validation for one pattern field: `null` when it is usable. */
export function validateMovieTemplate(raw: string): string | null {
  const result = renderVaultStem(raw, MOVIE_SAMPLE)
  return 'error' in result ? result.error : null
}

export function validateEpisodeTemplate(raw: string): string | null {
  const result = renderVaultStem(raw, EPISODE_SAMPLE)
  return 'error' in result ? result.error : null
}

export function validateSeasonFolder(raw: string): string | null {
  const result = renderSeasonFolder(raw, EPISODE_SAMPLE)
  return 'error' in result ? result.error : null
}

/** One line: the movie file name with the language suffix the vault adds. */
export function previewMovieFile(value: VaultNaming): string | null {
  const rendered = renderVaultStem(value.movie_template, MOVIE_SAMPLE)
  if ('error' in rendered) return null
  return `${rendered.stem}.${MOVIE_SAMPLE.lang_iso2}.mka`
}

/** The episode line: season folder (when set) plus the file name. */
export function previewEpisodeFile(value: VaultNaming): string | null {
  const rendered = renderVaultStem(value.episode_template, EPISODE_SAMPLE)
  if ('error' in rendered) return null
  const season = renderSeasonFolder(value.season_folder, EPISODE_SAMPLE)
  if ('error' in season) return null
  const file = `${rendered.stem}.${EPISODE_SAMPLE.lang_iso2}.mka`
  return season.folder === '' ? file : `${season.folder}/${file}`
}

/** The two starting points the "Naming style" select offers, by name. */
export const VAULT_NAMING_PRESETS: Record<'arr' | 'title_year', VaultNaming> = {
  arr: VAULT_NAMING_DEFAULTS,
  title_year: {
    movie_template: '{Title} ({Year})',
    episode_template: '{Title} - S{Season:00}E{Episode:00}',
    season_folder: 'Season {Season:00}',
  },
}

/** The preset keys, i.e. the choices — `custom` is a state and is never offered. */
export const VAULT_NAMING_PRESET_KEYS = Object.keys(VAULT_NAMING_PRESETS) as Array<
  keyof typeof VAULT_NAMING_PRESETS
>

export type VaultNamingPreset = keyof typeof VAULT_NAMING_PRESETS | 'custom'

/** Which preset the stored patterns are, or `custom` once they were edited. */
export function namingPresetFor(value: VaultNaming): VaultNamingPreset {
  for (const [preset, patterns] of Object.entries(VAULT_NAMING_PRESETS)) {
    if (
      patterns.movie_template === value.movie_template &&
      patterns.episode_template === value.episode_template &&
      patterns.season_folder === value.season_folder
    ) {
      return preset as keyof typeof VAULT_NAMING_PRESETS
    }
  }
  return 'custom'
}
